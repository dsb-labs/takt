package api_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/reconciler"
)

func TestSystemAPI_GetHealth(t *testing.T) {
	t.Parallel()

	resp := doSystem(t, NewMockPinger(t), NewMockObserver(t), nil, "/api/v1/health")

	assert.Equal(t, http.StatusOK, resp.Code)
	assert.JSONEq(t, `{"status":"ok"}`, resp.Body.String())
}

func TestSystemAPI_GetReadiness(t *testing.T) {
	t.Parallel()

	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	tt := []struct {
		Name          string
		Ping          error
		Observations  map[string]reconciler.Observation
		ExpectStatus  int
		ExpectReasons []string
	}{
		{
			Name: "ready when everything answers",
			Observations: map[string]reconciler.Observation{
				"container": {At: observed},
				"exec":      {At: observed},
			},
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "not ready when the database does not answer",
			Ping: errors.New("database is locked"),
			Observations: map[string]reconciler.Observation{
				"container": {At: observed},
			},
			ExpectStatus:  http.StatusServiceUnavailable,
			ExpectReasons: []string{"database: database is locked"},
		},
		{
			// The map is seeded before the first pass, so this is what a poller
			// sees between the server starting and its first pass completing.
			Name: "not ready before a driver has been observed",
			Observations: map[string]reconciler.Observation{
				"container": {},
				"exec":      {At: observed},
			},
			ExpectStatus:  http.StatusServiceUnavailable,
			ExpectReasons: []string{"driver container: not observed yet"},
		},
		{
			Name: "not ready when a driver failed to answer",
			Observations: map[string]reconciler.Observation{
				"container": {At: observed, Error: "daemon gone"},
				"exec":      {At: observed},
			},
			ExpectStatus:  http.StatusServiceUnavailable,
			ExpectReasons: []string{"driver container: daemon gone"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			db := NewMockPinger(t)
			db.EXPECT().PingContext(mock.Anything).Return(tc.Ping).Once()

			observer := NewMockObserver(t)
			observer.EXPECT().Observations().Return(tc.Observations).Once()

			resp := doSystem(t, db, observer, nil, "/api/v1/ready")
			assert.Equal(t, tc.ExpectStatus, resp.Code)

			var body struct {
				Ready   bool     `json:"ready"`
				Reasons []string `json:"reasons"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

			assert.Equal(t, tc.ExpectStatus == http.StatusOK, body.Ready)
			assert.Equal(t, tc.ExpectReasons, body.Reasons)
		})
	}
}

func TestSystemAPI_GetMetrics(t *testing.T) {
	t.Parallel()

	t.Run("serves gathered metrics as prometheus text", func(t *testing.T) {
		registry := prometheus.NewPedanticRegistry()

		counter := prometheus.NewCounter(prometheus.CounterOpts{
			Name: "takt_test_total",
			Help: "A counter the test registers.",
		})
		require.NoError(t, registry.Register(counter))
		counter.Inc()

		resp := doSystem(t, NewMockPinger(t), NewMockObserver(t), registry, "/api/v1/metrics")

		assert.Equal(t, http.StatusOK, resp.Code)
		assert.True(t, strings.HasPrefix(resp.Header().Get("Content-Type"), "text/plain"))
		assert.Contains(t, resp.Body.String(), "takt_test_total 1")
	})

	t.Run("reports a gather failure rather than truncating", func(t *testing.T) {
		failing := gatherFunc(func() ([]*dto.MetricFamily, error) {
			return nil, errors.New("collector broke")
		})

		resp := doSystem(t, NewMockPinger(t), NewMockObserver(t), failing, "/api/v1/metrics")

		assert.Equal(t, http.StatusInternalServerError, resp.Code)
		assert.Contains(t, resp.Body.String(), "failed to gather metrics")
	})
}

// The gatherFunc type lets a test stand a function in for a prometheus registry.
type gatherFunc func() ([]*dto.MetricFamily, error)

// Gather returns whatever the function reports.
func (f gatherFunc) Gather() ([]*dto.MetricFamily, error) {
	return f()
}

func doSystem(t *testing.T, db api.Pinger, observer api.Observer, metrics prometheus.Gatherer, target string) *httptest.ResponseRecorder {
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
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
		System:    api.NewSystemAPI(api.SystemAPIConfig{Logger: logger, DB: db, Observer: observer, Metrics: metrics}),
		Admin:     api.NewAdminAPI(api.AdminAPIConfig{Logger: logger, Admin: NewMockAdmin(t)}),
	}).Register(mux)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	return resp
}
