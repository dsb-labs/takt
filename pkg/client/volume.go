package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The Volume type is the client-side view of a volume: storage a workload mounts,
// with a lifetime of its own.
type Volume struct {
	// The name that identifies the volume.
	Name string
	// Where the volume's data is on the host running the server, which is what
	// something taking a backup needs.
	Path string
	// The names of the workloads whose specifications mount this volume. Empty for a
	// volume nothing is using, which is one that can be deleted without forcing.
	UsedBy []string
	// Arbitrary key-value pairs attached to the volume.
	Labels map[string]string
	// Who owns the volume's directory, as a numeric "uid" or "uid:gid". Empty
	// when the directory is owned by the user running the server.
	Owner string
	// The permission bits on the volume's directory, as an octal string. Empty
	// when the directory keeps the server's default.
	Mode string
	// The time the volume was created.
	CreatedAt time.Time
}

// checkName reports whether a name is usable as a single segment of a request path.
//
// The name is interpolated into the path, and Go's HTTP client resolves "." and ".."
// before sending, so a name carrying either would reach whichever endpoint the resolved
// path happens to name rather than failing. A manifest and the CLI both validate a name
// long before here, but this is a public package and a caller can pass anything.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q is not a single path segment", ErrInvalidVolumeName, name)
	}

	return nil
}

// ApplyVolume stores the volume the manifest describes, creating it when the
// name is new and replacing its labels, owner and mode when it is not,
// returning it as it now stands.
//
// The operation is idempotent: the stored volume becomes what the manifest
// says, however many times it is applied. The directory keeps its path across
// an apply, so nothing mounting the volume is redeployed, and the owner and
// mode are reapplied to it — which is also how a live volume is handed to
// another user. A manifest clearing either leaves the directory as it stands.
//
// A volume has to exist before a workload can mount it, so that a mistyped
// name is reported rather than quietly becoming a second empty volume.
func (c *Client) ApplyVolume(ctx context.Context, volume manifest.Volume) (Volume, error) {
	if err := checkName(volume.Name); err != nil {
		return Volume{}, err
	}

	resp, err := c.api.ApplyVolumeWithResponse(ctx, volume.Name, wire.FromVolume(volume))
	if err != nil {
		return Volume{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newVolume(resp.JSON200.Volume), nil
	case resp.JSON201 != nil:
		return newVolume(resp.JSON201.Volume), nil
	case resp.JSON400 != nil:
		return Volume{}, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return Volume{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Volume{}, newError(resp.StatusCode(), nil)
	}
}

// GetVolume returns the volume with the given name, returning ErrVolumeNotFound when
// no such volume exists.
func (c *Client) GetVolume(ctx context.Context, name string) (Volume, error) {
	if err := checkName(name); err != nil {
		return Volume{}, err
	}

	resp, err := c.api.GetVolumeWithResponse(ctx, name)
	if err != nil {
		return Volume{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newVolume(resp.JSON200.Volume), nil
	case resp.JSON404 != nil:
		return Volume{}, fmt.Errorf("%s: %w", resp.JSON404.Error, ErrVolumeNotFound)
	case resp.JSON500 != nil:
		return Volume{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Volume{}, newError(resp.StatusCode(), nil)
	}
}

// ListVolumes returns the volumes matching every one of the given queries, or all
// of them when none are given.
//
// A query is a "path=value" filter over the volume's labels, where the path is a
// JSON path such as "$.labels.app". A query can reach only the labels.
func (c *Client) ListVolumes(ctx context.Context, queries ...string) ([]Volume, error) {
	var params api.ListVolumesParams
	if len(queries) > 0 {
		params.Query = &queries
	}

	resp, err := c.api.ListVolumesWithResponse(ctx, &params)
	if err != nil {
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		volumes := make([]Volume, 0, len(resp.JSON200.Volumes))
		for _, volume := range resp.JSON200.Volumes {
			volumes = append(volumes, newVolume(volume))
		}

		return volumes, nil
	case resp.JSON400 != nil:
		return nil, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

// DeleteVolume removes the volume with the given name and everything stored in it.
//
// A volume a workload mounts is refused with ErrVolumeInUse, and the error names the
// workloads holding it. Pass WithForce to remove it anyway. Nothing else in takt
// removes a volume, so this is the only call that destroys stored data.
func (c *Client) DeleteVolume(ctx context.Context, name string, options ...DeleteVolumeOption) error {
	if err := checkName(name); err != nil {
		return err
	}

	config := new(deleteVolumeConfig)
	for _, option := range options {
		option(config)
	}

	params := api.DeleteVolumeParams{}
	if config.force {
		params.Force = &config.force
	}

	resp, err := c.api.DeleteVolumeWithResponse(ctx, name, &params)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON404 != nil:
		return fmt.Errorf("%s: %w", resp.JSON404.Error, ErrVolumeNotFound)
	case resp.JSON409 != nil:
		// The server's message stands on its own and already says the volume is in
		// use, naming the workloads holding it. The sentinel is joined to it rather
		// than prefixed onto it, so the reason is not stated twice.
		return fmt.Errorf("%s: %w", resp.JSON409.Error, ErrVolumeInUse)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

type (
	// The DeleteVolumeOption type configures how a volume is deleted.
	DeleteVolumeOption func(*deleteVolumeConfig)

	deleteVolumeConfig struct {
		force bool
	}
)

// WithForce removes a volume even though a workload mounts it. The workload keeps
// running and its mount stops resolving, so this is for a volume whose workloads are
// known not to need it.
func WithForce() DeleteVolumeOption {
	return func(config *deleteVolumeConfig) {
		config.force = true
	}
}

// newVolume maps a volume as the API reports it onto the client's own shape.
func newVolume(volume api.Volume) Volume {
	out := Volume{
		Name:      volume.Name,
		CreatedAt: volume.CreatedAt,
	}

	if volume.Path != nil {
		out.Path = *volume.Path
	}

	if volume.UsedBy != nil {
		out.UsedBy = *volume.UsedBy
	}

	if volume.Labels != nil {
		out.Labels = *volume.Labels
	}

	if volume.Owner != nil {
		out.Owner = *volume.Owner
	}

	if volume.Mode != nil {
		out.Mode = *volume.Mode
	}

	return out
}
