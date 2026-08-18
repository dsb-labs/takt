// Package service provides the domain orchestration layer for the orca server.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
)

var (
	// ErrWorkloadNotFound is returned when the requested workload does not exist.
	ErrWorkloadNotFound = errors.New("workload not found")
	// ErrUnsupportedRuntime is returned when a specification names a runtime the
	// server cannot run yet.
	ErrUnsupportedRuntime = errors.New("unsupported runtime")
	// ErrNoRuntime is returned when a specification names no runtime at all.
	ErrNoRuntime = errors.New("no runtime specified")
	// ErrAmbiguousRuntime is returned when a specification names more than one
	// runtime, leaving no single driver to run it.
	ErrAmbiguousRuntime = errors.New("more than one runtime specified")
)

type (
	// The Driver interface describes the runtime operations the service uses to
	// report on and read from workloads.
	//
	// Starting and stopping work is the reconciler's job, not the service's: the
	// service records what is wanted and the reconciler makes it so. The one
	// exception is Stop on delete, where the running work has to be gone before
	// the desired state that describes it is removed.
	Driver interface {
		// Stop should stop and discard everything the driver runs for the named workload.
		Stop(ctx context.Context, workload string) error
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// Logs should return the recent output of the named workload, limited to
		// the last tail lines.
		Logs(ctx context.Context, workload string, tail int) (string, error)
	}

	// The WorkloadRepository interface describes the persistence operations the
	// service uses.
	WorkloadRepository interface {
		// Upsert should store w as the desired state for its name, reporting whether
		// the workload was newly created.
		Upsert(ctx context.Context, w database.Workload) (database.Workload, bool, error)
		// Get should return the workload with the given name.
		Get(ctx context.Context, name string) (database.Workload, error)
		// List should return every stored workload.
		List(ctx context.Context) ([]database.Workload, error)
		// Delete should remove the workload with the given name.
		Delete(ctx context.Context, name string) error
	}

	// The WorkloadService type orchestrates the persistence layer and the driver
	// that runs workloads.
	WorkloadService struct {
		logger    *slog.Logger
		driver    Driver
		workloads WorkloadRepository
		notify    func()
	}
)

// NewWorkloadService returns a WorkloadService that stores workloads in the given
// repository and runs them through the given driver.
//
// The notify function is called whenever desired state changes, so that the
// reconciler can converge immediately rather than waiting for its next tick. It
// may be nil when no reconciler is running, as in tests.
func NewWorkloadService(logger *slog.Logger, d Driver, workloads WorkloadRepository, notify func()) *WorkloadService {
	return &WorkloadService{
		logger:    logger.With("component", "service"),
		driver:    d,
		workloads: workloads,
		notify:    notify,
	}
}

// Apply stores spec as the desired state for its name, returning the resulting
// workload and whether it was newly created.
//
// Applying an unchanged specification is a no-op that leaves the version alone;
// a changed one increments it, which is what later causes the reconciler to
// replace any running instance. Returns ErrNoRuntime when the specification names
// no runtime, or ErrUnsupportedRuntime when it names one the server cannot run.
func (s *WorkloadService) Apply(ctx context.Context, spec api.WorkloadSpec) (Workload, bool, error) {
	runtime, err := runtimeOf(spec)
	if err != nil {
		return Workload{}, false, err
	}

	encoded, hash, err := canonicalise(spec)
	if err != nil {
		return Workload{}, false, err
	}

	row := database.Workload{
		Name:     spec.Name,
		Runtime:  string(runtime),
		Spec:     encoded,
		SpecHash: hash,
	}

	if spec.Schedule != nil {
		row.Schedule = *spec.Schedule
	}
	if spec.Labels != nil {
		row.Labels = *spec.Labels
	}

	stored, created, err := s.workloads.Upsert(ctx, row)
	if err != nil {
		return Workload{}, false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.logger.With("workload", stored.Name, "version", stored.Version, "created", created).Debug("workload applied")
	s.wake()

	workload, err := s.hydrate(ctx, stored)
	if err != nil {
		return Workload{}, false, err
	}

	return workload, created, nil
}

// Get returns the workload with the given name, with the state observed from the
// driver merged in. Returns ErrWorkloadNotFound when no such workload exists.
func (s *WorkloadService) Get(ctx context.Context, name string) (Workload, error) {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	}

	return s.hydrate(ctx, row)
}

// List returns every workload, with the state observed from the driver merged in.
func (s *WorkloadService) List(ctx context.Context) ([]Workload, error) {
	rows, err := s.workloads.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list workloads: %w", err)
	}

	// One observation covers every workload, so the driver is asked once rather
	// than once per row.
	observed := s.observe(ctx)

	workloads := make([]Workload, 0, len(rows))
	for _, row := range rows {
		workload, err := newWorkload(row, observed[row.Name])
		if err != nil {
			return nil, err
		}

		workloads = append(workloads, workload)
	}

	return workloads, nil
}

// Delete removes the workload with the given name and stops everything the driver
// runs for it. Returns ErrWorkloadNotFound when no such workload exists.
//
// The driver is stopped before the desired state is removed so that a failure to
// stop leaves the workload in place, rather than orphaning running work that
// nothing records any more.
func (s *WorkloadService) Delete(ctx context.Context, name string) error {
	if _, err := s.workloads.Get(ctx, name); err != nil {
		if errors.Is(err, database.ErrWorkloadNotFound) {
			return ErrWorkloadNotFound
		}

		return fmt.Errorf("failed to load workload: %w", err)
	}

	if err := s.driver.Stop(ctx, name); err != nil {
		return fmt.Errorf("failed to stop workload: %w", err)
	}

	err := s.workloads.Delete(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return ErrWorkloadNotFound
	case err != nil:
		return fmt.Errorf("failed to delete workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload deleted")
	s.wake()

	return nil
}

// Logs returns the recent output of the named workload, limited to the last tail
// lines. Returns ErrWorkloadNotFound when no such workload exists.
func (s *WorkloadService) Logs(ctx context.Context, name string, tail int) (string, error) {
	if _, err := s.workloads.Get(ctx, name); err != nil {
		if errors.Is(err, database.ErrWorkloadNotFound) {
			return "", ErrWorkloadNotFound
		}

		return "", fmt.Errorf("failed to load workload: %w", err)
	}

	logs, err := s.driver.Logs(ctx, name, tail)
	if err != nil {
		return "", fmt.Errorf("failed to read workload logs: %w", err)
	}

	return logs, nil
}

func (s *WorkloadService) hydrate(ctx context.Context, row database.Workload) (Workload, error) {
	return newWorkload(row, s.observe(ctx)[row.Name])
}

// observe groups the driver's instances by workload name.
//
// A driver that cannot be reached is deliberately not an error: the desired state
// is still worth reporting, and a read of it shouldn't fail because the runtime is
// briefly unavailable. The failure is logged and callers see workloads with no
// instances, which reads as pending.
func (s *WorkloadService) observe(ctx context.Context) map[string][]driver.Instance {
	instances, err := s.driver.Observe(ctx)
	if err != nil {
		s.logger.With("error", err).Error("failed to observe driver instances")
		return nil
	}

	byWorkload := make(map[string][]driver.Instance, len(instances))
	for _, instance := range instances {
		byWorkload[instance.Workload] = append(byWorkload[instance.Workload], instance)
	}

	return byWorkload
}

func (s *WorkloadService) wake() {
	if s.notify != nil {
		s.notify()
	}
}

// runtimeOf reports which runtime a specification names, which is determined by
// which block it carries rather than by a discriminator field. Exactly one block
// must be present.
func runtimeOf(spec api.WorkloadSpec) (api.Runtime, error) {
	switch {
	case spec.Container != nil && spec.Script != nil:
		return "", ErrAmbiguousRuntime
	case spec.Container != nil:
		return api.Container, nil
	case spec.Script != nil:
		return api.Script, fmt.Errorf("%w: %q", ErrUnsupportedRuntime, api.Script)
	default:
		return "", ErrNoRuntime
	}
}

// canonicalise encodes spec as JSON and hashes it. Go's encoder writes struct
// fields in declaration order and map keys in sorted order, so the encoding is
// stable for a given specification and the hash can be compared to detect drift.
func canonicalise(spec api.WorkloadSpec) ([]byte, string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode workload spec: %w", err)
	}

	sum := sha256.Sum256(encoded)

	return encoded, hex.EncodeToString(sum[:]), nil
}

func newWorkload(row database.Workload, instances []driver.Instance) (Workload, error) {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return Workload{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	return Workload{
		Name:      row.Name,
		Version:   row.Version,
		Runtime:   api.Runtime(row.Runtime),
		Schedule:  row.Schedule,
		Spec:      spec,
		Labels:    row.Labels,
		Instances: instances,
		State:     stateOf(instances),
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// stateOf derives a workload's overall state from its instances. Running wins
// over everything — a workload with one healthy instance is running — then
// failure, then a clean exit; a workload with no instances at all is pending,
// because the reconciler has yet to start it.
func stateOf(instances []driver.Instance) api.WorkloadState {
	if len(instances) == 0 {
		return api.WorkloadStatePending
	}

	var failed, exited bool
	for _, instance := range instances {
		switch instance.State {
		case driver.StateRunning:
			return api.WorkloadStateRunning
		case driver.StateFailed:
			failed = true
		case driver.StateExited:
			exited = true
		}
	}

	switch {
	case failed:
		return api.WorkloadStateFailed
	case exited:
		return api.WorkloadStateStopped
	default:
		return api.WorkloadStatePending
	}
}

// The Workload type is the service's view of a workload: the desired state that
// was submitted, together with what the driver reports is running for it.
type Workload struct {
	// The name that identifies the workload.
	Name string
	// Incremented every time the workload's specification changes.
	Version int
	// Which runtime the specification names.
	Runtime api.Runtime
	// The cron expression describing when the workload should run, if any.
	Schedule string
	// The specification that was submitted.
	Spec api.WorkloadSpec
	// Arbitrary key-value pairs attached to the workload.
	Labels map[string]string
	// The instances the driver is currently running for the workload.
	Instances []driver.Instance
	// The workload's overall state, derived from its instances.
	State api.WorkloadState
	// The time the workload was first applied.
	CreatedAt time.Time
	// The time the workload's specification last changed.
	UpdatedAt time.Time
}
