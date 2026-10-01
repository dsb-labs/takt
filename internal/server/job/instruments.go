package job

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

type (
	// The instruments type holds the meters a runner records into.
	instruments struct {
		// How long each run took, by job and outcome.
		duration metric.Float64Histogram
	}
)

// newInstruments returns the instruments a runner records into. A nil meter
// records nothing.
func newInstruments(meter metric.Meter) instruments {
	return instruments{
		duration: telemetry.Histogram(meter, "takt.job.duration",
			"How long each run of a scheduled job took.", "s", telemetry.SlowBoundaries),
	}
}
