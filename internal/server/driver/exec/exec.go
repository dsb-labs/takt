// Package exec provides the driver that runs workloads as processes on the host.
//
// It is the counterpart to the docker driver for work that has no image: a command,
// its arguments, and an environment. The command is started directly rather than
// through a shell, so nothing has to decide how to split it and no shell is involved
// unless the command names one.
//
// A process outlives the server that started it. Orca releases it on shutdown and
// re-adopts it on the next start, which is what makes restarting the server a
// different thing from restarting the workloads it runs.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dsb-labs/orca/internal/server/driver"
)

// Name is how this driver identifies itself, and is what the server maps a workload's
// runtime onto when deciding which driver runs it.
const Name = "exec"

const (
	// The file holding what the driver knows about an instance.
	stateFile = "state.json"
	// The file the process's output is written to. Both streams go to the one file so
	// that reading it back gives them interleaved in the order they were written,
	// which is what the docker driver's demultiplexing produces.
	outputFile = "output.log"
	// The directory the process runs in.
	workingDir = "cwd"
	// The tree holding the driver's own records, which no workload runs inside.
	stateDir = "state"
	// The tree holding the directories workloads run in.
	workloadDir = "workloads"
	// How long a process is given to stop on its own before it is killed.
	stopGrace = 10 * time.Second
)

var (
	// ErrNotExecWorkload is returned when the driver is given a workload whose
	// specification carries no exec block.
	ErrNotExecWorkload = errors.New("workload does not describe a command")
	// ErrInvalidWorkloadID is returned when a workload's identifier could not be used
	// as a directory beneath the driver's root.
	ErrInvalidWorkloadID = errors.New("workload identifier is not usable as a directory")
	// ErrUnknownWorkload is returned when the driver has no record of a workload it
	// was asked about by name.
	ErrUnknownWorkload = errors.New("driver has no record of the workload")
)

// The identifiers orca assigns are xid values: twenty lowercase alphanumeric
// characters. Checked rather than trusted, because this driver removes directories and
// should not build a path from a value it has not looked at.
var idPattern = regexp.MustCompile(`^[0-9a-v]{20}$`)

type (
	// The Driver type runs workloads as processes on the host.
	Driver struct {
		logger *slog.Logger
		// Where the driver keeps its own records, which a workload has no path to.
		state string
		// Where a workload keeps its working directory and its output, which it
		// necessarily can reach.
		workloads string

		// Guards the supervised set, which the reconciler's goroutine and every
		// supervising goroutine both touch.
		mux sync.Mutex
		// The processes this server started and is still waiting on, keyed by
		// workload. A process adopted from an earlier server is absent: nothing here
		// can wait for a process it did not start.
		supervised map[string]*supervised
		// Reports that a supervised process has ended, so the reconciler converges
		// without waiting for its next tick.
		events chan driver.Event
		// Waits for supervising goroutines at shutdown, so none outlives the driver.
		waits sync.WaitGroup
	}

	// The Config type contains fields used to construct a Driver.
	Config struct {
		// The logger used for driver lifecycle events.
		Logger *slog.Logger
		// The directory beneath which the driver keeps everything, both its own
		// records and the directories it gives workloads.
		Root string
	}

	// The supervised type is a process this server started.
	supervised struct {
		cmd *exec.Cmd
		// Where the record for this process lives, which is in the driver's own tree
		// rather than the workload's.
		state string
	}
)

// New returns a Driver that runs processes under the root directory in config.
func New(config Config) *Driver {
	return &Driver{
		logger: config.Logger.With("component", "driver", "driver", Name),
		// Two trees rather than one. A workload runs in a directory of its own and can
		// reach everything beneath it, so the records the driver trusts to identify a
		// running process are kept where the workload has no path to them.
		state:      filepath.Join(config.Root, stateDir),
		workloads:  filepath.Join(config.Root, workloadDir),
		supervised: make(map[string]*supervised),
		// Buffered so that a process ending never blocks its own supervisor on a
		// reconciler that is mid-pass.
		events: make(chan driver.Event, 16),
	}
}

// Name returns the name this driver is registered under.
func (d *Driver) Name() string {
	return Name
}

// Start runs the workload's command, returning the identifier of the process.
//
// The process is given a directory of its own, a process group of its own, and an
// environment holding only what the workload asked for. Its output goes to a file in
// that directory, which is what Logs reads.
func (d *Driver) Start(ctx context.Context, w driver.Workload) (string, error) {
	spec := w.Spec.Exec
	if spec == nil {
		return "", ErrNotExecWorkload
	}

	if len(spec.Command) == 0 {
		return "", fmt.Errorf("%w: no command", ErrNotExecWorkload)
	}

	workload, err := d.version(d.workloads, w.ID, w.Version)
	if err != nil {
		return "", err
	}

	recordPath, err := d.version(d.state, w.ID, w.Version)
	if err != nil {
		return "", err
	}

	if err = os.MkdirAll(filepath.Join(workload, workingDir), 0o700); err != nil {
		return "", fmt.Errorf("failed to create workload directory: %w", err)
	}

	if err = os.MkdirAll(recordPath, 0o700); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	output, err := os.OpenFile(filepath.Join(workload, outputFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("failed to open workload output: %w", err)
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...) //nolint:gosec // the command is what the workload is
	cmd.Dir = filepath.Join(workload, workingDir)
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = environment(w.Env)

	// Its own process group, so that stopping the workload reaches whatever it
	// started rather than only the command itself. Setsid rather than Setpgid so the
	// process survives a signal sent to the server's own group, which is what makes a
	// long-running workload outlive an interrupted server.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err = cmd.Start(); err != nil {
		_ = output.Close()

		return "", fmt.Errorf("failed to start command: %w", err)
	}

	// Closed here rather than deferred: the child holds its own descriptor, so the
	// parent's copy has no further use and would otherwise be held for the life of
	// the server.
	_ = output.Close()

	ticks, err := startTicks(cmd.Process.Pid)
	if err != nil {
		// The process started, so it is running whether or not its start time could
		// be read. Recording zero means a later pass cannot confirm the pid is still
		// the same process, which is worse than failing here while the process can
		// still be stopped cleanly.
		_ = d.kill(cmd.Process.Pid)

		return "", fmt.Errorf("failed to read process start time: %w", err)
	}

	recorded := state{
		Workload:   w.Name,
		PID:        cmd.Process.Pid,
		StartTicks: ticks,
		SpecHash:   w.SpecHash,
		Version:    w.Version,
		StartedAt:  time.Now(),
	}

	if err = writeState(recordPath, recorded); err != nil {
		_ = d.kill(cmd.Process.Pid)

		return "", err
	}

	d.supervise(ctx, w.Name, &supervised{cmd: cmd, state: recordPath})

	d.logger.With("workload", w.Name, "pid", recorded.PID, "version", w.Version).Debug("process started")

	return strconv.Itoa(recorded.PID), nil
}

// supervise waits for a process and records how it ended.
//
// Waiting is what makes the exit code available: only the parent can collect it. A
// process adopted from an earlier server has no supervisor, so an exit that happens
// while orca is down leaves no code to record — Observe reports that as a failure,
// since a job that may not have finished is better run again than assumed complete.
func (d *Driver) supervise(ctx context.Context, workload string, process *supervised) {
	d.mux.Lock()
	d.supervised[workload] = process
	d.mux.Unlock()

	d.waits.Go(func() {
		err := process.cmd.Wait()

		d.mux.Lock()
		// Only forget this process if it is still the one registered. A replacement
		// started while this one was ending owns the entry now.
		if d.supervised[workload] == process {
			delete(d.supervised, workload)
		}
		d.mux.Unlock()

		code := 0

		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}

		if recorded, readErr := readState(process.state); readErr == nil {
			recorded.Ended = true
			recorded.ExitCode = code

			if writeErr := writeState(process.state, recorded); writeErr != nil {
				d.logger.With("workload", workload, "error", writeErr).Error("failed to record process exit")
			}
		}

		d.logger.With("workload", workload, "exit_code", code).Debug("process ended")

		// A hint that something moved, so the reconciler converges now rather than at
		// its next tick. Dropped rather than blocking when nothing is reading, since
		// the ticker covers it.
		select {
		case d.events <- driver.Event{Workload: workload}:
		case <-ctx.Done():
		default:
		}
	})
}

// Stop ends everything the driver runs for a workload and forgets its directories.
//
// The process group is signalled rather than the process, so that anything the command
// started is stopped with it. A group given time to stop and still running is killed:
// a workload that ignores the request must not be able to block the pass that asked.
func (d *Driver) Stop(ctx context.Context, id, workload string) error {
	// An orphan has no stored workload, so no identifier comes with it. The record in
	// each directory carries the name it belongs to, so the identifier is found by
	// reading those rather than by treating the name as a path.
	if id == "" {
		found, err := d.identify(workload)
		if err != nil {
			if errors.Is(err, ErrUnknownWorkload) {
				// Nothing here belongs to that workload, which is what an orphan of
				// another runtime looks like from here.
				return nil
			}

			return err
		}

		id = found
	}

	states, err := d.dir(d.state, id)
	if err != nil {
		d.logger.With("workload", workload, "error", err).Error("refusing to stop a workload")

		return err
	}

	directories, err := d.dir(d.workloads, id)
	if err != nil {
		return err
	}

	versions, err := d.versions(d.state, id)
	if err != nil {
		return err
	}

	for _, path := range versions {
		recorded, err := readState(path)
		if err != nil {
			// Nothing readable to stop. The directories are still removed below, since
			// a record that cannot be read describes nothing that can be converged.
			continue
		}

		if recorded.alive() {
			if err = d.terminate(ctx, recorded.PID); err != nil {
				return fmt.Errorf("failed to stop process: %w", err)
			}
		}
	}

	d.mux.Lock()
	delete(d.supervised, workload)
	d.mux.Unlock()

	// Removed only once nothing is running, so a workload's output survives for as
	// long as the workload does.
	for _, path := range []string{states, directories} {
		if err = os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove workload directory: %w", err)
		}
	}

	d.logger.With("workload", workload).Debug("workload stopped")

	return nil
}

// identify finds the identifier of a workload the driver has a record for, given only
// its name.
//
// Names are read from inside the records rather than from the directories holding them,
// which is what lets a directory be named for an identifier while a caller that only
// has a name can still find it.
func (d *Driver) identify(workload string) (string, error) {
	ids, err := d.known()
	if err != nil {
		return "", err
	}

	for _, id := range ids {
		versions, err := d.versions(d.state, id)
		if err != nil {
			continue
		}

		for _, path := range versions {
			recorded, err := readState(path)
			if err != nil {
				continue
			}

			if recorded.Workload == workload {
				return id, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %q", ErrUnknownWorkload, workload)
}

// signalable reports whether a pid is one this driver is willing to signal.
//
// Signals are sent to the negated pid, which addresses a process group. Two values
// make that mean something other than "the group this driver started": zero addresses
// the caller's own group, and one addresses every process the caller may signal at
// all. Neither can be a process the driver started, so both are refused rather than
// relied on to be absent.
func signalable(pid int) bool {
	return pid > 1
}

// terminate asks a process group to stop and kills it if it does not.
func (d *Driver) terminate(ctx context.Context, pid int) error {
	if !signalable(pid) {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to signal process group: %w", err)
	}

	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return d.kill(pid)
}

// kill ends a process group outright, for one that would not stop when asked.
func (d *Driver) kill(pid int) error {
	if !signalable(pid) {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to kill process group: %w", err)
	}

	return nil
}

// Observe reports an instance for every workload the driver has a record of.
//
// The records are the source of truth rather than the supervised set, because a
// process started by an earlier server has no supervisor and would otherwise be
// invisible — which would read as absent and have the workload started a second time.
func (d *Driver) Observe(_ context.Context) ([]driver.Instance, error) {
	ids, err := d.known()
	if err != nil {
		return nil, err
	}

	var instances []driver.Instance

	for _, id := range ids {
		versions, err := d.versions(d.state, id)
		if err != nil {
			// A directory the driver cannot have created, so there is nothing here it
			// can report on.
			d.logger.With("id", id, "error", err).Error("skipping an unusable state directory")

			continue
		}

		for _, path := range versions {
			recorded, err := readState(path)
			if err != nil {
				// A directory with no readable record describes nothing that can be
				// converged. Reporting an instance for it would have the reconciler
				// act on a workload it cannot identify.
				d.logger.With("id", id, "error", err).Debug("skipping unreadable instance state")

				continue
			}

			// The name comes from the record rather than from the directory, which is
			// named for the identifier. Nothing parses a path to learn a name.
			if recorded.Workload == "" {
				d.logger.With("id", id).Debug("skipping a record naming no workload")

				continue
			}

			instances = append(instances, instance(recorded.Workload, recorded))
		}
	}

	return instances, nil
}

// instance maps a record onto what the server reads.
func instance(workload string, recorded state) driver.Instance {
	out := driver.Instance{
		ID:        strconv.Itoa(recorded.PID),
		Workload:  workload,
		SpecHash:  recorded.SpecHash,
		Version:   recorded.Version,
		StartedAt: recorded.StartedAt,
		ExitCode:  recorded.ExitCode,
	}

	switch {
	case recorded.alive():
		out.State = driver.StateRunning
	case recorded.Ended && recorded.ExitCode == 0:
		out.State = driver.StateExited
	case recorded.Ended:
		out.State = driver.StateFailed
	default:
		// Gone, with no record of how it ended. Only the parent can collect an exit
		// code, so this is a process that ended while the server was down. Reported
		// as failed rather than as a clean exit: a job that may not have finished is
		// better run again than assumed complete.
		out.State = driver.StateFailed
	}

	return out
}

// Watch reports when a supervised process ends.
//
// There is no event stream to subscribe to, so the driver is its own source: the
// goroutine waiting on a process reports its exit. A process adopted from an earlier
// server produces no event, and the reconciler's ticker covers that.
func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	out := make(chan driver.Event)

	go func() {
		defer close(out)

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-d.events:
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// Logs writes the tail of a workload's output.
//
// Both streams were written to one file, so they come back interleaved in the order
// the process wrote them. A workload the driver has nothing for writes nothing, since
// the caller does not know which runtime holds it.
func (d *Driver) Logs(_ context.Context, out io.Writer, workload string, tail int) error {
	id, err := d.identify(workload)
	if err != nil {
		if errors.Is(err, ErrUnknownWorkload) {
			// A workload this driver does not run. The caller does not know which
			// runtime holds it, so writing nothing is the answer.
			return nil
		}

		return err
	}

	// Output lives in the workload's own tree, which is the one it writes to.
	versions, err := d.versions(d.workloads, id)
	if err != nil {
		return err
	}

	for _, path := range versions {
		if err = tailFile(out, filepath.Join(path, outputFile), tail); err != nil {
			return err
		}
	}

	return nil
}

// Release lets every supervised process outlive the driver.
//
// Called at shutdown, so that restarting the server is not the same thing as
// restarting the workloads it runs. The processes keep running and the next start
// adopts them from their records.
func (d *Driver) Release() {
	d.mux.Lock()
	defer d.mux.Unlock()

	for workload, process := range d.supervised {
		if err := process.cmd.Process.Release(); err != nil {
			d.logger.With("workload", workload, "error", err).Error("failed to release process")
		}
	}

	clear(d.supervised)
}

// Supervises reports whether the driver is still waiting on a workload's process.
//
// A supervised process is one this server started and can collect an exit code from. A
// released or adopted one is not, which is the difference between a workload that
// outlives the server and one that does not.
func (d *Driver) Supervises(workload string) bool {
	d.mux.Lock()
	defer d.mux.Unlock()

	_, ok := d.supervised[workload]

	return ok
}

// dir returns the directory holding a workload beneath the given tree.
//
// Directories are named for the workload's identifier rather than its name. An
// identifier is orca's own, where a name is the operator's handle and reaches a driver
// from places a manifest never validated — an orphan is named by a label on the work
// found running. Keying on the identifier means no name is ever a path component, so
// none has to be safe as one.
//
// The identifier is still checked, because a driver that removes directories should
// not build a path from a value it has not looked at.
func (d *Driver) dir(tree, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: %q", ErrInvalidWorkloadID, id)
	}

	return filepath.Join(tree, id), nil
}

// version returns the directory holding one version of a workload beneath the given
// tree.
func (d *Driver) version(tree, id string, version int) (string, error) {
	dir, err := d.dir(tree, id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, strconv.Itoa(version)), nil
}

// workloads lists the workloads the driver has directories for.
func (d *Driver) known() ([]string, error) {
	return d.subdirectories(d.state)
}

// versions returns the paths of every version of a workload the driver has a record
// for, oldest first, so that reading them yields a workload's history in order.
//
// The tree says which paths these are: records for observing what is running, workload
// directories for reading output.
func (d *Driver) versions(tree, id string) ([]string, error) {
	root, err := d.dir(tree, id)
	if err != nil {
		return nil, err
	}

	names, err := d.subdirectories(root)
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(root, name))
	}

	return paths, nil
}

// subdirectories lists the directories directly inside a path, reporting none when the
// path does not exist. A missing directory is a workload the driver has never run,
// which is not a failure.
func (d *Driver) subdirectories(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	return names, nil
}

// environment renders a workload's environment for a process.
//
// Only what the workload named is passed, plus a PATH. The server's own environment
// may hold credentials that are no business of a workload, and inheriting it would
// hand them to every process the driver starts. Without a PATH a command named by
// anything other than an absolute path could not be found at all.
func environment(env map[string]string) []string {
	out := make([]string, 0, len(env)+1)

	if _, ok := env["PATH"]; !ok {
		out = append(out, "PATH="+defaultPath)
	}

	for key, value := range env {
		out = append(out, key+"="+value)
	}

	return out
}

// The PATH given to a workload that names none of its own.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
