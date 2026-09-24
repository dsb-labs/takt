package score_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/score"
)

func TestScore_Values(t *testing.T) {
	t.Parallel()

	t.Run("reads the defaults beside the score", func(t *testing.T) {
		loaded, err := score.Load(filepath.Join("testdata", "blog"))
		require.NoError(t, err)

		values, err := loaded.Values()
		require.NoError(t, err)

		assert.Equal(t, score.Values{
			"image":  "example/blog:1.4.0",
			"motd":   "hello",
			"web":    map[string]any{"replicas": 2, "logLevel": "info"},
			"worker": map[string]any{"enabled": false},
		}, values)
	})

	t.Run("merges each file over the defaults in order", func(t *testing.T) {
		loaded, err := score.Load(filepath.Join("testdata", "blog"))
		require.NoError(t, err)

		last := filepath.Join(t.TempDir(), "last.yaml")
		require.NoError(t, os.WriteFile(last, []byte("web:\n  replicas: 5\n"), 0o600))

		values, err := loaded.Values(filepath.Join("testdata", "blog-overrides.yaml"), last)
		require.NoError(t, err)

		assert.Equal(t, score.Values{
			"image":  "example/blog:1.4.0",
			"motd":   "hello",
			"web":    map[string]any{"replicas": 5, "logLevel": "info"},
			"worker": map[string]any{"enabled": true},
		}, values)
	})

	t.Run("a score with no defaults file has no defaults", func(t *testing.T) {
		values, err := score.Score{Directory: t.TempDir()}.Values()
		require.NoError(t, err)
		assert.Empty(t, values)
	})

	t.Run("a score with no directory reads no defaults", func(t *testing.T) {
		values, err := score.Score{}.Values()
		require.NoError(t, err)
		assert.Empty(t, values)
	})

	t.Run("reports a values file that does not exist", func(t *testing.T) {
		_, err := score.Score{}.Values(filepath.Join(t.TempDir(), "missing.yaml"))
		assert.Error(t, err)
	})

	t.Run("reports a values file that is not yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "values.yaml")
		require.NoError(t, os.WriteFile(path, []byte("a: [\n"), 0o600))

		_, err := score.Score{}.Values(path)
		assert.Error(t, err)
	})
}

func TestValues_Merge(t *testing.T) {
	t.Parallel()

	t.Run("merges mappings key by key", func(t *testing.T) {
		values := score.Values{"web": map[string]any{"replicas": 2, "logLevel": "info"}}
		values.Merge(score.Values{"web": map[string]any{"replicas": 3}})

		assert.Equal(t, score.Values{"web": map[string]any{"replicas": 3, "logLevel": "info"}}, values)
	})

	t.Run("replaces sequences and scalars whole", func(t *testing.T) {
		values := score.Values{"hosts": []any{"a", "b"}, "motd": "hello"}
		values.Merge(score.Values{"hosts": []any{"c"}, "motd": "bye"})

		assert.Equal(t, score.Values{"hosts": []any{"c"}, "motd": "bye"}, values)
	})

	t.Run("a mapping replaces a scalar and a scalar replaces a mapping", func(t *testing.T) {
		values := score.Values{"a": "scalar", "b": map[string]any{"k": "v"}}
		values.Merge(score.Values{"a": map[string]any{"k": "v"}, "b": "scalar"})

		assert.Equal(t, score.Values{"a": map[string]any{"k": "v"}, "b": "scalar"}, values)
	})

	t.Run("does not alias the merged mapping", func(t *testing.T) {
		other := score.Values{"web": map[string]any{"replicas": 2}}

		values := score.Values{}
		values.Merge(other)
		values.Merge(score.Values{"web": map[string]any{"replicas": 3}})

		assert.Equal(t, 2, other["web"].(map[string]any)["replicas"])
	})
}
