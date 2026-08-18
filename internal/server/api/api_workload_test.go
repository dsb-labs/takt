package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

			if tc.Assert == nil {
				return
			}

			var got generated.Workload
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
			tc.Assert(t, got)
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

		var got generated.Workload
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))

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

		var got generated.Workload
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))

		require.NotNil(t, got.Instances)
		require.Len(t, *got.Instances, 1)
		require.NotNil(t, (*got.Instances)[0].ExitCode)
		assert.Equal(t, 137, *(*got.Instances)[0].ExitCode)
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

		var got []generated.Workload
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
		require.Len(t, got, 2)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
	})

	t.Run("returns an empty array when there are none", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// An empty array rather than null, so clients can iterate unconditionally.
		assert.JSONEq(t, `[]`, resp.Body.String())
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

		var got generated.Workload
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))

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

func TestWorkloadAPI_GetWorkloadLogs(t *testing.T) {
	t.Parallel()

	t.Run("returns the logs as plain text", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Logs(mock.Anything, "example", 100).Return("hello world\n", nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		assert.Equal(t, "hello world\n", resp.Body.String())
		assert.Contains(t, resp.Header().Get("Content-Type"), "text/plain")
	})

	t.Run("honours the tail parameter", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Logs(mock.Anything, "example", 20).Return("hello world\n", nil).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/example/logs?tail=20", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		svc := NewMockWorkloadService(t)
		svc.EXPECT().Logs(mock.Anything, "nope", 100).Return("", service.ErrWorkloadNotFound).Once()

		resp := do(t, svc, http.MethodGet, "/api/v1/workloads/nope/logs", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func do(t *testing.T, svc *MockWorkloadService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	api.NewWorkloadAPI(svc).Register(mux)

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
