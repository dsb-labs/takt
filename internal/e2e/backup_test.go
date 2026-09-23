package e2e_test

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"

	"github.com/dsb-labs/takt/internal/restore"
	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// TestBackupRestoresANode proves the whole point of the backup command: an archive
// taken from a server that is running restores onto an empty data directory and the
// node comes back.
//
// The database runs in write-ahead logging mode, so most of what a busy node has
// committed can be in the log rather than in state.db. A backup that copied the
// database file alone would restore a node missing whatever was applied most
// recently, and it would do it silently.
func (s *Suite) TestBackupRestoresANode() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("survives-a-restore")})
	s.Require().NoError(err)

	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	// Taken against the server that is still running, which is what makes this
	// different from stopping takt and copying files.
	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive, client.WithKeys()))

	restored := s.T().TempDir()
	s.restore(archive.Bytes(), restored)

	s.restart(withDataDirectory(restored))

	// The workload is in the restored database, and the server converged onto it
	// rather than treating the running container as an orphan.
	s.awaitState(name, client.WorkloadStateRunning)

	// The revision is unchanged, so the secret was restored rather than replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value opens under the key that came out of the archive, which only a
	// workload reading it can show.
	// A name of its own. Every name helper answers per test, so reusing one would
	// replace the workload under test rather than add a second one beside it.
	reader := s.workloadName() + "-reader"
	s.T().Cleanup(func() { s.cleanup(reader) })

	spec := s.jobSpec(reader, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(reader, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, reader, client.WithTail(10)))
	s.Contains(out.String(), "[survives-a-restore]")
}

// TestRestoreBringsBackVolumeData covers the part of a restore no archive can carry
// and no server can perform, which is the part that fails silently when it is got
// wrong.
//
// Volume data is arbitrary user data and is not in a backup, so it is copied by hand.
// A volume is found by the identifier it was assigned rather than by its name, so
// creating one of the same name on the restored node would give a fresh identifier,
// an empty volume, and a row pointing at nothing. Nothing about that reads as an
// error: the workload starts and its storage is empty.
//
// The workload writing into the volume runs on the host rather than in a container,
// so the files it leaves belong to the user running the test and copying them needs
// no privileges.
func (s *Suite) TestRestoreBringsBackVolumeData() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, volume := s.workloadName(), s.volumeName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	writer := s.execSpec(name, "sh", "-c", "echo precious > var/lib/example/file; exit 0")
	writer.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	writer.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), writer)
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateCompleted)

	s.Require().Equal("precious\n", s.volumeFile(created.Path, "file"))

	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive, client.WithKeys()))

	restored := s.T().TempDir()
	report := s.restore(archive.Bytes(), restored)

	// The restore says what it could not do, naming the identifier and the directory
	// the data belongs in. Without that an operator has a row and no way to work out
	// which directory answers to it.
	s.Empty(report.MissingKeys)
	s.Equal([]restore.Volume{{
		ID:   filepath.Base(created.Path),
		Name: volume,
		Path: filepath.Join(restored, "volumes", filepath.Base(created.Path)),
	}}, report.MissingVolumes)

	s.Require().NoError(os.MkdirAll(filepath.Join(restored, "volumes"), 0o700))
	s.Require().NoError(os.CopyFS(report.MissingVolumes[0].Path, os.DirFS(created.Path)))

	s.restart(withDataDirectory(restored))

	// The row still names the same directory, which is what the copy relied on.
	after, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal(report.MissingVolumes[0].Path, after.Path)
	s.Equal("precious\n", s.volumeFile(after.Path, "file"))

	// And a workload mounting it by name reaches the data, which is the whole chain:
	// the name resolves to the identifier, the identifier to the directory, and the
	// directory holds what was backed up.
	// A name of its own. Every name helper answers per test, so reusing one would
	// replace the workload under test rather than add a second one beside it.
	reader := s.workloadName() + "-reader"
	s.T().Cleanup(func() { s.cleanup(reader) })

	spec := s.execSpec(reader, "sh", "-c", "cat var/lib/example/file; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.awaitState(reader, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, reader, client.WithTail(10)))
	s.Contains(out.String(), "precious")
}

// TestBackupLeavesTheKeyringOut proves the default keeps the database and the keys
// apart, which is what makes an archive safe to keep somewhere a key would not be.
func (s *Suite) TestBackupLeavesTheKeyringOut() {
	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive))

	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	s.Require().NoError(err)

	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}

	s.Equal([]string{"state.db"}, names)
}

// TestRekeyKeepsSecretsReadable proves the point of the command: every secret is
// re-sealed under a new key and a workload reading one still gets its value.
//
// The workload is started before the rekey and read after it, so this covers the
// value surviving the rewrite rather than merely being set again.
func (s *Suite) TestRekeyKeepsSecretsReadable() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("survives-a-rekey")})
	s.Require().NoError(err)

	rekey, err := s.client.Rekey(s.ctx())
	s.Require().NoError(err)
	s.NotEmpty(rekey.KeyID)
	s.NotEqual(rekey.KeyID, rekey.PreviousKeyID)
	s.GreaterOrEqual(rekey.Secrets, 1)

	// The revision is unchanged. A rekey changes how a value is stored, not what it
	// is, so nothing reading it has any reason to be replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value opens under the key the rekey produced, which only a workload
	// reading it can show.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[survives-a-rekey]")
}

// TestRekeyDoesNotRedeployWorkloads proves the guarantee an operator decides a
// maintenance window on. A rekey moves no revision, so no specification hash moves
// and the instances that were running keep running.
func (s *Suite) TestRekeyDoesNotRedeployWorkloads() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("unchanged-by-a-rekey")})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	before, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(before.Instances, 1)

	_, err = s.client.Rekey(s.ctx())
	s.Require().NoError(err)

	// A pass has to have run against the rekeyed database before this means anything,
	// or it would pass simply by being asked too early.
	s.awaitState(name, client.WorkloadStateRunning)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(after.Instances, 1)

	s.Equal(before.Version, after.Version, "the rekey moved the workload's version")
	s.Equal(before.Instances[0].SpecHash, after.Instances[0].SpecHash, "the rekey moved a specification hash")

	// The same instance, not a replacement that happens to hash the same.
	s.Equal(before.Instances[0].ID, after.Instances[0].ID, "the rekey replaced the running instance")
}

// TestRekeySurvivesAServerRestart proves the database is what names the current key.
// A restarted server has a keyring holding both the old key and the new one, and only
// the rows say which of them opens what.
func (s *Suite) TestRekeySurvivesAServerRestart() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("across-a-rekey")})
	s.Require().NoError(err)

	rekey, err := s.client.Rekey(s.ctx())
	s.Require().NoError(err)

	// Both keys are on disk, so picking the newest file rather than reading the
	// database would be a coin toss the server must not be making.
	entries, err := os.ReadDir(filepath.Join(directory, "keys"))
	s.Require().NoError(err)
	s.Len(entries, 2)

	s.restart(withDataDirectory(directory))

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[across-a-rekey]")

	s.NotEmpty(rekey.PreviousKeyID)
}
