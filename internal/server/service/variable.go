package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrVariableNotFound is returned when the requested variable does not exist.
	ErrVariableNotFound = errors.New("variable not found")
	// ErrVariableInUse is returned when a variable a workload references is deleted
	// without being forced.
	ErrVariableInUse = errors.New("variable is in use")
	// ErrInvalidVariable is returned when a variable's name is not one takt will
	// accept.
	ErrInvalidVariable = errors.New("invalid variable")
)

type (
	// The VariableRepository interface describes the persistence operations the
	// variable service uses.
	VariableRepository interface {
		// Upsert should store the given value as the variable with the given name.
		Upsert(ctx context.Context, name, value string, labels map[string]string) (database.Variable, error)
		// Get should return the variable with the given name.
		Get(ctx context.Context, name string) (database.Variable, error)
		// List should return the variables matching every one of the given
		// queries, or every variable when given none.
		List(ctx context.Context, queries ...database.Query) ([]database.Variable, error)
		// Delete should remove the variable with the given name.
		Delete(ctx context.Context, name string) error
		// UsedBy should name the workloads referencing the variable with the given
		// name.
		UsedBy(ctx context.Context, name string) ([]string, error)
	}

	// The Variable type describes a variable as it is reported to a caller.
	//
	// The value is on it, which is what separates a variable from a Secret. A secret
	// is not readable back, so an operator cannot confirm what one holds. A variable
	// exists for the cases where being able to is worth more than hiding it.
	Variable struct {
		// The name that identifies the variable.
		Name string
		// The value the variable holds.
		Value string
		// The names of the workloads referencing the variable.
		UsedBy []string
		// Arbitrary key-value pairs attached to the variable.
		Labels map[string]string
		// The time the variable was created.
		CreatedAt time.Time
		// The time the variable last changed, by its value or its labels.
		UpdatedAt time.Time
	}

	// The VariableService type owns the variables a workload reads.
	VariableService struct {
		logger    *slog.Logger
		variables VariableRepository
		rehash    func(ctx context.Context, workload string) error
	}

	// The VariableServiceConfig type contains fields used to construct a
	// VariableService.
	VariableServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the variables.
		Variables VariableRepository
		// Called for each workload referencing a variable whose value changed, so that
		// its specification hash moves and the reconciler replaces its instances. May
		// be nil, in which case a change does not redeploy anything.
		Rehash func(ctx context.Context, workload string) error
	}
)

// NewVariableService returns a new instance of the VariableService type.
func NewVariableService(config VariableServiceConfig) *VariableService {
	return &VariableService{
		logger:    config.Logger.With("component", "service"),
		variables: config.Variables,
		rehash:    config.Rehash,
	}
}

// Set stores value as the variable with the given name, returning the stored
// variable and whether it was newly created.
//
// Setting a variable to the value it already holds is a no-op: nothing reading it is
// redeployed. That mirrors applying an unchanged manifest, and it means a
// configuration management tool that sets every variable on every run does not
// restart the fleet each time.
//
// A value that did change moves the hash of every workload referencing the variable,
// so the reconciler replaces their instances.
func (s *VariableService) Set(ctx context.Context, name, value string, labels map[string]string) (Variable, bool, error) {
	if !referenceNamePattern.MatchString(name) || len(name) > 63 {
		return Variable{}, false, fmt.Errorf("%w: name must be lowercase alphanumeric, optionally separated by dashes", ErrInvalidVariable)
	}

	if err := manifest.ValidateLabels(labels); err != nil {
		return Variable{}, false, fmt.Errorf("%w: %v", ErrInvalidVariable, err)
	}

	existing, err := s.variables.Get(ctx, name)
	switch {
	case err != nil && !errors.Is(err, database.ErrVariableNotFound):
		return Variable{}, false, fmt.Errorf("failed to load variable: %w", err)
	case err == nil && existing.Value == value:
		if maps.Equal(existing.Labels, labels) {
			variable, err := s.hydrate(ctx, existing)

			return variable, false, err
		}

		// The labels moved and the value did not. Written back through the same
		// upsert, and nothing referencing the variable is redeployed: what redeploys
		// a reader is the value it reads, and that is unchanged.
		relabelled, err := s.variables.Upsert(ctx, name, value, labels)
		if err != nil {
			return Variable{}, false, err
		}

		variable, err := s.hydrate(ctx, relabelled)

		return variable, false, err
	}

	created := errors.Is(err, database.ErrVariableNotFound)

	stored, err := s.variables.Upsert(ctx, name, value, labels)
	if err != nil {
		return Variable{}, false, err
	}

	// The workloads reading it are rehashed after the value has landed, so nothing is
	// redeployed to pick up a value that failed to store.
	if err = s.redeploy(ctx, name); err != nil {
		return Variable{}, false, err
	}

	s.logger.With("variable", name, "created", created).Info("variable set")

	variable, err := s.hydrate(ctx, stored)

	return variable, created, err
}

// Get returns the variable with the given name, along with the workloads
// referencing it.
func (s *VariableService) Get(ctx context.Context, name string) (Variable, error) {
	stored, err := s.variables.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrVariableNotFound):
		return Variable{}, fmt.Errorf("%w: %s", ErrVariableNotFound, name)
	case err != nil:
		return Variable{}, err
	}

	return s.hydrate(ctx, stored)
}

// List returns the variables matching every one of the given queries, along
// with the workloads referencing each one. Passing no queries returns every
// variable.
//
// Each query is a "path=value" string, where the path is a JSON path into the
// variable's labels, such as "$.labels.app". Returns ErrInvalidQuery when
// one is malformed.
func (s *VariableService) List(ctx context.Context, queries ...string) ([]Variable, error) {
	parsed, err := parseQueries(queries)
	if err != nil {
		return nil, err
	}

	stored, err := s.variables.List(ctx, parsed...)
	if err != nil {
		if errors.Is(err, database.ErrInvalidQueryPath) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
		}

		return nil, err
	}

	variables := make([]Variable, 0, len(stored))
	for _, row := range stored {
		variable, err := s.hydrate(ctx, row)
		if err != nil {
			return nil, err
		}

		variables = append(variables, variable)
	}

	return variables, nil
}

// Delete removes the variable with the given name.
//
// A variable a workload references is refused unless force is set, and the error
// names the workloads reading it. Forcing it through leaves those workloads running:
// they find out at their next start, which is when the value is actually needed.
func (s *VariableService) Delete(ctx context.Context, name string, force bool) error {
	usedBy, err := s.variables.UsedBy(ctx, name)
	if err != nil {
		return err
	}

	if len(usedBy) > 0 && !force {
		return fmt.Errorf("%w: read by %s", ErrVariableInUse, strings.Join(usedBy, ", "))
	}

	err = s.variables.Delete(ctx, name)
	switch {
	case errors.Is(err, database.ErrVariableNotFound):
		return fmt.Errorf("%w: %s", ErrVariableNotFound, name)
	case err != nil:
		return err
	}

	// The workloads that referenced it are rehashed for the same reason a change
	// rehashes them: what they were started against no longer describes what takt
	// holds, and the hash is how that is reported.
	if err = s.redeploy(ctx, name); err != nil {
		return err
	}

	s.logger.With("variable", name, "forced", force).Info("variable deleted")

	return nil
}

// Value returns what the named variable holds, for the resolver to substitute into a
// workload's environment as it starts.
//
// Reports database.ErrVariableNotFound when nothing holds the name, which the
// resolver turns into a refusal to start rather than handing the workload the
// reference text.
func (s *VariableService) Value(ctx context.Context, name string) (string, error) {
	stored, err := s.variables.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrVariableNotFound):
		return "", err
	case err != nil:
		return "", fmt.Errorf("failed to load variable %s: %w", name, err)
	}

	return stored.Value, nil
}

// redeploy moves the specification hash of every workload referencing the named
// variable, so that the reconciler replaces the instances reading the old value.
func (s *VariableService) redeploy(ctx context.Context, name string) error {
	if s.rehash == nil {
		return nil
	}

	usedBy, err := s.variables.UsedBy(ctx, name)
	if err != nil {
		return err
	}

	// One failed rehash does not stop the rest. The value has already landed, so
	// every reader that can be moved onto it should be — stopping at the first
	// failure would leave readers on the old value for no reason of their own.
	// The failures are joined so the caller reports exactly which readers were
	// left behind.
	var failed []error
	for _, workload := range usedBy {
		if err = s.rehash(ctx, workload); err != nil {
			failed = append(failed, fmt.Errorf("failed to redeploy workload %s: %w", workload, err))
		}
	}

	return errors.Join(failed...)
}

func (s *VariableService) hydrate(ctx context.Context, row database.Variable) (Variable, error) {
	usedBy, err := s.variables.UsedBy(ctx, row.Name)
	if err != nil {
		return Variable{}, err
	}

	return Variable{
		Name:      row.Name,
		Value:     row.Value,
		UsedBy:    usedBy,
		Labels:    row.Labels,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}
