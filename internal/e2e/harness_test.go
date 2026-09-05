package e2e_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/orca/internal/restore"
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
		// Where the running server keeps its state, so that a test can read the
		// database directly and check what did not reach it.
		directory string
		// Where the test's debug bundle is written: the server's spans, logs
		// and a final metrics scrape, kept per test so a failure can be
		// diagnosed from what the server actually did.
		artifacts string
		// The files the telemetry exporters write to, held open across a
		// restart so both servers' output lands in one bundle.
		traceFile *os.File
		logFile   *os.File
	}

	// The option type modifies how a test server is started.
	option func(*server.Config)
)

// withDataDirectory modifies the server to keep its database in the given directory,
// so that a later server can be started against the same desired state.
func withDataDirectory(directory string) option {
	return func(c *server.Config) { c.Data.Directory = directory }
}

// withAllowHostPaths modifies the server to accept path mounts under the given
// prefixes, which the default configuration refuses entirely.
func withAllowHostPaths(prefixes ...string) option {
	return func(c *server.Config) { c.Workload.AllowHostPaths = prefixes }
}

// withTLS modifies the server to terminate TLS with a self-signed pair generated
// for the test. The suite's client trusts the pair through the same option an
// operator would use.
func (s *Suite) withTLS() option {
	cert, key := s.generateCertificate()

	return func(c *server.Config) { c.HTTP.TLSCert, c.HTTP.TLSKey = cert, key }
}

// generateCertificate writes a self-signed certificate pair into a temporary
// directory and returns the certificate path and the key path.
func (s *Suite) generateCertificate() (string, string) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.Require().NoError(err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	s.Require().NoError(err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "orca e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		// The suite dials the address literal, so the address literal is what
		// the certificate has to name.
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	s.Require().NoError(err)

	keyDER, err := x509.MarshalECPrivateKey(private)
	s.Require().NoError(err)

	directory := s.T().TempDir()
	cert, key := filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	s.Require().NoError(os.WriteFile(cert, certPEM, 0o600))
	// Owner-only, because the server refuses a key anyone else can read.
	s.Require().NoError(os.WriteFile(key, keyPEM, 0o600))

	return cert, key
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

// TearDownTest finishes the test's debug bundle with a metrics scrape, then stops
// the server. Stopping is what flushes the batched spans and logs into the
// bundle, so the scrape has to come first while the server still answers.
func (s *Suite) TearDownTest() {
	s.scrapeMetrics()
	s.stop()
	s.closeBundle()
}

// start runs a server and points the suite's client at it.
func (s *Suite) start(options ...option) {
	config := server.DefaultConfig()
	config.HTTP.Address = "127.0.0.1:" + s.freePort()
	config.Data.Directory = s.T().TempDir()
	config.Logging.Level = "error"
	// Short enough that a test waiting for convergence isn't mostly waiting on the
	// ticker. Driver events already cover the prompt cases.
	config.Reconcile.Interval = time.Second

	if testing.Verbose() {
		config.Logging.Level = "debug"
	}

	// Every test writes a debug bundle: the server's spans and logs land in
	// per-test files as it runs, so a failure can be read from what the server
	// actually did rather than reconstructed from assertion messages.
	s.openBundle()

	traces, err := stdouttrace.New(stdouttrace.WithWriter(s.traceFile))
	s.Require().NoError(err)

	logs, err := stdoutlog.New(stdoutlog.WithWriter(s.logFile))
	s.Require().NoError(err)

	config.Telemetry.SpanExporter = traces
	config.Telemetry.LogExporter = logs

	for _, option := range options {
		option(&config)
	}

	ctx, cancel := context.WithCancel(context.Background())
	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error { return server.Run(ctx, config) })

	address := "http://" + config.HTTP.Address

	var clientOptions []client.Option
	if config.HTTP.TLSEnabled() {
		address = "https://" + config.HTTP.Address
		clientOptions = append(clientOptions, client.WithCACertificate(config.HTTP.TLSCert))
	}

	c, err := client.New(address, clientOptions...)
	s.Require().NoError(err)

	s.client, s.cancel, s.done, s.directory = c, cancel, group, config.Data.Directory

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

// openBundle creates the test's debug-bundle directory and opens the files the
// telemetry exporters write to. The files are truncated once per test and then
// held open across a restart, so a test that runs two servers accumulates both
// runs' output in one bundle rather than keeping only the last.
func (s *Suite) openBundle() {
	if s.traceFile != nil {
		return
	}

	s.artifacts = filepath.Join("artifacts", strings.ReplaceAll(s.T().Name(), "/", "_"))
	s.Require().NoError(os.MkdirAll(s.artifacts, 0o755))

	open := func(name string) *os.File {
		file, err := os.OpenFile(filepath.Join(s.artifacts, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		s.Require().NoError(err)

		return file
	}

	s.traceFile, s.logFile = open("trace.json"), open("logs.json")
}

// scrapeMetrics writes the server's final metrics into the test's debug bundle.
// Best-effort: a test that already stopped its server still passes, it just has
// no scrape to keep.
func (s *Suite) scrapeMetrics() {
	if s.client == nil || s.artifacts == "" {
		return
	}

	file, err := os.Create(filepath.Join(s.artifacts, "metrics.prom"))
	if err != nil {
		s.T().Logf("failed to create the metrics file: %v", err)

		return
	}
	defer file.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = s.client.Metrics(ctx, file); err != nil {
		s.T().Logf("failed to scrape metrics into the debug bundle: %v", err)
	}
}

// closeBundle closes the debug-bundle files once the server that wrote to them
// has stopped.
func (s *Suite) closeBundle() {
	for _, file := range []*os.File{s.traceFile, s.logFile} {
		if file != nil {
			file.Close()
		}
	}

	s.traceFile, s.logFile, s.artifacts = nil, nil, ""
}

func (s *Suite) ctx() context.Context {
	return s.T().Context()
}

// containerSpec returns a specification for a long-running container publishing the
// given ports.
func (s *Suite) containerSpec(name string, ports ...manifest.Port) manifest.Spec {
	return manifest.Spec{
		Version: "v1",
		Name:    name,
		Labels:  map[string]string{"some-key": "some-value"},
		Ports:   ports,
		Env:     map[string]string{"EXAMPLE": "EXAMPLE"},
		Container: &manifest.Container{
			Image: testImage,
		},
	}
}

// execSpec returns a specification that runs a command on the host rather than a
// container, which is the exec runtime's whole difference.
func (s *Suite) execSpec(name string, command ...string) manifest.Spec {
	return manifest.Spec{
		Version: "v1",
		Name:    name,
		Exec:    &manifest.Exec{Command: command},
	}
}

// jobSpec returns a specification for a workload that ends rather than serving, which
// is what a restart policy exists to describe.
//
// The image is the one the rest of the suite uses, so no second image is pulled. The
// command is what makes it end: a policy cannot be observed on a workload that runs
// until something stops it.
func (s *Suite) jobSpec(name string, policy manifest.RestartPolicy, exitCode int) manifest.Spec {
	return manifest.Spec{
		Version: "v1",
		Name:    name,
		Restart: &manifest.Restart{Policy: policy},
		Container: &manifest.Container{
			Image:   testImage,
			Command: []string{"sh", "-c", fmt.Sprintf("exit %d", exitCode)},
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
func (s *Suite) awaitState(name string, state client.WorkloadState) client.Workload {
	var workload client.Workload

	s.Require().Eventuallyf(func() bool {
		var err error
		workload, err = s.client.Get(s.ctx(), name)

		return err == nil && workload.State == state
	}, convergeTimeout, 500*time.Millisecond, "workload %q never reached state %q", name, state)

	return workload
}

// awaitHealth waits for the named workload's instance to reach the given health
// status and returns the workload.
func (s *Suite) awaitHealth(name string, status client.HealthStatus) client.Workload {
	var workload client.Workload

	s.Require().Eventuallyf(func() bool {
		var err error

		workload, err = s.client.Get(s.ctx(), name)
		if err != nil || len(workload.Instances) == 0 {
			return false
		}

		reported := workload.Instances[0].Health

		return reported != nil && reported.Status == status
	}, convergeTimeout, 500*time.Millisecond, "workload %q never reached health %q", name, status)

	return workload
}

// awaitInstance waits for the named workload to be running an instance and returns
// its identifier, which is how a later replacement is recognised.
func (s *Suite) awaitInstance(name string) string {
	var instance string

	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil || len(workload.Instances) == 0 {
			return false
		}

		instance = workload.Instances[0].ID

		return instance != ""
	}, convergeTimeout, 500*time.Millisecond, "workload %q never reported an instance", name)

	return instance
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

		return workload.Instances[0].ID != previous && workload.Instances[0].State == client.InstanceStateRunning
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

// awaitInstances waits for the named workload to be running the given number of
// instances and returns them, keyed by index.
func (s *Suite) awaitInstances(name string, count int) map[int]client.Instance {
	var byIndex map[int]client.Instance

	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil || len(workload.Instances) != count {
			return false
		}

		byIndex = make(map[int]client.Instance, count)
		for _, instance := range workload.Instances {
			if instance.State != client.InstanceStateRunning {
				return false
			}

			byIndex[instance.Index] = instance
		}

		return len(byIndex) == count
	}, convergeTimeout, 500*time.Millisecond, "workload %q never ran %d instances", name, count)

	return byIndex
}

// runningContainers returns the containers docker reports as running for the named
// workload, leaving out whatever a stop retained.
func (s *Suite) runningContainers(workload string) []string {
	out, err := exec.Command("docker", "ps", "--quiet",
		"--filter", "label=orca.workload="+workload).Output()
	s.Require().NoError(err)

	return strings.Fields(string(out))
}

// peers returns the distinct addresses the named workload's running containers carry
// in their PEER environment variable, reporting nil until the expected number of
// containers is running.
//
// Asked of docker rather than of orca, because the resolved environment is
// deliberately not reported by the API: what each instance was actually started with
// is the only place the answer exists.
func (s *Suite) peers(workload string, expected int) map[string]struct{} {
	ids := s.runningContainers(workload)
	if len(ids) != expected {
		return nil
	}

	peers := make(map[string]struct{}, len(ids))

	for _, id := range ids {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{range .Config.Env}}{{println .}}{{end}}", id).Output()
		if err != nil {
			// The container may have been replaced between listing and inspecting,
			// which the caller's retry absorbs.
			return nil
		}

		for line := range strings.SplitSeq(string(out), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "PEER="); ok {
				peers[value] = struct{}{}
			}
		}
	}

	return peers
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

// publishedPorts returns the port mappings docker reports for the named workload's
// container, as "<port>/<protocol>" strings.
//
// Asked of docker rather than of orca, since what the server recorded and what the
// runtime published are the two things a test about protocols has to see agree.
func (s *Suite) publishedPorts(workload string) []string {
	ids := s.containers(workload)
	s.Require().NotEmpty(ids)

	out, err := exec.Command("docker", "port", ids[0]).Output()
	s.Require().NoError(err)

	published := make([]string, 0, 2)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		mapping, _, ok := strings.Cut(line, " -> ")
		if !ok {
			continue
		}

		published = append(published, strings.TrimSpace(mapping))
	}

	return published
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

// volumeName derives a volume name from the running test, short enough to stay inside
// the 63 characters a name may have: a test's own name is already most of that.
func (s *Suite) volumeName() string {
	sum := sha256.Sum256([]byte(s.T().Name()))

	return "e2e-vol-" + hex.EncodeToString(sum[:6])
}

// volumeFile reads a file from inside a volume's directory on the host, which is how a
// test checks what a workload actually wrote. Returns empty when it is not there.
func (s *Suite) volumeFile(path, name string) string {
	contents, err := os.ReadFile(filepath.Join(path, name))
	if err != nil {
		return ""
	}

	return string(contents)
}

// mountedFile reads a file from inside the container the named workload is running,
// which is how a test checks what a mounted value actually looks like to the workload.
// Returns empty when there is no container or no such file.
//
// Read from inside rather than from the host, because that is the thing under test: a
// file rewritten in place is visible to the container, where one replaced by a rename
// would leave it reading the old inode.
func (s *Suite) mountedFile(workload, path string) string {
	containers := s.containers(workload)
	if len(containers) == 0 {
		return ""
	}

	out, err := exec.Command("docker", "exec", containers[0], "cat", path).Output()
	if err != nil {
		return ""
	}

	return string(out)
}

// mountsHold reports whether any file the server wrote for a mounted value contains
// the given value.
//
// This is the counterpart to databaseHolds for the one place a mounted secret's
// plaintext legitimately lives, and is how a test proves it is removed again.
func (s *Suite) mountsHold(value string) bool {
	var found bool

	root := filepath.Join(s.directory, "mounts", "files")

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			return nil
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if bytes.Contains(contents, []byte(value)) {
			found = true
		}

		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		s.T().Logf("failed to walk the mounted values: %v", err)
	}

	return found
}

// secretName derives a secret name from the running test's name, so that tests
// sharing a server cannot rotate each other's secrets.
func (s *Suite) secretName() string {
	sum := sha256.Sum256([]byte(s.T().Name()))

	return "e2e-sec-" + hex.EncodeToString(sum[:6])
}

// cleanupSecret removes a secret a test created, forcing it so that one held by a
// workload a failing test left behind does not survive into the next run.
func (s *Suite) cleanupSecret(name string) {
	if s.client == nil {
		return
	}

	err := s.client.DeleteSecret(context.Background(), name, client.WithForceDelete())
	if err != nil && !errors.Is(err, client.ErrSecretNotFound) {
		s.T().Logf("failed to delete secret %q: %v", name, err)
	}
}

// variableName derives a variable name from the running test's name, so that tests
// sharing a server cannot change each other's variables.
func (s *Suite) variableName() string {
	sum := sha256.Sum256([]byte(s.T().Name()))

	return "e2e-var-" + hex.EncodeToString(sum[:6])
}

// cleanupVariable removes a variable a test created, forcing it so that one held by a
// workload a failing test left behind does not survive into the next run.
func (s *Suite) cleanupVariable(name string) {
	if s.client == nil {
		return
	}

	err := s.client.DeleteVariable(context.Background(), name, client.WithForceDeleteVariable())
	if err != nil && !errors.Is(err, client.ErrVariableNotFound) {
		s.T().Logf("failed to delete variable %q: %v", name, err)
	}
}

// databaseHolds reports whether the raw bytes of the server's database contain the
// given value.
//
// The file is read rather than queried, so this covers anything a value could have
// reached: a column nothing selects, an index, or a page the write-ahead log has not
// checkpointed yet.
func (s *Suite) databaseHolds(value string) bool {
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		contents, err := os.ReadFile(filepath.Join(s.directory, name))
		if err != nil {
			continue
		}

		if bytes.Contains(contents, []byte(value)) {
			return true
		}
	}

	return false
}

// cleanupVolume removes a volume a test created, forcing it so that a test which failed
// partway through does not leave one behind held by a workload.
func (s *Suite) cleanupVolume(name string) {
	if s.client == nil {
		return
	}

	err := s.client.DeleteVolume(context.Background(), name, client.WithForce())
	if err != nil && !errors.Is(err, client.ErrVolumeNotFound) {
		s.T().Logf("failed to delete volume %q: %v", name, err)
	}
}

// The syncBuffer type collects what a followed log read writes while the test reads it,
// since the two happen on different goroutines.
type syncBuffer struct {
	mux sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.String()
}

// restore puts a backup archive into a data directory and returns what the restore
// reported it could not do.
//
// Through the same code "orca admin restore" runs, rather than by unpacking the
// archive here. A helper of its own would be a second implementation of the
// procedure, and the one that had never been run is the one under test.
func (s *Suite) restore(archive []byte, directory string) restore.Report {
	path := filepath.Join(s.T().TempDir(), "backup.zip")
	s.Require().NoError(os.WriteFile(path, archive, 0o600))

	config := server.DefaultConfig()
	config.Data.Directory = directory
	s.Require().NoError(config.Validate())

	report, err := restore.Run(s.ctx(), restore.Config{
		Archive:  path,
		Database: config.DatabasePath(),
		Keys:     config.KeysPath(),
		Volumes:  config.VolumesPath(),
	})
	s.Require().NoError(err)

	return report
}
