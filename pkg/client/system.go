package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dsb-labs/orca/internal/generated/api"
)

// The Readiness type is the client-side view of whether the server can do its
// job.
type Readiness struct {
	// Whether the database and every configured driver answered.
	Ready bool
	// Why the server is not ready. Empty when it is.
	Reasons []string
}

// Health reports whether the server is alive and serving requests. A nil error is
// the whole answer.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.api.GetHealthWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("failed to check health: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// Ready reports whether the server can do its job: the database answers, and
// every configured driver answered the most recent attempt to observe it.
//
// A not-ready answer is an answer rather than an error: the server responded,
// and the reasons say what is missing. The error is for a request that failed.
func (c *Client) Ready(ctx context.Context) (Readiness, error) {
	resp, err := c.api.GetReadinessWithResponse(ctx)
	if err != nil {
		return Readiness{}, fmt.Errorf("failed to check readiness: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newReadiness(*resp.JSON200), nil
	case resp.JSON503 != nil:
		return newReadiness(*resp.JSON503), nil
	case resp.JSON500 != nil:
		return Readiness{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Readiness{}, newError(resp.StatusCode(), nil)
	}
}

// Metrics writes the server's metrics to out, in the Prometheus text format.
//
// The output is copied as it arrives rather than returned, like Logs: the
// response grows with the number of workloads, and the usual destination is a
// file or a pipe rather than memory.
func (c *Client) Metrics(ctx context.Context, out io.Writer) error {
	resp, err := c.api.GetMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to read metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)

		return newError(resp.StatusCode, &body)
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to read metrics: %w", err)
	}

	return nil
}

// newReadiness converts a readiness result into its client-side view.
func newReadiness(result api.GetReadinessResult) Readiness {
	readiness := Readiness{Ready: result.Ready}
	if result.Reasons != nil {
		readiness.Reasons = *result.Reasons
	}

	return readiness
}
