package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/orca/internal/server/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
	"github.com/dsb-labs/orca/internal/server/driver/exec"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/reconciler"
	"github.com/dsb-labs/orca/internal/server/secret"
	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/internal/server/telemetry"
)

// Run starts the orca server using the given configuration and blocks until the
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
	logger.With("address", config.HTTP.Address).Debug("starting orca server")

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
		Path:           filepath.Join(config.Data.Directory, "state.db"),
		MeterProvider:  tel.MeterProvider(),
		TracerProvider: tel.TracerProvider(),
	})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	dockerClient, err := docker.NewClient(config.Docker.Host)
	if err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}
	defer dockerClient.Close()

	// Loaded before anything that could seal a value, so a server that cannot reach
	// its key fails to start rather than accepting a secret it could not store.
	key, err := secret.LoadKey(config.KeyPath())
	if err != nil {
		return fmt.Errorf("failed to load secret encryption key: %w", err)
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
	checker := health.New(health.Config{
		Meter: tel.MeterProvider().Meter("github.com/dsb-labs/orca/internal/server/health"),
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

	// The exec driver keeps its own trees under the data directory, beside the
	// database, so that everything orca owns on disk is in one place.
	execDriver := exec.New(exec.Config{
		Logger: logger,
		Root:   filepath.Join(config.Data.Directory, "exec"),
		// Read-only paths beyond the host's own directories, for a runtime that lives
		// somewhere those do not cover.
		AllowPaths: config.Exec.AllowPaths,
	})

	dockerDriver := docker.New(docker.Config{
		Logger:     logger,
		Client:     dockerClient,
		Bind:       config.Workload.Bind,
		ConfigFile: config.Docker.ConfigFile,
		Meter:      tel.MeterProvider().Meter("github.com/dsb-labs/orca/internal/server/driver/docker"),
		Tracer:     tel.TracerProvider().Tracer("github.com/dsb-labs/orca/internal/server/driver/docker"),
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
	mountSvc := service.NewMountService(service.MountServiceConfig{
		Logger:    logger,
		Secrets:   secretSvc,
		Variables: variableSvc,
		Directory: config.Data.Directory,
	})

	// The drivers are keyed by the name each one declares, which is what a workload's
	// runtime is matched against. A runtime with no driver is stored and left alone.
	//
	// Each consumer gets a map of its own interface: the reconciler starts and stops
	// work where the service only reads, so neither is handed a driver wider than it
	// needs.
	reconcile := reconciler.New(reconciler.Config{
		Logger: logger,
		Drivers: map[string]reconciler.Driver{
			docker.Name: dockerDriver,
			exec.Name:   execDriver,
		},
		Workloads: workloads,
		Ports:     ports,
		// One resolver for both kinds, because one value in an environment may hold
		// both and expansion refuses what it cannot resolve.
		Env: service.NewEnvResolver(service.EnvResolverConfig{
			Logger:    logger,
			Secrets:   secretSvc,
			Variables: variableSvc,
		}),
		// Written as a workload starts and removed when it stops, so a mounted value's
		// plaintext is on the disk for no longer than the workload reading it.
		Mounts:  mountSvc,
		Checker: checker,
		// A check goes to where the workload's ports are published, which is not
		// loopback for a server told to publish somewhere specific.
		Bind: config.Workload.Bind,
		Reallocate: func(ctx context.Context, workload string) (bool, error) {
			return svc.Reallocate(ctx, workload)
		},
		Interval: config.Reconcile.Interval,
		Meter:    tel.MeterProvider().Meter("github.com/dsb-labs/orca/internal/server/reconciler"),
		Tracer:   tel.TracerProvider().Tracer("github.com/dsb-labs/orca/internal/server/reconciler"),
	})

	volumeSvc := service.NewVolumeService(service.VolumeServiceConfig{
		Logger:    logger,
		Volumes:   volumes,
		Directory: config.Data.Directory,
	})

	allocator := port.New(port.Config{Min: config.Workload.MinPort, Max: config.Workload.MaxPort})

	// The count of allocations is read from the repository once per scrape, so a
	// pass never pays for it. A gauge that cannot be registered costs the metric
	// rather than the server.
	err = allocator.RegisterMetrics(
		tel.MeterProvider().Meter("github.com/dsb-labs/orca/internal/server/port"),
		func(ctx context.Context) (map[port.Protocol]int, error) {
			allocated, err := ports.Allocated(ctx)
			if err != nil {
				return nil, err
			}

			return map[port.Protocol]int{port.ProtocolTCP: len(allocated)}, nil
		},
	)
	if err != nil {
		logger.With("error", err).Warn("failed to register port pool gauges")
	}

	svc = service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger: logger,
		Drivers: map[string]service.Driver{
			docker.Name: dockerDriver,
			exec.Name:   execDriver,
		},
		Workloads: workloads,
		Ports:     ports,
		Volumes:   volumeSvc,
		// The repositories rather than the services: hashing a workload needs what
		// each thing currently holds and nothing else.
		Secrets:    secrets,
		Variables:  variables,
		Images:     dockerDriver,
		Allocator:  allocator,
		Checker:    checker,
		Reconciler: reconcile,
	})

	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: svc}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: volumeSvc}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: secretSvc}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: variableSvc}),
		System: api.NewSystemAPI(api.SystemAPIConfig{
			Logger: logger,
			DB:     db,
			// The reconciler itself, because a cached view of how each driver
			// last answered is something only the passes asking them can hold.
			Observer: reconcile,
			Metrics:  tel.Gatherer(),
		}),
	}).Register(mux)

	server := &http.Server{
		Addr: config.HTTP.Address,
		// Outermost on purpose, outside even the middleware: a request the
		// middleware refuses — an unpermitted host, an oversized body — is
		// still a request the server answered, and one worth measuring.
		// Outside the telemetry handler, which is the only place a handler can still
		// reach the connection's own writer: everything below wraps it, and none of
		// those wrappers carries a write deadline.
		Handler: api.Stream(otelhttp.NewHandler(api.Wrap(mux, logger, config.HTTP.Hosts), "orca",
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

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return reconcile.Run(ctx) })
	g.Go(func() error { return checker.Run(ctx) })
	g.Go(func() error {
		<-ctx.Done()

		// Processes the exec driver started keep running, so that restarting the
		// server is not the same thing as restarting the workloads it runs. The next
		// start adopts them from the records the driver left behind.
		execDriver.Release()

		return nil
	})
	g.Go(server.ListenAndServe)
	g.Go(func() error {
		<-ctx.Done()

		logger.Debug("shutting down http server")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		return server.Shutdown(shutdownCtx)
	})

	logger.With("address", config.HTTP.Address).Info("orca server listening")

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
