package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The ValueStore interface describes how the resolver reads what one reference
	// names.
	//
	// One interface for both kinds, because reading a secret and reading a variable
	// differ only in which service answers. Narrower than either service it is
	// satisfied by: substituting a value needs one name at a time and nothing else.
	ValueStore interface {
		// Value should return what the named secret or variable holds, reporting
		// ErrSecretNotFound or ErrVariableNotFound when nothing holds the name.
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
		secrets   ValueStore
		variables ValueStore
	}

	// The EnvResolverConfig type contains fields used to construct an EnvResolver.
	EnvResolverConfig struct {
		// The logger used for resolution events.
		Logger *slog.Logger
		// Where the secrets a workload reads are read from. May be nil, in which case
		// a workload referencing a secret fails to start.
		Secrets ValueStore
		// Where the variables a workload reads are read from. May be nil, in which
		// case a workload referencing a variable fails to start.
		Variables ValueStore
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
// service, and it exists for the reconciler to call as a workload starts.
//
// Returns manifest.ErrUnknownSecret or manifest.ErrUnknownVariable naming both the
// environment variable and what it could not read. Handing the workload the reference
// text would have it use that as the value.
func (r *EnvResolver) Resolve(ctx context.Context, env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}

	// Read once each, however many variables reference the same thing. Keyed by the
	// reference rather than by the name, because a secret and a variable may share a
	// name and hold different values.
	values := make(map[manifest.Reference]string)

	// Why a lookup came back empty, which the callback cannot report itself. A key
	// that cannot decrypt what it sealed leaves a reference unresolved just as a
	// missing value does, and an operator told the wrong one goes looking in the
	// wrong place. Nothing held under the name needs no such treatment: expansion
	// reports that itself, naming the kind that was asked for.
	var failed error

	resolved := make(map[string]string, len(env))
	for key, value := range env {
		expanded, err := manifest.Expand(value, func(reference manifest.Reference) (string, bool) {
			if value, ok := values[reference]; ok {
				return value, true
			}

			value, found, err := r.value(ctx, reference)
			switch {
			case err != nil:
				failed = err

				return "", false
			case !found:
				return "", false
			}

			values[reference] = value

			return value, true
		})
		switch {
		case failed != nil:
			return nil, failed
		case errors.Is(err, manifest.ErrUnknownSecret), errors.Is(err, manifest.ErrUnknownVariable):
			// Naming the environment variable as well as what it reads, so that an
			// operator has both ends of the reference that could not be resolved.
			return nil, fmt.Errorf("%s reads %w", key, err)
		case err != nil:
			return nil, fmt.Errorf("failed to resolve env %s: %w", key, err)
		}

		resolved[key] = expanded
	}

	return resolved, nil
}

// value reads what the reference names from whichever store holds that kind,
// reporting whether anything holds it.
//
// Nothing held under the name is a false rather than an error, so that expansion is
// what reports it. That keeps one description of an unresolved reference, whichever
// kind it named and wherever expansion was called from.
func (r *EnvResolver) value(ctx context.Context, reference manifest.Reference) (string, bool, error) {
	store := r.secrets
	missing := ErrSecretNotFound

	if reference.Kind == manifest.KindVariable {
		store = r.variables
		missing = ErrVariableNotFound
	}

	// A server holding no store of that kind holds nothing under the name, which is
	// the same answer as a name nobody created.
	if store == nil {
		return "", false, nil
	}

	value, err := store.Value(ctx, reference.Name)
	switch {
	case errors.Is(err, missing):
		return "", false, nil
	case err != nil:
		return "", false, err
	}

	return value, true, nil
}
