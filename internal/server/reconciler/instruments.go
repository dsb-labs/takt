package reconciler

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/orca/internal/server/telemetry"
)

// Outcomes for a pass that ended before it could converge anything.
const (
	// outcomeListFailed labels a pass that could not read desired state.
	outcomeListFailed telemetry.Outcome = "list_failed"
	// outcomeObserveFailed labels a pass a driver did not answer.
	outcomeObserveFailed telemetry.Outcome = "observe_failed"
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
// records nothing.
func newInstruments(meter metric.Meter) instruments {
	return instruments{
		passes: telemetry.Counter(meter, "orca.reconcile.passes",
			"The number of reconciliation passes completed.", "{pass}"),
		passDuration: telemetry.Histogram(meter, "orca.reconcile.pass.duration",
			"How long each reconciliation pass took.", "s"),
		converges: telemetry.Histogram(meter, "orca.workload.converge.duration",
			"How long converging each workload took.", "s"),
		workloads: telemetry.Gauge(meter, "orca.workloads",
			"The number of workloads in each state.", "{workload}"),
		restarts: telemetry.Counter(meter, "orca.workload.restarts",
			"The number of paced attempts to start a workload that keeps failing.", "{restart}"),
		giveups: telemetry.Counter(meter, "orca.workload.giveups",
			"The number of workloads given up on because they would not stay up.", "{giveup}"),
		observes: telemetry.Histogram(meter, "orca.driver.observe.duration",
			"How long each driver took to report what it is running.", "s"),
	}
}
