package e2e_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestVolumeSurvivesAnExecWorkloadBeingReplaced covers the thing volumes exist for.
//
// Stopping an exec workload removes the tree its working directory sits in, and the
// reconciler stops a workload before every replacement and every retry. So a volume
// that did not survive that would be no better than the working directory it replaces,
// which is what made this worth writing before volumes existed.
func (s *Suite) TestVolumeSurvivesAnExecWorkloadBeingReplaced() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)
	s.Require().NotEmpty(created.Path)

	// The command writes through the relative path, since the volume is placed inside
	// the directory the process runs in. It appends, so a second run leaves both lines.
	spec := s.execSpec(name, "sh", "-c", "echo ran >> var/lib/example/runs; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Equal("ran\n", s.volumeFile(created.Path, "runs"))

	// A changed specification replaces the instance, which stops the workload and
	// removes its working directory before starting the new one.
	spec.Env = map[string]string{"CHANGED": "yes"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// Two lines rather than one: the volume kept what the first run wrote, and the
	// replacement appended to it rather than starting from nothing.
	s.Require().Eventually(func() bool {
		return s.volumeFile(created.Path, "runs") == "ran\nran\n"
	}, convergeTimeout, 250*time.Millisecond, "the volume did not survive the workload being replaced")
}

// TestVolumeOutlivesTheWorkloadThatMountsIt covers the lifecycle that makes a volume a
// resource of its own: deleting a workload leaves what it stored.
func (s *Suite) TestVolumeOutlivesTheWorkloadThatMountsIt() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	spec := s.execSpec(name, "sh", "-c", "echo precious > var/lib/example/file; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// While the workload exists, the volume reports it as a holder and refuses to be
	// deleted.
	held, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	s.ErrorIs(s.client.DeleteVolume(s.ctx(), volume), client.ErrVolumeInUse)

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// The workload is gone and its data is not.
	s.Equal("precious\n", s.volumeFile(created.Path, "file"))

	free, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Empty(free.UsedBy, "the deleted workload is still reported as mounting the volume")

	// Only deleting the volume removes the data, which is the whole point of it being
	// a separate thing to delete.
	s.Require().NoError(s.client.DeleteVolume(s.ctx(), volume))

	_, err = s.client.GetVolume(s.ctx(), volume)
	s.ErrorIs(err, client.ErrVolumeNotFound)
	s.Empty(s.volumeFile(created.Path, "file"))
}

// TestVolumeMountedIntoAContainer covers the other runtime, where the volume is a bind
// mount and the workload uses the path as written.
func (s *Suite) TestVolumeMountedIntoAContainer() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	// An absolute path, used as written: a container has a filesystem of its own, so
	// the daemon can put the volume where the manifest says.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", "echo from-a-container > /var/lib/example/file"}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)
	s.Equal("from-a-container\n", s.volumeFile(created.Path, "file"))
}

// TestWorkloadMountingAnUnknownVolumeIsRejected covers the apply being refused rather
// than the volume being created, so a mistyped name is reported.
func (s *Suite) TestWorkloadMountingAnUnknownVolumeIsRejected() {
	name := s.workloadName()

	spec := s.execSpec(name, "sh", "-c", "exit 0")
	spec.Volumes = []manifest.VolumeMount{{Name: s.volumeName(), To: "/var/lib/example"}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)

	// The volume is named, since an operator who mistyped one can act on that and not
	// on "something went wrong".
	s.Contains(err.Error(), s.volumeName())

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	// And no volume was conjured up to satisfy the mount.
	_, err = s.client.GetVolume(s.ctx(), s.volumeName())
	s.ErrorIs(err, client.ErrVolumeNotFound)
}

// TestPathMountReachesTheHost covers a workload reading data takt does not manage,
// which is what a path mount exists for. The server is restarted with the host
// directory allowed, since the default configuration refuses every path mount.
func (s *Suite) TestPathMountReachesTheHost() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// A host directory outside takt's data directory, holding a file the workload
	// reads back through the mount.
	host := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(host, "greeting"), []byte("hello from the host\n"), 0o644))

	s.restart(withAllowHostPaths(host))

	// The exec runtime reaches the mount by the relative path, like a volume.
	spec := s.execSpec(name, "sh", "-c", "cat host-data/greeting; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Path: host, To: "/host-data"}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var logs strings.Builder
	s.Require().NoError(s.client.Logs(s.ctx(), &logs, name, client.WithTail(10)))
	s.Contains(logs.String(), "hello from the host")
}

// TestReadOnlyPathMountInAContainer covers the read-only flag being enforced by the
// bind rather than politely recorded: the workload proves it can read the mount and
// cannot write it.
func (s *Suite) TestReadOnlyPathMountInAContainer() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	host := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(host, "greeting"), []byte("hello from the host\n"), 0o644))

	s.restart(withAllowHostPaths(host))

	// The command exits cleanly only when the read succeeds and the write is
	// refused, so completion is the assertion.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c",
		"cat /host-data/greeting || exit 1; touch /host-data/nope 2>/dev/null && exit 1; exit 0"}
	spec.Volumes = []manifest.VolumeMount{{Path: host, To: "/host-data", ReadOnly: true}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// And the host directory holds only what it started with.
	_, err = os.Stat(filepath.Join(host, "nope"))
	s.True(os.IsNotExist(err), "the workload wrote through a read-only mount")
}

// TestPathMountRefusedByDefault covers the gate: an unconfigured
// server accepts no path mount at all, and nothing is stored when one is refused.
func (s *Suite) TestPathMountRefusedByDefault() {
	name := s.workloadName()

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Path: "/etc", To: "/host-etc", ReadOnly: true}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)

	// The error names the configuration that opens the path, since an operator who
	// meant it can act on that and not on "something went wrong".
	s.Contains(err.Error(), "allow-host-paths")

	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestVolumeOwnershipReachesTheDirectory covers a volume's owner and mode being
// applied to the directory backing it, on create and again on update.
func (s *Suite) TestVolumeOwnershipReachesTheDirectory() {
	volume := s.volumeName()
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	// The server runs inside the test process, so its own identifiers are the ones
	// a chown needs no capability for.
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Owner:   owner,
		Mode:    "0750",
	})
	s.Require().NoError(err)
	s.Equal(owner, created.Owner)
	s.Equal("0750", created.Mode)

	info, err := os.Stat(created.Path)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o750), info.Mode().Perm())

	// An update is how a live volume changes hands, so the new mode has to reach
	// the directory rather than only the row.
	updated, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Owner:   owner,
		Mode:    "0755",
	})
	s.Require().NoError(err)
	s.Equal("0755", updated.Mode)

	info, err = os.Stat(created.Path)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o755), info.Mode().Perm())
}

// TestMissingVolume covers the not-found path on every volume endpoint that takes a
// name.
func (s *Suite) TestMissingVolume() {
	_, err := s.client.GetVolume(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrVolumeNotFound)

	s.ErrorIs(s.client.DeleteVolume(s.ctx(), "does-not-exist"), client.ErrVolumeNotFound)
}

// TestVolumeApplyIsIdempotent covers the reason apply replaced create: the
// stored volume becomes what the manifest says, however many times it is
// applied, and the path and creation time it was given never move.
func (s *Suite) TestVolumeApplyIsIdempotent() {
	volume := s.volumeName()
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	spec := manifest.Volume{
		Version: "v1",
		Name:    volume,
		Labels:  map[string]string{"app": "example"},
	}

	created, err := s.client.ApplyVolume(s.ctx(), spec)
	s.Require().NoError(err)

	applied, err := s.client.ApplyVolume(s.ctx(), spec)
	s.Require().NoError(err)
	s.Equal(created.Path, applied.Path)
	s.Equal(created.CreatedAt, applied.CreatedAt)
	s.Equal(created.Labels, applied.Labels)
}

// TestVolumeLabelsSurviveAnUpdate covers the write path a volume gained for its
// labels, which is the only thing about a volume that can change.
func (s *Suite) TestVolumeLabelsSurviveAnUpdate() {
	volume := s.volumeName()
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Labels:  map[string]string{"app": "web"},
	})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "web"}, created.Labels)

	updated, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Labels:  map[string]string{"app": "api"},
	})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "api"}, updated.Labels)

	// The identifier the data is stored under is untouched, which is what makes this
	// safe to run against a volume holding something.
	s.Equal(created.Path, updated.Path)

	read, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "api"}, read.Labels)
}
