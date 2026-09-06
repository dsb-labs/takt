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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	generated "github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/service"
)

func TestSecretAPI_SetSecret(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Target       string
		Body         any
		SetupMocks   func(*MockSecretService)
		ExpectStatus int
		Assert       func(*testing.T, generated.Secret)
	}{
		{
			Name:   "creates a secret",
			Target: "/api/v1/secrets/db-password",
			Body:   generated.SecretSpec{Value: "hunter2"},
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, "db-password", []byte("hunter2"), mock.Anything).
					Return(secret("db-password"), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
			Assert: func(t *testing.T, got generated.Secret) {
				assert.Equal(t, "db-password", got.Name)
				assert.NotEmpty(t, got.Revision)
			},
		},
		{
			Name:   "updates a secret",
			Target: "/api/v1/secrets/db-password",
			Body:   generated.SecretSpec{Value: "hunter3"},
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, "db-password", []byte("hunter3"), mock.Anything).
					Return(secret("db-password"), false, nil).Once()
			},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:   "stores an empty value",
			Target: "/api/v1/secrets/db-password",
			Body:   generated.SecretSpec{Value: ""},
			SetupMocks: func(svc *MockSecretService) {
				// An empty secret is a value, not a missing one: a workload reading it
				// gets an empty variable rather than none.
				svc.EXPECT().Set(mock.Anything, "db-password", []byte(""), mock.Anything).
					Return(secret("db-password"), true, nil).Once()
			},
			ExpectStatus: http.StatusCreated,
		},
		{
			Name:         "rejects a name takt would not accept",
			Target:       "/api/v1/secrets/DB_PASSWORD",
			Body:         generated.SecretSpec{Value: "hunter2"},
			ExpectStatus: http.StatusBadRequest,
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, "DB_PASSWORD", mock.Anything, mock.Anything).
					Return(service.Secret{}, false, service.ErrInvalidSecret).Once()
			},
		},
		{
			Name:         "reports a failure to store",
			Target:       "/api/v1/secrets/db-password",
			Body:         generated.SecretSpec{Value: "hunter2"},
			ExpectStatus: http.StatusInternalServerError,
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, "db-password", mock.Anything, mock.Anything).
					Return(service.Secret{}, false, errors.New("database is gone")).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockSecretService(t)
			if tc.SetupMocks != nil {
				tc.SetupMocks(svc)
			}

			body, err := json.Marshal(tc.Body)
			require.NoError(t, err)

			resp := doSecret(t, svc, http.MethodPut, tc.Target, bytes.NewReader(body))
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.Assert == nil {
				return
			}

			var result generated.SetSecretResult
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
			tc.Assert(t, result.Secret)
		})
	}
}

func TestSecretAPI_GetSecret(t *testing.T) {
	t.Parallel()

	t.Run("returns the secret", func(t *testing.T) {
		svc := NewMockSecretService(t)

		stored := secret("db-password")
		stored.UsedBy = []string{"example"}

		svc.EXPECT().Get(mock.Anything, "db-password").Return(stored, nil).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets/db-password", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetSecretResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Equal(t, "db-password", result.Secret.Name)
		require.NotNil(t, result.Secret.UsedBy)
		assert.Equal(t, []string{"example"}, *result.Secret.UsedBy)
	})

	t.Run("omits the readers when nothing reads it", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Get(mock.Anything, "db-password").Return(secret("db-password"), nil).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets/db-password", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		// Absent rather than empty, so that "read by nothing" and "not reported" are
		// not the same value on the wire.
		var result generated.GetSecretResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Nil(t, result.Secret.UsedBy)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Get(mock.Anything, "nope").
			Return(service.Secret{}, service.ErrSecretNotFound).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestSecretAPI_ListSecrets(t *testing.T) {
	t.Parallel()

	t.Run("returns every secret", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().List(mock.Anything).
			Return([]service.Secret{secret("api-token"), secret("db-password")}, nil).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.ListSecretsResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		require.Len(t, result.Secrets, 2)
		assert.Equal(t, "api-token", result.Secrets[0].Name)
	})

	t.Run("returns an empty list when none are stored", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets", nil)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.ListSecretsResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		assert.Empty(t, result.Secrets)
	})
}

func TestSecretAPI_ListSecrets_Query(t *testing.T) {
	t.Parallel()

	t.Run("passes queries through", func(t *testing.T) {
		t.Parallel()

		svc := NewMockSecretService(t)
		svc.EXPECT().List(mock.Anything, []string{"$.labels.app=web", "$.labels.env=prod"}).
			Return(nil, nil).Once()

		resp := doSecret(t, svc, http.MethodGet,
			"/api/v1/secrets?query=%24.labels.app%3Dweb&query=%24.labels.env%3Dprod", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		svc := NewMockSecretService(t)
		svc.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, service.ErrInvalidQuery).Once()

		resp := doSecret(t, svc, http.MethodGet, "/api/v1/secrets?query=nonsense", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func TestSecretAPI_DeleteSecret(t *testing.T) {
	t.Parallel()

	t.Run("removes a secret nothing reads", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Delete(mock.Anything, "db-password", false).Return(nil).Once()

		resp := doSecret(t, svc, http.MethodDelete, "/api/v1/secrets/db-password", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Delete(mock.Anything, "db-password", false).
			Return(fmt.Errorf("%w: read by example", service.ErrSecretInUse)).Once()

		resp := doSecret(t, svc, http.MethodDelete, "/api/v1/secrets/db-password", nil)
		require.Equal(t, http.StatusConflict, resp.Code)

		// The workloads reading it are named, because the caller's next question is
		// which ones.
		assert.Contains(t, resp.Body.String(), "example")
	})

	t.Run("removes one a workload reads when forced", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Delete(mock.Anything, "db-password", true).Return(nil).Once()

		resp := doSecret(t, svc, http.MethodDelete, "/api/v1/secrets/db-password?force=true", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		svc := NewMockSecretService(t)

		svc.EXPECT().Delete(mock.Anything, "nope", false).Return(service.ErrSecretNotFound).Once()

		resp := doSecret(t, svc, http.MethodDelete, "/api/v1/secrets/nope", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestSecretAPI_NeverReturnsAValue(t *testing.T) {
	t.Parallel()

	const value = "hunter2-do-not-leak-me"

	// Every endpoint is driven with a service that holds the value, so a handler that
	// grew a way to report one would be caught here rather than in review.
	tt := []struct {
		Name       string
		Method     string
		Target     string
		Body       any
		SetupMocks func(*MockSecretService)
	}{
		{
			Name:   "set",
			Method: http.MethodPut,
			Target: "/api/v1/secrets/db-password",
			Body:   generated.SecretSpec{Value: value},
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, "db-password", []byte(value), mock.Anything).
					Return(secret("db-password"), true, nil).Once()
			},
		},
		{
			Name:   "get",
			Method: http.MethodGet,
			Target: "/api/v1/secrets/db-password",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Get(mock.Anything, "db-password").Return(secret("db-password"), nil).Once()
			},
		},
		{
			Name:   "list",
			Method: http.MethodGet,
			Target: "/api/v1/secrets",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().List(mock.Anything).Return([]service.Secret{secret("db-password")}, nil).Once()
			},
		},
		{
			Name:   "delete",
			Method: http.MethodDelete,
			Target: "/api/v1/secrets/db-password",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Delete(mock.Anything, "db-password", false).Return(nil).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockSecretService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)
				body = bytes.NewReader(encoded)
			}

			resp := doSecret(t, svc, tc.Method, tc.Target, body)
			require.Less(t, resp.Code, 300)

			assert.NotContains(t, resp.Body.String(), value)
			assert.NotContains(t, strings.ToLower(resp.Body.String()), `"value"`)
		})
	}
}

func TestSecretAPI_HidesInternalFailures(t *testing.T) {
	t.Parallel()

	// A failure while handling a secret could quote the value it was handling, so what
	// reaches the caller matters more here than elsewhere.
	internal := errors.New(`failed to encrypt secret: hunter2 under key ` +
		`/var/lib/takt/keys/da879s0hpe2ten8re4u0.key: cipher: message authentication failed`)

	tt := []struct {
		Name       string
		Method     string
		Target     string
		Body       any
		SetupMocks func(*MockSecretService)
	}{
		{
			Name:   "set",
			Method: http.MethodPut,
			Target: "/api/v1/secrets/db-password",
			Body:   generated.SecretSpec{Value: "hunter2"},
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Set(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(service.Secret{}, false, internal).Once()
			},
		},
		{
			Name:   "get",
			Method: http.MethodGet,
			Target: "/api/v1/secrets/db-password",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Get(mock.Anything, mock.Anything).Return(service.Secret{}, internal).Once()
			},
		},
		{
			Name:   "list",
			Method: http.MethodGet,
			Target: "/api/v1/secrets",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().List(mock.Anything).Return(nil, internal).Once()
			},
		},
		{
			Name:   "delete",
			Method: http.MethodDelete,
			Target: "/api/v1/secrets/db-password",
			SetupMocks: func(svc *MockSecretService) {
				svc.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything).Return(internal).Once()
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			svc := NewMockSecretService(t)
			tc.SetupMocks(svc)

			var body io.Reader
			if tc.Body != nil {
				encoded, err := json.Marshal(tc.Body)
				require.NoError(t, err)
				body = bytes.NewReader(encoded)
			}

			resp := doSecret(t, svc, tc.Method, tc.Target, body)
			require.Equal(t, http.StatusInternalServerError, resp.Code)

			reported := resp.Body.String()
			assert.NotContains(t, reported, "hunter2")
			assert.NotContains(t, reported, "da879s0hpe2ten8re4u0.key")
			assert.NotContains(t, reported, "authentication failed")
		})
	}
}

func secret(name string) service.Secret {
	now := time.Now().UTC().Truncate(time.Second)

	return service.Secret{
		Name:      name,
		Revision:  "9f2c4a1e8b7d3f6002a5c8e1b4d7f0a3",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func doSecret(t *testing.T, svc *MockSecretService, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: svc}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
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
