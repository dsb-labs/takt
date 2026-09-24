package score_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

func TestSummarise(t *testing.T) {
	t.Parallel()

	loaded := writeScore(t, map[string]string{
		"score.yaml": "version: v1\nname: app\nrelease: 2.0.0\nvolumes:\n  - data.yaml\nworkloads:\n  - web.yaml\nservices:\n  - web.service.yaml\nvariables:\n  - name: config\n    value: v\n  - name: db-host\nsecrets:\n  - name: pw\n",
		"data.yaml":  "version: v1\nname: data\n",
		"web.yaml": "version: v1\nname: web\nlabels:\n  app: web\nports:\n  - name: http\n    to: 80\nenv:\n  TOKEN: ${token:web}\n" +
			"volumes:\n  - token: scraper\n    to: /etc/token\n  - token: web\n    to: /etc/web-token\ncontainer:\n  image: example\n",
		"web.service.yaml": "version: v1\nname: web\ntarget:\n  labels:\n    app: web\n  port: 80\n",
	})

	rendered, err := score.Render(loaded, nil, score.WithName("prod"))
	require.NoError(t, err)

	assert.Equal(t, score.Summary{
		Name:       "prod",
		Package:    "app",
		Release:    "2.0.0",
		Volumes:    []string{"data"},
		Workloads:  []string{"web"},
		Services:   []string{"web"},
		Variables:  []string{"config"},
		Required:   []string{"db-host"},
		Secrets:    []string{"pw"},
		Principals: []string{"scraper", "web"},
	}, score.Summarise(rendered))
}

func TestSummarise_Empty(t *testing.T) {
	t.Parallel()

	summary := score.Summarise(score.Rendered{})
	assert.NotNil(t, summary.Required)
	assert.NotNil(t, summary.Secrets)
	assert.NotNil(t, summary.Principals)
}

func TestPrincipals(t *testing.T) {
	t.Parallel()

	assert.Empty(t, score.Principals(nil))
	assert.Empty(t, score.Principals([]manifest.Spec{{Env: map[string]string{"A": "${secret:x}"}}}))
}

func TestDocument_Numbered(t *testing.T) {
	t.Parallel()

	document := score.Document{Text: "version: v1\nname: web\n"}
	assert.Equal(t, "   1  version: v1\n   2  name: web\n", document.Numbered())
}
