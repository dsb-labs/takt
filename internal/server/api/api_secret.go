package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/service"
)

type (
	// The SecretService interface describes the secret operations the API exposes.
	//
	// There is nothing here that reads a value. The service can decrypt one, but only
	// to hand it to a workload that is starting, so the API is given an interface that
	// cannot ask.
	SecretService interface {
		// Set should store value as the secret with the given name, reporting whether
		// it was newly created.
		Set(ctx context.Context, name string, value []byte, labels map[string]string) (service.Secret, bool, error)
		// Get should return the secret with the given name, without its value.
		Get(ctx context.Context, name string) (service.Secret, error)
		// List should return the secrets matching every one of the given
		// "path=value" queries, or every secret the server holds when given none,
		// without their values.
		List(ctx context.Context, queries ...string) ([]service.Secret, error)
		// Delete should remove the secret with the given name, refusing one a workload
		// reads unless force is set.
		Delete(ctx context.Context, name string, force bool) error
	}

	// The SecretAPI type exposes HTTP endpoints for managing secrets.
	SecretAPI struct {
		logger  *slog.Logger
		secrets SecretService
	}

	// The SecretAPIConfig type contains fields used to construct a SecretAPI.
	SecretAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the secret operations.
		Secrets SecretService
	}
)

// NewSecretAPI returns a new instance of the SecretAPI type.
func NewSecretAPI(config SecretAPIConfig) *SecretAPI {
	return &SecretAPI{
		logger:  config.Logger.With("component", "api"),
		secrets: config.Secrets,
	}
}

// internalError logs why a request failed and returns the message the client is told
// instead.
//
// The error is never returned to the caller, which matters more here than elsewhere:
// a failure while encrypting or storing a secret could otherwise quote the value it
// was handling.
func (a *SecretAPI) internalError(operation string, err error) string {
	a.logger.With("error", err, "operation", operation).Error("failed to serve request")

	return "failed to " + operation
}

// SetSecret stores the given value as the named secret.
func (a *SecretAPI) SetSecret(ctx context.Context, request api.SetSecretRequestObject) (api.SetSecretResponseObject, error) {
	if request.Body == nil {
		return api.SetSecret400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: "request body is required"},
		}, nil
	}

	secret, created, err := a.secrets.Set(ctx, request.Name, []byte(request.Body.Value), labelsOf(request.Body.Labels))
	switch {
	case errors.Is(err, service.ErrInvalidSecret):
		return api.SetSecret400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.SetSecret500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("set secret", err),
			},
		}, nil
	}

	// Created and updated are distinguished for the same reason applying a workload
	// distinguishes them: an operator who expected to be rotating a secret should be
	// able to tell that they created one instead.
	if created {
		return api.SetSecret201JSONResponse{Secret: newSecret(secret)}, nil
	}

	return api.SetSecret200JSONResponse{Secret: newSecret(secret)}, nil
}

// GetSecret returns the secret with the given name, without its value.
func (a *SecretAPI) GetSecret(ctx context.Context, request api.GetSecretRequestObject) (api.GetSecretResponseObject, error) {
	secret, err := a.secrets.Get(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrSecretNotFound):
		return api.GetSecret404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("secret %q does not exist", request.Name),
			},
		}, nil
	case err != nil:
		return api.GetSecret500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("get secret", err),
			},
		}, nil
	}

	return api.GetSecret200JSONResponse{Secret: newSecret(secret)}, nil
}

// ListSecrets returns the secrets matching the request's queries, or every
// secret when it carries none, without their values.
func (a *SecretAPI) ListSecrets(ctx context.Context, request api.ListSecretsRequestObject) (api.ListSecretsResponseObject, error) {
	var queries []string
	if request.Params.Query != nil {
		queries = *request.Params.Query
	}

	secrets, err := a.secrets.List(ctx, queries...)
	switch {
	case errors.Is(err, service.ErrInvalidQuery):
		return api.ListSecrets400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.ListSecrets500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("list secrets", err),
			},
		}, nil
	}

	response := api.ListSecrets200JSONResponse{Secrets: make([]api.Secret, 0, len(secrets))}
	for _, secret := range secrets {
		response.Secrets = append(response.Secrets, newSecret(secret))
	}

	return response, nil
}

// DeleteSecret removes the secret with the given name.
//
// The response is 200 rather than 202, unlike deleting a workload: there is nothing
// running to wind down, so the secret is gone when the request returns.
func (a *SecretAPI) DeleteSecret(ctx context.Context, request api.DeleteSecretRequestObject) (api.DeleteSecretResponseObject, error) {
	force := request.Params.Force != nil && *request.Params.Force

	err := a.secrets.Delete(ctx, request.Name, force)
	switch {
	case errors.Is(err, service.ErrSecretNotFound):
		return api.DeleteSecret404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("secret %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrSecretInUse):
		// The workloads reading it are named, because the caller's next question is
		// which ones, and answering it costs nothing here.
		return api.DeleteSecret409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		return api.DeleteSecret500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("delete secret", err),
			},
		}, nil
	}

	return api.DeleteSecret200JSONResponse{}, nil
}

// newSecret converts a secret as the service reports it into its wire
// representation.
//
// There is no value to map. The service's own type does not carry one, so nothing
// here has to remember to leave it out.
func newSecret(secret service.Secret) api.Secret {
	wire := api.Secret{
		Name:      secret.Name,
		Revision:  secret.Revision,
		CreatedAt: secret.CreatedAt,
		UpdatedAt: secret.UpdatedAt,
	}

	// Absent rather than an empty array when nothing reads it, so that "read by
	// nothing" and "not reported" are not the same value on the wire.
	if len(secret.UsedBy) > 0 {
		wire.UsedBy = &secret.UsedBy
	}

	wire.Labels = wireLabels(secret.Labels)

	return wire
}
