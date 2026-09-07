// Package client provides a Go client for the takt server's HTTP API.
//
// The transport is generated from the OpenAPI document, so the wire format has a
// single description. This package wraps it to present canonical domain types and
// ordinary Go errors, which keeps generated response envelopes and pointer-heavy
// optional fields out of consumers such as the CLI.
package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
)

var (
	// ErrWorkloadNotFound is returned when no workload exists with the given name.
	ErrWorkloadNotFound = errors.New("workload not found")
	// ErrWorkloadInUse is returned when a workload another one references is deleted
	// without being forced.
	ErrWorkloadInUse = errors.New("workload is in use")
	// ErrVolumeNotFound is returned when no volume exists with the given name.
	ErrVolumeNotFound = errors.New("volume not found")
	// ErrVolumeInUse is returned when a volume a workload mounts is deleted without
	// being forced.
	ErrVolumeInUse = errors.New("volume is in use")
	// ErrInvalidVolumeName is returned when a name is not usable as a single segment
	// of a request path.
	ErrInvalidVolumeName = errors.New("invalid volume name")
	// ErrServiceNotFound is returned when no service exists with the given name.
	ErrServiceNotFound = errors.New("service not found")
	// ErrInvalidServiceName is returned when a name is not usable as a single
	// segment of a request path.
	ErrInvalidServiceName = errors.New("invalid service name")
	// ErrSecretNotFound is returned when no secret exists with the given name.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretInUse is returned when a secret a workload reads is deleted without
	// being forced.
	ErrSecretInUse = errors.New("secret is in use")
	// ErrInvalidSecretName is returned when a name is not usable as a single segment
	// of a request path.
	ErrInvalidSecretName = errors.New("invalid secret name")
	// ErrVariableNotFound is returned when no variable exists with the given name.
	ErrVariableNotFound = errors.New("variable not found")
	// ErrVariableInUse is returned when a variable a workload reads is deleted
	// without being forced.
	ErrVariableInUse = errors.New("variable is in use")
	// ErrInvalidVariableName is returned when a name is not usable as a single
	// segment of a request path.
	ErrInvalidVariableName = errors.New("invalid variable name")
)

type (
	// The Option type is a function that modifies how New builds a Client.
	Option func(*config)

	config struct {
		caCertificate string
	}

	// The Client type talks to an takt server over HTTP.
	Client struct {
		api *api.ClientWithResponses
		// The same server, reached without a request timeout, for the one call that
		// legitimately outlives one.
		stream *api.ClientWithResponses
	}

	// The Error type describes an unsuccessful response from the server.
	Error struct {
		// The HTTP status code the server responded with.
		Status int
		// The message the server reported.
		Message string
	}
)

// The largest error body the client will read. An error message is a sentence, so
// this is generous for anything the server legitimately sends while refusing to read
// an endless response into memory.
const maxErrorBody = 1 << 16

// WithCACertificate makes the client check the server's certificate against the
// PEM certificate authority file at the given path instead of the system roots.
// This is how a client trusts a server with a self-signed certificate without
// giving up verification altogether.
func WithCACertificate(path string) Option {
	return func(c *config) { c.caCertificate = path }
}

// New returns a Client that targets the takt server at the given address.
//
// An invalid address or an unusable certificate authority file is rejected here
// so that callers see the problem at construction time rather than on their
// first request.
func New(address string, options ...Option) (*Client, error) {
	var cfg config
	for _, option := range options {
		option(&cfg)
	}

	// One transport rather than one per inner client, so a followed log read
	// checks the server's certificate the same way every other request does and
	// the two share a connection pool.
	transport, err := cfg.transport()
	if err != nil {
		return nil, err
	}

	inner, err := api.NewClientWithResponses(address, api.WithHTTPClient(&http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to construct client: %w", err)
	}

	// A followed log read is open for as long as the workload runs, and the timeout
	// above covers reading the body as well as sending the request. One client cannot
	// serve both, so following gets its own and the caller's context is what ends it.
	// Every other request keeps the timeout.
	streaming, err := api.NewClientWithResponses(address, api.WithHTTPClient(&http.Client{
		Transport: transport,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to construct client: %w", err)
	}

	return &Client{api: inner, stream: streaming}, nil
}

// transport returns the transport every request goes through.
func (c config) transport() (http.RoundTripper, error) {
	if c.caCertificate == "" {
		return http.DefaultTransport, nil
	}

	pem, err := os.ReadFile(c.caCertificate)
	if err != nil {
		return nil, fmt.Errorf("failed to read ca certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in ca certificate file %s", c.caCertificate)
	}

	// Cloned from the default transport rather than built empty, so proxy
	// support, dial timeouts and HTTP/2 stay as they are everywhere else.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}

	return transport, nil
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

// IsConflict reports whether err is a server-side error with a 409 status, which
// the server uses when a workload cannot be applied because it is being deleted.
func IsConflict(err error) bool {
	return hasStatus(err, http.StatusConflict)
}

// IsUnavailable reports whether err is a server-side error with a 503 status, which
// the server uses when it has no host port to give a workload that needs one.
func IsUnavailable(err error) bool {
	return hasStatus(err, http.StatusServiceUnavailable)
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

// wireLabels puts labels on a request, absent rather than an empty object when there
// are none.
func wireLabels(labels map[string]string) *api.Labels {
	if len(labels) == 0 {
		return nil
	}

	wire := api.Labels(labels)

	return &wire
}
