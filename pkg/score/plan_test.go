package score_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

func workload(name string, env map[string]string, mounts ...manifest.VolumeMount) manifest.Spec {
	return manifest.Spec{
		Version:   "v1",
		Name:      name,
		Count:     1,
		Env:       env,
		Volumes:   mounts,
		Container: &manifest.Container{Image: "example"},
	}
}

func TestNewPlan(t *testing.T) {
	t.Parallel()

	t.Run("orders the blog score", func(t *testing.T) {
		rendered, err := score.Build("testdata/blog", nil)
		require.NoError(t, err)

		plan, err := score.NewPlan(rendered)
		require.NoError(t, err)

		assert.Equal(t, []score.Node{
			{Kind: score.KindVolume, Name: "blog-pgdata"},
			{Kind: score.KindVariable, Name: "motd"},
			{Kind: score.KindVariable, Name: "web-config"},
			{Kind: score.KindWorkload, Name: "blog-cron"},
			{Kind: score.KindWorkload, Name: "blog-db"},
			{Kind: score.KindWorkload, Name: "blog-web"},
			{Kind: score.KindService, Name: "blog"},
		}, plan.Steps)
		assert.Equal(t, []score.Node{
			{Kind: score.KindVariable, Name: "db-host"},
			{Kind: score.KindSecret, Name: "db-password"},
		}, plan.Requirements)
	})

	t.Run("a workload follows the workload it references", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{
				workload("a", map[string]string{"B": "${workload:b:http}"}),
				workload("b", map[string]string{"C": "${workload:c:http}"}),
				workload("c", nil),
			},
		}

		plan, err := score.NewPlan(rendered)
		require.NoError(t, err)

		assert.Equal(t, []score.Node{
			{Kind: score.KindWorkload, Name: "c"},
			{Kind: score.KindWorkload, Name: "b"},
			{Kind: score.KindWorkload, Name: "a"},
		}, plan.Steps)
		assert.Empty(t, plan.Requirements)
	})

	t.Run("a volume or workload the score does not hold is a requirement", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{
				workload("web", map[string]string{"DB": "${workload:postgres:pg}"}, manifest.VolumeMount{Name: "shared", To: "/data"}),
			},
		}

		plan, err := score.NewPlan(rendered)
		require.NoError(t, err)

		assert.Equal(t, []score.Node{{Kind: score.KindWorkload, Name: "web"}}, plan.Steps)
		assert.Equal(t, []score.Node{
			{Kind: score.KindVolume, Name: "shared"},
			{Kind: score.KindWorkload, Name: "postgres"},
		}, plan.Requirements)
	})

	t.Run("a declared secret nothing reads is still a requirement", func(t *testing.T) {
		plan, err := score.NewPlan(score.Rendered{Secrets: []string{"pw"}, Required: []string{"host"}})
		require.NoError(t, err)

		assert.Empty(t, plan.Steps)
		assert.Equal(t, []score.Node{
			{Kind: score.KindVariable, Name: "host"},
			{Kind: score.KindSecret, Name: "pw"},
		}, plan.Requirements)
	})

	t.Run("refuses a cycle", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{
				workload("a", map[string]string{"B": "${workload:b:http}"}),
				workload("b", map[string]string{"A": "${workload:a:http}"}),
				workload("c", nil),
			},
		}

		_, err := score.NewPlan(rendered)
		require.ErrorIs(t, err, score.ErrCycle)
		assert.Contains(t, err.Error(), "workload a, workload b")
		assert.NotContains(t, err.Error(), "workload c")
	})

	t.Run("refuses an undeclared variable", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{workload("web", map[string]string{"LEVEL": "${var:log-level}"})},
		}

		_, err := score.NewPlan(rendered)
		assert.ErrorIs(t, err, score.ErrUndeclared)
	})

	t.Run("refuses an undeclared secret", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{workload("web", nil, manifest.VolumeMount{Secret: "pw", To: "/etc/pw"})},
		}

		_, err := score.NewPlan(rendered)
		assert.ErrorIs(t, err, score.ErrUndeclared)
	})

	t.Run("a token reference is not a dependency", func(t *testing.T) {
		rendered := score.Rendered{
			Workloads: []manifest.Spec{workload("web", map[string]string{"TOKEN": "${token:web}"})},
		}

		plan, err := score.NewPlan(rendered)
		require.NoError(t, err)
		assert.Empty(t, plan.Requirements)
	})
}

func TestNode_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "workload web", score.Node{Kind: score.KindWorkload, Name: "web"}.String())
}
