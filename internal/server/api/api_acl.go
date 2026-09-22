package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The PolicyService interface describes the policy operations the API
	// exposes.
	PolicyService interface {
		// Get should return the current policy, at version zero before any
		// apply.
		Get(ctx context.Context) (service.Policy, error)
		// Apply should validate the document and replace the current one with
		// it, on the condition that ifMatch is the version of the document
		// being replaced.
		Apply(ctx context.Context, policy manifest.Policy, ifMatch int) (service.Policy, error)
	}

	// The ACLInitializer interface describes how the API mints the recovery
	// token.
	ACLInitializer interface {
		// Init should mint the recovery token, exactly once.
		Init(ctx context.Context) (string, error)
	}

	// The ACLAPI type exposes HTTP endpoints for the policy document and its
	// one-shot init.
	ACLAPI struct {
		logger   *slog.Logger
		policies PolicyService
		init     ACLInitializer
	}

	// The ACLAPIConfig type contains fields used to construct an ACLAPI.
	ACLAPIConfig struct {
		// The logger used to record failures the response deliberately
		// doesn't describe.
		Logger *slog.Logger
		// The service owning the policy document.
		Policies PolicyService
		// The service minting the recovery token.
		Init ACLInitializer
	}
)

// NewACLAPI returns a new instance of the ACLAPI type.
func NewACLAPI(config ACLAPIConfig) *ACLAPI {
	return &ACLAPI{
		logger:   config.Logger.With("component", "api"),
		policies: config.Policies,
		init:     config.Init,
	}
}

// GetACLPolicy returns the current policy document and its tag.
func (a *ACLAPI) GetACLPolicy(ctx context.Context, _ api.GetACLPolicyRequestObject) (api.GetACLPolicyResponseObject, error) {
	policy, err := a.policies.Get(ctx)
	if err != nil {
		return api.GetACLPolicy500JSONResponse{
			Error: internalError(a.logger, "get policy", err),
		}, nil
	}

	return api.GetACLPolicy200JSONResponse{
		Body:    api.GetACLPolicyResult{Policy: wire.FromPolicy(policy.Spec)},
		Headers: api.GetACLPolicy200ResponseHeaders{ETag: new(versionETag(policy.Version))},
	}, nil
}

// ApplyACLPolicy replaces the policy document, on the condition the caller
// read the one it is replacing.
func (a *ACLAPI) ApplyACLPolicy(ctx context.Context, request api.ApplyACLPolicyRequestObject) (api.ApplyACLPolicyResponseObject, error) {
	if request.Body == nil {
		return api.ApplyACLPolicy400JSONResponse{
			Error: "request body is required",
		}, nil
	}

	// Declared optional in the document so its absence lands here rather
	// than in the router, and refused because an unconditional apply is a
	// lost-update on its way to happening. Zero is a tag here, unlike on a
	// named resource: it is what a get reports before the first apply, and
	// the first apply has to carry it back.
	ifMatch, present, ok := parseIfMatch(request.Params.IfMatch)
	switch {
	case !present:
		return api.ApplyACLPolicy400JSONResponse{
			Error: "the If-Match header is required, carrying the tag read from GET /api/v1/acl",
		}, nil
	case !ok:
		return api.ApplyACLPolicy400JSONResponse{
			Error: "the If-Match header must carry a tag read from GET /api/v1/acl",
		}, nil
	}

	policy, err := a.policies.Apply(ctx, wire.ToPolicy(*request.Body), ifMatch)
	switch {
	case errors.Is(err, service.ErrInvalidPolicy):
		return api.ApplyACLPolicy400JSONResponse{
			Error: err.Error(),
		}, nil
	case errors.Is(err, service.ErrPolicyChanged):
		return api.ApplyACLPolicy412JSONResponse{
			Error: "the policy changed since it was read",
		}, nil
	case err != nil:
		return api.ApplyACLPolicy500JSONResponse{
			Error: internalError(a.logger, "apply policy", err),
		}, nil
	}

	return api.ApplyACLPolicy200JSONResponse{
		Body:    api.ApplyACLPolicyResult{Policy: wire.FromPolicy(policy.Spec)},
		Headers: api.ApplyACLPolicy200ResponseHeaders{ETag: new(versionETag(policy.Version))},
	}, nil
}

// InitACL mints the recovery token, exactly once.
func (a *ACLAPI) InitACL(ctx context.Context, _ api.InitACLRequestObject) (api.InitACLResponseObject, error) {
	credential, err := a.init.Init(ctx)
	switch {
	case errors.Is(err, service.ErrACLInitialized):
		return api.InitACL409JSONResponse{
			Error: "acl is already initialized; write the reset file into the data directory to run init again",
		}, nil
	case err != nil:
		return api.InitACL500JSONResponse{
			Error: internalError(a.logger, "initialize acl", err),
		}, nil
	}

	return api.InitACL201JSONResponse{Credential: credential}, nil
}
