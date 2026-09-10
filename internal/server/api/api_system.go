package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/reconciler"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The Pinger interface describes how the readiness endpoint asks whether the
	// database is reachable.
	Pinger interface {
		// PingContext should report an error when the database cannot be
		// reached.
		PingContext(ctx context.Context) error
	}

	// The Observer interface describes how the readiness endpoint reads the
	// reconciler's cached view of each driver.
	Observer interface {
		// Observations should report how the most recent attempt to observe
		// each driver ended, keyed by runtime name.
		Observations() map[string]reconciler.Observation
	}

	// The ScrapeTargeter interface describes how the discovery endpoint learns
	// which workloads prometheus should scrape.
	ScrapeTargeter interface {
		// ScrapeTargets should report the workloads that opted into scraping
		// as http_sd target groups, ordered by workload name.
		ScrapeTargets(ctx context.Context) ([]service.ScrapeTarget, error)
	}

	// The SystemAPI type exposes HTTP endpoints describing the server itself:
	// liveness, readiness, metrics, and the scrape targets it discovers for
	// prometheus.
	SystemAPI struct {
		logger   *slog.Logger
		db       Pinger
		observer Observer
		metrics  prometheus.Gatherer
		targets  ScrapeTargeter
	}

	// The SystemAPIConfig type contains fields used to construct a SystemAPI.
	SystemAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The database the readiness endpoint pings.
		DB Pinger
		// Reports the reconciler's cached view of each driver.
		Observer Observer
		// The gatherer the metrics endpoint reads from.
		Metrics prometheus.Gatherer
		// Reports the workloads prometheus should scrape.
		Targets ScrapeTargeter
	}
)

// NewSystemAPI returns a new instance of the SystemAPI type.
func NewSystemAPI(config SystemAPIConfig) *SystemAPI {
	return &SystemAPI{
		logger:   config.Logger.With("component", "api"),
		db:       config.DB,
		observer: config.Observer,
		metrics:  config.Metrics,
		targets:  config.Targets,
	}
}

// GetHealth reports that the server is alive. Handling the request is the answer,
// so there is nothing to check.
func (a *SystemAPI) GetHealth(_ context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	return api.GetHealth200JSONResponse{Status: api.Ok}, nil
}

// GetReadiness reports whether the server can do its job: the database answers,
// and every configured driver answered the most recent attempt to observe it.
func (a *SystemAPI) GetReadiness(ctx context.Context, _ api.GetReadinessRequestObject) (api.GetReadinessResponseObject, error) {
	var reasons []string

	// A live ping rather than a cached answer, unlike the drivers: the database
	// is a local file, so asking costs no more than remembering would.
	if err := a.db.PingContext(ctx); err != nil {
		reasons = append(reasons, fmt.Sprintf("database: %v", err))
	}

	for name, observation := range a.observer.Observations() {
		switch {
		case observation.At.IsZero():
			reasons = append(reasons, fmt.Sprintf("driver %s: not observed yet", name))
		case observation.Error != "":
			reasons = append(reasons, fmt.Sprintf("driver %s: %s", name, observation.Error))
		}
	}

	if len(reasons) > 0 {
		// The observations come from a map, so without this the reasons would
		// reorder between polls that saw no change.
		slices.Sort(reasons)

		return api.GetReadiness503JSONResponse{Ready: false, Reasons: &reasons}, nil
	}

	return api.GetReadiness200JSONResponse{Ready: true}, nil
}

// GetMetrics returns everything the server's meters record, in the Prometheus
// text format.
//
// Gathering happens before the first byte is written, so a failure is a 500
// rather than a truncated 200 nothing can retract.
func (a *SystemAPI) GetMetrics(_ context.Context, _ api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	families, err := a.metrics.Gather()
	if err != nil {
		return api.GetMetrics500JSONResponse{
			Error: internalError(a.logger, "gather metrics", err),
		}, nil
	}

	return metricsResponse{families: families}, nil
}

// The metricsResponse type writes gathered metrics in the Prometheus text format.
//
// The generated response type for this endpoint is a string with a bare
// text/plain content type. This writes through the Prometheus text encoder
// instead, whose content type also carries the format version scrapers expect.
type metricsResponse struct {
	families []*dto.MetricFamily
}

// VisitGetMetricsResponse writes the metric families to w as Prometheus text.
func (r metricsResponse) VisitGetMetricsResponse(w http.ResponseWriter) error {
	format := expfmt.NewFormat(expfmt.TypeTextPlain)

	w.Header().Set("Content-Type", string(format))
	w.WriteHeader(http.StatusOK)

	encoder := expfmt.NewEncoder(w, format)
	for _, family := range r.families {
		if err := encoder.Encode(family); err != nil {
			return err
		}
	}

	return nil
}

// GetPrometheusTargets reports the workloads that opted into scraping as
// target groups in the shape prometheus's http_sd_configs reads.
func (a *SystemAPI) GetPrometheusTargets(ctx context.Context, _ api.GetPrometheusTargetsRequestObject) (api.GetPrometheusTargetsResponseObject, error) {
	groups, err := a.targets.ScrapeTargets(ctx)
	if err != nil {
		return api.GetPrometheusTargets500JSONResponse{
			Error: internalError(a.logger, "discover scrape targets", err),
		}, nil
	}

	// Empty is an array rather than null, because prometheus decodes the body
	// as a list and a fleet with nothing to scrape is still an answer.
	response := make(api.GetPrometheusTargets200JSONResponse, 0, len(groups))
	for _, group := range groups {
		response = append(response, api.ScrapeTargetGroup{
			Targets: group.Targets,
			Labels:  group.Labels,
		})
	}

	return response, nil
}
