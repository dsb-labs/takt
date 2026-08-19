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

	if err := os.MkdirAll(config.Data.Directory, 0o755); err != nil {
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
	driver := docker.New(docker.Config{Logger: logger, Client: dockerClient})

	// The service and the reconciler each need something from the other: the service
	// wakes the reconciler when desired state changes, and the reconciler asks the
	// service to reallocate ports that turned out to be unusable. Both are passed as
	// functions so neither has to be half-constructed to build the other.
	var svc *service.WorkloadService

	reconcile := reconciler.New(reconciler.Config{
		Logger:    logger,
		Driver:    driver,
		Workloads: workloads,
		Reallocate: func(ctx context.Context, workload string) (bool, error) {
			return svc.Reallocate(ctx, workload)
		},
		Interval: config.Reconcile.Interval,
	})

	svc = service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    logger,
		Driver:    driver,
		Workloads: workloads,
		Ports:     ports,
		Allocator: port.New(port.Config{Min: config.Ports.Min, Max: config.Ports.Max}),
		Notify:    reconcile.Notify,
	})

	mux := http.NewServeMux()
	api.NewWorkloadAPI(svc).Register(mux)

	var handler http.Handler = mux
	for _, middleware := range []func(http.Handler) http.Handler{
		api.Recovery(logger),
		api.Logging(logger),
		api.Limit,
	} {
		handler = middleware(handler)
	}

	server := &http.Server{
		Addr:              config.HTTP.Address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return reconcile.Run(ctx) })
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
