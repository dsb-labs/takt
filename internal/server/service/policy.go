package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrPolicyChanged is returned when the policy an apply was conditioned
	// on is no longer the policy the server holds.
	ErrPolicyChanged = errors.New("policy changed since it was read")
	// ErrInvalidPolicy is returned when a policy document fails validation.
	ErrInvalidPolicy = errors.New("invalid policy")
)

type (
	// The PolicyRepository interface describes the persistence operations the
	// policy service uses.
	PolicyRepository interface {
		// Get should return the applied policy.
		Get(ctx context.Context) (database.Policy, error)
		// Apply should replace the policy with the given document, on the
		// condition that the stored version is still ifMatch, where zero
		// names the state before any apply. An unchanged document should be
		// left as it is, version included.
		Apply(ctx context.Context, document []byte, ifMatch int) (database.Policy, error)
	}

	// The Policy type is the service's view of the policy: the document that
	// was applied, together with the version the server counts for it.
	Policy struct {
		// The document as applied. Before any apply it is the empty document:
		// version v1, no grants, which grants nothing to anyone.
		Spec manifest.Policy
		// How many applies have changed the document, which the API reports
		// as its entity tag. Zero before any apply.
		Version int
		// The time the document was last applied. Zero before any apply.
		UpdatedAt time.Time
	}

	// The PolicyService type owns the access-control policy document.
	PolicyService struct {
		logger   *slog.Logger
		policies PolicyRepository
	}

	// The PolicyServiceConfig type contains fields used to construct a
	// PolicyService.
	PolicyServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the policy.
		Policies PolicyRepository
	}
)

// NewPolicyService returns a new instance of the PolicyService type.
func NewPolicyService(config PolicyServiceConfig) *PolicyService {
	return &PolicyService{
		logger:   config.Logger.With("component", "service"),
		policies: config.Policies,
	}
}

// Get returns the current policy. Before any apply, it is the empty document
// at version zero, which is the tag a first apply carries back.
func (s *PolicyService) Get(ctx context.Context) (Policy, error) {
	stored, err := s.policies.Get(ctx)
	switch {
	case errors.Is(err, database.ErrNoPolicy):
		return Policy{Spec: manifest.Policy{Version: "v1"}}, nil
	case err != nil:
		return Policy{}, err
	}

	return hydratePolicy(stored)
}

// Apply validates the document and replaces the current one with it, on the
// condition that ifMatch is the version of the document being replaced, where
// zero is the empty policy no apply has yet replaced. The whole document
// replaces, so a grant absent from it is revoked.
//
// Returns the policy as applied, which is what a get would now report. An
// apply that changes nothing leaves the version where it is.
func (s *PolicyService) Apply(ctx context.Context, policy manifest.Policy, ifMatch int) (Policy, error) {
	if err := manifest.ValidatePolicy(policy); err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}

	document, err := json.Marshal(policy)
	if err != nil {
		return Policy{}, fmt.Errorf("failed to encode policy: %w", err)
	}

	stored, err := s.policies.Apply(ctx, document, ifMatch)
	switch {
	case errors.Is(err, database.ErrPolicyChanged):
		return Policy{}, ErrPolicyChanged
	case err != nil:
		return Policy{}, err
	}

	s.logger.With("version", stored.Version, "grants", len(policy.Grants)).Info("policy applied")

	return hydratePolicy(stored)
}

// hydratePolicy decodes a stored policy into the service's view of it.
func hydratePolicy(stored database.Policy) (Policy, error) {
	var spec manifest.Policy
	if err := json.Unmarshal(stored.Document, &spec); err != nil {
		return Policy{}, fmt.Errorf("failed to decode the stored policy: %w", err)
	}

	return Policy{Spec: spec, Version: stored.Version, UpdatedAt: stored.UpdatedAt}, nil
}
