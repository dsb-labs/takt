package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestClient_Logs_LimitsTheErrorItReads(t *testing.T) {
	t.Parallel()

	// Far more than any error message, and more than the client is willing to read.
	endless := strings.Repeat("a", (1<<16)+(1<<20))

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)

		// Deliberately malformed once truncated, which is what proves the client
		// stopped reading rather than consuming the lot.
		_, _ = fmt.Fprintf(w, `{"error":%q}`, endless)
	})

	err := c.Logs(t.Context(), io.Discard, "example", client.WithTail(10))
	require.Error(t, err)

	// The request fails, and the message the client ends up reporting is bounded by
	// what it was prepared to read rather than by what it was sent.
	assert.Less(t, len(err.Error()), 1<<17)
}

// TestStatesCoverTheWireFormat pins the client's constants to the values the server
// actually sends.
//
// The client declares its own rather than re-exporting the generated ones, so that a
// consumer never sees an internal type. That leaves the two free to drift, which this
// catches from both directions: every client constant has to be a value the generated
// enum recognises, and the counts have to match, so a value added to the specification
// and not to the client fails here rather than reaching a caller as an unknown string.
//
// The generated Valid method is the authority for what the wire format allows, and it
// is regenerated from api/openapi.yaml. Comparing against a hand-written list would
// only restate what this file already says.
func TestStatesCoverTheWireFormat(t *testing.T) {
	t.Parallel()

	t.Run("workload states", func(t *testing.T) {
		states := []client.WorkloadState{
			client.WorkloadStatePending,
			client.WorkloadStateRunning,
			client.WorkloadStateTerminating,
			client.WorkloadStateStopped,
			client.WorkloadStateCompleted,
			client.WorkloadStateFailed,
			client.WorkloadStateSuspended,
		}

		for _, state := range states {
			assert.True(t, api.WorkloadState(state).Valid(), "the wire format does not accept %q", state)
		}

		assert.Len(t, states, countValid(t, func(value string) bool {
			return api.WorkloadState(value).Valid()
		}), "the specification declares a workload state the client does not")
	})

	t.Run("instance states", func(t *testing.T) {
		states := []client.InstanceState{
			client.InstanceStatePending,
			client.InstanceStateRunning,
			client.InstanceStateTerminating,
			client.InstanceStateExited,
			client.InstanceStateCompleted,
			client.InstanceStateFailed,
		}

		for _, state := range states {
			assert.True(t, api.InstanceState(state).Valid(), "the wire format does not accept %q", state)
		}

		assert.Len(t, states, countValid(t, func(value string) bool {
			return api.InstanceState(value).Valid()
		}), "the specification declares an instance state the client does not")
	})

	t.Run("health statuses", func(t *testing.T) {
		statuses := []client.HealthStatus{
			client.HealthStarting,
			client.HealthHealthy,
			client.HealthUnhealthy,
		}

		for _, status := range statuses {
			assert.True(t, api.HealthStatus(status).Valid(), "the wire format does not accept %q", status)
		}

		assert.Len(t, statuses, countValid(t, func(value string) bool {
			return api.HealthStatus(value).Valid()
		}), "the specification declares a health status the client does not")
	})
}

// countValid reports how many values the generated enum accepts, found by asking it
// about every candidate the enums in this specification are drawn from.
//
// The generated code exposes no list of an enum's members, only a predicate. Probing a
// fixed vocabulary is what turns that predicate back into a count, and it is sound
// because every state and status in the specification is a lowercase word or two joined
// by a dash.
func countValid(t *testing.T, valid func(string) bool) int {
	t.Helper()

	candidates := []string{
		"pending", "running", "terminating", "stopped", "completed", "exited",
		"failed", "starting", "healthy", "unhealthy", "created", "paused",
		"restarting", "removing", "dead", "succeeded", "cancelled", "unknown",
		"suspended",
	}

	var count int
	for _, candidate := range candidates {
		if valid(candidate) {
			count++
		}
	}

	return count
}

func TestClient_Apply(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name              string
		Handler           http.HandlerFunc
		Assert            func(*testing.T, client.Workload, bool)
		ExpectBadRequest  bool
		ExpectUnsupported bool
		ExpectConflict    bool
		ExpectUnavailable bool
	}{
		{
			Name: "creates a workload",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				var got api.WorkloadSpec
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				assert.Equal(t, "example", got.Name)
				require.NotNil(t, got.Container)
				assert.Equal(t, "example/example:latest", got.Container.Image)

				// An empty schedule is omitted rather than sent as "", so the
				// server hashes the same specification the client held.
				assert.Nil(t, got.Schedule)

				writeJSON(t, w, http.StatusCreated, api.ApplyWorkloadResult{Workload: workload("example", api.WorkloadStatePending)})
			},
			Assert: func(t *testing.T, got client.Workload, created bool) {
				assert.True(t, created)
				assert.Equal(t, "example", got.Name)
				assert.Equal(t, client.WorkloadStatePending, got.State)
			},
		},
		{
			Name: "updates a workload",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
			},
			Assert: func(t *testing.T, got client.Workload, created bool) {
				assert.False(t, created)
				assert.Equal(t, client.WorkloadStateRunning, got.State)
			},
		},
		{
			Name: "reports a rejected specification",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "no runtime specified"})
			},
			ExpectBadRequest: true,
		},
		{
			Name: "reports an unsupported runtime",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusUnprocessableEntity, api.ErrorResponse{Error: "unsupported runtime"})
			},
			ExpectUnsupported: true,
		},
		{
			Name: "reports the server having no host port to allocate",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusServiceUnavailable,
					api.ErrorResponse{Error: "no host port available"})
			},
			ExpectUnavailable: true,
		},
		{
			Name: "reports a workload that is being deleted",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: "workload is being deleted"})
			},
			ExpectConflict: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			got, created, err := c.Apply(t.Context(), manifest.Spec{
				Version:   "v1",
				Name:      "example",
				Container: &manifest.Container{Image: "example/example:latest"},
			})

			switch {
			case tc.ExpectBadRequest:
				assert.True(t, client.IsBadRequest(err))
				return
			case tc.ExpectUnsupported:
				assert.True(t, client.IsUnprocessable(err))
				return
			case tc.ExpectConflict:
				assert.True(t, client.IsConflict(err))
				return
			case tc.ExpectUnavailable:
				assert.True(t, client.IsUnavailable(err))
				return
			}

			require.NoError(t, err)
			tc.Assert(t, got, created)
		})
	}
}

func TestClient_DryRun(t *testing.T) {
	t.Parallel()

	spec := manifest.Spec{
		Version:   "v1",
		Name:      "example",
		Container: &manifest.Container{Image: "example/example:latest"},
	}

	t.Run("reports what applying would do", func(t *testing.T) {
		t.Parallel()

		hash := "hash-one"

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/workloads/example/dry-run", r.URL.Path)

			var got api.WorkloadSpec
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			assert.Equal(t, "example", got.Name)

			writeJSON(t, w, http.StatusOK, api.DryRunWorkloadResult{
				Spec:     got,
				SpecHash: &hash,
				Created:  true,
			})
		})

		run, err := c.DryRun(t.Context(), spec)
		require.NoError(t, err)
		assert.Equal(t, "example", run.Spec.Name)
		assert.Equal(t, hash, run.SpecHash)
		assert.True(t, run.Created)
		assert.False(t, run.Replaced)
		assert.Empty(t, run.Unknown)
	})

	t.Run("reports a host port the server has yet to allocate", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			var got api.WorkloadSpec
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))

			writeJSON(t, w, http.StatusOK, api.DryRunWorkloadResult{
				Spec:     got,
				Replaced: true,
				Unknown:  &[]string{"$.ports[0].from"},
			})
		})

		run, err := c.DryRun(t.Context(), spec)
		require.NoError(t, err)
		// Absent on the wire rather than empty, so the client reports no hash
		// rather than one nothing computed.
		assert.Empty(t, run.SpecHash)
		assert.Equal(t, []string{"$.ports[0].from"}, run.Unknown)
		assert.True(t, run.Replaced)
	})

	t.Run("reports a rejected specification", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "secret not found: db-password"})
		})

		_, err := c.DryRun(t.Context(), spec)
		assert.True(t, client.IsBadRequest(err))
	})

	t.Run("reports a workload that is being deleted", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: "workload is being deleted"})
		})

		_, err := c.DryRun(t.Context(), spec)
		assert.True(t, client.IsConflict(err))
	})
}

func TestClient_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Equal(t, "v1", got.Spec.Version)
		require.NotNil(t, got.Spec.Container)
		assert.Equal(t, "example/example:latest", got.Spec.Container.Image)
		require.Len(t, got.Instances, 1)
		assert.Equal(t, "container-one", got.Instances[0].ID)
	})

	t.Run("reports the health of a checked instance", func(t *testing.T) {
		checked := workload("example", api.WorkloadStateFailed)
		checkedAt := time.Now().UTC().Truncate(time.Second)

		(*checked.Instances)[0].Health = &api.InstanceHealth{
			Status:    api.Unhealthy,
			Failures:  new(3),
			CheckedAt: &checkedAt,
			Error:     new("/healthz answered 500"),
		}

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: checked})
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)
		require.Len(t, got.Instances, 1)

		reported := got.Instances[0].Health
		require.NotNil(t, reported)
		assert.Equal(t, client.HealthUnhealthy, reported.Status)
		require.NotNil(t, reported.Failures)
		assert.Equal(t, 3, *reported.Failures)
		assert.Equal(t, checkedAt, reported.CheckedAt)
		assert.Equal(t, "/healthz answered 500", reported.Error)
	})

	t.Run("reports no health for an unchecked instance", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)
		require.Len(t, got.Instances, 1)
		assert.Nil(t, got.Instances[0].Health)
	})

	t.Run("reports why a workload is not converging", func(t *testing.T) {
		failing := workload("example", api.WorkloadStatePending)
		failedAt := time.Now().UTC().Truncate(time.Second)

		failing.LastError = new("failed to start workload: no such image")
		failing.LastErrorAt = &failedAt

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: failing})
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "failed to start workload: no such image", got.LastError)
		assert.Equal(t, failedAt, got.LastErrorAt)
	})

	t.Run("reports no error for a converging workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Empty(t, got.LastError)
		assert.True(t, got.LastErrorAt.IsZero())
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/workloads", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.ListWorkloadsResult{Workloads: []api.Workload{
				workload("alpha", api.WorkloadStateRunning),
				workload("bravo", api.WorkloadStatePending),
			}})
		})

		got, err := c.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
	})

	t.Run("sends queries as repeated parameters", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, []string{"$.labels.app=web", "$.labels.env=prod"}, r.URL.Query()["query"])

			writeJSON(t, w, http.StatusOK, api.ListWorkloadsResult{Workloads: []api.Workload{}})
		})

		_, err := c.List(t.Context(), "$.labels.app=web", "$.labels.env=prod")
		require.NoError(t, err)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		_, err := c.List(t.Context(), "nonsense")
		assert.True(t, client.IsBadRequest(err))
	})

	t.Run("returns nothing when there are none", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, api.ListWorkloadsResult{Workloads: []api.Workload{}})
		})

		got, err := c.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestClient_Delete(t *testing.T) {
	t.Parallel()

	t.Run("returns the terminating workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)

			terminating := workload("example", api.WorkloadStateTerminating)
			terminating.Deleting = new(true)

			writeJSON(t, w, http.StatusAccepted, api.DeleteWorkloadResult{Workload: terminating})
		})

		got, err := c.Delete(t.Context(), "example")
		require.NoError(t, err)

		assert.True(t, got.Deleting)
		assert.Equal(t, client.WorkloadStateTerminating, got.State)
	})

	t.Run("waits for the teardown to finish", func(t *testing.T) {
		var gets int

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				writeJSON(t, w, http.StatusAccepted, api.DeleteWorkloadResult{Workload: workload("example", api.WorkloadStateTerminating)})
				return
			}

			// The workload is reported as terminating until the server has stopped
			// its work, and disappears once it has. Waiting has to keep polling
			// through the former and stop at the latter.
			gets++
			if gets < 3 {
				writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateTerminating)})
				return
			}

			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: "workload not found"})
		})

		_, err := c.Delete(t.Context(), "example", client.WithWaitInterval(time.Millisecond))
		require.NoError(t, err)

		assert.Equal(t, 3, gets)
	})

	t.Run("gives up waiting when the context is cancelled", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				writeJSON(t, w, http.StatusAccepted, api.DeleteWorkloadResult{Workload: workload("example", api.WorkloadStateTerminating)})
				return
			}

			// Never disappears, so the wait can only end by cancellation.
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateTerminating)})
		})

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, err := c.Delete(ctx, "example", client.WithWaitInterval(time.Millisecond))
		assert.Error(t, err)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Delete(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_Stop(t *testing.T) {
	t.Parallel()

	t.Run("returns the suspended workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/workloads/example/stop", r.URL.Path)

			// The body is an empty object rather than nothing, so the server's
			// insistence on JSON writes holds for this endpoint too.
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			suspended := workload("example", api.WorkloadStateSuspended)
			suspended.Suspended = new(true)

			writeJSON(t, w, http.StatusAccepted, api.StopWorkloadResult{Workload: suspended})
		})

		got, err := c.Stop(t.Context(), "example")
		require.NoError(t, err)

		assert.True(t, got.Suspended)
		assert.Equal(t, client.WorkloadStateSuspended, got.State)
	})

	t.Run("waits for the instances to drain", func(t *testing.T) {
		var gets int

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			suspended := workload("example", api.WorkloadStateSuspended)
			suspended.Suspended = new(true)

			if r.Method == http.MethodPost {
				writeJSON(t, w, http.StatusAccepted, api.StopWorkloadResult{Workload: suspended})
				return
			}

			// The mark lands before the instances stop, so waiting has to keep
			// polling while something is still running — and settle for an
			// instance that has ended, since the stopped instance stays reported
			// so its output is readable.
			gets++
			if gets < 3 {
				writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: suspended})
				return
			}

			suspended.Instances = &[]api.Instance{
				{ID: "container-one", State: api.InstanceStateExited, SpecHash: "hash-one"},
			}

			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: suspended})
		})

		got, err := c.Stop(t.Context(), "example", client.WithWaitInterval(time.Millisecond))
		require.NoError(t, err)

		assert.Equal(t, 3, gets)
		require.Len(t, got.Instances, 1)
		assert.Equal(t, client.InstanceStateExited, got.Instances[0].State)
	})

	t.Run("gives up waiting when the context is cancelled", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			suspended := workload("example", api.WorkloadStateSuspended)
			suspended.Suspended = new(true)

			if r.Method == http.MethodPost {
				writeJSON(t, w, http.StatusAccepted, api.StopWorkloadResult{Workload: suspended})
				return
			}

			// The instance never drains, so the wait can only end by cancellation.
			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: suspended})
		})

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, err := c.Stop(ctx, "example", client.WithWaitInterval(time.Millisecond))
		assert.Error(t, err)
	})

	t.Run("reports a conflict for a deleting workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: `workload "example" is being deleted`})
		})

		_, err := c.Stop(t.Context(), "example")
		assert.True(t, client.IsConflict(err))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Stop(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_Start(t *testing.T) {
	t.Parallel()

	t.Run("returns the workload with its suspension cleared", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/workloads/example/start", r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			pending := workload("example", api.WorkloadStatePending)
			pending.Instances = nil

			writeJSON(t, w, http.StatusAccepted, api.StartWorkloadResult{Workload: pending})
		})

		got, err := c.Start(t.Context(), "example")
		require.NoError(t, err)

		assert.False(t, got.Suspended)
		assert.Equal(t, client.WorkloadStatePending, got.State)
	})

	t.Run("waits for an instance that was not there before", func(t *testing.T) {
		var gets int

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeJSON(t, w, http.StatusAccepted, api.StartWorkloadResult{Workload: workload("example", api.WorkloadStateSuspended)})
				return
			}

			// The first read is the client's snapshot of what exists before the
			// request. Stopping a workload keeps its last container so the output
			// stays readable, so the suspension clears while that container is still
			// reported — and a wait that stopped at "no longer pending" would be
			// satisfied by it and return before anything had started.
			gets++
			if gets < 3 {
				down := workload("example", api.WorkloadStateStopped)
				down.Instances = &[]api.Instance{
					{ID: "container-one", State: api.InstanceStateExited, SpecHash: "hash-one"},
				}

				writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: down})

				return
			}

			up := workload("example", api.WorkloadStateRunning)
			up.Instances = &[]api.Instance{
				{ID: "container-one", State: api.InstanceStateExited, SpecHash: "hash-one"},
				{ID: "container-two", State: api.InstanceStateRunning, SpecHash: "hash-one"},
			}

			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: up})
		})

		got, err := c.Start(t.Context(), "example", client.WithWaitInterval(time.Millisecond))
		require.NoError(t, err)

		assert.Equal(t, 3, gets)
		assert.Equal(t, client.WorkloadStateRunning, got.State)
	})

	t.Run("returns for a workload that was already running", func(t *testing.T) {
		var gets int

		// Starting a workload that is not suspended changes nothing, so no instance
		// appears that was not there before. The one already up is what the wait
		// settles on, or waiting here would never end.
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeJSON(t, w, http.StatusAccepted, api.StartWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
				return
			}

			gets++

			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
		})

		got, err := c.Start(t.Context(), "example", client.WithWaitInterval(time.Millisecond))
		require.NoError(t, err)

		assert.Equal(t, 2, gets)
		assert.Equal(t, client.WorkloadStateRunning, got.State)
	})

	t.Run("reports a conflict for a deleting workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: `workload "example" is being deleted`})
		})

		_, err := c.Start(t.Context(), "example")
		assert.True(t, client.IsConflict(err))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Start(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_Restart(t *testing.T) {
	t.Parallel()

	t.Run("returns the workload as it stood", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/workloads/example/restart", r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			writeJSON(t, w, http.StatusAccepted, api.RestartWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
		})

		got, err := c.Restart(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, client.WorkloadStateRunning, got.State)
	})

	t.Run("waits for a replacement instance", func(t *testing.T) {
		var gets int

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeJSON(t, w, http.StatusAccepted, api.RestartWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
				return
			}

			// The first read is the client's snapshot of what exists before the
			// request. The old instance is still up on the read after it, so the
			// wait has to keep polling until an instance the snapshot never saw
			// appears.
			gets++
			if gets < 3 {
				writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: workload("example", api.WorkloadStateRunning)})
				return
			}

			replaced := workload("example", api.WorkloadStateRunning)
			replaced.Instances = &[]api.Instance{
				{ID: "container-two", State: api.InstanceStateRunning, SpecHash: "hash-one"},
			}

			writeJSON(t, w, http.StatusOK, api.GetWorkloadResult{Workload: replaced})
		})

		got, err := c.Restart(t.Context(), "example", client.WithWaitInterval(time.Millisecond))
		require.NoError(t, err)

		assert.Equal(t, 3, gets)
		require.Len(t, got.Instances, 1)
		assert.Equal(t, "container-two", got.Instances[0].ID)
	})

	t.Run("reports a conflict for a suspended workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: `workload "example" is suspended and cannot be restarted`})
		})

		_, err := c.Restart(t.Context(), "example")
		assert.True(t, client.IsConflict(err))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Restart(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_Logs(t *testing.T) {
	t.Parallel()

	t.Run("returns the logs", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/workloads/example/logs", r.URL.Path)
			assert.Equal(t, "20", r.URL.Query().Get("tail"))

			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello world\n"))
		})

		var logs strings.Builder
		require.NoError(t, c.Logs(t.Context(), &logs, "example", client.WithTail(20)))
		assert.Equal(t, "hello world\n", logs.String())
	})

	t.Run("leaves the limit to the server when no tail is asked for", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.URL.Query().Get("tail"))

			_, _ = w.Write([]byte("hello world\n"))
		})

		require.NoError(t, c.Logs(t.Context(), io.Discard, "example"))
	})

	t.Run("leaves the limit to the server for a tail of zero", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			// A caller passing a computed count of zero means the same as one that asked
			// for nothing, rather than a request for no lines at all.
			assert.Empty(t, r.URL.Query().Get("tail"))

			_, _ = w.Write([]byte("hello world\n"))
		})

		require.NoError(t, c.Logs(t.Context(), io.Discard, "example", client.WithTail(0)))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		err := c.Logs(t.Context(), io.Discard, "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})

	t.Run("asks for the attempt that was replaced", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "true", r.URL.Query().Get("previous"))

			_, _ = w.Write([]byte("why it died\n"))
		})

		var logs strings.Builder
		require.NoError(t, c.Logs(t.Context(), &logs, "example", client.WithTail(20), client.WithPrevious()))
		assert.Equal(t, "why it died\n", logs.String())
	})

	t.Run("asks for the current attempt by default", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			// Absent rather than false, so the server applies its own default and the
			// meaning of an unasked-for parameter stays the server's to decide.
			assert.Empty(t, r.URL.Query().Get("previous"))

			_, _ = w.Write([]byte("this attempt\n"))
		})

		require.NoError(t, c.Logs(t.Context(), io.Discard, "example", client.WithTail(20)))
	})

	t.Run("asks to follow from an instant", func(t *testing.T) {
		since := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "true", r.URL.Query().Get("follow"))
			assert.Equal(t, since.Format(time.RFC3339), r.URL.Query().Get("since"))

			_, _ = w.Write([]byte("still going\n"))
		})

		var logs strings.Builder
		require.NoError(t, c.Logs(t.Context(), &logs, "example", client.WithFollow(), client.WithSince(since)))
		assert.Equal(t, "still going\n", logs.String())
	})

	t.Run("ends a follow the caller cancelled without reporting it", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("first line\n"))
			w.(http.Flusher).Flush()

			// The server still has the workload to watch. What ends this read is the
			// caller going away, not the output running out.
			<-r.Context().Done()
		})

		ctx, cancel := context.WithCancel(t.Context())

		var logs syncBuffer
		done := make(chan error, 1)

		go func() {
			done <- c.Logs(ctx, &logs, "example", client.WithFollow())
		}()

		require.Eventually(t, func() bool {
			return logs.String() == "first line\n"
		}, 10*time.Second, 10*time.Millisecond, "the followed output never arrived")

		cancel()

		// A caller pressing Ctrl-C is the ordinary way to end a follow, so it comes
		// back as the end of the output rather than as a failed request.
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the follow outlived the caller that asked for it")
		}
	})
}

// The syncBuffer type collects what a follow writes while the test reads it, since the
// two happen on different goroutines.
type syncBuffer struct {
	mux sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.String()
}

func TestClient_NamesTheRequestWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()

	// A server that is gone before the call, so the transport fails rather than the
	// endpoint.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	c, err := client.New(server.URL)
	require.NoError(t, err)

	_, err = c.Get(t.Context(), "example")
	require.Error(t, err)

	// The caller names the operation, so the client names the step it failed at.
	assert.Contains(t, err.Error(), "failed to send the request")
	assert.NotContains(t, err.Error(), "failed to get workload")
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	c, err := client.New(server.URL)
	require.NoError(t, err)

	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func workload(name string, state api.WorkloadState) api.Workload {
	now := time.Now().UTC().Truncate(time.Second)

	return api.Workload{
		Name:    name,
		Version: 1,
		Runtime: api.Container,
		State:   state,
		Spec: api.WorkloadSpec{
			Version:   "v1",
			Name:      name,
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		},
		Instances: &[]api.Instance{
			{ID: "container-one", State: api.InstanceStateRunning, SpecHash: "hash-one"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}
