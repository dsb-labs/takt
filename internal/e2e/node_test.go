package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestServingTLS covers the server terminating TLS itself: the client trusts the
// self-signed pair through its certificate authority option, a workload goes in
// and comes out over the encrypted connection, and a followed log read still
// streams. The follow matters because a TLS listener negotiates HTTP/2, and a
// stream that buffers under it would pass every other request while breaking
// this one.
func (s *Suite) TestServingTLS() {
	s.restart(s.withTLS())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Container.Command = []string{"sh", "-c", `i=0; while true; do i=$((i+1)); echo "line $i"; sleep 1; done`}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	ctx, cancel := context.WithCancel(s.ctx())
	s.T().Cleanup(cancel)

	var out syncBuffer

	done := make(chan error, 1)
	go func() {
		done <- s.client.Logs(ctx, &out, name, client.WithTail(1), client.WithFollow())
	}()

	// A second line can only have arrived through the followed connection, which
	// is what proves the stream flushes over TLS.
	s.Require().Eventuallyf(func() bool {
		return strings.Count(out.String(), "line ") > 1
	}, convergeTimeout, 500*time.Millisecond, "the followed output never arrived over tls")

	cancel()

	select {
	case err = <-done:
		s.Require().NoError(err)
	case <-time.After(convergeTimeout):
		s.Fail("the follow outlived the caller that asked for it")
	}
}

// TestObservability covers the surface an operator points a monitor at: liveness,
// readiness, and the metrics scrape a Prometheus would take.
func (s *Suite) TestObservability() {
	s.Require().NoError(s.client.Health(s.ctx()))

	// Readiness needs a completed pass over every driver, so it is awaited
	// rather than asserted.
	s.Require().Eventually(func() bool {
		readiness, err := s.client.Ready(s.ctx())

		return err == nil && readiness.Ready
	}, convergeTimeout, 100*time.Millisecond, "server never reported ready")

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	var metrics bytes.Buffer
	s.Require().NoError(s.client.Metrics(s.ctx(), &metrics))

	// The pass counter proves takt's own instruments are on the scrape, and the
	// workload gauge proves per-workload measurement made it through a real
	// converge.
	s.Contains(metrics.String(), "takt_reconcile_passes_total")
	s.Contains(metrics.String(), "takt_workloads")
}

// TestNode covers the read an operator takes of the machine itself: what the
// server is, and what the host has.
func (s *Suite) TestNode() {
	node, err := s.client.GetNode(s.ctx())
	s.Require().NoError(err)

	hostname, err := os.Hostname()
	s.Require().NoError(err)
	s.Equal(hostname, node.Hostname)
	s.Positive(node.CPUs)
	s.NotEmpty(node.Kernel)

	// The version is whatever the build carries, which for a server started
	// inside the test process is not a release, so only its presence is
	// asserted.
	s.NotEmpty(node.Version)

	// The instants are read from the kernel and recorded at startup rather
	// than derived from the clock, so their order is the check.
	s.False(node.StartedAt.IsZero())
	s.True(node.BootedAt.Before(node.StartedAt))

	s.Positive(node.Memory.Total)
	s.LessOrEqual(node.Memory.Used, node.Memory.Total)

	// The directories are the configured ones, reported whether or not the
	// first volume has created the volumes directory yet.
	s.Equal(s.directory, node.Disks.Data.Path)
	s.Equal(filepath.Join(s.directory, "volumes"), node.Disks.Volumes.Path)
	s.Positive(node.Disks.Data.Total)
	s.LessOrEqual(node.Disks.Data.Free, node.Disks.Data.Total)

	// A workload naming limits adds them to what the node has promised once
	// it runs, and one naming none is counted rather than summed.
	limited := s.workloadName() + "-limited"
	unlimited := s.workloadName() + "-unlimited"
	s.T().Cleanup(func() { s.cleanup(limited) })
	s.T().Cleanup(func() { s.cleanup(unlimited) })

	spec := s.containerSpec(limited)
	spec.Resources = &manifest.Resources{Memory: "64m", CPU: 0.25}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(unlimited))
	s.Require().NoError(err)
	s.awaitState(limited, client.WorkloadStateRunning)
	s.awaitState(unlimited, client.WorkloadStateRunning)

	after, err := s.client.GetNode(s.ctx())
	s.Require().NoError(err)
	s.Equal(node.Allocated.Memory+64<<20, after.Allocated.Memory)
	s.InDelta(node.Allocated.CPU+0.25, after.Allocated.CPU, 0.001)
	s.Equal(node.Allocated.UnlimitedMemory+1, after.Allocated.UnlimitedMemory)
	s.Equal(node.Allocated.UnlimitedCPU+1, after.Allocated.UnlimitedCPU)
}

// TestDebugBundle proves every test leaves the server's spans and logs on disk,
// which is what a failed run is diagnosed from.
func (s *Suite) TestDebugBundle() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	// Spans and logs are batched and only flushed by the server shutting down,
	// so restarting is what makes the bundle readable mid-test — and proves a
	// restarting test accumulates both servers' output in one bundle.
	s.restart(withDataDirectory(s.directory))

	trace, err := os.ReadFile(filepath.Join(s.artifacts, "trace.json"))
	s.Require().NoError(err)
	s.Contains(string(trace), `"reconcile"`, "the pass's root span is in the bundle")

	logs, err := os.ReadFile(filepath.Join(s.artifacts, "logs.json"))
	s.Require().NoError(err)
	s.NotEmpty(logs, "the server's own log records are in the bundle")
}

// TestServiceReportsBackends covers the service resource end to end: a service
// TestPrometheusDiscovery covers the discovery endpoint end to end: a workload
// opts in by label, and the endpoint answers in the shape http_sd_configs
// reads, with a target per instance at the port the labels selected.
func (s *Suite) TestPrometheusDiscovery() {
	// The helper derives one name from the test, and this test needs three
	// workloads that survive each other's applies.
	labelled := s.workloadName() + "-full"
	bare := s.workloadName() + "-bare"
	silent := s.workloadName() + "-silent"

	s.T().Cleanup(func() {
		s.cleanup(labelled)
		s.cleanup(bare)
		s.cleanup(silent)
	})

	// Two ports, so the selection has something to choose, and two instances,
	// so the group carries a target per instance. The labels the endpoint
	// consumes select the port, and the rest of the namespace passes through.
	spec := s.containerSpec(labelled, manifest.Port{Name: "http", To: 80}, manifest.Port{Name: "metrics", To: 9100})
	spec.Count = 2
	spec.Labels = map[string]string{
		"prometheus.scrape": "true",
		"prometheus.port":   "metrics",
		"prometheus.path":   "/",
	}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// One port and only the opt-in, so everything else is defaulted.
	spec = s.containerSpec(bare, manifest.Port{To: 9100})
	spec.Labels = map[string]string{"prometheus.scrape": "true"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// No labels, so the fleet having other workloads changes nothing.
	spec = s.containerSpec(silent, manifest.Port{To: 80})

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// Discovery describes desired state, so the targets exist as soon as the
	// specifications are stored and their ports are allocated — nothing here
	// waits for an instance to run.
	resp, err := http.Get(s.address + "/api/v1/system/prometheus-sd")
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var groups []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&groups))

	found := make(map[string]int)
	for i, group := range groups {
		found[group.Labels["takt_workload"]] = i
	}

	s.NotContains(found, silent, "a workload that did not opt in was discovered")

	// The labelled workload: a target per instance, at the metrics port the
	// label selected, with the namespace passed through prefix-stripped.
	s.Require().Contains(found, labelled)
	group := groups[found[labelled]]
	s.Equal(labelled, group.Labels["job"])
	s.Equal("/", group.Labels["__metrics_path__"])
	s.NotContains(group.Labels, "scrape", "a consumed label leaked into the series")

	stored, err := s.client.Get(s.ctx(), labelled)
	s.Require().NoError(err)

	// The host half of a target is the configured workload address, which the
	// harness owns — the ports are what this test can hold the endpoint to.
	expected := make([]string, 0, 2)
	for _, port := range stored.Ports {
		if port.Name == "metrics" {
			expected = append(expected, strconv.Itoa(port.From))
		}
	}
	s.Require().Len(expected, 2)

	got := make([]string, 0, len(group.Targets))
	for _, target := range group.Targets {
		_, p, err := net.SplitHostPort(target)
		s.Require().NoError(err)
		got = append(got, p)
	}
	s.ElementsMatch(expected, got)

	// The bare workload: the sole port selected without a label, the job
	// defaulted to the workload's name.
	s.Require().Contains(found, bare)
	s.Equal(bare, groups[found[bare]].Labels["job"])
	s.Len(groups[found[bare]].Targets, 1)
}
