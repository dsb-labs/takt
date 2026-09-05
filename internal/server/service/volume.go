package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/pkg/manifest"
)

var (
	// ErrVolumeNotFound is returned when no volume exists with the requested name.
	ErrVolumeNotFound = errors.New("volume not found")
	// ErrVolumeExists is returned when a volume already exists with the given name.
	ErrVolumeExists = errors.New("volume already exists")
	// ErrVolumeInUse is returned when a volume a workload mounts is deleted without
	// being forced.
	ErrVolumeInUse = errors.New("volume is in use")
	// ErrInvalidVolume is returned when a volume's name is not one orca will accept.
	ErrInvalidVolume = errors.New("invalid volume")
)

// The names a volume may have, which are the names a workload may have. A volume's
// name reaches orca from a manifest and is reported back to an operator, so it is
// held to the same shape rather than to whatever a filesystem would tolerate.
var volumeNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type (
	// The VolumeRepository interface describes the persistence operations the volume
	// service uses.
	VolumeRepository interface {
		// Insert should record a new volume, assigning its identifier and
		// creation time.
		Insert(ctx context.Context, volume database.Volume) (database.Volume, error)
		// Update should replace the mutable fields of the volume with the given
		// volume's name: its labels, owner and mode.
		Update(ctx context.Context, volume database.Volume) (database.Volume, error)
		// Get should return the volume with the given name.
		Get(ctx context.Context, name string) (database.Volume, error)
		// List should return the volumes matching every one of the given queries,
		// or every volume when given none.
		List(ctx context.Context, queries ...database.Query) ([]database.Volume, error)
		// Delete should remove the volume with the given name.
		Delete(ctx context.Context, name string) error
		// UsedBy should name the workloads whose specifications mount the volume
		// with the given name.
		UsedBy(ctx context.Context, name string) ([]string, error)
	}

	// The Volume type describes a volume as it is reported to a caller.
	Volume struct {
		// The name that identifies the volume.
		Name string
		// Where the volume's data is on the host.
		//
		// Reported to a caller of the API, which is how something taking a backup
		// finds it. Not given to the workloads mounting the volume: one told where it
		// sits inside orca's data directory could walk out of it.
		Path string
		// The names of the workloads whose specifications mount the volume.
		UsedBy []string
		// Arbitrary key-value pairs attached to the volume.
		Labels map[string]string
		// The time the volume was created.
		CreatedAt time.Time
	}

	// The VolumeService type owns volumes and the directories backing them.
	VolumeService struct {
		logger  *slog.Logger
		volumes VolumeRepository
		root    string
	}

	// The VolumeServiceConfig type contains fields used to construct a
	// VolumeService.
	VolumeServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the volumes.
		Volumes VolumeRepository
		// The directory holding every volume, one per identifier.
		Root string
	}
)

// NewVolumeService returns a new instance of the VolumeService type.
func NewVolumeService(config VolumeServiceConfig) *VolumeService {
	return &VolumeService{
		logger:  config.Logger.With("component", "service"),
		volumes: config.Volumes,
		root:    config.Root,
	}
}

// Create records a volume with the given name and creates the directory backing it.
//
// The row is written first. A directory with no row is invisible to everything and
// would be created again under a new identifier, where a row with no directory is
// reported as a volume whose data cannot be reached — so the failure that leaves
// nothing behind is the one to prefer.
func (s *VolumeService) Create(ctx context.Context, name string, labels map[string]string) (Volume, error) {
	if !volumeNamePattern.MatchString(name) || len(name) > 63 {
		return Volume{}, fmt.Errorf("%w: name must be lowercase alphanumeric, optionally separated by dashes", ErrInvalidVolume)
	}

	if err := manifest.ValidateLabels(labels); err != nil {
		return Volume{}, fmt.Errorf("%w: %v", ErrInvalidVolume, err)
	}

	stored, err := s.volumes.Insert(ctx, database.Volume{Name: name, Labels: labels})
	switch {
	case errors.Is(err, database.ErrVolumeExists):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeExists, name)
	case err != nil:
		return Volume{}, err
	}

	path, err := s.path(stored.ID)
	if err != nil {
		return Volume{}, err
	}

	// Readable only by the user running the server, like everything else orca keeps.
	// A workload runs as that user, so this is about what else on the host can read a
	// workload's data rather than about the workload itself.
	if err = os.MkdirAll(path, 0o700); err != nil {
		// The row would otherwise name a volume whose directory does not exist, and
		// nothing later creates it.
		if removeErr := s.volumes.Delete(ctx, name); removeErr != nil {
			s.logger.With("volume", name, "error", removeErr).
				Error("failed to remove a volume whose directory could not be created")
		}

		return Volume{}, fmt.Errorf("failed to create volume directory: %w", err)
	}

	s.logger.With("volume", name).Debug("volume created")

	return Volume{Name: stored.Name, Path: path, Labels: stored.Labels, CreatedAt: stored.CreatedAt}, nil
}

// Get returns the volume with the given name, along with the workloads mounting it.
func (s *VolumeService) Get(ctx context.Context, name string) (Volume, error) {
	stored, err := s.volumes.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrVolumeNotFound):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return Volume{}, err
	}

	usedBy, err := s.volumes.UsedBy(ctx, name)
	if err != nil {
		return Volume{}, err
	}

	return s.hydrate(stored, usedBy)
}

// List returns the volumes matching every one of the given queries, along with
// the workloads mounting each one. Passing no queries returns every volume.
//
// Each query is a "path=value" string, where the path is a JSON path into the
// volume's labels, such as "$.labels.app". Returns ErrInvalidQuery when one
// is malformed.
func (s *VolumeService) List(ctx context.Context, queries ...string) ([]Volume, error) {
	parsed, err := parseQueries(queries)
	if err != nil {
		return nil, err
	}

	stored, err := s.volumes.List(ctx, parsed...)
	if err != nil {
		if errors.Is(err, database.ErrInvalidQueryPath) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
		}

		return nil, err
	}

	volumes := make([]Volume, 0, len(stored))
	for _, row := range stored {
		usedBy, err := s.volumes.UsedBy(ctx, row.Name)
		if err != nil {
			return nil, err
		}

		volume, err := s.hydrate(row, usedBy)
		if err != nil {
			return nil, err
		}

		volumes = append(volumes, volume)
	}

	return volumes, nil
}

// Delete removes the volume with the given name and everything stored in it.
//
// A volume a workload mounts is refused unless force is set, and the error names the
// workloads holding it. Nothing else in orca removes a volume, so this is the only
// thing that destroys stored data.
func (s *VolumeService) Delete(ctx context.Context, name string, force bool) error {
	stored, err := s.volumes.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrVolumeNotFound):
		return fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return err
	}

	usedBy, err := s.volumes.UsedBy(ctx, name)
	if err != nil {
		return err
	}

	if len(usedBy) > 0 && !force {
		return fmt.Errorf("%w: mounted by %s", ErrVolumeInUse, strings.Join(usedBy, ", "))
	}

	path, err := s.path(stored.ID)
	if err != nil {
		return err
	}

	// The directory goes first. A row with no directory reads as a volume whose data
	// cannot be reached, where a directory with no row is unreachable and unnamed:
	// nothing would ever remove it, since removing one is only ever asked for by
	// name.
	//
	// A permission failure names its fix, because nothing else about it says why a
	// directory orca created cannot be removed: a container wrote to the volume as a
	// user other than the server's, which is what an image that switches user does.
	if err = os.RemoveAll(path); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("failed to remove volume directory: %w: grant the server CAP_DAC_OVERRIDE to remove files a workload wrote as another user", err)
		}

		return fmt.Errorf("failed to remove volume directory: %w", err)
	}

	if err = s.volumes.Delete(ctx, name); err != nil {
		return err
	}

	s.logger.With("volume", name, "forced", force).Info("volume deleted")

	return nil
}

// Path returns where the named volume's data is, so that a caller resolving a mount
// does not have to know how a volume is laid out.
func (s *VolumeService) Path(ctx context.Context, name string) (string, error) {
	stored, err := s.volumes.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrVolumeNotFound):
		return "", fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return "", err
	}

	return s.path(stored.ID)
}

// Update replaces the mutable fields of the volume with the given name, returning it
// as it now stands. Returns ErrVolumeNotFound when no such volume exists.
//
// The labels are the whole of what a volume has to change. Its name identifies it,
// its identifier is what its directory is named for, and its contents are the
// workloads' to write — so there is nothing else an update of a volume could mean.
func (s *VolumeService) Update(ctx context.Context, name string, labels map[string]string) (Volume, error) {
	if err := manifest.ValidateLabels(labels); err != nil {
		return Volume{}, fmt.Errorf("%w: %v", ErrInvalidVolume, err)
	}

	stored, err := s.volumes.Update(ctx, database.Volume{Name: name, Labels: labels})
	switch {
	case errors.Is(err, database.ErrVolumeNotFound):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return Volume{}, err
	}

	usedBy, err := s.volumes.UsedBy(ctx, name)
	if err != nil {
		return Volume{}, err
	}

	return s.hydrate(stored, usedBy)
}

func (s *VolumeService) hydrate(row database.Volume, usedBy []string) (Volume, error) {
	path, err := s.path(row.ID)
	if err != nil {
		return Volume{}, err
	}

	return Volume{
		Name:      row.Name,
		Path:      path,
		UsedBy:    usedBy,
		Labels:    row.Labels,
		CreatedAt: row.CreatedAt,
	}, nil
}

// path returns the directory holding the volume with the given identifier.
//
// The identifier is checked before it becomes a path component, for the same reason
// the exec driver checks a workload's: a value that has not been looked at should not
// be joined onto a directory orca removes things from. It comes from the database
// here rather than from a manifest, which makes this a guard against orca's own
// mistakes rather than a caller's.
func (s *VolumeService) path(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: identifier %q is not usable as a directory", ErrInvalidVolume, id)
	}

	return filepath.Join(s.root, id), nil
}

// xid values: twenty lowercase alphanumeric characters.
var idPattern = regexp.MustCompile(`^[0-9a-v]{20}$`)
