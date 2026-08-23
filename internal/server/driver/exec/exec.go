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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// Name is how this driver identifies itself, and is what the server maps a workload's
// runtime onto when deciding which driver runs it.
const Name = "exec"

const (
	// The file holding what the driver knows about an instance.
	stateFile = "state.json"
	// The file marking a record that describes an attempt which was stopped and kept,
	// rather than one waiting to be restarted. Its presence is the whole signal, so it
	// holds nothing.
	retainedFile = "retained"
	// The file holding the output of the attempt a replacement took the place of.
	//
	// A separate file rather than a marker beside the output, because a restart at an
	// unchanged version runs in the directory the previous attempt used: sharing one
	// file would append the new attempt's output to the old, and nothing could then say
	// where one ended and the other began. Renaming on stop is what separates them, and
	// it means the current attempt always opens a file of its own.
	previousFile = "previous.log"
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
	// ErrInvalidMount is returned when a volume could not be placed inside the
	// workload's working directory.
	ErrInvalidMount = errors.New("volume cannot be mounted there")
	// ErrUnknownWorkload is returned when the driver has no record of a workload it
	// was asked about by name.
	ErrUnknownWorkload = errors.New("driver has no record of the workload")
	// ErrUnknownSignal is returned when the driver is asked to send a signal it does
	// not recognise.
	ErrUnknownSignal = errors.New("signal is not one this driver sends")
)

// The signals this driver will send, keyed by the name a specification uses.
//
// A map rather than a parse of the name, so that the set is exactly what a manifest may
// ask for. Keyed on the manifest's own constants rather than on literals, so that the
// accepted set cannot drift from what a manifest may say.
//
// Nothing here stops a workload: whether one runs is the reconciler's decision, so a
// driver that could be asked to kill a process would be taking it.
var signals = map[string]syscall.Signal{
	string(manifest.SignalHUP):  syscall.SIGHUP,
	string(manifest.SignalUSR1): syscall.SIGUSR1,
	string(manifest.SignalUSR2): syscall.SIGUSR2,
}

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
		// The paths every workload may read, beyond the host's own files that a
		// command needs to run at all.
		allowed []string

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
		// Extra paths every workload may read, for a runtime that lives somewhere the
		// host's own directories do not cover.
		//
		// Read-only, and the operator's decision rather than a workload's: the API has
		// no authentication, so a workload able to widen its own confinement would undo
		// it.
		AllowPaths []string
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
		allowed:    config.AllowPaths,
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
//
// It is also confined: the kernel refuses it every path outside its own directory, the
// volumes it mounts, the values it mounts and the host's own files. A host whose kernel
// cannot do that is refused here rather than running the workload unconfined.
func (d *Driver) Start(ctx context.Context, w driver.Workload) (string, error) {
	spec := w.Spec.Exec
	if spec == nil {
		return "", ErrNotExecWorkload
	}

	if len(spec.Command) == 0 {
		return "", fmt.Errorf("%w: no command", ErrNotExecWorkload)
	}

	// Asked before anything is created, so a host that cannot confine leaves no
	// half-started workload behind.
	if err := Confinable(); err != nil {
		return "", err
	}

	workload, err := d.version(d.workloads, w.ID, w.Version)
	if err != nil {
		return "", err
	}

	recordPath, err := d.version(d.state, w.ID, w.Version)
	if err != nil {
		return "", err
	}

	cwd := filepath.Join(workload, workingDir)

	if err = os.MkdirAll(cwd, 0o700); err != nil {
		return "", fmt.Errorf("failed to create workload directory: %w", err)
	}

	if err = os.MkdirAll(recordPath, 0o700); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	// A restart at an unchanged version reuses the directory the previous attempt was
	// stopped in, so the mark left there describes the attempt this one replaces. Left in
	// place it would report a running process as one kept for its output.
	if err = unretain(recordPath); err != nil {
		return "", err
	}

	if err = mount(cwd, w.Volumes); err != nil {
		return "", err
	}

	env := environment(w.Env)

	// Resolved before the ruleset is built, because a grant is a path and a command may
	// name none: "sh" is found on a PATH. Looked up against the workload's own PATH
	// rather than the server's, so the binary granted is the one the exec will find.
	//
	// After the working directory exists, since a command named relatively is resolved
	// against it.
	command, err := resolve(spec.Command, env, cwd)
	if err != nil {
		return "", err
	}

	// Opened before the process is confined, and stays writable to it afterwards: a
	// descriptor already open is not reached through a path, so the ruleset does not
	// have to grant the file the driver's own tree holds.
	output, err := os.OpenFile(filepath.Join(workload, outputFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("failed to open workload output: %w", err)
	}

	// Orca itself rather than the workload's command. The process confines itself and
	// then becomes the command, which is the only point a ruleset can be applied: after
	// the fork, so it restricts the workload rather than the server, and before the
	// exec, so the command never runs unconfined.
	self, err := os.Executable()
	if err != nil {
		_ = output.Close()

		return "", fmt.Errorf("failed to locate the orca binary: %w", err)
	}

	cmd := exec.Command(self, confineArg)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = env

	// Its own process group, so that stopping the workload reaches whatever it
	// started rather than only the command itself. Setsid rather than Setpgid so the
	// process survives a signal sent to the server's own group, which is what makes a
	// long-running workload outlive an interrupted server.
	//
	// The exec keeps it, so the command lands in the group the driver recorded.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	rules, status, closePipes, err := pipes(cmd)
	if err != nil {
		_ = output.Close()

		return "", err
	}

	if err = cmd.Start(); err != nil {
		_ = output.Close()
		closePipes()

		return "", fmt.Errorf("failed to start command: %w", err)
	}

	// Closed here rather than deferred: the child holds its own descriptors, so the
	// parent's copies have no further use and would otherwise be held for the life of
	// the server. The status pipe in particular only reports the workload is running by
	// reaching end of file, which it cannot while this process can still write to it.
	_ = output.Close()
	closePipes()

	if err = confineWith(cmd, rules, status, ruleset{
		Command: command,
		// Its own directory and its volumes, which is everything it may write. The
		// volumes are named by where they are on the host rather than by the link
		// inside the working directory, because the kernel resolves a link before it
		// decides.
		Write: append([]string{cwd}, hosts(w.Volumes)...),
		Read:  d.allowed,
		// A mounted value is a file rather than a directory, and lives beside every
		// other workload's. Granting the file rather than the directory is what stops
		// one workload reading another's.
		Files: append(files(w.Volumes), command[0]),
	}); err != nil {
		// The process is confined or it is not running, so there is nothing left to
		// supervise either way.
		_ = d.kill(cmd.Process.Pid)

		return "", err
	}

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

// Stop ends everything the driver runs for a workload, keeping the output of the
// version it most recently ran so that it can still be read.
//
// The process group is signalled rather than the process, so that anything the command
// started is stopped with it. A group given time to stop and still running is killed:
// a workload that ignores the request must not be able to block the pass that asked.
//
// One version's output is kept for the reason the docker driver keeps a container: the
// reconciler stops a workload before it starts it, so removing everything here is what
// used to discard the output of the attempt that just failed. What survives is that
// version's output.log and its record, marked retained. The working directory goes with
// the rest, because it holds the symlinks to the workload's volumes and nothing is left
// to read them.
//
// Every other version goes, in both trees. Keeping one version rather than all of them
// bounds what a workload crashing in a loop leaves on the disk.
func (d *Driver) Stop(ctx context.Context, id, workload string) error {
	found, err := d.resolve(workload, id)
	if err != nil || found == "" {
		return err
	}

	id = found

	versions, err := d.halt(ctx, id, workload)
	if err != nil {
		return err
	}

	keep := newest(versions)

	for _, path := range versions {
		if path == keep {
			continue
		}

		if err = d.discardVersion(id, path); err != nil {
			return err
		}
	}

	if keep == "" {
		return nil
	}

	if err = d.keep(id, keep); err != nil {
		return err
	}

	d.logger.With("workload", workload).Debug("workload stopped, keeping its output")

	return nil
}

// Discard ends everything the driver runs for a workload and removes both of its trees,
// including the output Stop kept.
//
// This is what a delete and an orphan take, where Stop is what a replacement takes. The
// kept output exists so that an operator can read why the previous attempt failed, and a
// workload nobody asked for has no such reader.
func (d *Driver) Discard(ctx context.Context, id, workload string) error {
	found, err := d.resolve(workload, id)
	if err != nil || found == "" {
		return err
	}

	if _, err = d.halt(ctx, found, workload); err != nil {
		return err
	}

	for _, tree := range []string{d.state, d.workloads} {
		path, err := d.dir(tree, found)
		if err != nil {
			return err
		}

		if err = os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove workload directory: %w", err)
		}
	}

	d.logger.With("workload", workload).Debug("workload discarded")

	return nil
}

// halt ends every process the driver runs for a workload and returns the version
// directories it has records in, so that a caller can decide what to keep.
func (d *Driver) halt(ctx context.Context, id, workload string) ([]string, error) {
	versions, err := d.versions(d.state, id)
	if err != nil {
		return nil, err
	}

	for _, path := range versions {
		recorded, err := readState(path)
		if err != nil {
			// Nothing readable to stop. The directory is still dealt with by the caller,
			// since a record that cannot be read describes nothing that can be
			// converged.
			continue
		}

		if recorded.alive() {
			if err = d.terminate(ctx, recorded.PID); err != nil {
				return nil, fmt.Errorf("failed to stop process: %w", err)
			}
		}
	}

	d.mux.Lock()
	delete(d.supervised, workload)
	d.mux.Unlock()

	return versions, nil
}

// resolve finds the identifier of the workload a caller means, reporting an empty one
// when the driver has no record of it.
//
// An orphan has no stored workload, so no identifier comes with it. The record in each
// directory carries the name it belongs to, so the identifier is found by reading those
// rather than by treating the name as a path.
func (d *Driver) resolve(workload, id string) (string, error) {
	if id != "" {
		return id, nil
	}

	found, err := d.identify(workload)
	if err != nil {
		if errors.Is(err, ErrUnknownWorkload) {
			// Nothing here belongs to that workload, which is what a workload of
			// another runtime looks like from here.
			return "", nil
		}

		return "", err
	}

	return found, nil
}

// keep sets a version's output aside as the previous attempt's and removes the directory
// the process ran in.
//
// The output is moved rather than left where it is, because a restart at an unchanged
// version reuses this directory: the next attempt opens the output file afresh, and
// without the move it would append to the attempt this one is keeping.
//
// The working directory goes because it holds the symlinks to the workload's volumes and
// whatever the command wrote beside them, none of which has a reader once the process has
// ended.
func (d *Driver) keep(id, path string) error {
	recorded, err := readState(path)
	if err != nil {
		// No record to read, so there is no version to find the output under. Observe
		// skips such a version anyway, and the files are left rather than moved on the
		// strength of a record that could not be read.
		return nil
	}

	workload, err := d.version(d.workloads, id, recorded.Version)
	if err != nil {
		return err
	}

	if err = os.RemoveAll(filepath.Join(workload, workingDir)); err != nil {
		return fmt.Errorf("failed to remove workload directory: %w", err)
	}

	// Renamed over whatever the last stop kept, so one attempt's output is held rather
	// than accumulating one file per attempt. A workload that has written nothing has no
	// file to move, which is not a failure.
	err = os.Rename(filepath.Join(workload, outputFile), filepath.Join(workload, previousFile))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to keep the previous output: %w", err)
	}

	return retain(path)
}

// discardVersion removes one version of a workload from both trees.
func (d *Driver) discardVersion(id, path string) error {
	recorded, err := readState(path)
	if err == nil {
		workload, err := d.version(d.workloads, id, recorded.Version)
		if err != nil {
			return err
		}

		if err = os.RemoveAll(workload); err != nil {
			return fmt.Errorf("failed to remove workload directory: %w", err)
		}
	}

	if err = os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to remove workload directory: %w", err)
	}

	return nil
}

// newest returns the version directory holding the most recent record, which is the one
// worth keeping for its output.
//
// Ordered by the version the record names rather than by the directory's name, so
// nothing depends on how a path sorts — "10" sorts before "9" as text.
func newest(versions []string) string {
	var (
		newest  string
		version = -1
	)

	for _, path := range versions {
		recorded, err := readState(path)
		if err != nil {
			continue
		}

		if recorded.Version > version {
			newest, version = path, recorded.Version
		}
	}

	return newest
}

// Signal sends the named signal to every process the driver runs for the named
// workload.
//
// This exists for a workload that mounts a value and asked to be told when it changes
// rather than replaced.
//
// The process is signalled rather than its group, which is the opposite of what Stop
// does and deliberately so: stopping a workload has to reach whatever it started, where
// a reload is for the program that read the file. A group-wide reload would reach
// children that never asked for one.
func (d *Driver) Signal(_ context.Context, id, workload, signal string) error {
	sig, ok := signals[signal]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSignal, signal)
	}

	if id == "" {
		found, err := d.identify(workload)
		if err != nil {
			if errors.Is(err, ErrUnknownWorkload) {
				// Nothing here belongs to that workload, which is what a workload of
				// another runtime looks like from here.
				return nil
			}

			return err
		}

		id = found
	}

	versions, err := d.versions(d.state, id)
	if err != nil {
		return err
	}

	for _, path := range versions {
		recorded, err := readState(path)
		if err != nil {
			// Nothing readable to signal. A record that cannot be read describes
			// nothing that can be converged, which Observe already reports.
			continue
		}

		if !recorded.alive() {
			continue
		}

		if !signalable(recorded.PID) {
			return fmt.Errorf("refusing to signal pid %d", recorded.PID)
		}

		if err = syscall.Kill(recorded.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to signal process: %w", err)
		}

		d.logger.With("workload", workload, "pid", recorded.PID, "signal", signal).Debug("process signalled")
	}

	return nil
}

// mount places each of the workload's volumes inside its working directory.
//
// The volume ends up at the mount path taken as relative to that directory, so a
// workload reaches it by the relative path rather than by the absolute one its manifest
// names. Resolving the absolute path there would mean confining the process to its own
// directory, which needs privileges orca does not have.
//
// A symlink rather than a bind mount, because mounting requires privileges orca does
// not have: it runs as an ordinary user, and a workload reads and writes through a link
// perfectly well. The links are made fresh on every start, since stopping the workload
// removed the directory holding the last set. The link is the disposable part — the
// volume it points at is not.
//
// The target path comes from a specification submitted over the API, so it is placed
// through an os.Root opened on the working directory. Every name is then resolved by
// the kernel relative to that directory and one reaching outside it fails, rather than
// the containment depending on this function cleaning the path correctly.
func mount(cwd string, volumes []driver.Volume) error {
	if len(volumes) == 0 {
		return nil
	}

	root, err := os.OpenRoot(cwd)
	if err != nil {
		return fmt.Errorf("failed to open workload directory: %w", err)
	}

	defer root.Close()

	for _, volume := range volumes {
		// The working directory is the workload's root, so a leading slash means that
		// directory rather than the host's root. Cleaning first collapses a path
		// trying to climb out into one that cannot, and os.Root refuses what is left
		// if it still escapes.
		target := strings.TrimPrefix(filepath.Clean("/"+volume.Target), "/")
		if target == "" {
			return fmt.Errorf("%w: volume %q names no path to mount at", ErrInvalidMount, volume.Name)
		}

		if parent := filepath.Dir(target); parent != "." {
			if err = root.MkdirAll(parent, 0o700); err != nil {
				return fmt.Errorf("%w: volume %q: %w", ErrInvalidMount, volume.Name, err)
			}
		}

		// The link points outside the root, which is the whole point: a volume
		// outlives the workload, so it cannot live in a directory that is removed
		// with it.
		if err = root.Symlink(volume.Host, target); err != nil {
			return fmt.Errorf("%w: volume %q: %w", ErrInvalidMount, volume.Name, err)
		}
	}

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

			instances = append(instances, instance(recorded.Workload, recorded, retained(path)))
		}
	}

	return instances, nil
}

// instance maps a record onto what the server reads.
func instance(workload string, recorded state, retained bool) driver.Instance {
	out := driver.Instance{
		ID:        strconv.Itoa(recorded.PID),
		Workload:  workload,
		SpecHash:  recorded.SpecHash,
		Version:   recorded.Version,
		StartedAt: recorded.StartedAt,
		ExitCode:  recorded.ExitCode,
		Retained:  retained,
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
// Which output that is depends on options: the version now running by default, and the
// one a replacement kept when Previous is asked for. The two are never combined, for the
// reason the docker driver does not combine them — the result would be two runs spliced
// together with nothing marking the boundary.
//
// Both streams were written to one file, so they come back interleaved in the order
// the process wrote them. A workload the driver has nothing for writes nothing, since
// the caller does not know which runtime holds it.
func (d *Driver) Logs(_ context.Context, out io.Writer, workload string, options driver.LogOptions) error {
	id, err := d.identify(workload)
	if err != nil {
		if errors.Is(err, ErrUnknownWorkload) {
			// A workload this driver does not run. The caller does not know which
			// runtime holds it, so writing nothing is the answer.
			return nil
		}

		return err
	}

	// Output lives in the workload's own tree, which is the one it writes to. Which file
	// holds which attempt is the whole of the bookkeeping: the current one writes to
	// outputFile, and a stop moves it aside to previousFile.
	versions, err := d.versions(d.workloads, id)
	if err != nil {
		return err
	}

	name := outputFile
	if options.Previous {
		name = previousFile
	}

	for _, path := range versions {
		if err = tailFile(out, filepath.Join(path, name), options.Tail); err != nil {
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

// resolve returns the workload's command with its binary named by an absolute path.
//
// A confined workload is granted its binary, and a grant is a path: "sh" is not one, so
// it has to be found before the ruleset is built. The lookup uses the workload's own
// PATH rather than the server's, so the file granted is the one the exec will find.
//
// A command that cannot be found is reported here, where the failure names it, rather
// than as a ruleset that could not be populated.
func resolve(command, env []string, cwd string) ([]string, error) {
	// Absolute already, so there is nothing to look up and nothing about the
	// environment that could change what runs.
	if filepath.IsAbs(command[0]) {
		return command, nil
	}

	// A name with a separator in it is relative to the working directory, which the
	// process starts in. Resolved against that rather than searched for, because a
	// PATH lookup is only for a bare name.
	if strings.Contains(command[0], string(filepath.Separator)) {
		return named(command, filepath.Join(cwd, command[0]))
	}

	for _, dir := range filepath.SplitList(pathOf(env)) {
		if dir == "" {
			continue
		}

		candidate := filepath.Join(dir, command[0])

		// Executable by somebody, which is what a PATH search looks for. Whether this
		// process may run it is the kernel's answer, and the exec reports it.
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return named(command, candidate)
		}
	}

	return nil, fmt.Errorf("%w: %s was not found on the workload's path", ErrNotExecWorkload, command[0])
}

// named returns the command with its binary replaced by the path it resolved to.
//
// A copy rather than the specification's own slice, which is decoded from the stored
// workload and has no business being rewritten.
func named(command []string, path string) ([]string, error) {
	out := slices.Clone(command)
	out[0] = path

	return out, nil
}

// pathOf returns the PATH from a rendered environment, which is what a bare command
// name is searched for on.
func pathOf(env []string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			return value
		}
	}

	return defaultPath
}
