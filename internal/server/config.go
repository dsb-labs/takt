// Package server provides the orca server and its configuration.
package server

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

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
		// Settings for the exec runtime.
		Exec ExecConfig `toml:"exec"`
		// Reconciliation settings.
		Reconcile ReconcileConfig `toml:"reconcile"`
		// Settings for the workloads orca runs.
		Workload WorkloadConfig `toml:"workload"`
		// Secret storage settings.
		Secrets SecretsConfig `toml:"secrets"`
		// Trace and log export settings.
		Telemetry TelemetryConfig `toml:"telemetry"`
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
		// The directory holding the keys a secret's value is encrypted under.
		//
		// A key is generated on first start if the directory is empty. Anything that
		// can read this directory can read every secret orca holds, so it is created
		// readable only by the user running the server — and it belongs on a backup,
		// because a secret sealed under a key that is gone cannot be recovered.
		//
		// A directory rather than a file because rotating a key writes the new one
		// before anything points at it. Each key is named by an identifier the
		// database records, so which one is current is something the database
		// answers.
		//
		// Empty puts the keyring beside the database, in the data directory.
		Keys string `toml:"keys"`
	}

	// The DockerConfig type contains configuration for talking to the Docker daemon.
	DockerConfig struct {
		// The daemon to connect to. Empty uses the environment's configuration,
		// which falls back to the local socket.
		Host string `toml:"host"`
		// The docker credential file registry credentials are resolved from when a
		// workload's image is pulled.
		//
		// Empty reads docker's own default location, so a docker login by the user
		// running the server just works. Set it when that default holds nothing —
		// notably when orca itself runs in a container. The file is read when a
		// pull happens rather than at startup, and an absent file means anonymous
		// pulls.
		ConfigFile string `toml:"config-file"`
	}

	// The ExecConfig type contains configuration for the exec runtime.
	ExecConfig struct {
		// Extra paths every exec workload may read.
		//
		// An exec workload is confined to its own directory, the volumes and values it
		// mounts, and the host's own system directories. That covers a command
		// installed the ordinary way and not one whose runtime lives elsewhere — a
		// language installed under a home directory, or a nix store. Name those here.
		//
		// Read-only, so this opens what a workload may read rather than what it may
		// change. It is host configuration rather than a manifest field on purpose: the
		// API has no authentication, so a workload able to widen its own confinement
		// would undo it. Which paths are opened is a decision the operator who
		// administers the host makes.
		//
		// Every path must be absolute. A path that is not there is ignored, so a list
		// covering several hosts does not have to match each one exactly.
		AllowPaths []string `toml:"allow-paths"`
	}

	// The ReconcileConfig type contains configuration for the reconciliation loop.
	ReconcileConfig struct {
		// How often a full reconciliation pass runs regardless of driver events.
		// Events make convergence prompt. This bounds how long a missed one can
		// go unnoticed.
		Interval time.Duration `toml:"interval"`
	}

	// The WorkloadConfig type contains configuration for the workloads orca runs:
	// the address their host ports are published on, and the range it allocates
	// those ports from.
	WorkloadConfig struct {
		// The address a workload's host ports are published on.
		//
		// Every interface by default. A published port is one something has to
		// reach, and the things that reach it include the other workloads on this
		// host: a container cannot dial a port published on loopback, since loopback
		// inside a container is its own. Name an interface's address to restrict
		// what a workload is exposed to.
		//
		// This is not the API's address, which stays on loopback. Reaching the API
		// is enough to run code on the host, where reaching a workload's port only
		// reaches what that workload serves.
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

	// The TelemetryConfig type contains configuration for exporting traces and
	// logs.
	//
	// Metrics need none of this: they are always collected and served by the
	// /metrics endpoint.
	TelemetryConfig struct {
		// The OTLP endpoint traces and logs are exported to over HTTP, as a URL
		// such as "http://collector.internal:4318". The scheme decides whether
		// the connection uses TLS. Empty exports neither, which is the default.
		//
		// Everything beyond the endpoint — headers, timeouts, sampling, resource
		// attributes — is read from the standard OTEL_* environment variables
		// the SDK already honours, rather than repeated here.
		OTLPEndpoint string `toml:"otlp-endpoint"`

		// Replaces the OTLP span exporter when set. Never read from the
		// configuration file. Tests use it to write spans somewhere they can
		// read.
		SpanExporter sdktrace.SpanExporter `toml:"-"`
		// Replaces the OTLP log exporter when set, under the same rules as
		// SpanExporter.
		LogExporter sdklog.Exporter `toml:"-"`
	}

	// The LoggingConfig type contains configuration for application logging.
	LoggingConfig struct {
		// The minimum level to emit. One of "debug", "info", "warn", "error".
		Level string `toml:"level"`
	}
)

const (
	// The address anything on this host reaches this host by when there is nothing
	// better to say.
	loopback = "127.0.0.1"
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
			// Every interface, unlike the API. A workload's port exists to be
			// reached, and one of the things reaching it is another workload on this
			// host: a container dialling a port published on loopback reaches its own
			// loopback rather than the host's, so loopback is the one value that
			// leaves workloads unable to reach each other.
			//
			// Named explicitly rather than left empty, because empty is what docker
			// reads as every interface. A reader should not have to know that to see
			// which of the two this is.
			Bind:    "0.0.0.0",
			MinPort: port.DefaultMin,
			MaxPort: port.DefaultMax,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// DatabasePath returns the SQLite database file inside the configured data
// directory.
//
// On the configuration rather than beside the server that reads it, because the
// server is not the only thing that needs to find it. Restoring a node writes the
// database back, and it has the configuration and nothing else to go on. A second
// copy of this join would be a second definition of where a node keeps its state,
// and the two drifting apart puts the database somewhere the server does not look.
func (c Config) DatabasePath() string {
	return filepath.Join(c.Data.Directory, "state.db")
}

// VolumesPath returns the directory holding every volume, one per identifier.
//
// Beside DatabasePath for the same reason. A volume is found by the identifier it
// was assigned, so restoring a node has to report which of those directories are
// not there — and it cannot ask the service that owns the layout, because that
// service only exists once a server is running.
func (c Config) VolumesPath() string {
	return filepath.Join(c.Data.Directory, "volumes")
}

// KeysPath returns the directory holding the secret encryption keys.
//
// Resolved here rather than defaulted in DefaultConfig, because the default sits
// inside the data directory and a configuration file may have moved that. A default
// computed before the file was read would point at the directory orca is not using.
func (c Config) KeysPath() string {
	if c.Secrets.Keys != "" {
		return c.Secrets.Keys
	}

	return filepath.Join(c.Data.Directory, "keys")
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

// Validate the configuration fields, resolving the data directory before anything
// reads it.
//
// The directory is made absolute here rather than left as written. A relative one
// produces workloads that cannot start: Landlock is given the path of a workload's
// own directory and docker is given the path of a volume, and both refuse a path that
// is not absolute. The failure then surfaces as a workload that never converges,
// which says nothing about the configuration that caused it.
//
// This is the ordinary case rather than an exotic one. The development configuration
// in the repository names ./data, and the fallback used when the home directory
// cannot be read is "data".
//
// An empty directory is left alone so that the check below reports it as missing.
// Resolving it would silently turn it into the working directory.
func (c *Config) Validate() error {
	if c.Data.Directory != "" {
		directory, err := filepath.Abs(c.Data.Directory)
		if err != nil {
			return fmt.Errorf("failed to resolve the data directory: %w", err)
		}

		c.Data.Directory = directory
	}

	return errors.Join(
		c.HTTP.validate(),
		c.Data.validate(),
		c.Reconcile.validate(),
		c.Workload.validate(),
		c.Docker.validate(),
		c.Exec.validate(),
		c.Telemetry.validate(),
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

// Address returns the address a workload dials to reach another workload's published
// ports.
//
// The bind address answers this whenever it names an interface: that is where the
// ports are published, and an operator who wrote one has already said which interface
// they meant. The unspecified address is the exception. It publishes on every
// interface and names none, and nothing can dial it — a process that tried would
// reach its own loopback rather than the host.
//
// So the unspecified address resolves to the address of the interface carrying the
// default route, which is the one anything on this host would reach the host by. A
// host with no route out has no such address, and loopback comes back along with the
// reason: an exec workload still reaches its neighbours there, and a container was
// never going to.
func (c WorkloadConfig) Address() (string, error) {
	if parsed := net.ParseIP(c.Bind); parsed != nil && !parsed.IsUnspecified() {
		return c.Bind, nil
	}

	// RFC-5737 (3): 192.0.2.0/24 is reserved for documentation and is not routed, so
	// this asks the kernel which local address it would send from without sending
	// anything. A UDP dial performs no handshake.
	conn, err := net.Dial("udp", "192.0.2.1:1")
	if err != nil {
		return loopback, fmt.Errorf("failed to resolve the address of this host: %w", err)
	}
	defer conn.Close()

	address, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return loopback, fmt.Errorf("failed to read the address of this host: %w", err)
	}

	return address, nil
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

func (c DockerConfig) validate() error {
	// Absolute when set, because the server's working directory is nowhere an
	// operator meant to keep credentials. Whether the file exists is deliberately
	// not checked: an absent file means anonymous pulls.
	if c.ConfigFile != "" && !filepath.IsAbs(c.ConfigFile) {
		return fmt.Errorf("docker config file must be absolute, got %q", c.ConfigFile)
	}

	return nil
}

func (c ExecConfig) validate() error {
	for _, path := range c.AllowPaths {
		// Absolute, because this opens a path to every exec workload and each one runs
		// in a directory of its own. A relative path would name somewhere different for
		// each of them, and nowhere the operator meant.
		if !filepath.IsAbs(path) {
			return fmt.Errorf("exec allowed path must be absolute, got %q", path)
		}
	}

	return nil
}

func (c TelemetryConfig) validate() error {
	if c.OTLPEndpoint == "" {
		return nil
	}

	// Parseable with a scheme and a host, and nothing more. Whether anything
	// answers there is deliberately not checked, matching the docker config file:
	// a collector that is down at startup is not a configuration error.
	endpoint, err := url.Parse(c.OTLPEndpoint)
	switch {
	case err != nil:
		return fmt.Errorf("telemetry otlp endpoint is not a valid url: %q", c.OTLPEndpoint)
	case endpoint.Scheme != "http" && endpoint.Scheme != "https":
		return fmt.Errorf("telemetry otlp endpoint must use http or https, got %q", c.OTLPEndpoint)
	case endpoint.Host == "":
		return fmt.Errorf("telemetry otlp endpoint must name a host, got %q", c.OTLPEndpoint)
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
