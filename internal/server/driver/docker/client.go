package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type (
	// The Client interface describes the Docker Engine surface the Driver uses.
	//
	// The Docker SDK's own client is awkward to stand in for during tests — its
	// APIClient interface has unexported methods, so it cannot be implemented
	// outside its package — so the driver consumes this narrower interface
	// instead and ships an adapter around the real thing. The adapter also
	// flattens the SDK's single-field result structs, so the driver reads the
	// value it asked for rather than unwrapping one.
	Client interface {
		// ImageList should return the images held locally that match the given options.
		ImageList(ctx context.Context, options client.ImageListOptions) ([]image.Summary, error)
		// ImagePull should pull the named image, returning the progress stream the
		// caller must drain and close for the pull to complete.
		ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (io.ReadCloser, error)
		// DistributionInspect should ask the image's registry for its manifest,
		// which carries the digest the reference currently resolves to.
		DistributionInspect(ctx context.Context, ref, encodedAuth string) (registry.DistributionInspect, error)
		// ContainerCreate should create a container from the given configuration.
		ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
		// ContainerStart should start the container with the given identifier.
		ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) error
		// ContainerStop should stop the container with the given identifier.
		ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) error
		// ContainerRemove should remove the container with the given identifier.
		ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) error
		// ContainerKill should send the named signal to the container with the given
		// identifier.
		ContainerKill(ctx context.Context, id, signal string) error
		// ContainerList should return the containers matching the given options.
		ContainerList(ctx context.Context, options client.ContainerListOptions) ([]container.Summary, error)
		// ContainerInspect should return the full state of the container with the
		// given identifier.
		ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
		// ContainerLogs should return the log stream for the container with the
		// given identifier.
		ContainerLogs(ctx context.Context, id string, options client.ContainerLogsOptions) (io.ReadCloser, error)
		// ContainerStatsOneShot should return a single reading of the resource usage
		// of the container with the given identifier, without waiting for a second
		// sample. The caller must close the body.
		ContainerStatsOneShot(ctx context.Context, id string) (io.ReadCloser, error)
		// Events should return a stream of engine events matching the given options,
		// alongside a channel carrying any error that ends the stream.
		Events(ctx context.Context, options client.EventsListOptions) (<-chan events.Message, <-chan error)
		// Close should release the client's underlying resources.
		Close() error
	}

	engineClient struct {
		inner *client.Client
	}
)

// NewClient returns a Client talking to the Docker daemon described by the
// environment, negotiating the API version so that takt works against older
// daemons rather than failing on an unsupported version.
//
// When host is empty the environment's configuration is used, which falls back
// to the local socket.
func NewClient(ctx context.Context, host string) (Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}

	inner, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to construct docker client: %w", err)
	}

	// Negotiated here rather than on the first request, so the version is
	// settled before the event stream and the first observation arrive together.
	// The outcome is the same as the lazy path's, a daemon that cannot be
	// reached included: negotiation falls back to the client's default version
	// either way, so the ping's error is not one to surface.
	_, _ = inner.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})

	return &engineClient{inner: inner}, nil
}

func (c *engineClient) ImageList(ctx context.Context, options client.ImageListOptions) ([]image.Summary, error) {
	result, err := c.inner.ImageList(ctx, options)
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func (c *engineClient) ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (io.ReadCloser, error) {
	return c.inner.ImagePull(ctx, ref, options)
}

func (c *engineClient) DistributionInspect(ctx context.Context, ref, encodedAuth string) (registry.DistributionInspect, error) {
	result, err := c.inner.DistributionInspect(ctx, ref, client.DistributionInspectOptions{EncodedRegistryAuth: encodedAuth})
	if err != nil {
		return registry.DistributionInspect{}, err
	}

	return result.DistributionInspect, nil
}

func (c *engineClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	result, err := c.inner.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
		Platform:         platform,
		Name:             name,
	})
	if err != nil {
		return container.CreateResponse{}, err
	}

	return container.CreateResponse{ID: result.ID, Warnings: result.Warnings}, nil
}

func (c *engineClient) ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) error {
	_, err := c.inner.ContainerStart(ctx, id, options)
	return err
}

func (c *engineClient) ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) error {
	_, err := c.inner.ContainerStop(ctx, id, options)
	return err
}

func (c *engineClient) ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) error {
	_, err := c.inner.ContainerRemove(ctx, id, options)
	return err
}

func (c *engineClient) ContainerKill(ctx context.Context, id, signal string) error {
	_, err := c.inner.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: signal})
	return err
}

func (c *engineClient) ContainerList(ctx context.Context, options client.ContainerListOptions) ([]container.Summary, error) {
	result, err := c.inner.ContainerList(ctx, options)
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func (c *engineClient) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := c.inner.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}

	return result.Container, nil
}

func (c *engineClient) ContainerLogs(ctx context.Context, id string, options client.ContainerLogsOptions) (io.ReadCloser, error) {
	return c.inner.ContainerLogs(ctx, id, options)
}

func (c *engineClient) ContainerStatsOneShot(ctx context.Context, id string) (io.ReadCloser, error) {
	result, err := c.inner.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return nil, err
	}

	return result.Body, nil
}

func (c *engineClient) Events(ctx context.Context, options client.EventsListOptions) (<-chan events.Message, <-chan error) {
	result := c.inner.Events(ctx, options)
	return result.Messages, result.Err
}

func (c *engineClient) Close() error {
	return c.inner.Close()
}
