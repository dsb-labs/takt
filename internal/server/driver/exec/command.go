//go:build linux

package exec

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The suffix on the cgroup a command an operator runs gets, beside the instance's
// own. A random tail follows it, so two operators running commands in one instance
// at once never share a cgroup: killing one would end the other.
const execCgroupPrefix = "-exec-"

// Exec runs a command inside the running instance of a workload, copying its output
// to the given writers and its input from the given reader until it ends, and
// reports the status it exited with.
//
// The command runs as a probe does, which is to say as the instance does: in its
// working directory, under its ruleset, with its environment resolved again for
// this run, in a cgroup of its own beside the instance's under the same limits. An
// operator's command is something a person typed rather than something the
// manifest named, and the runtime's promise covers it all the same, because the
// instance is what they asked to look inside.
//
// A command that outlives the caller's context is killed, along with everything it
// started. The input is copied on a goroutine of its own, which ends when the
// reader does. A command that ended before its input did leaves that goroutine
// blocked on the reader, so the caller ends the reader when the session ends.
func (d *Driver) Exec(ctx context.Context, w driver.Workload, exec driver.Exec) (int, error) {
	cmd, limits, err := d.launch(ctx, w, exec, execCgroupPrefix+rand.Text())
	if err != nil {
		return 0, err
	}
	defer limits.discard()

	return d.wait(ctx, cmd)
}

// launch starts a command inside the running instance of a workload, confined as the
// instance is, returning the started command and the cgroup it runs in. The cgroup
// is nil for an instance that has none, and is the caller's to discard once the
// command has ended.
//
// This is the part a probe and an operator's command share. Everything up to the
// moment the command replaces the trampoline is the same for both: finding the
// instance, resolving the environment, the cgroup beside the instance's, the
// ruleset, the limits.
func (d *Driver) launch(ctx context.Context, w driver.Workload, exec driver.Exec, suffix string) (*osexec.Cmd, *cgroup, error) {
	if len(exec.Command) == 0 {
		return nil, nil, fmt.Errorf("%w: no command", ErrNotExecWorkload)
	}

	if err := Confinable(); err != nil {
		return nil, nil, err
	}

	recorded, err := d.running(w.ID, w.Instance)
	if err != nil {
		return nil, nil, err
	}

	workload, err := d.place(d.workloads, w.ID, w.Instance, recorded.Version)
	if err != nil {
		return nil, nil, err
	}

	cwd := filepath.Join(workload, workingDir)

	if exec.Env == nil {
		return nil, nil, errors.New("command names no environment to resolve")
	}

	resolved, err := exec.Env(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve environment for command: %w", err)
	}

	env := environment(resolved)

	command, err := resolve(exec.Command, env, cwd)
	if err != nil {
		return nil, nil, err
	}

	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to locate the takt binary: %w", err)
	}

	limits, err := sideCgroup(recorded, w.Spec.Resources, suffix)
	if err != nil {
		return nil, nil, err
	}

	cmd := osexec.Command(self, confineArg)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = exec.Stdout, exec.Stderr
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// The output is a pipe the driver copies from rather than a file, and Wait
	// waits for the copy to finish. A command that left a child holding the pipe
	// would otherwise hold the wait open after the command itself had ended.
	cmd.WaitDelay = time.Second

	if limits != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(limits.dir.Fd())
	}

	// The input is a pipe of the driver's own rather than the reader itself. Given
	// a reader, Wait would also wait for the copy from it to finish, and a reader
	// nobody has typed at yet never finishes: the command would have ended and the
	// wait would sit behind the operator's keyboard. A pipe handed to the child as
	// a file involves no copy Wait knows about, and the driver copies into it on a
	// goroutine the wait never sees.
	var input *os.File
	if exec.Stdin != nil {
		var writer *os.File

		input, writer, err = os.Pipe()
		if err != nil {
			limits.discard()

			return nil, nil, fmt.Errorf("failed to open the input pipe: %w", err)
		}

		cmd.Stdin = input

		go func() {
			_, _ = io.Copy(writer, exec.Stdin)
			_ = writer.Close()
		}()
	}

	rules, status, closePipes, err := pipes(cmd)
	if err != nil {
		closeInput(input)
		limits.discard()

		return nil, nil, err
	}

	if err = cmd.Start(); err != nil {
		closeInput(input)
		closePipes()
		limits.discard()

		return nil, nil, fmt.Errorf("failed to start command: %w", err)
	}

	closeInput(input)
	closePipes()
	limits.close()

	err = confineWith(cmd, rules, status, d.root, ruleset{
		Command: command,
		Write:   append([]string{cwd}, hosts(recorded.Volumes)...),
		Read:    d.allowed,
		Deny:    d.denied,
		Files:   append(files(recorded.Volumes), command[0]),
	})
	if err != nil {
		_ = d.kill(cmd.Process.Pid)
		_ = cmd.Wait()
		limits.discard()

		return nil, nil, err
	}

	if err = limits.restrict(); err != nil {
		_ = d.kill(cmd.Process.Pid)
		_ = cmd.Wait()
		limits.discard()

		return nil, nil, err
	}

	return cmd, limits, nil
}

// closeInput gives up the parent's end of the child's input, which is nil for a
// command given none. The child holds its own descriptor, so the parent's copy has
// no further use once the process has started.
func closeInput(input *os.File) {
	if input != nil {
		_ = input.Close()
	}
}

// wait waits for a command to end, killing it when the caller's context ends first,
// and reports the status it exited with.
//
// Wait collects the process's own exit, and the kill reaches its group, so a command
// that forked and then hung has everything it started ended with it. The context's
// error is what a cancelled command reports, since the exit status of a killed
// process says nothing an operator can act on.
func (d *Driver) wait(ctx context.Context, cmd *osexec.Cmd) (int, error) {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var err error

	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = d.kill(cmd.Process.Pid)
		<-waited

		return 0, ctx.Err()
	}

	exit, ok := errors.AsType[*osexec.ExitError](err)
	switch {
	case ok:
		return exit.ExitCode(), nil
	case err != nil:
		return 0, fmt.Errorf("failed to wait for command: %w", err)
	default:
		return 0, nil
	}
}

// running finds the record of an instance's process that is alive, reporting
// driver.ErrNoRunningInstance when none is.
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
		return state{}, fmt.Errorf("%w: instance %d", driver.ErrNoRunningInstance, instance)
	}

	recorded, err := d.states.read(current.path)
	if err != nil {
		return state{}, err
	}

	if !recorded.alive() {
		return state{}, fmt.Errorf("%w: instance %d", driver.ErrNoRunningInstance, instance)
	}

	return recorded, nil
}

// sideCgroup makes the cgroup a command runs in, beside the instance's own and under
// the limits the specification names, returning nil for an instance that has none.
//
// The process limit while the trampoline runs is the same allowance the instance's
// own trampoline had, for the same reason, and restrict tightens it once the command
// is running.
func sideCgroup(recorded state, resources *manifest.Resources, suffix string) (*cgroup, error) {
	if recorded.Cgroup == "" {
		return nil, nil
	}

	path := recorded.Cgroup + suffix
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to create the command's cgroup: %w", pathless(err))
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

		return nil, fmt.Errorf("failed to open the command's cgroup: %w", pathless(err))
	}

	return group, nil
}
