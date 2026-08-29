package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type (
	// The Client interface describes the Docker Engine surface the Driver uses.
	//
	// The Docker SDK's own client is awkward to stand in for during tests — its
	// APIClient interface has unexported methods, so it cannot be implemented
	// outside its package — so the driver consumes this narrower interface
	// instead and ships an adapter around the real thing.
	Client interface {
		// ImageList should return the images held locally that match the given options.
		ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
		// ImagePull should pull the named image, returning the progress stream the
		// caller must drain and close for the pull to complete.
		ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
		// DistributionInspect should ask the image's registry for its manifest,
		// which carries the digest the reference currently resolves to.
		DistributionInspect(ctx context.Context, ref, encodedAuth string) (registry.DistributionInspect, error)
		// ContainerCreate should create a container from the given configuration.
		ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
		// ContainerStart should start the container with the given identifier.
		ContainerStart(ctx context.Context, id string, options container.StartOptions) error
		// ContainerStop should stop the container with the given identifier.
		ContainerStop(ctx context.Context, id string, options container.StopOptions) error
		// ContainerRemove should remove the container with the given identifier.
		ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error
		// ContainerKill should send the named signal to the container with the given
		// identifier.
		ContainerKill(ctx context.Context, id, signal string) error
		// ContainerList should return the containers matching the given options.
		ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
		// ContainerInspect should return the full state of the container with the
		// given identifier.
		ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
		// ContainerLogs should return the log stream for the container with the
		// given identifier.
		ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error)
		// Events should return a stream of engine events matching the given options,
		// alongside a channel carrying any error that ends the stream.
		Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
		// Close should release the client's underlying resources.
		Close() error
	}

	engineClient struct {
		inner *client.Client
	}
)

// NewClient returns a Client talking to the Docker daemon described by the
// environment, negotiating the API version so that orca works against older
// daemons rather than failing on an unsupported version.
//
// When host is empty the environment's configuration is used, which falls back
// to the local socket.
func NewClient(ctx context.Context, host string) (Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}

	inner, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to construct docker client: %w", err)
	}

	// Negotiated here rather than on the first request, because the negotiation
	// writes the version the client then reads on every call, unsynchronised.
	// Left to the first request, two callers arriving together — the event
	// stream and an observation do — race that write. The outcome is the same
	// as the lazy path's, a daemon that cannot be reached included: negotiation
	// falls back to the client's default version either way.
	inner.NegotiateAPIVersion(ctx)

	return &engineClient{inner: inner}, nil
}

func (c *engineClient) ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error) {
	return c.inner.ImageList(ctx, options)
}

func (c *engineClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return c.inner.ImagePull(ctx, ref, options)
}

func (c *engineClient) DistributionInspect(ctx context.Context, ref, encodedAuth string) (registry.DistributionInspect, error) {
	return c.inner.DistributionInspect(ctx, ref, encodedAuth)
}

func (c *engineClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	return c.inner.ContainerCreate(ctx, config, hostConfig, networkingConfig, platform, name)
}

func (c *engineClient) ContainerStart(ctx context.Context, id string, options container.StartOptions) error {
	return c.inner.ContainerStart(ctx, id, options)
}

func (c *engineClient) ContainerStop(ctx context.Context, id string, options container.StopOptions) error {
	return c.inner.ContainerStop(ctx, id, options)
}

func (c *engineClient) ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error {
	return c.inner.ContainerRemove(ctx, id, options)
}

func (c *engineClient) ContainerKill(ctx context.Context, id, signal string) error {
	return c.inner.ContainerKill(ctx, id, signal)
}

func (c *engineClient) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	return c.inner.ContainerList(ctx, options)
}

func (c *engineClient) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	return c.inner.ContainerInspect(ctx, id)
}

func (c *engineClient) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	return c.inner.ContainerLogs(ctx, id, options)
}

func (c *engineClient) Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error) {
	return c.inner.Events(ctx, options)
}

func (c *engineClient) Close() error {
	return c.inner.Close()
}
