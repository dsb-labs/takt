package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	generated "github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestWorkloadAPI_ApplyWorkload(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Path         string
		Body         any
		SetupMocks   func(*MockWorkloadService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Workload)
		AssertBody   func(*testing.T, string)
	}{
		{
			Name: "creates a new workload",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(spec generated.WorkloadSpec) bool {
					return spec.Name == "example" && spec.Container != nil
				})).Return(workload("example", generated.WorkloadStateRunning), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
			Assert: func(t *testing.T, w generated.Workload) {
				assert.Equal(t, "example", w.Name)
				assert.Equal(t, 1, w.Version)
			},
		},
		{
			Name: "updates an existing workload",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(workload("example", generated.WorkloadStateRunning), false, nil).Once()
			},
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "rejects a name that disagrees with the path",
			Path: "/api/v1/workloads/other",
			Body: containerSpec("example"),
			// A mismatch is ambiguous, so the service is never called.
			SetupMocks:   func(*MockWorkloadService) {},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name: "reports an unsupported runtime",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrUnsupportedRuntime).Once()
			},
			ExpectStatus: http.StatusUnprocessableEntity,
		},
		{
			Name: "reports a specification that fails validation",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrInvalidSpec).Once()
			},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name: "reports a specification naming no runtime",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrNoRuntime).Once()
			},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name: "reports a specification naming two runtimes",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrAmbiguousRuntime).Once()
			},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name: "reports a volume the workload mounts but does not exist",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, fmt.Errorf("%w: example-data", service.ErrVolumeNotFound)).Once()
			},
			// The caller's to fix, and the message names the volume: an operator who
			// mistyped one is otherwise told only that something went wrong.
			ExpectStatus: http.StatusBadRequest,
			AssertBody: func(t *testing.T, body string) {
				assert.Contains(t, body, "example-data")
			},
		},
		{
			Name: "reports a secret the workload reads but does not exist",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, fmt.Errorf("%w: db-password", service.ErrSecretNotFound)).Once()
			},
			// Named for the same reason a missing volume is. The workload could never
			// start, and the operator who typed the name is the one who can correct it.
			ExpectStatus: http.StatusBadRequest,
			AssertBody: func(t *testing.T, body string) {
				assert.Contains(t, body, "db-password")
			},
		},
		{
			Name: "reports a variable the workload reads but does not exist",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, fmt.Errorf("%w: log-level", service.ErrVariableNotFound)).Once()
			},
			ExpectStatus: http.StatusBadRequest,
			AssertBody: func(t *testing.T, body string) {
				assert.Contains(t, body, "log-level")
			},
		},
		{
			Name: "reports a workload that is being deleted",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrWorkloadDeleting).Once()
			},
			ExpectStatus: http.StatusConflict,
		},
		{
			Name: "reports a pinned host port another workload holds",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrHostPortTaken).Once()
			},
			// A conflict with state that already exists, not a malformed request.
			ExpectStatus: http.StatusConflict,
		},
		{
			Name: "reports having no host port to allocate",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, service.ErrNoPortsAvailable).Once()
			},
			// Nothing about the request needs to change: it becomes servable once a
			// workload is deleted or the range widened, which is what separates this
			// from the 4xx cases above.
			ExpectStatus: http.StatusServiceUnavailable,
		},
		{
			Name: "reports an unexpected failure",
			Path: "/api/v1/workloads/example",
			Body: containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, errors.New("disk is full")).Once()
			},
			ExpectStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockWorkloadService(t)
			tc.SetupMocks(svc)

			body, err := json.Marshal(tc.Body)
			require.NoError(t, err)

			resp := do(t, svc, http.MethodPut, tc.Path, bytes.NewReader(body))
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.AssertBody != nil {
				tc.AssertBody(t, resp.Body.String())
			}

			if tc.Assert == nil {
				return
			}

			var result generated.ApplyWorkloadResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Workload)
		})
	}
}

// TestWorkloadAPI_HidesInternalFailures covers every endpoint's unexpected-failure
// path, since that is the one branch a caller can reach without the server having
// decided what to tell them.
func TestWorkloadAPI_HidesInternalFailures(t *testing.T) {
	t.Parallel()

	// Shaped like the errors that actually arrive here: wrapped on the way up, and
	// carrying operational detail picked up along the route.
	internal := errors.New(`failed to query workloads: SELECT id, name FROM workload: ` +
		`unable to open database file /var/lib/orca/state.db`)

	tt := []struct {
		Name       string
		Method     string
		Target     string
		Body       any
		SetupMocks func(*MockWorkloadService)
	}{
		{
			Name:   "apply",
			Method: http.MethodPut,
			Target: "/api/v1/workloads/example",
			Body:   containerSpec("example"),
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Workload{}, false, internal).Once()
			},
		},
		{
			Name:   "get",
			Method: http.MethodGet,
			Target: "/api/v1/workloads/example",
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Get(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
		{
			Name:   "list",
			Method: http.MethodGet,
			Target: "/api/v1/workloads",
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().List(mock.Anything).Return(nil, internal).Once()
			},
		},
		{
			Name:   "delete",
			Method: http.MethodDelete,
			Target: "/api/v1/workloads/example",
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Delete(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
		{
			// The logs endpoint establishes the workload exists before it writes
			// anything, so a failure there is the one it can still report.
			Name:   "logs",
			Method: http.MethodGet,
			Target: "/api/v1/workloads/example/logs",
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Get(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
		{
			Name:   "stop",
			Method: http.MethodPost,
			Target: "/api/v1/workloads/example/stop",
			Body:   struct{}{},
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Stop(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
		{
			Name:   "start",
			Method: http.MethodPost,
			Target: "/api/v1/workloads/example/start",
			Body:   struct{}{},
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Start(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
		{
			Name:   "restart",
			Method: http.MethodPost,
			Target: "/api/v1/workloads/example/restart",
			Body:   struct{}{},
			SetupMocks: func(svc *MockWorkloadService) {
				svc.EXPECT().Restart(mock.Anything, "example").Return(service.Workload{}, internal).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockWorkloadService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)

				body = bytes.NewReader(encoded)
			}

			resp := do(t, svc, tc.Method, tc.Target, body)
			require.Equal(t, http.StatusInternalServerError, resp.Code)

			// The response says what failed, and nothing about how the server is put
			// together. A caller learning the database's path or the text of a query
			// is being handed reconnaissance.
			assert.NotContains(t, resp.Body.String(), "/var/lib/orca")
			assert.NotContains(t, resp.Body.String(), "SELECT")
			assert.NotContains(t, resp.Body.String(), "database file")
		})
	}
}

func TestWorkloadAPI_GetWorkload(t *testing.T) {
	t.Parallel()

	t.Run("returns the workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		assert.Equal(t, "example", got.Name)
		assert.Equal(t, generated.WorkloadStateRunning, got.State)
		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)

		// A running instance has not ended, so it carries no exit code.
		assert.Nil(t, (*got.Instances)[0].ExitCode)
	})

	t.Run("reports an exit code once the instance has stopped", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		stopped := workload("example", generated.WorkloadStateFailed)
		stopped.Instances = []driver.Instance{
			{ID: "container-one", State: driver.StateFailed, ExitCode: 137, SpecHash: "hash-one"},
		}

		svc.EXPECT().Get(mock.Anything, "example").Return(stopped, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)
		require.NotNil(t, (*got.Instances)[0].ExitCode)
		assert.Equal(t, 137, *(*got.Instances)[0].ExitCode)
	})

	t.Run("reports the health of a checked workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		checked := workload("example", generated.WorkloadStateFailed)
		checked.Health = service.Health{
			Checked: true,
			Result: health.Result{
				Status:    health.StatusUnhealthy,
				Failures:  3,
				CheckedAt: time.Now(),
				Error:     "/healthz answered 500",
			},
		}

		svc.EXPECT().Get(mock.Anything, "example").Return(checked, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)

		reported := (*got.Instances)[0].Health
		require.NotNil(t, reported)
		assert.Equal(t, generated.Unhealthy, reported.Status)
		require.NotNil(t, reported.Failures)
		assert.Equal(t, 3, *reported.Failures)
		require.NotNil(t, reported.Error)
		assert.Equal(t, "/healthz answered 500", *reported.Error)
		assert.NotNil(t, reported.CheckedAt)
	})

	t.Run("reports what the runtime says when orca checks nothing", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		// An image carrying its own HEALTHCHECK is being checked by docker whether or
		// not the manifest declares one, and surfacing that beats discarding it.
		declared := workload("example", generated.WorkloadStateRunning)
		declared.Instances = []driver.Instance{
			{ID: "container-one", State: driver.StateRunning, SpecHash: "hash-one", RuntimeHealth: "healthy"},
		}

		svc.EXPECT().Get(mock.Anything, "example").Return(declared, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)

		reported := (*got.Instances)[0].Health
		require.NotNil(t, reported)
		assert.Equal(t, generated.Healthy, reported.Status)

		// orca did not run this check, so it counted no failures against it.
		assert.Nil(t, reported.Failures)
	})

	t.Run("reports no health for an unchecked workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)
		assert.Nil(t, (*got.Instances)[0].Health)
	})

	t.Run("reports why a workload is not converging", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		failing := workload("example", generated.WorkloadStatePending)
		failing.LastError = "failed to start workload: no such image"
		failing.LastErrorAt = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

		svc.EXPECT().Get(mock.Anything, "example").Return(failing, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		require.NotNil(t, got.LastError)
		assert.Equal(t, failing.LastError, *got.LastError)
		require.NotNil(t, got.LastErrorAt)
		assert.Equal(t, failing.LastErrorAt, *got.LastErrorAt)
	})

	t.Run("reports no error for a converging workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		assert.Nil(t, result.Workload.LastError)
		assert.Nil(t, result.Workload.LastErrorAt)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestWorkloadAPI_ListWorkloads(t *testing.T) {
	t.Parallel()

	t.Run("returns every workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().List(mock.Anything).Return([]service.Workload{
			workload("alpha", generated.WorkloadStateRunning),
			workload("bravo", generated.WorkloadStatePending),
		}, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.ListWorkloadsResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workloads
		require.Len(t, got, 2)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
	})

	t.Run("passes queries through", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().List(mock.Anything, []string{"$.labels.app=web", "$.labels.env=prod"}).
			Return(nil, nil).Once()

		resp := do(t, svc, http.MethodGet,
			"/api/v1/workloads?query=%24.labels.app%3Dweb&query=%24.labels.env%3Dprod", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, service.ErrInvalidQuery).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads?query=nonsense", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("returns an empty array when there are none", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// An object holding an empty array rather than null, so a client can reach
		// for the field and iterate it unconditionally.
		assert.JSONEq(t, `{"workloads":[]}`, resp.Body.String())
	})
}

func TestWorkloadAPI_DeleteWorkload(t *testing.T) {
	t.Parallel()

	t.Run("accepts the deletion and returns the terminating workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		terminating := workload("example", generated.WorkloadStateTerminating)
		terminating.Deleting = true

		svc.EXPECT().Delete(mock.Anything, "example").Return(terminating, nil).Once()

		resp := do(t, svc, http.MethodDelete, "/api/v1/workloads/example", nil)

		// 202 rather than 204: the workload is not gone when the request returns,
		// so the caller gets the workload back and can watch it disappear.
		require.Equal(t, http.StatusAccepted, resp.Code)

		var result generated.DeleteWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		assert.Equal(t, generated.WorkloadStateTerminating, got.State)
		require.NotNil(t, got.Deleting)
		assert.True(t, *got.Deleting)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Delete(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodDelete, "/api/v1/workloads/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestWorkloadAPI_StopWorkload(t *testing.T) {
	t.Parallel()

	t.Run("accepts the stop and returns the suspended workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		suspended := workload("example", generated.WorkloadStateSuspended)
		suspended.Suspended = true

		svc.EXPECT().Stop(mock.Anything, "example").Return(suspended, nil).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/stop", bytes.NewReader([]byte("{}")))

		// 202 rather than 200: the workload is only marked when the request
		// returns, and the reconciler stops its work afterwards.
		require.Equal(t, http.StatusAccepted, resp.Code)

		var result generated.StopWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		assert.Equal(t, generated.WorkloadStateSuspended, got.State)
		require.NotNil(t, got.Suspended)
		assert.True(t, *got.Suspended)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Stop(mock.Anything, "example").
			Return(service.Workload{}, service.ErrWorkloadDeleting).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/stop", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusConflict, resp.Code)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Stop(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/nope/stop", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestWorkloadAPI_StartWorkload(t *testing.T) {
	t.Parallel()

	t.Run("accepts the start and returns the workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Start(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStatePending), nil).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/start", bytes.NewReader([]byte("{}")))
		require.Equal(t, http.StatusAccepted, resp.Code)

		var result generated.StartWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		got := result.Workload

		// The mark is cleared and nothing has started yet, so the workload reads
		// as pending rather than suspended.
		assert.Equal(t, generated.WorkloadStatePending, got.State)
		assert.Nil(t, got.Suspended)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Start(mock.Anything, "example").
			Return(service.Workload{}, service.ErrWorkloadDeleting).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/start", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusConflict, resp.Code)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Start(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/nope/start", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestWorkloadAPI_RestartWorkload(t *testing.T) {
	t.Parallel()

	t.Run("accepts the restart and returns the workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Restart(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/restart", bytes.NewReader([]byte("{}")))
		require.Equal(t, http.StatusAccepted, resp.Code)

		var result generated.RestartWorkloadResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		// The request is recorded rather than acted on, so the workload reads
		// exactly as it did.
		assert.Equal(t, generated.WorkloadStateRunning, result.Workload.State)
	})

	t.Run("refuses a suspended workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Restart(mock.Anything, "example").
			Return(service.Workload{}, service.ErrWorkloadSuspended).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/restart", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusConflict, resp.Code)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Restart(mock.Anything, "example").
			Return(service.Workload{}, service.ErrWorkloadDeleting).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/example/restart", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusConflict, resp.Code)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Restart(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodPost, "/api/v1/workloads/nope/restart", bytes.NewReader([]byte("{}")))
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestWorkloadAPI_GetWorkloadLogs(t *testing.T) {
	t.Parallel()

	t.Run("returns the logs as plain text", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(workload("example", generated.WorkloadStateRunning), nil).Once()
		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 100}).
			RunAndReturn(func(_ context.Context, out io.Writer, _ string, _ driver.LogOptions) error {
				_, err := out.Write([]byte("hello world\n"))
				return err
			}).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		assert.Equal(t, "hello world\n", resp.Body.String())
		assert.Contains(t, resp.Header().Get("Content-Type"), "text/plain")
	})

	t.Run("honours the tail parameter", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(workload("example", generated.WorkloadStateRunning), nil).Once()
		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 20}).Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?tail=20", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("caps an unbounded tail", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		// The server reads what it is asked to read, so an uncapped request would let
		// a caller decide how much work it does.
		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 10000}).Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?tail=999999999", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("raises a tail below the minimum", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").
			Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		// Docker reads a negative count as every line, so passing one straight
		// through would return the whole of a workload's output and make the cap
		// above bypassable by a minus sign.
		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 1}).Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?tail=-1", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "nope").
			Return(service.Workload{}, service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/nope/logs", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("asks for the attempt that was replaced", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		// For a workload restarting repeatedly this is the attempt that failed, where
		// the one running now has not failed yet.
		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 20, Previous: true}).
			Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?tail=20&previous=true", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reads the current attempt when nothing asks otherwise", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 100}).
			Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("follows the current attempt from an instant", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(workload("example", generated.WorkloadStateRunning), nil).Once()

		since := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)

		svc.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 100, Follow: true, Since: since}).
			Return(nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?follow=true&since=2026-08-25T12:00:00Z", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("refuses to follow the attempt that was replaced", func(t *testing.T) {
		svc := NewMockWorkloadService(t)

		// A retained instance has already ended, so there is nothing for a follow of it
		// to wait on. Saying so beats answering with an ordinary read that the caller
		// would sit watching forever.
		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?follow=true&previous=true", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func do(t *testing.T, svc *MockWorkloadService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: svc}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
		System:    api.NewSystemAPI(api.SystemAPIConfig{Logger: logger, DB: NewMockPinger(t), Observer: NewMockObserver(t)}),
	}).Register(mux)

	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	return resp
}

func containerSpec(name string) generated.WorkloadSpec {
	return generated.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &generated.ContainerSpec{Image: "example/example:latest"},
	}
}

func workload(name string, state generated.WorkloadState) service.Workload {
	return service.Workload{
		Name:      name,
		Version:   1,
		Runtime:   generated.Container,
		Spec:      containerSpec(name),
		State:     state,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Instances: []driver.Instance{
			{ID: "container-one", State: driver.StateRunning, SpecHash: "hash-one"},
		},
	}
}
