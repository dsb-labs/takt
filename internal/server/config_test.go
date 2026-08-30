package server_test

import (
	"net"
	"net/netip"
	"path/filepath"
	"strings"
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
				assert.Equal(t, "/etc/orca/tls/cert.pem", config.HTTP.TLSCert)
				assert.Equal(t, "/etc/orca/tls/key.pem", config.HTTP.TLSKey)
				assert.True(t, config.HTTP.TLSEnabled())
				assert.Equal(t, "0.0.0.0", config.Workload.Bind)
				assert.Equal(t, 25000, config.Workload.MinPort)
				assert.Equal(t, 26000, config.Workload.MaxPort)
				assert.Equal(t, "/etc/orca/keys", config.Secrets.Keys)
				assert.Equal(t, "/etc/orca/keys", config.KeysPath())
				assert.Equal(t, []string{"/opt/runtime", "/nix/store"}, config.Exec.AllowPaths)
				assert.Equal(t, "http://collector.example.com:4318", config.Telemetry.OTLPEndpoint)
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

	t.Run("publishes workload ports on every interface", func(t *testing.T) {
		// Unlike the API. A workload's port exists to be reached, and one of the
		// things reaching it is another workload on this host: a container dialling
		// a port published on loopback reaches its own loopback rather than the
		// host, so loopback is the one value that leaves workloads unable to reach
		// each other.
		//
		// Parsed rather than compared, which also pins that the default is not empty:
		// docker reads an empty host address as every interface, so a reader would
		// have to know that to see which of the two was meant.
		bind, err := netip.ParseAddr(server.DefaultConfig().Workload.Bind)
		require.NoError(t, err, "the default bind address must name an interface explicitly")

		assert.True(t, bind.IsUnspecified(), "workload ports are published on one interface: %s", bind)
	})
}

func TestWorkloadConfig_Address(t *testing.T) {
	t.Parallel()

	t.Run("dials the interface the ports are published on", func(t *testing.T) {
		// An operator who named an interface has already said where the ports are,
		// so there is nothing to work out.
		address, err := server.WorkloadConfig{Bind: "10.0.0.5"}.Address()
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.5", address)
	})

	t.Run("dials loopback when the ports are published there", func(t *testing.T) {
		address, err := server.WorkloadConfig{Bind: "127.0.0.1"}.Address()
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", address)
	})

	t.Run("resolves the unspecified address to one that can be dialled", func(t *testing.T) {
		// The unspecified address publishes everywhere and names nowhere. A workload
		// told to dial it would reach its own loopback, so it has to be resolved to
		// an address of this host.
		address, err := server.WorkloadConfig{Bind: "0.0.0.0"}.Address()
		if err != nil {
			// A host with no route out has no address to offer beyond loopback,
			// which is the documented answer rather than a failure.
			assert.Equal(t, "127.0.0.1", address)

			return
		}

		parsed, err := netip.ParseAddr(address)
		require.NoError(t, err)
		assert.False(t, parsed.IsUnspecified(), "the resolved address cannot be dialled: %s", address)
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
			Name: "a tls certificate pair",
			Mutate: func(c *server.Config) {
				c.HTTP.TLSCert, c.HTTP.TLSKey = "/etc/orca/tls/cert.pem", "/etc/orca/tls/key.pem"
			},
		},
		{
			// Half a pair caught here is a clearer failure than a server that
			// cannot present a certificate.
			Name:         "a tls certificate without its key",
			Mutate:       func(c *server.Config) { c.HTTP.TLSCert = "/etc/orca/tls/cert.pem" },
			ExpectsError: true,
		},
		{
			Name:         "a tls key without its certificate",
			Mutate:       func(c *server.Config) { c.HTTP.TLSKey = "/etc/orca/tls/key.pem" },
			ExpectsError: true,
		},
		{
			// The server's working directory is nowhere an operator meant to keep
			// key material.
			Name: "a relative tls certificate",
			Mutate: func(c *server.Config) {
				c.HTTP.TLSCert, c.HTTP.TLSKey = "cert.pem", "/etc/orca/tls/key.pem"
			},
			ExpectsError: true,
		},
		{
			Name: "a relative tls key",
			Mutate: func(c *server.Config) {
				c.HTTP.TLSCert, c.HTTP.TLSKey = "/etc/orca/tls/cert.pem", "key.pem"
			},
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
			Name:   "a valid otlp endpoint",
			Mutate: func(c *server.Config) { c.Telemetry.OTLPEndpoint = "https://collector.example.com:4318" },
		},
		{
			Name:         "an otlp endpoint that is not a url",
			Mutate:       func(c *server.Config) { c.Telemetry.OTLPEndpoint = "://collector" },
			ExpectsError: true,
		},
		{
			// The exporters speak OTLP over HTTP, so a scheme they cannot use —
			// or a bare host that parses as one — is refused at startup rather
			// than failing quietly on the first export.
			Name:         "an otlp endpoint without an http scheme",
			Mutate:       func(c *server.Config) { c.Telemetry.OTLPEndpoint = "collector.example.com:4318" },
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

// TestConfig_Validate_ResolvesDataDirectory covers the one thing validation changes
// rather than checks.
//
// A relative data directory produces workloads that cannot start: Landlock is given a
// workload's own directory and docker is given a volume's, and both refuse a path that
// is not absolute. The development configuration in the repository names ./data, so
// this is the ordinary case.
func TestConfig_Validate_ResolvesDataDirectory(t *testing.T) {
	t.Parallel()

	t.Run("makes a relative directory absolute", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "./data"

		require.NoError(t, config.Validate())

		assert.True(t, filepath.IsAbs(config.Data.Directory),
			"the data directory is still relative: %s", config.Data.Directory)
		assert.True(t, strings.HasSuffix(config.Data.Directory, "/data"))
	})

	t.Run("leaves an absolute directory alone", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "/var/lib/orca"

		require.NoError(t, config.Validate())
		assert.Equal(t, "/var/lib/orca", config.Data.Directory)
	})

	// Resolving an empty directory would silently turn it into the working
	// directory, and the operator would never learn they had not set one.
	t.Run("leaves an empty directory to be reported as missing", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = ""

		err := config.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "data directory is required")
	})

	// The keyring and the database are derived from the directory, so both follow it
	// once it has been resolved.
	t.Run("carries the resolved directory into what derives from it", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "./data"

		require.NoError(t, config.Validate())
		assert.True(t, filepath.IsAbs(config.KeysPath()))
		assert.True(t, filepath.IsAbs(config.DatabasePath()))
	})
}

func TestConfig_DatabasePath(t *testing.T) {
	t.Parallel()

	// Derived from the configuration rather than known only to the server, because
	// restoring a node has to write the database back and has the configuration and
	// nothing else to go on.
	config := server.DefaultConfig()
	config.Data.Directory = "/var/lib/orca"

	assert.Equal(t, filepath.Join("/var/lib/orca", "state.db"), config.DatabasePath())
}

func TestConfig_KeysPath(t *testing.T) {
	t.Parallel()

	t.Run("puts the keyring beside the database by default", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "/var/lib/orca"

		// Resolved against the data directory as configured, not as defaulted. A path
		// computed before the file was read would name the directory orca is not using.
		assert.Equal(t, filepath.Join("/var/lib/orca", "keys"), config.KeysPath())
	})

	t.Run("uses the directory it was given", func(t *testing.T) {
		config := server.DefaultConfig()
		config.Data.Directory = "/var/lib/orca"
		config.Secrets.Keys = "/etc/orca/keys"

		// Keeping the keys off the disk holding the database is a decision an operator
		// is allowed to make.
		assert.Equal(t, "/etc/orca/keys", config.KeysPath())
	})
}
