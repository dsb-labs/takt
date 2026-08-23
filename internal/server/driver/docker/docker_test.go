package docker_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
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
			Name:     "starts a container with orca's ownership labels",
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
					mock.Anything, mock.Anything, "orca-example-2-1",
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
				driver.Volume{Name: "example-data", Host: "/var/lib/orca/volumes/abc", Target: "/var/lib/example"},
			),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything, mock.Anything,
					mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
						// A bind of the directory orca owns, rather than a docker
						// named volume: the exec runtime needs a real path anyway, so
						// one mechanism serves both.
						return len(host.Mounts) == 1 &&
							host.Mounts[0].Type == mount.TypeBind &&
							host.Mounts[0].Source == "/var/lib/orca/volumes/abc" &&
							host.Mounts[0].Target == "/var/lib/example"
					}),
					mock.Anything, mock.Anything, "orca-example-1-1",
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
					mock.Anything, mock.Anything, mock.Anything, "orca-example-1-1",
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
					mock.Anything, mock.Anything, mock.Anything, "orca-example-1-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			// Ownership is expressed entirely through these labels: Observe reads
			// them to decide which containers are orca's and which workload each
			// belongs to. A manifest that could set them would be able to disown a
			// container or claim another workload's, so orca's own must win.
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
					mock.Anything, mock.Anything, mock.Anything, "orca-example-2-1",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "pulls the image when it isn't present locally",
			Workload: workload("example", 0, "", containerSpec("example/example:latest", nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				// Read to number the attempt, so a replacement cannot collide with a
				// container being kept for its output.
				c.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return(nil, nil).Once()
				c.EXPECT().ImagePull(mock.Anything, "example/example:latest", mock.Anything).
					Return(io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), nil).Once()
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
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			client := NewMockClient(t)
			tc.SetupMocks(client)

			d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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
				Logger: newTestLogger(t),
				Client: client,
				Bind:   tc.Bind,
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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Stop(t.Context(), "", "example"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Stop(t.Context(), "", "example"))
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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Discard(t.Context(), "", "example"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Discard(t.Context(), "", "example"))
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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		// The file has already been rewritten, so a workload that was not told is
		// something the caller has to hear about.
		assert.Error(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.Equal(t, driver.StateExited, instances[0].State)
		assert.Zero(t, instances[0].ExitCode)
	})

	t.Run("reports a container being removed as terminating", func(t *testing.T) {
		client := NewMockClient(t)

		// Removal is orca's own doing — a replacement or a delete in progress — so
		// it must not be reported as a failure, and must not be inspected for an
		// exit code it hasn't produced yet.
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{
				ID:     "container-one",
				State:  dockercontainer.StateRemoving,
				Labels: map[string]string{docker.LabelWorkload: "example"},
			},
		}, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		// Nothing orca does will move a dead container on; docker retries it when
		// the daemon restarts, so it stays a failure rather than terminating.
		assert.Equal(t, driver.StateFailed, instances[0].State)
	})

	t.Run("reports nothing when it owns no containers", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 2)

		for _, instance := range instances {
			assert.Falsef(t, instance.Retained, "%s read as replaced by another workload's container", instance.ID)
		}
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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

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

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Previous: true}))
		assert.Equal(t, "why it died\n", out.String())
	})

	t.Run("writes nothing when the workload has only ever run once", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "current", Labels: map[string]string{docker.LabelWorkload: "example", docker.LabelAttempt: "1"}},
		}, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		// There is no earlier attempt, and saying so by writing nothing is better than
		// falling back to the current one and labelling it as the previous.
		var out strings.Builder
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20, Previous: true}))
		assert.Empty(t, out.String())
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
		mock.Anything, mock.Anything, mock.Anything, "orca-example-1-3",
	).Return(dockercontainer.CreateResponse{ID: "container-three"}, nil).Once()

	client.EXPECT().ContainerStart(mock.Anything, "container-three", mock.Anything).Return(nil).Once()

	d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

	_, err := d.Start(t.Context(), workload("example", 1, "hash-one", containerSpec("example/example:latest", nil), nil, nil))
	require.NoError(t, err)
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

// workload builds the neutral shape a driver is handed, so a test names only the
// fields it cares about.
func workload(name string, version int, hash string, spec api.ContainerSpec, ports []driver.Port, labels map[string]string) driver.Workload {
	return driver.Workload{
		Name:     name,
		Version:  version,
		SpecHash: hash,
		Ports:    ports,
		Labels:   labels,
		Spec: api.WorkloadSpec{
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

// containerSpec builds a container block, taking nil for the parts a test leaves out.
func containerSpec(image string, command []string) api.ContainerSpec {
	spec := api.ContainerSpec{Image: image}

	if command != nil {
		spec.Command = &command
	}

	return spec
}

// ports builds a single published port, which is all any of these tests needs.
func ports(container, host int) []driver.Port {
	return []driver.Port{{Container: container, Host: host}}
}
