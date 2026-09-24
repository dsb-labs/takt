package score_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/score"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("reads the score in a directory", func(t *testing.T) {
		loaded, err := score.Load(filepath.Join("testdata", "blog"))
		require.NoError(t, err)

		assert.Equal(t, "blog", loaded.Name)
		assert.Equal(t, "1.4.0", loaded.Release)
		assert.Equal(t, filepath.Join("testdata", "blog"), loaded.Directory)
		assert.Equal(t, []string{"pgdata.yaml"}, loaded.Volumes)
		assert.Equal(t, []string{"db.yaml", "web.yaml", "worker.yaml"}, loaded.Workloads)
		assert.Equal(t, []string{"web.service.yaml"}, loaded.Services)
		assert.Equal(t, []score.Variable{
			{Name: "web-config", File: "files/web.json"},
			{Name: "motd", Value: "{{ .Values.motd | upper }}"},
			{Name: "db-host"},
		}, loaded.Variables)
		assert.Equal(t, []score.Secret{{Name: "db-password"}}, loaded.Secrets)
	})

	t.Run("reads a score named directly", func(t *testing.T) {
		loaded, err := score.Load(filepath.Join("testdata", "blog", "score.yaml"))
		require.NoError(t, err)

		assert.Equal(t, "blog", loaded.Name)
		assert.Equal(t, filepath.Join("testdata", "blog"), loaded.Directory)
	})

	t.Run("reports a location that does not exist", func(t *testing.T) {
		_, err := score.Load(filepath.Join("testdata", "missing"))
		assert.Error(t, err)
	})

	tt := []struct {
		Name string
		File string
	}{
		{Name: "refuses a score without a version", File: "no_version.yaml"},
		{Name: "refuses an unknown key", File: "unknown_key.yaml"},
		{Name: "refuses a variable with both a file and a value", File: "both_sources.yaml"},
		{Name: "refuses a path outside the score", File: "escapes.yaml"},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			_, err := score.Load(filepath.Join("testdata", "bad", tc.File))
			assert.Error(t, err)
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := func() score.Score {
		return score.Score{Version: "v1", Name: "blog", Release: "1.0.0"}
	}

	tt := []struct {
		Name         string
		Modify       func(*score.Score)
		ExpectsError bool
	}{
		{
			Name:   "accepts a minimal score",
			Modify: func(*score.Score) {},
		},
		{
			Name:         "refuses a version it does not understand",
			Modify:       func(s *score.Score) { s.Version = "v2" },
			ExpectsError: true,
		},
		{
			Name:         "refuses a name outside the workload grammar",
			Modify:       func(s *score.Score) { s.Name = "Blog" },
			ExpectsError: true,
		},
		{
			Name:         "refuses a missing release",
			Modify:       func(s *score.Score) { s.Release = "" },
			ExpectsError: true,
		},
		{
			Name:         "refuses a release that cannot be a label value",
			Modify:       func(s *score.Score) { s.Release = "1.0\t0" },
			ExpectsError: true,
		},
		{
			Name:         "refuses an absolute path",
			Modify:       func(s *score.Score) { s.Volumes = []string{"/etc/pgdata.yaml"} },
			ExpectsError: true,
		},
		{
			Name:         "refuses an empty path",
			Modify:       func(s *score.Score) { s.Services = []string{""} },
			ExpectsError: true,
		},
		{
			Name: "refuses a variable listed twice",
			Modify: func(s *score.Score) {
				s.Variables = []score.Variable{{Name: "config", Value: "a"}, {Name: "config"}}
			},
			ExpectsError: true,
		},
		{
			Name:         "refuses a variable with a bad name",
			Modify:       func(s *score.Score) { s.Variables = []score.Variable{{Name: "web_config"}} },
			ExpectsError: true,
		},
		{
			Name:         "refuses a secret listed twice",
			Modify:       func(s *score.Score) { s.Secrets = []score.Secret{{Name: "pw"}, {Name: "pw"}} },
			ExpectsError: true,
		},
		{
			Name: "accepts a nested relative path",
			Modify: func(s *score.Score) {
				s.Workloads = []string{"workloads/web.yaml", "./db.yaml"}
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			s := valid()
			tc.Modify(&s)

			err := score.Validate(s)
			if tc.ExpectsError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
		})
	}
}

func TestVariable_Owned(t *testing.T) {
	t.Parallel()

	assert.True(t, score.Variable{Name: "a", File: "a.json"}.Owned())
	assert.True(t, score.Variable{Name: "a", Value: "v"}.Owned())
	assert.False(t, score.Variable{Name: "a"}.Owned())
}
