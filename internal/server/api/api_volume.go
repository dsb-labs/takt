package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/service"
)

type (
	// The VolumeService interface describes the volume operations the API exposes.
	VolumeService interface {
		// Create should create a volume with the given name, along with the
		// directory backing it.
		Create(ctx context.Context, name string) (service.Volume, error)
		// Get should return the volume with the given name.
		Get(ctx context.Context, name string) (service.Volume, error)
		// List should return every volume the server holds.
		List(ctx context.Context) ([]service.Volume, error)
		// Delete should remove the volume with the given name and everything stored
		// in it, refusing a volume a workload mounts unless force is set.
		Delete(ctx context.Context, name string, force bool) error
	}

	// The VolumeAPI type exposes HTTP endpoints for managing volumes.
	VolumeAPI struct {
		logger  *slog.Logger
		volumes VolumeService
	}

	// The VolumeAPIConfig type contains fields used to construct a VolumeAPI.
	VolumeAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the volume operations.
		Volumes VolumeService
	}
)

// NewVolumeAPI returns a new instance of the VolumeAPI type.
func NewVolumeAPI(config VolumeAPIConfig) *VolumeAPI {
	return &VolumeAPI{
		logger:  config.Logger.With("component", "api"),
		volumes: config.Volumes,
	}
}

// internalError logs why a request failed and returns the message the client is told
// instead.
func (a *VolumeAPI) internalError(operation string, err error) string {
	a.logger.With("error", err, "operation", operation).Error("failed to serve request")

	return "failed to " + operation
}

// CreateVolume creates a volume and the directory backing it.
func (a *VolumeAPI) CreateVolume(ctx context.Context, request api.CreateVolumeRequestObject) (api.CreateVolumeResponseObject, error) {
	if request.Body == nil {
		return api.CreateVolume400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: "request body is required"},
		}, nil
	}

	volume, err := a.volumes.Create(ctx, request.Body.Name)
	switch {
	case errors.Is(err, service.ErrInvalidVolume):
		return api.CreateVolume400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case errors.Is(err, service.ErrVolumeExists):
		// A volume holds data, so creating one that already exists is reported
		// rather than treated as success. The caller may well have meant a name they
		// have not used yet.
		return api.CreateVolume409JSONResponse{
			Error: fmt.Sprintf("volume %q already exists", request.Body.Name),
		}, nil
	case err != nil:
		return api.CreateVolume500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("create volume", err),
			},
		}, nil
	}

	return api.CreateVolume201JSONResponse{Volume: newVolume(volume)}, nil
}

// GetVolume returns the volume with the given name.
func (a *VolumeAPI) GetVolume(ctx context.Context, request api.GetVolumeRequestObject) (api.GetVolumeResponseObject, error) {
	volume, err := a.volumes.Get(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrVolumeNotFound):
		return api.GetVolume404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("volume %q does not exist", request.Name),
			},
		}, nil
	case err != nil:
		return api.GetVolume500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("get volume", err),
			},
		}, nil
	}

	return api.GetVolume200JSONResponse{Volume: newVolume(volume)}, nil
}

// ListVolumes returns every volume the server holds.
func (a *VolumeAPI) ListVolumes(ctx context.Context, _ api.ListVolumesRequestObject) (api.ListVolumesResponseObject, error) {
	volumes, err := a.volumes.List(ctx)
	if err != nil {
		return api.ListVolumes500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("list volumes", err),
			},
		}, nil
	}

	response := api.ListVolumes200JSONResponse{Volumes: make([]api.Volume, 0, len(volumes))}
	for _, volume := range volumes {
		response.Volumes = append(response.Volumes, newVolume(volume))
	}

	return response, nil
}

// DeleteVolume removes the volume with the given name and everything stored in it.
//
// The response is 200 rather than 202, unlike deleting a workload: there is nothing
// running to wind down, only a directory to remove, so the volume is gone when the
// request returns.
func (a *VolumeAPI) DeleteVolume(ctx context.Context, request api.DeleteVolumeRequestObject) (api.DeleteVolumeResponseObject, error) {
	force := request.Params.Force != nil && *request.Params.Force

	err := a.volumes.Delete(ctx, request.Name, force)
	switch {
	case errors.Is(err, service.ErrVolumeNotFound):
		return api.DeleteVolume404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("volume %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrVolumeInUse):
		// The workloads holding it are named, because the caller's next question is
		// which ones, and answering it costs nothing here.
		return api.DeleteVolume409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		return api.DeleteVolume500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("delete volume", err),
			},
		}, nil
	}

	return api.DeleteVolume200JSONResponse{}, nil
}

// newVolume converts a volume as the service reports it into its wire representation.
func newVolume(volume service.Volume) api.Volume {
	wire := api.Volume{
		Name:      volume.Name,
		CreatedAt: volume.CreatedAt,
		Path:      &volume.Path,
	}

	// Absent rather than an empty array when nothing mounts it, so that "used by
	// nothing" and "not reported" are not the same value on the wire.
	if len(volume.UsedBy) > 0 {
		wire.UsedBy = &volume.UsedBy
	}

	return wire
}
