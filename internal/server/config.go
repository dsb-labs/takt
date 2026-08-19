// Package server provides the orca server and its configuration.
package server

import (
	"errors"
	"fmt"
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
		// Host port allocation settings.
		Ports PortsConfig `toml:"ports"`
		// Logging settings.
		Logging LoggingConfig `toml:"logging"`
	}

	// The HTTPConfig type contains configuration for the HTTP listener.
	HTTPConfig struct {
		// The address the HTTP server binds to, in host:port form.
		Address string `toml:"address"`
	}

	// The DataConfig type contains configuration for the server's on-disk state.
	DataConfig struct {
		// The directory the SQLite database is stored in.
		Directory string `toml:"directory"`
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

	// The PortsConfig type contains configuration for the host ports orca allocates
	// to workloads that don't ask for a particular one.
	PortsConfig struct {
		// The lowest host port that may be allocated.
		Min int `toml:"min"`
		// The highest host port that may be allocated.
		Max int `toml:"max"`
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
			Address: ":7373",
		},
		Data: DataConfig{
			Directory: defaultDataDir(),
		},
		Reconcile: ReconcileConfig{
			Interval: 10 * time.Second,
		},
		Ports: PortsConfig{
			Min: port.DefaultMin,
			Max: port.DefaultMax,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
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
		c.Ports.validate(),
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

func (c PortsConfig) validate() error {
	switch {
	case c.Min < 1 || c.Min > 65535:
		return errors.New("port range minimum must be between 1 and 65535")
	case c.Max < 1 || c.Max > 65535:
		return errors.New("port range maximum must be between 1 and 65535")
	case c.Min > c.Max:
		return errors.New("port range minimum must not exceed its maximum")
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
