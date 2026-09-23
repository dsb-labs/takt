package e2e_test

import (
	"bytes"
	"path/filepath"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func (s *Suite) TestWorkloadMountsValues() {
	name, secret, variable := s.workloadName(), s.secretName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte(`{"key":"hunter2"}`)})
	s.Require().NoError(err)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: `{"level":"debug"}`})
	s.Require().NoError(err)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `cat /var/secret.json /var/example.json; exit 0`}
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret.json"},
		{Var: variable, To: "/var/example.json"},
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading the files from inside the container is the only proof they were written
	// and mounted where the manifest asked for.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), `{"key":"hunter2"}`)
	s.Contains(out.String(), `{"level":"debug"}`)

	// What was stored is the name of what is read. A path resolved into the stored
	// specification would put takt's own layout in the API and move the hash with
	// every version.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(workload.Spec.Volumes, 2)
	s.Equal(secret, workload.Spec.Volumes[0].Secret)
	s.Equal(variable, workload.Spec.Volumes[1].Var)

	// The secret's value is nowhere in the database, exactly as for one an environment
	// reads. Mounting one writes it to the filesystem, which is the documented cost of
	// asking for a file.
	s.False(s.databaseHolds("hunter2"), "the mounted secret's value reached the database")

	// Both report the workload reading them, which is what makes a change reach it.
	heldSecret, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal([]string{name}, heldSecret.UsedBy)

	heldVariable, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal([]string{name}, heldVariable.UsedBy)
}

// TestReadOnlyRootfsLeavesMountsUsable covers the interaction between a read-only
// root filesystem and what takt mounts: a volume and a mounted value are bind mounts
// with rules of their own, so the volume stays writable and the value stays readable
// at its 0444 mode while the image's own filesystem refuses writes.
func (s *Suite) TestReadOnlyRootfsLeavesMountsUsable() {
	name, secret, volume := s.workloadName(), s.secretName(), s.volumeName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("hunter2")})
	s.Require().NoError(err)

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	// The command proves all three properties at once: the mounted value is readable,
	// the volume accepts a write, and the root filesystem does not. The workload only
	// exits cleanly when the write to the rootfs failed.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.ReadOnly = true
	spec.Container.Command = []string{"sh", "-c",
		`cat /var/secret.txt && echo written > /var/lib/example/file && ! touch /rootfs-write`}
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret.txt"},
		{Name: volume, To: "/var/lib/example"},
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "hunter2")

	s.Equal("written\n", s.volumeFile(created.Path, "file"))
}

// TestChangingAMountedValueRedeploysItsWorkload covers the default delivery mode: a
// mount naming no signal is replaced, exactly as a referenced secret is.
func (s *Suite) TestChangingAMountedValueRedeploysItsWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("first")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	applied, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	_, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("second")})
	s.Require().NoError(err)
	s.False(created)

	// A workload that reads its file once at startup has to be replaced to see a new
	// value, which is what a moved hash arranges.
	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)

		return err == nil && workload.Version > applied.Version
	}, convergeTimeout, 500*time.Millisecond, "rotating a mounted secret did not bump the workload's version")

	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)

		return err == nil && len(workload.Instances) == 1 &&
			workload.Instances[0].ID != original &&
			workload.Instances[0].State == client.InstanceStateRunning
	}, convergeTimeout, 500*time.Millisecond, "rotating a mounted secret did not replace the instance")

	// The replacement reads the new value, which is what the whole exercise was for.
	s.Equal("second", s.mountedFile(name, "/var/secret"))
}

// TestSignallingAMountedValueKeepsTheWorkload covers the other delivery mode, which is
// the reason a mount may name a signal: the file changes underneath a workload that
// keeps running.
func (s *Suite) TestSignallingAMountedValueKeepsTheWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("first")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret", Signal: manifest.SignalHUP},
	}

	applied, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	original := s.instanceID(name)
	s.Require().Equal("first", s.mountedFile(name, "/var/secret"))

	_, created, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("second")})
	s.Require().NoError(err)
	s.False(created)

	// The file changes underneath the running container, which only works because it is
	// rewritten in place: a bind mount follows the inode, so a replaced file would
	// leave the container reading the old value forever.
	s.Require().Eventuallyf(func() bool {
		return s.mountedFile(name, "/var/secret") == "second"
	}, convergeTimeout, 500*time.Millisecond, "the mounted secret was never refreshed")

	// And the workload was never disturbed. This is what naming a signal asks for, and
	// what a moved hash would have prevented.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Equal(applied.Version, workload.Version, "signalling a mounted secret bumped the workload's version")
	s.Require().Len(workload.Instances, 1)
	s.Equal(original, workload.Instances[0].ID, "signalling a mounted secret replaced the instance")
	s.Equal(client.InstanceStateRunning, workload.Instances[0].State)
}

// TestDeletingAWorkloadRemovesItsMountedValues covers the plaintext leaving the disk,
// which is what bounds how long mounting a secret exposes it.
func (s *Suite) TestDeletingAWorkloadRemovesItsMountedValues() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("on-disk")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// The file exists while the workload does, which is the cost of mounting a value.
	s.Require().Equal("on-disk", s.mountedFile(name, "/var/secret"))
	s.Require().True(s.mountsHold("on-disk"), "the mounted secret was never written to the host")

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// And no longer, which is what makes mounting a bounded exposure rather than a
	// permanent one.
	s.False(s.mountsHold("on-disk"), "the mounted secret outlived the workload")
}

// TestMountingAnUnknownValueIsRejected covers the apply being refused rather than the
// workload being stored and left unable to start.
func (s *Suite) TestMountingAnUnknownValueIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: "e2e-sec-does-not-exist", To: "/var/secret"}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-sec-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestExecWorkloadMountsAValue covers a mounted value reaching the other runtime, which
// places it by symlink rather than by bind mount and so reads it by the relative path.
func (s *Suite) TestExecWorkloadMountsAValue() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("exec-mounted")})
	s.Require().NoError(err)

	// The relative path, as for a mounted volume: making the absolute one resolve there
	// would need the process confined to its own directory.
	spec := s.execSpec(name, "sh", "-c", `cat var/secret; exit 0`)
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "exec-mounted")
}

// TestStopRemovesMountedValues covers what suspension does to the disk: a stopped
// workload has no reader for the values it mounted, so their plaintext is removed
// rather than sitting there for the life of the suspension.
func (s *Suite) TestStopRemovesMountedValues() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("plaintext")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// The value is on the disk while the workload runs — that is what a mount is.
	mounted, err := filepath.Glob(filepath.Join(s.directory, "mounts", "files", "*"))
	s.Require().NoError(err)
	s.Require().NotEmpty(mounted)

	_, err = s.client.Stop(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// The wait returns when nothing is running, and the removal happens on the pass
	// that observes that, so the files may be a pass behind the stop.
	s.Require().Eventuallyf(func() bool {
		remaining, err := filepath.Glob(filepath.Join(s.directory, "mounts", "files", "*"))

		return err == nil && len(remaining) == 0
	}, convergeTimeout, 500*time.Millisecond, "stopping a workload left its mounted values on the disk")
}
