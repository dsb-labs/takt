// Package driver defines the vocabulary shared between the takt server and the
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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrImagePulling is returned by a driver's Start when the workload's image is
	// being fetched and the instance cannot start until it arrives.
	//
	// It is a waiting state rather than a failure: the caller should leave the
	// instance pending and try again on a later pass, keeping its ports and its
	// restart pacing untouched. The pull itself runs in the driver's background,
	// so returning this is what keeps a slow registry out of the reconcile pass.
	ErrImagePulling = errors.New("image pull in progress")

	// ErrHostPathDenied is returned when a path mount reaches outside every prefix
	// the server's configuration allows.
	ErrHostPathDenied = errors.New("host path not allowed")

	// ErrHostPathReadOnly is returned when a path mount reaches a prefix the server's
	// configuration grants for reading only, and the mount did not ask for that.
	ErrHostPathReadOnly = errors.New("host path allowed read-only")

	// ErrHostPathNotFile is returned when a path mount under a prefix granted for
	// reading only reaches a socket, a FIFO or a device, which a read-only mount
	// does not stop a workload from writing to.
	ErrHostPathNotFile = errors.New("host path is not a regular file or directory")
)

// The HostPath type is one entry of the server's allow-host-paths configuration:
// a prefix a path mount may sit beneath, and whether the grant is for reading only.
type HostPath struct {
	// The absolute prefix.
	Path string
	// Whether a mount beneath the prefix must be read-only.
	ReadOnly bool
}

// ParseHostPath reads one allow-host-paths entry. A trailing ":ro" marks the
// prefix as granted for reading only, the way docker's own volume flag does, and
// the bare form keeps its meaning: any mount beneath it, writable or not. ":rw"
// spells the bare form out, so someone reaching for docker's pair is not handed a
// prefix that ends in ":rw".
//
// Only those suffixes are recognised. A colon anywhere else is part of the path,
// since a path may contain one.
func ParseHostPath(entry string) (HostPath, error) {
	path, readOnly := strings.CutSuffix(entry, ":ro")
	if !readOnly {
		path, _ = strings.CutSuffix(entry, ":rw")
	}

	// Absolute, because a path mount's own path must be and a relative prefix
	// could never match one.
	if !filepath.IsAbs(path) {
		return HostPath{}, fmt.Errorf("allowed host path must be absolute, got %q", entry)
	}

	return HostPath{Path: path, ReadOnly: readOnly}, nil
}

// ParseHostPaths reads every allow-host-paths entry, stopping at the first that
// cannot be read.
func ParseHostPaths(entries []string) ([]HostPath, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	paths := make([]HostPath, 0, len(entries))
	for _, entry := range entries {
		path, err := ParseHostPath(entry)
		if err != nil {
			return nil, err
		}

		paths = append(paths, path)
	}

	return paths, nil
}

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
	// This is a state takt's own actions produce: replacing an outdated instance
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
		// never validated, where an identifier is takt's own.
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

	// The Probe type describes a command health check a driver performs where the
	// workload runs.
	//
	// It is separate from Workload because a probe is asked of an instance that is
	// already running: the driver finds the instance from the workload's identity,
	// and what the probe carries is only what the instance does not already hold.
	Probe struct {
		// The command to run, and its arguments. Exit status zero passes.
		Command []string
		// Env should resolve the environment the workload was started with, for a
		// runtime that does not keep it once the process is running. A runtime
		// whose instance carries its own environment never calls it.
		//
		// A function rather than a map so that a secret is read, and a token
		// minted, only when a runtime has to.
		Env func(ctx context.Context) (map[string]string, error)
	}

	// The Tail type is a writer that keeps the end of what is written to it, so
	// that a probe writing without end costs the server no more than the limit.
	//
	// A check that fails says why in a line or two, and a probe's output is never
	// written to the workload's log, so what this keeps is the whole of what an
	// operator sees of it.
	Tail struct {
		mux  sync.Mutex
		kept []byte
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
		// This is separate from the check takt performs: an image may carry a
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

	// The Usage type describes what one running instance is consuming, as its
	// runtime reports it at one moment.
	//
	// It is observed state, and raw: a driver reports counters rather than rates,
	// because a rate needs two readings and a driver is asked for one. Whoever holds
	// the previous reading turns the processor time into a rate.
	Usage struct {
		// The memory the instance is using, in bytes. Inactive page cache is left
		// out, because the kernel reclaims it before a limit is enforced. Docker
		// reports the same figure, so the two agree.
		Memory uint64
		// The processor time the instance has consumed since it started, summed
		// across every processor.
		CPU time.Duration
		// The number of processes and threads the instance is running.
		Pids int
		// When the runtime took the reading.
		At time.Time
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
		// one leaves the workload unreachable at an address takt reports.
		Protocol string
	}

	// The Volume type describes a volume a workload mounts, with the path it lives
	// at on the host already resolved.
	//
	// A driver is given the path rather than the name, so that nothing about how a
	// volume is stored has to be understood by the runtimes that mount one.
	Volume struct {
		// The name that identifies the volume. A path mount has no name of its
		// own, so it carries the host path here and anything reporting the mount
		// names it by that.
		Name string
		// Where the volume's data is on the host.
		Host string
		// Where the workload finds it. What that means is the driver's business: a
		// path inside a container, or one resolved against an exec workload's own
		// working directory.
		Target string
		// Whether the workload may only read what is mounted. Validation proves
		// this reaches only runtimes that can enforce it.
		ReadOnly bool
		// How mount events travel between the host and the container, in
		// docker's spelling. Validation proves this reaches only a host path
		// on a runtime that can honour it. Empty carries nothing.
		Propagation string
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

// How much of what a probe writes a Tail keeps.
const tailLimit = 1024

// Write keeps the last of everything written so far, up to the limit.
func (t *Tail) Write(p []byte) (int, error) {
	t.mux.Lock()
	defer t.mux.Unlock()

	t.kept = append(t.kept, p...)
	if len(t.kept) > tailLimit {
		t.kept = slices.Clone(t.kept[len(t.kept)-tailLimit:])
	}

	return len(p), nil
}

// Suffix returns what was kept, trimmed and formatted to follow an exit status in
// an error, or nothing when nothing was written.
func (t *Tail) Suffix() string {
	t.mux.Lock()
	defer t.mux.Unlock()

	text := strings.TrimSpace(string(t.kept))
	if text == "" {
		return ""
	}

	return ": " + text
}

// NewWorkload maps a stored workload onto the shape a driver runs, decoding the
// specification the server persisted.
//
// The runtime block is left in Spec rather than pulled apart here: which block matters
// is the driver's business, and a server that understood each of them would have to
// change every time a runtime was added.
//
// hostPaths is the list of prefixes a path mount may sit beneath. Each path mount
// is checked against it again here, and the driver is handed the path with its
// links followed, so what a driver binds is what was checked. The tree can change
// between an apply and a start, and a link swapped in beneath an allowed prefix
// would otherwise carry the mount wherever it pointed.
func NewWorkload(row database.Workload, hostPaths []HostPath) (Workload, error) {
	spec, err := manifest.DecodeWorkload(row.Spec)
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
		Env:      spec.Env,
	}

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
			// Which source a mount names is asked of the manifest package rather
			// than inferred from which field is set. A mounted secret or variable
			// is written as the workload starts and added to this by whoever wrote
			// it, so that nothing about a value ever reaches a stored
			// specification.
			kind, err := manifest.KindOf(mount)
			if err != nil {
				continue
			}

			switch kind {
			case manifest.MountVolume:
				// Unresolved for the same reason a port can be: the server had not
				// finished settling the workload. Mounting nothing would be worse
				// than waiting, since the workload would start and write somewhere
				// else.
				if mount.From == "" {
					continue
				}

				w.Volumes = append(w.Volumes, Volume{
					Name:     mount.Name,
					Host:     mount.From,
					Target:   mount.To,
					ReadOnly: mount.ReadOnly,
				})
			case manifest.MountPath:
				host, err := ResolveHostPath(mount.Path, mount.ReadOnly, hostPaths)
				if err != nil {
					return Workload{}, err
				}

				w.Volumes = append(w.Volumes, Volume{
					Name:        mount.Path,
					Host:        host,
					Target:      mount.To,
					ReadOnly:    mount.ReadOnly,
					Propagation: mount.Propagation,
				})
			}
		}
	}

	return w, nil
}

// ResolveHostPath reports where a path mount reaches once every symbolic link in it
// is followed, and refuses one that reaches outside the given prefixes.
//
// Both the path and each prefix are resolved before they are compared, so a link
// beneath an allowed directory cannot carry a mount outside it, and a prefix that is
// itself a link — /var/run on most hosts — still covers what it points at. A path
// that does not exist yet is resolved as far as it does, since a mount may name
// something that appears later. A prefix of / opens everything.
//
// The most specific prefix the path sits under is the one that decides. A
// read-only / beside a writable /mnt/media then means a mount under /mnt/media may
// write and one under /etc may not, where the first match to be listed would let
// either entry shadow the other. readOnly is what the mount asked for, and a prefix
// granted for reading only refuses a mount that did not ask.
//
// A read-only grant holds for regular files and directories only. The kernel
// applies a read-only mount to writes through the filesystem, and a connect on a
// unix socket, a write to a FIFO or an ioctl on a device is none of those, so a
// socket or device beneath a read-only prefix is refused however the mount was
// asked for. A more specific prefix without the suffix opens it. A path that does
// not exist yet is not judged, since the check runs again as each instance starts.
//
// Returns ErrHostPathDenied when the path sits under no prefix,
// ErrHostPathReadOnly when the prefix it sits under is granted read-only and the
// mount is not, and ErrHostPathNotFile when a read-only prefix covers something
// other than a regular file or a directory.
func ResolveHostPath(path string, readOnly bool, prefixes []HostPath) (string, error) {
	resolved, err := resolve(path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve host path %q: %w", path, err)
	}

	var matched HostPath
	var found bool
	var depth int
	for _, prefix := range prefixes {
		root, err := resolve(prefix.Path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve allowed host path %q: %w", prefix.Path, err)
		}

		if root != string(filepath.Separator) && resolved != root &&
			!strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			continue
		}

		if !found || len(root) > depth {
			matched, found, depth = prefix, true, len(root)
		}
	}

	switch {
	case found && matched.ReadOnly && !readOnly:
		return "", fmt.Errorf("%w: %q is under %q, which this server's workload allow-host-paths "+
			"configuration grants for reading only, so the mount must say readOnly: true",
			ErrHostPathReadOnly, path, matched.Path)
	case found && matched.ReadOnly:
		if kind, ok := specialFile(resolved); ok {
			return "", fmt.Errorf("%w: %q is a %s under %q, which this server's workload "+
				"allow-host-paths configuration grants for reading only, and a read-only mount "+
				"does not stop writes to a %s", ErrHostPathNotFile, path, kind, matched.Path, kind)
		}

		return resolved, nil
	case found:
		return resolved, nil
	}

	if resolved != filepath.Clean(path) {
		return "", fmt.Errorf("%w: %q reaches %q, which is not under a prefix this server's "+
			"workload allow-host-paths configuration names", ErrHostPathDenied, path, resolved)
	}

	return "", fmt.Errorf("%w: %q is not under a prefix this server's workload "+
		"allow-host-paths configuration names", ErrHostPathDenied, path)
}

// resolve follows every symbolic link in path. The part of the path that does not
// exist is carried across unchanged beneath the deepest ancestor that does.
func resolve(path string) (string, error) {
	path = filepath.Clean(path)

	var rest string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		switch {
		case err == nil:
			return filepath.Join(resolved, rest), nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}

		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}

		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// specialFile reports whether path is a socket, a FIFO or a device, and which. It
// is asked of a resolved path, so the leaf is never a link, and a path that does
// not exist is not special.
func specialFile(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}

	switch mode := info.Mode(); {
	case mode&fs.ModeSocket != 0:
		return "socket", true
	case mode&fs.ModeNamedPipe != 0:
		return "FIFO", true
	case mode&fs.ModeDevice != 0:
		return "device", true
	}

	return "", false
}
