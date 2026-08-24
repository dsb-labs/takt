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
				assert.Equal(t, "/etc/orca/docker-config.json", config.Docker.ConfigFile)
				assert.Equal(t, 30*time.Second, config.Reconcile.Interval)
				assert.Equal(t, []string{"orca.example.com"}, config.HTTP.Hosts)
				assert.Equal(t, "0.0.0.0", config.Workload.Bind)
				assert.Equal(t, 25000, config.Workload.MinPort)
				assert.Equal(t, 26000, config.Workload.MaxPort)
				assert.Equal(t, "/etc/orca/secret.key", config.Secrets.KeyFile)
				assert.Equal(t, "/etc/orca/secret.key", config.KeyPath())
				assert.Equal(t, []string{"/opt/runtime", "/nix/store"}, config.Exec.AllowPaths)
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
		assert.Positive(t, config.Workload.MinPort)
		assert.Positive(t, config.Workload.MaxPort)
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

	t.Run("publishes workload ports on loopback", func(t *testing.T) {
		// A published port exposes whatever the workload serves, so it is the same
		// decision as binding the API and gets the same default. An operator who
		// restricted reach to orca's own port would otherwise still be publishing
		// every workload to the network.
		//
		// Parsed rather than compared, which also pins that the default is not empty:
		// docker reads an empty host address as every interface.
		bind, err := netip.ParseAddr(server.DefaultConfig().Workload.Bind)
		require.NoError(t, err, "the default bind address must name an interface explicitly")

		assert.True(t, bind.IsLoopback(), "workload ports are published off-host: %s", bind)
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
			Mutate:       func(c *server.Config) { c.Workload.MinPort, c.Workload.MaxPort = 30000, 20000 },
			ExpectsError: true,
		},
		{
			Name:         "a port range outside the usable range",
			Mutate:       func(c *server.Config) { c.Workload.MaxPort = 70000 },
			ExpectsError: true,
		},
		{
			// Refused rather than taken as a default, because docker reads an empty
			// host address as every interface — the opposite of what orca defaults to.
			Name:         "an empty workload bind address",
			Mutate:       func(c *server.Config) { c.Workload.Bind = "" },
			ExpectsError: true,
		},
		{
			// A name would have to be resolved, and what it resolved to could change
			// under a workload already published on it.
			Name:         "a workload bind address that is not an address",
			Mutate:       func(c *server.Config) { c.Workload.Bind = "localhost" },
			ExpectsError: true,
		},
		{
			Name:         "an unknown log level",
			Mutate:       func(c *server.Config) { c.Logging.Level = "chatty" },
			ExpectsError: true,
		},
		{
			Name:   "absolute paths an exec workload may read",
			Mutate: func(c *server.Config) { c.Exec.AllowPaths = []string{"/opt/runtime", "/nix/store"} },
		},
		{
			Name:   "an absolute docker config file",
			Mutate: func(c *server.Config) { c.Docker.ConfigFile = "/etc/orca/docker-config.json" },
		},
		{
			// The server's working directory is nowhere an operator meant to keep
			// credentials.
			Name:         "a relative docker config file",
			Mutate:       func(c *server.Config) { c.Docker.ConfigFile = "docker-config.json" },
			ExpectsError: true,
		},
		{
			// Every exec workload runs in a directory of its own, so a relative path
			// names somewhere different for each of them and nowhere the operator meant.
			Name:         "a relative path an exec workload may read",
			Mutate:       func(c *server.Config) { c.Exec.AllowPaths = []string{"runtime"} },
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

func TestConfig_KeyPath(t *testing.T) {
	t.Parallel()

	t.Run("puts the key beside the database by default", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "/var/lib/orca"

		// Resolved against the data directory as configured, not as defaulted. A path
		// computed before the file was read would name the directory orca is not using.
		assert.Equal(t, filepath.Join("/var/lib/orca", "secret.key"), config.KeyPath())
	})

	t.Run("uses the file it was given", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "/var/lib/orca"
		config.Secrets.KeyFile = "/etc/orca/secret.key"

		// Keeping the key off the disk holding the database is a decision an operator
		// is allowed to make.
		assert.Equal(t, "/etc/orca/secret.key", config.KeyPath())
	})
}
