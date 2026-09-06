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

	generated "github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/service"
)

func TestVariableAPI_SetVariable(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Target       string
		Body         any
		SetupMocks   func(*MockVariableService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Variable)
	}{
		{
			Name:   "creates a variable",
			Target: "/api/v1/variables/log-level",
			Body:   generated.VariableSpec{Value: "debug"},
			SetupMocks: func(svc *MockVariableService) {
				svc.EXPECT().Set(mock.Anything, "log-level", "debug", mock.Anything).
					Return(variable("log-level", "debug"), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
			Assert: func(t *testing.T, got generated.Variable) {
				assert.Equal(t, "log-level", got.Name)
				assert.Equal(t, "debug", got.Value)
			},
		},
		{
			Name:   "updates a variable",
			Target: "/api/v1/variables/log-level",
			Body:   generated.VariableSpec{Value: "info"},
			SetupMocks: func(svc *MockVariableService) {
				svc.EXPECT().Set(mock.Anything, "log-level", "info", mock.Anything).
					Return(variable("log-level", "info"), false, nil).Once()
			},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:   "stores an empty value",
			Target: "/api/v1/variables/empty",
			Body:   generated.VariableSpec{Value: ""},
			SetupMocks: func(svc *MockVariableService) {
				// An empty variable is a value, not a missing one: a workload reading it
				// gets an empty environment variable rather than none.
				svc.EXPECT().Set(mock.Anything, "empty", "", mock.Anything).
					Return(variable("empty", ""), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
		},
		{
			Name:         "rejects a name takt would not accept",
			Target:       "/api/v1/variables/LOG_LEVEL",
			Body:         generated.VariableSpec{Value: "debug"},
			ExpectStatus: http.StatusBadRequest,
			SetupMocks: func(svc *MockVariableService) {
				svc.EXPECT().Set(mock.Anything, "LOG_LEVEL", mock.Anything, mock.Anything).
					Return(service.Variable{}, false, service.ErrInvalidVariable).Once()
			},
		},
		{
			Name:         "reports a failure to store",
			Target:       "/api/v1/variables/log-level",
			Body:         generated.VariableSpec{Value: "debug"},
			ExpectStatus: http.StatusInternalServerError,
			SetupMocks: func(svc *MockVariableService) {
				svc.EXPECT().Set(mock.Anything, "log-level", mock.Anything, mock.Anything).
					Return(service.Variable{}, false, errors.New("database is gone")).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockVariableService(t)
			if tc.SetupMocks != nil {
				tc.SetupMocks(svc)
			}

			body, err := json.Marshal(tc.Body)
			require.NoError(t, err)

			resp := doVariable(t, svc, http.MethodPut, tc.Target, bytes.NewReader(body))
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.Assert == nil {
				return
			}

			var result generated.SetVariableResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Variable)
		})
	}
}

func TestVariableAPI_GetVariable(t *testing.T) {
	t.Parallel()

	t.Run("returns the variable and its value", func(t *testing.T) {
		svc := NewMockVariableService(t)

		stored := variable("log-level", "debug")
		stored.UsedBy = []string{"example"}

		svc.EXPECT().Get(mock.Anything, "log-level").Return(stored, nil).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables/log-level", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// The value is reported, which is the whole difference from a secret.
		var result generated.GetVariableResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Equal(t, "log-level", result.Variable.Name)
		assert.Equal(t, "debug", result.Variable.Value)
		require.NotNil(t, result.Variable.UsedBy)
		assert.Equal(t, []string{"example"}, *result.Variable.UsedBy)
	})

	t.Run("omits the readers when nothing reads it", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Get(mock.Anything, "log-level").
			Return(variable("log-level", "debug"), nil).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables/log-level", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// Absent rather than empty, so that "read by nothing" and "not reported" are
		// not the same value on the wire.
		var result generated.GetVariableResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Nil(t, result.Variable.UsedBy)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Get(mock.Anything, "nope").
			Return(service.Variable{}, service.ErrVariableNotFound).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("reports a read that failed", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Get(mock.Anything, "log-level").
			Return(service.Variable{}, errors.New("database is gone")).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables/log-level", nil)
		assert.Equal(t, http.StatusInternalServerError, resp.Code)
	})
}

func TestVariableAPI_ListVariables(t *testing.T) {
	t.Parallel()

	t.Run("returns every variable with its value", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().List(mock.Anything).Return([]service.Variable{
			variable("db-host", "localhost"),
			variable("log-level", "debug"),
		}, nil).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// Listing values is the point: reviewing what a fleet is configured with is
		// why an operator reaches for a variable rather than a secret.
		var result generated.ListVariablesResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		require.Len(t, result.Variables, 2)
		assert.Equal(t, "db-host", result.Variables[0].Name)
		assert.Equal(t, "localhost", result.Variables[0].Value)
	})

	t.Run("returns an empty list when none are stored", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.ListVariablesResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Empty(t, result.Variables)
	})

	t.Run("reports a read that failed", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().List(mock.Anything).Return(nil, errors.New("database is gone")).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables", nil)
		assert.Equal(t, http.StatusInternalServerError, resp.Code)
	})
}

func TestVariableAPI_ListVariables_Query(t *testing.T) {
	t.Parallel()

	t.Run("passes queries through", func(t *testing.T) {
		t.Parallel()

		svc := NewMockVariableService(t)
		svc.EXPECT().List(mock.Anything, []string{"$.labels.app=web", "$.labels.env=prod"}).
			Return(nil, nil).Once()

		resp := doVariable(t, svc, http.MethodGet,
			"/api/v1/variables?query=%24.labels.app%3Dweb&query=%24.labels.env%3Dprod", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		svc := NewMockVariableService(t)
		svc.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, service.ErrInvalidQuery).Once()

		resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables?query=nonsense", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func TestVariableAPI_DeleteVariable(t *testing.T) {
	t.Parallel()

	t.Run("removes a variable nothing reads", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Delete(mock.Anything, "log-level", false).Return(nil).Once()

		resp := doVariable(t, svc, http.MethodDelete, "/api/v1/variables/log-level", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Delete(mock.Anything, "log-level", false).
			Return(fmt.Errorf("%w: read by example", service.ErrVariableInUse)).Once()

		resp := doVariable(t, svc, http.MethodDelete, "/api/v1/variables/log-level", nil)
		require.Equal(t, http.StatusConflict, resp.Code)

		// The workloads reading it are named, because the caller's next question is
		// which ones.
		assert.Contains(t, resp.Body.String(), "example")
	})

	t.Run("removes one a workload reads when forced", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Delete(mock.Anything, "log-level", true).Return(nil).Once()

		resp := doVariable(t, svc, http.MethodDelete, "/api/v1/variables/log-level?force=true", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Delete(mock.Anything, "nope", false).Return(service.ErrVariableNotFound).Once()

		resp := doVariable(t, svc, http.MethodDelete, "/api/v1/variables/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("reports a delete that failed", func(t *testing.T) {
		svc := NewMockVariableService(t)

		svc.EXPECT().Delete(mock.Anything, "log-level", false).
			Return(errors.New("database is gone")).Once()

		resp := doVariable(t, svc, http.MethodDelete, "/api/v1/variables/log-level", nil)
		assert.Equal(t, http.StatusInternalServerError, resp.Code)
	})
}

// There is deliberately no counterpart to TestSecretAPI_NeverReturnsAValue. A
// variable's value is part of every response that carries one, so a test asserting
// otherwise would be asserting the feature away.

func TestVariableAPI_HidesInternalFailures(t *testing.T) {
	t.Parallel()

	svc := NewMockVariableService(t)

	svc.EXPECT().Get(mock.Anything, "log-level").
		Return(service.Variable{}, errors.New("open /var/lib/takt/state.db: permission denied")).Once()

	resp := doVariable(t, svc, http.MethodGet, "/api/v1/variables/log-level", nil)
	require.Equal(t, http.StatusInternalServerError, resp.Code)

	// The value is not worth hiding, but why a request failed still is: a database
	// path describes the server rather than the request.
	assert.NotContains(t, resp.Body.String(), "state.db")
	assert.NotContains(t, resp.Body.String(), "permission denied")
}

func variable(name, value string) service.Variable {
	now := time.Now().UTC().Truncate(time.Second)

	return service.Variable{
		Name:      name,
		Value:     value,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func doVariable(t *testing.T, svc *MockVariableService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: svc}),
		System:    api.NewSystemAPI(api.SystemAPIConfig{Logger: logger, DB: NewMockPinger(t), Observer: NewMockObserver(t)}),
		Admin:     api.NewAdminAPI(api.AdminAPIConfig{Logger: logger, Admin: NewMockAdmin(t)}),
	}).Register(mux)

	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	return resp
}
