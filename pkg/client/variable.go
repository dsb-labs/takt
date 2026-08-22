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
	// The Variable type is the client-side view of a variable: a value both a
	// workload and an operator can read.
	//
	// The value is on it, unlike a Secret. That is the whole difference between the
	// two, and it is why anything worth hiding belongs in a secret instead.
	Variable struct {
		// The name that identifies the variable, and which a manifest references.
		Name string
		// The value the variable holds.
		Value string
		// The names of the workloads whose specifications reference this variable.
		// Empty for a variable nothing reads, which is one that can be deleted without
		// forcing.
		UsedBy []string
		// The time the variable was created.
		CreatedAt time.Time
		// The time the variable's value last changed.
		UpdatedAt time.Time
	}

	// The DeleteVariableOption type configures how a variable is deleted.
	DeleteVariableOption func(*deleteVariableConfig)

	deleteVariableConfig struct {
		force bool
	}
)

// WithForceDeleteVariable removes a variable even though a workload reads it.
//
// Named apart from WithForceDelete, which deletes a secret, because an option that
// could be passed to either call would compile against the wrong one.
func WithForceDeleteVariable() DeleteVariableOption {
	return func(config *deleteVariableConfig) {
		config.force = true
	}
}

// checkVariableName reports whether a name is usable as a single segment of a request
// path, for the same reason a secret's is checked.
func checkVariableName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q is not a single path segment", ErrInvalidVariableName, name)
	}

	return nil
}

// SetVariable stores value as the variable with the given name, reporting whether it
// was newly created.
//
// Setting a variable to the value it already holds does nothing, so a caller that
// sets every variable on every run does not restart the workloads reading them. A
// value that did change replaces those workloads, and reaches them as they start.
func (c *Client) SetVariable(ctx context.Context, name, value string) (Variable, bool, error) {
	if err := checkVariableName(name); err != nil {
		return Variable{}, false, err
	}

	resp, err := c.api.SetVariableWithResponse(ctx, name, api.VariableSpec{Value: value})
	if err != nil {
		return Variable{}, false, fmt.Errorf("failed to set variable: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		return newVariable(resp.JSON201.Variable), true, nil
	case resp.JSON200 != nil:
		return newVariable(resp.JSON200.Variable), false, nil
	case resp.JSON400 != nil:
		return Variable{}, false, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return Variable{}, false, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Variable{}, false, newError(resp.StatusCode(), nil)
	}
}

// GetVariable returns the variable with the given name, returning ErrVariableNotFound
// when no such variable exists.
func (c *Client) GetVariable(ctx context.Context, name string) (Variable, error) {
	if err := checkVariableName(name); err != nil {
		return Variable{}, err
	}

	resp, err := c.api.GetVariableWithResponse(ctx, name)
	if err != nil {
		return Variable{}, fmt.Errorf("failed to get variable: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newVariable(resp.JSON200.Variable), nil
	case resp.JSON404 != nil:
		return Variable{}, fmt.Errorf("%s: %w", resp.JSON404.Error, ErrVariableNotFound)
	case resp.JSON500 != nil:
		return Variable{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Variable{}, newError(resp.StatusCode(), nil)
	}
}

// ListVariables returns every variable the server holds, with their values.
func (c *Client) ListVariables(ctx context.Context) ([]Variable, error) {
	resp, err := c.api.ListVariablesWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list variables: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		variables := make([]Variable, 0, len(resp.JSON200.Variables))
		for _, variable := range resp.JSON200.Variables {
			variables = append(variables, newVariable(variable))
		}

		return variables, nil
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

// DeleteVariable removes the variable with the given name.
//
// A variable a workload reads is refused with ErrVariableInUse, and the error names
// the workloads reading it. Pass WithForceDeleteVariable to remove it anyway, which
// leaves those workloads running until something replaces them and then unable to
// start.
func (c *Client) DeleteVariable(ctx context.Context, name string, options ...DeleteVariableOption) error {
	if err := checkVariableName(name); err != nil {
		return err
	}

	config := new(deleteVariableConfig)
	for _, option := range options {
		option(config)
	}

	params := api.DeleteVariableParams{}
	if config.force {
		params.Force = &config.force
	}

	resp, err := c.api.DeleteVariableWithResponse(ctx, name, &params)
	if err != nil {
		return fmt.Errorf("failed to delete variable: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON404 != nil:
		return fmt.Errorf("%s: %w", resp.JSON404.Error, ErrVariableNotFound)
	case resp.JSON409 != nil:
		// The server's message stands on its own and already names the workloads
		// reading it, so the sentinel is joined to it rather than prefixed onto it.
		return fmt.Errorf("%s: %w", resp.JSON409.Error, ErrVariableInUse)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// newVariable maps a variable as the API reports it onto the client's own shape.
func newVariable(variable api.Variable) Variable {
	out := Variable{
		Name:      variable.Name,
		Value:     variable.Value,
		CreatedAt: variable.CreatedAt,
		UpdatedAt: variable.UpdatedAt,
	}

	if variable.UsedBy != nil {
		out.UsedBy = *variable.UsedBy
	}

	return out
}
