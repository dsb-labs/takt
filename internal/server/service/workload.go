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
	"slices"
	"strings"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/port"
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
	// ErrWorkloadDeleting is returned when applying a workload that is currently
	// being torn down.
	ErrWorkloadDeleting = errors.New("workload is being deleted")
	// ErrHostPortTaken is returned when a specification pins a host port that
	// another workload already holds.
	ErrHostPortTaken = errors.New("host port already in use")
	// ErrInvalidQuery is returned when a list query is malformed.
	ErrInvalidQuery = errors.New("invalid query")
	// ErrNoPortsAvailable is returned when no host port is free for a workload that
	// needs one allocated.
	ErrNoPortsAvailable = errors.New("no host port available")
)

type (
	// The Driver interface describes the runtime operations the service uses to
	// report on and read from workloads.
	//
	// The service never starts or stops work. It records what is wanted — including
	// that a workload should go away — and the reconciler makes it so, which keeps
	// a single component responsible for touching the runtime.
	Driver interface {
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// Logs should return the recent output of the named workload, limited to
		// the last tail lines.
		Logs(ctx context.Context, workload string, tail int) (string, error)
	}

	// The WorkloadRepository interface describes the persistence operations the
	// service uses.
	WorkloadRepository interface {
		// Upsert should store w as the desired state for its name, claiming the given
		// ports in the same write, and report whether the workload was newly created.
		Upsert(ctx context.Context, w database.Workload, ports ...database.Port) (database.Workload, bool, error)
		// Get should return the workload with the given name.
		Get(ctx context.Context, name string) (database.Workload, error)
		// List should return the workloads matching every one of the given queries,
		// or all of them when none are given.
		List(ctx context.Context, queries ...database.Query) ([]database.Workload, error)
		// MarkDeleting should record that the workload with the given name is to be
		// deleted, returning it as it now stands.
		MarkDeleting(ctx context.Context, name string) (database.Workload, error)
	}

	// The PortRepository interface describes the port allocation operations the
	// service uses.
	PortRepository interface {
		// List should return the ports allocated to the workload with the given
		// identifier.
		List(ctx context.Context, workloadID string) ([]database.Port, error)
		// Claim should record the given ports as allocated to the workload with the
		// given identifier, replacing whatever it held before.
		Claim(ctx context.Context, workloadID string, ports []database.Port) error
		// HolderOf should name the workload the given host port is allocated to,
		// reporting false when no workload holds it.
		HolderOf(ctx context.Context, host int) (string, bool, error)
		// ListAll should return the ports allocated to every workload, keyed by
		// workload identifier.
		ListAll(ctx context.Context) (map[string][]database.Port, error)
		// Allocated should return every host port allocated to any workload.
		Allocated(ctx context.Context) ([]int, error)
	}

	// The Allocator interface describes how the service obtains a host port for a
	// workload that didn't ask for a particular one.
	Allocator interface {
		// Allocate should return a free host port, avoiding those in taken.
		Allocate(taken []int) (int, error)
	}

	// The WorkloadService type orchestrates the persistence layer and the driver
	// that runs workloads.
	WorkloadService struct {
		logger    *slog.Logger
		driver    Driver
		workloads WorkloadRepository
		ports     PortRepository
		allocator Allocator
		notify    func()
	}
)

// The WorkloadServiceConfig type contains fields used to construct a WorkloadService.
type WorkloadServiceConfig struct {
	// The logger used for service events.
	Logger *slog.Logger
	// The driver used to observe and read from workloads.
	Driver Driver
	// The repository holding desired state.
	Workloads WorkloadRepository
	// The repository holding port allocations.
	Ports PortRepository
	// The allocator used to choose host ports.
	Allocator Allocator
	// Called whenever desired state changes, so that the reconciler can converge
	// immediately rather than waiting for its next tick. May be nil when no
	// reconciler is running, as in tests.
	Notify func()
}

// NewWorkloadService returns a WorkloadService built from the given configuration.
func NewWorkloadService(config WorkloadServiceConfig) *WorkloadService {
	return &WorkloadService{
		logger:    config.Logger.With("component", "service"),
		driver:    config.Driver,
		workloads: config.Workloads,
		ports:     config.Ports,
		allocator: config.Allocator,
		notify:    config.Notify,
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

	// A workload mid-teardown cannot be resurrected by re-applying it: the
	// reconciler is still removing its work, so accepting the change would race
	// that teardown and could leave the new instance being torn down instead.
	existing, err := s.workloads.Get(ctx, spec.Name)
	switch {
	case err != nil && !errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, false, fmt.Errorf("failed to load workload: %w", err)
	case err == nil && !existing.DeletedAt.IsZero():
		return Workload{}, false, ErrWorkloadDeleting
	}

	// Ports are resolved before the specification is hashed, so the host ports orca
	// settled on are part of what the reconciler compares against. A reallocation
	// then reads as an ordinary specification change and replaces the container
	// bound to the old port.
	var held []database.Port
	if err == nil {
		if held, err = s.ports.List(ctx, existing.ID); err != nil {
			return Workload{}, false, fmt.Errorf("failed to read workload ports: %w", err)
		}
	}

	stored, created, err := s.store(ctx, spec, runtime, held)
	if err != nil {
		return Workload{}, false, err
	}

	s.logger.With("workload", stored.Name, "version", stored.Version, "created", created).Debug("workload applied")
	s.wake()

	workload, err := s.hydrate(ctx, stored)
	if err != nil {
		return Workload{}, false, err
	}

	return workload, created, nil
}

// store resolves the specification's ports, writes it, and claims the ports it
// settled on.
//
// Allocation reads the ports already promised and then claims one, so two applies
// racing each other can choose the same free port; the unique constraint on the
// claim means one of them loses. That collision is orca's to resolve rather than the
// caller's, so a dynamic port is simply resolved again against what is now allocated.
// A pinned port that collides is a different matter entirely: the caller asked for
// something specific and has to be told it isn't available.
func (s *WorkloadService) store(ctx context.Context, spec api.WorkloadSpec, runtime api.Runtime, held []database.Port) (database.Workload, bool, error) {
	// Bounded because a caller waiting on a request would rather hear that orca
	// couldn't settle its ports than wait indefinitely for a quiet moment.
	const attempts = 5

	for attempt := range attempts {
		ports, err := s.resolvePorts(ctx, spec.Name, held, portMappings(spec))
		if err != nil {
			return database.Workload{}, false, err
		}

		encoded, hash, err := canonicalise(withResolvedPorts(spec, ports))
		if err != nil {
			return database.Workload{}, false, err
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

		// The row and its ports are written together, so a claim that loses a race
		// leaves no workload behind for the reconciler to start against ports
		// nothing holds.
		stored, created, err := s.workloads.Upsert(ctx, row, ports...)
		switch {
		case err == nil:
			return stored, created, nil
		case !errors.Is(err, database.ErrHostPortTaken):
			return database.Workload{}, false, fmt.Errorf("failed to store workload: %w", err)
		case pinned(portMappings(spec)):
			return database.Workload{}, false, fmt.Errorf("%w: %v", ErrHostPortTaken, err)
		}

		s.logger.With("workload", spec.Name, "attempt", attempt+1).
			Debug("host port was claimed by another workload, allocating again")
	}

	return database.Workload{}, false, fmt.Errorf("%w: gave up after %d attempts", ErrHostPortTaken, attempts)
}

// pinned reports whether any mapping names a host port explicitly, which decides
// whether a claim collision is the caller's problem or orca's to retry.
func pinned(mappings []api.PortMapping) bool {
	return slices.ContainsFunc(mappings, func(mapping api.PortMapping) bool {
		return mapping.From != nil
	})
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

// List returns the workloads matching every one of the given queries, with the state
// observed from the driver merged in. Passing no queries returns every workload.
//
// Each query is a "path=value" string, where the path is a JSON path into the stored
// specification. Returns ErrInvalidQuery when one is malformed.
func (s *WorkloadService) List(ctx context.Context, queries ...string) ([]Workload, error) {
	parsed, err := parseQueries(queries)
	if err != nil {
		return nil, err
	}

	rows, err := s.workloads.List(ctx, parsed...)
	if err != nil {
		if errors.Is(err, database.ErrInvalidQueryPath) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
		}

		return nil, fmt.Errorf("failed to list workloads: %w", err)
	}

	// One observation covers every workload, and so does one read of the port
	// allocations: asking per row would turn a list into a query per workload.
	observed := s.observe(ctx)

	ports, err := s.ports.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read workload ports: %w", err)
	}

	workloads := make([]Workload, 0, len(rows))
	for _, row := range rows {
		workload, err := newWorkload(row, observed[row.Name], ports[row.ID])
		if err != nil {
			return nil, err
		}

		workloads = append(workloads, workload)
	}

	return workloads, nil
}

// Delete marks the workload with the given name for deletion and returns it as it
// now stands. Returns ErrWorkloadNotFound when no such workload exists.
//
// Deletion is asynchronous: the workload is marked and the reconciler tears its
// work down, removing the desired state only once the driver reports nothing is
// left. Stopping the work here instead would race the reconciler for the same
// containers, and removing the row eagerly would destroy the desired state the
// teardown is driven from — leaving running work that nothing records. Keeping the
// row also makes the teardown observable, so a caller can watch the workload reach
// terminating and then disappear.
func (s *WorkloadService) Delete(ctx context.Context, name string) (Workload, error) {
	marked, err := s.workloads.MarkDeleting(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to mark workload for deletion: %w", err)
	}

	s.logger.With("workload", name).Debug("workload marked for deletion")
	s.wake()

	return s.hydrate(ctx, marked)
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

// parseQueries turns "path=value" strings into repository queries.
//
// The value may itself contain an equals sign — a label value could — so only the
// first one separates the two halves.
func parseQueries(queries []string) ([]database.Query, error) {
	if len(queries) == 0 {
		return nil, nil
	}

	parsed := make([]database.Query, 0, len(queries))
	for _, query := range queries {
		path, value, ok := strings.Cut(query, "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: %q must be in path=value form", ErrInvalidQuery, query)
		case path == "":
			return nil, fmt.Errorf("%w: %q has no path", ErrInvalidQuery, query)
		}

		parsed = append(parsed, database.Query{Path: path, Value: value})
	}

	return parsed, nil
}

// Reallocate gives the named workload fresh host ports for any it holds
// dynamically, reporting whether anything changed.
//
// This exists for the reconciler to call when a workload fails to start, which may
// be because a host port orca chose has been taken by something outside orca. Only
// dynamic ports move: a pinned port was asked for explicitly, so replacing it would
// be overriding the operator rather than revising a guess, and a workload with no
// dynamic ports is left entirely alone.
//
// The workload's stored specification is rewritten with the new ports, which bumps
// its version. That is what makes the reconciler replace the container bound to the
// old port rather than leaving it running on an address nothing records.
func (s *WorkloadService) Reallocate(ctx context.Context, name string) (bool, error) {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return false, ErrWorkloadNotFound
	case err != nil:
		return false, fmt.Errorf("failed to load workload: %w", err)
	}

	held, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("failed to read workload ports: %w", err)
	}

	var spec api.WorkloadSpec
	if err = json.Unmarshal(row.Spec, &spec); err != nil {
		return false, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	// Dropping the dynamic allocations is what makes resolution pick new ports for
	// them, since resolution only reuses what it finds still held.
	var pinned []database.Port
	var dynamic bool

	for _, port := range held {
		if port.Dynamic {
			dynamic = true
			continue
		}

		pinned = append(pinned, port)
	}

	if !dynamic {
		return false, nil
	}

	// The stored specification already has its host ports filled in, so the dynamic
	// ones are cleared to ask for a fresh allocation rather than the same port back.
	ports, err := s.resolvePorts(ctx, name, pinned, requestedMappings(spec, held))
	if err != nil {
		return false, err
	}

	for i := range ports {
		ports[i].WorkloadID = row.ID
	}

	if err = s.ports.Claim(ctx, row.ID, ports); err != nil {
		return false, fmt.Errorf("failed to claim workload ports: %w", err)
	}

	encoded, hash, err := canonicalise(withResolvedPorts(spec, ports))
	if err != nil {
		return false, err
	}

	row.Spec, row.SpecHash = encoded, hash

	if _, _, err = s.workloads.Upsert(ctx, row); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.wake()

	return true, nil
}

// requestedMappings recovers what a specification originally asked for from the
// stored one, whose host ports have already been resolved. A mapping whose host port
// was allocated is returned without it, so resolution allocates afresh; a pinned one
// keeps it.
func requestedMappings(spec api.WorkloadSpec, held []database.Port) []api.PortMapping {
	allocated := make(map[int]struct{}, len(held))
	for _, port := range held {
		if port.Dynamic {
			allocated[port.Container] = struct{}{}
		}
	}

	mappings := portMappings(spec)
	requested := make([]api.PortMapping, 0, len(mappings))

	for _, mapping := range mappings {
		if _, ok := allocated[mapping.To]; ok {
			requested = append(requested, api.PortMapping{To: mapping.To})
			continue
		}

		requested = append(requested, mapping)
	}

	return requested
}

func (s *WorkloadService) resolvePorts(ctx context.Context, name string, existing []database.Port, mappings []api.PortMapping) ([]database.Port, error) {
	held := make(map[int]database.Port, len(existing))
	for _, port := range existing {
		held[port.Container] = port
	}

	// Ports already promised to any workload are off limits, along with the ones
	// resolved so far in this specification.
	allocated, err := s.ports.Allocated(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read allocated ports: %w", err)
	}

	taken := make([]int, 0, len(allocated)+len(mappings))
	taken = append(taken, allocated...)

	resolved := make([]database.Port, 0, len(mappings))
	for _, mapping := range mappings {
		port, err := s.resolvePort(ctx, name, held, taken, mapping)
		if err != nil {
			return nil, err
		}

		resolved = append(resolved, port)
		taken = append(taken, port.Host)
	}

	return resolved, nil
}

func (s *WorkloadService) resolvePort(ctx context.Context, name string, held map[int]database.Port, taken []int, mapping api.PortMapping) (database.Port, error) {
	// A pinned host port is a decision orca must not quietly override, so it is
	// used as given once nothing else holds it.
	if mapping.From != nil {
		holder, isHeld, err := s.ports.HolderOf(ctx, *mapping.From)
		switch {
		case err != nil:
			return database.Port{}, fmt.Errorf("failed to look up host port: %w", err)
		case isHeld && holder != name:
			return database.Port{}, fmt.Errorf("%w: %d is used by workload %q", ErrHostPortTaken, *mapping.From, holder)
		}

		return database.Port{Container: mapping.To, Host: *mapping.From}, nil
	}

	// An existing allocation is kept so that the workload's address doesn't move
	// every time something unrelated about it changes.
	if previous, ok := held[mapping.To]; ok && previous.Dynamic {
		return previous, nil
	}

	host, err := s.allocator.Allocate(taken)
	if err != nil {
		if errors.Is(err, port.ErrRangeExhausted) {
			// Every port orca may allocate is in use. The request was valid and will
			// become servable when a workload is deleted or the range widened, so it
			// is reported as a capacity problem rather than a fault or a bad request.
			return database.Port{}, fmt.Errorf("%w for %d: %v", ErrNoPortsAvailable, mapping.To, err)
		}

		return database.Port{}, fmt.Errorf("failed to allocate host port for %d: %w", mapping.To, err)
	}

	return database.Port{Container: mapping.To, Host: host, Dynamic: true}, nil
}

// withResolvedPorts returns spec with every port's host side filled in.
//
// The resolved ports are part of the specification that gets hashed, which is what
// makes a reallocated port replace the container running on the old one: to the
// reconciler it is simply a specification that has changed.
func withResolvedPorts(spec api.WorkloadSpec, ports []database.Port) api.WorkloadSpec {
	if spec.Container == nil || len(ports) == 0 {
		return spec
	}

	byContainer := make(map[int]database.Port, len(ports))
	for _, port := range ports {
		byContainer[port.Container] = port
	}

	mappings := make([]api.PortMapping, 0, len(ports))
	for _, mapping := range *spec.Container.Ports {
		resolved, ok := byContainer[mapping.To]
		if !ok {
			continue
		}

		mappings = append(mappings, api.PortMapping{To: mapping.To, From: new(resolved.Host)})
	}

	// The container block is a pointer into the caller's specification, so it is
	// copied rather than written through.
	container := *spec.Container
	container.Ports = &mappings
	spec.Container = &container

	return spec
}

// newResolvedPorts maps stored allocations onto the wire format.
func newResolvedPorts(ports []database.Port) []api.ResolvedPort {
	if len(ports) == 0 {
		return nil
	}

	resolved := make([]api.ResolvedPort, 0, len(ports))
	for _, port := range ports {
		resolved = append(resolved, api.ResolvedPort{
			To:      port.Container,
			From:    port.Host,
			Dynamic: port.Dynamic,
		})
	}

	return resolved
}

// portMappings returns the port mappings a specification publishes.
func portMappings(spec api.WorkloadSpec) []api.PortMapping {
	if spec.Container == nil || spec.Container.Ports == nil {
		return nil
	}

	return *spec.Container.Ports
}

func (s *WorkloadService) hydrate(ctx context.Context, row database.Workload) (Workload, error) {
	ports, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read workload ports: %w", err)
	}

	return newWorkload(row, s.observe(ctx)[row.Name], ports)
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

func newWorkload(row database.Workload, instances []driver.Instance, ports []database.Port) (Workload, error) {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return Workload{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	deleting := !row.DeletedAt.IsZero()

	return Workload{
		Name:      row.Name,
		Version:   row.Version,
		Runtime:   api.Runtime(row.Runtime),
		Schedule:  row.Schedule,
		Spec:      spec,
		Labels:    row.Labels,
		Instances: instances,
		Ports:     newResolvedPorts(ports),
		State:     stateOf(instances, deleting),
		Deleting:  deleting,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// stateOf derives a workload's overall state from its instances and whether it is
// being deleted.
//
// A workload marked for deletion is terminating whatever its instances are doing,
// because that is the only thing that will happen to it from here — reporting it as
// running while it is on its way out would invite a caller to wait for something
// that is never coming back.
//
// Otherwise running wins: a workload whose replacement is already up while its
// predecessor is still going away is running, not terminating. Then teardown in
// progress is reported ahead of how the departing instance ended, since the exit is
// a consequence of the teardown rather than news in its own right. Failure outranks
// a clean exit, and a workload with no instances at all is pending, because the
// reconciler has yet to start it.
func stateOf(instances []driver.Instance, deleting bool) api.WorkloadState {
	if deleting {
		return api.WorkloadStateTerminating
	}

	if len(instances) == 0 {
		return api.WorkloadStatePending
	}

	var terminating, failed, exited bool
	for _, instance := range instances {
		switch instance.State {
		case driver.StateRunning:
			return api.WorkloadStateRunning
		case driver.StateTerminating:
			terminating = true
		case driver.StateFailed:
			failed = true
		case driver.StateExited:
			exited = true
		}
	}

	switch {
	case terminating:
		return api.WorkloadStateTerminating
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
	// The port mappings the server settled on, including any it allocated.
	Ports []api.ResolvedPort
	// The workload's overall state, derived from its instances and whether it is
	// being deleted.
	State api.WorkloadState
	// Whether the workload has been marked for deletion and is being torn down.
	Deleting bool
	// The time the workload was first applied.
	CreatedAt time.Time
	// The time the workload's specification last changed.
	UpdatedAt time.Time
}
