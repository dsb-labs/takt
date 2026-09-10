//go:build linux

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/certificate"
	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver/docker"
	"github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/mount"
	"github.com/dsb-labs/takt/internal/server/port"
	"github.com/dsb-labs/takt/internal/server/reconciler"
	"github.com/dsb-labs/takt/internal/server/resolve"
	"github.com/dsb-labs/takt/internal/server/secret"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/internal/server/telemetry"
	"github.com/dsb-labs/takt/internal/ui"
)

// Run starts the takt server using the given configuration and blocks until the
// context is cancelled or the server stops with an error.
func Run(ctx context.Context, config Config) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Before the logger, because when log export is configured the logger fans
	// out into the telemetry pipeline — and everything below builds from the
	// logger, so this is what puts every component's records on the wire.
	tel, err := telemetry.New(ctx, telemetry.Config{
		Endpoint:     config.Telemetry.OTLPEndpoint,
		SpanExporter: config.Telemetry.SpanExporter,
		LogExporter:  config.Telemetry.LogExporter,
	})
	if err != nil {
		return fmt.Errorf("failed to construct telemetry: %w", err)
	}

	logger := newLogger(config.Logging, tel.LogHandler())
	logger.With("address", config.HTTP.Address).Debug("starting takt server")

	defer func() {
		// Exported signals are batched, so this flush is what makes the last
		// spans and logs of a run reach their exporter. Bounded so a collector
		// that stopped answering cannot hold up shutdown.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := tel.Shutdown(shutdownCtx); err != nil {
			logger.With("error", err).Warn("failed to shut down telemetry")
		}
	}()

	// Only the owner: the database holds every workload's specification, and a
	// specification carries whatever the operator put in its environment. A
	// world-readable directory would publish those to every local account.
	if err := os.MkdirAll(config.Data.Directory, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	db, err := database.Open(ctx, database.Config{
		Logger:         logger,
		Path:           config.DatabasePath(),
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
	})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	dockerClient, err := docker.NewClient(ctx, config.Docker.Host)
	if err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}
	defer dockerClient.Close()

	// Opened before anything that could seal a value, so a server that cannot reach
	// its keys fails to start rather than accepting a secret it could not store.
	keys, err := secret.NewStore(config.KeysPath())
	if err != nil {
		return fmt.Errorf("failed to open the keyring: %w", err)
	}

	encryptionKeys := database.NewEncryptionKeyRepository(db)

	keyID, err := currentKey(ctx, keys, encryptionKeys)
	if err != nil {
		return err
	}

	key, err := keys.Read(keyID)
	if err != nil {
		return fmt.Errorf("failed to read the secret encryption key: %w", err)
	}

	cipher, err := secret.New(key)
	if err != nil {
		return fmt.Errorf("failed to construct secret cipher: %w", err)
	}

	workloads := database.NewWorkloadRepository(db)
	ports := database.NewPortRepository(db)
	volumes := database.NewVolumeRepository(db)
	secrets := database.NewSecretRepository(db)
	variables := database.NewVariableRepository(db)
	tokens := database.NewTokenRepository(db)

	// Recovering from a lost recovery token is a host-level act: write the
	// reset file into the data directory and restart. Consuming it here
	// removes the recovery token, which is exactly the state that permits
	// `takt acl init` to run again. Client tokens and the policy survive.
	if _, err = os.Stat(config.ACLResetPath()); err == nil {
		if err = tokens.DeleteRecovery(ctx); err != nil {
			return fmt.Errorf("failed to consume the acl reset file: %w", err)
		}

		if err = os.Remove(config.ACLResetPath()); err != nil {
			return fmt.Errorf("failed to remove the acl reset file: %w", err)
		}

		logger.Warn("acl reset consumed, init is permitted again")
	}

	checker := health.New(health.Config{
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
	})

	// A host that cannot confine an exec workload is reported here rather than when the
	// first one is started, so an operator learns at startup instead of from a workload
	// that will not run.
	//
	// Not fatal: the docker runtime is unaffected, and refusing to serve at all would
	// take container workloads down over a runtime the operator may not be using.
	if err = exec.Confinable(); err != nil {
		logger.With("error", err).Warn("exec workloads cannot run on this host and will be refused")
	}

	// Reported at startup for the reason confinement is, and separately from it:
	// a host can confine without delegating a cgroup subtree, and only workloads
	// asking for resource limits are refused by the answer here.
	if err = exec.Enforceable(); err != nil {
		logger.With("error", err).Warn("exec resource limits cannot be enforced on this host and will be refused")
	}

	// The exec driver keeps its own trees under the data directory, beside the
	// database, so that everything takt owns on disk is in one place.
	execDriver := exec.New(exec.Config{
		Logger: logger,
		Root:   filepath.Join(config.Data.Directory, "exec"),
		// Read-only paths beyond the host's own directories, for a runtime that lives
		// somewhere those do not cover.
		AllowPaths: config.Exec.AllowPaths,
	})

	dockerDriver := docker.New(docker.Config{
		Logger:         logger,
		Client:         dockerClient,
		Bind:           config.Workload.Bind,
		ConfigFile:     config.Docker.ConfigFile,
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
	})

	// The service and the reconciler each need something from the other: the service
	// wakes the reconciler when desired state changes, and the reconciler asks the
	// service to reallocate ports that turned out to be unusable. Both are passed as
	// functions so neither has to be half-constructed to build the other.
	//
	// The secret and variable services need the same treatment for the same reason:
	// changing either has to rehash the workloads reading it.
	var svc *service.WorkloadService

	// One closure serves both, because Rehash recomputes against whatever a workload
	// references rather than against what it was told changed.
	rehash := func(ctx context.Context, workload string) error {
		_, err := svc.Rehash(ctx, workload)

		return err
	}

	secretSvc := service.NewSecretService(service.SecretServiceConfig{
		Logger:  logger,
		Secrets: secrets,
		Cipher:  cipher,
		KeyID:   keyID,
		Rehash:  rehash,
	})

	variableSvc := service.NewVariableService(service.VariableServiceConfig{
		Logger:    logger,
		Variables: variables,
		Rehash:    rehash,
	})

	// The services rather than the repositories, because materialising a mounted value
	// means reading the value itself — which for a secret is decryption, and lives
	// behind the secret service.
	mounter := mount.New(mount.Config{
		Logger:    logger,
		Secrets:   secretSvc,
		Variables: variableSvc,
		Directory: config.Data.Directory,
	})

	// The address a workload dials to reach another workload's published ports.
	// Resolved once at startup rather than per reference: it is a property of this
	// host and its configuration, neither of which moves while the server runs.
	workloadAddress, err := config.Workload.Address()
	if err != nil {
		logger.With("error", err, "address", workloadAddress).
			Warn("this host has no routable address, so only exec workloads can reach another workload")
	}

	addresses := resolve.NewAddressResolver(resolve.AddressResolverConfig{
		Logger:    logger,
		Workloads: workloads,
		Ports:     ports,
		Address:   workloadAddress,
	})

	// The drivers are keyed by the name each one declares, which is what a workload's
	// runtime is matched against. A runtime with no driver is stored and left alone.
	//
	// Each consumer gets a map of its own interface: the reconciler starts and stops
	// work where the service only reads, so neither is handed a driver wider than it
	// needs.
	reconcile := reconciler.New(reconciler.Config{
		Logger: logger,
		// Wrapped so that every driver explains a converge the same way,
		// whatever instrumentation the runtime behind it carries.
		Drivers: map[string]reconciler.Driver{
			docker.Name: telemetry.WrapDriver(dockerDriver, tel.TracerProvider()),
			exec.Name:   telemetry.WrapDriver(execDriver, tel.TracerProvider()),
		},
		Workloads: workloads,
		Ports:     ports,
		// One resolver for both kinds, because one value in an environment may hold
		// both and expansion refuses what it cannot resolve.
		Env: resolve.NewEnvResolver(resolve.EnvResolverConfig{
			Logger:    logger,
			Secrets:   secretSvc,
			Variables: variableSvc,
			Workloads: addresses,
		}),
		// Written as a workload starts and removed when it stops, so a mounted value's
		// plaintext is on the disk for no longer than the workload reading it.
		Mounts:  mounter,
		Checker: checker,
		// A check goes to where the workload's ports are published, which is not
		// loopback for a server told to publish somewhere specific.
		Bind: config.Workload.Bind,
		Reallocate: func(ctx context.Context, workload string, instance int) (bool, error) {
			return svc.ReallocateInstance(ctx, workload, instance)
		},
		Interval:       config.Reconcile.Interval,
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
	})

	volumeSvc := service.NewVolumeService(service.VolumeServiceConfig{
		Logger:  logger,
		Volumes: volumes,
		Root:    config.VolumesPath(),
	})

	adminSvc := service.NewAdminService(service.AdminServiceConfig{
		Logger:   logger,
		Database: config.DatabasePath(),
		Keys:     keys,
		Secrets:  secretSvc,
	})

	tokenSvc := service.NewTokenService(service.TokenServiceConfig{
		Logger: logger,
		Tokens: tokens,
	})

	policySvc := service.NewPolicyService(service.PolicyServiceConfig{
		Logger:   logger,
		Policies: database.NewPolicyRepository(db),
	})

	// All are nil without OIDC, which is how the login exchanges and the
	// browser flow answer that they are not configured.
	var (
		verifier     service.IdentityVerifier
		exchanger    service.IdentityExchanger
		relyingParty *api.OIDCRelyingParty
	)

	if config.Auth.OIDCEnabled() {
		oidcVerifier, endpoint, err := service.NewOIDCVerifier(ctx, config.Auth.OIDC.Issuer, config.Auth.OIDC.ClientID)
		if err != nil {
			return err
		}

		verifier = oidcVerifier
		// The CLI's loopback flow hands its authorization code to the server,
		// because the exchange is what needs the client secret.
		exchanger = service.NewOIDCExchanger(config.Auth.OIDC.ClientID, config.Auth.OIDC.ClientSecret, endpoint)
		relyingParty = &api.OIDCRelyingParty{
			Issuer:   config.Auth.OIDC.Issuer,
			ClientID: config.Auth.OIDC.ClientID,
		}

		// The browser flow needs somewhere the issuer can send the browser
		// back to, so it activates on the redirect URL being named. The CLI
		// runs its own loopback flow and needs none of this.
		if config.Auth.OIDC.RedirectURL != "" {
			scopes := config.Auth.OIDC.Scopes
			if len(scopes) == 0 {
				scopes = []string{"openid", "email", "profile"}
			}

			relyingParty.Flow = &oauth2.Config{
				ClientID:     config.Auth.OIDC.ClientID,
				ClientSecret: config.Auth.OIDC.ClientSecret,
				Endpoint:     endpoint,
				RedirectURL:  strings.TrimSuffix(config.Auth.OIDC.RedirectURL, "/") + "/api/v1/auth/oidc/callback",
				Scopes:       scopes,
			}
		}
	}

	authSvc := service.NewAuthService(service.AuthServiceConfig{
		Logger:    logger,
		Tokens:    tokens,
		Policies:  policySvc,
		Verifier:  verifier,
		Exchanger: exchanger,
	})

	// A nil authenticator is how the middleware knows the layer is off. The
	// interface is only given a value when the [auth] block is present, so
	// an absent block costs no lookups on any request.
	var authenticator middleware.Authenticator
	if config.Auth != nil {
		authenticator = authSvc
	}

	// The count of allocations is read from the repository once per scrape, so a
	// pass never pays for it.
	allocator := port.New(port.Config{
		Min:            config.Workload.MinPort,
		Max:            config.Workload.MaxPort,
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
		Allocated:      ports.Allocated,
	})

	claimer := port.NewClaimer(port.ClaimerConfig{Allocator: allocator, Ports: ports})

	svc = service.NewWorkloadService(service.WorkloadServiceConfig{
		Address: workloadAddress,
		Logger:  logger,
		Drivers: map[string]service.Driver{
			docker.Name: dockerDriver,
			exec.Name:   execDriver,
		},
		Workloads: workloads,
		Ports:     ports,
		Volumes:   volumeSvc,
		// The repositories rather than the services: hashing a workload needs what
		// each thing currently holds and nothing else.
		Secrets:        secrets,
		Variables:      variables,
		Addresses:      addresses,
		Images:         dockerDriver,
		Claimer:        claimer,
		Checker:        checker,
		Reconciler:     reconcile,
		AllowHostPaths: config.Workload.AllowHostPaths,
	})

	serviceSvc := service.NewServiceService(service.ServiceServiceConfig{
		Logger:   logger,
		Services: database.NewServiceRepository(db),
		// The workload service rather than the repository: a backend is a
		// question about observed instances, which only the hydrated listing
		// answers.
		Workloads: svc,
		Address:   workloadAddress,
	})

	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: svc}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: volumeSvc}),
		Services:  api.NewServiceAPI(api.ServiceAPIConfig{Logger: logger, Services: serviceSvc}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: secretSvc}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: variableSvc}),
		System: api.NewSystemAPI(api.SystemAPIConfig{
			Logger: logger,
			DB:     db,
			// The reconciler itself, because a cached view of how each driver
			// last answered is something only the passes asking them can hold.
			Observer: reconcile,
			Metrics:  tel.Gatherer(),
			Targets:  svc,
		}),
		Admin: api.NewAdminAPI(api.AdminAPIConfig{Logger: logger, Admin: adminSvc}),
		Auth: api.NewAuthAPI(api.AuthAPIConfig{
			Logger: logger,
			Auth:   authSvc,
			OIDC:   relyingParty,
			Secure: config.HTTP.TLSEnabled(),
		}),
		ACL:    api.NewACLAPI(api.ACLAPIConfig{Logger: logger, Policies: policySvc, Init: tokenSvc}),
		Tokens: api.NewTokenAPI(api.TokenAPIConfig{Logger: logger, Tokens: tokenSvc}),
	}).Register(mux)

	webUI, err := ui.Handler()
	if err != nil {
		return fmt.Errorf("failed to load the web ui: %w", err)
	}

	// The UI holds the root and the API holds /api/v1, and the mux prefers the
	// most specific pattern, so every API route wins over this catch-all. It is
	// registered for GET only: the UI is pages and assets, and nothing writes
	// to it.
	mux.Handle("GET /", webUI)

	server := &http.Server{
		Addr: config.HTTP.Address,
		// Outermost on purpose, outside even the middleware: a request the
		// middleware refuses — an unpermitted host, an oversized body — is
		// still a request the server answered, and one worth measuring.
		// Outside the telemetry handler, which is the only place a handler can still
		// reach the connection's own writer: everything below wraps it, and none of
		// those wrappers carries a write deadline.
		Handler: middleware.Stream(otelhttp.NewHandler(middleware.Wrap(mux, logger, config.HTTP.Hosts, authenticator), "takt",
			otelhttp.WithMeterProvider(tel.MeterProvider()),
			otelhttp.WithTracerProvider(tel.TracerProvider()),
			otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{}, propagation.Baggage{},
			)),
		)),
		// A client that opens a connection and then stalls — mid-header, mid-body, or
		// while reading a response — otherwise holds it indefinitely. These bound how
		// long any one request may occupy the server.
		//
		// WriteTimeout is deliberately generous: reading a workload's logs streams
		// its output, and a large tail from a chatty container legitimately takes
		// longer than answering an ordinary request.
		//
		// A followed read clears it altogether, from the handler rather than here.
		// That response is open for as long as the workload runs, which no deadline
		// set for every request could allow for.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	if config.HTTP.TLSEnabled() {
		// Loaded here rather than at the first handshake, so a pair the server
		// cannot present stops it from starting instead of failing every
		// connection.
		loader, err := certificate.New(certificate.Config{
			Logger:      logger,
			Certificate: config.HTTP.TLSCert,
			Key:         config.HTTP.TLSKey,
		})
		if err != nil {
			return fmt.Errorf("failed to load tls certificate: %w", err)
		}

		server.TLSConfig = &tls.Config{
			GetCertificate: loader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	// Opened here rather than by Serve, so readiness is signalled only once
	// the address is bound.
	listener, err := net.Listen("tcp", config.HTTP.Address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", config.HTTP.Address, err)
	}

	serve := func() error { return server.Serve(listener) }
	if config.HTTP.TLSEnabled() {
		serve = func() error { return server.ServeTLS(listener, "", "") }
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return reconcile.Run(ctx) })
	g.Go(func() error { return checker.Run(ctx) })
	g.Go(func() error {
		// An expired token already refuses to authenticate, so the sweep is
		// hygiene for the token list rather than security — hourly is plenty.
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if err := tokenSvc.Sweep(ctx); err != nil {
					logger.With("error", err).Warn("failed to sweep expired tokens")
				}
			}
		}
	})
	g.Go(func() error {
		<-ctx.Done()

		// Processes the exec driver started keep running, so that restarting the
		// server is not the same thing as restarting the workloads it runs. The next
		// start adopts them from the records the driver left behind.
		execDriver.Release()

		return nil
	})
	g.Go(serve)
	g.Go(func() error {
		<-ctx.Done()

		logger.Debug("shutting down http server")

		if err := notify("STOPPING=1"); err != nil {
			logger.With("error", err).Warn("failed to signal stopping to the service manager")
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		return server.Shutdown(shutdownCtx)
	})

	logger.With("address", config.HTTP.Address, "tls", config.HTTP.TLSEnabled()).Info("takt server listening")

	if err = notify("READY=1"); err != nil {
		logger.With("error", err).Warn("failed to signal readiness to the service manager")
	}

	err = g.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

// newLogger returns the server's logger: a text handler on stderr at the
// configured level, joined with the extra handler when one is given.
//
// The multi handler dispatches on each handler's own level, so a quiet stderr
// does not censor what an exporting handler carries.
func newLogger(config LoggingConfig, extra slog.Handler) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(config.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	if extra != nil {
		handler = slog.NewMultiHandler(handler, extra)
	}

	return slog.New(handler)
}

// notify sends the given state to the service manager listening on the socket
// named by NOTIFY_SOCKET, following the sd_notify(3) protocol. Outside a
// service manager the variable is empty and notify does nothing.
func notify(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}

	// A socket in the abstract namespace is named with a leading '@' in the
	// variable and a leading NUL byte on the wire.
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:]
	}

	conn, err := net.Dial("unixgram", socket)
	if err != nil {
		return fmt.Errorf("failed to dial the notify socket: %w", err)
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("failed to write to the notify socket: %w", err)
	}

	return nil
}

// currentKey returns the identifier of the key secrets are sealed under, generating
// one when the database records none.
//
// The database is what answers this, not the keyring. A keyring may hold several
// keys — a rekey keeps the one it replaced, because that key still opens the backups
// taken before it — so which of them seals what is a question only the rows can
// answer.
func currentKey(ctx context.Context, keys *secret.Store, recorded *database.EncryptionKeyRepository) (string, error) {
	current, err := recorded.Current(ctx)
	switch {
	case err == nil:
		return current.ID, nil
	case !errors.Is(err, database.ErrNoCurrentKey):
		return "", fmt.Errorf("failed to read the current encryption key: %w", err)
	}

	// Written to the keyring before it is recorded. A key recorded with nothing
	// behind it would leave every secret sealed under it unopenable, where a key
	// nothing records is inert and swept later.
	id, err := keys.Create()
	if err != nil {
		return "", fmt.Errorf("failed to generate a secret encryption key: %w", err)
	}

	if err = recorded.Adopt(ctx, id); err != nil {
		return "", err
	}

	return id, nil
}
