//go:build linux

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrNoRunningInstance is returned when a probe is asked of an instance the
	// driver has nothing running for.
	ErrNoRunningInstance = errors.New("no running instance to probe")
	// ErrProbeFailed is returned when a probe ran and exited with a status other
	// than zero. The error carries the status and the tail of what the command
	// wrote.
	ErrProbeFailed = errors.New("probe failed")
)

const (
	// How much of what a probe writes is kept for the error it reports. A check
	// that fails says why in a line or two, and the output is never written to the
	// workload's log, so this is the whole of what an operator sees of it.
	probeOutputLimit = 1024
	// The suffix on the cgroup a probe runs in, beside the workload's own.
	probeCgroupSuffix = "-probe"
)

// Probe runs a command health check for one instance of a workload, returning nil
// when the command exits zero.
//
// The command runs as the instance does. It starts in the instance's working
// directory, is confined to the same ruleset — the directory, the volumes the
// instance was started with, the host's own files — through the same trampoline the
// instance went through, and is given the environment the workload asked for,
// resolved again for this run. The promise the runtime makes is that nothing a
// manifest names runs unconfined, and a check is something a manifest names.
//
// Where the instance has a cgroup, the probe gets one of its own beside it under the
// same limits, rather than joining the instance's. The limits already written there
// are the command's, and the trampoline that confines the probe is a Go runtime that
// a small process limit would refuse to start. A cgroup of its own is also what makes
// the deadline certain: killing it reaches a probe that left its process group, and
// it is removed once the probe has ended.
//
// Output is captured to a bounded buffer for the error and never reaches the
// instance's log, which is the workload's own.
func (d *Driver) Probe(ctx context.Context, w driver.Workload, probe driver.Probe) error {
	if len(probe.Command) == 0 {
		return fmt.Errorf("%w: no command", ErrNotExecWorkload)
	}

	if err := Confinable(); err != nil {
		return err
	}

	recorded, err := d.running(w.ID, w.Instance)
	if err != nil {
		return err
	}

	workload, err := d.place(d.workloads, w.ID, w.Instance, recorded.Version)
	if err != nil {
		return err
	}

	cwd := filepath.Join(workload, workingDir)

	if probe.Env == nil {
		return errors.New("probe names no environment to resolve")
	}

	resolved, err := probe.Env(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve environment for probe: %w", err)
	}

	env := environment(resolved)

	command, err := resolve(probe.Command, env, cwd)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to locate the takt binary: %w", err)
	}

	limits, err := probeCgroup(recorded, w.Spec.Resources)
	if err != nil {
		return err
	}

	output := &tail{limit: probeOutputLimit}

	cmd := osexec.Command(self, confineArg)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// The output is a pipe the driver copies from rather than a file, and Wait
	// waits for the copy to finish. A probe that left a child holding the pipe
	// would otherwise hold the wait open after the probe itself had ended.
	cmd.WaitDelay = time.Second

	if limits != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(limits.dir.Fd())
	}

	rules, status, closePipes, err := pipes(cmd)
	if err != nil {
		limits.discard()

		return err
	}

	if err = cmd.Start(); err != nil {
		closePipes()
		limits.discard()

		return fmt.Errorf("failed to start probe: %w", err)
	}

	closePipes()
	limits.close()

	// Removed once the probe has ended however it ended, killing whatever it left
	// behind. The group kill below is what ends a probe that overran, and this is
	// what ends anything that escaped the group.
	defer limits.discard()

	err = confineWith(cmd, rules, status, ruleset{
		Command: command,
		Write:   append([]string{cwd}, hosts(recorded.Volumes)...),
		Read:    d.allowed,
		Deny:    d.denied,
		Files:   append(files(recorded.Volumes), command[0]),
	})
	if err != nil {
		_ = d.kill(cmd.Process.Pid)
		_ = cmd.Wait()

		return err
	}

	if err = limits.restrict(); err != nil {
		_ = d.kill(cmd.Process.Pid)
		_ = cmd.Wait()

		return err
	}

	return d.await(ctx, cmd, output)
}

// await waits for a probe to end, killing it when the caller's deadline passes
// first, and turns how it ended into the verdict.
//
// Wait collects the process's own exit, and the kill reaches its group, so a probe
// that forked and then hung has everything it started ended with it. The context's
// error is what a timed-out probe reports, since the exit status of a killed process
// says nothing an operator can act on.
func (d *Driver) await(ctx context.Context, cmd *osexec.Cmd, output *tail) error {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var err error

	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = d.kill(cmd.Process.Pid)
		<-waited

		return ctx.Err()
	}

	exit, ok := errors.AsType[*osexec.ExitError](err)
	switch {
	case ok:
		return fmt.Errorf("%w: exit status %d%s", ErrProbeFailed, exit.ExitCode(), tailOf(output))
	case err != nil:
		return fmt.Errorf("failed to wait for probe: %w", err)
	default:
		return nil
	}
}

// running finds the record of an instance's process that is alive, reporting
// ErrNoRunningInstance when none is.
//
// The newest record alone is considered: an older version of the slot is one the
// reconciler has stopped or is about to, and probing it would answer a question
// about an instance on its way out.
func (d *Driver) running(id string, instance int) (state, error) {
	records, err := d.records(id)
	if err != nil {
		return state{}, err
	}

	current := newest(ofInstance(records, instance))
	if current.path == "" {
		return state{}, fmt.Errorf("%w: instance %d", ErrNoRunningInstance, instance)
	}

	recorded, err := d.states.read(current.path)
	if err != nil {
		return state{}, err
	}

	if !recorded.alive() {
		return state{}, fmt.Errorf("%w: instance %d", ErrNoRunningInstance, instance)
	}

	return recorded, nil
}

// probeCgroup makes the cgroup a probe runs in, beside the instance's own and under
// the limits the specification names, returning nil for an instance that has none.
//
// The process limit while the trampoline runs is the same allowance the instance's
// own trampoline had, for the same reason, and restrict tightens it once the command
// is running.
func probeCgroup(recorded state, resources *manifest.Resources) (*cgroup, error) {
	if recorded.Cgroup == "" {
		return nil, nil
	}

	path := recorded.Cgroup + probeCgroupSuffix
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to create the probe's cgroup: %w", pathless(err))
	}

	group := &cgroup{path: path, resources: resources}

	if resources != nil && resources.Pids > 0 {
		if err := limitFile(path, "pids.max", strconv.Itoa(max(resources.Pids, trampolinePids))); err != nil {
			_ = os.Remove(path)

			return nil, err
		}
	}

	var err error
	if group.dir, err = os.Open(path); err != nil {
		_ = os.Remove(path)

		return nil, fmt.Errorf("failed to open the probe's cgroup: %w", pathless(err))
	}

	return group, nil
}

// The tail type is a writer that keeps the end of what is written to it, up to its
// limit, so that a probe writing without end costs the server no more than the
// limit.
type tail struct {
	mux   sync.Mutex
	limit int
	kept  []byte
}

// Write keeps the last limit bytes of everything written so far.
func (t *tail) Write(p []byte) (int, error) {
	t.mux.Lock()
	defer t.mux.Unlock()

	t.kept = append(t.kept, p...)
	if len(t.kept) > t.limit {
		t.kept = slices.Clone(t.kept[len(t.kept)-t.limit:])
	}

	return len(p), nil
}

// tailOf returns what a probe wrote, formatted to follow its exit status in an error,
// or nothing when it wrote nothing.
func tailOf(output *tail) string {
	output.mux.Lock()
	defer output.mux.Unlock()

	text := strings.TrimSpace(string(output.kept))
	if text == "" {
		return ""
	}

	return ": " + text
}
