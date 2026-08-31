//go:build linux

package exec

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/go-units"
	"golang.org/x/sys/unix"

	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/pkg/manifest"
)

var (
	// ErrNotEnforceable is returned when the host gives orca no cgroup subtree of its
	// own, which is what an exec workload's resource limits are enforced with.
	//
	// There is deliberately no degraded mode, for the reason confinement has none: a
	// limit that silently did not apply on some hosts would be a guarantee nothing
	// could reason about. In particular rlimits are not an approximation — RLIMIT_AS
	// caps address space rather than memory used, RLIMIT_NPROC counts the user's
	// processes rather than the workload's, and no rlimit bounds processor time the
	// way a quota does — so the same manifest field would mean something different
	// per runtime.
	ErrNotEnforceable = errors.New("host does not delegate a cgroup subtree to enforce resource limits with")
)

const (
	// Where the kernel mounts the cgroup hierarchy, which the path in
	// /proc/self/cgroup is relative to.
	cgroupMount = "/sys/fs/cgroup"
	// The cgroup the server moves itself into, leaving the delegated root empty of
	// processes.
	//
	// The kernel refuses to give a cgroup's children controllers while the cgroup
	// holds processes of its own, so a server sitting in the root of its delegation
	// could never limit anything. Named main because that is what systemd's
	// DelegateSubgroup= conventionally creates, and a server systemd already placed
	// there has nothing to move.
	serverCgroup = "main"
	// The prefix on every cgroup the driver creates for a workload, which is what
	// tells its own directories from anything else in the delegated subtree.
	cgroupPrefix = "orca-"
	// The process limit while the trampoline runs, for a workload that asked for a
	// smaller one. Enough threads for a Go runtime doing almost nothing, and a
	// bound rather than no limit, so even the moment before the exec cannot fork
	// freely.
	trampolinePids = 16
	// The controllers a workload's limits are written to, in the form
	// cgroup.subtree_control accepts.
	controllers = "+memory +cpu +pids"
)

// The cgroup type is the directory a limited workload runs in, held open so the
// process can be placed into it as it is created.
type cgroup struct {
	// Where the cgroup is, which is what the record keeps and cleanup removes.
	path string
	// The directory itself, passed to the kernel as the process is cloned so the
	// command never runs outside its limits.
	dir *os.File
	// The process limit the workload asked for, written only once the command has
	// replaced the trampoline. Zero when it asked for none.
	//
	// The other limits are written before the process exists, but this one cannot
	// be: the kernel counts threads, and the trampoline is a Go runtime holding
	// several where the command holds one until it says otherwise. Written up
	// front, a small legitimate limit would kill the trampoline rather than bound
	// the command.
	pids int
}

// Enforceable reports whether this host lets orca enforce resource limits on an exec
// workload, returning ErrNotEnforceable when it does not.
//
// Asked before a workload is started rather than discovered while starting one, for
// the reason Confinable is. Exported for the same reason too: the server refuses a
// manifest asking for limits it cannot enforce, and refusing at apply time needs the
// same answer starting the workload would get.
func Enforceable() error {
	_, err := delegated()

	return err
}

// Enforceable reports what the package-level Enforceable reports, from the driver
// itself. The server holds drivers rather than packages, and asks the one that
// would run a workload whether its host can enforce the limits.
func (d *Driver) Enforceable() error {
	return Enforceable()
}

// delegated finds the cgroup subtree the host has given orca to manage, asking only
// once: a process cannot change which cgroup it was started in, so the answer cannot
// change either.
var delegated = sync.OnceValues(delegation)

// delegation locates the delegated subtree and reports whether orca can enforce
// limits beneath it.
//
// The subtree is derived rather than configured: the cgroup this process is in is
// the one a service manager delegated, and a path in a configuration file would
// only ever restate it or contradict it. Enforcing takes the memory, cpu and pids
// controllers, and the right to create children and hand them controllers — which
// is what running under systemd with Delegate=yes grants, and what running as an
// ordinary user otherwise does not.
func delegation() (string, error) {
	own, err := ownCgroup()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotEnforceable, err)
	}

	// A server already inside its leaf is looking at a subtree prepared on an
	// earlier start, or one systemd's DelegateSubgroup= prepared for it. The
	// delegation is the parent either way.
	if filepath.Base(own) == serverCgroup {
		own = filepath.Dir(own)
	}

	data, err := os.ReadFile(filepath.Join(own, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotEnforceable, pathless(err))
	}

	available := strings.Fields(string(data))
	for _, controller := range []string{"memory", "cpu", "pids"} {
		if !slices.Contains(available, controller) {
			return "", fmt.Errorf("%w: the %s controller is not delegated (run orca under systemd with Delegate=yes)",
				ErrNotEnforceable, controller)
		}
	}

	for _, path := range []string{own, filepath.Join(own, "cgroup.subtree_control")} {
		if err = unix.Access(path, unix.W_OK); err != nil {
			return "", fmt.Errorf("%w: the subtree is not writable (run orca under systemd with Delegate=yes)",
				ErrNotEnforceable)
		}
	}

	return own, nil
}

// ownCgroup returns the cgroup this process is in, as a path under the mount.
func ownCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("failed to read this process's cgroup: %w", err)
	}

	// The entry for the v2 hierarchy, which is the only one with limits worth the
	// name. A host still mounting v1 controllers lists those first, so the lines
	// are searched rather than the first one taken.
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return filepath.Join(cgroupMount, rest), nil
		}
	}

	return "", errors.New("this process is not in a cgroup2 hierarchy")
}

// Guards preparation of the delegated subtree, which the first limited workload
// does for the life of the process. A flag rather than sync.Once, because a failed
// attempt has to be retried by the next workload rather than remembered forever —
// what fails here is a race with processes coming and going, not a fact about the
// host.
var (
	prepareMux  sync.Mutex
	prepareDone bool
)

// prepared makes the delegated subtree able to hold limited workloads, once.
//
// Deferred to the first workload that needs it rather than done at startup, so a
// host that never limits anything is left exactly as it was.
func prepared() error {
	prepareMux.Lock()
	defer prepareMux.Unlock()

	if prepareDone {
		return nil
	}

	if err := prepare(); err != nil {
		return err
	}

	prepareDone = true

	return nil
}

// prepare empties the delegated root of processes and hands the root's children the
// controllers limits are written to.
//
// The vacating is what makes the second step legal: the kernel refuses controllers
// to a cgroup's children while the cgroup holds processes of its own, and the root
// holds at least the server — plus every unlimited workload it started, which
// inherited its cgroup. All of them move to the server's leaf, where nothing limits
// them and the same is true of anything they start afterwards, since a child
// inherits the cgroup of its parent.
//
// The two steps race with unlimited workloads still being started, whose processes
// appear in the root between the vacating and the enabling. The kernel reports
// that, so the pair is retried rather than ordered.
//
// Every step tolerates having already been done, because a restarted server
// prepares the same subtree its predecessor did — systemd's DelegateSubgroup= even
// creates the leaf itself.
func prepare() error {
	root, err := delegated()
	if err != nil {
		return err
	}

	leaf := filepath.Join(root, serverCgroup)
	if err = os.Mkdir(leaf, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create the server's cgroup: %w", pathless(err))
	}

	for attempt := 0; ; attempt++ {
		if err = vacate(root, leaf); err != nil {
			return err
		}

		err = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(controllers), 0o644)
		switch {
		case err == nil:
			sweep(root)

			return nil
		case !errors.Is(err, syscall.EBUSY) || attempt >= 4:
			return fmt.Errorf("failed to enable the resource controllers: %w", pathless(err))
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// vacate moves every process in the delegated root into the server's leaf.
//
// Every process rather than only the server: a workload without limits was cloned
// into whatever cgroup the server was in, and one process left behind is enough for
// the kernel to refuse the controllers.
func vacate(root, leaf string) error {
	data, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("failed to read the delegated root's processes: %w", pathless(err))
	}

	for line := range strings.Lines(string(data)) {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}

		// A process that ended between the read and the move needs no moving, and
		// the kernel says so rather than anything having to check first.
		err = os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(pid), 0o644)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to move a process out of the delegated root: %w", pathless(err))
		}
	}

	return nil
}

// sweep removes workload cgroups nothing accounts for, which a server that stopped
// between creating one and recording it leaves behind.
//
// Best effort, and deliberately so. A cgroup that refuses removal is one with
// processes still in it — a limited workload adopted from an earlier server — and
// the record naming it is what removes it when the workload stops.
//
// A young cgroup is left alone. Two processes can share one delegation — the test
// suites run that way — and another's cgroup is empty for the moment between its
// creation and its process being cloned into it. What this exists to remove has
// been abandoned for as long as its server has been gone, so ignoring the fresh
// costs nothing.
func sweep(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), cgroupPrefix) {
			continue
		}

		if info, err := entry.Info(); err != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}

		_ = os.Remove(filepath.Join(root, entry.Name()))
	}
}

// limit creates the cgroup a workload's resource limits are enforced by, returning
// nil for a workload that asked for none.
//
// The limits are written before the directory is handed back, so by the time a
// process can be placed in it every limit already applies. The identifier reaching
// the path was checked by the caller resolving the workload's directories, which
// happens before any of this.
func (d *Driver) limit(w driver.Workload) (*cgroup, error) {
	resources := w.Spec.Resources
	if resources == nil {
		return nil, nil
	}

	if err := prepared(); err != nil {
		return nil, fmt.Errorf("failed to prepare the cgroup subtree: %w", err)
	}

	root, err := delegated()
	if err != nil {
		return nil, err
	}

	// The instance is part of the name so that a workload's instances get cgroups
	// of their own: two sharing one would share the limits it enforces.
	path := filepath.Join(root, cgroupPrefix+w.ID+"-"+strconv.Itoa(w.Instance)+"-"+strconv.Itoa(w.Version))
	if err = os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to create the workload's cgroup: %w", pathless(err))
	}

	if err = limits(path, resources); err != nil {
		_ = os.Remove(path)

		return nil, err
	}

	dir, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)

		return nil, fmt.Errorf("failed to open the workload's cgroup: %w", pathless(err))
	}

	return &cgroup{path: path, dir: dir, pids: resources.Pids}, nil
}

// limits writes a workload's resource limits into its cgroup. A limit the
// specification does not name is left alone, which the kernel reads as unlimited.
//
// The values mean exactly what they mean on the container runtime, which is the
// point of accepting the same section: validation proved the memory size parses,
// so an error here means the stored specification and the rules have diverged.
func limits(path string, resources *manifest.Resources) error {
	if resources.Memory != "" {
		memory, err := units.RAMInBytes(resources.Memory)
		if err != nil {
			return fmt.Errorf("failed to parse memory limit %q: %w", resources.Memory, err)
		}

		if err = limitFile(path, "memory.max", strconv.FormatInt(memory, 10)); err != nil {
			return err
		}

		// Swap is pinned to zero so the limit is hard. Left alone, a workload at
		// its limit swaps rather than stops, and a limit the workload can swap
		// past does not mean what the manifest said. The container runtime pins it
		// the same way.
		if err = limitFile(path, "memory.swap.max", "0"); err != nil {
			return err
		}
	}

	if resources.CPU > 0 {
		// The kernel expresses a processor limit as a quota of microseconds per
		// period. Rounded rather than truncated so the workload gets the nearest
		// representable limit to the one it asked for.
		if err := limitFile(path, "cpu.max", fmt.Sprintf("%d 100000", int64(math.Round(resources.CPU*100000)))); err != nil {
			return err
		}
	}

	if resources.Pids > 0 {
		// The trampoline's allowance when the workload asked for less, since the
		// kernel would count the trampoline's threads against the workload's
		// limit. The limit asked for is written once the command is running, which
		// restrictPids does.
		if err := limitFile(path, "pids.max", strconv.Itoa(max(resources.Pids, trampolinePids))); err != nil {
			return err
		}
	}

	return nil
}

// restrictPids tightens the process limit to the one the workload asked for, once
// the command has replaced the trampoline and holds a single thread.
//
// Tightening below the current count is legal: nothing dies, and new forks fail.
// A command that forks in the moment before the write runs against the
// trampoline's allowance rather than the limit, so an early fork can briefly
// exceed the limit — bounded by that allowance — and every fork after the write
// answers to the limit.
func (c *cgroup) restrictPids() error {
	if c == nil || c.pids == 0 {
		return nil
	}

	return limitFile(c.path, "pids.max", strconv.Itoa(c.pids))
}

// limitFile writes one limit into a cgroup, naming the limit when the kernel
// refuses it.
func limitFile(path, name, value string) error {
	if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", name, pathless(err))
	}

	return nil
}

// close gives up the held directory, once the process is placed or will never be.
func (c *cgroup) close() {
	if c != nil {
		_ = c.dir.Close()
	}
}

// discard removes a cgroup that ended up with no process to hold, closing it first.
func (c *cgroup) discard() {
	if c == nil {
		return
	}

	c.close()
	_ = discardCgroup(c.path)
}

// discardCgroup ends whatever a cgroup still holds and removes it.
//
// The kill reaches what signalling the process group cannot: a process that made a
// session of its own leaves the group, but nothing leaves the cgroup. Removal then
// waits briefly for the kernel to notice the deaths, since a cgroup is only
// removable once it is empty. A cgroup already gone is fine — the caller is asking
// for it to not exist, and it does not.
func discardCgroup(path string) error {
	err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to kill the workload's cgroup: %w", pathless(err))
	}

	deadline := time.Now().Add(stopGrace)
	for {
		err = os.Remove(path)
		switch {
		case err == nil || os.IsNotExist(err):
			return nil
		case !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline):
			return fmt.Errorf("failed to remove the workload's cgroup: %w", pathless(err))
		}

		time.Sleep(50 * time.Millisecond)
	}
}
