package health

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

type (
	// The instruments type holds the meters a checker records into.
	instruments struct {
		// How long each probe took, by workload and outcome.
		duration metric.Float64Histogram
	}
)

// newInstruments returns the instruments a checker records into. A nil meter
// records nothing.
func newInstruments(meter metric.Meter) instruments {
	return instruments{
		duration: telemetry.Histogram(meter, "takt.health.check.duration",
			"How long each health probe took.", "s"),
	}
}
