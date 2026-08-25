package reconciler

import (
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type (
	// The instruments type holds the meters a reconciler records into.
	instruments struct {
		// Counts completed passes, by outcome.
		passes metric.Int64Counter
		// How long each pass took, by outcome.
		passDuration metric.Float64Histogram
		// How long converging each workload took.
		converges metric.Float64Histogram
		// The number of workloads in each state, recorded once per pass.
		workloads metric.Int64Gauge
		// Counts paced attempts to start a workload that keeps failing.
		restarts metric.Int64Counter
		// Counts workloads given up on because they would not stay up.
		giveups metric.Int64Counter
		// How long each driver took to report what it is running.
		observes metric.Float64Histogram
	}
)

// newInstruments returns the instruments a reconciler records into. A nil meter
// records nothing. An instrument that cannot be built is reported once and
// everything records nothing, so a bad name costs the metrics rather than the
// server.
func newInstruments(logger *slog.Logger, meter metric.Meter) instruments {
	if meter == nil {
		meter = noop.Meter{}
	}

	built, err := buildInstruments(meter)
	if err != nil {
		logger.With("error", err).Warn("failed to build reconciler instruments, metrics will record nothing")

		built, _ = buildInstruments(noop.Meter{})
	}

	return built
}

// buildInstruments creates every instrument against the given meter, reporting
// the creation failures joined.
func buildInstruments(meter metric.Meter) (instruments, error) {
	var errs []error

	passes, err := meter.Int64Counter("orca.reconcile.passes",
		metric.WithDescription("The number of reconciliation passes completed."),
		metric.WithUnit("{pass}"))
	errs = append(errs, err)

	passDuration, err := meter.Float64Histogram("orca.reconcile.pass.duration",
		metric.WithDescription("How long each reconciliation pass took."),
		metric.WithUnit("s"))
	errs = append(errs, err)

	converges, err := meter.Float64Histogram("orca.workload.converge.duration",
		metric.WithDescription("How long converging each workload took."),
		metric.WithUnit("s"))
	errs = append(errs, err)

	workloads, err := meter.Int64Gauge("orca.workloads",
		metric.WithDescription("The number of workloads in each state."),
		metric.WithUnit("{workload}"))
	errs = append(errs, err)

	restarts, err := meter.Int64Counter("orca.workload.restarts",
		metric.WithDescription("The number of paced attempts to start a workload that keeps failing."),
		metric.WithUnit("{restart}"))
	errs = append(errs, err)

	giveups, err := meter.Int64Counter("orca.workload.giveups",
		metric.WithDescription("The number of workloads given up on because they would not stay up."),
		metric.WithUnit("{giveup}"))
	errs = append(errs, err)

	observes, err := meter.Float64Histogram("orca.driver.observe.duration",
		metric.WithDescription("How long each driver took to report what it is running."),
		metric.WithUnit("s"))
	errs = append(errs, err)

	return instruments{
		passes:       passes,
		passDuration: passDuration,
		converges:    converges,
		workloads:    workloads,
		restarts:     restarts,
		giveups:      giveups,
		observes:     observes,
	}, errors.Join(errs...)
}
