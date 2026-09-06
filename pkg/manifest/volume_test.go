package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestParseVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Expected     manifest.Volume
		ExpectsError bool
	}{
		{
			Name:     "a volume manifest",
			File:     "volume/volume.yaml",
			Expected: manifest.Volume{Version: "v1", Name: "example-data"},
		},
		{
			Name: "a volume manifest with labels",
			File: "volume/volume_labels.yaml",
			Expected: manifest.Volume{
				Version: "v1",
				Name:    "example-data",
				Labels:  map[string]string{"app": "web", "app.kubernetes.io/name": "example"},
			},
		},
		{
			Name: "a volume manifest with an owner and a mode",
			File: "volume/volume_owner.yaml",
			Expected: manifest.Volume{
				Version: "v1",
				Name:    "example-data",
				Owner:   "470:470",
				Mode:    "0755",
			},
		},
		{
			// A scalar decoded into a string keeps its text, so the octal form
			// survives without the quotes an operator will forget.
			Name: "keeps an unquoted mode as written",
			File: "volume/volume_unquoted_mode.yaml",
			Expected: manifest.Volume{
				Version: "v1",
				Name:    "example-data",
				Mode:    "0755",
			},
		},
		{
			// A name would resolve against the host's user database, so the same
			// manifest would mean different users on different hosts.
			Name:         "rejects an owner written as a name",
			File:         "volume/volume_bad_owner.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a mode that is not octal",
			File:         "volume/volume_bad_mode.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a name takt would not accept",
			File:         "volume/volume_bad_name.yaml",
			ExpectsError: true,
		},
		{
			// A volume's labels answer to the same rules a workload's do, reserved
			// prefix included, so an operator learns them once.
			Name:         "rejects a label takt reserves for itself",
			File:         "volume/volume_bad_label.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a schema version it does not understand",
			File:         "volume/volume_bad_version.yaml",
			ExpectsError: true,
		},
		{
			// A key this package does not know is a misunderstanding worth
			// reporting rather than ignoring.
			Name:         "rejects an unknown field",
			File:         "volume/volume_unknown_field.yaml",
			ExpectsError: true,
		},
		{
			// Which resource a file describes is decided by what it is given to, so
			// a workload manifest handed to this reads as unknown keys.
			Name:         "rejects a workload manifest",
			File:         "workload/container.yaml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.File))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			volume, err := manifest.ParseVolume(f)
			if tc.ExpectsError {
				assert.Error(t, err)
				assert.Zero(t, volume)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, volume)
		})
	}
}
