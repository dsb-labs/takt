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
		// Get should return the current policy and its tag.
		Get(ctx context.Context) (manifest.Policy, string, error)
		// Apply should validate the policy and replace the current document
		// with it, on the condition that ifMatch is the tag of the document
		// being replaced.
		Apply(ctx context.Context, policy manifest.Policy, ifMatch string) (manifest.Policy, string, error)
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
	policy, etag, err := a.policies.Get(ctx)
	if err != nil {
		return api.GetACLPolicy500JSONResponse{
			Error: internalError(a.logger, "get policy", err),
		}, nil
	}

	return api.GetACLPolicy200JSONResponse{
		Body:    api.GetACLPolicyResult{Policy: wire.FromPolicy(policy)},
		Headers: api.GetACLPolicy200ResponseHeaders{ETag: new(quoteETag(etag))},
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
	// lost-update on its way to happening.
	if request.Params.IfMatch == nil || *request.Params.IfMatch == "" {
		return api.ApplyACLPolicy400JSONResponse{
			Error: "the If-Match header is required, carrying the tag read from GET /api/v1/acl",
		}, nil
	}

	policy, etag, err := a.policies.Apply(ctx, wire.ToPolicy(*request.Body), unquoteETag(*request.Params.IfMatch))
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
		Body:    api.ApplyACLPolicyResult{Policy: wire.FromPolicy(policy)},
		Headers: api.ApplyACLPolicy200ResponseHeaders{ETag: new(quoteETag(etag))},
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

// quoteETag wraps a tag in the quotes the ETag header carries.
func quoteETag(etag string) string {
	return `"` + etag + `"`
}

// unquoteETag strips the quotes an If-Match header carries, accepting a bare
// tag too so a caller pasting the value by hand is not refused over quoting.
func unquoteETag(etag string) string {
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}

	return etag
}
