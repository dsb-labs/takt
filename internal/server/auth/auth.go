// Package auth provides the identity a request carries and the tokens that
// prove it.
//
// A token is random bytes the server stores only a hash of. An identity is
// what a token resolves to: a principal, the groups asserted for it, and the
// role the policy grants it. Everything above this package deals in
// identities, so how a credential becomes one is decided in exactly one place.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Identity type describes who a request comes from.
	//
	// The zero value is the anonymous identity: no credential was presented.
	Identity struct {
		// The name the caller authenticated as. Empty for the anonymous and
		// recovery identities.
		Principal string
		// The groups asserted for the principal when its token was minted.
		Groups []string
		// The role the policy grants the principal.
		Role manifest.Role
		// The identifier of the token that authenticated the request. Empty
		// when authentication is disabled or no credential was presented.
		TokenID string
		// Whether the request authenticated with the recovery token, which
		// sits above policy.
		Recovery bool
		// Whether authentication is disabled, in which case every request
		// holds the whole API as it did before the layer existed.
		Disabled bool
	}

	// The Kind type names the two types of token.
	Kind string
)

const (
	// KindRecovery is the token `takt acl init` prints once. It sits above
	// policy and exists for init and lockout recovery, not for daily use.
	KindRecovery Kind = "recovery"
	// KindClient is a token bound to a principal name and evaluated against
	// the policy on every request.
	KindClient Kind = "client"
)

const (
	// SessionCookie is the name of the cookie a login sets for the browser
	// UI. The cookie's value is a client token, so a session is revocable
	// like any other credential.
	SessionCookie = "takt_session"

	// TokenTTL is how long a token minted by a login lives. Static tokens
	// created with `takt token create` do not expire.
	TokenTTL = 12 * time.Hour

	// The prefixes a token string carries, which make a leaked token
	// greppable and let a credential that is not a takt token be refused
	// before a database lookup.
	recoveryPrefix = "takt_r_"
	clientPrefix   = "takt_c_"
)

// Tokens are encoded with the lowercase alphabet and no padding, so a token
// survives being typed, quoted and grepped without case or "=" surprises.
var tokenEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewToken returns a token of the given kind and the hash the server stores
// for it. The token itself is shown once and never stored.
func NewToken(kind Kind) (token, hash string, err error) {
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", "", fmt.Errorf("failed to generate token: %w", err)
	}

	prefix := clientPrefix
	if kind == KindRecovery {
		prefix = recoveryPrefix
	}

	token = prefix + tokenEncoding.EncodeToString(secret)

	return token, HashToken(token), nil
}

// HashToken returns the hash a token is stored and looked up by.
//
// A plain hash rather than a slow one, because the token is 256 bits of
// random data: there is nothing for a brute force to guess, and lookups
// happen on every request.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// KindOf reports which kind of token a credential claims to be. The second
// return value is false when the credential is not a takt token at all.
func KindOf(credential string) (Kind, bool) {
	switch {
	case strings.HasPrefix(credential, recoveryPrefix):
		return KindRecovery, true
	case strings.HasPrefix(credential, clientPrefix):
		return KindClient, true
	default:
		return "", false
	}
}

// The roles in order of what they contain, so a comparison answers whether
// one role covers another.
var roleRank = map[manifest.Role]int{
	manifest.RoleViewer:   1,
	manifest.RoleOperator: 2,
	manifest.RoleAdmin:    3,
}

// Allows reports whether a held role covers a required one. The roles are
// hierarchical: admin covers operator, operator covers viewer.
func Allows(held, required manifest.Role) bool {
	return roleRank[held] >= roleRank[required]
}

// Evaluate returns the role the policy grants a principal with the given
// asserted groups. The second return value is false when no grant matches.
//
// A grant matches when it names the principal directly, names a policy group
// the principal is a member of, or names a group the identity provider
// asserted for the principal. When several grants match, the highest role
// wins.
func Evaluate(policy manifest.Policy, principal string, groups []string) (manifest.Role, bool) {
	var (
		held    manifest.Role
		matched bool
	)

	for _, grant := range policy.Grants {
		if !grantMatches(policy, grant, principal, groups) {
			continue
		}

		if !matched || Allows(grant.Role, held) {
			held = grant.Role
		}

		matched = true
	}

	return held, matched
}

// grantMatches reports whether one grant applies to the principal.
func grantMatches(policy manifest.Policy, grant manifest.PolicyGrant, principal string, groups []string) bool {
	for _, entry := range grant.Principals {
		name, isGroup := strings.CutPrefix(entry, "group:")
		if !isGroup {
			if entry == principal {
				return true
			}

			continue
		}

		if slices.Contains(groups, name) {
			return true
		}

		for _, group := range policy.Groups {
			if group.Name == name && slices.Contains(group.Members, principal) {
				return true
			}
		}
	}

	return false
}
