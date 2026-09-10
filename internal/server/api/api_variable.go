package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The VariableService interface describes the variable operations the API
	// exposes.
	VariableService interface {
		// Set should store value as the variable with the given name, reporting
		// whether it was newly created.
		Set(ctx context.Context, name, value string, labels map[string]string) (service.Variable, bool, error)
		// Get should return the variable with the given name.
		Get(ctx context.Context, name string) (service.Variable, error)
		// List should return the variables matching every one of the given
		// "path=value" queries, or every variable the server holds when given
		// none.
		List(ctx context.Context, queries ...string) ([]service.Variable, error)
		// Delete should remove the variable with the given name, refusing one a
		// workload reads unless force is set.
		Delete(ctx context.Context, name string, force bool) error
	}

	// The VariableAPI type exposes HTTP endpoints for managing variables.
	VariableAPI struct {
		logger    *slog.Logger
		variables VariableService
	}

	// The VariableAPIConfig type contains fields used to construct a VariableAPI.
	VariableAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the variable operations.
		Variables VariableService
	}
)

// NewVariableAPI returns a new instance of the VariableAPI type.
func NewVariableAPI(config VariableAPIConfig) *VariableAPI {
	return &VariableAPI{
		logger:    config.Logger.With("component", "api"),
		variables: config.Variables,
	}
}

// SetVariable stores the given value as the named variable.
func (a *VariableAPI) SetVariable(ctx context.Context, request api.SetVariableRequestObject) (api.SetVariableResponseObject, error) {
	if request.Body == nil {
		return api.SetVariable400JSONResponse{
			Error: "request body is required",
		}, nil
	}

	variable, created, err := a.variables.Set(ctx, request.Name, request.Body.Value, labelsOf(request.Body.Labels))
	switch {
	case errors.Is(err, service.ErrInvalidVariable):
		return api.SetVariable400JSONResponse{
			Error: err.Error(),
		}, nil
	case err != nil:
		return api.SetVariable500JSONResponse{
			Error: internalError(a.logger, "set variable", err),
		}, nil
	}

	// Created and updated are distinguished for the same reason applying a workload
	// distinguishes them: an operator who expected to be changing a variable should be
	// able to tell that they created one instead.
	if created {
		return api.SetVariable201JSONResponse{Variable: newVariable(variable)}, nil
	}

	return api.SetVariable200JSONResponse{Variable: newVariable(variable)}, nil
}

// GetVariable returns the variable with the given name.
func (a *VariableAPI) GetVariable(ctx context.Context, request api.GetVariableRequestObject) (api.GetVariableResponseObject, error) {
	variable, err := a.variables.Get(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrVariableNotFound):
		return api.GetVariable404JSONResponse{
			Error: fmt.Sprintf("variable %q does not exist", request.Name),
		}, nil
	case err != nil:
		return api.GetVariable500JSONResponse{
			Error: internalError(a.logger, "get variable", err),
		}, nil
	}

	return api.GetVariable200JSONResponse{Variable: newVariable(variable)}, nil
}

// ListVariables returns the variables matching the request's queries, or every
// variable when it carries none.
func (a *VariableAPI) ListVariables(ctx context.Context, request api.ListVariablesRequestObject) (api.ListVariablesResponseObject, error) {
	var queries []string
	if request.Params.Query != nil {
		queries = *request.Params.Query
	}

	variables, err := a.variables.List(ctx, queries...)
	switch {
	case errors.Is(err, service.ErrInvalidQuery):
		return api.ListVariables400JSONResponse{
			Error: err.Error(),
		}, nil
	case err != nil:
		return api.ListVariables500JSONResponse{
			Error: internalError(a.logger, "list variables", err),
		}, nil
	}

	response := api.ListVariables200JSONResponse{Variables: make([]api.Variable, 0, len(variables))}
	for _, variable := range variables {
		response.Variables = append(response.Variables, newVariable(variable))
	}

	return response, nil
}

// DeleteVariable removes the variable with the given name.
//
// The response is 200 rather than 202, unlike deleting a workload: there is nothing
// running to wind down, so the variable is gone when the request returns.
func (a *VariableAPI) DeleteVariable(ctx context.Context, request api.DeleteVariableRequestObject) (api.DeleteVariableResponseObject, error) {
	force := request.Params.Force != nil && *request.Params.Force

	err := a.variables.Delete(ctx, request.Name, force)
	switch {
	case errors.Is(err, service.ErrVariableNotFound):
		return api.DeleteVariable404JSONResponse{
			Error: fmt.Sprintf("variable %q does not exist", request.Name),
		}, nil
	case errors.Is(err, service.ErrVariableInUse):
		// The workloads reading it are named, because the caller's next question is
		// which ones, and answering it costs nothing here.
		return api.DeleteVariable409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		return api.DeleteVariable500JSONResponse{
			Error: internalError(a.logger, "delete variable", err),
		}, nil
	}

	return api.DeleteVariable200JSONResponse{}, nil
}

// newVariable converts a variable as the service reports it into its wire
// representation.
func newVariable(variable service.Variable) api.Variable {
	wire := api.Variable{
		Name:      variable.Name,
		Value:     variable.Value,
		CreatedAt: variable.CreatedAt,
		UpdatedAt: variable.UpdatedAt,
	}

	// Absent rather than an empty array when nothing reads it, so that "read by
	// nothing" and "not reported" are not the same value on the wire.
	if len(variable.UsedBy) > 0 {
		wire.UsedBy = &variable.UsedBy
	}

	wire.Labels = wireLabels(variable.Labels)

	return wire
}
