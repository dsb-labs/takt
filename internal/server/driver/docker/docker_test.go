package docker_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/driver/docker"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestDriver_Start(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Workload   driver.Workload
		SetupMocks func(*MockClient)
		Assert     func(*testing.T, string)
		ExpectErr  error
	}{
		{
			Name: "publishes a port on the protocol it names",
			Workload: workload("example", 1, "hash", containerSpec("example/example:latest", nil),
				[]driver.Port{{Container: 53, Host: 20000, Protocol: "udp"}}, nil),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						_, exposed := config.ExposedPorts["53/udp"]
						return exposed
					}),
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						// Publishing 53/tcp instead would leave the workload
						// unreachable at the address takt reports for it.
						bindings := host.PortBindings["53/udp"]

						return len(bindings) == 1 && bindings[0].HostPort == "20000" &&
							len(host.PortBindings) == 1
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "starts a container with takt's ownership labels",
			Workload: withEnv(workload("example", 2, "hash-two", containerSpec("example/example:latest", nil), ports(8080, 4141), map[string]string{"some-key": "some-value"}), map[string]string{"EXAMPLE": "EXAMPLE"}),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.Image == "example/example:latest" &&
							config.Labels[docker.LabelWorkload] == "example" &&
							config.Labels[docker.LabelSpecHash] == "hash-two" &&
							config.Labels[docker.LabelVersion] == "2" &&
							config.Labels["some-key"] == "some-value" &&
							len(config.Env) == 1 && config.Env[0] == "EXAMPLE=EXAMPLE"
					}),
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						bindings := host.PortBindings["8080/tcp"]

						// A driver told nothing about where to publish uses loopback,
						// so forgetting to say never exposes a workload to the network.
						return len(bindings) == 1 && bindings[0].HostPort == "4141" &&
							bindings[0].HostIP == "127.0.0.1"
					}),
					mock.Anything, mock.Anything, "takt-example-2-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name: "mounts the workload's volumes",
			Workload: withVolumes(
				workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil),
				driver.Volume{Name: "example-data", Host: "/var/lib/takt/volumes/abc", Target: "/var/lib/example"},
			),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						// A bind of the directory takt owns, rather than a docker
						// named volume: the exec runtime needs a real path anyway, so
						// one mechanism serves both.
						return len(host.Mounts) == 1 &&
							host.Mounts[0].Type == mount.TypeBind &&
							host.Mounts[0].Source == "/var/lib/takt/volumes/abc" &&
							host.Mounts[0].Target == "/var/lib/example"
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// The bind is what enforces a read-only mount, so the flag has to
			// survive the trip into the host configuration.
			Name: "mounts a read-only bind when the mount asks",
			Workload: withVolumes(
				workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil),
				driver.Volume{Name: "example-data", Host: "/var/lib/takt/volumes/abc", Target: "/var/lib/example", ReadOnly: true},
			),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						return len(host.Mounts) == 1 && host.Mounts[0].ReadOnly
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "runs the command the workload names",
			Workload: workload("example", 1, "hash-one", containerSpec("example/example:latest", []string{"sh", "-c", "exit 0"}), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return len(config.Cmd) == 3 && config.Cmd[0] == "sh" && config.Cmd[2] == "exit 0"
					}),
					mock.Anything, mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// Nil rather than empty, so the image keeps the command it declares. An
			// empty slice would replace it with nothing, and the container would have
			// nothing to run.
			Name:     "leaves the image's own command alone when the workload names none",
			Workload: workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.Cmd == nil
					}),
					mock.Anything, mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// Ownership is expressed entirely through these labels: Observe reads
			// them to decide which containers are takt's and which workload each
			// belongs to. A manifest that could set them would be able to disown a
			// container or claim another workload's, so takt's own must win.
			Name: "refuses to let a manifest overwrite the ownership labels",
			Workload: workload("example", 2, "hash-two", containerSpec("example/example:latest", nil), nil, map[string]string{
				docker.LabelWorkload: "someone-elses-workload",
				docker.LabelSpecHash: "forged-hash",
				docker.LabelVersion:  "999",
			}),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.Labels[docker.LabelWorkload] == "example" &&
							config.Labels[docker.LabelSpecHash] == "hash-two" &&
							config.Labels[docker.LabelVersion] == "2"
					}),
					mock.Anything, mock.Anything, mock.Anything, "takt-example-2-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// A pull no longer happens inside Start: the image being absent
			// reports the background pull instead, which is covered by the
			// dedicated pull tests below.
			Name:     "reports a pull in progress when the image isn't present locally",
			Workload: workload("example", 0, "", containerSpec("example/example:latest", nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return(nil, nil).Once()
				c.EXPECT().ImagePull(mock.Anything, "example/example:latest", mock.Anything).
					Return(io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), nil).Maybe()
			},
			ExpectErr: driver.ErrImagePulling,
		},
		{
			// A never policy that fell through to a pull would be indistinguishable
			// from missing, so an absent image must fail: the strict mocks fail this
			// case if ImagePull is called.
			Name:     "refuses to start under the never policy when the image is absent",
			Workload: workload("example", 0, "", pulledSpec("example/example:latest", manifest.PullNever), nil, nil),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ImageList(mock.Anything, mock.Anything).Return(nil, nil).Once()
			},
		},
		{
			Name:     "starts under the never policy when the image is present",
			Workload: workload("example", 0, "", pulledSpec("example/example:latest", manifest.PullNever), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()
				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()
				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "removes the container when it cannot be started",
			Workload: workload("example", 0, "", containerSpec("example/example:latest", nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()
				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()
				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).
					Return(errors.New("no such image")).Once()
				c.EXPECT().ContainerRemove(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			ExpectErr: nil,
		},
		{
			Name: "applies the workload's resource limits",
			Workload: withResources(
				workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil),
				manifest.Resources{Memory: "512m", CPU: 0.5, Pids: 100},
			),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						// Swap is pinned to the memory limit so the limit is hard: left
						// alone, docker lets the container swap up to twice the limit.
						return host.Memory == 512*1024*1024 &&
							host.MemorySwap == 512*1024*1024 &&
							host.NanoCPUs == 500_000_000 &&
							host.PidsLimit != nil && *host.PidsLimit == 100
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// The option is applied by the driver rather than carried in the
			// specification, so it holds for every container without moving the hash
			// of a workload that exists already.
			Name:     "denies new privileges to a workload that asked for nothing",
			Workload: workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						return len(host.SecurityOpt) == 1 && host.SecurityOpt[0] == "no-new-privileges" &&
							host.Memory == 0 && host.MemorySwap == 0 &&
							host.NanoCPUs == 0 && host.PidsLimit == nil &&
							!host.ReadonlyRootfs && host.CapAdd == nil && host.CapDrop == nil
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "applies the container's hardening fields",
			Workload: workload("example", 1, "hash-one", hardenedSpec("example/example:latest"), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.User == "65532:65532"
					}),
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						return host.ReadonlyRootfs &&
							slices.Equal(host.CapAdd, []string{"NET_ADMIN"}) &&
							slices.Equal(host.CapDrop, []string{"ALL"})
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// A read-only root filesystem applies to the image's own layers. A volume
			// and a mounted value are bind mounts with rules of their own, so they
			// must not inherit the flag or a workload could not write its data.
			Name: "leaves bind mounts writable under a read-only root filesystem",
			Workload: withVolumes(
				workload("example", 1, "hash-one", hardenedSpec("example/example:latest"), nil, nil),
				driver.Volume{Name: "example-data", Host: "/var/lib/takt/volumes/abc", Target: "/var/lib/example"},
			),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						return host.ReadonlyRootfs &&
							len(host.Mounts) == 1 && !host.Mounts[0].ReadOnly
					}),
					mock.Anything, mock.Anything, "takt-example-1-0-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			client := NewMockClient(t)
			tc.SetupMocks(client)

			d := testDriver(t, client)

			id, err := d.Start(t.Context(), tc.Workload)
			switch {
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			case tc.Assert == nil:
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			tc.Assert(t, id)
		})
	}
}

func TestDriver_Start_PullsInBackground(t *testing.T) {
	t.Parallel()

	// A pull runs behind the reconcile pass rather than inside it. The first
	// start reports the pull in progress, and a later one finds it finished
	// and starts the container.
	client := NewMockClient(t)

	client.EXPECT().ImageList(mock.Anything, mock.Anything).Return(nil, nil)
	client.EXPECT().ImagePull(mock.Anything, "example/example:latest", mock.Anything).
		Return(io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), nil).Once()
	client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()
	client.EXPECT().ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()
	client.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

	d := testDriver(t, client)
	w := workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil)

	_, err := d.Start(t.Context(), w)
	require.ErrorIs(t, err, driver.ErrImagePulling)

	var id string

	require.Eventually(t, func() bool {
		got, err := d.Start(t.Context(), w)
		if err != nil {
			return false
		}

		id = got

		return true
	}, 5*time.Second, 10*time.Millisecond)

	assert.Equal(t, "container-one", id)
}

func TestDriver_Start_AlwaysPolicyPullsInBackground(t *testing.T) {
	t.Parallel()

	// An always policy exists to fetch the tag's current content, so what is
	// held locally is not even asked about: the strict mocks fail this test
	// if ImageList is called. The pull still runs in the background.
	client := NewMockClient(t)

	client.EXPECT().ImagePull(mock.Anything, "example/example:latest", mock.Anything).
		Return(io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), nil).Once()
	client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()
	client.EXPECT().ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()
	client.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

	d := testDriver(t, client)
	w := workload("example", 0, "", pulledSpec("example/example:latest", manifest.PullAlways), nil, nil)

	_, err := d.Start(t.Context(), w)
	require.ErrorIs(t, err, driver.ErrImagePulling)

	require.Eventually(t, func() bool {
		_, err := d.Start(t.Context(), w)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}

func TestDriver_Start_ReportsPullFailure(t *testing.T) {
	t.Parallel()

	// A failed pull is reported by the start that finds it finished, so the
	// failure reaches the workload's state rather than being swallowed by the
	// background.
	client := NewMockClient(t)

	client.EXPECT().ImageList(mock.Anything, mock.Anything).Return(nil, nil)
	client.EXPECT().ImagePull(mock.Anything, "example/example:latest", mock.Anything).
		Return(nil, errors.New("registry unreachable")).Once()

	d := testDriver(t, client)
	w := workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil)

	_, err := d.Start(t.Context(), w)
	require.ErrorIs(t, err, driver.ErrImagePulling)

	require.Eventually(t, func() bool {
		_, err := d.Start(t.Context(), w)

		return err != nil && !errors.Is(err, driver.ErrImagePulling)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestDriver_Start_PublishAddress(t *testing.T) {
	t.Parallel()

	// Which interfaces a workload is reachable on is the operator's decision, so the
	// address reaches docker as given rather than being narrowed or widened here.
	tt := []struct {
		Name     string
		Bind     string
		ExpectIP string
	}{
		{
			Name:     "publishes on the configured address",
			Bind:     "10.0.0.5",
			ExpectIP: "10.0.0.5",
		},
		{
			Name:     "publishes on every interface when asked",
			Bind:     "0.0.0.0",
			ExpectIP: "0.0.0.0",
		},
		{
			// Docker would read an empty address as every interface, so the driver
			// names loopback rather than passing one through.
			Name:     "falls back to loopback when told nothing",
			Bind:     "",
			ExpectIP: "127.0.0.1",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			client := NewMockClient(t)

			// Read to number the attempt, so a replacement cannot collide with a
			// container being kept for its output.
			client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

			client.EXPECT().ImageList(mock.Anything, mock.Anything).
				Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

			client.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
				mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
					bindings := host.PortBindings["8080/tcp"]
					return len(bindings) == 1 && bindings[0].HostIP == tc.ExpectIP
				}),
				mock.Anything, mock.Anything, mock.Anything,
			).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

			client.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

			d := docker.New(docker.Config{
				Logger:     newTestLogger(t),
				Client:     client,
				Bind:       tc.Bind,
				ConfigFile: filepath.Join(t.TempDir(), "config.json"),
			})

			_, err := d.Start(t.Context(), workload("example", 1, "hash", containerSpec("example/example:latest", nil), ports(8080, 4141), nil))
			require.NoError(t, err)
		})
	}
}

func TestDriver_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stops every container and keeps the newest for its output", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", Labels: map[string]string{docker.LabelAttempt: "1"}},
			{ID: "container-two", Labels: map[string]string{docker.LabelAttempt: "2"}},
		}, nil).Once()

		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		// The newest is kept and the one it replaced goes. Docker holds the logs of a
		// container that exists, so keeping the container is what makes the output of
		// the attempt that just failed readable afterwards.
		client.EXPECT().ContainerRemove(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("keeps exactly one container however many attempts there have been", func(t *testing.T) {
		client := NewMockClient(t)

		// Unbounded retention is a disk leak on a workload crashing in a loop, so
		// everything but the newest goes on every stop rather than accumulating.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", Labels: map[string]string{docker.LabelAttempt: "1"}},
			{ID: "container-two", Labels: map[string]string{docker.LabelAttempt: "2"}},
			{ID: "container-three", Labels: map[string]string{docker.LabelAttempt: "3"}},
		}, nil).Once()

		for _, id := range []string{"container-one", "container-two", "container-three"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerRemove(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		d := testDriver(t, client)

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("forces removal so it doesn't race the container shutting down", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", Labels: map[string]string{docker.LabelAttempt: "1"}},
			{ID: "container-two", Labels: map[string]string{docker.LabelAttempt: "2"}},
		}, nil).Once()

		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		// ContainerStop returns before the container has necessarily stopped, so an
		// unforced remove fails with "container is running" and leaves the
		// container behind for every later pass to trip over.
		client.EXPECT().ContainerRemove(mock.Anything, "container-one",
			mock.MatchedBy(func(options dockercontainer.RemoveOptions) bool {
				return options.Force
			})).Return(nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("keeps one container per instance", func(t *testing.T) {
		client := NewMockClient(t)

		// Instances run beside each other, so each keeps a corpse of its own
		// rather than the workload keeping one across all of them.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero-old", Labels: map[string]string{docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "zero-new", Labels: map[string]string{docker.LabelInstance: "0", docker.LabelAttempt: "2"}},
			{ID: "one-only", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
		}, nil).Once()

		for _, id := range []string{"zero-old", "zero-new", "one-only"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		client.EXPECT().ContainerRemove(mock.Anything, "zero-old", mock.Anything).Return(nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("keeps stopping past a container that fails", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero-only", Labels: map[string]string{docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "one-only", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
		}, nil).Once()

		// The first instance's stop fails, and the second is still stopped: a
		// deadline under a saturated daemon must not throw the remaining
		// instances back to the next pass unattempted. Nothing is removed —
		// each instance's only container is its retained corpse.
		client.EXPECT().ContainerStop(mock.Anything, "zero-only", mock.Anything).
			Return(errors.New("context deadline exceeded")).Once()
		client.EXPECT().ContainerStop(mock.Anything, "one-only", mock.Anything).Return(nil).Once()

		d := testDriver(t, client)

		assert.Error(t, d.Stop(t.Context(), "", "example"))
	})
}

func TestDriver_StopInstance(t *testing.T) {
	t.Parallel()

	t.Run("stops one instance and leaves the others running", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero-only", Labels: map[string]string{docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "one-old", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
			{ID: "one-new", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "2"}},
		}, nil).Once()

		// Only the selected instance's containers are touched, and it keeps its
		// newest exactly as a workload-wide stop would.
		for _, id := range []string{"one-old", "one-new"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		client.EXPECT().ContainerRemove(mock.Anything, "one-old", mock.Anything).Return(nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.StopInstance(t.Context(), "", "example", 1))
	})
}

func TestDriver_Discard(t *testing.T) {
	t.Parallel()

	t.Run("removes everything including what a stop retained", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", Labels: map[string]string{docker.LabelAttempt: "1"}},
			{ID: "container-two", Labels: map[string]string{docker.LabelAttempt: "2"}},
		}, nil).Once()

		// Nothing is kept. A retained container exists so an operator can read why the
		// previous attempt failed, and a workload nobody asked for has no such reader —
		// one left behind is a container the orphan sweep finds on every pass forever.
		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
			client.EXPECT().ContainerRemove(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		d := testDriver(t, client)

		require.NoError(t, d.Discard(t.Context(), "", "example"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Discard(t.Context(), "", "example"))
	})

	t.Run("keeps discarding past a container that fails", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", Labels: map[string]string{docker.LabelAttempt: "1"}},
			{ID: "container-two", Labels: map[string]string{docker.LabelAttempt: "2"}},
		}, nil).Once()

		// The first container's stop fails and the second is still discarded, so
		// the progress a pass makes under a saturated daemon is kept rather than
		// redone from the start on the next one.
		client.EXPECT().ContainerStop(mock.Anything, "container-one", mock.Anything).
			Return(errors.New("context deadline exceeded")).Once()
		client.EXPECT().ContainerStop(mock.Anything, "container-two", mock.Anything).Return(nil).Once()
		client.EXPECT().ContainerRemove(mock.Anything, "container-two", mock.Anything).Return(nil).Once()

		d := testDriver(t, client)

		assert.Error(t, d.Discard(t.Context(), "", "example"))
	})

	t.Run("removes one instance including what a stop retained for it", func(t *testing.T) {
		client := NewMockClient(t)

		// A removed instance is not being replaced, so nothing will read the
		// output a retained container keeps for it.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero-only", Labels: map[string]string{docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "one-old", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
			{ID: "one-new", Labels: map[string]string{docker.LabelInstance: "1", docker.LabelAttempt: "2"}},
		}, nil).Once()

		for _, id := range []string{"one-old", "one-new"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
			client.EXPECT().ContainerRemove(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		d := testDriver(t, client)

		require.NoError(t, d.DiscardInstance(t.Context(), "", "example", 1))
	})
}

func TestDriver_Signal(t *testing.T) {
	t.Parallel()

	t.Run("signals every running container it owns for the workload", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", State: dockercontainer.StateRunning},
			{ID: "container-two", State: dockercontainer.StateRunning},
		}, nil).Once()

		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerKill(mock.Anything, id, "SIGHUP").Return(nil).Once()
		}

		d := testDriver(t, client)

		require.NoError(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})

	t.Run("leaves a container that is not running alone", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", State: dockercontainer.StateExited},
			{ID: "container-two", State: dockercontainer.StateRunning},
		}, nil).Once()

		// Docker refuses to signal a container that has stopped, and one that has
		// stopped has nothing to reload.
		client.EXPECT().ContainerKill(mock.Anything, "container-two", "SIGHUP").Return(nil).Once()

		d := testDriver(t, client)

		require.NoError(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := testDriver(t, client)

		// The reconciler asks the driver that runs the workload, and there is nothing
		// here to reload.
		require.NoError(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})

	t.Run("reports a signal the daemon refused", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one", State: dockercontainer.StateRunning},
		}, nil).Once()

		client.EXPECT().ContainerKill(mock.Anything, "container-one", "SIGHUP").
			Return(errors.New("daemon said no")).Once()

		d := testDriver(t, client)

		// The file has already been rewritten, so a workload that was not told is
		// something the caller has to hear about.
		assert.Error(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})
}

// TestDriver_ObserveWorkload covers reading one workload rather than the whole host.
// The daemon does the filtering, which is what makes a single-workload read cost the
// same whether the host runs one container or two hundred.
func TestDriver_ObserveWorkload(t *testing.T) {
	t.Parallel()

	t.Run("asks the daemon for one workload's containers", func(t *testing.T) {
		client := NewMockClient(t)

		var asked dockercontainer.ListOptions

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, options dockercontainer.ListOptions) ([]dockercontainer.Summary, error) {
				asked = options

				return []dockercontainer.Summary{
					{
						ID:    "container-one",
						State: dockercontainer.StateRunning,
						Labels: map[string]string{
							docker.LabelWorkload: "example",
							docker.LabelSpecHash: "hash-one",
						},
					},
				}, nil
			}).Once()

		d := testDriver(t, client)

		instances, err := d.ObserveWorkload(t.Context(), "cvhs0dq0kqj4c9r8m1a0", "example")
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, "example", instances[0].Workload)

		// The filter is the point. Listing everything and discarding the rest is what
		// this exists to stop, so the label has to carry the name.
		assert.Contains(t, asked.Filters.Get("label"), docker.LabelWorkload+"=example")
	})

	// Retention is decided within whatever was listed. supersededBy groups by
	// workload before choosing, so narrowing the listing to one workload has to reach
	// the same answer for it that a listing of the host would.
	t.Run("reports the superseded container of the workload as retained", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:      "older",
				State:   dockercontainer.StateRunning,
				Created: 1000,
				Labels: map[string]string{
					docker.LabelWorkload: "example",
					docker.LabelVersion:  "1",
				},
			},
			{
				ID:      "newer",
				State:   dockercontainer.StateRunning,
				Created: 2000,
				Labels: map[string]string{
					docker.LabelWorkload: "example",
					docker.LabelVersion:  "2",
				},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.ObserveWorkload(t.Context(), "cvhs0dq0kqj4c9r8m1a0", "example")
		require.NoError(t, err)
		require.Len(t, instances, 2)

		retained := map[string]bool{}
		for _, instance := range instances {
			retained[instance.ID] = instance.Retained
		}

		assert.True(t, retained["older"], "the superseded container is not retained")
		assert.False(t, retained["newer"], "the current container is retained")
	})
}

func TestDriver_Observe(t *testing.T) {
	t.Parallel()

	t.Run("reports running containers without inspecting them", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:    "container-one",
				State: dockercontainer.StateRunning,
				Labels: map[string]string{
					docker.LabelWorkload: "example",
					docker.LabelSpecHash: "hash-one",
					docker.LabelVersion:  "3",
				},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.Equal(t, "container-one", instances[0].ID)
		assert.Equal(t, "example", instances[0].Workload)
		assert.Equal(t, "hash-one", instances[0].SpecHash)
		assert.Equal(t, 3, instances[0].Version)
		assert.Equal(t, driver.StateRunning, instances[0].State)
	})

	t.Run("inspects stopped containers for their exit code", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:    "container-one",
				State: dockercontainer.StateExited,
				Labels: map[string]string{
					docker.LabelWorkload: "example",
					docker.LabelSpecHash: "hash-one",
				},
			},
		}, nil).Once()

		client.EXPECT().ContainerInspect(mock.Anything, "container-one").Return(dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				State: &dockercontainer.State{
					Status:    dockercontainer.StateExited,
					ExitCode:  137,
					StartedAt: "2026-08-18T12:00:00Z",
				},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		// A non-zero exit overrides the exited status: the container did not
		// finish cleanly, however docker labels it.
		assert.Equal(t, driver.StateFailed, instances[0].State)
		assert.Equal(t, 137, instances[0].ExitCode)
		assert.False(t, instances[0].StartedAt.IsZero())
	})

	t.Run("keeps a clean exit as exited", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "container-one",
				State:  dockercontainer.StateExited,
				Labels: map[string]string{docker.LabelWorkload: "example"},
			},
		}, nil).Once()

		client.EXPECT().ContainerInspect(mock.Anything, "container-one").Return(dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				State: &dockercontainer.State{Status: dockercontainer.StateExited, ExitCode: 0},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.Equal(t, driver.StateExited, instances[0].State)
		assert.Zero(t, instances[0].ExitCode)
	})

	t.Run("reports a container being removed as terminating", func(t *testing.T) {
		client := NewMockClient(t)

		// Removal is takt's own doing — a replacement or a delete in progress — so
		// it must not be reported as a failure, and must not be inspected for an
		// exit code it hasn't produced yet.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "container-one",
				State:  dockercontainer.StateRemoving,
				Labels: map[string]string{docker.LabelWorkload: "example"},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.Equal(t, driver.StateTerminating, instances[0].State)
		assert.Zero(t, instances[0].ExitCode)
	})

	t.Run("reports a container docker could not remove as failed", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "container-one",
				State:  dockercontainer.StateDead,
				Labels: map[string]string{docker.LabelWorkload: "example"},
			},
		}, nil).Once()

		client.EXPECT().ContainerInspect(mock.Anything, "container-one").Return(dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				State: &dockercontainer.State{Status: dockercontainer.StateDead},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		// Nothing takt does will move a dead container on. Docker retries it when
		// the daemon restarts, so it stays a failure rather than terminating.
		assert.Equal(t, driver.StateFailed, instances[0].State)
	})

	t.Run("reports nothing when it owns no containers", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		assert.Empty(t, instances)
	})

	t.Run("marks a container something has replaced as retained", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "retained",
				State:  dockercontainer.StateExited,
				Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"},
			},
			{
				ID:     "current",
				State:  dockercontainer.StateRunning,
				Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "2"},
			},
		}, nil).Once()

		client.EXPECT().ContainerInspect(mock.Anything, "retained").Return(dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				State: &dockercontainer.State{Status: dockercontainer.StateExited, ExitCode: 1},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 2)

		byID := make(map[string]driver.Instance, len(instances))
		for _, instance := range instances {
			byID[instance.ID] = instance
		}

		// Reported rather than hidden, so the orphan sweep can still find it, and
		// flagged so that nothing deciding what to run mistakes it for an instance.
		assert.True(t, byID["retained"].Retained)
		assert.False(t, byID["current"].Retained)

		// Its real state, so nothing has to invent one for it.
		assert.Equal(t, driver.StateFailed, byID["retained"].State)
	})

	t.Run("marks nothing retained for a workload that has run once", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "current",
				State:  dockercontainer.StateRunning,
				Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.False(t, instances[0].Retained)
	})

	t.Run("keeps each workload's attempts to itself", func(t *testing.T) {
		client := NewMockClient(t)

		// Attempts are numbered per workload, so a workload on its first attempt must
		// not read as superseded by another workload's second.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "first-of-one",
				State:  dockercontainer.StateRunning,
				Labels: map[string]string{docker.LabelWorkload: "one", docker.LabelAttempt: "1"},
			},
			{
				ID:     "second-of-two",
				State:  dockercontainer.StateRunning,
				Labels: map[string]string{docker.LabelWorkload: "two", docker.LabelAttempt: "2"},
			},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 2)

		for _, instance := range instances {
			assert.Falsef(t, instance.Retained, "%s read as replaced by another workload's container", instance.ID)
		}
	})
}

func TestDriver_Observe_Instances(t *testing.T) {
	t.Parallel()

	t.Run("reports each instance's newest as current", func(t *testing.T) {
		client := NewMockClient(t)

		// Two live instances are two currents, not a current and a corpse.
		// Retention is decided within an instance, so only the attempt another
		// container of the same instance replaced reads as retained.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "one-old", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
			{ID: "one-new", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelInstance: "1", docker.LabelAttempt: "2"}},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 3)

		byID := make(map[string]driver.Instance, len(instances))
		for _, instance := range instances {
			byID[instance.ID] = instance
		}

		assert.Equal(t, 0, byID["zero"].Index)
		assert.False(t, byID["zero"].Retained)
		assert.Equal(t, 1, byID["one-new"].Index)
		assert.False(t, byID["one-new"].Retained)
		assert.True(t, byID["one-old"].Retained)
	})

	t.Run("reads a container without the label as the first instance", func(t *testing.T) {
		client := NewMockClient(t)

		// What every container was before the label existed, so a server upgrade
		// adopts what is already running rather than replacing it.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "legacy", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"}},
		}, nil).Once()

		d := testDriver(t, client)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, 0, instances[0].Index)
		assert.False(t, instances[0].Retained)
	})
}

func TestDriver_Logs(t *testing.T) {
	t.Parallel()

	t.Run("demultiplexes the container's log stream", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one"},
		}, nil).Once()

		client.EXPECT().ContainerLogs(mock.Anything, "container-one", mock.MatchedBy(func(options dockercontainer.LogsOptions) bool {
			return options.ShowStdout && options.ShowStderr && options.Tail == "20"
		})).Return(io.NopCloser(strings.NewReader(multiplexed("hello world\n"))), nil).Once()

		d := testDriver(t, client)

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20}))
		assert.Equal(t, "hello world\n", out.String())
	})

	t.Run("reads the current attempt and not the one it replaced", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "retained", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"}},
			{ID: "current", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "2"}},
		}, nil).Once()

		// Only the current one. Writing both would return two runs spliced together
		// with nothing marking the boundary.
		client.EXPECT().ContainerLogs(mock.Anything, "current", mock.Anything).
			Return(io.NopCloser(strings.NewReader(multiplexed("this attempt\n"))), nil).Once()

		d := testDriver(t, client)

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20}))
		assert.Equal(t, "this attempt\n", out.String())
	})

	t.Run("reads the attempt that was replaced when asked for the previous one", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "retained", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"}},
			{ID: "current", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "2"}},
		}, nil).Once()

		// The whole point of retaining it: for a workload crashing in a loop this is
		// the attempt that failed, where the current one has not failed yet.
		client.EXPECT().ContainerLogs(mock.Anything, "retained", mock.Anything).
			Return(io.NopCloser(strings.NewReader(multiplexed("why it died\n"))), nil).Once()

		d := testDriver(t, client)

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Previous: true}))
		assert.Equal(t, "why it died\n", out.String())
	})

	t.Run("writes nothing when the workload has only ever run once", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "current", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"}},
		}, nil).Once()

		d := testDriver(t, client)

		// There is no earlier attempt, and saying so by writing nothing is better than
		// falling back to the current one and labelling it as the previous.
		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Previous: true}))
		assert.Empty(t, out.String())
	})

	t.Run("asks the daemon to follow and to skip what came before", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one"},
		}, nil).Once()

		since := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)

		// The daemon timestamps every line it holds, so the filtering happens there
		// rather than over a stream the driver would have to parse.
		client.EXPECT().ContainerLogs(mock.Anything, "container-one", mock.MatchedBy(func(options dockercontainer.LogsOptions) bool {
			return options.Follow && options.Since == since.Format(time.RFC3339Nano)
		})).Return(io.NopCloser(strings.NewReader(multiplexed("still going\n"))), nil).Once()

		d := testDriver(t, client)

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Follow: true, Since: since}))
		assert.Equal(t, "still going\n", out.String())
	})

	t.Run("reads only the selected instance", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "zero", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelInstance: "0", docker.LabelAttempt: "1"}},
			{ID: "one", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelInstance: "1", docker.LabelAttempt: "1"}},
		}, nil).Once()

		client.EXPECT().ContainerLogs(mock.Anything, "one", mock.Anything).
			Return(io.NopCloser(strings.NewReader(multiplexed("instance one\n"))), nil).Once()

		d := testDriver(t, client)

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Instance: new(1)}))
		assert.Equal(t, "instance one\n", out.String())
	})

	t.Run("ends a followed read without error when the caller goes away", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one"},
		}, nil).Once()

		client.EXPECT().ContainerLogs(mock.Anything, "container-one", mock.Anything).
			Return(io.NopCloser(iotest.ErrReader(errors.New("connection closed"))), nil).Once()

		d := testDriver(t, client)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		// A caller who stopped listening is how a follow ends. Whatever the transport
		// noticed first is the end of the read, not a failure to report back to
		// somebody who is no longer there.
		var out strings.Builder
		require.NoError(t, d.Logs(ctx, &out, "example", driver.LogOptions{Tail: 20, Follow: true}))
	})
}

func TestDriver_Start_NumbersEachAttempt(t *testing.T) {
	t.Parallel()

	// A restart at an unchanged version reuses the version, so the attempt is what
	// stops a replacement colliding with the container being kept for its output.
	client := NewMockClient(t)

	client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
		{ID: "retained", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "2"}},
	}, nil).Once()

	client.EXPECT().ImageList(mock.Anything, mock.Anything).
		Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

	client.EXPECT().ContainerCreate(mock.Anything,
		mock.MatchedBy(func(config *dockercontainer.Config) bool {
			return config.Labels[docker.LabelAttempt] == "3"
		}),
		mock.Anything, mock.Anything, mock.Anything, "takt-example-1-0-3",
	).Return(dockercontainer.CreateResponse{ID: "container-three"}, nil).Once()

	client.EXPECT().ContainerStart(mock.Anything, "container-three", mock.Anything).Return(nil).Once()

	d := testDriver(t, client)

	_, err := d.Start(t.Context(), workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil))
	require.NoError(t, err)
}

func TestDriver_Digest(t *testing.T) {
	t.Parallel()

	t.Run("returns the digest the registry reports", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().DistributionInspect(mock.Anything, "example/example:latest", "").
			Return(registry.DistributionInspect{
				Descriptor: ocispec.Descriptor{Digest: "sha256:abc123"},
			}, nil).Once()

		d := testDriver(t, client)

		digest, err := d.Digest(t.Context(), "example/example:latest")
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", digest)
	})

	t.Run("reports a registry it cannot reach", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().DistributionInspect(mock.Anything, "example/example:latest", "").
			Return(registry.DistributionInspect{}, errors.New("registry unreachable")).Once()

		d := testDriver(t, client)

		_, err := d.Digest(t.Context(), "example/example:latest")
		assert.Error(t, err)
	})

	t.Run("asks with the credentials the file holds for the registry", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().DistributionInspect(mock.Anything, "registry.example.com/app:latest", mock.MatchedBy(sentCredentials("some-user", "some-password"))).
			Return(registry.DistributionInspect{
				Descriptor: ocispec.Descriptor{Digest: "sha256:abc123"},
			}, nil).Once()

		d := credentialedDriver(t, client, credentialFile(t, "registry.example.com", "some-user", "some-password"))

		digest, err := d.Digest(t.Context(), "registry.example.com/app:latest")
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", digest)
	})

	t.Run("finds a docker hub login under the legacy index key", func(t *testing.T) {
		client := NewMockClient(t)

		// A docker login against Docker Hub is stored under the legacy index
		// address, not the docker.io domain a bare reference normalises to.
		client.EXPECT().DistributionInspect(mock.Anything, "example/example:latest", mock.MatchedBy(sentCredentials("some-user", "some-password"))).
			Return(registry.DistributionInspect{
				Descriptor: ocispec.Descriptor{Digest: "sha256:abc123"},
			}, nil).Once()

		d := credentialedDriver(t, client, credentialFile(t, "https://index.docker.io/v1/", "some-user", "some-password"))

		_, err := d.Digest(t.Context(), "example/example:latest")
		require.NoError(t, err)
	})

	t.Run("asks anonymously about a registry the file does not mention", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().DistributionInspect(mock.Anything, "other.example.com/app:latest", "").
			Return(registry.DistributionInspect{
				Descriptor: ocispec.Descriptor{Digest: "sha256:abc123"},
			}, nil).Once()

		d := credentialedDriver(t, client, credentialFile(t, "registry.example.com", "some-user", "some-password"))

		_, err := d.Digest(t.Context(), "other.example.com/app:latest")
		require.NoError(t, err)
	})

	t.Run("refuses a credential file it cannot parse", func(t *testing.T) {
		// The registry is never asked: the strict mocks fail this case if
		// DistributionInspect is called.
		client := NewMockClient(t)

		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))

		d := credentialedDriver(t, client, path)

		_, err := d.Digest(t.Context(), "example/example:latest")
		assert.Error(t, err)
	})

	t.Run("reports a credential helper it cannot run without what it resolved", func(t *testing.T) {
		// The registry is never asked: the strict mocks fail this case if
		// DistributionInspect is called.
		client := NewMockClient(t)

		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"credHelpers":{"registry.example.com":"takt-test-absent"}}`), 0o600))

		d := credentialedDriver(t, client, path)

		_, err := d.Digest(t.Context(), "registry.example.com/app:latest")
		require.Error(t, err)
		assert.ErrorContains(t, err, "registry.example.com")
	})
}

func TestDriver_Start_RegistryAuth(t *testing.T) {
	t.Parallel()

	// The pull carries the same credentials a digest lookup does, resolved from
	// the file each time so a docker login on the host takes effect without a
	// restart.
	client := NewMockClient(t)

	// Read to number the attempt, so a replacement cannot collide with a
	// container being kept for its output.
	client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

	client.EXPECT().ImageList(mock.Anything, mock.Anything).Return(nil, nil)

	client.EXPECT().ImagePull(mock.Anything, "registry.example.com/app:latest",
		mock.MatchedBy(func(options image.PullOptions) bool {
			return sentCredentials("some-user", "some-password")(options.RegistryAuth)
		}),
	).Return(io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), nil).Once()

	client.EXPECT().ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()
	client.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

	d := credentialedDriver(t, client, credentialFile(t, "registry.example.com", "some-user", "some-password"))
	w := workload("example", 1, "hash-one", containerSpec("registry.example.com/app:latest", nil), nil, nil)

	_, err := d.Start(t.Context(), w)
	require.ErrorIs(t, err, driver.ErrImagePulling)

	require.Eventually(t, func() bool {
		_, err := d.Start(t.Context(), w)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}

// multiplexed frames payload the way the docker daemon frames the output of a
// container without a TTY: an 8 byte header carrying the stream type and the
// payload length, followed by the payload itself.
func multiplexed(payload string) string {
	header := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	size := len(payload)
	header[4] = byte(size >> 24)
	header[5] = byte(size >> 16)
	header[6] = byte(size >> 8)
	header[7] = byte(size)

	return string(header) + payload
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}

// testDriver builds a driver whose credential file is pinned to a path holding
// nothing, so a docker login on the machine running the tests cannot reach them.
func testDriver(t *testing.T, client docker.Client) *docker.Driver {
	t.Helper()

	return credentialedDriver(t, client, filepath.Join(t.TempDir(), "config.json"))
}

// credentialedDriver builds a driver reading registry credentials from the given
// file.
func credentialedDriver(t *testing.T, client docker.Client, configFile string) *docker.Driver {
	t.Helper()

	return docker.New(docker.Config{
		Logger:     newTestLogger(t),
		Client:     client,
		ConfigFile: configFile,
	})
}

// credentialFile writes a docker credential file holding a single login, keyed
// as a docker login would key it, and returns its path. The login is held in the
// file itself rather than behind a helper, so resolving it runs no programs.
func credentialFile(t *testing.T, registry, username, password string) string {
	t.Helper()

	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	content := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, auth)

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// sentCredentials matches the encoded credential the engine API takes, asserting
// the login inside it rather than comparing encodings byte for byte.
func sentCredentials(username, password string) func(string) bool {
	return func(encoded string) bool {
		auth, err := registry.DecodeAuthConfig(encoded)
		if err != nil {
			return false
		}

		return auth.Username == username && auth.Password == password
	}
}

// workload builds the neutral shape a driver is handed, so a test names only the
// fields it cares about.
func workload(name string, version int, hash string, spec manifest.Container, ports []driver.Port, labels map[string]string) driver.Workload {
	return driver.Workload{
		Name:     name,
		Version:  version,
		SpecHash: hash,
		Ports:    ports,
		Labels:   labels,
		Spec: manifest.Spec{
			Version:   "v1",
			Name:      name,
			Container: &spec,
		},
	}
}

// withEnv sets the environment on a workload, which lives beside the runtime block
// rather than inside it.
func withEnv(w driver.Workload, env map[string]string) driver.Workload {
	w.Env = env

	return w
}

func withVolumes(w driver.Workload, volumes ...driver.Volume) driver.Workload {
	w.Volumes = volumes

	return w
}

// withResources sets the resource limits on a workload, which live beside the
// runtime block rather than inside it.
func withResources(w driver.Workload, resources manifest.Resources) driver.Workload {
	w.Spec.Resources = &resources

	return w
}

// containerSpec builds a container block, taking nil for the parts a test leaves out.
func containerSpec(image string, command []string) manifest.Container {
	return manifest.Container{Image: image, Command: command}
}

// pulledSpec builds a container block naming a pull policy.
func pulledSpec(image string, policy manifest.PullPolicy) manifest.Container {
	return manifest.Container{Image: image, Pull: policy}
}

// hardenedSpec builds a container block naming every hardening field.
func hardenedSpec(image string) manifest.Container {
	return manifest.Container{
		Image:    image,
		User:     "65532:65532",
		ReadOnly: true,
		CapAdd:   []string{"NET_ADMIN"},
		CapDrop:  []string{"ALL"},
	}
}

// ports builds a single published TCP port, which is all most of these tests needs.
func ports(container, host int) []driver.Port {
	return []driver.Port{{Container: container, Host: host, Protocol: "tcp"}}
}
