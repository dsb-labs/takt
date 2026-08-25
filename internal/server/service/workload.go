// Package service provides the domain orchestration layer for the orca server.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// How long a read will wait on the driver before reporting desired state without
// observed state. A caller asking what exists should not be held up indefinitely by a
// runtime that has stopped answering.
const observeTimeout = 10 * time.Second

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
	// ErrWorkloadSuspended is returned when restarting a workload that is
	// suspended, since nothing would start until it is started again.
	ErrWorkloadSuspended = errors.New("workload is suspended")
	// ErrHostPortTaken is returned when a specification pins a host port that
	// another workload already holds.
	ErrHostPortTaken = errors.New("host port already in use")
	// ErrInvalidQuery is returned when a list query is malformed.
	ErrInvalidQuery = errors.New("invalid query")
	// ErrNoPortsAvailable is returned when no host port is free for a workload that
	// needs one allocated.
	ErrNoPortsAvailable = errors.New("no host port available")
	// ErrInvalidSpec is returned when a specification does not describe a runnable
	// workload.
	ErrInvalidSpec = errors.New("invalid specification")
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
		// Logs should write the recent output of the named workload to out, as the
		// options describe. A driver with nothing for the name should write nothing
		// rather than fail, since the caller does not know which runtime holds the
		// workload.
		Logs(ctx context.Context, out io.Writer, workload string, options driver.LogOptions) error
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
		// Suspend should record that the workload with the given name is not to
		// run, returning it as it now stands.
		Suspend(ctx context.Context, name string) (database.Workload, error)
		// Resume should clear the workload's suspension, returning it as it now
		// stands.
		Resume(ctx context.Context, name string) (database.Workload, error)
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
		// LastError should report why the last converge pass over a workload
		// failed and when, reporting false when the workload's last pass
		// succeeded or none has run.
		LastError(workload string) (string, time.Time, bool)
	}

	// The PortRepository interface describes the port allocation operations the
	// service uses.
	PortRepository interface {
		// List should return the ports allocated to the workload with the given
		// identifier.
		List(ctx context.Context, workloadID string) ([]database.Port, error)
		// HolderOf should name the workload the given host port is allocated to,
		// reporting false when no workload holds it.
		HolderOf(ctx context.Context, host int) (string, bool, error)
		// ListAll should return the ports allocated to every workload, keyed by
		// workload identifier.
		ListAll(ctx context.Context) (map[string][]database.Port, error)
		// Allocated should return every host port allocated to any workload.
		Allocated(ctx context.Context) ([]int, error)
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

	// The Allocator interface describes how the service obtains a host port for a
	// workload that didn't ask for a particular one.
	Allocator interface {
		// Allocate should return a host port that is free on every protocol named,
		// avoiding the ports already taken on each of them.
		Allocate(protocols []port.Protocol, taken map[port.Protocol][]int) (int, error)
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
		Result(workload string) (health.Result, bool)
	}

	// The Health type reports what orca established about a workload's health,
	// and whether it checks the workload at all.
	Health struct {
		// Whether the workload declares a check orca performs.
		Checked bool
		// The most recent outcome, meaningful only when Checked.
		Result health.Result
	}

	// The hashedSpec type is what a workload's specification hash is computed over
	// when the workload reads a secret or a variable.
	//
	// It exists so that what a workload reads reaches the hash without being stored:
	// the specification is written to the database on its own, and this wrapper is
	// built only to be hashed and discarded. Its shape is therefore part of what a
	// hash means — changing it re-hashes every workload that reads either and
	// replaces their instances.
	//
	// Both maps are omitted when empty, which is what stopped adding variables from
	// re-hashing every workload that already read a secret. Such a workload encodes
	// exactly as it did before variables existed, because the field that would have
	// been written as null is left out instead.
	hashedSpec struct {
		// The stored specification, encoded exactly as it is persisted.
		Spec json.RawMessage `json:"spec"`
		// The revision of each secret the workload reads, keyed by name. Go's encoder
		// sorts map keys, so this contributes the same bytes for a given set of
		// secrets however they were collected.
		Secrets map[string]string `json:"secrets,omitempty"`
		// The value of each variable the workload reads, keyed by name.
		//
		// The value rather than a revision, unlike a secret. A secret is held at arm's
		// length because the hash is reported and one computed over a value would
		// confirm a guess at it; a variable's value is reported by the API anyway, so
		// the indirection would protect nothing and cost a column.
		Variables map[string]string `json:"variables,omitempty"`
		// The digest the image's registry reports, set only for a workload whose pull
		// policy is always. It reaches the hash without being stored, the way a
		// secret's revision does, so a rebuilt tag replaces the instance without the
		// digest being echoed back as though the operator wrote it.
		Digest string `json:"digest,omitempty"`
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

	// The references type carries what a workload reads and what those things
	// currently hold, from the one scan of a specification to the write that records
	// it and the hash that covers it.
	//
	// The names travel alongside what was read because the two answer different
	// questions. The names are stored, so that finding the workloads to redeploy when
	// something moves does not depend on parsing every specification; what was read
	// reaches the hash and is then discarded.
	references struct {
		// The names of the secrets the specification references.
		secrets []string
		// The names of the variables the specification references.
		variables []string
		// The revision of each secret that exists, keyed by name.
		revisions map[string]string
		// The value of each variable that exists, keyed by name.
		values map[string]string
		// What the specification reads only through a mount naming a signal, which is
		// deliberately kept out of the hash.
		//
		// Such a workload asked to be signalled rather than replaced, and a hash that
		// moved with the value would have the reconciler replace it — which is the one
		// thing naming a signal asks not to happen. That the workload reads it is
		// still recorded, so deleting one still reports the workloads holding it.
		refreshed []manifest.Reference
	}

	// The WorkloadService type orchestrates the persistence layer and the driver
	// that runs workloads.
	WorkloadService struct {
		logger     *slog.Logger
		drivers    map[string]Driver
		workloads  WorkloadRepository
		ports      PortRepository
		volumes    VolumeLocator
		secrets    SecretRevisions
		variables  VariableValues
		images     ImageResolver
		allocator  Allocator
		checker    Checker
		reconciler Reconciler
	}
)

// The WorkloadServiceConfig type contains fields used to construct a WorkloadService.
type WorkloadServiceConfig struct {
	// The logger used for service events.
	Logger *slog.Logger
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
	// Where a pull-always workload's image digest is resolved. May be nil, in which
	// case such a workload is rejected.
	Images ImageResolver
	// The allocator used to choose host ports.
	Allocator Allocator
	// The checker that establishes whether workloads are working. May be nil, in
	// which case no workload is checked and none reports health.
	Checker Checker
	// The loop that owns the runtime: woken whenever desired state changes,
	// handed restart requests to act on, and read for the last converge error of
	// each workload. May be nil when no reconciler is running, as in tests, in
	// which case nothing is woken and no workload reports an error.
	Reconciler Reconciler
}

// NewWorkloadService returns a WorkloadService built from the given configuration.
func NewWorkloadService(config WorkloadServiceConfig) *WorkloadService {
	return &WorkloadService{
		logger:     config.Logger.With("component", "service"),
		drivers:    config.Drivers,
		workloads:  config.Workloads,
		ports:      config.Ports,
		volumes:    config.Volumes,
		secrets:    config.Secrets,
		variables:  config.Variables,
		images:     config.Images,
		allocator:  config.Allocator,
		checker:    config.Checker,
		reconciler: config.Reconciler,
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
	// The runtime is resolved first so that naming none or naming two is reported as
	// exactly that, rather than as a general validation failure.
	runtime, err := runtimeOf(spec)
	if err != nil {
		return Workload{}, false, err
	}

	// Everything else is validated here rather than only in the client that parsed a
	// manifest. The rules are what make a workload runnable at all — a name the
	// runtime can represent, an image to run, a schedule that parses — so a caller
	// that skips the CLI has to be held to them too. Without this the server accepted
	// an unknown schema version, a name breaking its own documented rules, and an
	// empty image that could only ever fail to start.
	if err = manifest.Validate(manifest.NewSpec(spec)); err != nil {
		return Workload{}, false, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
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

	// Volumes are resolved to the paths they live at before the specification is
	// hashed, for the same reason ports are: the runtime is then handed a path rather
	// than a name to look up, and a volume whose path changed reads as an ordinary
	// change and replaces the instances bound to the old one.
	//
	// Resolved here rather than inside store, since unlike a port allocation there is
	// no race to lose: a volume either exists or it does not.
	spec, err = s.resolveVolumes(ctx, spec)
	if err != nil {
		return Workload{}, false, err
	}

	// The secrets the workload reads are resolved to their revisions rather than their
	// values, and the variables to their values. Either reaches the hash so that
	// changing one replaces the instances reading it. A secret's value stays out of
	// both the hash and the stored specification, and is read only as the workload
	// starts.
	//
	// A reference to something that does not exist is refused here rather than at
	// start. The workload could never run, and the operator asking for it is the one
	// who can fix the name.
	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return Workload{}, false, err
	}

	if absent := missing(read.secrets, read.revisions); len(absent) > 0 {
		return Workload{}, false, fmt.Errorf("%w: %s", ErrSecretNotFound, strings.Join(absent, ", "))
	}

	if absent := missing(read.variables, read.values); len(absent) > 0 {
		return Workload{}, false, fmt.Errorf("%w: %s", ErrVariableNotFound, strings.Join(absent, ", "))
	}

	// Resolved here rather than inside store, whose port retries would repeat the
	// registry round-trip for nothing: the digest does not depend on the ports.
	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return Workload{}, false, err
	}

	stored, created, err := s.store(ctx, spec, runtime, held, read, digest)
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
func (s *WorkloadService) store(
	ctx context.Context,
	spec api.WorkloadSpec,
	runtime api.Runtime,
	held []database.Port,
	read references,
	digest string,
) (database.Workload, bool, error) {
	// Bounded because a caller waiting on a request would rather hear that orca
	// couldn't settle its ports than wait indefinitely for a quiet moment.
	const attempts = 5

	for attempt := range attempts {
		ports, err := s.resolvePorts(ctx, spec.Name, held, portMappings(spec))
		if err != nil {
			return database.Workload{}, false, err
		}

		encoded, hash, err := canonicalise(withResolvedPorts(spec, ports), read, digest)
		if err != nil {
			return database.Workload{}, false, err
		}

		row := database.Workload{
			Name:      spec.Name,
			Runtime:   string(runtime),
			Spec:      encoded,
			SpecHash:  hash,
			Secrets:   read.secrets,
			Variables: read.variables,
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
		message, at := s.lastError(row.Name)

		workload, err := newWorkload(row, observed[row.Name], ports[row.ID], s.health(row.Name), message, at)
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
	if _, err := s.workloads.Get(ctx, name); err != nil {
		if errors.Is(err, database.ErrWorkloadNotFound) {
			return ErrWorkloadNotFound
		}

		return fmt.Errorf("failed to load workload: %w", err)
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

	// Read again rather than carried over, for the same reason the ports are handed
	// back to the write: the stored links are replaced by whatever the write is given,
	// so a row that named none would have its references forgotten.
	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return false, err
	}

	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return false, err
	}

	encoded, hash, err := canonicalise(withResolvedPorts(spec, ports), read, digest)
	if err != nil {
		return false, err
	}

	row.Spec, row.SpecHash = encoded, hash
	row.Secrets, row.Variables = read.secrets, read.variables

	// The ports travel with the write, as they do for an apply. Claiming them
	// separately beforehand would not survive it: the write replaces a workload's
	// allocation with whatever it was handed, so an Upsert given none clears the
	// rows that were just claimed and leaves the specification naming host ports
	// nothing holds.
	if _, _, err = s.workloads.Upsert(ctx, row, ports...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.wake()

	return true, nil
}

// Rehash recomputes the named workload's specification hash against the secrets and
// variables it currently reads, reporting whether the hash moved.
//
// This exists for the secret and variable services to call when a value changes. One
// method serves both, because it recomputes against whatever the workload references
// rather than against what it was told changed. Nothing about the specification
// changes: the stored bytes are written back exactly as they were read, and only the
// hash moves. That is what makes a rotated secret or a changed variable read as an
// ordinary specification change, so the reconciler replaces the instances holding the
// old value and the new one is resolved as they start.
//
// Something that has been deleted moves the hash too. The workload is then asking for
// something orca no longer holds, which is reported when it next tries to start
// rather than by silently leaving the old value running.
//
// A pull-always workload's image digest is resolved again here as well, so a rehash
// can also pick up a rebuilt tag — and fails when the registry is unreachable, since
// a hash computed without the digest would claim the image is unchanged when nothing
// checked.
func (s *WorkloadService) Rehash(ctx context.Context, name string) (bool, error) {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return false, ErrWorkloadNotFound
	case err != nil:
		return false, fmt.Errorf("failed to load workload: %w", err)
	}

	var spec api.WorkloadSpec
	if err = json.Unmarshal(row.Spec, &spec); err != nil {
		return false, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return false, err
	}

	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return false, err
	}

	_, hash, err := canonicalise(spec, read, digest)
	if err != nil {
		return false, err
	}

	if hash == row.SpecHash {
		// Nothing about the workload moved, which for a workload mounting a value it
		// asked to be signalled about is exactly right: such a value stays out of the
		// hash so that the instance is not replaced. The reconciler is still woken, or
		// nothing would compare what was delivered against what orca now holds until
		// its next tick.
		if len(read.refreshed) > 0 {
			s.wake()
		}

		return false, nil
	}

	// The workload keeps the ports it holds. The specification already names them, so
	// resolving them again would be asking for the allocation orca has, and the write
	// has to carry them or it would clear them.
	held, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("failed to read workload ports: %w", err)
	}

	row.SpecHash = hash
	row.Secrets, row.Variables = read.secrets, read.variables

	if _, _, err = s.workloads.Upsert(ctx, row, held...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload rehashed")
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

	host, err := s.allocator.Allocate([]port.Protocol{port.ProtocolTCP}, map[port.Protocol][]int{
		port.ProtocolTCP: taken,
	})
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

// resolveVolumes fills in where each mounted volume lives on the host, rejecting a
// specification naming one that does not exist.
//
// A volume has to exist before it can be mounted. Creating one here would make a
// mistyped name a second empty volume, which reads as success while the data the
// workload wanted sits under the name that was meant.
// Only a mount naming a volume is resolved. A mounted secret or variable is written
// by the reconciler as the workload starts, at a path that changes with every version,
// so storing one would move the hash for a reason the operator did not ask for and put
// orca's own layout in the API.
func (s *WorkloadService) resolveVolumes(ctx context.Context, spec api.WorkloadSpec) (api.WorkloadSpec, error) {
	if spec.Volumes == nil || len(*spec.Volumes) == 0 {
		return spec, nil
	}

	mounts := make([]api.VolumeMount, 0, len(*spec.Volumes))
	for _, mount := range *spec.Volumes {
		// Which source a mount names is read through the same rules validation applied,
		// rather than by inspecting the wire fields again here. Each mount is converted
		// on its own, so nothing depends on a conversion of the whole specification
		// yielding one element per wire mount in the same order.
		kind, err := manifest.KindOf(manifest.NewVolumeMount(mount))
		if err != nil {
			return spec, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
		}

		if kind != manifest.MountVolume {
			// The source fields are carried across untouched, so what the workload
			// reads is stored as written rather than as a value.
			mounts = append(mounts, mount)

			continue
		}

		if s.volumes == nil {
			return spec, fmt.Errorf("%w: this server holds no volumes", ErrVolumeNotFound)
		}

		// Whatever went wrong is returned as it stands, including a volume that does
		// not exist: the locator already names what it could not find, so wrapping it
		// again would only repeat the name.
		path, err := s.volumes.Path(ctx, *mount.Name)
		if err != nil {
			return spec, err
		}

		mount.From = new(path)
		mounts = append(mounts, mount)
	}

	// The specification is taken by value, so this replaces only this copy's slice
	// header and leaves the caller's alone.
	spec.Volumes = &mounts

	return spec, nil
}

// resolveReferences returns what the specification's environment references, along
// with what each of those currently holds.
//
// The specification comes back untouched, unlike ports and volumes: a reference is
// stored as written, because resolving a secret would put its value in the database
// and a variable is stored the same way for consistency. What was read travels
// separately, reaching the hash without being stored.
//
// Something that does not exist is simply absent rather than an error. That is what
// makes deleting a secret or a variable move the hash of the workloads reading it,
// which is how they come to report that something they need has gone. Refusing the
// reference is the business of the caller that can act on it.
func (s *WorkloadService) resolveReferences(ctx context.Context, spec api.WorkloadSpec) (references, error) {
	resolvedSpec := manifest.NewSpec(spec)

	found, err := manifest.References(resolvedSpec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	refreshed, err := manifest.Refreshed(resolvedSpec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	resolved := references{
		secrets:   manifest.Names(found, manifest.KindSecret),
		variables: manifest.Names(found, manifest.KindVariable),
		refreshed: refreshed,
	}

	switch {
	case len(resolved.secrets) == 0:
	case s.secrets == nil:
		return references{}, fmt.Errorf("%w: this server holds no secrets", ErrSecretNotFound)
	default:
		if resolved.revisions, err = s.secrets.Revisions(ctx, resolved.secrets); err != nil {
			return references{}, fmt.Errorf("failed to read secret revisions: %w", err)
		}
	}

	switch {
	case len(resolved.variables) == 0:
	case s.variables == nil:
		return references{}, fmt.Errorf("%w: this server holds no variables", ErrVariableNotFound)
	default:
		if resolved.values, err = s.variables.Values(ctx, resolved.variables); err != nil {
			return references{}, fmt.Errorf("failed to read variable values: %w", err)
		}
	}

	return resolved, nil
}

// resolveDigest returns the digest a pull-always workload's image currently
// resolves to, and the empty string for every other workload.
//
// The digest is read whenever a hash is computed — an apply, a rehash, a port
// reallocation — rather than stored, so each of those picks up a rebuilt tag and
// none can disagree with the others about what the tag holds. The cost is that
// each is a registry round-trip, and each fails when the registry is unreachable.
// That is the honest outcome: a hash computed without the digest would claim the
// image is unchanged when nothing checked.
func (s *WorkloadService) resolveDigest(ctx context.Context, spec api.WorkloadSpec) (string, error) {
	if spec.Container == nil || spec.Container.Pull == nil || *spec.Container.Pull != api.PullPolicyAlways {
		return "", nil
	}

	if s.images == nil {
		return "", fmt.Errorf("%w: this server cannot resolve image digests", ErrInvalidSpec)
	}

	digest, err := s.images.Digest(ctx, spec.Container.Image)
	if err != nil {
		return "", fmt.Errorf("failed to resolve image digest: %w", err)
	}

	return digest, nil
}

// missing names the referenced things of one kind that do not exist.
//
// Every name is checked rather than the counts compared, so that an operator is told
// which one to create rather than that one of them is absent.
func missing(names []string, held map[string]string) []string {
	var absent []string
	for _, name := range names {
		if _, ok := held[name]; !ok {
			absent = append(absent, name)
		}
	}

	return absent
}

// withResolvedPorts returns spec with every port's host side filled in.
//
// The resolved ports are part of the specification that gets hashed, which is what
// makes a reallocated port replace the container running on the old one: to the
// reconciler it is simply a specification that has changed.
func withResolvedPorts(spec api.WorkloadSpec, ports []database.Port) api.WorkloadSpec {
	if spec.Ports == nil || len(ports) == 0 {
		return spec
	}

	byPort := make(map[int]database.Port, len(ports))
	for _, port := range ports {
		byPort[port.Container] = port
	}

	mappings := make([]api.PortMapping, 0, len(ports))
	for _, mapping := range *spec.Ports {
		resolved, ok := byPort[mapping.To]
		if !ok {
			continue
		}

		mappings = append(mappings, api.PortMapping{To: mapping.To, From: new(resolved.Host)})
	}

	// The specification is taken by value, so assigning the mappings here replaces
	// only this copy's slice header and leaves the caller's alone.
	spec.Ports = &mappings

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
	if spec.Ports == nil {
		return nil
	}

	return *spec.Ports
}

func (s *WorkloadService) hydrate(ctx context.Context, row database.Workload) (Workload, error) {
	ports, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read workload ports: %w", err)
	}

	message, at := s.lastError(row.Name)

	return newWorkload(row, s.observe(ctx)[row.Name], ports, s.health(row.Name), message, at)
}

// observe groups the driver's instances by workload name.
//
// A driver that cannot be reached is deliberately not an error: the desired state
// is still worth reporting, and a read of it shouldn't fail because the runtime is
// briefly unavailable. The failure is logged and callers see workloads with no
// instances, which reads as pending.
//
// The call is bounded so that a wedged daemon makes a read of desired state slower
// rather than hanging it: a request that never returns is worse than one that
// reports what it does know.
func (s *WorkloadService) observe(ctx context.Context) map[string][]driver.Instance {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	var instances []driver.Instance
	for _, runtime := range s.drivers {
		observed, err := runtime.Observe(ctx)
		if err != nil {
			// Reported rather than returned: desired state is still worth reading, and
			// a read should not fail because one runtime is briefly unavailable.
			s.logger.With("error", err, "runtime", runtime.Name()).Error("failed to observe driver instances")

			continue
		}

		instances = append(instances, observed...)
	}

	byWorkload := make(map[string][]driver.Instance, len(instances))
	for _, instance := range instances {
		byWorkload[instance.Workload] = append(byWorkload[instance.Workload], instance)
	}

	return byWorkload
}

// wake asks for a reconciliation pass, so that a change to desired state is acted
// on immediately rather than on the next tick.
func (s *WorkloadService) wake() {
	if s.reconciler != nil {
		s.reconciler.Notify()
	}
}

// lastError returns why a workload's last converge pass failed, reporting zero values
// for one that is converging.
func (s *WorkloadService) lastError(workload string) (string, time.Time) {
	if s.reconciler == nil {
		return "", time.Time{}
	}

	message, at, ok := s.reconciler.LastError(workload)
	if !ok {
		return "", time.Time{}
	}

	return message, at
}

// health returns what orca knows about a workload's health.
func (s *WorkloadService) health(workload string) Health {
	if s.checker == nil {
		return Health{}
	}

	result, checked := s.checker.Result(workload)

	return Health{Checked: checked, Result: result}
}

// healthState reports the instance state a workload's health implies, so that a
// workload which is running but not working converges rather than being left alone.
//
// A failing check makes an instance failed, which routes it into the same paced
// restart a crashed container takes — the reaction to "not working" is the same
// whether the process died or merely stopped answering. A check that has not yet
// passed makes the instance pending, which the reconciler treats as up: a workload
// still starting must not be replaced for not having answered yet.
func healthState(state driver.State, reported Health) driver.State {
	if !reported.Checked || state != driver.StateRunning {
		return state
	}

	switch reported.Result.Status {
	case health.StatusUnhealthy:
		return driver.StateFailed
	case health.StatusStarting:
		return driver.StatePending
	default:
		return state
	}
}

// runtimeOf reports which runtime a specification names, which is determined by
// which block it carries rather than by a discriminator field. Exactly one block
// must be present.
func runtimeOf(spec api.WorkloadSpec) (api.Runtime, error) {
	switch {
	case spec.Container != nil && spec.Exec != nil:
		return "", ErrAmbiguousRuntime
	case spec.Container != nil:
		return api.Container, nil
	case spec.Exec != nil:
		return api.Exec, nil
	default:
		return "", ErrNoRuntime
	}
}

// canonicalise encodes spec as JSON and hashes it, mixing in the revision of each
// secret and the value of each variable the workload reads. Go's encoder writes
// struct fields in declaration order and map keys in sorted order, so the encoding is
// stable for a given specification and the hash can be compared to detect drift.
//
// The returned bytes are always the specification alone: what was read reaches the
// hash without being stored, so nothing about a secret is written to the database or
// echoed back by the API. Mixing it in is what makes a rotated secret or a changed
// variable read as an ordinary specification change, so the reconciler replaces the
// instances holding the old value.
//
// A secret contributes its revision and never its value, because the hash is
// reported and one computed over a value would confirm a guess at it. A variable
// contributes its value, which the API reports anyway.
//
// What the workload reads only through a mount naming a signal contributes nothing.
// Such a mount asked for the file to be rewritten and the workload signalled, and a
// hash that moved with the value would replace the instance instead.
//
// A pull-always workload's image digest is mixed in the same way, so a rebuilt tag
// reads as an ordinary specification change. It is empty for every other workload.
//
// A workload reading none of these hashes exactly as it would without this, which is
// what stops an upgrade replacing every running instance.
func canonicalise(spec api.WorkloadSpec, read references, digest string) ([]byte, string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode workload spec: %w", err)
	}

	revisions, values := read.hashed()

	if len(revisions) == 0 && len(values) == 0 && digest == "" {
		sum := sha256.Sum256(encoded)

		return encoded, hex.EncodeToString(sum[:]), nil
	}

	hashed, err := json.Marshal(hashedSpec{
		Spec:      encoded,
		Secrets:   revisions,
		Variables: values,
		Digest:    digest,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode workload spec: %w", err)
	}

	sum := sha256.Sum256(hashed)

	return encoded, hex.EncodeToString(sum[:]), nil
}

// hashed returns what reaches the specification's hash: the revision of each secret
// and the value of each variable, less anything read only through a mount naming a
// signal.
//
// Both maps come back nil when nothing is left, rather than empty. hashedSpec omits an
// empty map, so a workload whose only reading is refreshed hashes exactly as one that
// reads nothing at all — which is what keeps adding a signalling mount from being a
// specification change in its own right.
func (r references) hashed() (map[string]string, map[string]string) {
	if len(r.refreshed) == 0 {
		return r.revisions, r.values
	}

	revisions := maps.Clone(r.revisions)
	values := maps.Clone(r.values)

	for _, reference := range r.refreshed {
		if reference.Kind == manifest.KindVariable {
			delete(values, reference.Name)

			continue
		}

		delete(revisions, reference.Name)
	}

	if len(revisions) == 0 {
		revisions = nil
	}
	if len(values) == 0 {
		values = nil
	}

	return revisions, values
}

func newWorkload(row database.Workload, instances []driver.Instance, ports []database.Port, reported Health, lastError string, lastErrorAt time.Time) (Workload, error) {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return Workload{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	deleting := !row.DeletedAt.IsZero()
	suspended := !row.SuspendedAt.IsZero()
	policy := manifest.NewSpec(spec).Restart

	// An instance a driver keeps only so that its output can still be read is left out
	// of what the workload reports. It has ended and nothing will restart it, so
	// reporting it would have a workload that is running perfectly well read as failed
	// on the strength of the attempt before it — which is the opposite of what retaining
	// the output is for. Its output is reached through the logs endpoint instead.
	instances = slices.DeleteFunc(instances, func(instance driver.Instance) bool {
		return instance.Retained
	})

	// Health is folded into the instance states before the workload's own state is
	// derived, so a container that is up but not working reads as failed rather than
	// running — and is replaced by the same paced path a crashed one takes.
	//
	// The restart policy is applied after it, on the instances that have ended. An
	// instance the policy retires is finished with, so a stale health result must not
	// reopen the question of whether it is working.
	for i := range instances {
		instances[i].State = healthState(instances[i].State, reported)
		instances[i].State = CompletionState(instances[i], policy)
	}

	// A suspended workload's occurrences will not happen, so none is reported: a
	// time a caller could wait for that the server has no intention of honouring
	// would be worse than no answer.
	var next time.Time
	if !suspended {
		next = nextRun(manifest.NewSpec(spec).Schedule, instances, row.UpdatedAt)
	}

	return Workload{
		Name:        row.Name,
		Version:     row.Version,
		Runtime:     api.Runtime(row.Runtime),
		Spec:        spec,
		Labels:      row.Labels,
		Instances:   instances,
		Ports:       newResolvedPorts(ports),
		Health:      reported,
		State:       StateOf(instances, deleting, suspended),
		Deleting:    deleting,
		Suspended:   suspended,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		NextRun:     next,
		LastError:   lastError,
		LastErrorAt: lastErrorAt,
	}, nil
}

// nextRun reports when a scheduled workload runs again, or the zero time when it runs
// continuously or has not run yet.
//
// Derived on read rather than stored, like every other observed value: the occurrence
// is a function of the expression and the last run, both of which are already known.
func nextRun(schedule *manifest.Schedule, instances []driver.Instance, applied time.Time) time.Time {
	if schedule == nil {
		return time.Time{}
	}

	parsed, err := schedule.Parsed()
	if err != nil {
		// Validated before it was stored, so this means the specification and the
		// rules have diverged. Nothing useful can be reported.
		return time.Time{}
	}

	var last time.Time
	for _, instance := range instances {
		if instance.StartedAt.After(last) {
			last = instance.StartedAt
		}
	}

	// Counted from the last run, or from when the specification was applied for a
	// workload that has not run yet, which is what the reconciler does.
	if last.IsZero() {
		last = applied
	}

	return parsed.Next(last)
}

// CompletionState reports the state an ended instance reads as once its workload's
// restart policy has had its say.
//
// Only a clean exit the policy retires becomes completed. An instance that exited
// non-zero stays failed however the policy treats it, because how a workload ended and
// whether it runs again are separate facts: a job retired under "never" still has to
// say that it failed, or an operator reading it would see a success.
//
// An instance still running is untouched. The policy describes what happens when work
// ends, and this one has not ended.
func CompletionState(instance driver.Instance, restart *manifest.Restart) driver.State {
	if instance.State != driver.StateExited {
		return instance.State
	}

	// Attempts are not counted here. A workload that gave up has ended, and how it
	// ended is what this reports: giving up is the reconciler's decision about whether
	// to run it again.
	if restart.Policy.Restarts(instance.ExitCode) {
		return instance.State
	}

	return driver.StateCompleted
}

// StateOf derives a workload's overall state from its instances and whether it is
// being deleted or suspended.
//
// A workload marked for deletion is terminating whatever its instances are doing,
// because that is the only thing that will happen to it from here — reporting it as
// running while it is on its way out would invite a caller to wait for something
// that is never coming back.
//
// A suspended workload reads as suspended on the same reasoning: an instance still
// up is mid-stop, and nothing will run until the workload is started again. Neither
// stopped nor completed would be true — the server does not intend to fix it, and
// its restart policy did not ask for the end.
//
// Otherwise running wins: a workload whose replacement is already up while its
// predecessor is still going away is running, not terminating. Then teardown in
// progress is reported ahead of how the departing instance ended, since the exit is
// a consequence of the teardown rather than news in its own right. Failure outranks
// a clean exit, and a workload with no instances at all is pending, because the
// reconciler has yet to start it.
func StateOf(instances []driver.Instance, deleting, suspended bool) api.WorkloadState {
	if deleting {
		return api.WorkloadStateTerminating
	}

	if suspended {
		return api.WorkloadStateSuspended
	}

	if len(instances) == 0 {
		return api.WorkloadStatePending
	}

	var terminating, failed, exited, completed bool
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
		case driver.StateCompleted:
			completed = true
		}
	}

	// Ranked so that nothing masks a problem. A workload with one completed instance
	// and one failed instance is failed: the completion is true but it is not the fact
	// an operator needs first.
	switch {
	case terminating:
		return api.WorkloadStateTerminating
	case failed:
		return api.WorkloadStateFailed
	case exited:
		return api.WorkloadStateStopped
	case completed:
		return api.WorkloadStateCompleted
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
	// The specification that was submitted.
	Spec api.WorkloadSpec
	// Arbitrary key-value pairs attached to the workload.
	Labels map[string]string
	// The instances the driver is currently running for the workload.
	Instances []driver.Instance
	// The port mappings the server settled on, including any it allocated.
	Ports []api.ResolvedPort
	// What orca established about whether the workload is working.
	Health Health
	// The workload's overall state, derived from its instances and whether it is
	// being deleted or suspended.
	State api.WorkloadState
	// Whether the workload has been marked for deletion and is being torn down.
	Deleting bool
	// Whether the workload has been stopped and is intentionally not running.
	Suspended bool
	// The time the workload was first applied.
	CreatedAt time.Time
	// The time the workload's specification last changed, or a suspended
	// workload was last resumed.
	UpdatedAt time.Time
	// When the workload next runs, for one that names a schedule. Zero for a workload
	// that runs continuously, and for a scheduled one that has not run yet.
	NextRun time.Time
	// Why the last converge pass over the workload failed. Empty for one that is
	// converging. Held in memory by the reconciler, so it clears when a pass
	// succeeds and does not survive a server restart.
	LastError string
	// When the last converge failure was recorded, meaningful only when LastError
	// is set.
	LastErrorAt time.Time
}
