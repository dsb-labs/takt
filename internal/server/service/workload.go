// Package service provides the domain orchestration layer for the takt server.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/port"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrWorkloadNotFound is returned when the requested workload does not exist.
	ErrWorkloadNotFound = errors.New("workload not found")
	// ErrUnsupportedRuntime is returned when a specification names a runtime the
	// server cannot run yet, or asks one for resource limits its host cannot
	// enforce.
	ErrUnsupportedRuntime = errors.New("unsupported runtime")
	// ErrWorkloadDeleting is returned when applying a workload that is currently
	// being torn down.
	ErrWorkloadDeleting = errors.New("workload is being deleted")
	// ErrWorkloadChanged is returned when the workload an apply was conditioned on
	// is no longer the workload the server holds.
	ErrWorkloadChanged = errors.New("workload changed since it was read")
	// ErrWorkloadSuspended is returned when restarting a workload that is
	// suspended, since nothing would start until it is started again.
	ErrWorkloadSuspended = errors.New("workload is suspended")
	// ErrPortNotPublished is returned when a specification references a port the
	// referenced workload does not publish.
	ErrPortNotPublished = errors.New("port not published")
	// ErrInvalidQuery is returned when a list query is malformed.
	ErrInvalidQuery = errors.New("invalid query")
	// ErrInvalidSpec is returned when a specification does not describe a runnable
	// workload.
	ErrInvalidSpec = errors.New("invalid specification")
	// ErrWorkloadInUse is returned when a workload another one references is deleted
	// without being forced.
	ErrWorkloadInUse = errors.New("workload is in use")
	// ErrNoSuchInstance is returned when a request selects an instance index the
	// workload's count does not include.
	ErrNoSuchInstance = errors.New("no such instance")
	// ErrInstanceRequired is returned when a request must select one instance of a
	// workload running several and selects none.
	ErrInstanceRequired = errors.New("an instance must be selected")
)

type (
	// The Driver interface describes the runtime operations the service uses to
	// report on and read from workloads.
	//
	// The service never starts or stops work. It records what is wanted — including
	// that a workload should go away — and the reconciler makes it so, which keeps
	// a single component responsible for touching the runtime.
	Driver interface {
		// Name should return the name the driver is known by.
		Name() string
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// ObserveWorkload should report every instance the driver is currently
		// running for one workload.
		ObserveWorkload(ctx context.Context, id, name string) ([]driver.Instance, error)
		// Usage should report what each instance of one workload is consuming,
		// keyed by the identifier the instance is reported under. A driver reports
		// counters rather than rates, and leaves out an instance it has no reading
		// for.
		Usage(ctx context.Context, id, name string) (map[string]driver.Usage, error)
		// Logs should write the recent output of the named workload to out, as the
		// options describe. A driver with nothing for the name should write nothing
		// rather than fail, since the caller does not know which runtime holds the
		// workload.
		Logs(ctx context.Context, out io.Writer, workload string, options driver.LogOptions) error
		// Enforceable should report whether the host lets the driver enforce
		// resource limits, returning an error naming what is missing when it
		// does not. A driver whose limits need nothing of the host answers nil
		// unconditionally, as the container runtime does.
		Enforceable() error
	}

	// The WorkloadRepository interface describes the persistence operations the
	// service uses.
	WorkloadRepository interface {
		// Upsert should store w as the desired state for its name, claiming the given
		// ports in the same write, and report whether the workload was newly created.
		// A non-zero ifMatch conditions the write on the stored version still
		// being that one.
		Upsert(ctx context.Context, w database.Workload, ifMatch int, ports ...database.Port) (database.Workload, bool, error)
		// Get should return the workload with the given name.
		Get(ctx context.Context, name string) (database.Workload, error)
		// List should return the workloads matching every one of the given queries,
		// or all of them when none are given.
		List(ctx context.Context, queries ...database.Query) ([]database.Workload, error)
		// MarkDeleting should record that the workload with the given name is to be
		// deleted, returning it as it now stands beside the names of the workloads
		// referencing it. Unless force is set, a referenced workload is left as it
		// is and database.ErrWorkloadReferenced returned with the names.
		MarkDeleting(ctx context.Context, name string, force bool) (database.Workload, []string, error)
		// Suspend should record that the workload with the given name is not to
		// run, returning it as it now stands.
		Suspend(ctx context.Context, name string) (database.Workload, error)
		// Resume should clear the workload's suspension, returning it as it now
		// stands.
		Resume(ctx context.Context, name string) (database.Workload, error)
		// ReferencedBy should name the workloads whose specifications reference the
		// workload with the given name.
		ReferencedBy(ctx context.Context, name string) ([]string, error)
	}

	// The Reconciler interface describes what the service asks of the loop that
	// owns the runtime.
	//
	// The service records what is wanted and never touches running work, so
	// everything here crosses that one boundary: waking the loop when desired
	// state changes, recording a restart request for it to act on, and reading
	// why its last pass over a workload failed — which is only observable during
	// the pass that hits it, so the reconciler is the one component that has it.
	Reconciler interface {
		// Notify should ask for a reconciliation pass to run as soon as possible,
		// and never block.
		Notify()
		// Restart should record that the workload's instances are to be replaced
		// on the next pass over it.
		Restart(workload string)
	}

	// The WorkloadEventRepository interface describes the event operations the
	// service uses.
	WorkloadEventRepository interface {
		// Record should record an event against the named workload, coalescing it
		// with an event already recorded carrying the same reason and data.
		Record(ctx context.Context, name string, reason event.Reason, data []byte) error
		// List should return the events recorded against the named workload, most
		// recently seen first, up to limit of them. A non-zero since should drop
		// the events last seen at or before it.
		List(ctx context.Context, name string, since time.Time, limit int) ([]database.WorkloadEvent, error)
	}

	// The PortRepository interface describes the port allocation operations the
	// service uses.
	PortRepository interface {
		// List should return the ports allocated to the workload with the given
		// identifier.
		List(ctx context.Context, workloadID string) ([]database.Port, error)
		// HolderOf should name the workload the given host port is allocated to,
		// reporting false when no workload holds it.
		HolderOf(ctx context.Context, host int, protocol string) (string, bool, error)
		// ListAll should return the ports allocated to every workload, keyed by
		// workload identifier.
		ListAll(ctx context.Context) (map[string][]database.Port, error)
		// Allocated should return every host port allocated to any workload, keyed
		// by the protocol it is allocated on.
		Allocated(ctx context.Context) (map[string][]int, error)
		// Claim should record the given ports as allocated to the workload with
		// the given identifier, replacing whatever was allocated to it before.
		Claim(ctx context.Context, workloadID string, ports []database.Port) error
	}

	// The VolumeLocator interface describes how the service finds out where a
	// mounted volume's data lives.
	//
	// Narrower than the volume service it is satisfied by: applying a workload needs
	// to resolve a name to a path and nothing else, so that is all this asks for.
	VolumeLocator interface {
		// Path should return where the named volume's data is, reporting
		// ErrVolumeNotFound when no such volume exists.
		Path(ctx context.Context, name string) (string, error)
	}

	// The Claimer interface describes how the service settles a workload's ports on
	// the host ports they are reached at.
	Claimer interface {
		// Resolve should settle every mapping on a host port for each of the
		// workload's instances, keeping the allocations every instance already
		// holds.
		Resolve(ctx context.Context, workload string, held []port.Claim, mappings []manifest.Port, count int) ([]port.Claim, error)
		// Preview should settle every mapping it can without allocating anything,
		// reporting whether any of them still needs a host port.
		Preview(ctx context.Context, workload string, held []port.Claim, mappings []manifest.Port, count int) ([]port.Claim, bool, error)
	}

	// The Checker interface describes how the service reads the health of
	// workloads.
	//
	// Registering the checks is the reconciler's job rather than this one's: a
	// check has to be kept in step with what is actually running, and the
	// reconciler is what runs continuously. Registering on read would mean a
	// restarted server checked nothing until somebody happened to look.
	Checker interface {
		// Result should return the most recent outcome for a workload, reporting
		// false when it has no check registered.
		Result(workload string, instance int) (health.Result, bool)
	}

	// The SecretRevisions interface describes how the service learns what version of
	// each secret a workload reads.
	//
	// Narrower than the secret service it is satisfied by: hashing a workload needs
	// the revisions and nothing else, and in particular has no business decrypting
	// anything.
	SecretRevisions interface {
		// Revisions should return the current revision of each named secret, keyed by
		// name, omitting any that do not exist.
		Revisions(ctx context.Context, names []string) (map[string]string, error)
	}

	// The VariableValues interface describes how the service learns what each
	// variable a workload reads currently holds.
	//
	// Narrower than the variable service it is satisfied by, as SecretRevisions is.
	VariableValues interface {
		// Values should return the current value of each named variable, keyed by
		// name, omitting any that do not exist.
		Values(ctx context.Context, names []string) (map[string]string, error)
	}

	// The WorkloadAddresses interface describes how the service learns what address
	// a reference to another workload resolves to.
	//
	// Narrower than the address service it is satisfied by: hashing a workload needs
	// the address and nothing else.
	WorkloadAddresses interface {
		// Address should return the address the reference names as read by one
		// instance of the referencing workload, reporting
		// database.ErrWorkloadNotFound when nothing holds the name and
		// resolve.ErrPortNotPublished when the workload publishes no such port.
		Address(ctx context.Context, reference manifest.Reference, reader string, readerInstance int) (string, error)
	}

	// The ImageResolver interface describes how the service learns which digest an
	// image reference currently resolves to.
	//
	// Narrower than the docker driver it is satisfied by: hashing a pull-always
	// workload needs the digest and nothing else.
	ImageResolver interface {
		// Digest should return the digest the image's registry currently reports
		// for the given reference.
		Digest(ctx context.Context, ref string) (string, error)
	}

	// The WorkloadService type orchestrates the persistence layer and the driver
	// that runs workloads.
	WorkloadService struct {
		logger     *slog.Logger
		address    string
		drivers    map[string]Driver
		workloads  WorkloadRepository
		ports      PortRepository
		volumes    VolumeLocator
		secrets    SecretRevisions
		variables  VariableValues
		addresses  WorkloadAddresses
		images     ImageResolver
		claims     Claimer
		checker    Checker
		reconciler Reconciler
		events     WorkloadEventRepository
		hostPaths  []string
		samples    *usageSamples
	}
)

// The WorkloadServiceConfig type contains fields used to construct a WorkloadService.
type WorkloadServiceConfig struct {
	// The logger used for service events.
	Logger *slog.Logger
	// The address a workload's ports are reached at, joined with each
	// instance's host port when a scrape target is built. The same address
	// workload references and service backends resolve to.
	Address string
	// The drivers used to observe and read from workloads, keyed by the name each
	// one declares.
	Drivers map[string]Driver
	// The repository holding desired state.
	Workloads WorkloadRepository
	// The repository holding port allocations.
	Ports PortRepository
	// Where mounted volumes are resolved to paths. May be nil, in which case a
	// workload mounting a volume is rejected.
	Volumes VolumeLocator
	// Where the revisions of the secrets a workload reads are read from. May be nil,
	// in which case a workload referencing a secret is rejected.
	Secrets SecretRevisions
	// Where the values of the variables a workload reads are read from. May be nil,
	// in which case a workload referencing a variable is rejected.
	Variables VariableValues
	// Where the address of a referenced workload is resolved. May be nil, in which
	// case a workload referencing another is rejected.
	Addresses WorkloadAddresses
	// Where a pull-always workload's image digest is resolved. May be nil, in which
	// case such a workload is rejected.
	Images ImageResolver
	// The claimer used to settle a workload's ports on host ports.
	Claimer Claimer
	// The checker that establishes whether workloads are working. May be nil, in
	// which case no workload is checked and none reports health.
	Checker Checker
	// The loop that owns the runtime: woken whenever desired state changes and
	// handed restart requests to act on. May be nil when no reconciler is
	// running, as in tests, in which case nothing is woken.
	Reconciler Reconciler
	// The repository holding what the server recorded about each workload. May be
	// nil, in which case every workload reports no events.
	Events WorkloadEventRepository
	// The absolute prefixes a path mount may sit beneath. Empty rejects every
	// path mount, which is the safe default: a host path reaches outside
	// takt-managed state, so which ones are reachable is the operator's call.
	AllowHostPaths []string
}

// NewWorkloadService returns a WorkloadService built from the given configuration.
func NewWorkloadService(config WorkloadServiceConfig) *WorkloadService {
	return &WorkloadService{
		address:    config.Address,
		logger:     config.Logger.With("component", "service"),
		drivers:    config.Drivers,
		workloads:  config.Workloads,
		ports:      config.Ports,
		volumes:    config.Volumes,
		secrets:    config.Secrets,
		variables:  config.Variables,
		addresses:  config.Addresses,
		images:     config.Images,
		claims:     config.Claimer,
		checker:    config.Checker,
		reconciler: config.Reconciler,
		events:     config.Events,
		hostPaths:  config.AllowHostPaths,
		samples:    newUsageSamples(),
	}
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

	workload, err := s.hydrate(ctx, row)
	if err != nil {
		return Workload{}, err
	}

	// Only here. Reading one workload is what an operator asking what it is using
	// does, where hydrate is also the path every mutation returns through, and a
	// reading per instance would be paid for by all of them.
	s.usage(ctx, row, workload.Spec, workload.Instances)

	return workload, nil
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
		workload, err := newWorkload(row, observed[row.Name], ports[row.ID], s.healths(row.Name, observed[row.Name]))
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
//
// A workload another one references is refused with ErrWorkloadInUse unless force is
// set, and the error names the workloads reading its address. An apply naming a
// workload that does not exist is refused, so removing one without this check would
// leave a specification nobody could re-apply. Forcing it through redeploys those
// workloads, which then report the reference they can no longer resolve and retry
// until something holds the name again.
func (s *WorkloadService) Delete(ctx context.Context, name string, force bool) (Workload, error) {
	// The references are read and the row marked in one transaction, so a
	// reference written between the two cannot leave a workload pointing at a
	// name that is on its way out.
	marked, referencing, err := s.workloads.MarkDeleting(ctx, name, force)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case errors.Is(err, database.ErrWorkloadReferenced):
		return Workload{}, fmt.Errorf("%w: referenced by %s", ErrWorkloadInUse, strings.Join(referencing, ", "))
	case err != nil:
		return Workload{}, fmt.Errorf("failed to mark workload for deletion: %w", err)
	}

	// Rehashed once the workload is on its way out, for the reason a deleted secret
	// rehashes what read it: what those workloads were started against no longer
	// describes what takt holds, and the hash is how that is reported.
	s.rehashAll(ctx, name, referencing, event.AddressRemoved)

	s.logger.With("workload", name).Debug("workload marked for deletion")

	// Recorded even though the row is on its way out, because the teardown takes
	// passes and an operator watching it happen is reading this. The events go with
	// the row when it finally goes.
	s.record(ctx, name, event.Deleted, event.Fields{})
	s.wake()

	return s.hydrate(ctx, marked)
}

// Stop marks the workload with the given name as suspended and returns it as it
// now stands. Returns ErrWorkloadNotFound when no such workload exists, or
// ErrWorkloadDeleting when it is being torn down, since there is nothing left to
// hold down.
//
// Stopping is asynchronous, on the same reasoning as Delete: the workload is
// marked and the reconciler stops its work, so nothing here races the loop that
// owns the runtime. Suspension is desired state — it survives a server restart
// and holds until Start clears it — and it leaves the specification and version
// untouched, so starting the workload again resumes it rather than replacing it.
func (s *WorkloadService) Stop(ctx context.Context, name string) (Workload, error) {
	existing, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	case !existing.DeletedAt.IsZero():
		return Workload{}, ErrWorkloadDeleting
	}

	marked, err := s.workloads.Suspend(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to suspend workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload suspended")

	// Only the suspension that changed something is recorded. Suspending a workload
	// already suspended asks for what is already true.
	if existing.SuspendedAt.IsZero() {
		s.record(ctx, name, event.Suspended, event.Fields{})
	}

	s.wake()

	return s.hydrate(ctx, marked)
}

// Start clears the suspension of the workload with the given name and returns it
// as it now stands. Returns ErrWorkloadNotFound when no such workload exists, or
// ErrWorkloadDeleting when it is being torn down, since it can never run again.
//
// Starting is asynchronous: the mark is cleared and the next reconcile pass
// starts the workload from whatever specification is stored, including one
// applied while it was suspended. Starting a workload that is not suspended
// changes nothing.
func (s *WorkloadService) Start(ctx context.Context, name string) (Workload, error) {
	existing, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	case !existing.DeletedAt.IsZero():
		return Workload{}, ErrWorkloadDeleting
	}

	resumed, err := s.workloads.Resume(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to resume workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload resumed")

	// As with suspending, only a resume that changed something is recorded: starting
	// a workload that was never suspended changes nothing.
	if !existing.SuspendedAt.IsZero() {
		s.record(ctx, name, event.Resumed, event.Fields{})
	}

	s.wake()

	return s.hydrate(ctx, resumed)
}

// Restart asks the reconciler to replace the workload's running instances and
// returns the workload as it now stands. Returns ErrWorkloadNotFound when no
// such workload exists, ErrWorkloadDeleting when it is being torn down, and
// ErrWorkloadSuspended when it is suspended, since nothing would start.
//
// The request is recorded in memory rather than stored: one the server loses to
// a crash can simply be made again. The specification and its version are
// untouched, so the new instances run exactly what the old ones did.
func (s *WorkloadService) Restart(ctx context.Context, name string) (Workload, error) {
	existing, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	case !existing.DeletedAt.IsZero():
		return Workload{}, ErrWorkloadDeleting
	case !existing.SuspendedAt.IsZero():
		return Workload{}, ErrWorkloadSuspended
	}

	if s.reconciler != nil {
		s.reconciler.Restart(name)
	}

	s.logger.With("workload", name).Debug("workload restart requested")
	s.wake()

	return s.hydrate(ctx, existing)
}

// Logs writes the recent output of the named workload to out, as the options describe.
// Returns ErrWorkloadNotFound when no such workload exists.
//
// The output is streamed rather than returned so that a workload with a lot to say
// doesn't have to be held in memory in its entirety before any of it is sent.
func (s *WorkloadService) Logs(ctx context.Context, out io.Writer, name string, options driver.LogOptions) error {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return ErrWorkloadNotFound
	case err != nil:
		return fmt.Errorf("failed to load workload: %w", err)
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return err
	}

	if options.Instance != nil && (*options.Instance < 0 || *options.Instance >= spec.Count) {
		return fmt.Errorf("%w: workload %s runs %d", ErrNoSuchInstance, name, spec.Count)
	}

	// A follow reads one stream until it ends. With several instances and no
	// selector there is no one stream to read, and interleaving them would return
	// output nothing could attribute.
	if options.Follow && options.Instance == nil && spec.Count > 1 {
		return fmt.Errorf("%w: workload %s runs %d instances and a follow reads one", ErrInstanceRequired, name, spec.Count)
	}

	// Every driver is asked. Which runtime holds the workload is knowable from its
	// row, but a driver with nothing for the name writes nothing, so asking is
	// cheaper than threading the runtime through and getting it wrong.
	for _, runtime := range s.drivers {
		if err := runtime.Logs(ctx, out, name, options); err != nil {
			return fmt.Errorf("failed to read workload logs: %w", err)
		}
	}

	return nil
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

// wake asks for a reconciliation pass, so that a change to desired state is acted
// on immediately rather than on the next tick.
func (s *WorkloadService) wake() {
	if s.reconciler != nil {
		s.reconciler.Notify()
	}
}
