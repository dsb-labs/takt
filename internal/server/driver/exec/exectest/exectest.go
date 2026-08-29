// Package exectest gives a test binary the host facilities exec workloads need.
//
// The exec driver's tests want the same two things of the binary they run in that
// the server wants of orca: that it can act as the confinement trampoline, and that
// it sits in a delegated cgroup subtree so resource limits are enforceable. The
// first is the binary's own doing. The second is a grant only a service manager can
// make, which is what this package asks systemd for.
package exectest

import (
	"errors"
	"os"
	osexec "os/exec"

	"github.com/dsb-labs/orca/internal/server/driver/exec"
)

// The variable marking a binary that has already asked for delegation, so a host
// that grants a scope without the needed controllers runs the tests once — which
// fail, naming what is missing — rather than asking again forever.
const marker = "ORCA_TEST_DELEGATED"

// Redelegate re-executes the test binary inside a systemd scope that delegates a
// cgroup subtree, when the host has not already delegated one to it.
//
// Called from TestMain, after the Confine trampoline check: a binary started to
// become a workload's command must become it, not re-execute the suite. When the
// host already delegates — or offers no way to — this returns and the suite runs
// as it was started, with the tests needing delegation skipping.
//
// It does not return when it re-executes. The suite runs in the scope, its output
// arrives on the descriptors this process already holds, and its verdict is this
// process's exit code.
//
// When this cannot acquire delegation, the tests that need it fail naming what is
// missing rather than skip. The hosts the suite supports are ones systemd can
// grant a subtree on, which is the stance the confinement tests take about
// Landlock.
func Redelegate() {
	if exec.Enforceable() == nil {
		return
	}

	if os.Getenv(marker) != "" {
		return
	}

	if _, err := osexec.LookPath("systemd-run"); err != nil {
		return
	}

	// Asked with a command that does nothing, so a host without a user session —
	// where systemd-run fails for its own reasons — is found out here rather than
	// read as a failing suite.
	if err := osexec.Command("systemd-run", "--user", "--scope", "--quiet", "-p", "Delegate=yes", "true").Run(); err != nil {
		return
	}

	self, err := os.Executable()
	if err != nil {
		return
	}

	args := append([]string{"--user", "--scope", "--quiet", "-p", "Delegate=yes", self}, os.Args[1:]...)

	cmd := osexec.Command("systemd-run", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), marker+"=1")

	err = cmd.Run()
	if exit, ok := errors.AsType[*osexec.ExitError](err); ok {
		os.Exit(exit.ExitCode())
	}

	if err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}
