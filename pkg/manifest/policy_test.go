package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestParsePolicy(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Expected     manifest.Policy
		ExpectsError bool
	}{
		{
			Name: "the sample policy document",
			File: "policy/policy.yaml",
			Expected: manifest.Policy{
				Version: "v1",
				OIDC: &manifest.PolicyOIDC{
					PrincipalClaim: "email",
					GroupsClaim:    "groups",
				},
				Groups: []manifest.PolicyGroup{
					{Name: "infra", Members: []string{"david@dsb.dev", "ci"}},
				},
				Grants: []manifest.PolicyGrant{
					{Principals: []string{"group:infra"}, Role: manifest.RoleOperator},
					{Principals: []string{"david@dsb.dev"}, Role: manifest.RoleAdmin},
					{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
				},
			},
		},
		{
			// A fresh policy grants nothing, which is the state `takt acl init`
			// leaves behind.
			Name:     "a policy with no grants",
			File:     "policy/policy_minimal.yaml",
			Expected: manifest.Policy{Version: "v1"},
		},
		{
			Name:         "rejects a schema version it does not understand",
			File:         "policy/policy_bad_version.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects an oidc block without a principal claim",
			File:         "policy/policy_no_principal_claim.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a role that is not one of the three",
			File:         "policy/policy_bad_role.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a grant with no principals",
			File:         "policy/policy_empty_grant.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a group defined more than once",
			File:         "policy/policy_dup_group.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a group with no members",
			File:         "policy/policy_group_no_members.yaml",
			ExpectsError: true,
		},
		{
			// Grants reference a group as "group:<name>", so a colon in the name
			// itself would make the reference ambiguous.
			Name:         "rejects a group name containing a colon",
			File:         "policy/policy_group_colon.yaml",
			ExpectsError: true,
		},
		{
			// A key this package does not know is a misunderstanding worth
			// reporting rather than ignoring.
			Name:         "rejects an unknown field",
			File:         "policy/policy_unknown_field.yaml",
			ExpectsError: true,
		},
		{
			// Which resource a file describes is decided by what it is given to, so
			// a workload manifest handed to this reads as unknown keys.
			Name:         "rejects a workload manifest",
			File:         "workload/container.yaml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.File))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			policy, err := manifest.ParsePolicy(f)
			if tc.ExpectsError {
				assert.Error(t, err)
				assert.Zero(t, policy)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, policy)
		})
	}
}
