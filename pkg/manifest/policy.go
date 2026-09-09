package manifest

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type (
	// The Policy type describes the access-control policy: who may call the API,
	// and with which role.
	//
	// The document is applied whole. A grant absent from the document is revoked
	// on the next apply, so the file in a git repository is the complete answer
	// to who holds access.
	Policy struct {
		// The manifest schema version. Must be "v1".
		Version string `json:"version"`
		// How an OIDC identity becomes a principal and groups. Absent when
		// static tokens are the only authentication.
		OIDC *PolicyOIDC `json:"oidc,omitempty"`
		// Named collections of principals a grant can reference as one.
		Groups []PolicyGroup `json:"groups,omitempty"`
		// The bindings of roles to principals. An empty list grants nothing,
		// which is the state a fresh policy starts in.
		Grants []PolicyGrant `json:"grants,omitempty"`
	}

	// The PolicyOIDC type names the claims an OIDC identity token is read by.
	PolicyOIDC struct {
		// The claim whose value becomes the caller's principal name. By
		// convention this is "email", so a human principal is an email address
		// and cannot collide with a machine's bare name.
		PrincipalClaim string `yaml:"principalClaim" json:"principalClaim"`
		// The claim holding the caller's group names. Empty means the identity
		// carries no groups.
		GroupsClaim string `yaml:"groupsClaim,omitempty" json:"groupsClaim,omitempty"`
	}

	// The PolicyGroup type names a collection of principals defined in the
	// policy itself, as opposed to a group an identity provider asserts.
	PolicyGroup struct {
		// The name a grant references the group by, prefixed with "group:".
		Name string `json:"name"`
		// The principals the group contains.
		Members []string `json:"members"`
	}

	// The PolicyGrant type binds one role to a set of principals or groups.
	PolicyGrant struct {
		// The principals the grant applies to. An entry prefixed with "group:"
		// names a policy group or a group the identity provider asserts.
		Principals []string `json:"principals"`
		// The role the principals hold.
		Role Role `json:"role"`
	}

	// The Role type names one of the fixed roles a grant can bind.
	Role string
)

const (
	// RoleViewer reads everything.
	RoleViewer Role = "viewer"
	// RoleOperator drives workload, volume, service, variable and secret
	// lifecycle.
	RoleOperator Role = "operator"
	// RoleAdmin performs system-level operations: applying the policy document
	// and managing tokens.
	RoleAdmin Role = "admin"
)

// The prefix a grant's principal entry carries when it names a group rather
// than a principal.
const groupPrefix = "group:"

// ParsePolicy reads a policy manifest from r and returns the policy it
// describes.
//
// A policy manifest carries no kind field. Which resource a file describes is
// decided by what it is given to, so a file naming a workload's fields is
// reported as having unknown keys rather than being accepted as half a policy.
func ParsePolicy(r io.Reader) (Policy, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	if err := ValidatePolicy(policy); err != nil {
		return Policy{}, err
	}

	return policy, nil
}

// ValidatePolicy reports whether policy is a usable policy document.
//
// A grant may reference a group no identity provider has asserted yet and a
// principal nobody has authenticated as, because that is the onboarding order:
// merge the grant, then the person logs in. Only the document's own shape is
// validated here.
func ValidatePolicy(policy Policy) error {
	if err := validateVersion(policy.Version); err != nil {
		return err
	}

	if policy.OIDC != nil && policy.OIDC.PrincipalClaim == "" {
		return errors.New("invalid policy: oidc requires a principalClaim")
	}

	if err := validateGroups(policy.Groups); err != nil {
		return err
	}

	return validateGrants(policy.Grants)
}

// validateGroups reports whether every group has a usable name and members.
func validateGroups(groups []PolicyGroup) error {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		if group.Name == "" {
			return errors.New("invalid policy: a group requires a name")
		}

		if strings.Contains(group.Name, ":") {
			return fmt.Errorf("invalid policy: group name %q must not contain a colon, grants reference it as %q",
				group.Name, groupPrefix+group.Name)
		}

		if slices.Contains(names, group.Name) {
			return fmt.Errorf("invalid policy: group %q is defined more than once", group.Name)
		}

		if len(group.Members) == 0 {
			return fmt.Errorf("invalid policy: group %q has no members", group.Name)
		}

		if slices.Contains(group.Members, "") {
			return fmt.Errorf("invalid policy: group %q has an empty member", group.Name)
		}

		names = append(names, group.Name)
	}

	return nil
}

// validateGrants reports whether every grant binds a known role to at least one
// principal.
func validateGrants(grants []PolicyGrant) error {
	for _, grant := range grants {
		if len(grant.Principals) == 0 {
			return errors.New("invalid policy: a grant requires at least one principal")
		}

		for _, principal := range grant.Principals {
			if principal == "" || principal == groupPrefix {
				return errors.New("invalid policy: a grant names an empty principal")
			}
		}

		switch grant.Role {
		case RoleViewer, RoleOperator, RoleAdmin:
		default:
			return fmt.Errorf("invalid policy: role %q is not one of %s, %s or %s",
				grant.Role, RoleViewer, RoleOperator, RoleAdmin)
		}
	}

	return nil
}
