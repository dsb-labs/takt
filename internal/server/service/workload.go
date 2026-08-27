// Package service provides the domain orchestration layer for the orca server.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/spechash"
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
	// ErrWorkloadDeleting is returned when applying a workload that is currently
	// being torn down.
	ErrWorkloadDeleting = errors.New("workload is being deleted")
	// ErrWorkloadSuspended is returned when restarting a workload that is
	// suspended, since nothing would start until it is started again.
	ErrWorkloadSuspended = errors.New("workload is suspended")
	// ErrInvalidQuery is returned when a list query is malformed.
	ErrInvalidQuery = errors.New("invalid query")
	// ErrInvalidSpec is returned when a specification does not describe a runnable
	// workload.
	ErrInvalidSpec = errors.New("invalid specification")
	// ErrWorkloadInUse is returned when a workload another one references is deleted
	// without being forced.
	ErrWorkloadInUse = errors.New("workload is in use")
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
		HolderOf(ctx context.Context, host int, protocol string) (string, bool, error)
		// ListAll should return the ports allocated to every workload, keyed by
		// workload identifier.
		ListAll(ctx context.Context) (map[string][]database.Port, error)
		// Allocated should return every host port allocated to any workload, keyed
		// by the protocol it is allocated on.
		Allocated(ctx context.Context) (map[string][]int, error)
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
		// Resolve should settle every mapping on a host port, keeping the
		// allocations the workload already holds.
		Resolve(ctx context.Context, workload string, held []port.Claim, mappings []manifest.Port) ([]port.Claim, error)
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

	// The WorkloadState type names what a workload is doing overall, derived from
	// the instances the driver reports and whether the workload is being torn down.
	//
	// Owned here rather than taken from the wire format, because a state is
	// something orca establishes rather than something a caller submits. The HTTP
	// API maps it onto the state it publishes, as it does every other field.
	WorkloadState string

	// The ResolvedPort type describes a port mapping as it was actually applied,
	// carrying the host port the server settled on. This is what a caller uses to
	// reach the workload.
	ResolvedPort struct {
		// What the specification called this port. Empty for one it did not name.
		Name string
		// The port the workload listens on inside its runtime.
		To int
		// The host port that reaches it.
		From int
		// The transport protocol the port is published on.
		Protocol manifest.Protocol
		// Whether the host port was allocated by the server rather than pinned by
		// the specification.
		Dynamic bool
	}

	// The Health type reports what orca established about a workload's health,
	// and whether it checks the workload at all.
	Health struct {
		// Whether the workload declares a check orca performs.
		Checked bool
		// The most recent outcome, meaningful only when Checked.
		Result health.Result
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
		// Address should return the address the reference names, reporting
		// ErrWorkloadNotFound when nothing holds the name and ErrPortNotPublished
		// when the workload publishes no such port.
		Address(ctx context.Context, reference manifest.Reference) (string, error)
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
		// The names of the other workloads the specification references, without
		// repeats: one workload referenced at two ports is one name.
		workloads []string
		// The revision of each secret that exists, keyed by name.
		revisions map[string]string
		// The value of each variable that exists, keyed by name.
		values map[string]string
		// The address each workload reference resolved to, keyed by the reference as
		// it was written. Nil when the specification references no workload, so that
		// one which references none hashes as it did before it could.
		//
		// The whole address rather than the port alone. A workload is started with
		// what this holds, so a host that changed leaves every consumer holding an
		// address that no longer reaches anything — which is a change to the
		// specification in every sense that matters.
		addresses map[string]string
		// The workload references naming a workload that does not exist, and those
		// naming a port the referenced workload does not publish.
		//
		// Recorded rather than returned as an error, because a workload that has gone
		// has to move the hash of whatever reads it rather than stop it being
		// rehashed. Refusing the reference is the business of the caller that can act
		// on it, which is the apply.
		unknown     []string
		absentPorts []string
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
		addresses  WorkloadAddresses
		images     ImageResolver
		claims     Claimer
		checker    Checker
		reconciler Reconciler
	}
)

const (
	// WorkloadStatePending is a workload the reconciler has yet to start.
	WorkloadStatePending WorkloadState = "pending"
	// WorkloadStateRunning is a workload with an instance up.
	WorkloadStateRunning WorkloadState = "running"
	// WorkloadStateTerminating is a workload being torn down, or one whose instance
	// is on its way out.
	WorkloadStateTerminating WorkloadState = "terminating"
	// WorkloadStateStopped is a workload whose instance ended cleanly and will not
	// be started again.
	WorkloadStateStopped WorkloadState = "stopped"
	// WorkloadStateCompleted is a workload whose run finished, which is the end a
	// job is meant to reach.
	WorkloadStateCompleted WorkloadState = "completed"
	// WorkloadStateFailed is a workload whose instance ended badly.
	WorkloadStateFailed WorkloadState = "failed"
	// WorkloadStateSuspended is a workload that has been stopped and is
	// intentionally not running.
	WorkloadStateSuspended WorkloadState = "suspended"
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
		addresses:  config.Addresses,
		images:     config.Images,
		claims:     config.Claimer,
		checker:    config.Checker,
		reconciler: config.Reconciler,
	}
}

// Apply stores spec as the desired state for its name, returning the resulting
// workload and whether it was newly created.
//
// Applying an unchanged specification is a no-op that leaves the version alone;
// a changed one increments it, which is what later causes the reconciler to
// replace any running instance. Returns manifest.ErrNoRuntime when the
// specification names no runtime, or ErrUnsupportedRuntime when it names one the
// server cannot run.
func (s *WorkloadService) Apply(ctx context.Context, spec manifest.Spec) (Workload, bool, error) {
	// Resolved here rather than trusted from the caller, so that what gets stored and
	// hashed is the specification with its defaults in place however it arrived. A
	// caller that already resolved them changes nothing by asking again.
	spec.Defaults()

	// The runtime is resolved first so that naming none or naming two is reported as
	// exactly that, rather than as a general validation failure.
	runtime, err := manifest.RuntimeOf(spec)
	if err != nil {
		return Workload{}, false, err
	}

	// Everything else is validated here rather than only in the client that parsed a
	// manifest. The rules are what make a workload runnable at all — a name the
	// runtime can represent, an image to run, a schedule that parses — so a caller
	// that skips the CLI has to be held to them too. Without this the server accepted
	// an unknown schema version, a name breaking its own documented rules, and an
	// empty image that could only ever fail to start.
	if err = manifest.Validate(spec); err != nil {
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

	// A workload that does not exist is refused here rather than at start, as a secret
	// is. The workload could never run, and the operator asking for it is the one who
	// can fix the name or apply the workload it names first.
	if len(read.unknown) > 0 {
		return Workload{}, false, fmt.Errorf("%w: %s", ErrWorkloadNotFound, strings.Join(read.unknown, ", "))
	}

	if len(read.absentPorts) > 0 {
		return Workload{}, false, fmt.Errorf("%w: %s", ErrPortNotPublished, strings.Join(read.absentPorts, ", "))
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

	// Whatever reads this workload's address is rehashed after it has landed, since
	// an apply may have moved the ports it publishes.
	s.redeploy(ctx, stored.Name)

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
	spec manifest.Spec,
	runtime manifest.Runtime,
	held []database.Port,
	read references,
	digest string,
) (database.Workload, bool, error) {
	// Bounded because a caller waiting on a request would rather hear that orca
	// couldn't settle its ports than wait indefinitely for a quiet moment.
	const attempts = 5

	current := heldClaims(held)

	for attempt := range attempts {
		claims, err := s.claims.Resolve(ctx, spec.Name, current, spec.Ports)
		if err != nil {
			return database.Workload{}, false, err
		}

		ports := allocations(claims, "")

		encoded, hash, err := spechash.Compute(port.Resolved(spec, claims), read.hashInputs(digest))
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
			Workloads: read.workloads,
		}

		row.Labels = spec.Labels

		// The row and its ports are written together, so a claim that loses a race
		// leaves no workload behind for the reconciler to start against ports
		// nothing holds.
		stored, created, err := s.workloads.Upsert(ctx, row, ports...)
		switch {
		case err == nil:
			return stored, created, nil
		case !errors.Is(err, database.ErrHostPortTaken):
			return database.Workload{}, false, fmt.Errorf("failed to store workload: %w", err)
		case port.Pinned(spec.Ports):
			return database.Workload{}, false, fmt.Errorf("%w: %v", port.ErrHostPortTaken, err)
		}

		s.logger.With("workload", spec.Name, "attempt", attempt+1).
			Debug("host port was claimed by another workload, allocating again")
	}

	return database.Workload{}, false, fmt.Errorf("%w: gave up after %d attempts", port.ErrHostPortTaken, attempts)
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
//
// A workload another one references is refused with ErrWorkloadInUse unless force is
// set, and the error names the workloads reading its address. An apply naming a
// workload that does not exist is refused, so removing one without this check would
// leave a specification nobody could re-apply. Forcing it through redeploys those
// workloads, which then report the reference they can no longer resolve and retry
// until something holds the name again.
func (s *WorkloadService) Delete(ctx context.Context, name string, force bool) (Workload, error) {
	referencing, err := s.workloads.ReferencedBy(ctx, name)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read the workloads referencing this one: %w", err)
	}

	if len(referencing) > 0 && !force {
		return Workload{}, fmt.Errorf("%w: referenced by %s", ErrWorkloadInUse, strings.Join(referencing, ", "))
	}

	marked, err := s.workloads.MarkDeleting(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to mark workload for deletion: %w", err)
	}

	// Rehashed once the workload is on its way out, for the reason a deleted secret
	// rehashes what read it: what those workloads were started against no longer
	// describes what orca holds, and the hash is how that is reported.
	s.rehashAll(ctx, name, referencing)

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

	spec, err := manifest.Decode(row.Spec)
	if err != nil {
		return false, err
	}

	// Dropping the dynamic allocations is what makes resolution pick new ports for
	// them, since resolution only reuses what it finds still held.
	var pinned []port.Claim
	var dynamic bool

	current := heldClaims(held)
	for _, claim := range current {
		if claim.Dynamic {
			dynamic = true
			continue
		}

		pinned = append(pinned, claim)
	}

	if !dynamic {
		return false, nil
	}

	// The stored specification already has its host ports filled in, so the dynamic
	// ones are cleared to ask for a fresh allocation rather than the same port back.
	claims, err := s.claims.Resolve(ctx, name, pinned, port.Requested(spec.Ports, current))
	if err != nil {
		return false, err
	}

	ports := allocations(claims, row.ID)

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

	encoded, hash, err := spechash.Compute(port.Resolved(spec, claims), read.hashInputs(digest))
	if err != nil {
		return false, err
	}

	row.Spec, row.SpecHash = encoded, hash
	row.Secrets, row.Variables, row.Workloads = read.secrets, read.variables, read.workloads

	// The ports travel with the write, as they do for an apply. Claiming them
	// separately beforehand would not survive it: the write replaces a workload's
	// allocation with whatever it was handed, so an Upsert given none clears the
	// rows that were just claimed and leaves the specification naming host ports
	// nothing holds.
	if _, _, err = s.workloads.Upsert(ctx, row, ports...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	// The whole point of the reallocation is that the workload's address moved, so
	// everything reading it is now holding one that reaches nothing.
	s.redeploy(ctx, name)

	s.wake()

	return true, nil
}

// redeploy rehashes the workloads referencing the named one, so that a host port that
// moved replaces the instances reading the address it was reached at.
//
// This is the one place a hash moves for a reason nothing went through the service
// for. A secret or a variable changes because somebody set it, where a host port is
// reallocated by the reconciler when a workload fails to start. Without this the
// reference reads as automatic and quietly is not.
//
// A failure is logged rather than returned. The workload whose ports moved is already
// stored, and reporting its apply or its reallocation as failed would describe
// something that did not happen. A consumer left unrehashed is holding an address that
// may well still reach the workload, and the next thing to touch it recomputes the
// hash against what orca currently holds.
func (s *WorkloadService) redeploy(ctx context.Context, name string) {
	referencing, err := s.workloads.ReferencedBy(ctx, name)
	if err != nil {
		s.logger.With("workload", name, "error", err).Error("failed to read the workloads referencing this one")

		return
	}

	s.rehashAll(ctx, name, referencing)
}

// rehashAll rehashes each of the named workloads, which reference the workload whose
// address may have moved.
//
// Separate from redeploy so that a caller which has already read the referencing
// workloads — a deletion, which had to read them to refuse one — does not read them a
// second time to act on them.
func (s *WorkloadService) rehashAll(ctx context.Context, name string, referencing []string) {
	for _, workload := range referencing {
		if _, err := s.Rehash(ctx, workload); err != nil {
			s.logger.With("workload", workload, "references", name, "error", err).
				Error("failed to rehash a workload referencing one whose address may have moved")
		}
	}
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

	spec, err := manifest.Decode(row.Spec)
	if err != nil {
		return false, err
	}

	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return false, err
	}

	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return false, err
	}

	_, hash, err := spechash.Compute(spec, read.hashInputs(digest))
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
	row.Secrets, row.Variables, row.Workloads = read.secrets, read.variables, read.workloads

	if _, _, err = s.workloads.Upsert(ctx, row, held...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload rehashed")
	s.wake()

	return true, nil
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
func (s *WorkloadService) resolveVolumes(ctx context.Context, spec manifest.Spec) (manifest.Spec, error) {
	if len(spec.Volumes) == 0 {
		return spec, nil
	}

	mounts := make([]manifest.VolumeMount, 0, len(spec.Volumes))
	for _, mount := range spec.Volumes {
		// Which source a mount names is read through the same rules validation
		// applied, rather than by inspecting the fields again here.
		kind, err := manifest.KindOf(mount)
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
		path, err := s.volumes.Path(ctx, mount.Name)
		if err != nil {
			return spec, err
		}

		mount.From = path
		mounts = append(mounts, mount)
	}

	// The specification is taken by value, so this replaces only this copy's slice
	// header and leaves the caller's alone.
	spec.Volumes = mounts

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
// makes deleting a secret, a variable or a referenced workload move the hash of the
// workloads reading it, which is how they come to report that something they need has
// gone. Refusing the reference is the business of the caller that can act on it.
func (s *WorkloadService) resolveReferences(ctx context.Context, spec manifest.Spec) (references, error) {
	found, err := manifest.References(spec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	refreshed, err := manifest.Refreshed(spec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	resolved := references{
		secrets:   manifest.Names(found, manifest.KindSecret),
		variables: manifest.Names(found, manifest.KindVariable),
		workloads: manifest.Names(found, manifest.KindWorkload),
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

	// The port a reference names is part of what is being resolved, so these are read
	// one reference at a time rather than one workload at a time.
	for _, reference := range manifest.Of(found, manifest.KindWorkload) {
		if s.addresses == nil {
			return references{}, fmt.Errorf("%w: this server resolves no workload addresses", ErrWorkloadNotFound)
		}

		address, err := s.addresses.Address(ctx, reference)
		switch {
		case errors.Is(err, ErrWorkloadNotFound):
			resolved.unknown = append(resolved.unknown, reference.String())

			continue
		case errors.Is(err, ErrPortNotPublished):
			resolved.absentPorts = append(resolved.absentPorts, reference.String())

			continue
		case err != nil:
			return references{}, fmt.Errorf("failed to resolve the address of workload %s: %w", reference.Name, err)
		}

		if resolved.addresses == nil {
			resolved.addresses = make(map[string]string, len(found))
		}

		resolved.addresses[reference.String()] = address
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
func (s *WorkloadService) resolveDigest(ctx context.Context, spec manifest.Spec) (string, error) {
	if spec.Container == nil || spec.Container.Pull != manifest.PullAlways {
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

// heldClaims maps stored allocations onto what claiming reads. The workload
// identifier is dropped, because settling a workload's ports does not need to know
// which workload it is settling.
func heldClaims(ports []database.Port) []port.Claim {
	claims := make([]port.Claim, 0, len(ports))
	for _, allocation := range ports {
		claims = append(claims, port.Claim{
			Name:      allocation.Name,
			Container: allocation.Container,
			Host:      allocation.Host,
			Protocol:  port.Protocol(allocation.Protocol),
			Dynamic:   allocation.Dynamic,
		})
	}

	return claims
}

// allocations maps settled claims onto the rows that record them, against the
// workload they belong to. The identifier is empty for an apply, whose row is written
// in the same statement and has none to give yet.
func allocations(claims []port.Claim, workloadID string) []database.Port {
	ports := make([]database.Port, 0, len(claims))
	for _, claim := range claims {
		ports = append(ports, database.Port{
			WorkloadID: workloadID,
			Name:       claim.Name,
			Container:  claim.Container,
			Host:       claim.Host,
			Protocol:   string(claim.Protocol),
			Dynamic:    claim.Dynamic,
		})
	}

	return ports
}

// newResolvedPorts maps stored allocations onto the service's view of them.
func newResolvedPorts(ports []database.Port) []ResolvedPort {
	if len(ports) == 0 {
		return nil
	}

	resolved := make([]ResolvedPort, 0, len(ports))
	for _, port := range ports {
		resolved = append(resolved, ResolvedPort{
			Name:     port.Name,
			To:       port.Container,
			From:     port.Host,
			Protocol: manifest.Protocol(port.Protocol),
			Dynamic:  port.Dynamic,
		})
	}

	return resolved
}

func (s *WorkloadService) hydrate(ctx context.Context, row database.Workload) (Workload, error) {
	ports, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read workload ports: %w", err)
	}

	message, at := s.lastError(row.Name)

	return newWorkload(row, s.observeWorkload(ctx, row), ports, s.health(row.Name), message, at)
}

// observeWorkload asks each driver what it is running for one workload.
//
// Reading a single workload used to observe every workload on the host and keep one
// entry, so the cost of reading one grew with the number running. A load test
// measured a single read at half a millisecond against one workload and fifty-four
// against a hundred and sixty, for the same request — and this is the path every
// apply, delete, stop, start and restart returns through, not only a get.
//
// Failures are handled as they are for a full observation: reported, not returned.
// The reasoning there applies unchanged.
func (s *WorkloadService) observeWorkload(ctx context.Context, row database.Workload) []driver.Instance {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	var instances []driver.Instance

	for _, runtime := range s.drivers {
		observed, err := runtime.ObserveWorkload(ctx, row.ID, row.Name)
		if err != nil {
			s.logger.With("error", err, "runtime", runtime.Name(), "workload", row.Name).
				Error("failed to observe driver instances")

			continue
		}

		instances = append(instances, observed...)
	}

	return instances
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

// hashInputs returns what the specification's hash covers beyond the specification
// itself.
//
// Narrower than the references type it comes from. The names a workload reads are
// stored so that finding what to redeploy does not depend on parsing every
// specification, and a reference naming something that has gone is the apply's
// business. Neither reaches the hash.
func (r references) hashInputs(digest string) spechash.Inputs {
	return spechash.Inputs{
		Revisions: r.revisions,
		Values:    r.values,
		Addresses: r.addresses,
		Refreshed: r.refreshed,
		Digest:    digest,
	}
}

func newWorkload(row database.Workload, instances []driver.Instance, ports []database.Port, reported Health, lastError string, lastErrorAt time.Time) (Workload, error) {
	spec, err := manifest.Decode(row.Spec)
	if err != nil {
		return Workload{}, err
	}

	deleting := !row.DeletedAt.IsZero()
	suspended := !row.SuspendedAt.IsZero()
	policy := spec.Restart

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
		next = nextRun(spec.Schedule, instances, row.UpdatedAt)
	}

	return Workload{
		Name:        row.Name,
		Version:     row.Version,
		Runtime:     manifest.Runtime(row.Runtime),
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
func StateOf(instances []driver.Instance, deleting, suspended bool) WorkloadState {
	if deleting {
		return WorkloadStateTerminating
	}

	if suspended {
		return WorkloadStateSuspended
	}

	if len(instances) == 0 {
		return WorkloadStatePending
	}

	var terminating, failed, exited, completed bool
	for _, instance := range instances {
		switch instance.State {
		case driver.StateRunning:
			return WorkloadStateRunning
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
		return WorkloadStateTerminating
	case failed:
		return WorkloadStateFailed
	case exited:
		return WorkloadStateStopped
	case completed:
		return WorkloadStateCompleted
	default:
		return WorkloadStatePending
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
	Runtime manifest.Runtime
	// The specification that was submitted.
	Spec manifest.Spec
	// Arbitrary key-value pairs attached to the workload.
	Labels map[string]string
	// The instances the driver is currently running for the workload.
	Instances []driver.Instance
	// The port mappings the server settled on, including any it allocated.
	Ports []ResolvedPort
	// What orca established about whether the workload is working.
	Health Health
	// The workload's overall state, derived from its instances and whether it is
	// being deleted or suspended.
	State WorkloadState
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
