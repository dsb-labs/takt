package api_test

import (
	"bytes"
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
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestVolumeAPI_CreateVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Body         any
		SetupMocks   func(*MockVolumeService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Volume)
	}{
		{
			Name: "creates a volume",
			Body: generated.VolumeSpec{Version: "v1", Name: "example-data"},
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Create(mock.Anything, "example-data").
					Return(testVolume("example-data"), nil).Once()
			},
			ExpectStatus: http.StatusCreated,
			Assert: func(t *testing.T, v generated.Volume) {
				assert.Equal(t, "example-data", v.Name)

				// Where the data is, which is what something taking a backup needs.
				require.NotNil(t, v.Path)
				assert.Equal(t, "/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0", *v.Path)
			},
		},
		{
			Name: "reports a name another volume holds",
			Body: generated.VolumeSpec{Version: "v1", Name: "example-data"},
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Create(mock.Anything, "example-data").
					Return(service.Volume{}, service.ErrVolumeExists).Once()
			},
			// A volume holds data, so a repeated create is reported rather than
			// treated as success.
			ExpectStatus: http.StatusConflict,
		},
		{
			Name: "reports a name orca will not accept",
			Body: generated.VolumeSpec{Version: "v1", Name: "Example_Data"},
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Create(mock.Anything, "Example_Data").
					Return(service.Volume{}, service.ErrInvalidVolume).Once()
			},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name:         "requires a body",
			Body:         nil,
			SetupMocks:   func(*MockVolumeService) {},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name: "reports an unexpected failure",
			Body: generated.VolumeSpec{Version: "v1", Name: "example-data"},
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Create(mock.Anything, "example-data").
					Return(service.Volume{}, errors.New("disk is full")).Once()
			},
			ExpectStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVolumeService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)
				body = bytes.NewReader(encoded)
			}

			resp := doVolume(t, svc, http.MethodPost, "/api/v1/volumes", body)
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.Assert == nil {
				return
			}

			var result generated.CreateVolumeResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Volume)
		})
	}
}

func TestVolumeAPI_GetVolume(t *testing.T) {
	t.Parallel()

	usedByTwo := testVolume("example-data")
	usedByTwo.UsedBy = []string{"alpha", "bravo"}

	tt := []struct {
		Name         string
		Target       string
		SetupMocks   func(*MockVolumeService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Volume)
	}{
		{
			Name:   "returns the volume and what mounts it",
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Get(mock.Anything, "example-data").Return(usedByTwo, nil).Once()
			},
			ExpectStatus: http.StatusOK,
			Assert: func(t *testing.T, v generated.Volume) {
				assert.Equal(t, "example-data", v.Name)
				require.NotNil(t, v.UsedBy)
				assert.Equal(t, []string{"alpha", "bravo"}, *v.UsedBy)
			},
		},
		{
			Name:   "omits what mounts a volume nothing uses",
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Get(mock.Anything, "example-data").
					Return(testVolume("example-data"), nil).Once()
			},
			ExpectStatus: http.StatusOK,
			Assert: func(t *testing.T, v generated.Volume) {
				// Absent rather than an empty array, so "used by nothing" and "not
				// reported" are not the same value on the wire.
				assert.Nil(t, v.UsedBy)
			},
		},
		{
			Name:   "reports a volume that does not exist",
			Target: "/api/v1/volumes/nope",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Get(mock.Anything, "nope").
					Return(service.Volume{}, service.ErrVolumeNotFound).Once()
			},
			ExpectStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVolumeService(t)
			tc.SetupMocks(svc)

			resp := doVolume(t, svc, http.MethodGet, tc.Target, nil)
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.Assert == nil {
				return
			}

			var result generated.GetVolumeResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Volume)
		})
	}
}

func TestVolumeAPI_ListVolumes(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		SetupMocks func(*MockVolumeService)
		ExpectBody string
		Assert     func(*testing.T, []generated.Volume)
	}{
		{
			Name: "returns every volume",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().List(mock.Anything).
					Return([]service.Volume{testVolume("alpha"), testVolume("bravo")}, nil).Once()
			},
			Assert: func(t *testing.T, volumes []generated.Volume) {
				require.Len(t, volumes, 2)
				assert.Equal(t, "alpha", volumes[0].Name)
				assert.Equal(t, "bravo", volumes[1].Name)

				// Where the data is, which is what something taking a backup needs.
				require.NotNil(t, volumes[0].Path)
				assert.Equal(t, "/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0", *volumes[0].Path)
			},
		},
		{
			Name: "returns an empty array when there are none",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().List(mock.Anything).Return(nil, nil).Once()
			},
			// An object holding an empty array rather than null, so a client can reach
			// for the field and iterate it unconditionally.
			ExpectBody: `{"volumes":[]}`,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVolumeService(t)
			tc.SetupMocks(svc)

			resp := doVolume(t, svc, http.MethodGet, "/api/v1/volumes", nil)
			require.Equal(t, http.StatusOK, resp.Code)

			if tc.ExpectBody != "" {
				assert.JSONEq(t, tc.ExpectBody, resp.Body.String())
			}

			if tc.Assert == nil {
				return
			}

			var result generated.ListVolumesResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Volumes)
		})
	}
}

func TestVolumeAPI_DeleteVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Target       string
		SetupMocks   func(*MockVolumeService)
		ExpectStatus int
		ExpectBody   string
		Contains     []string
	}{
		{
			Name:   "removes the volume",
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Delete(mock.Anything, "example-data", false).Return(nil).Once()
			},
			// 200 rather than 202, unlike deleting a workload: there is nothing running
			// to wind down, so the volume is gone when the request returns.
			ExpectStatus: http.StatusOK,
			// A body, so a client decoding one needs no branch for this operation.
			ExpectBody: `{}`,
		},
		{
			Name:   "forces the deletion when asked",
			Target: "/api/v1/volumes/example-data?force=true",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Delete(mock.Anything, "example-data", true).Return(nil).Once()
			},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:   "reports the workloads holding a volume in use",
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Delete(mock.Anything, "example-data", false).
					Return(fmt.Errorf("%w: mounted by alpha, bravo", service.ErrVolumeInUse)).Once()
			},
			ExpectStatus: http.StatusConflict,
			// The names reach the caller, because the next question is which workloads
			// are holding it.
			Contains: []string{"alpha", "bravo"},
		},
		{
			Name:   "reports a volume that does not exist",
			Target: "/api/v1/volumes/nope",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Delete(mock.Anything, "nope", false).Return(service.ErrVolumeNotFound).Once()
			},
			ExpectStatus: http.StatusNotFound,
		},
		{
			Name:   "reports an unrecognised failure as unexpected",
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				// Not one of the errors this handler knows, even though it describes a
				// volume in use: the sentinel is what the status is decided from.
				svc.EXPECT().Delete(mock.Anything, "example-data", false).
					Return(errors.New("volume is in use: mounted by alpha")).Once()
			},
			ExpectStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVolumeService(t)
			tc.SetupMocks(svc)

			resp := doVolume(t, svc, http.MethodDelete, tc.Target, nil)
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.ExpectBody != "" {
				assert.JSONEq(t, tc.ExpectBody, resp.Body.String())
			}

			for _, want := range tc.Contains {
				assert.Contains(t, resp.Body.String(), want)
			}
		})
	}
}

// TestVolumeAPI_HidesInternalFailures covers every endpoint's unexpected-failure path,
// since that is the one branch a caller can reach without the server having decided
// what to tell them.
func TestVolumeAPI_HidesInternalFailures(t *testing.T) {
	t.Parallel()

	// Shaped like the errors that actually arrive here: wrapped on the way up, and
	// carrying operational detail picked up along the route.
	internal := errors.New("failed to remove volume directory: unlinkat " +
		"/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0: permission denied")

	tt := []struct {
		Name       string
		Method     string
		Target     string
		Body       any
		SetupMocks func(*MockVolumeService)
	}{
		{
			Name:   "create",
			Method: http.MethodPost,
			Target: "/api/v1/volumes",
			Body:   generated.VolumeSpec{Version: "v1", Name: "example-data"},
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Create(mock.Anything, mock.Anything).
					Return(service.Volume{}, internal).Once()
			},
		},
		{
			Name:   "get",
			Method: http.MethodGet,
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Get(mock.Anything, "example-data").Return(service.Volume{}, internal).Once()
			},
		},
		{
			Name:   "list",
			Method: http.MethodGet,
			Target: "/api/v1/volumes",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().List(mock.Anything).Return(nil, internal).Once()
			},
		},
		{
			Name:   "delete",
			Method: http.MethodDelete,
			Target: "/api/v1/volumes/example-data",
			SetupMocks: func(svc *MockVolumeService) {
				svc.EXPECT().Delete(mock.Anything, "example-data", false).Return(internal).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVolumeService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)
				body = bytes.NewReader(encoded)
			}

			resp := doVolume(t, svc, tc.Method, tc.Target, body)
			require.Equal(t, http.StatusInternalServerError, resp.Code)

			// None of what the error carried reaches the caller: not the path, not the
			// syscall, not the reason.
			assert.NotContains(t, resp.Body.String(), "/var/lib/orca")
			assert.NotContains(t, resp.Body.String(), "unlinkat")
			assert.NotContains(t, resp.Body.String(), "permission denied")
		})
	}
}

func testVolume(name string) service.Volume {
	return service.Volume{
		Name:      name,
		Path:      "/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func doVolume(t *testing.T, svc *MockVolumeService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: svc}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
	}).Register(mux)

	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	return resp
}
