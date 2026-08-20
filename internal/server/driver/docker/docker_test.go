package docker_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
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
			Name: "starts a container with orca's ownership labels",
			Workload: workload("example", 2, "hash-two", containerSpec("example/example:latest", nil, map[string]string{"EXAMPLE": "EXAMPLE"}),
				ports(8080, 4141), map[string]string{"some-key": "some-value"}),
			SetupMocks: func(c *MockClient) {
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
						return len(bindings) == 1 && bindings[0].HostPort == "4141"
					}),
					mock.Anything, mock.Anything, "orca-example-2",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "runs the command the workload names",
			Workload: workload("example", 1, "hash-one", containerSpec("example/example:latest", []string{"sh", "-c", "exit 0"}, nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return len(config.Cmd) == 3 && config.Cmd[0] == "sh" && config.Cmd[2] == "exit 0"
					}),
					mock.Anything, mock.Anything, mock.Anything, "orca-example-1",
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
			Workload: workload("example", 1, "hash-one", containerSpec("example/example:latest", nil, nil), nil, nil),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.Cmd == nil
					}),
					mock.Anything, mock.Anything, mock.Anything, "orca-example-1",
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
			Workload: workload("example", 2, "hash-two", containerSpec("example/example:latest", nil, nil), nil, map[string]string{
				docker.LabelWorkload: "someone-elses-workload",
				docker.LabelSpecHash: "forged-hash",
				docker.LabelVersion:  "999",
			}),
			SetupMocks: func(c *MockClient) {
				c.EXPECT().ImageList(mock.Anything, mock.Anything).
					Return([]image.Summary{{ID: "sha256:abc"}}, nil).Once()

				c.EXPECT().ContainerCreate(mock.Anything,
					mock.MatchedBy(func(config *dockercontainer.Config) bool {
						return config.Labels[docker.LabelWorkload] == "example" &&
							config.Labels[docker.LabelSpecHash] == "hash-two" &&
							config.Labels[docker.LabelVersion] == "2"
					}),
					mock.Anything, mock.Anything, mock.Anything, "orca-example-2",
				).Return(dockercontainer.CreateResponse{ID: "container-one"}, nil).Once()

				c.EXPECT().ContainerStart(mock.Anything, "container-one", mock.Anything).Return(nil).Once()
			},
			Assert: func(t *testing.T, id string) {
				assert.Equal(t, "container-one", id)
			},
		},
		{
			Name:     "pulls the image when it isn't present locally",
			Workload: workload("example", 0, "", containerSpec("example/example:latest", nil, nil), nil, nil),
			SetupMocks: func(c *MockClient) {
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
			Workload: workload("example", 0, "", containerSpec("example/example:latest", nil, nil), nil, nil),
			SetupMocks: func(c *MockClient) {
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

func TestDriver_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stops and removes every container it owns for the workload", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one"},
			{ID: "container-two"},
		}, nil).Once()

		for _, id := range []string{"container-one", "container-two"} {
			client.EXPECT().ContainerStop(mock.Anything, id, mock.Anything).Return(nil).Once()
			client.EXPECT().ContainerRemove(mock.Anything, id, mock.Anything).Return(nil).Once()
		}

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Stop(t.Context(), "example"))
	})

	t.Run("forces removal so it doesn't race the container shutting down", func(t *testing.T) {
		client := NewMockClient(t)

		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return([]dockercontainer.Summary{
			{ID: "container-one"},
		}, nil).Once()

		client.EXPECT().ContainerStop(mock.Anything, "container-one", mock.Anything).Return(nil).Once()

		// ContainerStop returns before the container has necessarily stopped, so an
		// unforced remove fails with "container is running" and leaves the
		// container behind for every later pass to trip over.
		client.EXPECT().ContainerRemove(mock.Anything, "container-one",
			mock.MatchedBy(func(options dockercontainer.RemoveOptions) bool {
				return options.Force
			})).Return(nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Stop(t.Context(), "example"))
	})

	t.Run("succeeds when it owns nothing for the workload", func(t *testing.T) {
		client := NewMockClient(t)
		client.EXPECT().ContainerList(mock.Anything, mock.Anything).Return(nil, nil).Once()

		d := docker.New(docker.Config{Logger: newTestLogger(t), Client: client})

		require.NoError(t, d.Stop(t.Context(), "example"))
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
		require.NoError(t, d.Logs(t.Context(), &out, "example", 20))
		assert.Equal(t, "hello world\n", out.String())
	})
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
	w := driver.Workload{
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

	// Lifted out of the block the same way NewWorkload does it, so a test exercises
	// the field the driver actually reads.
	if spec.Env != nil {
		w.Env = *spec.Env
	}

	return w
}

// containerSpec builds a container block, taking nil for the parts a test leaves out.
func containerSpec(image string, command []string, env map[string]string) api.ContainerSpec {
	spec := api.ContainerSpec{Image: image}

	if command != nil {
		spec.Command = &command
	}
	if env != nil {
		spec.Env = &env
	}

	return spec
}

// ports builds a single published port, which is all any of these tests needs.
func ports(container, host int) []driver.Port {
	return []driver.Port{{Container: container, Host: host}}
}
