package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/cli"
)

// These tests set environment variables, so none of them run in parallel.

// pointConfigAt returns a path the test owns for its config file, and clears
// the connection variables so the surrounding environment cannot leak into a
// case. Resolve treats an empty variable as unset.
func pointConfigAt(t *testing.T) string {
	t.Helper()

	t.Setenv("TAKT_CONFIG", "")
	t.Setenv("TAKT_ADDRESS", "")
	t.Setenv("TAKT_TOKEN", "")
	t.Setenv("TAKT_CA_CERT", "")

	return filepath.Join(t.TempDir(), "config")
}

func TestResolve(t *testing.T) {
	t.Run("defaults with nothing configured", func(t *testing.T) {
		path := pointConfigAt(t)

		config, err := cli.Resolve(cli.Sources{ConfigPath: path})
		require.NoError(t, err)
		assert.Equal(t, cli.Config{Address: cli.DefaultAddress, File: path}, config)
	})

	t.Run("reads the config file", func(t *testing.T) {
		path := pointConfigAt(t)
		require.NoError(t, cli.Write(path, cli.Config{
			Address: "https://takt.example.com",
			Token:   "takt_c_file",
			CACert:  "/etc/takt/ca.pem",
		}))

		config, err := cli.Resolve(cli.Sources{ConfigPath: path})
		require.NoError(t, err)
		assert.Equal(t, cli.Config{
			Address: "https://takt.example.com",
			Token:   "takt_c_file",
			CACert:  "/etc/takt/ca.pem",
			File:    path,
		}, config)
	})

	t.Run("the environment overrides the file", func(t *testing.T) {
		path := pointConfigAt(t)
		require.NoError(t, cli.Write(path, cli.Config{Address: "https://file.example.com", Token: "takt_c_file"}))

		t.Setenv("TAKT_ADDRESS", "https://env.example.com")
		t.Setenv("TAKT_TOKEN", "takt_c_env")
		t.Setenv("TAKT_CA_CERT", "/etc/takt/env-ca.pem")

		config, err := cli.Resolve(cli.Sources{ConfigPath: path})
		require.NoError(t, err)
		assert.Equal(t, cli.Config{
			Address: "https://env.example.com",
			Token:   "takt_c_env",
			CACert:  "/etc/takt/env-ca.pem",
			File:    path,
		}, config)
	})

	t.Run("flags override the environment", func(t *testing.T) {
		path := pointConfigAt(t)
		t.Setenv("TAKT_ADDRESS", "https://env.example.com")
		t.Setenv("TAKT_TOKEN", "takt_c_env")
		t.Setenv("TAKT_CA_CERT", "/etc/takt/env-ca.pem")

		config, err := cli.Resolve(cli.Sources{ConfigPath: path, Address: "https://flag.example.com", CACert: "/etc/takt/flag-ca.pem"})
		require.NoError(t, err)
		// There is no token flag, so the token keeps coming from the
		// environment even when everything else was given on the command
		// line.
		assert.Equal(t, cli.Config{
			Address: "https://flag.example.com",
			Token:   "takt_c_env",
			CACert:  "/etc/takt/flag-ca.pem",
			File:    path,
		}, config)
	})

	t.Run("each field resolves independently", func(t *testing.T) {
		path := pointConfigAt(t)
		require.NoError(t, cli.Write(path, cli.Config{Address: "https://file.example.com", Token: "takt_c_file"}))

		t.Setenv("TAKT_CA_CERT", "/etc/takt/env-ca.pem")

		config, err := cli.Resolve(cli.Sources{ConfigPath: path, Address: "https://flag.example.com"})
		require.NoError(t, err)
		assert.Equal(t, cli.Config{
			Address: "https://flag.example.com",
			Token:   "takt_c_file",
			CACert:  "/etc/takt/env-ca.pem",
			File:    path,
		}, config)
	})

	t.Run("reports a file it cannot parse", func(t *testing.T) {
		path := pointConfigAt(t)
		require.NoError(t, os.WriteFile(path, []byte("not toml = ="), 0o600))

		_, err := cli.Resolve(cli.Sources{ConfigPath: path})
		assert.Error(t, err)
	})
}

func TestWrite(t *testing.T) {
	t.Run("creates the directory and file readable only by the owner", func(t *testing.T) {
		path := filepath.Join(pointConfigAt(t), ".takt", "config")

		require.NoError(t, cli.Write(path, cli.Config{Address: cli.DefaultAddress, Token: "takt_c_secret"}))

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, 0o600, info.Mode().Perm())

		dir, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.EqualValues(t, 0o700, dir.Mode().Perm())
	})

	t.Run("round-trips through load", func(t *testing.T) {
		path := pointConfigAt(t)

		expected := cli.Config{Address: "https://takt.example.com", Token: "takt_c_secret"}
		require.NoError(t, cli.Write(path, expected))

		loaded, err := cli.Load(path)
		require.NoError(t, err)

		expected.File = path
		assert.Equal(t, expected, loaded)
	})

	t.Run("replaces what was there", func(t *testing.T) {
		path := pointConfigAt(t)

		require.NoError(t, cli.Write(path, cli.Config{Token: "takt_c_old"}))
		require.NoError(t, cli.Write(path, cli.Config{Token: "takt_c_new"}))

		loaded, err := cli.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_new", loaded.Token)
	})
}

func TestConfig_Save(t *testing.T) {
	t.Run("writes back to the file the settings were read from", func(t *testing.T) {
		path := pointConfigAt(t)

		settings, err := cli.Load(path)
		require.NoError(t, err)

		settings.Address = "https://takt.example.com"
		settings.Token = "takt_c_secret"
		require.NoError(t, settings.Save())

		loaded, err := cli.Load(path)
		require.NoError(t, err)
		assert.Equal(t, settings, loaded)
	})

	t.Run("refuses settings that record no file", func(t *testing.T) {
		assert.Error(t, cli.Config{Address: cli.DefaultAddress}.Save())
	})
}

func TestPath(t *testing.T) {
	t.Run("the flag wins over the environment", func(t *testing.T) {
		t.Setenv("TAKT_CONFIG", "/from/env")

		path, err := cli.Path("/from/flag")
		require.NoError(t, err)
		assert.Equal(t, "/from/flag", path)
	})

	t.Run("the environment wins over the default", func(t *testing.T) {
		t.Setenv("TAKT_CONFIG", "/from/env")

		path, err := cli.Path("")
		require.NoError(t, err)
		assert.Equal(t, "/from/env", path)
	})

	t.Run("defaults under the home directory", func(t *testing.T) {
		t.Setenv("TAKT_CONFIG", "")
		t.Setenv("HOME", t.TempDir())

		path, err := cli.Path("")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(os.Getenv("HOME"), ".takt", "config"), path)
	})
}
