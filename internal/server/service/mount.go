package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/wire"
	"github.com/dsb-labs/orca/pkg/manifest"
)

var (
	// ErrInvalidMount is returned when a mount could not be materialised as a file.
	ErrInvalidMount = errors.New("invalid mount")
)

const (
	// The directory beneath the data directory holding everything about mounted
	// values.
	mountDir = "mounts"
	// The tree holding the files workloads mount.
	mountFileDir = "files"
	// The tree holding what the service records about what it wrote, which no
	// workload has a path to.
	mountStateDir = "state"
)

type (
	// The MountService type materialises the secrets and variables a workload mounts
	// as files on the host, and owns the directories holding them.
	//
	// It exists because a runtime mounts a path rather than a value: there is no way
	// to put a secret inside a container without writing it somewhere first. Keeping
	// that in one place means one component decides where such a file lives, what it
	// is called, who can read it and when it is removed.
	//
	// The files are written as a workload starts and removed when it stops, so a
	// value's plaintext exists on disk for as long as the workload reading it and no
	// longer.
	MountService struct {
		logger    *slog.Logger
		secrets   ValueStore
		variables ValueStore
		files     string
		state     string
	}

	// The MountServiceConfig type contains fields used to construct a MountService.
	MountServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// Where the secrets a workload mounts are read from. May be nil, in which case
		// a workload mounting a secret fails to start.
		Secrets ValueStore
		// Where the variables a workload mounts are read from. May be nil, in which
		// case a workload mounting a variable fails to start.
		Variables ValueStore
		// The directory orca keeps its state in. Mounted values live in a
		// subdirectory of it.
		Directory string
	}

	// The Refresh type reports one mounted value whose contents changed, and what the
	// workload asked to be told with.
	Refresh struct {
		// What the mount reads, so that a caller can report which value moved.
		Reference manifest.Reference
		// The signal the workload asked for.
		Signal manifest.Signal
	}

	// The delivered type is what the service records about the files it wrote for one
	// version of a workload.
	//
	// Only a digest of each value is kept, never the value. The record lives beside
	// the files it describes rather than in the database, because it describes what is
	// on this host's disk: a value's plaintext is already there, and the digest says
	// nothing more than whether it has changed.
	delivered struct {
		// The digest of what was written for each mount, keyed by the file's name.
		Digests map[string]string `json:"digests"`
	}
)

// NewMountService returns a new instance of the MountService type.
func NewMountService(config MountServiceConfig) *MountService {
	root := filepath.Join(config.Directory, mountDir)

	return &MountService{
		logger:    config.Logger.With("component", "service"),
		secrets:   config.Secrets,
		variables: config.Variables,
		// Two trees rather than one, for the reason the exec driver has two: a
		// workload reaches the files mounted into it, so what orca records about
		// having written them is kept where the workload has no path to it.
		files: filepath.Join(root, mountFileDir),
		state: filepath.Join(root, mountStateDir),
	}
}

// Deliver writes a file for every secret and variable the specification mounts, and
// returns them as mounts the driver can honour.
//
// Called as a workload starts, so a value's plaintext is written as late as it can be
// and the workload always starts against what orca holds now. A workload mounting
// nothing writes nothing and creates no directories.
//
// Returns ErrSecretNotFound or ErrVariableNotFound naming what it could not read.
// Writing an empty file instead would hand the workload a value orca does not hold,
// which it would then use.
func (s *MountService) Deliver(ctx context.Context, id string, version int, spec api.WorkloadSpec) ([]driver.Volume, error) {
	mounts := valueMounts(spec)
	if len(mounts) == 0 {
		return nil, nil
	}

	dir, err := s.version(s.files, id, version)
	if err != nil {
		return nil, err
	}

	// Readable only by the user running the server. The files inside are readable by
	// anyone who can reach them, so that a container running as a user of its own can
	// read what it mounts; this directory is what stops anything else on the host
	// reaching them at all.
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create mount directory: %w", pathless(err))
	}

	record := delivered{Digests: make(map[string]string, len(mounts))}

	volumes := make([]driver.Volume, 0, len(mounts))
	for _, mount := range mounts {
		reference, _ := mount.Reference()

		value, err := s.value(ctx, reference)
		if err != nil {
			return nil, err
		}

		name := fileName(reference)

		if err = write(filepath.Join(dir, name), value); err != nil {
			return nil, err
		}

		record.Digests[name] = digest(value)

		volumes = append(volumes, driver.Volume{
			Name:   reference.Name,
			Host:   filepath.Join(dir, name),
			Target: mount.To,
		})
	}

	if err = s.record(id, version, record); err != nil {
		return nil, err
	}

	s.logger.With("workload", id, "version", version, "mounts", len(volumes)).Debug("mounted values delivered")

	return volumes, nil
}

// Refresh rewrites the mounted values that have changed since they were delivered,
// and reports the signal each affected workload asked for.
//
// Only a mount naming a signal is considered. One that named none asked to be
// replaced, which the specification's hash already arranges, and rewriting its file
// underneath a running workload would change what it read without telling it.
//
// The file is rewritten in place rather than replaced. A bind mount follows the inode,
// so a renamed file would leave the workload reading the old contents forever — which
// is the one failure this exists to avoid. The caller signals the workload once this
// returns, so nothing is told to reload a file that has not been written yet.
func (s *MountService) Refresh(ctx context.Context, name, id string, version int, spec api.WorkloadSpec) ([]Refresh, error) {
	mounts := signalledMounts(spec)
	if len(mounts) == 0 {
		return nil, nil
	}

	dir, err := s.version(s.files, id, version)
	if err != nil {
		return nil, err
	}

	record, err := s.delivered(id, version)
	if err != nil {
		return nil, err
	}

	// Nothing was recorded for this version, so there is nothing to compare against
	// and no file of ours to rewrite. The workload has not started yet, and delivery
	// is what writes both.
	if len(record.Digests) == 0 {
		return nil, nil
	}

	var refreshed []Refresh

	for _, mount := range mounts {
		reference, _ := mount.Reference()

		value, err := s.value(ctx, reference)
		if err != nil {
			return nil, err
		}

		file := fileName(reference)
		current := digest(value)

		if previous, ok := record.Digests[file]; !ok || current == previous {
			// Unchanged, or never delivered for this version. Either way there is
			// nothing to tell the workload about.
			continue
		}

		if err = write(filepath.Join(dir, file), value); err != nil {
			return nil, err
		}

		record.Digests[file] = current

		refreshed = append(refreshed, Refresh{Reference: reference, Signal: mount.Signal})
	}

	if len(refreshed) == 0 {
		return nil, nil
	}

	// Recorded after the files are written, so a failure part-way leaves the old
	// digests and the next pass writes again rather than reporting the change
	// delivered when it was not.
	if err = s.record(id, version, record); err != nil {
		return nil, err
	}

	s.logger.With("workload", name, "version", version, "mounts", len(refreshed)).Info("mounted values refreshed")

	return refreshed, nil
}

// Forget removes everything the service wrote for a workload, which is what takes a
// mounted value's plaintext off the disk.
//
// Called once nothing is running for the workload, so the files outlive the process
// reading them by no longer than the teardown takes. A workload the service never
// wrote for is not an error: it mounted nothing, and there is nothing to remove.
func (s *MountService) Forget(id string) error {
	for _, tree := range []string{s.files, s.state} {
		dir, err := s.dir(tree, id)
		if err != nil {
			return err
		}

		if err = os.RemoveAll(dir); err != nil {
			return fmt.Errorf("failed to remove mount directory: %w", pathless(err))
		}
	}

	return nil
}

// Prune removes what the service wrote for workloads other than those named.
//
// This is how a value delivered by a server that stopped before it could tear the
// workload down is eventually removed. Left alone, such a file would sit on the disk
// holding a secret's plaintext for a workload that no longer exists.
func (s *MountService) Prune(keep []string) error {
	// A set rather than a scan of the slice per directory, so that a host with many
	// workloads does not turn this into a comparison of every name against every other.
	wanted := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		wanted[id] = struct{}{}
	}

	for _, tree := range []string{s.files, s.state} {
		entries, err := os.ReadDir(tree)
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing has ever been delivered on this host.
				continue
			}

			return fmt.Errorf("failed to read mount directory: %w", pathless(err))
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			if _, ok := wanted[entry.Name()]; ok {
				continue
			}

			if err = os.RemoveAll(filepath.Join(tree, entry.Name())); err != nil {
				return fmt.Errorf("failed to remove mount directory: %w", pathless(err))
			}

			s.logger.With("workload", entry.Name()).Debug("removed the mounted values of a workload that no longer exists")
		}
	}

	return nil
}

// value reads what the reference names from whichever store holds that kind.
//
// Unlike the resolver's, something the store does not hold is an error rather than a
// false: a mount names what it reads directly, so there is no expansion to report it
// and nothing else that would.
func (s *MountService) value(ctx context.Context, reference manifest.Reference) (string, error) {
	store, missing := storeFor(s.secrets, s.variables, reference.Kind)

	// A server holding no store of that kind holds nothing under the name, which is
	// the same answer as a name nobody created.
	if store == nil {
		return "", fmt.Errorf("%w: %s", missing, reference.Name)
	}

	value, err := store.Value(ctx, reference.Name)
	if err != nil {
		return "", err
	}

	return value, nil
}

// record writes what was delivered for one version of a workload.
func (s *MountService) record(id string, version int, record delivered) error {
	dir, err := s.dir(s.state, id)
	if err != nil {
		return err
	}

	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create mount state directory: %w", pathless(err))
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to encode delivered mounts: %w", err)
	}

	path := filepath.Join(dir, strconv.Itoa(version)+".json")

	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("failed to record delivered mounts: %w", pathless(err))
	}

	return nil
}

// delivered reads what was written for one version of a workload, reporting an empty
// record when nothing has been.
func (s *MountService) delivered(id string, version int) (delivered, error) {
	dir, err := s.dir(s.state, id)
	if err != nil {
		return delivered{}, err
	}

	contents, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(version)+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing has been delivered for this version, which is what a workload
			// that has not started yet looks like.
			return delivered{}, nil
		}

		return delivered{}, fmt.Errorf("failed to read delivered mounts: %w", pathless(err))
	}

	var record delivered
	if err = json.Unmarshal(contents, &record); err != nil {
		return delivered{}, fmt.Errorf("failed to decode delivered mounts: %w", err)
	}

	return record, nil
}

// dir returns the directory holding a workload's mounts beneath the given tree.
//
// The identifier is checked before it becomes a path component, for the same reason the
// volume service checks one: a service that removes directories should not build a path
// from a value it has not looked at.
func (s *MountService) dir(tree, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: identifier %q is not usable as a directory", ErrInvalidMount, id)
	}

	return filepath.Join(tree, id), nil
}

// version returns the directory holding one version of a workload's mounts.
//
// Keyed by version as the exec driver's directories are, so that a replacement's files
// do not overwrite those of the instance it is replacing while that one is still
// reading them.
func (s *MountService) version(tree, id string, version int) (string, error) {
	dir, err := s.dir(tree, id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, strconv.Itoa(version)), nil
}

// write puts value in the file at path, creating it if it does not exist and
// truncating it if it does.
//
// The same file is rewritten rather than replaced, because a bind mount follows the
// inode: a container reads what was mounted, so a file swapped underneath it would
// leave the workload reading the old contents indefinitely.
//
// The file ends up read-only and readable by anyone who can reach it, which is wider
// than orca's other files. A container runs as a user of its own, rarely the one
// running the server, so a file only that user could read would be unreadable by the
// workload that mounted it. What keeps it private is the directory above, which only
// the server's user may enter.
//
// Rewriting one therefore has to widen the mode first: a read-only file cannot be
// opened for writing even by its owner. The window that opens is inside a directory
// nothing else on the host can enter, and the mode is narrowed again before the caller
// signals anything.
func write(path, value string) error {
	// A file that does not exist yet is the ordinary case, since a value is written
	// before it is ever rewritten. Anything else is reported here rather than left to
	// fail the write below, which would name the write when the mode is what stopped it.
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to make mounted value writable: %w", pathless(err))
	}

	if err := os.WriteFile(path, []byte(value), 0o444); err != nil {
		return fmt.Errorf("failed to write mounted value: %w", pathless(err))
	}

	// WriteFile applies the mode only when it creates the file, so one that already
	// existed still carries what it was widened to.
	if err := os.Chmod(path, 0o444); err != nil {
		return fmt.Errorf("failed to set mounted value permissions: %w", pathless(err))
	}

	return nil
}

// pathless strips the filesystem path from an error, keeping the cause.
//
// The service's errors reach the API, and a path inside the data directory is a
// detail of the host that a caller has no business seeing. The wrap above each
// call already names the operation, so the path adds nothing the cause does not.
func pathless(err error) error {
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err
	}

	if linkErr, ok := errors.AsType[*os.LinkError](err); ok {
		return linkErr.Err
	}

	return err
}

// fileName returns what a mounted value's file is called.
//
// The kind is part of the name because a secret and a variable may share one, and the
// two are different values. Neither is used as a directory: the name is validated as a
// reference name, which cannot contain a separator or a dot.
func fileName(reference manifest.Reference) string {
	return string(reference.Kind) + "-" + reference.Name
}

// digest returns what is recorded about a delivered value, which is enough to tell
// whether it changed and nothing more.
//
// A digest rather than the value, because this is written to disk and read back on
// every pass: recording a secret's plaintext a second time, in a file whose only job
// is to answer "has this moved", would put it somewhere nothing needs it.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}

// valueMounts returns the mounts of a secret or a variable the specification names, in
// the order it names them.
//
// Only the mounts are converted, rather than the whole specification through NewSpec.
// This is asked on every pass for every running workload, and converting the rest would
// allocate a restart policy, a port slice and a health check only to discard them.
func valueMounts(spec api.WorkloadSpec) []manifest.VolumeMount {
	if spec.Volumes == nil {
		return nil
	}

	var mounts []manifest.VolumeMount
	for _, mount := range *spec.Volumes {
		converted := wire.ToVolumeMount(mount)
		if _, ok := converted.Reference(); ok {
			mounts = append(mounts, converted)
		}
	}

	return mounts
}

// signalledMounts returns the mounts whose changes the workload asked to be signalled
// about rather than replaced for.
//
// One pass over the specification rather than a filter of valueMounts, so the common
// case of a workload with nothing to refresh allocates nothing at all.
func signalledMounts(spec api.WorkloadSpec) []manifest.VolumeMount {
	if spec.Volumes == nil {
		return nil
	}

	var mounts []manifest.VolumeMount
	for _, mount := range *spec.Volumes {
		if mount.Signal == nil || *mount.Signal == "" {
			continue
		}

		converted := wire.ToVolumeMount(mount)
		if _, ok := converted.Reference(); ok {
			mounts = append(mounts, converted)
		}
	}

	return mounts
}
