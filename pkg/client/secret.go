package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The Secret type is the client-side view of a secret: a value a workload can
	// read and an operator cannot.
	//
	// There is no value on it. Nothing reads a secret back out of orca, so there is
	// nothing for this type to carry.
	Secret struct {
		// The name that identifies the secret, and which a manifest references.
		Name string
		// Changes whenever the secret's value changes, and never otherwise.
		//
		// Useful for confirming a rotation landed. It says nothing about the value.
		Revision string
		// The names of the workloads whose specifications reference this secret. Empty
		// for a secret nothing reads, which is one that can be deleted without
		// forcing.
		UsedBy []string
		// Arbitrary key-value pairs attached to the secret.
		Labels map[string]string
		// The time the secret was created.
		CreatedAt time.Time
		// The time the secret last changed, by its value or its labels. The revision
		// is what says the value moved.
		UpdatedAt time.Time
	}

	// The DeleteSecretOption type configures how a secret is deleted.
	DeleteSecretOption func(*deleteSecretConfig)

	deleteSecretConfig struct {
		force bool
	}
)

// WithForceDelete removes a secret even though a workload reads it.
//
// Separate from WithForce, which deletes a volume, because an option that could be
// passed to either call would compile against the wrong one.
func WithForceDelete() DeleteSecretOption {
	return func(config *deleteSecretConfig) {
		config.force = true
	}
}

// checkSecretName reports whether a name is usable as a single segment of a request
// path, for the same reason a volume's is checked.
func checkSecretName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q is not a single path segment", ErrInvalidSecretName, name)
	}

	return nil
}

// SetSecret stores value as the secret with the given name, reporting whether it was
// newly created.
//
// Setting a secret to the value it already holds does nothing, so a caller that sets
// every secret on every run does not restart the workloads reading them. A value that
// did change replaces those workloads, and reaches them as they start.
func (c *Client) SetSecret(ctx context.Context, name string, value []byte, labels map[string]string) (Secret, bool, error) {
	if err := checkSecretName(name); err != nil {
		return Secret{}, false, err
	}

	resp, err := c.api.SetSecretWithResponse(ctx, name, api.SecretSpec{Value: string(value), Labels: wireLabels(labels)})
	if err != nil {
		return Secret{}, false, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		return newSecret(resp.JSON201.Secret), true, nil
	case resp.JSON200 != nil:
		return newSecret(resp.JSON200.Secret), false, nil
	case resp.JSON400 != nil:
		return Secret{}, false, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return Secret{}, false, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Secret{}, false, newError(resp.StatusCode(), nil)
	}
}

// GetSecret returns the secret with the given name, without its value, returning
// ErrSecretNotFound when no such secret exists.
func (c *Client) GetSecret(ctx context.Context, name string) (Secret, error) {
	if err := checkSecretName(name); err != nil {
		return Secret{}, err
	}

	resp, err := c.api.GetSecretWithResponse(ctx, name)
	if err != nil {
		return Secret{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newSecret(resp.JSON200.Secret), nil
	case resp.JSON404 != nil:
		return Secret{}, fmt.Errorf("%s: %w", resp.JSON404.Error, ErrSecretNotFound)
	case resp.JSON500 != nil:
		return Secret{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Secret{}, newError(resp.StatusCode(), nil)
	}
}

// ListSecrets returns every secret the server holds, without their values.
func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	resp, err := c.api.ListSecretsWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		secrets := make([]Secret, 0, len(resp.JSON200.Secrets))
		for _, secret := range resp.JSON200.Secrets {
			secrets = append(secrets, newSecret(secret))
		}

		return secrets, nil
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

// DeleteSecret removes the secret with the given name.
//
// A secret a workload reads is refused with ErrSecretInUse, and the error names the
// workloads reading it. Pass WithForceDelete to remove it anyway, which leaves those
// workloads running until something replaces them and then unable to start.
func (c *Client) DeleteSecret(ctx context.Context, name string, options ...DeleteSecretOption) error {
	if err := checkSecretName(name); err != nil {
		return err
	}

	config := new(deleteSecretConfig)
	for _, option := range options {
		option(config)
	}

	params := api.DeleteSecretParams{}
	if config.force {
		params.Force = &config.force
	}

	resp, err := c.api.DeleteSecretWithResponse(ctx, name, &params)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON404 != nil:
		return fmt.Errorf("%s: %w", resp.JSON404.Error, ErrSecretNotFound)
	case resp.JSON409 != nil:
		// The server's message stands on its own and already names the workloads
		// reading it, so the sentinel is joined to it rather than prefixed onto it.
		return fmt.Errorf("%s: %w", resp.JSON409.Error, ErrSecretInUse)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// newSecret maps a secret as the API reports it onto the client's own shape.
func newSecret(secret api.Secret) Secret {
	out := Secret{
		Name:      secret.Name,
		Revision:  secret.Revision,
		CreatedAt: secret.CreatedAt,
		UpdatedAt: secret.UpdatedAt,
	}

	if secret.Labels != nil {
		out.Labels = *secret.Labels
	}

	if secret.UsedBy != nil {
		out.UsedBy = *secret.UsedBy
	}

	return out
}
