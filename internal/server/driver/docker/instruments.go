package docker

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

type (
	// The instruments type holds the meters the driver records into.
	instruments struct {
		// How long each image pull took.
		pulls metric.Float64Histogram
		// Counts the images removed because nothing referenced them.
		pruned metric.Int64Counter
		// Counts the bytes those images occupied.
		reclaimed metric.Int64Counter
	}
)

// newInstruments returns the instruments a driver records into. A nil meter
// records nothing.
func newInstruments(meter metric.Meter) instruments {
	return instruments{
		pulls: telemetry.Histogram(meter, "takt.image.pull.duration",
			"How long each image pull took.", "s", telemetry.SlowBoundaries),
		pruned: telemetry.Counter(meter, "takt.image.pruned",
			"The number of images removed because no workload named them and no container used them.", "{image}"),
		reclaimed: telemetry.Counter(meter, "takt.image.reclaimed",
			"The size of the images removed because nothing referenced them.", "By"),
	}
}
