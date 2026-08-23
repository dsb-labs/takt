package manifest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestParseReferences(t *testing.T) {
	t.Parallel()

	secret := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindSecret, Name: name}
	}

	variable := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindVariable, Name: name}
	}

	tt := []struct {
		Name      string
		Value     string
		Expected  []manifest.Reference
		ExpectErr error
	}{
		{
			Name:     "a whole value",
			Value:    "${secret:db-password}",
			Expected: []manifest.Reference{secret("db-password")},
		},
		{
			Name:     "a whole value referencing a variable",
			Value:    "${var:log-level}",
			Expected: []manifest.Reference{variable("log-level")},
		},
		{
			Name:     "inside a larger string",
			Value:    "postgres://app:${secret:db-password}@localhost:5432/app",
			Expected: []manifest.Reference{secret("db-password")},
		},
		{
			Name:     "a variable inside a larger string",
			Value:    "postgres://app@${var:db-host}:5432/app",
			Expected: []manifest.Reference{variable("db-host")},
		},
		{
			Name:     "several in one value",
			Value:    "${secret:user}:${secret:password}",
			Expected: []manifest.Reference{secret("user"), secret("password")},
		},
		{
			Name:  "both kinds in one value",
			Value: "postgres://app:${secret:db-password}@${var:db-host}/app",
			Expected: []manifest.Reference{
				secret("db-password"),
				variable("db-host"),
			},
		},
		{
			Name:     "the same one twice",
			Value:    "${secret:token} ${secret:token}",
			Expected: []manifest.Reference{secret("token")},
		},
		{
			Name:     "the same variable twice",
			Value:    "${var:region} ${var:region}",
			Expected: []manifest.Reference{variable("region")},
		},
		{
			Name:  "a secret and a variable sharing a name",
			Value: "${secret:token} ${var:token}",
			// Two references rather than one. The kinds resolve from different places,
			// so a name held by both names two different things.
			Expected: []manifest.Reference{secret("token"), variable("token")},
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
			Expected: []manifest.Reference{secret("token")},
		},
		{
			Name:     "an escaped sigil beside a variable reference",
			Value:    "$$${var:token}",
			Expected: []manifest.Reference{variable("token")},
		},
		{
			Name:  "an escaped reference",
			Value: "$${secret:token}",
		},
		{
			Name:  "an escaped variable reference",
			Value: "$${var:token}",
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
			Name:      "an unterminated variable reference",
			Value:     "${var:log-level",
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
			Name:      "a name that is not one a variable may have",
			Value:     "${var:LOG_LEVEL}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "an empty name",
			Value:     "${secret:}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "an empty variable name",
			Value:     "${var:}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a name closed before it opens",
			Value:     "${secret}",
			ExpectErr: manifest.ErrInvalidReference,
		},
		{
			Name:      "a variable closed before it opens",
			Value:     "${var}",
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
			references, err := manifest.ParseReferences(tc.Value)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, references)
		})
	}
}

func TestParseReferences_NamesBothFormsInAnError(t *testing.T) {
	t.Parallel()

	// An operator who wrote something that is not a reference has to be told what one
	// looks like, and both kinds are equally likely to have been meant.
	_, err := manifest.ParseReferences("${env:HOME}")
	require.ErrorIs(t, err, manifest.ErrInvalidReference)
	assert.Contains(t, err.Error(), "${secret:name}")
	assert.Contains(t, err.Error(), "${var:name}")
}

func TestExpand(t *testing.T) {
	t.Parallel()

	secrets := map[string]string{
		"db-password": "hunter2",
		"user":        "app",
		"empty":       "",
	}

	variables := map[string]string{
		"db-host":   "localhost",
		"log-level": "debug",
		"blank":     "",
	}

	resolve := func(reference manifest.Reference) (string, bool) {
		if reference.Kind == manifest.KindVariable {
			value, ok := variables[reference.Name]

			return value, ok
		}

		value, ok := secrets[reference.Name]

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
			Name:     "a whole value referencing a variable",
			Value:    "${var:log-level}",
			Expected: "debug",
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
			Name:     "both kinds in one value",
			Value:    "postgres://${secret:user}:${secret:db-password}@${var:db-host}/app",
			Expected: "postgres://app:hunter2@localhost/app",
		},
		{
			Name:  "a secret and a variable sharing a name",
			Value: "${secret:empty}${var:blank}",
			// Both hold nothing, so this proves only that neither resolved against the
			// other's map. The kinds are told apart in TestExpand_TellsTheKindsApart.
			Expected: "",
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
			Name:     "an escaped variable reference",
			Value:    "$${var:log-level}",
			Expected: "${var:log-level}",
		},
		{
			Name:     "a secret holding nothing",
			Value:    "prefix-${secret:empty}-suffix",
			Expected: "prefix--suffix",
		},
		{
			Name:     "a variable holding nothing",
			Value:    "prefix-${var:blank}-suffix",
			Expected: "prefix--suffix",
		},
		{
			Name:      "a secret that does not exist",
			Value:     "${secret:nope}",
			ExpectErr: manifest.ErrUnknownSecret,
		},
		{
			Name:      "a variable that does not exist",
			Value:     "${var:nope}",
			ExpectErr: manifest.ErrUnknownVariable,
		},
		{
			Name:  "a variable named after a secret that exists",
			Value: "${var:db-password}",
			// The kind is part of what is being asked for, so a variable is not
			// satisfied by a secret of the same name.
			ExpectErr: manifest.ErrUnknownVariable,
		},
		{
			Name:      "a secret named after a variable that exists",
			Value:     "${secret:log-level}",
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

func TestExpand_TellsTheKindsApart(t *testing.T) {
	t.Parallel()

	// A name held by both a secret and a variable resolves to a different value for
	// each, so the callback is told which one is being asked for rather than having to
	// guess from the name.
	expanded, err := manifest.Expand("${secret:token}/${var:token}", func(reference manifest.Reference) (string, bool) {
		if reference.Kind == manifest.KindVariable {
			return "public", true
		}

		return "private", true
	})
	require.NoError(t, err)
	assert.Equal(t, "private/public", expanded)
}

func TestReferences(t *testing.T) {
	t.Parallel()

	secret := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindSecret, Name: name}
	}

	variable := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindVariable, Name: name}
	}

	t.Run("names every secret the environment references", func(t *testing.T) {
		references, err := manifest.References(manifest.Spec{
			Env: map[string]string{
				"DSN":     "postgres://app:${secret:db-password}@localhost/app",
				"TOKEN":   "${secret:api-token}",
				"LITERAL": "nothing here",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{secret("api-token"), secret("db-password")}, references)
	})

	t.Run("names every variable the environment references", func(t *testing.T) {
		references, err := manifest.References(manifest.Spec{
			Env: map[string]string{
				"HOST":  "${var:db-host}",
				"LEVEL": "${var:log-level}",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{variable("db-host"), variable("log-level")}, references)
	})

	t.Run("groups the kinds together", func(t *testing.T) {
		references, err := manifest.References(manifest.Spec{
			Env: map[string]string{
				"A": "${var:zeta}",
				"B": "${secret:alpha}",
				"C": "${var:mu}",
				"D": "${secret:omega}",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{
			secret("alpha"),
			secret("omega"),
			variable("mu"),
			variable("zeta"),
		}, references)
	})

	t.Run("returns them in a stable order", func(t *testing.T) {
		spec := manifest.Spec{
			Env: map[string]string{
				"A": "${secret:zeta}",
				"B": "${var:alpha}",
				"C": "${secret:mu}",
				"D": "${var:omega}",
			},
		}

		// These references reach the hash of a specification, so an order that followed
		// map iteration would make an unchanged workload hash differently each apply.
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
		references, err := manifest.References(manifest.Spec{
			Env: map[string]string{"${secret:db-password}": "literal"},
		})
		require.NoError(t, err)
		assert.Empty(t, references)
	})

	t.Run("names the variable holding a bad reference", func(t *testing.T) {
		_, err := manifest.References(manifest.Spec{
			Env: map[string]string{"DSN": "${secret:unterminated"},
		})
		require.ErrorIs(t, err, manifest.ErrInvalidReference)
		assert.Contains(t, err.Error(), "DSN")
	})

	t.Run("returns nothing for a workload with no environment", func(t *testing.T) {
		references, err := manifest.References(manifest.Spec{})
		require.NoError(t, err)
		assert.Empty(t, references)
	})

	t.Run("names what a mount reads", func(t *testing.T) {
		// A mounted value is the same thing an env value references, so it is recorded
		// as read: that is what makes deleting one report the workloads holding it.
		references, err := manifest.References(manifest.Spec{
			Volumes: []manifest.VolumeMount{
				{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
				{Var: "app-config", To: "/etc/app/config.json"},
				{Name: "example-data", To: "/var/lib/example"},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{secret("tls-cert"), variable("app-config")}, references)
	})

	t.Run("names what a mount reads whatever its delivery mode", func(t *testing.T) {
		// Naming a signal changes how a change is delivered, not whether the workload
		// reads the value.
		references, err := manifest.References(manifest.Spec{
			Volumes: []manifest.VolumeMount{
				{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{secret("tls-cert")}, references)
	})

	t.Run("counts a value read from both places once", func(t *testing.T) {
		references, err := manifest.References(manifest.Spec{
			Env:     map[string]string{"CERT": "${secret:tls-cert}"},
			Volumes: []manifest.VolumeMount{{Secret: "tls-cert", To: "/etc/tls/cert.pem"}},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{secret("tls-cert")}, references)
	})
}

func TestRefreshed(t *testing.T) {
	t.Parallel()

	secret := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindSecret, Name: name}
	}

	variable := func(name string) manifest.Reference {
		return manifest.Reference{Kind: manifest.KindVariable, Name: name}
	}

	t.Run("names what only a signalling mount reads", func(t *testing.T) {
		// These are the references whose value must stay out of the hash, or the
		// instance would be replaced rather than signalled.
		refreshed, err := manifest.Refreshed(manifest.Spec{
			Volumes: []manifest.VolumeMount{
				{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
				{Var: "app-config", To: "/etc/app/config.json", Signal: manifest.SignalUSR1},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []manifest.Reference{secret("tls-cert"), variable("app-config")}, refreshed)
	})

	t.Run("ignores a mount that asked to be replaced", func(t *testing.T) {
		refreshed, err := manifest.Refreshed(manifest.Spec{
			Volumes: []manifest.VolumeMount{{Secret: "tls-cert", To: "/etc/tls/cert.pem"}},
		})
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("ignores a value the environment also reads", func(t *testing.T) {
		// An environment variable is fixed once a process has started, so a workload
		// reading the value there has to be replaced to see a change. Refreshing the
		// file it also mounts happens anyway and costs nothing.
		refreshed, err := manifest.Refreshed(manifest.Spec{
			Env: map[string]string{"CERT": "${secret:tls-cert}"},
			Volumes: []manifest.VolumeMount{
				{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
			},
		})
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("ignores a value another mount asked to be replaced for", func(t *testing.T) {
		// One mount asked to be replaced, so the workload is replaced — and a hash that
		// did not move would leave that mount's file stale.
		refreshed, err := manifest.Refreshed(manifest.Spec{
			Volumes: []manifest.VolumeMount{
				{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
				{Secret: "tls-cert", To: "/etc/other/cert.pem"},
			},
		})
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("returns nothing for a workload that mounts no value", func(t *testing.T) {
		refreshed, err := manifest.Refreshed(manifest.Spec{
			Env:     map[string]string{"CERT": "${secret:tls-cert}"},
			Volumes: []manifest.VolumeMount{{Name: "example-data", To: "/var/lib/example"}},
		})
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})
}

func TestKindOf(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Mount     manifest.VolumeMount
		Expected  manifest.MountKind
		ExpectErr error
	}{
		{
			Name:     "a mounted volume",
			Mount:    manifest.VolumeMount{Name: "example-data", To: "/var/lib/example"},
			Expected: manifest.MountVolume,
		},
		{
			Name:     "a mounted secret",
			Mount:    manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			Expected: manifest.MountSecret,
		},
		{
			Name:     "a mounted variable",
			Mount:    manifest.VolumeMount{Var: "app-config", To: "/etc/app/config.json"},
			Expected: manifest.MountVariable,
		},
		{
			Name:      "no source at all",
			Mount:     manifest.VolumeMount{To: "/etc/tls/cert.pem"},
			ExpectErr: manifest.ErrNoMountSource,
		},
		{
			Name:      "two sources",
			Mount:     manifest.VolumeMount{Secret: "tls-cert", Var: "app-config", To: "/etc/tls/cert.pem"},
			ExpectErr: manifest.ErrAmbiguousMountSource,
		},
		{
			Name: "three sources",
			Mount: manifest.VolumeMount{
				Name:   "example-data",
				Secret: "tls-cert",
				Var:    "app-config",
				To:     "/etc/tls/cert.pem",
			},
			ExpectErr: manifest.ErrAmbiguousMountSource,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			kind, err := manifest.KindOf(tc.Mount)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				assert.Empty(t, kind)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, kind)
		})
	}
}

func TestVolumeMount_Reference(t *testing.T) {
	t.Parallel()

	t.Run("reports what a mounted secret reads", func(t *testing.T) {
		reference, ok := manifest.VolumeMount{Secret: "tls-cert"}.Reference()
		require.True(t, ok)
		assert.Equal(t, manifest.Reference{Kind: manifest.KindSecret, Name: "tls-cert"}, reference)
	})

	t.Run("reports what a mounted variable reads", func(t *testing.T) {
		reference, ok := manifest.VolumeMount{Var: "app-config"}.Reference()
		require.True(t, ok)
		assert.Equal(t, manifest.Reference{Kind: manifest.KindVariable, Name: "app-config"}, reference)
	})

	t.Run("reports that a mounted volume reads nothing", func(t *testing.T) {
		// A volume holds whatever the workload puts there, so there is nothing orca
		// resolves for it.
		_, ok := manifest.VolumeMount{Name: "example-data"}.Reference()
		assert.False(t, ok)
	})
}

func TestNames(t *testing.T) {
	t.Parallel()

	references := []manifest.Reference{
		{Kind: manifest.KindSecret, Name: "db-password"},
		{Kind: manifest.KindSecret, Name: "api-token"},
		{Kind: manifest.KindVariable, Name: "log-level"},
		{Kind: manifest.KindVariable, Name: "db-host"},
	}

	t.Run("returns the secrets", func(t *testing.T) {
		assert.Equal(t, []string{"db-password", "api-token"}, manifest.Names(references, manifest.KindSecret))
	})

	t.Run("returns the variables", func(t *testing.T) {
		assert.Equal(t, []string{"log-level", "db-host"}, manifest.Names(references, manifest.KindVariable))
	})

	t.Run("returns nothing for a kind that is absent", func(t *testing.T) {
		secrets := []manifest.Reference{{Kind: manifest.KindSecret, Name: "only"}}
		assert.Empty(t, manifest.Names(secrets, manifest.KindVariable))
	})

	t.Run("returns nothing for no references", func(t *testing.T) {
		assert.Empty(t, manifest.Names(nil, manifest.KindSecret))
	})
}
