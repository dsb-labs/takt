package wire

import (
	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// ToPolicy maps a wire policy document onto the canonical shape.
func ToPolicy(spec api.PolicySpec) manifest.Policy {
	out := manifest.Policy{
		Version: spec.Version,
	}

	if spec.Oidc != nil {
		out.OIDC = &manifest.PolicyOIDC{
			PrincipalClaim: spec.Oidc.PrincipalClaim,
		}

		if spec.Oidc.GroupsClaim != nil {
			out.OIDC.GroupsClaim = *spec.Oidc.GroupsClaim
		}
	}

	if spec.Groups != nil {
		out.Groups = make([]manifest.PolicyGroup, 0, len(*spec.Groups))
		for _, group := range *spec.Groups {
			out.Groups = append(out.Groups, manifest.PolicyGroup{
				Name:    group.Name,
				Members: group.Members,
			})
		}
	}

	if spec.Grants != nil {
		out.Grants = make([]manifest.PolicyGrant, 0, len(*spec.Grants))
		for _, grant := range *spec.Grants {
			out.Grants = append(out.Grants, manifest.PolicyGrant{
				Principals: grant.Principals,
				Role:       manifest.Role(grant.Role),
			})
		}
	}

	return out
}

// FromPolicy maps a canonical policy document onto the wire shape.
//
// Empty optional values are sent as absent rather than as empty ones, matching
// what FromSpec does for a workload.
func FromPolicy(policy manifest.Policy) api.PolicySpec {
	out := api.PolicySpec{
		Version: policy.Version,
	}

	if policy.OIDC != nil {
		oidc := api.PolicyOIDC{PrincipalClaim: policy.OIDC.PrincipalClaim}
		if policy.OIDC.GroupsClaim != "" {
			oidc.GroupsClaim = new(policy.OIDC.GroupsClaim)
		}

		out.Oidc = &oidc
	}

	if len(policy.Groups) > 0 {
		groups := make([]api.PolicyGroup, 0, len(policy.Groups))
		for _, group := range policy.Groups {
			groups = append(groups, api.PolicyGroup{
				Name:    group.Name,
				Members: group.Members,
			})
		}

		out.Groups = &groups
	}

	if len(policy.Grants) > 0 {
		grants := make([]api.PolicyGrant, 0, len(policy.Grants))
		for _, grant := range policy.Grants {
			grants = append(grants, api.PolicyGrant{
				Principals: grant.Principals,
				Role:       api.Role(grant.Role),
			})
		}

		out.Grants = &grants
	}

	return out
}
