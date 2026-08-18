// Package client provides a Go client for the orca server's HTTP API.
//
// The transport is generated from the OpenAPI document, so the wire format has a
// single description. This package wraps it to present canonical domain types and
// ordinary Go errors, which keeps generated response envelopes and pointer-heavy
// optional fields out of consumers such as the CLI.
package client

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
)

var (
	// ErrWorkloadNotFound is returned when no workload exists with the given name.
	ErrWorkloadNotFound = errors.New("workload not found")
)

type (
	// The Client type talks to an orca server over HTTP.
	Client struct {
		api *api.ClientWithResponses
	}

	// The Error type describes an unsuccessful response from the server.
	Error struct {
		// The HTTP status code the server responded with.
		Status int
		// The message the server reported.
		Message string
	}
)

// New returns a Client that targets the orca server at the given address.
//
// An invalid address is rejected here so that callers see the problem at
// construction time rather than on their first request.
func New(address string) (*Client, error) {
	inner, err := api.NewClientWithResponses(address, api.WithHTTPClient(&http.Client{
		Timeout: 30 * time.Second,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to construct client: %w", err)
	}

	return &Client{api: inner}, nil
}

// Error returns the message the server reported.
func (e Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server responded with %s", http.StatusText(e.Status))
	}

	return e.Message
}

// IsNotFound reports whether err is a server-side error with a 404 status.
func IsNotFound(err error) bool {
	return hasStatus(err, http.StatusNotFound)
}

// IsBadRequest reports whether err is a server-side error with a 400 status.
func IsBadRequest(err error) bool {
	return hasStatus(err, http.StatusBadRequest)
}

// IsUnprocessable reports whether err is a server-side error with a 422 status,
// which the server uses for a specification it understands but cannot run.
func IsUnprocessable(err error) bool {
	return hasStatus(err, http.StatusUnprocessableEntity)
}

func hasStatus(err error, status int) bool {
	clientErr, ok := errors.AsType[Error](err)
	if !ok {
		return false
	}

	return clientErr.Status == status
}

// newError builds an Error from a response the server didn't handle successfully.
// The body is decoded for the server's own message where one is present, since it
// describes the failure better than the status text can.
func newError(status int, message *api.ErrorResponse) error {
	err := Error{Status: status}
	if message != nil {
		err.Message = message.Error
	}

	return err
}
