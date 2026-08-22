// Package server provides the orca server and its configuration.
package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/dsb-labs/orca/internal/server/port"
)

type (
	// The Config type contains the top-level configuration for the orca server.
	Config struct {
		// HTTP server settings.
		HTTP HTTPConfig `toml:"http"`
		// On-disk state settings.
		Data DataConfig `toml:"data"`
		// Docker daemon settings.
		Docker DockerConfig `toml:"docker"`
		// Reconciliation settings.
		Reconcile ReconcileConfig `toml:"reconcile"`
		// Settings for the workloads orca runs.
		Workload WorkloadConfig `toml:"workload"`
		// Secret storage settings.
		Secrets SecretsConfig `toml:"secrets"`
		// Logging settings.
		Logging LoggingConfig `toml:"logging"`
	}

	// The HTTPConfig type contains configuration for the HTTP listener.
	HTTPConfig struct {
		// The address the HTTP server binds to, in host:port form.
		Address string `toml:"address"`
		// The host names a request may name, beyond an address literal and
		// "localhost", which are always accepted.
		//
		// A request naming anything else is refused. Reaching this API is enough to
		// run code on the host, and a browser will send a request to a loopback
		// address on behalf of any page the operator visited — so the name a request
		// asks for is checked rather than assumed to be orca's own. Set this to the
		// name a reverse proxy in front of orca serves.
		Hosts []string `toml:"hosts"`
	}

	// The DataConfig type contains configuration for the server's on-disk state.
	DataConfig struct {
		// The directory the SQLite database is stored in.
		Directory string `toml:"directory"`
	}

	// The SecretsConfig type contains configuration for the secrets orca holds.
	SecretsConfig struct {
		// The file holding the key a secret's value is encrypted under.
		//
		// Generated on first start if nothing is there. Anything that can read this
		// file can read every secret orca holds, so it is written readable only by
		// the user running the server — and it belongs on a backup, because a secret
		// sealed under a key that is gone cannot be recovered.
		//
		// Empty puts it beside the database, in the data directory.
		KeyFile string `toml:"key-file"`
	}

	// The DockerConfig type contains configuration for talking to the Docker daemon.
	DockerConfig struct {
		// The daemon to connect to. Empty uses the environment's configuration,
		// which falls back to the local socket.
		Host string `toml:"host"`
	}

	// The ReconcileConfig type contains configuration for the reconciliation loop.
	ReconcileConfig struct {
		// How often a full reconciliation pass runs regardless of driver events.
		// Events make convergence prompt; this bounds how long a missed one can
		// go unnoticed.
		Interval time.Duration `toml:"interval"`
	}

	// The WorkloadConfig type contains configuration for the workloads orca runs:
	// the address their host ports are published on, and the range it allocates
	// those ports from.
	WorkloadConfig struct {
		// The address a workload's host ports are published on.
		//
		// Loopback by default, for the same reason the API listens there: publishing
		// a port is exposing whatever the workload serves, and which interfaces that
		// reaches should be a decision an operator made rather than one orca made for
		// them. Set it to "0.0.0.0" to publish on every interface.
		//
		// This applies to a port orca publishes on a workload's behalf, which means a
		// container. An exec workload binds its port itself, so what it listens on is
		// the process's business and orca has nothing to say about it.
		Bind string `toml:"bind"`
		// The lowest host port that may be allocated.
		MinPort int `toml:"min-port"`
		// The highest host port that may be allocated.
		MaxPort int `toml:"max-port"`
	}

	// The LoggingConfig type contains configuration for application logging.
	LoggingConfig struct {
		// The minimum level to emit. One of "debug", "info", "warn", "error".
		Level string `toml:"level"`
	}
)

// DefaultConfig returns a Config populated with sensible defaults, so that the
// server runs out of the box with no configuration file at all.
func DefaultConfig() Config {
	return Config{
		HTTP: HTTPConfig{
			// Loopback rather than every interface. The API has no authentication,
			// and it can start containers — so reaching the port is enough to run
			// code on the host. Binding it to the network is a decision an operator
			// should have to make, not one a default makes for them.
			Address: "127.0.0.1:7373",
		},
		Data: DataConfig{
			Directory: defaultDataDir(),
		},
		Reconcile: ReconcileConfig{
			Interval: 10 * time.Second,
		},
		Workload: WorkloadConfig{
			// Loopback, like the API. A workload's port is published for something to
			// reach, but which interfaces that means is the same decision as binding
			// the API — so it is one an operator makes rather than a default.
			//
			// Named explicitly rather than left empty, because empty is what docker
			// reads as every interface. A reader should not have to know that to see
			// which of the two this is.
			Bind:    "127.0.0.1",
			MinPort: port.DefaultMin,
			MaxPort: port.DefaultMax,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// KeyPath returns the file holding the secret encryption key.
//
// Resolved here rather than defaulted in DefaultConfig, because the default sits
// inside the data directory and a configuration file may have moved that. A default
// computed before the file was read would point at the directory orca is not using.
func (c Config) KeyPath() string {
	if c.Secrets.KeyFile != "" {
		return c.Secrets.KeyFile
	}

	return filepath.Join(c.Data.Directory, "secret.key")
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "data"
	}

	return filepath.Join(home, ".local", "share", "orca")
}

// LoadConfig the configuration file at the specified path. The configuration file
// is expected in TOML format.
//
// Values absent from the file keep their default, so a file only has to describe
// what it changes.
func LoadConfig(path string) (Config, error) {
	config := DefaultConfig()
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return Config{}, fmt.Errorf("failed to decode config file: %w", err)
	}

	return config, nil
}

// Validate the configuration fields.
func (c *Config) Validate() error {
	return errors.Join(
		c.HTTP.validate(),
		c.Data.validate(),
		c.Reconcile.validate(),
		c.Workload.validate(),
		c.Logging.validate(),
	)
}

func (c HTTPConfig) validate() error {
	if c.Address == "" {
		return errors.New("http address is required")
	}

	return nil
}

func (c DataConfig) validate() error {
	if c.Directory == "" {
		return errors.New("data directory is required")
	}

	return nil
}

func (c ReconcileConfig) validate() error {
	if c.Interval <= 0 {
		return errors.New("reconcile interval must be greater than zero")
	}

	return nil
}

func (c WorkloadConfig) validate() error {
	switch {
	case c.Bind == "":
		return errors.New("workload bind address is required")
	// An address rather than a name, because this is what a port is published on
	// rather than somewhere orca connects to. A name would have to be resolved, and
	// what it resolved to could change under a running workload.
	case net.ParseIP(c.Bind) == nil:
		return fmt.Errorf("workload bind address must be an IP address, got %q", c.Bind)
	case c.MinPort < 1 || c.MinPort > 65535:
		return errors.New("workload port range minimum must be between 1 and 65535")
	case c.MaxPort < 1 || c.MaxPort > 65535:
		return errors.New("workload port range maximum must be between 1 and 65535")
	case c.MinPort > c.MaxPort:
		return errors.New("workload port range minimum must not exceed its maximum")
	}

	return nil
}

func (c LoggingConfig) validate() error {
	switch strings.ToLower(c.Level) {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("invalid log level: %q", c.Level)
	}
}
