package e2e_test

import (
	"bytes"
	"strconv"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestWorkloadMountsValues covers the whole point of mounting a secret or a variable:
// the value reaches the workload as a file it can read, and nothing about the value is
// stored.
// TestWorkloadReachesAnotherWorkload covers the whole point of referencing a
// workload: the address takt chose reaches the workload it names, from inside another
// container.
func (s *Suite) TestWorkloadReachesAnotherWorkload() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	served, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)
	s.Require().Len(served.Ports, 1)
	s.Equal("http", served.Ports[0].Name)
	s.True(served.Ports[0].Dynamic)

	s.awaitState(backend, client.WorkloadStateRunning)

	spec := s.jobSpec(consumer, manifest.RestartNever, 0)
	spec.Container.Command = []string{
		"sh", "-c", `echo "[$ADDR]"; wget -q -O- "http://$ADDR" >/dev/null && echo REACHED`,
	}
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(consumer, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, consumer, client.WithTail(10)))

	// The host port takt allocated, resolved into the consumer's environment on the
	// path that actually starts work.
	s.Contains(out.String(), ":"+strconv.Itoa(served.Ports[0].From)+"]")

	// And a request over it is answered, which is the only thing that proves a
	// container can reach a port published for another workload.
	s.Contains(out.String(), "REACHED")

	// What was stored is the reference. An address in the specification would be one
	// the workload keeps after takt has moved it.
	workload, err := s.client.Get(s.ctx(), consumer)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["ADDR"], "${workload:"+backend+":http}")
}

// TestMovingAPortRedeploysItsConsumers covers the address reaching the specification
// hash. Without it the reference would read as automatic and quietly would not be.
func (s *Suite) TestMovingAPortRedeploysItsConsumers() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80, From: 8280}))
	s.Require().NoError(err)
	s.awaitState(backend, client.WorkloadStateRunning)

	spec := s.containerSpec(consumer)
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(consumer, client.WorkloadStateRunning)
	instance := s.awaitInstance(consumer)

	// The backend moves to another host port, which is a change to the consumer's
	// address without a word of the consumer's manifest changing.
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80, From: 8281}))
	s.Require().NoError(err)

	s.awaitInstanceOtherThan(consumer, instance)

	after, err := s.client.Get(s.ctx(), consumer)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestDeletingAReferencedWorkload covers the guard on the other end of a reference,
// since applying a workload that names one which does not exist is rejected.
func (s *Suite) TestDeletingAReferencedWorkload() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	spec := s.containerSpec(consumer)
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	_, err = s.client.Delete(s.ctx(), backend)
	s.Require().ErrorIs(err, client.ErrWorkloadInUse)
	s.Contains(err.Error(), consumer)

	// Still there, and still running, since the refusal happened before anything was
	// marked.
	held, err := s.client.Get(s.ctx(), backend)
	s.Require().NoError(err)
	s.False(held.Deleting)

	// Forcing it through is the operator saying they know. The consumer keeps running
	// until it next starts, which is when the reference it can no longer resolve
	// matters.
	deleted, err := s.client.Delete(s.ctx(), backend, client.WithForceDeleteWorkload(), client.WithWait())
	s.Require().NoError(err)
	s.True(deleted.Deleting)
}

// TestWorkloadsMayReferenceEachOther covers a cycle, which looks alarming and is not:
// allocating a port does not consult a reference, so there is no fixpoint to solve.
func (s *Suite) TestWorkloadsMayReferenceEachOther() {
	first, second := s.workloadName()+"-first", s.workloadName()+"-second"
	s.T().Cleanup(func() { s.cleanup(second) })
	s.T().Cleanup(func() { s.cleanup(first) })

	// A reference has to name a workload that exists, so the cycle is reached by
	// applying one, then the other, then the first again.
	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(first, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	other := s.containerSpec(second, manifest.Port{Name: "http", To: 80})
	other.Env = map[string]string{"PEER": "${workload:" + first + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), other)
	s.Require().NoError(err)

	closing := s.containerSpec(first, manifest.Port{Name: "http", To: 80})
	closing.Env = map[string]string{"PEER": "${workload:" + second + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), closing)
	s.Require().NoError(err)

	// Both resolve and both run. Neither is waiting on the other.
	s.awaitState(first, client.WorkloadStateRunning)
	s.awaitState(second, client.WorkloadStateRunning)
}

// TestReferencingAnUnknownWorkloadIsRejected covers the apply-time guard, which is
// what an unknown secret gets and for the same reason.
func (s *Suite) TestReferencingAnUnknownWorkloadIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"ADDR": "${workload:e2e-workload-does-not-exist}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-workload-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestReadersSpreadAcrossInstances covers what scaling a referenced workload does to
// its readers: each reader instance resolves the reference to an instance of its
// own, and a count change rebalances them.
func (s *Suite) TestReadersSpreadAcrossInstances() {
	target, reader := s.workloadName()+"-target", s.workloadName()+"-reader"
	s.T().Cleanup(func() { s.cleanup(reader) })
	s.T().Cleanup(func() { s.cleanup(target) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(target, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	readers := s.containerSpec(reader)
	readers.Count = 3
	readers.Env = map[string]string{"PEER": "${workload:" + target + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), readers)
	s.Require().NoError(err)

	s.awaitInstances(reader, 3)

	// One target instance, so every reader resolves the same address.
	s.Len(s.peers(reader, 3), 1, "three readers of one instance share its address")

	// Scaling the target up moves the arithmetic, and the readers roll onto the
	// new capacity: three readers over three instances land one on each.
	scaled := s.containerSpec(target, manifest.Port{Name: "http", To: 80})
	scaled.Count = 3

	_, _, err = s.client.Apply(s.ctx(), scaled)
	s.Require().NoError(err)

	s.Require().Eventuallyf(func() bool {
		return len(s.peers(reader, 3)) == 3
	}, convergeTimeout, time.Second, "the readers never spread across the target's instances")

	// Scaling back down rolls them onto what remains.
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(target, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	s.Require().Eventuallyf(func() bool {
		return len(s.peers(reader, 3)) == 1
	}, convergeTimeout, time.Second, "the readers never fell back to the remaining instance")
}
