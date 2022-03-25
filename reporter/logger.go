package reporter

import (
	"time"

	"github.com/rs/zerolog"
)

// Logger is an alias for zerolog.Logger
type Logger = zerolog.Logger

// BurstSampler is just a façade for zerolog.BurstSampler
func BurstSampler(period time.Duration, burst uint32) zerolog.Sampler {
	return &zerolog.BurstSampler{
		Period: period,
		Burst:  burst,
	}
}
