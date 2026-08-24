package docker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/distribution/reference"
	"github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types/registry"
)

// The key docker's credential file stores Docker Hub logins under. The file
// predates the docker.io registry name, so a login against Docker Hub is keyed
// by the legacy index address rather than the domain an image reference
// carries. Docker's own cli hardcodes the same mapping.
const indexServer = "https://index.docker.io/v1/"

// registryAuth resolves the credentials the docker credential file holds for
// the registry the given image reference names, returning them in the encoded
// form the Docker Engine API takes. An empty string means the registry is
// asked anonymously, which is what an absent file or a registry the file does
// not mention resolves to.
//
// The file is read on every call rather than once at startup, so a docker
// login on the host takes effect without restarting the server. Resolution
// runs the credential helpers the file can name, so a host keeping its logins
// in the OS keychain works without holding a password in the file at all.
//
// The resolved credential must never appear in a log line or an error: a
// failure names the registry, not what was sent to it.
func (d *Driver) registryAuth(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("failed to parse image reference: %w", err)
	}

	key := reference.Domain(named)
	if key == "docker.io" {
		key = indexServer
	}

	path := d.configFile
	if path == "" {
		path = filepath.Join(config.Dir(), config.ConfigFileName)
	}

	file, err := os.Open(path)
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err != nil:
		// An unreadable file means anonymous pulls rather than a failure, so a
		// host that never logged in to anything keeps working. The warning is
		// the trail an operator follows when a private pull later fails.
		d.logger.With("error", err, "path", path).Warn("failed to read docker config file")
		return "", nil
	}
	defer file.Close()

	configFile, err := config.LoadFromReader(file)
	if err != nil {
		return "", fmt.Errorf("failed to parse docker config file: %w", err)
	}

	auth, err := configFile.GetAuthConfig(key)
	if err != nil {
		return "", fmt.Errorf("failed to resolve credentials for registry %q: %w", key, err)
	}

	resolved := registry.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Password,
		Auth:          auth.Auth,
		ServerAddress: auth.ServerAddress,
		IdentityToken: auth.IdentityToken,
		RegistryToken: auth.RegistryToken,
	}

	// Anonymous stays the empty string rather than an encoded empty credential,
	// so a registry the file does not mention sees exactly the request it saw
	// before the driver knew about credentials.
	if resolved == (registry.AuthConfig{}) {
		return "", nil
	}

	d.logger.With("registry", key).Debug("resolved registry credentials")

	encoded, err := registry.EncodeAuthConfig(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to encode registry credentials: %w", err)
	}

	return encoded, nil
}
