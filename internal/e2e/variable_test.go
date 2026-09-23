package e2e_test

import (
	"bytes"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestWorkloadReadsAVariable covers the whole point of a variable: the value reaches
// the workload, and an operator can still see what it is.
func (s *Suite) TestWorkloadReadsAVariable() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	stored, created, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "localhost"})
	s.Require().NoError(err)
	s.True(created)
	s.Equal("localhost", stored.Value)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{"DSN": "postgres://app@${var:" + variable + "}/app"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading it back out of the container is the only proof the value was resolved
	// on the path that actually starts work.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app@localhost/app]")

	// What was stored is the reference. A variable's value is not a secret, but
	// resolving it into the stored specification would mean a change to the variable
	// no longer reaching the workload that reads it.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["DSN"], "${var:"+variable+"}")
	s.NotContains(workload.Spec.Env["DSN"], "localhost")

	// And unlike a secret, the value is readable back. This is the difference the
	// whole feature exists for.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("localhost", held.Value)
	s.Equal([]string{name}, held.UsedBy)
}

// TestWorkloadReadsBothKinds covers one value holding a secret and a variable, which
// is what the single expansion pass is for.
func (s *Suite) TestWorkloadReadsBothKinds() {
	name := s.workloadName()
	secret, variable := s.secretName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("hunter2")})
	s.Require().NoError(err)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "db.internal"})
	s.Require().NoError(err)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{
		"DSN": "postgres://app:${secret:" + secret + "}@${var:" + variable + "}/app",
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Both kinds resolved in one value. A pass that handled only one would have
	// refused the other rather than substituting it.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app:hunter2@db.internal/app]")

	// The secret's value is still nowhere in the database, even though the variable
	// beside it is. Holding both in one value does not weaken the secret.
	s.False(s.databaseHolds("hunter2"), "the plaintext reached the database")
}

// TestChangingAVariableRedeploysItsWorkload covers a changed value replacing the
// instances reading the old one, which is what the value in the hash is for.
func (s *Suite) TestChangingAVariableRedeploysItsWorkload() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "first"})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The variable now reports the workload reading it, which is what makes a change
	// know what to redeploy.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	changed, created, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "second"})
	s.Require().NoError(err)
	s.False(created)
	s.Equal("second", changed.Value)

	// The instance is replaced without the specification having changed at all.
	s.awaitInstanceOtherThan(name, instance)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestUnchangedVariableIsNotRedeployed covers the no-op write, so that a tool setting
// every variable on every run does not restart the fleet each time.
func (s *Suite) TestUnchangedVariableIsNotRedeployed() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	first, _, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "unchanged"})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	again, created, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "unchanged"})
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.UpdatedAt, again.UpdatedAt)

	// Nothing moved, so nothing is replaced. Watched over several reconciliation
	// passes rather than asserted at once, since a redeploy would take a moment to
	// appear and an immediate check would pass whether or not one was coming.
	//
	// A plain loop rather than Never, whose condition runs on a goroutine that
	// outlives the assertion: it would still be reading the suite's client while the
	// teardown replaced it.
	for range 10 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(workload.Instances, 1)
		s.Require().Equal(before.Version, workload.Version,
			"an unchanged variable bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"an unchanged variable replaced its workload's instance")
	}
}

// TestDeletingAVariableInUseIsRefused covers the refusal naming the workloads, and
// what forcing it does to them.
func (s *Suite) TestDeletingAVariableInUseIsRefused() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "held"})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	err = s.client.DeleteVariable(s.ctx(), variable)
	s.Require().ErrorIs(err, client.ErrVariableInUse)
	s.Contains(err.Error(), name)

	// Still there, so the refusal was a refusal rather than a report.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("held", held.Value)

	s.Require().NoError(s.client.DeleteVariable(s.ctx(), variable, client.WithForceDeleteVariable()))

	_, err = s.client.GetVariable(s.ctx(), variable)
	s.ErrorIs(err, client.ErrVariableNotFound)

	// Re-creating it recovers the workload, which had been left unable to start.
	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "held"})
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
}

// TestWorkloadReadingAnUnknownVariableIsRejected covers the apply being refused rather
// than the workload being stored and left unable to start.
func (s *Suite) TestWorkloadReadingAnUnknownVariableIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:e2e-var-does-not-exist}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-var-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestVariableSurvivesAServerRestart covers the value outliving the process, since a
// variable that had to be set again after every restart would be useless.
func (s *Suite) TestVariableSurvivesAServerRestart() {
	variable := s.variableName()
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	directory := s.directory

	stored, _, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "persisted"})
	s.Require().NoError(err)

	s.restart(withDataDirectory(directory))

	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("persisted", held.Value)
	s.Equal(stored.UpdatedAt, held.UpdatedAt)
}

// TestMissingVariable covers the not-found path on every variable endpoint that takes
// a name.
func (s *Suite) TestMissingVariable() {
	_, err := s.client.GetVariable(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrVariableNotFound)

	s.ErrorIs(s.client.DeleteVariable(s.ctx(), "does-not-exist"), client.ErrVariableNotFound)
}
