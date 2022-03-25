// Package core plumbs all the other components together.
package core

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/golang/protobuf/proto"
	"gopkg.in/tomb.v2"

	"akvorado/daemon"
	"akvorado/flow"
	"akvorado/geoip"
	"akvorado/http"
	"akvorado/kafka"
	"akvorado/reporter"
	"akvorado/snmp"
)

// Component represents the HTTP compomenent.
type Component struct {
	r      *reporter.Reporter
	d      *Dependencies
	t      tomb.Tomb
	config Configuration

	metrics metrics

	healthy            chan reporter.ChannelHealthcheckFunc
	httpFlowClients    uint32 // for dumping flows
	httpFlowChannel    chan *flow.Message
	httpFlowFlushDelay time.Duration

	classifierCache     *ristretto.Cache
	classifierErrLogger reporter.Logger
}

// Dependencies define the dependencies of the HTTP component.
type Dependencies struct {
	Daemon daemon.Component
	Flow   *flow.Component
	Snmp   *snmp.Component
	GeoIP  *geoip.Component
	Kafka  *kafka.Component
	HTTP   *http.Component
}

// New creates a new core component.
func New(r *reporter.Reporter, configuration Configuration, dependencies Dependencies) (*Component, error) {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(configuration.ClassifierCacheSize) * 10,
		MaxCost:     int64(configuration.ClassifierCacheSize),
		BufferItems: 64,
		Metrics:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot initialize classifier cache: %w", err)
	}
	c := Component{
		r:      r,
		d:      &dependencies,
		config: configuration,

		healthy:            make(chan reporter.ChannelHealthcheckFunc),
		httpFlowClients:    0,
		httpFlowChannel:    make(chan *flow.Message, 10),
		httpFlowFlushDelay: time.Second,

		classifierCache:     cache,
		classifierErrLogger: r.Sample(reporter.BurstSampler(10*time.Second, 3)),
	}
	c.d.Daemon.Track(&c.t, "core")
	c.initMetrics()
	return &c, nil
}

// Start starts the core component.
func (c *Component) Start() error {
	c.r.Info().Msg("starting core component")
	for i := 0; i < c.config.Workers; i++ {
		workerID := i
		c.t.Go(func() error {
			return c.runWorker(workerID)
		})
	}

	c.r.RegisterHealthcheck("core", c.channelHealthcheck())
	c.d.HTTP.AddHandler("/api/v0/flows", c.FlowsHTTPHandler())
	return nil
}

// runWorker starts a worker.
func (c *Component) runWorker(workerID int) error {
	c.r.Debug().Int("worker", workerID).Msg("starting core worker")

	errLogger := c.r.Sample(reporter.BurstSampler(time.Minute, 10))
	workerIDStr := strconv.Itoa(workerID)
	for {
		startIdle := time.Now()
		select {
		case <-c.t.Dying():
			c.r.Debug().Int("worker", workerID).Msg("stopping core worker")
			return nil
		case cb := <-c.healthy:
			if cb != nil {
				cb(reporter.HealthcheckOK, fmt.Sprintf("worker %d ok", workerID))
			}
		case flow := <-c.d.Flow.Flows():
			startBusy := time.Now()
			if flow == nil {
				c.r.Warn().Int("worker", workerID).Msg("no more flow available, stopping")
				return errors.New("no more flow available")
			}

			sampler := net.IP(flow.SamplerAddress).String()
			c.metrics.flowsReceived.WithLabelValues(sampler).Inc()

			// Hydratation
			if skip := c.hydrateFlow(sampler, flow); skip {
				continue
			}

			// Serialize flow (use length-prefixed protobuf)
			buf := proto.NewBuffer([]byte{})
			err := buf.EncodeMessage(flow)
			if err != nil {
				errLogger.Err(err).Str("sampler", sampler).Msg("unable to serialize flow")
				c.metrics.flowsErrors.WithLabelValues(sampler, err.Error()).Inc()
				continue
			}

			// Forward to Kafka (this could block
			c.d.Kafka.Send(sampler, buf.Bytes())
			c.metrics.flowsForwarded.WithLabelValues(sampler).Inc()

			// If we have HTTP clients, send to them too
			if atomic.LoadUint32(&c.httpFlowClients) > 0 {
				select {
				case c.httpFlowChannel <- flow: // OK
				default: // Overflow, best effort and ignore
				}
			}

			idleTime := float64(startBusy.Sub(startIdle).Nanoseconds()) / 1000 / 1000 / 1000
			busyTime := float64(time.Since(startBusy).Nanoseconds()) / 1000 / 1000 / 1000
			c.metrics.loopTime.WithLabelValues(workerIDStr, "idle").Observe(idleTime)
			c.metrics.loopTime.WithLabelValues(workerIDStr, "busy").Observe(busyTime)

		}
	}
}

// Stop stops the core component.
func (c *Component) Stop() error {
	defer func() {
		close(c.httpFlowChannel)
		close(c.healthy)
		c.r.Info().Msg("core component stopped")
	}()
	c.r.Info().Msg("stopping core component")
	c.t.Kill(nil)
	return c.t.Wait()
}

func (c *Component) channelHealthcheck() reporter.HealthcheckFunc {
	return reporter.ChannelHealthcheck(c.t.Context(nil), c.healthy)
}
