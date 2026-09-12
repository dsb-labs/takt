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

type (
	// The Config type contains the settings a client connects with.
	//
	// There is no flag for the token, deliberately: a token passed as a flag
	// lands in shell history and process listings for no benefit the other
	// sources lack.
	Config struct {
		// The address of the takt server, as a URL.
		Address string `toml:"address"`
		// The token presented as a bearer credential. Empty sends no credential.
		Token string `toml:"token"`
		// The path of a PEM certificate authority file to check the server's
		// certificate against instead of the system roots.
		CACert string `toml:"ca_cert"`
		// The file the settings were read from, and where Save writes them
		// back. Not a setting itself: Load records it, and it never appears
		// in the file.
		File string `toml:"-"`
	}

	// The Sources type names the flag values Resolve folds in. An empty field
	// means the flag was not given, so the environment and the file stay
	// visible behind it.
	Sources struct {
		// The --config flag value, naming the config file. Empty resolves
		// the file the way Path does.
		ConfigPath string
		// The --address flag value.
		Address string
		// The --ca-cert flag value.
		CACert string
	}
)

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
// a Config with no settings: a client that never logged in simply has no
// file yet. Either way the result records the path, so Save knows where the
// settings belong.
func Load(path string) (Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{File: path}, nil
		}

		return Config{}, fmt.Errorf("failed to decode config file: %w", err)
	}

	config.File = path

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
// field from the most specific source that supplies it: the flag values in
// sources, then the TAKT_ADDRESS, TAKT_TOKEN and TAKT_CA_CERT environment
// variables, then the config file the sources name.
//
// The address falls back to DefaultAddress when nothing supplies one, so a
// client on the same host as its server needs no configuration at all.
func Resolve(sources Sources) (Config, error) {
	path, err := Path(sources.ConfigPath)
	if err != nil {
		return Config{}, err
	}

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

	if sources.Address != "" {
		config.Address = sources.Address
	}
	if sources.CACert != "" {
		config.CACert = sources.CACert
	}

	if config.Address == "" {
		config.Address = DefaultAddress
	}

	return config, nil
}

// Save writes the settings back to the file they were read from, creating it
// for a client that has never logged in. It refuses settings that record no
// file, which is what a Config built by hand rather than by Load or Resolve
// carries.
func (c Config) Save() error {
	if c.File == "" {
		return errors.New("the settings record no file to save to")
	}

	return Write(c.File, c)
}
