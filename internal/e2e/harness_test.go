package e2e_test

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/orca/internal/server"
	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The Suite type runs an orca server for each test and exercises it through the
	// public client.
	//
	// The server runs inside the test process rather than as a built binary, so a
	// failure surfaces in the test output rather than in a log file that has to be
	// found, and so the test can stop and restart it directly.
	Suite struct {
		suite.Suite

		client *client.Client
		cancel context.CancelFunc
		done   *errgroup.Group
	}

	// The option type modifies how a test server is started.
	option func(*server.Config)
)

// withDataDirectory modifies the server to keep its database in the given directory,
// so that a later server can be started against the same desired state.
func withDataDirectory(directory string) option {
	return func(c *server.Config) { c.Data.Directory = directory }
}

// SetupSuite checks that the daemon these tests need is actually reachable, failing
// the whole suite up front rather than once per test.
func (s *Suite) SetupSuite() {
	s.Require().NoError(exec.Command("docker", "version").Run(), "docker daemon is not reachable")
}

// SetupTest starts a server for the test that is about to run.
func (s *Suite) SetupTest() {
	s.start()
}

// TearDownTest stops the server the test was using.
func (s *Suite) TearDownTest() {
	s.stop()
}

// start runs a server and points the suite's client at it.
func (s *Suite) start(options ...option) {
	config := server.DefaultConfig()
	config.HTTP.Address = "127.0.0.1:" + s.freePort()
	config.Data.Directory = s.T().TempDir()
	config.Logging.Level = "error"
	// Short enough that a test waiting for convergence isn't mostly waiting on the
	// ticker; driver events already cover the prompt cases.
	config.Reconcile.Interval = time.Second

	if testing.Verbose() {
		config.Logging.Level = "debug"
	}

	for _, option := range options {
		option(&config)
	}

	ctx, cancel := context.WithCancel(context.Background())
	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error { return server.Run(ctx, config) })

	c, err := client.New("http://" + config.HTTP.Address)
	s.Require().NoError(err)

	s.client, s.cancel, s.done = c, cancel, group

	// Run opens the database and connects to docker before it listens, so a test
	// has to wait for the listener rather than assume it.
	s.Require().Eventually(func() bool {
		return s.dial(config.HTTP.Address)
	}, 30*time.Second, 50*time.Millisecond, "server never started listening")
}

// stop shuts the current server down and waits for it to exit, reporting a failure if
// it stopped with an error. It is safe to call when no server is running.
func (s *Suite) stop() {
	if s.cancel == nil {
		return
	}

	s.cancel()
	s.NoError(s.done.Wait())

	s.client, s.cancel, s.done = nil, nil, nil
}

// restart stops the current server and starts a fresh one, which is how the tests
// exercise what survives a restart.
func (s *Suite) restart(options ...option) {
	s.stop()
	s.start(options...)
}

func (s *Suite) ctx() context.Context {
	return s.T().Context()
}

// containerSpec returns a specification for a long-running container publishing the
// given port mapping.
func (s *Suite) containerSpec(name, ports string) manifest.Spec {
	return manifest.Spec{
		Version: "v1",
		Name:    name,
		Labels:  map[string]string{"some-key": "some-value"},
		Container: &manifest.Container{
			Image: testImage,
			Env:   map[string]string{"EXAMPLE": "EXAMPLE"},
			Ports: []string{ports},
		},
	}
}

// workloadName derives a DNS-label-safe workload name from the running test's name,
// so a failing test leaves behind containers that say which test made them.
func (s *Suite) workloadName() string {
	name := strings.ToLower(s.T().Name())
	name = strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(name)

	return "e2e-" + name
}

// awaitState waits for the named workload to reach the given state and returns it.
func (s *Suite) awaitState(name, state string) client.Workload {
	var workload client.Workload

	s.Require().Eventuallyf(func() bool {
		var err error
		workload, err = s.client.Get(s.ctx(), name)

		return err == nil && workload.State == state
	}, convergeTimeout, 500*time.Millisecond, "workload %q never reached state %q", name, state)

	return workload
}

// awaitInstanceOtherThan waits for the named workload to be running an instance that
// isn't the given one, which is how a replacement is distinguished from the instance
// it replaced.
func (s *Suite) awaitInstanceOtherThan(name, previous string) {
	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil || len(workload.Instances) == 0 {
			return false
		}

		return workload.Instances[0].ID != previous && workload.Instances[0].State == "running"
	}, convergeTimeout, 500*time.Millisecond, "workload %q never replaced instance %s", name, previous)
}

// instanceID returns the identifier of the single instance the named workload is
// running, failing the test when it isn't running exactly one.
func (s *Suite) instanceID(name string) string {
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(workload.Instances, 1)

	return workload.Instances[0].ID
}

// awaitListening waits until something accepts connections at the given address,
// which is how the tests prove a published port really reaches the container.
func (s *Suite) awaitListening(address string) {
	s.Require().Eventuallyf(func() bool {
		return s.dial(address)
	}, convergeTimeout, 500*time.Millisecond, "nothing ever listened on %s", address)
}

// names returns the names of the given workloads, for asserting on what a list
// contains without depending on its order.
func (s *Suite) names(workloads []client.Workload) []string {
	names := make([]string, 0, len(workloads))
	for _, workload := range workloads {
		names = append(names, workload.Name)
	}

	return names
}

// containers returns the identifiers of the containers docker holds for the named
// workload, whatever state they are in.
func (s *Suite) containers(workload string) []string {
	out, err := exec.Command("docker", "ps", "--all", "--quiet",
		"--filter", "label=orca.workload="+workload).Output()
	s.Require().NoError(err)

	return strings.Fields(string(out))
}

// cleanup removes a workload and anything docker still holds for it, so that a test
// failing part-way through doesn't leave containers behind for the next run.
func (s *Suite) cleanup(name string) {
	if s.client != nil {
		if _, err := s.client.Delete(context.Background(), name, client.WithWait()); err != nil {
			s.T().Logf("failed to delete workload %q: %v", name, err)
		}
	}

	for _, id := range s.containers(name) {
		if err := exec.Command("docker", "rm", "--force", id).Run(); err != nil {
			s.T().Logf("failed to remove container %s: %v", id, err)
		}
	}
}

func (s *Suite) dial(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return false
	}

	return conn.Close() == nil
}

// freePort asks the operating system for an unused port, so that a run of the suite
// doesn't collide with anything already listening.
func (s *Suite) freePort() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	s.Require().NoError(err)

	defer listener.Close()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	s.Require().NoError(err)

	return port
}
