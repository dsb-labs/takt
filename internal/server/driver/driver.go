// Package driver defines the vocabulary shared between the orca server and the
// runtimes it can run workloads on.
//
// A driver is responsible for turning a workload's specification into running
// work, for reporting what it currently has running, and for stopping it again.
// Drivers own their own bookkeeping: the server persists desired state only and
// asks the driver what is actually running, so a driver is free to record its
// instances however its runtime allows — container labels, unit properties, a
// pidfile — without that choice reaching the database or the API.
package driver

import (
	"time"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// The State type describes the state of a single instance as reported by its driver.
type State string

const (
	// StatePending indicates the instance has been created but is not yet running.
	StatePending State = "pending"
	// StateRunning indicates the instance is running.
	StateRunning State = "running"
	// StateTerminating indicates the instance is being torn down and will shortly
	// be gone.
	//
	// This is a state orca's own actions produce: replacing an outdated instance
	// and deleting a workload both stop a container before removing it, and a
	// runtime need not complete either step before reporting. It is deliberately
	// distinct from a failure, because nothing has gone wrong, and from an exit,
	// because there is nothing left to restart.
	StateTerminating State = "terminating"
	// StateExited indicates the instance ran to completion and exited cleanly.
	StateExited State = "exited"
	// StateCompleted indicates the instance did what it was asked to do: it exited
	// cleanly, and its workload's restart policy asks for nothing further.
	//
	// Only a clean exit reaches this state. An instance that exited non-zero stays
	// failed however its policy treats it, because how a workload ended and whether
	// it runs again are separate facts. A workload that will not be restarted still
	// has to say whether it succeeded.
	//
	// No driver reports this. A driver reports what it observed, which is that the
	// instance exited or failed, and it knows nothing about the policy. The server
	// decides what that ending means and rewrites the state before anything reads
	// it, which keeps the policy in one place.
	StateCompleted State = "completed"
	// StateFailed indicates the instance exited with a non-zero status, or could
	// not be started at all.
	StateFailed State = "failed"
)

type (
	// The Workload type describes the work a driver should run, in terms every runtime
	// shares.
	//
	// It is desired state, where Instance is observed state. The two are deliberately
	// separate: what the server wants and what a runtime reports are different things,
	// and a driver is what turns one into the other.
	//
	// The runtime-specific part of a specification stays behind Spec rather than being
	// spread across fields only one driver reads. A driver takes what it recognises and
	// ignores the rest, so a new runtime adds a block rather than widening this type.
	Workload struct {
		// The identifier the server assigned to the workload, which is stable across
		// a rename and safe to use as a path component.
		//
		// A driver needing somewhere on disk keys it on this rather than on the name:
		// a name is the operator's handle and reaches a driver from places a manifest
		// never validated, where an identifier is orca's own.
		ID string
		// The name of the workload, which identifies it to the operator.
		Name string
		// The index of the instance this work runs as. A workload asking for N
		// instances is handed to its driver N times, each with its own index and
		// its own ports.
		Instance int
		// The version of the specification this work is created from.
		Version int
		// The hash of the specification this work is created from, recorded by the
		// driver so that drift can be detected later.
		SpecHash string
		// The environment variables set for the work.
		Env map[string]string
		// The ports the work publishes, each already resolved to a host port.
		Ports []Port
		// The volumes the work mounts, each already resolved to a path on the host.
		Volumes []Volume
		// Arbitrary key-value pairs the operator attached to the workload.
		Labels map[string]string
		// The specification the workload was stored with, which carries the runtime
		// block the driver reads.
		Spec manifest.Spec
	}

	// The Instance type describes one unit of work a driver is running on behalf of
	// a workload. It is observed state: every field reflects what the driver found
	// when asked, never what the server wishes were true.
	Instance struct {
		// The driver's opaque handle for this instance, such as a container ID.
		// Meaningful only to the driver that produced it.
		ID string
		// The name of the workload the instance belongs to.
		Workload string
		// The index of the instance among the workload's instances. Each index is
		// converged on its own, so a driver reports the index it recorded when the
		// instance started.
		Index int
		// The hash of the specification the instance was started from. When this
		// differs from the workload's desired hash, the instance is stale and
		// will be replaced.
		SpecHash string
		// The version of the specification the instance was started from.
		Version int
		// The instance's current state.
		State State
		// The exit code, set once the instance has exited.
		ExitCode int
		// The time the instance last started, if it has started at all.
		StartedAt time.Time
		// The ports the instance actually has published, as reported by its runtime.
		Ports []Port
		// What the runtime reports about the instance's own health, when the image
		// declares a check of its own. Empty when it declares none.
		//
		// This is separate from the check orca performs: an image may carry a
		// HEALTHCHECK that docker is already running, and ignoring it would discard
		// something the operator asked for.
		RuntimeHealth string
		// Whether the instance is kept only so that its output can still be read.
		//
		// A driver retains the instance it most recently stopped rather than destroying
		// it, so the output of an attempt that failed outlives the attempt. Such an
		// instance is not work the driver is doing: it has ended, nothing will restart
		// it, and whoever reasons about what is running has to leave it out or a corpse
		// reads as an instance.
		//
		// It is reported rather than hidden because the orphan sweep needs to see it. A
		// workload deleted while the server was down leaves one behind, and one absent
		// from an observation would never be reaped.
		Retained bool
	}

	// The LogOptions type describes which of a workload's output to read.
	LogOptions struct {
		// How many lines to read from the end of the output.
		Tail int
		// Whether to read the instance the driver retained rather than the ones it is
		// running.
		//
		// The two are never combined. A driver holds the current attempt and the one
		// before it, and concatenating them would return two runs spliced together with
		// nothing marking the boundary.
		Previous bool
		// Whether to keep writing output as the instance produces it, rather than
		// returning once the tail has been written.
		//
		// The stream ends when the instance ends. A driver follows the work it is
		// running now, so a replacement is a new instance and a new read: what the
		// caller asked to watch has finished.
		//
		// Never combined with Previous. A retained instance has already ended, so
		// there is nothing further for it to say.
		Follow bool
		// Which instance's output to read, by its index. Nil reads every instance,
		// which is what a caller passing nothing means.
		//
		// A pointer rather than an index, because zero is a valid index and the
		// unset case has to be told apart from asking for the first instance.
		Instance *int
		// The instant to read the output from, ignoring anything written before it.
		//
		// The zero time reads from as far back as Tail allows, which is what a caller
		// passing nothing means.
		//
		// A driver honours this only when its runtime timestamps the output it keeps.
		// One that does not ignores this rather than guessing, because a line's time
		// would have to be invented and a filter built on an invented time is worse
		// than no filter.
		Since time.Time
	}

	// The Port type describes a published port of an instance.
	Port struct {
		// The port the workload listens on inside its runtime.
		Container int
		// The host port that reaches it.
		Host int
		// The transport protocol the port is published on, which is either tcp or
		// udp. The two are separate address spaces, so a driver publishing the wrong
		// one leaves the workload unreachable at an address orca reports.
		Protocol string
	}

	// The Volume type describes a volume a workload mounts, with the path it lives
	// at on the host already resolved.
	//
	// A driver is given the path rather than the name, so that nothing about how a
	// volume is stored has to be understood by the runtimes that mount one.
	Volume struct {
		// The name that identifies the volume.
		Name string
		// Where the volume's data is on the host.
		Host string
		// Where the workload finds it. What that means is the driver's business: a
		// path inside a container, or one resolved against an exec workload's own
		// working directory.
		Target string
	}

	// The Event type reports that a driver's view of an instance has changed, so
	// that the server can reconcile sooner than its next scheduled pass.
	//
	// An event carries only the affected workload: it is a hint that something
	// moved, not a description of what. The reconciler responds by observing the
	// driver afresh, which keeps it correct even if events are coalesced, delayed,
	// or dropped entirely.
	Event struct {
		// The name of the workload whose state changed.
		Workload string
	}
)

// NewWorkload maps a stored workload onto the shape a driver runs, decoding the
// specification the server persisted.
//
// The runtime block is left in Spec rather than pulled apart here: which block matters
// is the driver's business, and a server that understood each of them would have to
// change every time a runtime was added.
func NewWorkload(row database.Workload) (Workload, error) {
	spec, err := manifest.Decode(row.Spec)
	if err != nil {
		return Workload{}, err
	}

	w := Workload{
		ID:       row.ID,
		Name:     row.Name,
		Version:  row.Version,
		SpecHash: row.SpecHash,
		Labels:   row.Labels,
		Spec:     spec,
	}

	w.Env = spec.Env

	if len(spec.Ports) > 0 {
		w.Ports = make([]Port, 0, len(spec.Ports))
		for _, mapping := range spec.Ports {
			// A specification reaching a driver has had its ports resolved, so a
			// mapping with no host port is a workload the server has not finished
			// settling. It is left for a later pass rather than published wrongly.
			if mapping.From == 0 {
				continue
			}

			protocol := string(manifest.ProtocolTCP)
			if mapping.Protocol != "" {
				protocol = string(mapping.Protocol)
			}

			w.Ports = append(w.Ports, Port{Container: mapping.To, Host: mapping.From, Protocol: protocol})
		}
	}

	if len(spec.Volumes) > 0 {
		w.Volumes = make([]Volume, 0, len(spec.Volumes))
		for _, mount := range spec.Volumes {
			// Only a volume is resolved here, and which source a mount names is asked
			// of the manifest package rather than inferred from which field is set. A
			// mounted secret or variable is written as the workload starts and added to
			// this by whoever wrote it, so that nothing about a value ever reaches a
			// stored specification.
			if kind, err := manifest.KindOf(mount); err != nil || kind != manifest.MountVolume {
				continue
			}

			// Unresolved for the same reason a port can be: the server had not
			// finished settling the workload. Mounting nothing would be worse than
			// waiting, since the workload would start and write somewhere else.
			if mount.From == "" {
				continue
			}

			w.Volumes = append(w.Volumes, Volume{Name: mount.Name, Host: mount.From, Target: mount.To})
		}
	}

	return w, nil
}
