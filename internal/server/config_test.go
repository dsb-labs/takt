package server_test

import (
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Assert       func(*testing.T, server.Config)
		ExpectsError bool
	}{
		{
			Name: "a full configuration",
			File: "full.toml",
			Assert: func(t *testing.T, config server.Config) {
				assert.Equal(t, "localhost:9999", config.HTTP.Address)
				assert.Equal(t, "/var/lib/orca", config.Data.Directory)
				assert.Equal(t, "tcp://localhost:2375", config.Docker.Host)
				assert.Equal(t, 30*time.Second, config.Reconcile.Interval)
				assert.Equal(t, 25000, config.Ports.Min)
				assert.Equal(t, 26000, config.Ports.Max)
				assert.Equal(t, "debug", config.Logging.Level)
			},
		},
		{
			Name: "a partial configuration keeps the defaults",
			File: "partial.toml",
			Assert: func(t *testing.T, config server.Config) {
				assert.Equal(t, "warn", config.Logging.Level)

				// A file only has to describe what it changes, so everything it
				// leaves out must still be usable.
				defaults := server.DefaultConfig()
				assert.Equal(t, defaults.HTTP.Address, config.HTTP.Address)
				assert.Equal(t, defaults.Reconcile.Interval, config.Reconcile.Interval)
				require.NoError(t, config.Validate())
			},
		},
		{
			Name:         "malformed toml",
			File:         "invalid.toml",
			ExpectsError: true,
		},
		{
			Name:         "a missing file",
			File:         "nope.toml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			config, err := server.LoadConfig(filepath.Join("testdata", tc.File))
			if tc.ExpectsError {
				assert.Zero(t, config)
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			tc.Assert(t, config)
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	t.Run("is usable without a configuration file", func(t *testing.T) {
		// The binary has to run with no configuration at all, so the defaults
		// must pass their own validation.
		config := server.DefaultConfig()
		require.NoError(t, config.Validate())

		assert.NotEmpty(t, config.Data.Directory)
		assert.Positive(t, config.Reconcile.Interval)
		assert.Positive(t, config.Ports.Min)
		assert.Positive(t, config.Ports.Max)
	})

	t.Run("binds to loopback", func(t *testing.T) {
		// The API has no authentication and applying a workload runs a container, so
		// reaching the port is enough to run code on the host. Exposing that to a
		// network has to be something an operator chose.
		host, _, err := net.SplitHostPort(server.DefaultConfig().HTTP.Address)
		require.NoError(t, err)

		address, err := netip.ParseAddr(host)
		require.NoError(t, err, "the default address must name an interface explicitly")

		assert.True(t, address.IsLoopback(), "the default address is reachable off-host: %s", host)
	})
}

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Mutate       func(*server.Config)
		ExpectsError bool
	}{
		{
			Name:   "the defaults are valid",
			Mutate: func(*server.Config) {},
		},
		{
			Name:         "an empty http address",
			Mutate:       func(c *server.Config) { c.HTTP.Address = "" },
			ExpectsError: true,
		},
		{
			Name:         "an empty data directory",
			Mutate:       func(c *server.Config) { c.Data.Directory = "" },
			ExpectsError: true,
		},
		{
			Name:         "a zero reconcile interval",
			Mutate:       func(c *server.Config) { c.Reconcile.Interval = 0 },
			ExpectsError: true,
		},
		{
			Name:         "a port range minimum above its maximum",
			Mutate:       func(c *server.Config) { c.Ports.Min, c.Ports.Max = 30000, 20000 },
			ExpectsError: true,
		},
		{
			Name:         "a port range outside the usable range",
			Mutate:       func(c *server.Config) { c.Ports.Max = 70000 },
			ExpectsError: true,
		},
		{
			Name:         "an unknown log level",
			Mutate:       func(c *server.Config) { c.Logging.Level = "chatty" },
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			config := server.DefaultConfig()
			tc.Mutate(&config)

			err := config.Validate()
			if tc.ExpectsError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
		})
	}
}
