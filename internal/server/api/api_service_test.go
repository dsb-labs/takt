package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	generated "github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// testServiceSpec returns a wire specification for the example service,
// selecting workloads labelled app=web on 8080.
func testServiceSpec() generated.ServiceSpec {
	return generated.ServiceSpec{
		Version: "v1",
		Name:    "example",
		Target: generated.ServiceTarget{
			Labels: generated.Labels{"app": "web"},
			Port:   8080,
		},
	}
}

// testServiceResult returns a service as the service layer reports it, with one
// backend resolved.
func testServiceResult() service.Service {
	return service.Service{
		Name: "example",
		Target: manifest.ServiceTarget{
			Labels:   map[string]string{"app": "web"},
			Port:     8080,
			Protocol: manifest.ProtocolTCP,
		},
		Backends: []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func TestServiceAPI_ApplyService(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Target       string
		Body         any
		SetupMocks   func(*MockServiceService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Service)
	}{
		{
			Name:   "creates a service",
			Target: "/api/v1/services/example",
			Body:   testServiceSpec(),
			SetupMocks: func(svc *MockServiceService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(testServiceResult(), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
			Assert: func(t *testing.T, s generated.Service) {
				assert.Equal(t, "example", s.Name)

				require.NotNil(t, s.Backends)
				require.Len(t, *s.Backends, 1)
				assert.Equal(t, "web", (*s.Backends)[0].Workload)
				assert.Equal(t, "203.0.113.10:20000", (*s.Backends)[0].Address)
			},
		},
		{
			Name:   "updates a service that exists",
			Target: "/api/v1/services/example",
			Body:   testServiceSpec(),
			SetupMocks: func(svc *MockServiceService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(testServiceResult(), false, nil).Once()
			},
			ExpectStatus: http.StatusOK,
		},
		{
			// The path is where the resource's identity lives, so a body naming
			// something else is reported rather than resolved either way.
			Name:         "reports a body naming a different service",
			Target:       "/api/v1/services/other",
			Body:         testServiceSpec(),
			SetupMocks:   func(*MockServiceService) {},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name:   "reports an invalid service",
			Target: "/api/v1/services/example",
			Body:   testServiceSpec(),
			SetupMocks: func(svc *MockServiceService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Service{}, false, service.ErrInvalidService).Once()
			},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name:         "requires a body",
			Target:       "/api/v1/services/example",
			Body:         nil,
			SetupMocks:   func(*MockServiceService) {},
			ExpectStatus: http.StatusBadRequest,
		},
		{
			Name:   "reports an unexpected failure",
			Target: "/api/v1/services/example",
			Body:   testServiceSpec(),
			SetupMocks: func(svc *MockServiceService) {
				svc.EXPECT().Apply(mock.Anything, mock.Anything).
					Return(service.Service{}, false, errors.New("disk is full")).Once()
			},
			ExpectStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockServiceService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)
				body = bytes.NewReader(encoded)
			}

			resp := doService(t, svc, http.MethodPut, tc.Target, body)
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.Assert == nil {
				return
			}

			var result generated.ApplyServiceResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Service)
		})
	}
}

func TestServiceAPI_GetService(t *testing.T) {
	t.Parallel()

	t.Run("returns the service", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(testServiceResult(), nil).Once()

		resp := doService(t, svc, http.MethodGet, "/api/v1/services/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetServiceResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		assert.Equal(t, "example", result.Service.Name)
		assert.Equal(t, 8080, result.Service.Target.Port)

		require.NotNil(t, result.Service.Backends)
		assert.Len(t, *result.Service.Backends, 1)
	})

	t.Run("omits backends when nothing is fit to serve", func(t *testing.T) {
		t.Parallel()

		drained := testServiceResult()
		drained.Backends = nil

		svc := NewMockServiceService(t)
		svc.EXPECT().Get(mock.Anything, "example").Return(drained, nil).Once()

		resp := doService(t, svc, http.MethodGet, "/api/v1/services/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// Absent rather than empty, so "no backends" and "not reported" stay
		// two different values on the wire.
		assert.NotContains(t, resp.Body.String(), "backends")
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().Get(mock.Anything, "nope").
			Return(service.Service{}, service.ErrServiceNotFound).Once()

		resp := doService(t, svc, http.MethodGet, "/api/v1/services/nope", nil)
		require.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestServiceAPI_ListServices(t *testing.T) {
	t.Parallel()

	t.Run("returns the matching services", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().List(mock.Anything, []string{"$.labels.app=web"}).
			Return([]service.Service{testServiceResult()}, nil).Once()

		resp := doService(t, svc, http.MethodGet, "/api/v1/services?query=%24.labels.app%3Dweb", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.ListServicesResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))

		require.Len(t, result.Services, 1)
		assert.Equal(t, "example", result.Services[0].Name)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().List(mock.Anything, []string{"nope"}).
			Return(nil, service.ErrInvalidQuery).Once()

		resp := doService(t, svc, http.MethodGet, "/api/v1/services?query=nope", nil)
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func TestServiceAPI_DeleteService(t *testing.T) {
	t.Parallel()

	t.Run("removes the service", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().Delete(mock.Anything, "example").Return(nil).Once()

		resp := doService(t, svc, http.MethodDelete, "/api/v1/services/example", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// An empty object rather than no body, like every other response.
		assert.JSONEq(t, "{}", resp.Body.String())
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		svc := NewMockServiceService(t)
		svc.EXPECT().Delete(mock.Anything, "nope").Return(service.ErrServiceNotFound).Once()

		resp := doService(t, svc, http.MethodDelete, "/api/v1/services/nope", nil)
		require.Equal(t, http.StatusNotFound, resp.Code)
	})
}

// doService serves one request against an API whose service endpoints answer
// from the given mock.
func doService(t *testing.T, svc *MockServiceService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Services:  api.NewServiceAPI(api.ServiceAPIConfig{Logger: logger, Services: svc}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
		System:    api.NewSystemAPI(api.SystemAPIConfig{Logger: logger, DB: NewMockPinger(t), Observer: NewMockObserver(t)}),
		Admin:     api.NewAdminAPI(api.AdminAPIConfig{Logger: logger, Admin: NewMockAdmin(t)}),
	}).Register(mux)

	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp := httptest.NewRecorder()
	// Served through the disabled-mode authenticate middleware, as the server
	// does when the configuration carries no [auth] block, so the authorize
	// layer passes every request as it did before the auth layer existed.
	middleware.Authenticate(nil)(mux).ServeHTTP(resp, req)

	return resp
}
