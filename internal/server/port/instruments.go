package port

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RegisterMetrics registers gauges describing the allocator's pool: the number of
// host ports in the configured range, and the number currently allocated on each
// protocol.
//
// Usage is reported per protocol because the two are separate address spaces. A
// single number counting both could exceed the capacity of the range, which would
// read as a fault rather than as a range holding as many UDP ports as TCP ones.
//
// The allocated function is asked once per scrape rather than once per pass, so
// its cost lands on the reader — for the repository behind it, one read of the
// allocations it already records.
func (a *Allocator) RegisterMetrics(meter metric.Meter, allocated func(ctx context.Context) (map[Protocol]int, error)) error {
	capacity, err := meter.Int64ObservableGauge("orca.ports.capacity",
		metric.WithDescription("The number of host ports in the configured range."),
		metric.WithUnit("{port}"))
	if err != nil {
		return fmt.Errorf("failed to build the port capacity gauge: %w", err)
	}

	used, err := meter.Int64ObservableGauge("orca.ports.used",
		metric.WithDescription("The number of host ports currently allocated to workloads."),
		metric.WithUnit("{port}"))
	if err != nil {
		return fmt.Errorf("failed to build the port usage gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		observer.ObserveInt64(capacity, int64(a.max-a.min+1))

		counts, err := allocated(ctx)
		if err != nil {
			return fmt.Errorf("failed to count allocated ports: %w", err)
		}

		for _, protocol := range []Protocol{ProtocolTCP, ProtocolUDP} {
			observer.ObserveInt64(used, int64(counts[protocol]),
				metric.WithAttributes(attribute.String("protocol", string(protocol))))
		}

		return nil
	}, capacity, used)
	if err != nil {
		return fmt.Errorf("failed to register the port gauges: %w", err)
	}

	return nil
}
