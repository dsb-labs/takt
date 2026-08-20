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

	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/orca/internal/server/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/reconciler"
	"github.com/dsb-labs/orca/internal/server/service"
)

// Run starts the orca server using the given configuration and blocks until the
// context is cancelled or the server stops with an error.
func Run(ctx context.Context, config Config) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	logger := newLogger(config.Logging)
	logger.With("address", config.HTTP.Address).Debug("starting orca server")

	// Only the owner: the database holds every workload's specification, and a
	// specification carries whatever the operator put in its environment. A
	// world-readable directory would publish those to every local account.
	if err := os.MkdirAll(config.Data.Directory, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	db, err := database.Open(ctx, database.Config{
		Logger: logger,
		Path:   filepath.Join(config.Data.Directory, "state.db"),
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

	workloads := database.NewWorkloadRepository(db)
	ports := database.NewPortRepository(db)
	checker := health.New()

	// Keyed by the name each driver declares, which is what a workload's runtime is
	// matched against. A runtime with no driver here is stored and left alone.
	runtimes := map[string]*docker.Driver{
		docker.Name: docker.New(docker.Config{Logger: logger, Client: dockerClient}),
	}

	// The service and the reconciler each need something from the other: the service
	// wakes the reconciler when desired state changes, and the reconciler asks the
	// service to reallocate ports that turned out to be unusable. Both are passed as
	// functions so neither has to be half-constructed to build the other.
	var svc *service.WorkloadService

	reconcile := reconciler.New(reconciler.Config{
		Logger:    logger,
		Drivers:   reconcilerDrivers(runtimes),
		Workloads: workloads,
		Ports:     ports,
		Checker:   checker,
		Reallocate: func(ctx context.Context, workload string) (bool, error) {
			return svc.Reallocate(ctx, workload)
		},
		Interval: config.Reconcile.Interval,
	})

	svc = service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    logger,
		Drivers:   serviceDrivers(runtimes),
		Workloads: workloads,
		Ports:     ports,
		Allocator: port.New(port.Config{Min: config.Ports.Min, Max: config.Ports.Max}),
		Checker:   checker,
		Notify:    reconcile.Notify,
	})

	mux := http.NewServeMux()
	api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: svc}).Register(mux)

	var handler http.Handler = mux
	for _, middleware := range []func(http.Handler) http.Handler{
		api.Recovery(logger),
		api.Logging(logger),
		api.Limit,
	} {
		handler = middleware(handler)
	}

	server := &http.Server{
		Addr:    config.HTTP.Address,
		Handler: handler,
		// A client that opens a connection and then stalls — mid-header, mid-body, or
		// while reading a response — otherwise holds it indefinitely. These bound how
		// long any one request may occupy the server.
		//
		// WriteTimeout is deliberately generous: reading a workload's logs streams
		// its output, and a large tail from a chatty container legitimately takes
		// longer than answering an ordinary request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return reconcile.Run(ctx) })
	g.Go(func() error { return checker.Run(ctx) })
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

func newLogger(config LoggingConfig) *slog.Logger {
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

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// reconcilerDrivers adapts the drivers onto the interface the reconciler consumes.
//
// The reconciler and the service take deliberately different views of a driver — one
// starts and stops work, the other only reads — so each is handed a map of its own
// interface rather than sharing one wider than either needs. Assigning each driver
// explicitly keeps that a compile-time check.
func reconcilerDrivers(runtimes map[string]*docker.Driver) map[string]reconciler.Driver {
	out := make(map[string]reconciler.Driver, len(runtimes))
	for name, runtime := range runtimes {
		out[name] = runtime
	}

	return out
}

// serviceDrivers adapts the drivers onto the interface the service consumes.
func serviceDrivers(runtimes map[string]*docker.Driver) map[string]service.Driver {
	out := make(map[string]service.Driver, len(runtimes))
	for name, runtime := range runtimes {
		out[name] = runtime
	}

	return out
}
