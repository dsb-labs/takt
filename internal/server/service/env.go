package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The SecretValue interface describes how the resolver reads the secret behind a
	// reference.
	//
	// Narrower than the secret service it is satisfied by: substituting a value needs
	// to read one secret at a time and nothing else.
	SecretValue interface {
		// Value should return the plaintext of the named secret, reporting
		// ErrSecretNotFound when no such secret exists.
		Value(ctx context.Context, name string) (string, error)
	}

	// The VariableValue interface describes how the resolver reads the variable
	// behind a reference.
	VariableValue interface {
		// Value should return what the named variable holds, reporting
		// ErrVariableNotFound when no such variable exists.
		Value(ctx context.Context, name string) (string, error)
	}

	// The EnvResolver type turns the references in a workload's environment into the
	// values it is started with.
	//
	// One type resolves both kinds because one value may hold both. A single pass is
	// not an optimisation: expansion refuses a reference it cannot resolve, so a pass
	// that saw only secrets would have to treat a variable as unresolvable, and the
	// alternative of ignoring what it does not recognise would hand a workload the
	// reference text as though it were the value.
	EnvResolver struct {
		logger    *slog.Logger
		secrets   SecretValue
		variables VariableValue
	}

	// The EnvResolverConfig type contains fields used to construct an EnvResolver.
	EnvResolverConfig struct {
		// The logger used for resolution events.
		Logger *slog.Logger
		// Where the secrets a workload reads are read from. May be nil, in which case
		// a workload referencing a secret fails to start.
		Secrets SecretValue
		// Where the variables a workload reads are read from. May be nil, in which
		// case a workload referencing a variable fails to start.
		Variables VariableValue
	}
)

// NewEnvResolver returns a new instance of the EnvResolver type.
func NewEnvResolver(config EnvResolverConfig) *EnvResolver {
	return &EnvResolver{
		logger:    config.Logger.With("component", "service"),
		secrets:   config.Secrets,
		variables: config.Variables,
	}
}

// Resolve returns env with every reference replaced by the value it names.
//
// This is the only thing that produces a secret's plaintext outside the secret
// service, and it exists for the reconciler to call as a workload starts. Returns
// ErrSecretNotFound or ErrVariableNotFound naming what could not be resolved: handing
// the workload the reference text would have it use that as the value.
func (r *EnvResolver) Resolve(ctx context.Context, env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}

	// Read once each, however many variables reference the same thing. Keyed by the
	// reference rather than by the name, because a secret and a variable may share a
	// name and hold different values.
	values := make(map[manifest.Reference]string)

	// Why a lookup came back empty, which the callback cannot report itself. Something
	// nobody created and a key that cannot decrypt what it sealed both leave a
	// reference unresolved, and an operator told the wrong one goes looking in the
	// wrong place.
	var failed error
	var missing error

	resolved := make(map[string]string, len(env))
	for key, value := range env {
		expanded, err := manifest.Expand(value, func(reference manifest.Reference) (string, bool) {
			if value, ok := values[reference]; ok {
				return value, true
			}

			value, err := r.value(ctx, reference)
			switch {
			case errors.Is(err, ErrSecretNotFound), errors.Is(err, ErrVariableNotFound):
				missing = err

				return "", false
			case err != nil:
				failed = err

				return "", false
			}

			values[reference] = value

			return value, true
		})
		switch {
		case failed != nil:
			return nil, failed
		case missing != nil:
			// Naming the environment variable as well as what it reads, so that an
			// operator has both ends of the reference that could not be resolved.
			return nil, fmt.Errorf("%s reads %w", key, missing)
		case err != nil:
			return nil, fmt.Errorf("failed to resolve env %s: %w", key, err)
		}

		resolved[key] = expanded
	}

	return resolved, nil
}

// value reads what the reference names from whichever store holds that kind.
func (r *EnvResolver) value(ctx context.Context, reference manifest.Reference) (string, error) {
	if reference.Kind == manifest.KindVariable {
		if r.variables == nil {
			return "", fmt.Errorf("%w: this server holds no variables", ErrVariableNotFound)
		}

		return r.variables.Value(ctx, reference.Name)
	}

	if r.secrets == nil {
		return "", fmt.Errorf("%w: this server holds no secrets", ErrSecretNotFound)
	}

	return r.secrets.Value(ctx, reference.Name)
}
