// Package e2e provides end-to-end tests that exercise a real takt server against a
// real Docker daemon.
//
// These tests are the only place takt's layers are exercised together as an operator
// uses them: a manifest goes in through the client and containers come out on the
// daemon. They deliberately cover whole journeys rather than individual behaviours —
// the unit tests own the edge cases — and they are what catches the mistakes that
// only appear when the real runtime is involved, such as a container removal racing
// the shutdown it was meant to follow.
//
// The suite is not parallel, and cannot be. Every server shares one Docker daemon,
// and the reconciler stops any takt-labelled container that no workload asks for, so
// two servers running at once would tear down each other's work.
package e2e_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	execdriver "github.com/dsb-labs/takt/internal/server/driver/exec"
)

const (
	// The image the tests run. Small, quick to start, and long-running, so a
	// workload built from it stays up until takt stops it.
	testImage = "nginx:1.27-alpine"
	// How long to wait for the server to converge on a desired state. Generous
	// because the first workload to want the image pays to pull it, which is part
	// of what these tests are checking.
	convergeTimeout = 2 * time.Minute
)

// TestMain lets this test binary act as a confinement trampoline.
//
// The server runs inside the test process, so the binary it executes to start a
// confined exec workload is this one. Without this the workload would run the suite a
// second time instead of confining itself and becoming the command.
//
// The server the suite runs also assumes a delegated cgroup subtree, so it can
// enforce resource limits on exec workloads rather than refuse them. That is a
// property of how the suite was started: run it through "make e2e" or
// scripts/delegated.sh, which grant one.
func TestMain(m *testing.M) {
	execdriver.Confine()

	// Refused before any test runs, rather than skipped. The driver takes the cgroup
	// it was started in for its delegated subtree, and a terminal's own scope
	// qualifies: run there, the suite moves the terminal's processes into the leaf
	// it makes and runs workloads beside them, and terminals have died that way.
	// The script sets this for the scope it makes, and nothing else should.
	if os.Getenv("TAKT_DELEGATED_SCOPE") == "" {
		fmt.Fprintln(os.Stderr, `this suite prepares the cgroup it is started in: run it through "make e2e" or scripts/delegated.sh`)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func TestEndToEnd(t *testing.T) {
	// The suite needs a docker daemon and takes minutes rather than seconds, so the
	// main CI workflow leaves it out and the e2e workflow runs it. Saying so here is
	// what tells somebody reading a log that the suite was left out on purpose.
	if testing.Short() {
		t.Skip("end-to-end tests need a docker daemon: run without -short")
	}

	suite.Run(t, new(Suite))
}
