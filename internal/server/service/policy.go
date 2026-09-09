package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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
		// Apply should replace the policy with the given document and tag, on
		// the condition that the stored tag still equals previousETag. An
		// empty previousETag means no policy is expected to exist yet.
		Apply(ctx context.Context, document []byte, etag, previousETag string) error
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

// Get returns the current policy and the tag a conditional apply presents
// back. Before any apply, the policy is the empty document: version v1, no
// grants, which grants nothing to anyone.
func (s *PolicyService) Get(ctx context.Context) (manifest.Policy, string, error) {
	stored, err := s.policies.Get(ctx)
	switch {
	case errors.Is(err, database.ErrNoPolicy):
		return emptyPolicy()
	case err != nil:
		return manifest.Policy{}, "", err
	}

	var policy manifest.Policy
	if err = json.Unmarshal(stored.Document, &policy); err != nil {
		return manifest.Policy{}, "", fmt.Errorf("failed to decode the stored policy: %w", err)
	}

	return policy, stored.ETag, nil
}

// Apply validates the policy and replaces the current document with it, on
// the condition that ifMatch is the tag of the document being replaced. The
// whole document replaces, so a grant absent from it is revoked.
//
// Returns the applied policy and its new tag, which is what a get would now
// report.
func (s *PolicyService) Apply(ctx context.Context, policy manifest.Policy, ifMatch string) (manifest.Policy, string, error) {
	if err := manifest.ValidatePolicy(policy); err != nil {
		return manifest.Policy{}, "", fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}

	document, err := json.Marshal(policy)
	if err != nil {
		return manifest.Policy{}, "", fmt.Errorf("failed to encode policy: %w", err)
	}

	// The empty document is never stored, so its tag maps back to the "no row
	// yet" condition the repository expresses as an empty previous tag.
	previous := ifMatch
	if _, emptyETag, emptyErr := emptyPolicy(); emptyErr == nil && ifMatch == emptyETag {
		previous = ""
	}

	etag := policyETag(document)

	err = s.policies.Apply(ctx, document, etag, previous)
	switch {
	case errors.Is(err, database.ErrPolicyChanged):
		return manifest.Policy{}, "", ErrPolicyChanged
	case err != nil:
		return manifest.Policy{}, "", err
	}

	s.logger.With("etag", etag, "grants", len(policy.Grants)).Info("policy applied")

	return policy, etag, nil
}

// emptyPolicy returns the policy a server holds before any apply, with the
// tag derived from it the same way an applied document's is.
func emptyPolicy() (manifest.Policy, string, error) {
	policy := manifest.Policy{Version: "v1"}

	document, err := json.Marshal(policy)
	if err != nil {
		return manifest.Policy{}, "", fmt.Errorf("failed to encode the empty policy: %w", err)
	}

	return policy, policyETag(document), nil
}

// policyETag derives the tag for a canonical policy document. Derived rather
// than counted, so a pipeline reading the policy twice sees the same tag.
func policyETag(document []byte) string {
	sum := sha256.Sum256(document)

	return hex.EncodeToString(sum[:])
}
