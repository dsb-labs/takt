package manifest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestParseReferences(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Value     string
		Expected  []string
		ExpectErr error
	}{
		{
			Name:     "a whole value",
			Value:    "${secret:db-password}",
			Expected: []string{"db-password"},
		},
		{
			Name:     "inside a larger string",
			Value:    "postgres://app:${secret:db-password}@localhost:5432/app",
			Expected: []string{"db-password"},
		},
		{
			Name:     "several in one value",
			Value:    "${secret:user}:${secret:password}",
			Expected: []string{"user", "password"},
		},
		{
			Name:     "the same one twice",
			Value:    "${secret:token} ${secret:token}",
			Expected: []string{"token"},
		},
		{
			Name:  "a value referencing nothing",
			Value: "postgres://localhost/example",
		},
		{
			Name:  "an empty value",
			Value: "",
		},
		{
			Name:  "an escaped sigil",
			Value: "$$notasecret",
		},
		{
			Name:     "an escaped sigil beside a reference",
			Value:    "$$${secret:token}",
			Expected: []string{"token"},
		},
		{
			Name:  "an escaped reference",
			Value: "$${secret:token}",
		},
		{
			Name:  "a trailing escaped sigil",
			Value: "cost: 5$$",
		},
		{
			Name:      "an unterminated reference",
			Value:     "${secret:db-password",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a bare sigil",
			Value:     "$HOME",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a trailing sigil",
			Value:     "cost: 5$",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "another kind of reference",
			Value:     "${env:HOME}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a name that is not one a secret may have",
			Value:     "${secret:DB_PASSWORD}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "an empty name",
			Value:     "${secret:}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a name closed before it opens",
			Value:     "${secret}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a template a reader might expect to work",
			Value:     `{{ if .Debug }}on{{ end }}`,
			Expected:  nil,
			ExpectErr: nil,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			names, err := manifest.ParseReferences(tc.Value)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, names)
		})
	}
}

func TestExpand(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"db-password": "hunter2",
		"user":        "app",
		"empty":       "",
	}

	resolve := func(name string) (string, bool) {
		value, ok := values[name]

		return value, ok
	}

	tt := []struct {
		Name      string
		Value     string
		Expected  string
		ExpectErr error
	}{
		{
			Name:     "a whole value",
			Value:    "${secret:db-password}",
			Expected: "hunter2",
		},
		{
			Name:     "inside a larger string",
			Value:    "postgres://app:${secret:db-password}@localhost:5432/app",
			Expected: "postgres://app:hunter2@localhost:5432/app",
		},
		{
			Name:     "several in one value",
			Value:    "${secret:user}:${secret:db-password}",
			Expected: "app:hunter2",
		},
		{
			Name:     "a value referencing nothing",
			Value:    "postgres://localhost/example",
			Expected: "postgres://localhost/example",
		},
		{
			Name:     "an escaped sigil",
			Value:    "$$notasecret",
			Expected: "$notasecret",
		},
		{
			Name:     "an escaped sigil beside a reference",
			Value:    "$$${secret:user}",
			Expected: "$app",
		},
		{
			Name:  "an escaped reference",
			Value: "$${secret:user}",
			// Escaping the sigil is how a value that has to hold the reference text
			// itself says so, rather than being substituted.
			Expected: "${secret:user}",
		},
		{
			Name:     "a secret holding nothing",
			Value:    "prefix-${secret:empty}-suffix",
			Expected: "prefix--suffix",
		},
		{
			Name:      "a secret that does not exist",
			Value:     "${secret:nope}",
			ExpectErr: manifest.ErrUnknownSecret,
		},
		{
			Name:      "a malformed reference",
			Value:     "${secret:nope",
			ExpectErr: manifest.ErrInvalidReference,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			expanded, err := manifest.Expand(tc.Value, resolve)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, expanded)
		})
	}
}

func TestReferences(t *testing.T) {
	t.Parallel()

	t.Run("names every secret the environment references", func(t *testing.T) {
		names, err := manifest.References(manifest.Spec{
			Env: map[string]string{
				"DSN":     "postgres://app:${secret:db-password}@localhost/app",
				"TOKEN":   "${secret:api-token}",
				"LITERAL": "nothing here",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"api-token", "db-password"}, names)
	})

	t.Run("returns them in a stable order", func(t *testing.T) {
		spec := manifest.Spec{
			Env: map[string]string{
				"A": "${secret:zeta}",
				"B": "${secret:alpha}",
				"C": "${secret:mu}",
			},
		}

		// These names reach the hash of a specification, so an order that followed map
		// iteration would make an unchanged workload hash differently each apply.
		first, err := manifest.References(spec)
		require.NoError(t, err)

		for range 10 {
			again, err := manifest.References(spec)
			require.NoError(t, err)
			assert.Equal(t, first, again)
		}
	})

	t.Run("ignores a reference in a key", func(t *testing.T) {
		// A key names an environment variable rather than something a workload reads,
		// so there is nothing to substitute into.
		names, err := manifest.References(manifest.Spec{
			Env: map[string]string{"${secret:db-password}": "literal"},
		})
		require.NoError(t, err)
		assert.Empty(t, names)
	})

	t.Run("names the variable holding a bad reference", func(t *testing.T) {
		_, err := manifest.References(manifest.Spec{
			Env: map[string]string{"DSN": "${secret:unterminated"},
		})
		require.ErrorIs(t, err, manifest.ErrInvalidReference)
		assert.Contains(t, err.Error(), "DSN")
	})

	t.Run("returns nothing for a workload with no environment", func(t *testing.T) {
		names, err := manifest.References(manifest.Spec{})
		require.NoError(t, err)
		assert.Empty(t, names)
	})
}
