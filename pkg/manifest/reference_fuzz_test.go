package manifest_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/manifest"
)

// What a reference's name may be, restated here rather than read from the package so
// that a change to the pattern has to be made deliberately in both places.
var fuzzedName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// FuzzParseReferences checks the properties the reference grammar promises, against
// inputs nobody thought to write down.
//
// The grammar is hand-parsed and now has three kinds, one of which takes a second
// segment, so the value here is in the invariants rather than in any particular
// input: whatever the fuzzer finds, what comes back is either an error or a set of
// references that expansion can act on.
//
// A kind is a literal the fuzzer will not synthesise from an input that does not
// already hold it, so every kind has to be seeded or its parsing is never reached.
func FuzzParseReferences(f *testing.F) {
	for _, seed := range []string{
		"",
		"${secret:db-password}",
		"${var:log-level}",
		"postgres://app:${secret:db-password}@${var:db-host}/app",
		"${secret:token} ${secret:token}",
		"${secret:token} ${var:token}",
		"${workload:postgres}",
		"${workload:postgres:pg}",
		"${workload:postgres:5432}",
		"postgres://app:${secret:db-password}@${workload:postgres:pg}/app",
		"${workload:api:http} ${workload:api:grpc}",
		"${workload:postgres:}",
		"${workload:postgres:PG}",
		"${workload:postgres:pg:extra}",
		"${secret:db-password:pg}",
		"${var:db-host:pg}",
		"$$notasecret",
		"$${secret:token}",
		"$$${secret:token}",
		"cost: 5$$",
		"cost: 5$",
		"$HOME",
		"${secret:db-password",
		"${env:HOME}",
		"${secret:DB_PASSWORD}",
		"${secret:}",
		"${secret}",
		"${var}",
		"{{ if .Debug }}on{{ end }}",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		references, err := manifest.ParseReferences(value)

		// Expansion parses before it substitutes, so what one accepts the other has to
		// handle. Expand reads the opening and the closing brace back without checking
		// either, which is only safe while this holds.
		expanded, expandErr := manifest.Expand(value, func(reference manifest.Reference) (string, bool) {
			return "resolved-" + reference.Name, true
		})

		if err != nil {
			assert.ErrorIs(t, err, manifest.ErrInvalidReference)
			require.Error(t, expandErr)
			assert.Equal(t, err.Error(), expandErr.Error())

			return
		}

		require.NoError(t, expandErr)

		for _, reference := range references {
			assert.Contains(t, []manifest.ReferenceKind{
				manifest.KindSecret,
				manifest.KindVariable,
				manifest.KindWorkload,
			}, reference.Kind)

			// A name that reaches a caller is used to look a value up and to name a
			// file, so one the pattern does not admit must never be reported as valid.
			assert.Regexp(t, fuzzedName, reference.Name)
			assert.LessOrEqual(t, len(reference.Name), 63)

			// Only a workload reference names a port, and a port is held to the same
			// shape a name is: it selects one of a workload's ports, which are named
			// under the same rules.
			if reference.Port != "" {
				assert.Equal(t, manifest.KindWorkload, reference.Kind)
				assert.Regexp(t, fuzzedName, string(reference.Port))
				assert.LessOrEqual(t, len(reference.Port), 63)
			}

			// What a reference is written as round-trips. The hash of a workload
			// reading one is keyed on this, so a reference that stringified to
			// something other than the text it was parsed from would key two
			// different references the same way.
			//
			// The opening is spelled out here rather than read from the package, for
			// the reason the name pattern is: a change to the syntax has to be made
			// deliberately in both places.
			assert.Contains(t, value, "${"+string(reference.Kind)+":"+reference.String()+"}")

			// Nothing is passed through: a reference that was reported was also
			// substituted, rather than left in the output as the text that wrote it.
			assert.Contains(t, expanded, "resolved-"+reference.Name)
		}

		assert.Len(t, slices.Compact(slices.Clone(references)), len(references))

		// A dollar sign the parser did not account for is the failure this grammar
		// exists to prevent, so an input holding one that opened nothing has to have
		// been reported above rather than reaching here.
		if strings.Contains(value, "$") {
			assert.True(t, len(references) > 0 || strings.Contains(value, "$$"))
		}
	})
}
