// Package event names the things the server records about a workload while
// converging it.
//
// The vocabulary lives in its own package because every tier handles it: the
// reconciler and the workload service record events, the database stores them,
// and the API publishes them. Owned here rather than taken from the wire format,
// because a reason is something takt establishes rather than something a caller
// submits.
package event

// The Reason type names why an event was recorded.
//
// It is a string rather than an integer because the value is both stored in a
// column and sent on the wire. An integer would take its meaning from the order
// its constants are declared in, so reordering them would reinterpret every row
// already written.
type Reason string

// DefaultMaxEvents is the number of events kept for one workload when the
// operator has not chosen. Older events are removed as new ones arrive, which
// bounds the storage by construction and needs no sweeper.
//
// It is small because the events worth reading are the recent ones: an operator
// opens the card to learn why a workload looks the way it does now. A busy
// workload loses its history sooner than a quiet one, which is the right way
// round, because the quiet workload's last event is the one still worth reading.
const DefaultMaxEvents = 10

// The reasons a workload has not started yet.
const (
	// ImagePulling is recorded while a workload's image is being fetched, which
	// otherwise looks the same as a workload the reconciler has not reached.
	ImagePulling Reason = "imagePulling"
	// ImagePulled is recorded when a workload's image has been fetched.
	ImagePulled Reason = "imagePulled"
	// ImagePullFailed is recorded when a workload's image cannot be fetched.
	ImagePullFailed Reason = "imagePullFailed"
	// RestartPaced is recorded while a failing instance waits out its backoff.
	RestartPaced Reason = "restartPaced"
	// RestartGaveUp is recorded when a workload has exhausted its restart attempts.
	RestartGaveUp Reason = "restartGaveUp"
	// ReferenceUnresolved is recorded when a workload's expected hash cannot be
	// resolved, which leaves it sitting still until whatever it names appears.
	ReferenceUnresolved Reason = "referenceUnresolved"
)

// The reasons a workload restarted.
const (
	// SpecificationModified is recorded when a workload's specification changes.
	SpecificationModified Reason = "specificationModified"
	// PortsDrifted is recorded when an instance is replaced because the host ports
	// it was given no longer match the ones it should have.
	PortsDrifted Reason = "portsDrifted"
	// HashMoved is recorded when an instance is replaced because its specification
	// hash no longer matches the one desired. Why the hash moved is a separate
	// event, written by whatever moved it.
	HashMoved Reason = "hashMoved"
	// SecretChanged is recorded when a workload is rehashed because a secret it
	// reads changed.
	SecretChanged Reason = "secretChanged"
	// VariableChanged is recorded when a workload is rehashed because a variable it
	// reads changed.
	VariableChanged Reason = "variableChanged"
	// AddressMoved is recorded when a workload is rehashed because another workload
	// it addresses moved.
	AddressMoved Reason = "addressMoved"
	// HealthCheckFailing is recorded when a workload's health check fails, which is
	// what tells a failed check apart from a crash.
	HealthCheckFailing Reason = "healthCheckFailing"
	// HealthCheckRecovered is recorded when a failing health check passes again.
	HealthCheckRecovered Reason = "healthCheckRecovered"
	// InstanceExited is recorded when an instance ends on its own and is restarted.
	InstanceExited Reason = "instanceExited"
)

// The reasons naming something done to a workload.
const (
	// Applied is recorded when a workload is applied.
	Applied Reason = "applied"
	// Suspended is recorded when a workload is suspended.
	Suspended Reason = "suspended"
	// Resumed is recorded when a suspended workload is resumed.
	Resumed Reason = "resumed"
	// RestartRequested is recorded when an operator asks for a restart.
	RestartRequested Reason = "restartRequested"
	// Deleted is recorded when a workload is marked for deletion.
	Deleted Reason = "deleted"
	// InstanceStarted is recorded when an instance is started.
	InstanceStarted Reason = "instanceStarted"
	// InstanceRemoved is recorded when an instance is removed because the desired
	// count no longer asks for it.
	InstanceRemoved Reason = "instanceRemoved"
	// PortsAbandoned is recorded when the host ports chosen for an instance are
	// given up after it failed to start.
	PortsAbandoned Reason = "portsAbandoned"
	// MountsRefreshed is recorded when a workload is signalled because the values it
	// mounts changed.
	MountsRefreshed Reason = "mountsRefreshed"
)

// The reasons particular to a workload on a schedule.
const (
	// RunStarted is recorded when a scheduled occurrence starts.
	RunStarted Reason = "runStarted"
	// RunFinished is recorded when a scheduled occurrence ends.
	RunFinished Reason = "runFinished"
	// OccurrenceSkipped is recorded when an occurrence is skipped because the
	// previous run has not finished.
	OccurrenceSkipped Reason = "occurrenceSkipped"
	// OccurrenceReplaced is recorded when an occurrence replaces a run that has not
	// finished.
	OccurrenceReplaced Reason = "occurrenceReplaced"
	// ScheduleInvalid is recorded when a workload's schedule cannot be parsed.
	ScheduleInvalid Reason = "scheduleInvalid"
)

// ConvergeFailed is recorded when a pass over a workload fails without a more
// specific reason having been recorded for it.
const ConvergeFailed Reason = "convergeFailed"
