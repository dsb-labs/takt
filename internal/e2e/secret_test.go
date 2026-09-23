package e2e_test

import (
	"bytes"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestWorkloadReadsASecret covers the whole point of a secret: the value reaches the
// workload, and nothing else.
func (s *Suite) TestWorkloadReadsASecret() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("hunter2")})
	s.Require().NoError(err)
	s.True(created)
	s.NotEmpty(stored.Revision)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{"DSN": "postgres://app:${secret:" + secret + "}@localhost/app"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading it back out of the container is the only proof the value was resolved
	// on the path that actually starts work.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app:hunter2@localhost/app]")

	// What was stored is the reference, because the API echoes the specification back
	// to anything that can reach the server.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["DSN"], "${secret:"+secret+"}")
	s.NotContains(workload.Spec.Env["DSN"], "hunter2")

	// And the value is nowhere in the database, including pages the write-ahead log
	// has not checkpointed. This is the property the whole feature exists for.
	s.False(s.databaseHolds("hunter2"), "the plaintext reached the database")
}

// TestRotatingASecretRedeploysItsWorkload covers a changed value replacing the
// instances reading the old one, which is what the revision in the hash is for.
func (s *Suite) TestRotatingASecretRedeploysItsWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("first")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The secret now reports the workload reading it, which is what makes a rotation
	// know what to redeploy.
	held, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	rotated, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("second")})
	s.Require().NoError(err)
	s.False(created)
	s.NotEqual(held.Revision, rotated.Revision)

	// The instance is replaced without the specification having changed at all.
	s.awaitInstanceOtherThan(name, instance)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestUnchangedSecretIsNotRedeployed covers the no-op write, so that a tool setting
// every secret on every run does not restart the fleet each time.
func (s *Suite) TestUnchangedSecretIsNotRedeployed() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	first, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("unchanged")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	again, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("unchanged")})
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.Revision, again.Revision)
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
			"an unchanged secret bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"an unchanged secret replaced its workload's instance")
	}
}

// TestRelabellingASecretDoesNotRedeployIt covers the constraint that makes labels
// safe to put on a secret: filing one is not rotating it.
//
// A secret's revision is mixed into the specification hash of every workload reading
// it, so moving the revision replaces instances across the node. That is right for a
// value that changed and absurd for a piece of bookkeeping.
func (s *Suite) TestRelabellingASecretDoesNotRedeployIt() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	first, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("unchanged"), Labels: map[string]string{"app": "web"}})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "web"}, first.Labels)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The same value under different labels. The labels land and the revision does
	// not move.
	relabelled, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{
		Name:   secret,
		Value:  []byte("unchanged"),
		Labels: map[string]string{"app": "api", "team": "platform"},
	})
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.Revision, relabelled.Revision)
	s.Equal(map[string]string{"app": "api", "team": "platform"}, relabelled.Labels)

	// Labels replace rather than merge, so setting the value with none removes them.
	// The revision still holds.
	cleared, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("unchanged")})
	s.Require().NoError(err)
	s.Equal(first.Revision, cleared.Revision)
	s.Empty(cleared.Labels)

	// And nothing reading the secret was replaced by any of it. Watched over several
	// reconciliation passes, since a redeploy would take a moment to appear and an
	// immediate check would pass whether or not one was coming.
	for range 10 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(workload.Instances, 1)
		s.Require().Equal(before.Version, workload.Version,
			"relabelling a secret bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"relabelling a secret replaced its workload's instance")
	}
}

// TestDeletingASecretInUseIsRefused covers the refusal naming the workloads, and what
// forcing it does to them.
func (s *Suite) TestDeletingASecretInUseIsRefused() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("held")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	err = s.client.DeleteSecret(s.ctx(), secret)
	s.Require().ErrorIs(err, client.ErrSecretInUse)

	// The workload is named, since an operator can act on that and not on "something
	// is using it".
	s.Contains(err.Error(), name)

	// It is still there, so the refusal was a refusal rather than a report.
	_, err = s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)

	s.Require().NoError(s.client.DeleteSecret(s.ctx(), secret, client.WithForceDelete()))

	_, err = s.client.GetSecret(s.ctx(), secret)
	s.ErrorIs(err, client.ErrSecretNotFound)

	// Re-creating it recovers the workload on its own, which is what the link
	// outliving the secret is for.
	_, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("restored")})
	s.Require().NoError(err)
	s.True(created)

	s.awaitState(name, client.WorkloadStateRunning)
}

// TestWorkloadReadingAnUnknownSecretIsRejected covers the apply being refused, so a
// mistyped name is reported rather than stored as a workload that can never start.
func (s *Suite) TestWorkloadReadingAnUnknownSecretIsRejected() {
	name := s.workloadName()

	spec := s.execSpec(name, "sh", "-c", "exit 0")
	spec.Env = map[string]string{"VALUE": "${secret:" + s.secretName() + "}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), s.secretName())

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestSecretSurvivesAServerRestart covers the key being read back from disk, since a
// server that sealed a value has to be able to open it again.
func (s *Suite) TestSecretSurvivesAServerRestart() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("across-restarts")})
	s.Require().NoError(err)

	s.restart(withDataDirectory(directory))

	// The revision is unchanged, so the secret was read rather than replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value still opens under the key the new server loaded, which only a
	// workload reading it can show.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[across-restarts]")
}

// TestMissingSecret covers the not-found path on every secret endpoint that takes a
// name.
func (s *Suite) TestMissingSecret() {
	_, err := s.client.GetSecret(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrSecretNotFound)

	s.ErrorIs(s.client.DeleteSecret(s.ctx(), "does-not-exist"), client.ErrSecretNotFound)
}
