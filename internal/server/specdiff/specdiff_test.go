package specdiff_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/specdiff"
)

func TestChanged(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Stored   string
		Applying string
		Skip     []string
		Expects  []string
	}{
		{
			Name:     "identical specifications",
			Stored:   `{"name":"example","container":{"image":"nginx:1.27"}}`,
			Applying: `{"name":"example","container":{"image":"nginx:1.27"}}`,
		},
		{
			Name:     "a field written differently",
			Stored:   `{"name":"example","container":{"image":"nginx:1.27"}}`,
			Applying: `{"name":"example","container":{"image":"nginx:1.28"}}`,
			Expects:  []string{"$.container.image"},
		},
		{
			Name:     "a field added",
			Stored:   `{"name":"example"}`,
			Applying: `{"name":"example","container":{"image":"nginx:1.27"}}`,
			Expects:  []string{"$.container"},
		},
		{
			Name:     "a field removed",
			Stored:   `{"name":"example","labels":{"app":"web"}}`,
			Applying: `{"name":"example"}`,
			Expects:  []string{"$.labels"},
		},
		{
			Name:     "a field that changed shape",
			Stored:   `{"name":"example","command":"serve"}`,
			Applying: `{"name":"example","command":["serve","--port","80"]}`,
			Expects:  []string{"$.command"},
		},
		{
			Name:     "an element of an array",
			Stored:   `{"ports":[{"to":80},{"to":443}]}`,
			Applying: `{"ports":[{"to":80},{"to":8443}]}`,
			Expects:  []string{"$.ports[1].to"},
		},
		{
			Name:     "an element appended to an array",
			Stored:   `{"ports":[{"to":80}]}`,
			Applying: `{"ports":[{"to":80},{"to":443}]}`,
			Expects:  []string{"$.ports[1]"},
		},
		{
			Name:     "an element inserted at the front of an array",
			Stored:   `{"ports":[{"to":443}]}`,
			Applying: `{"ports":[{"to":80},{"to":443}]}`,
			Expects:  []string{"$.ports[0].to", "$.ports[1]"},
		},
		{
			Name:     "an element removed from an array",
			Stored:   `{"ports":[{"to":80},{"to":443}]}`,
			Applying: `{"ports":[{"to":80}]}`,
			Expects:  []string{"$.ports[1]"},
		},
		{
			Name:     "several fields at once",
			Stored:   `{"name":"example","labels":{"app":"web"},"container":{"image":"nginx:1.27"}}`,
			Applying: `{"name":"example","labels":{"app":"api"},"container":{"image":"nginx:1.28"}}`,
			Expects:  []string{"$.container.image", "$.labels.app"},
		},
		{
			Name:     "a skipped path",
			Stored:   `{"ports":[{"to":80,"from":30993}]}`,
			Applying: `{"ports":[{"to":80}]}`,
			Skip:     []string{"$.ports[0].from"},
		},
		{
			Name:     "a skipped path alongside a real change",
			Stored:   `{"ports":[{"to":80,"from":30993}],"container":{"image":"nginx:1.27"}}`,
			Applying: `{"ports":[{"to":8080}],"container":{"image":"nginx:1.27"}}`,
			Skip:     []string{"$.ports[0].from"},
			Expects:  []string{"$.ports[0].to"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			paths, err := specdiff.Changed([]byte(tc.Stored), []byte(tc.Applying), tc.Skip)
			require.NoError(t, err)

			assert.Equal(t, tc.Expects, paths)
		})
	}
}

func TestChanged_ReportsTheSamePathsHowever(t *testing.T) {
	t.Parallel()

	// Enough keys that a map iterated in Go's order would produce them differently
	// from one call to the next.
	stored := `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8}`
	applying := `{"a":0,"b":0,"c":0,"d":0,"e":0,"f":0,"g":0,"h":0}`

	expects := []string{"$.a", "$.b", "$.c", "$.d", "$.e", "$.f", "$.g", "$.h"}

	for range 16 {
		paths, err := specdiff.Changed([]byte(stored), []byte(applying), nil)
		require.NoError(t, err)

		assert.Equal(t, expects, paths)
	}
}

func TestChanged_RefusesWhatItCannotDecode(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Stored   string
		Applying string
	}{
		{
			Name:     "the stored specification",
			Stored:   `{"name":`,
			Applying: `{"name":"example"}`,
		},
		{
			Name:     "the specification being applied",
			Stored:   `{"name":"example"}`,
			Applying: `{"name":`,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			_, err := specdiff.Changed([]byte(tc.Stored), []byte(tc.Applying), nil)
			assert.Error(t, err)
		})
	}
}
