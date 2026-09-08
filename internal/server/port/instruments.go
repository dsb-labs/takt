package port

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// registerMetrics registers gauges describing the allocator's pool: the number of
// host ports in the configured range, and the number currently allocated on each
// protocol.
//
// Usage is reported per protocol because the two are separate address spaces. A
// single number counting both could exceed the capacity of the range, which would
// read as a fault rather than as a range holding as many UDP ports as TCP ones.
//
// The allocated function reports the ports held on each protocol, which is the
// shape the repository already returns. Counting them is done here rather than by
// the caller: a conversion at the call site is a conversion that can be got wrong,
// and one that turned a map of ports into the number of protocols went unnoticed
// until a load test put more than one port on a node.
//
// The allocated function is asked once per scrape rather than once per pass, so
// its cost lands on the reader — for the repository behind it, one read of the
// allocations it already records.
func (a *Allocator) registerMetrics(meter metric.Meter, tracer trace.Tracer, allocated func(ctx context.Context) (map[string][]int, error)) {
	capacity, err := meter.Int64ObservableGauge("takt.ports.capacity",
		metric.WithDescription("The number of host ports in the configured range."),
		metric.WithUnit("{port}"))
	if err != nil {
		otel.Handle(fmt.Errorf("failed to build the port capacity gauge: %w", err))

		return
	}

	used, err := meter.Int64ObservableGauge("takt.ports.used",
		metric.WithDescription("The number of host ports currently allocated to workloads."),
		metric.WithUnit("{port}"))
	if err != nil {
		otel.Handle(fmt.Errorf("failed to build the port usage gauge: %w", err))

		return
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		// The reader collects on its own context, so the read below traces as a
		// root. Named here so it reads as what it is, rather than as whatever
		// query the repository happens to run.
		ctx, span := tracer.Start(ctx, "ports.observe")
		defer span.End()

		observer.ObserveInt64(capacity, int64(a.max-a.min+1))

		held, err := allocated(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())

			return fmt.Errorf("failed to count allocated ports: %w", err)
		}

		// Both protocols are observed whether or not either holds anything, so a
		// range that has emptied reads as zero rather than as a series that stopped
		// being reported.
		for _, protocol := range []Protocol{ProtocolTCP, ProtocolUDP} {
			observer.ObserveInt64(used, int64(len(held[string(protocol)])),
				metric.WithAttributes(attribute.String("protocol", string(protocol))))
		}

		return nil
	}, capacity, used)
	if err != nil {
		otel.Handle(fmt.Errorf("failed to register the port gauges: %w", err))
	}
}
