package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestNewToken(t *testing.T) {
	t.Parallel()

	t.Run("a client token", func(t *testing.T) {
		token, hash, err := auth.NewToken(auth.KindClient)
		require.NoError(t, err)

		kind, ok := auth.KindOf(token)
		assert.True(t, ok)
		assert.Equal(t, auth.KindClient, kind)
		assert.Equal(t, auth.HashToken(token), hash)
	})

	t.Run("a recovery token", func(t *testing.T) {
		token, hash, err := auth.NewToken(auth.KindRecovery)
		require.NoError(t, err)

		kind, ok := auth.KindOf(token)
		assert.True(t, ok)
		assert.Equal(t, auth.KindRecovery, kind)
		assert.Equal(t, auth.HashToken(token), hash)
	})

	t.Run("two tokens differ", func(t *testing.T) {
		first, _, err := auth.NewToken(auth.KindClient)
		require.NoError(t, err)

		second, _, err := auth.NewToken(auth.KindClient)
		require.NoError(t, err)

		assert.NotEqual(t, first, second)
	})

	t.Run("a credential that is not a token", func(t *testing.T) {
		_, ok := auth.KindOf("Bearer something")
		assert.False(t, ok)
	})
}

func TestAllows(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Held     manifest.Role
		Required manifest.Role
		Expected bool
	}{
		{Name: "viewer covers viewer", Held: manifest.RoleViewer, Required: manifest.RoleViewer, Expected: true},
		{Name: "viewer does not cover operator", Held: manifest.RoleViewer, Required: manifest.RoleOperator, Expected: false},
		{Name: "viewer does not cover admin", Held: manifest.RoleViewer, Required: manifest.RoleAdmin, Expected: false},
		{Name: "operator covers viewer", Held: manifest.RoleOperator, Required: manifest.RoleViewer, Expected: true},
		{Name: "operator does not cover admin", Held: manifest.RoleOperator, Required: manifest.RoleAdmin, Expected: false},
		{Name: "admin covers everything", Held: manifest.RoleAdmin, Required: manifest.RoleViewer, Expected: true},
		{Name: "no role covers nothing", Held: "", Required: manifest.RoleViewer, Expected: false},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.Expected, auth.Allows(tc.Held, tc.Required))
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	policy := manifest.Policy{
		Version: "v1",
		Groups: []manifest.PolicyGroup{
			{Name: "infra", Members: []string{"david@dsb.dev", "ci"}},
		},
		Grants: []manifest.PolicyGrant{
			{Principals: []string{"group:infra"}, Role: manifest.RoleOperator},
			{Principals: []string{"david@dsb.dev"}, Role: manifest.RoleAdmin},
			{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
			{Principals: []string{"group:sre"}, Role: manifest.RoleOperator},
		},
	}

	tt := []struct {
		Name      string
		Principal string
		Groups    []string
		Expected  manifest.Role
		Matched   bool
	}{
		{
			Name:      "a direct grant",
			Principal: "prometheus",
			Expected:  manifest.RoleViewer,
			Matched:   true,
		},
		{
			Name:      "membership of a policy group",
			Principal: "ci",
			Expected:  manifest.RoleOperator,
			Matched:   true,
		},
		{
			// david matches both the infra group's operator grant and a direct
			// admin grant, and the highest role wins.
			Name:      "the highest matching role wins",
			Principal: "david@dsb.dev",
			Expected:  manifest.RoleAdmin,
			Matched:   true,
		},
		{
			// The sre group is defined nowhere in the policy, so membership
			// comes from what the identity provider asserted at login.
			Name:      "a group the identity provider asserted",
			Principal: "someone@dsb.dev",
			Groups:    []string{"sre"},
			Expected:  manifest.RoleOperator,
			Matched:   true,
		},
		{
			Name:      "a principal nothing grants to",
			Principal: "stranger",
			Matched:   false,
		},
		{
			// An asserted group only matches a "group:" entry, so a principal
			// cannot be granted to by naming its group as a bare principal.
			Name:      "an asserted group does not match a bare principal entry",
			Principal: "someone@dsb.dev",
			Groups:    []string{"prometheus"},
			Matched:   false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			role, matched := auth.Evaluate(policy, tc.Principal, tc.Groups)

			assert.Equal(t, tc.Matched, matched)
			assert.Equal(t, tc.Expected, role)
		})
	}
}
