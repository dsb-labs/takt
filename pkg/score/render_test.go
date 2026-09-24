package score_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/golden"

	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

func loadBlog(t *testing.T, files ...string) (score.Score, score.Values) {
	t.Helper()

	loaded, err := score.Load(filepath.Join("testdata", "blog"))
	require.NoError(t, err)

	values, err := loaded.Values(files...)
	require.NoError(t, err)

	return loaded, values
}

// writeScore lays out a score in a temporary directory, for the cases that need
// a manifest the blog fixture would not want to carry.
func writeScore(t *testing.T, files map[string]string) score.Score {
	t.Helper()

	directory := t.TempDir()
	for name, content := range files {
		path := filepath.Join(directory, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	loaded, err := score.Load(directory)
	require.NoError(t, err)

	return loaded
}

func TestRender(t *testing.T) {
	t.Parallel()

	t.Run("renders every manifest against the defaults", func(t *testing.T) {
		loaded, values := loadBlog(t)

		rendered, err := score.Render(loaded, values)
		require.NoError(t, err)

		assert.Equal(t, "blog", rendered.Name)
		assert.Equal(t, "1.4.0", rendered.Release)
		assert.Equal(t, "blog", rendered.Package)

		labels := map[string]string{"score": "blog", "score.release": "1.4.0"}
		assert.Equal(t, labels, rendered.Labels())

		require.Len(t, rendered.Volumes, 1)
		assert.Equal(t, "blog-pgdata", rendered.Volumes[0].Name)
		assert.Equal(t, labels, rendered.Volumes[0].Labels)

		// The worker is disabled by default, so its file rendered to nothing and
		// the stream in web.yaml yielded two.
		require.Len(t, rendered.Workloads, 3)
		assert.Equal(t, "blog-db", rendered.Workloads[0].Name)
		assert.Equal(t, "blog-web", rendered.Workloads[1].Name)
		assert.Equal(t, "blog-cron", rendered.Workloads[2].Name)
		assert.Equal(t, 2, rendered.Workloads[1].Count)
		assert.Equal(t, "example/blog:1.4.0", rendered.Workloads[1].Container.Image)
		assert.Equal(t, "postgres://postgres:${secret:db-password}@${workload:blog-db:pg}/app", rendered.Workloads[1].Env["DATABASE_URL"])
		assert.Equal(t, map[string]string{"app": "blog", "tier": "web", "score": "blog", "score.release": "1.4.0"}, rendered.Workloads[1].Labels)
		assert.Equal(t, labels, rendered.Workloads[2].Labels)

		require.Len(t, rendered.Services, 1)
		assert.Equal(t, "blog", rendered.Services[0].Name)
		assert.Equal(t, labels, rendered.Services[0].Labels)

		assert.Equal(t, []manifest.Variable{
			{Name: "web-config", Value: "{\n  \"package\": \"blog\",\n  \"release\": \"1.4.0\",\n  \"logLevel\": \"info\"\n}\n", Labels: labels},
			{Name: "motd", Value: "HELLO", Labels: labels},
		}, rendered.Variables)
		assert.Equal(t, []string{"db-host"}, rendered.Required)
		assert.Equal(t, []string{"db-password"}, rendered.Secrets)

		require.Len(t, rendered.Documents, 6)
		assert.Equal(t, "worker.yaml", rendered.Documents[4].Source)
		assert.True(t, rendered.Documents[4].Skipped)
		assert.False(t, rendered.Documents[3].Skipped)

		golden.Assert(t, stream(rendered), "blog.golden")
	})

	t.Run("values files change what renders", func(t *testing.T) {
		loaded, values := loadBlog(t, filepath.Join("testdata", "blog-overrides.yaml"))

		rendered, err := score.Render(loaded, values)
		require.NoError(t, err)

		require.Len(t, rendered.Workloads, 4)
		assert.Equal(t, 3, rendered.Workloads[1].Count)
		assert.Equal(t, "blog-worker", rendered.Workloads[3].Name)
	})

	t.Run("the install name reaches every template", func(t *testing.T) {
		loaded, values := loadBlog(t)

		rendered, err := score.Render(loaded, values, score.WithName("staging"))
		require.NoError(t, err)

		assert.Equal(t, "staging", rendered.Name)
		assert.Equal(t, "blog", rendered.Package)
		assert.Equal(t, "staging-pgdata", rendered.Volumes[0].Name)
		assert.Equal(t, "staging-db", rendered.Workloads[0].Name)
		assert.Equal(t, "staging", rendered.Services[0].Name)
		assert.Equal(t, "staging", rendered.Workloads[0].Labels["score"])
		assert.Equal(t, "staging", rendered.Variables[0].Labels["score"])
		assert.Contains(t, rendered.Variables[0].Value, `"package": "blog"`)
	})

	t.Run("refuses an install name outside the workload grammar", func(t *testing.T) {
		loaded, values := loadBlog(t)

		_, err := score.Render(loaded, values, score.WithName("Staging"))
		assert.Error(t, err)
	})

	t.Run("a value the values do not hold fails the render", func(t *testing.T) {
		loaded, values := loadBlog(t)
		delete(values, "image")

		rendered, err := score.Render(loaded, values)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "web.yaml")

		// Everything before the failure is still reported.
		assert.Len(t, rendered.Volumes, 1)
		assert.Len(t, rendered.Workloads, 1)
	})

	t.Run("a document that does not parse is reported with its text", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: bad\nrelease: 1.0.0\nworkloads:\n  - web.yaml\n",
			"web.yaml":   "version: v1\nname: web\ncontainer:\n  image: example\n  imag: typo\n",
		})

		rendered, err := score.Render(loaded, nil)
		require.ErrorIs(t, err, score.ErrInvalidDocument)
		assert.Contains(t, err.Error(), "web.yaml")

		require.Len(t, rendered.Documents, 1)
		assert.Contains(t, rendered.Documents[0].Text, "imag: typo")
	})

	t.Run("refuses two manifests naming the same resource", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: dup\nrelease: 1.0.0\nvolumes:\n  - a.yaml\n  - b.yaml\n",
			"a.yaml":     "version: v1\nname: data\n",
			"b.yaml":     "version: v1\nname: data\n",
		})

		_, err := score.Render(loaded, nil)
		assert.ErrorIs(t, err, score.ErrDuplicateName)
	})

	t.Run("refuses a manifest that sets a score label", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: lbl\nrelease: 1.0.0\nvolumes:\n  - a.yaml\n",
			"a.yaml":     "version: v1\nname: data\nlabels:\n  score: other\n",
		})

		_, err := score.Render(loaded, nil)
		assert.ErrorIs(t, err, score.ErrReservedLabel)
	})

	t.Run("a comment-only document is skipped", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: skip\nrelease: 1.0.0\nvolumes:\n  - a.yaml\n",
			"a.yaml":     "# nothing here\n\n---\nversion: v1\nname: data\n---\n",
		})

		rendered, err := score.Render(loaded, nil)
		require.NoError(t, err)

		assert.Len(t, rendered.Documents, 3)
		assert.Len(t, rendered.Volumes, 1)
	})

	t.Run("impure template functions are not available", func(t *testing.T) {
		for _, call := range []string{"now", "env \"HOME\"", "uuidv4", "randAlpha 4", "randInt 1 10", "genPrivateKey \"rsa\""} {
			loaded := writeScore(t, map[string]string{
				"score.yaml": "version: v1\nname: pure\nrelease: 1.0.0\nvariables:\n  - name: v\n    value: '{{ " + call + " }}'\n",
			})

			_, err := score.Render(loaded, nil)
			assert.Error(t, err, "allowed %s", call)
		}
	})

	t.Run("pure template functions are available", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: pure\nrelease: 1.0.0\nvariables:\n  - name: v\n    value: '{{ list 1 2 3 | join \",\" }} {{ \"x\" | upper }} {{ sha256sum \"a\" | trunc 8 }}'\n",
		})

		rendered, err := score.Render(loaded, nil)
		require.NoError(t, err)
		assert.Equal(t, "1,2,3 X ca978112", rendered.Variables[0].Value)
	})

	t.Run("reports a manifest file that does not exist", func(t *testing.T) {
		loaded := writeScore(t, map[string]string{
			"score.yaml": "version: v1\nname: missing\nrelease: 1.0.0\nworkloads:\n  - web.yaml\n",
		})

		_, err := score.Render(loaded, nil)
		assert.Error(t, err)
	})
}

// stream joins the rendered documents the way the render command prints them,
// so the golden file doubles as a record of that format.
func stream(rendered score.Rendered) string {
	var out strings.Builder
	for _, document := range rendered.Documents {
		if document.Skipped {
			continue
		}

		out.WriteString("---\n# Source: " + document.Source + "\n")
		out.WriteString(document.Text)
	}

	return out.String()
}

func TestBuild(t *testing.T) {
	t.Parallel()

	t.Run("loads, merges and renders", func(t *testing.T) {
		rendered, err := score.Build(filepath.Join("testdata", "blog"), []string{filepath.Join("testdata", "blog-overrides.yaml")}, score.WithName("staging"))
		require.NoError(t, err)

		assert.Equal(t, "staging", rendered.Name)
		assert.Len(t, rendered.Workloads, 4)
	})

	t.Run("reports a score that does not exist", func(t *testing.T) {
		_, err := score.Build(filepath.Join("testdata", "missing"), nil)
		assert.Error(t, err)
	})

	t.Run("reports a values file that does not exist", func(t *testing.T) {
		_, err := score.Build(filepath.Join("testdata", "blog"), []string{"missing.yaml"})
		assert.Error(t, err)
	})
}
