package service

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The name this package's telemetry is recorded under, which describes the code
// declaring it rather than whatever assembles the server.
const scope = "github.com/dsb-labs/takt/internal/server/service"

type (
	// The driftState type holds, for every pulled workload the last check looked
	// at, whether it was found behind its tag. Observed by the gauge rather than
	// set on it, so a workload that stops being checked — deleted, suspended, or
	// no longer pulled — stops being reported rather than freezing at its last
	// value.
	driftState struct {
		mux      sync.Mutex
		workload map[string]bool
	}
)

// replace what the last check found with what this one did.
func (d *driftState) replace(found map[string]bool) {
	d.mux.Lock()
	defer d.mux.Unlock()

	d.workload = found
}

// behind reports whether the last check found the named workload behind its tag.
func (d *driftState) behind(workload string) bool {
	d.mux.Lock()
	defer d.mux.Unlock()

	return d.workload[workload]
}

// observe reports each checked workload to the gauge.
func (d *driftState) observe(observer metric.Observer, gauge metric.Int64ObservableGauge) {
	d.mux.Lock()
	defer d.mux.Unlock()

	for workload, drifted := range d.workload {
		var value int64
		if drifted {
			value = 1
		}

		observer.ObserveInt64(gauge, value, metric.WithAttributes(attribute.String("workload", workload)))
	}
}

// registerMetrics registers the gauge saying which pulled workloads are behind their
// tag. One series per workload checked, holding one while the tag resolves to a
// digest other than the one the workload was hashed with and zero otherwise, so a
// scraper alerting on it is how an operator is told.
func (s *WorkloadService) registerMetrics(meter metric.Meter) {
	drifted, err := meter.Int64ObservableGauge("takt.workload.image.drifted",
		metric.WithDescription("Whether a pulled workload's tag resolves to a digest other than the one it is running."),
		metric.WithUnit("1"))
	if err != nil {
		otel.Handle(fmt.Errorf("failed to build the image drift gauge: %w", err))

		return
	}

	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		s.drifted.observe(observer, drifted)

		return nil
	}, drifted)
	if err != nil {
		otel.Handle(fmt.Errorf("failed to register the image drift gauge: %w", err))
	}
}
