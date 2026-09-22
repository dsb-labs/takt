package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Policy type is the client-side view of the policy: the document the
	// server holds, together with the tag a conditional apply hands back.
	//
	// The document is nested rather than flattened so that what this carries
	// beside it can never be mistaken for part of it. A file an operator keeps
	// holds only the Spec.
	Policy struct {
		// The canonical current document. Before any apply it is the empty
		// document: version v1, no grants, which grants nothing to anyone.
		Spec manifest.Policy
		// The entity tag identifying this version of the document, exactly as
		// the ETag header carried it, which ApplyPolicy presents back.
		ETag string
	}
)

// InitACL mints the recovery token and returns it. It works exactly once:
// ErrACLInitialized is reported for as long as a recovery token exists, and
// only the reset file in the server's data directory makes it work again.
func (c *Client) InitACL(ctx context.Context) (string, error) {
	resp, err := c.api.InitACLWithResponse(ctx, api.InitACLRequest{})
	if err != nil {
		return "", fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		return resp.JSON201.Credential, nil
	case resp.JSON409 != nil:
		return "", fmt.Errorf("%s: %w", resp.JSON409.Error, ErrACLInitialized)
	case resp.JSON500 != nil:
		return "", newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return "", newError(resp.StatusCode(), nil)
	}
}

// GetPolicy returns the canonical current policy document and the tag a
// conditional apply presents back.
func (c *Client) GetPolicy(ctx context.Context) (Policy, error) {
	resp, err := c.api.GetACLPolicyWithResponse(ctx)
	if err != nil {
		return Policy{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Policy{Spec: wire.ToPolicy(resp.JSON200.Policy), ETag: resp.HTTPResponse.Header.Get("ETag")}, nil
	case resp.JSON500 != nil:
		return Policy{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Policy{}, newError(resp.StatusCode(), nil)
	}
}

// ApplyPolicy replaces the whole policy with the given document, on the
// condition that the policy still carries the tag WithIfMatch names. A stale
// tag is reported as ErrPolicyChanged rather than silently clobbering a
// concurrent apply: read the policy again and re-apply.
//
// Without WithIfMatch the tag is read first and the apply conditioned on it,
// which is the one-step form `takt acl apply` uses. The apply is never
// unconditional, unlike a resource apply without the option. The policy
// always exists, so there is always a tag to name, and an apply that named
// none would be a lost update on its way to happening.
//
// Returns the policy as applied, carrying its new tag.
func (c *Client) ApplyPolicy(ctx context.Context, policy manifest.Policy, options ...ApplyOption) (Policy, error) {
	if err := manifest.ValidatePolicy(policy); err != nil {
		return Policy{}, err
	}

	etag := ifMatch(options)
	if etag == nil {
		current, err := c.GetPolicy(ctx)
		if err != nil {
			return Policy{}, err
		}

		etag = new(current.ETag)
	}

	params := api.ApplyACLPolicyParams{IfMatch: etag}

	resp, err := c.api.ApplyACLPolicyWithResponse(ctx, &params, wire.FromPolicy(policy))
	if err != nil {
		return Policy{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Policy{Spec: wire.ToPolicy(resp.JSON200.Policy), ETag: resp.HTTPResponse.Header.Get("ETag")}, nil
	case resp.JSON400 != nil:
		return Policy{}, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON412 != nil:
		return Policy{}, fmt.Errorf("%s: %w", resp.JSON412.Error, ErrPolicyChanged)
	case resp.JSON500 != nil:
		return Policy{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Policy{}, newError(resp.StatusCode(), nil)
	}
}
