package e2e_test

import (
	"context"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// selects workloads by label and reports the address of every running instance,
// the backends drain when the workload stops, and deleting the service leaves
// the workloads alone.
func (s *Suite) TestServiceReportsBackends() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80})
	spec.Count = 2
	spec.Labels = map[string]string{"service-e2e": name}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.awaitInstances(name, 2)

	// The service is applied after the workload here, but nothing depends on
	// the order: a service is a question asked of whatever is running.
	applied, err := s.client.ApplyService(s.ctx(), manifest.Service{
		Version: "v1",
		Name:    name,
		Target: manifest.ServiceTarget{
			Labels: map[string]string{"service-e2e": name},
			Port:   80,
		},
	})
	s.Require().NoError(err)
	s.T().Cleanup(func() {
		if s.client != nil {
			_ = s.client.DeleteService(context.Background(), name)
		}
	})

	// The protocol was left unset, so the stored target reports the default.
	s.Equal(manifest.ProtocolTCP, applied.Target.Protocol)

	backends := s.awaitBackends(name, 2)

	// Each backend names the workload and instance its address reaches, and
	// each instance holds a host port of its own.
	addresses := make(map[string]struct{}, 2)
	for _, backend := range backends {
		s.Equal(name, backend.Workload)
		addresses[backend.Address] = struct{}{}
	}
	s.Len(addresses, 2, "two instances are two addresses")

	// A stopped workload has no running instances, so the backends drain
	// without the service changing.
	_, err = s.client.Stop(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)
	s.awaitBackends(name, 0)

	// Deleting the service removes only the record of it.
	s.Require().NoError(s.client.DeleteService(s.ctx(), name))

	_, err = s.client.GetService(s.ctx(), name)
	s.ErrorIs(err, client.ErrServiceNotFound)

	_, err = s.client.Get(s.ctx(), name)
	s.Require().NoError(err, "deleting the service must leave the workload")
}

// TestServiceStreamsBackends checks that a followed services list reports the
// backends as they arrive and as they go, without being asked again.
func (s *Suite) TestServiceStreamsBackends() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// The service first, so the stream is open before anything it selects
	// exists and the arrival is something it has to notice. Labelled, since
	// the stream is narrowed by a query over the service's own labels.
	_, err := s.client.ApplyService(s.ctx(), manifest.Service{
		Version: "v1",
		Name:    name,
		Labels:  map[string]string{"service-e2e": name},
		Target: manifest.ServiceTarget{
			Labels: map[string]string{"service-e2e": name},
			Port:   80,
		},
	})
	s.Require().NoError(err)
	s.T().Cleanup(func() {
		if s.client != nil {
			_ = s.client.DeleteService(context.Background(), name)
		}
	})

	ctx, cancel := context.WithCancel(s.ctx())
	defer cancel()

	sets := make(chan []client.ServiceBackend, 64)
	done := make(chan error, 1)

	go func() {
		done <- s.client.StreamServices(ctx, func(services []client.Service) error {
			for _, service := range services {
				if service.Name == name {
					sets <- service.Backends
				}
			}

			return nil
		}, `$.labels."service-e2e"=`+name)
	}()

	// awaitSet reads sets until one carries the given number of backends.
	// Intermediate sets are allowed: two instances may come up in two passes.
	awaitSet := func(count int) []client.ServiceBackend {
		for {
			select {
			case backends := <-sets:
				if len(backends) == count {
					return backends
				}
			case <-time.After(convergeTimeout):
				s.Require().Failf("the stream stalled", "service %q never reported %d backends", name, count)
			}
		}
	}

	// The first line is written at once, and nothing is running yet.
	s.Empty(awaitSet(0))

	spec := s.containerSpec(name, manifest.Port{To: 80})
	spec.Count = 2
	spec.Labels = map[string]string{"service-e2e": name}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	backends := awaitSet(2)
	for _, backend := range backends {
		s.Equal(name, backend.Workload)
	}

	// Stopping drains the backends, and the stream says so.
	_, err = s.client.Stop(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)
	s.Empty(awaitSet(0))

	cancel()
	s.NoError(<-done)
}

// TestShutdownEndsOpenStreams checks that stopping the server does not wait on a
// client holding a followed response open. Shutdown waits for every response to
// end, and a stream ends only when its context does, so the server has to end
// them itself or sit at its deadline and report a failure.
func (s *Suite) TestShutdownEndsOpenStreams() {
	ctx, cancel := context.WithCancel(s.ctx())
	defer cancel()

	// A stream over the whole list, which the server writes to at once and then
	// holds open. Nothing has to exist for it to be open.
	first := make(chan struct{}, 1)
	done := make(chan error, 1)

	go func() {
		done <- s.client.StreamServices(ctx, func([]client.Service) error {
			select {
			case first <- struct{}{}:
			default:
			}

			return nil
		})
	}()

	select {
	case <-first:
	case <-time.After(convergeTimeout):
		s.Require().Fail("the stream never wrote its first line")
	}

	started := time.Now()
	s.stop()

	// Well inside the thirty seconds Shutdown would otherwise wait, and stop has
	// already checked the server exited without error.
	s.Less(time.Since(started), 10*time.Second, "shutdown waited on the open stream")

	// The server ends the stream as its own context ends, which the client reads
	// as the list closing rather than as a failure.
	select {
	case <-done:
	case <-time.After(convergeTimeout):
		s.Require().Fail("the stream outlived the server")
	}
}
