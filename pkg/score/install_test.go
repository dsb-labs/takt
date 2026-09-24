package score_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

func labelled(name, release string) map[string]string {
	return map[string]string{score.LabelName: name, score.LabelRelease: release}
}

func TestList(t *testing.T) {
	t.Parallel()

	t.Run("groups resources by the install label", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().ListVolumes(mock.Anything).Return([]client.Volume{
			{Name: "blog-pgdata", Labels: labelled("blog", "1.4.0")},
			{Name: "loose", Labels: map[string]string{"app": "loose"}},
		}, nil)
		c.EXPECT().ListVariables(mock.Anything).Return([]client.Variable{
			{Name: "web-config", Labels: labelled("blog", "1.4.0")},
			{Name: "unlabelled"},
		}, nil)
		c.EXPECT().List(mock.Anything).Return([]client.Workload{
			{Name: "blog-web", Spec: manifest.Spec{Labels: labelled("blog", "1.5.0")}},
			{Name: "blog-db", Spec: manifest.Spec{Labels: labelled("blog", "1.4.0")}},
			{Name: "api", Spec: manifest.Spec{Labels: labelled("api", "0.1.0")}},
		}, nil)
		c.EXPECT().ListServices(mock.Anything).Return([]client.Service{
			{Name: "blog", Labels: labelled("blog", "1.4.0")},
		}, nil)

		installs, err := score.List(t.Context(), c)
		require.NoError(t, err)

		assert.Equal(t, []score.Install{
			{Name: "api", Releases: []string{"0.1.0"}, Workloads: []string{"api"}},
			{
				Name:      "blog",
				Releases:  []string{"1.4.0", "1.5.0"},
				Volumes:   []string{"blog-pgdata"},
				Variables: []string{"web-config"},
				Workloads: []string{"blog-db", "blog-web"},
				Services:  []string{"blog"},
			},
		}, installs)
	})

	t.Run("reports a list that fails", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().ListVolumes(mock.Anything).Return(nil, errors.New("boom"))

		_, err := score.List(t.Context(), c)
		assert.Error(t, err)
	})
}

func TestInstalled(t *testing.T) {
	t.Parallel()

	t.Run("queries by the install name", func(t *testing.T) {
		query := "$.labels.score=blog"

		c := NewMockClient(t)
		c.EXPECT().ListVolumes(mock.Anything, []string{query}).Return(nil, nil)
		c.EXPECT().ListVariables(mock.Anything, []string{query}).Return(nil, nil)
		c.EXPECT().List(mock.Anything, []string{query}).Return([]client.Workload{
			{Name: "blog-web", Spec: manifest.Spec{Labels: labelled("blog", "1.4.0")}},
		}, nil)
		c.EXPECT().ListServices(mock.Anything, []string{query}).Return(nil, nil)

		install, err := score.Installed(t.Context(), c, "blog")
		require.NoError(t, err)
		assert.Equal(t, score.Install{Name: "blog", Releases: []string{"1.4.0"}, Workloads: []string{"blog-web"}}, install)
	})

	t.Run("an install nothing carries holds nothing", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().ListVolumes(mock.Anything, mock.Anything).Return(nil, nil)
		c.EXPECT().ListVariables(mock.Anything, mock.Anything).Return(nil, nil)
		c.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil)
		c.EXPECT().ListServices(mock.Anything, mock.Anything).Return(nil, nil)

		install, err := score.Installed(t.Context(), c, "blog")
		require.NoError(t, err)
		assert.Equal(t, score.Install{Name: "blog"}, install)
		assert.Empty(t, install.Nodes())
	})
}

func TestPrunable(t *testing.T) {
	t.Parallel()

	install := score.Install{
		Name:      "blog",
		Volumes:   []string{"blog-pgdata", "old-data"},
		Variables: []string{"motd", "old-config", "web-config"},
		Workloads: []string{"blog-cron", "blog-db", "blog-web", "old-worker"},
		Services:  []string{"blog", "old"},
	}

	assert.Equal(t, []score.Node{
		{Kind: score.KindVolume, Name: "old-data"},
		{Kind: score.KindVariable, Name: "old-config"},
		{Kind: score.KindWorkload, Name: "old-worker"},
		{Kind: score.KindService, Name: "old"},
	}, score.Prunable(blog(t), install))

	assert.Empty(t, score.Prunable(blog(t), score.Install{Name: "blog"}))
}

func TestDestroy(t *testing.T) {
	t.Parallel()

	nodes := []score.Node{
		{Kind: score.KindVolume, Name: "data"},
		{Kind: score.KindVariable, Name: "config"},
		{Kind: score.KindWorkload, Name: "web"},
		{Kind: score.KindWorkload, Name: "db"},
		{Kind: score.KindService, Name: "web"},
	}

	// web reads the variable, mounts the volume and reaches db, so it has to
	// go before all three, and the service before everything.
	web := workload("web", map[string]string{"DB": "${workload:db:pg}", "C": "${var:config}"}, manifest.VolumeMount{Name: "data", To: "/data"})
	db := workload("db", nil)

	t.Run("deletes in reverse dependency order, waiting for workloads", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().Get(mock.Anything, "web").Return(client.Workload{Spec: web}, nil)
		c.EXPECT().Get(mock.Anything, "db").Return(client.Workload{Spec: db}, nil)

		var order []string
		c.EXPECT().DeleteService(mock.Anything, "web").Run(func(context.Context, string) { order = append(order, "service web") }).Return(nil)
		c.EXPECT().Delete(mock.Anything, "web", mock.Anything).Run(func(_ context.Context, _ string, options ...client.LifecycleOption) {
			assert.Len(t, options, 1)
			order = append(order, "workload web")
		}).Return(client.Workload{}, nil)
		c.EXPECT().Delete(mock.Anything, "db", mock.Anything).Run(func(context.Context, string, ...client.LifecycleOption) { order = append(order, "workload db") }).Return(client.Workload{}, nil)
		c.EXPECT().DeleteVariable(mock.Anything, "config").Run(func(context.Context, string, ...client.DeleteVariableOption) {
			order = append(order, "variable config")
		}).Return(nil)
		c.EXPECT().DeleteVolume(mock.Anything, "data").Run(func(context.Context, string, ...client.DeleteVolumeOption) { order = append(order, "volume data") }).Return(nil)

		deleted, err := score.Destroy(t.Context(), c, nodes)
		require.NoError(t, err)

		assert.Equal(t, []string{"service web", "workload web", "workload db", "variable config", "volume data"}, order)
		assert.Len(t, deleted, 5)
	})

	t.Run("stops at the first failure and reports what went", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().Get(mock.Anything, "web").Return(client.Workload{Spec: web}, nil)
		c.EXPECT().Get(mock.Anything, "db").Return(client.Workload{Spec: db}, nil)
		c.EXPECT().DeleteService(mock.Anything, "web").Return(nil)
		c.EXPECT().Delete(mock.Anything, "web", mock.Anything).Return(client.Workload{}, errors.New("boom"))

		deleted, err := score.Destroy(t.Context(), c, nodes)
		require.Error(t, err)
		assert.Equal(t, []score.Node{{Kind: score.KindService, Name: "web"}}, deleted)
	})

	t.Run("reports a workload it cannot read", func(t *testing.T) {
		c := NewMockClient(t)
		c.EXPECT().Get(mock.Anything, "web").Return(client.Workload{}, client.ErrWorkloadNotFound)

		_, err := score.Destroy(t.Context(), c, nodes[:3])
		assert.Error(t, err)
	})

	t.Run("nothing to destroy is a no-op", func(t *testing.T) {
		deleted, err := score.Destroy(t.Context(), NewMockClient(t), nil)
		require.NoError(t, err)
		assert.Empty(t, deleted)
	})
}
