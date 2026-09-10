// Package cli provides the connection settings a takt client resolves from
// flags, the environment and the config file, in that order.
//
// The file is the human path: `takt auth login` writes the token it minted
// here, because a child process cannot set an environment variable in its
// parent shell. The environment is the machine path: CI injects TAKT_TOKEN in
// one line. Flags are the most specific and win over both. Exported so that
// anyone writing their own takt client can honour the same settings.
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// The Config type contains the settings a client connects with.
//
// There is no flag for the token, deliberately: a token passed as a flag
// lands in shell history and process listings for no benefit the other
// sources lack.
type Config struct {
	// The address of the takt server, as a URL.
	Address string `toml:"address"`
	// The token presented as a bearer credential. Empty sends no credential.
	Token string `toml:"token"`
	// The path of a PEM certificate authority file to check the server's
	// certificate against instead of the system roots.
	CACert string `toml:"ca_cert"`
}

// DefaultAddress is where a client connects when nothing names a server.
const DefaultAddress = "http://localhost:7373"

// The environment variables the settings are read from.
const (
	envAddress = "TAKT_ADDRESS"
	envToken   = "TAKT_TOKEN"
	envCACert  = "TAKT_CA_CERT"
	envConfig  = "TAKT_CONFIG"
)

// Path returns where the config file lives: the given --config flag value,
// what TAKT_CONFIG names, or .takt/config under the home directory. Pointing
// either at another file is the multi-server and multi-identity answer.
func Path(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}

	if path := os.Getenv(envConfig); path != "" {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve the home directory: %w", err)
	}

	return filepath.Join(home, ".takt", "config"), nil
}

// Load reads the config file at the given path, reporting an absent file as
// the zero Config: a client that never logged in simply has no file yet.
func Load(path string) (Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}

		return Config{}, fmt.Errorf("failed to decode config file: %w", err)
	}

	return config, nil
}

// Write stores the config file at the given path, creating its directory
// readable only by the owner and the file the same way, because the file
// holds a credential.
func Write(path string, config Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	defer f.Close()

	if err = toml.NewEncoder(f).Encode(config); err != nil {
		return fmt.Errorf("failed to encode config file: %w", err)
	}

	return nil
}

// Resolve returns the settings a client should connect with, taking each
// field from the most specific source that supplies it: the given flag
// values, then the TAKT_ADDRESS, TAKT_TOKEN and TAKT_CA_CERT environment
// variables, then the config file at the given path. An empty flag value
// means the flag was not given.
//
// The address falls back to DefaultAddress when nothing supplies one, so a
// client on the same host as its server needs no configuration at all.
func Resolve(path, address, caCert string) (Config, error) {
	config, err := Load(path)
	if err != nil {
		return Config{}, err
	}

	if value := os.Getenv(envAddress); value != "" {
		config.Address = value
	}
	if value := os.Getenv(envToken); value != "" {
		config.Token = value
	}
	if value := os.Getenv(envCACert); value != "" {
		config.CACert = value
	}

	if address != "" {
		config.Address = address
	}
	if caCert != "" {
		config.CACert = caCert
	}

	if config.Address == "" {
		config.Address = DefaultAddress
	}

	return config, nil
}
