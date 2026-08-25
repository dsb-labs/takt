package docker

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/orca/internal/server/telemetry"
)

type (
	// The instruments type holds the meters the driver records into.
	instruments struct {
		// How long each image pull took.
		pulls metric.Float64Histogram
	}
)

// newInstruments returns the instruments the driver records into. A nil meter
// records nothing.
func newInstruments(meter metric.Meter) instruments {
	return instruments{
		pulls: telemetry.Histogram(meter, "orca.image.pull.duration",
			"How long each image pull took.", "s"),
	}
}
