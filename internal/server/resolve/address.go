// Package resolve turns the references in a workload's specification into the
// values they name: a secret's plaintext, a variable's value, or the address
// another workload is reached at.
//
// It sits below the service package so that both tiers can resolve: the workload
// service proves a reference resolves as a specification is applied, and the
// reconciler resolves the real values as a workload starts.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"strconv"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrPortNotPublished is returned when a reference names a port the referenced
	// workload does not publish.
	ErrPortNotPublished = errors.New("port not published")
)

type (
	// The WorkloadLocator interface describes how the address resolver finds the
	// workload a reference names.
	//
	// Narrower than the repository it is satisfied by: resolving an address needs one
	// workload at a time and nothing else.
	WorkloadLocator interface {
		// Get should return the workload with the given name, reporting
		// database.ErrWorkloadNotFound when no such workload exists.
		Get(ctx context.Context, name string) (database.Workload, error)
	}

	// The PortLocator interface describes how the address resolver finds the ports a
	// workload publishes.
	PortLocator interface {
		// List should return the ports allocated to the workload with the given
		// identifier.
		List(ctx context.Context, workloadID string) ([]database.Port, error)
	}

	// The AddressResolver type turns a reference to another workload into the address
	// that workload is reached at.
	//
	// The host is the same for every workload, since takt publishes their ports on
	// this one machine. What differs is the port, which takt may have chosen and may
	// revise, and which is the reason a workload's address is worth referencing
	// rather than writing down.
	AddressResolver struct {
		logger    *slog.Logger
		workloads WorkloadLocator
		ports     PortLocator
		address   string
	}

	// The Target type names the instance of a referenced workload a reader lands
	// on, which is what a reader's readiness is judged against.
	Target struct {
		// The name of the referenced workload.
		Workload string
		// The index of the instance the reader's arithmetic picked.
		Instance int
	}

	// The AddressResolverConfig type contains fields used to construct an
	// AddressResolver.
	AddressResolverConfig struct {
		// The logger used for resolution events.
		Logger *slog.Logger
		// Where the referenced workload is read from.
		Workloads WorkloadLocator
		// Where the ports a workload publishes are read from.
		Ports PortLocator
		// The address a workload dials to reach another workload's published ports.
		Address string
	}
)

// NewAddressResolver returns a new instance of the AddressResolver type.
func NewAddressResolver(config AddressResolverConfig) *AddressResolver {
	return &AddressResolver{
		logger:    config.Logger.With("component", "resolve"),
		workloads: config.Workloads,
		ports:     config.Ports,
		address:   config.Address,
	}
}

// Address returns the address the reference names, as read by one instance of the
// referencing workload.
//
// A reference naming a port resolves to a host and a port. One naming none resolves
// to the host alone, so that a workload composing an address it already knows the
// port of does not have to name it twice.
//
// A workload running several instances publishes a port at several addresses, and a
// reference still resolves to one. Which one is chosen by the reader's identity:
// instance readerInstance of the workload named reader lands on
// (hash(reader) + readerInstance) mod count. The choice is deterministic, so a
// dry run and an apply agree, and a reader workload's own instances spread evenly
// across the target's. A count change moves the arithmetic and the readers follow,
// which is rebalancing rather than an accident.
//
// The referenced workload must publish at least one port, whichever form was written.
// A workload publishing none is reachable at no address, so a reference to one could
// never mean anything.
//
// Returns database.ErrWorkloadNotFound when nothing holds the name, or
// ErrPortNotPublished when the workload holds it but publishes no such port.
func (r *AddressResolver) Address(ctx context.Context, reference manifest.Reference, reader string, readerInstance int) (string, error) {
	_, published, err := r.published(ctx, reference)
	if err != nil {
		return "", err
	}

	if reference.Port == "" {
		return r.address, nil
	}

	slot := pick(reader, readerInstance, countOf(published))

	for _, port := range published {
		if port.Instance != slot {
			continue
		}

		if reference.Port.Matches(port.Name, port.Container) {
			return net.JoinHostPort(r.address, strconv.Itoa(port.Host)), nil
		}
	}

	return "", fmt.Errorf("%w: workload %s does not publish %s", ErrPortNotPublished, reference.Name, reference.Port)
}

// Target returns which instance of the referenced workload the given instance of
// the reader lands on, by the same arithmetic Address uses. A reference naming no
// port resolves to the host alone and lands on no instance in particular, so it
// reports false.
//
// This exists for the reconciler, which holds a reader's first start until the
// instance it will talk to is ready. Returns the errors Address does.
func (r *AddressResolver) Target(ctx context.Context, reference manifest.Reference, reader string, readerInstance int) (Target, bool, error) {
	if reference.Port == "" {
		return Target{}, false, nil
	}

	row, published, err := r.published(ctx, reference)
	if err != nil {
		return Target{}, false, err
	}

	return Target{Workload: row.Name, Instance: pick(reader, readerInstance, countOf(published))}, true, nil
}

// published reads the referenced workload and the ports it publishes, refusing a
// workload that publishes none: such a workload is reachable at no address, so a
// reference to one could never mean anything.
func (r *AddressResolver) published(ctx context.Context, reference manifest.Reference) (database.Workload, []database.Port, error) {
	row, err := r.workloads.Get(ctx, reference.Name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return database.Workload{}, nil, fmt.Errorf("%w: %s", database.ErrWorkloadNotFound, reference.Name)
	case err != nil:
		return database.Workload{}, nil, fmt.Errorf("failed to load workload: %w", err)
	}

	published, err := r.ports.List(ctx, row.ID)
	if err != nil {
		return database.Workload{}, nil, fmt.Errorf("failed to read workload ports: %w", err)
	}

	if len(published) == 0 {
		return database.Workload{}, nil, fmt.Errorf("%w: workload %s publishes no ports", ErrPortNotPublished, reference.Name)
	}

	return row, published, nil
}

// countOf reads how many instances a workload runs from the ports it publishes.
//
// Read from the rows rather than by decoding the specification: every instance
// holds rows, so the highest index says how many there are, and the rows are what
// is being chosen between anyway.
func countOf(published []database.Port) int {
	count := 1
	for _, port := range published {
		if port.Instance >= count {
			count = port.Instance + 1
		}
	}

	return count
}

// pick chooses which of a target's instances a reader lands on.
//
// The offset by the reader's own instance is what spreads a reader workload's
// replicas exactly evenly: three readers of a three-instance target land one on
// each. The hash spreads unrelated readers, so they do not all crowd the first
// instance.
func pick(reader string, readerInstance, count int) int {
	if count <= 1 {
		return 0
	}

	digest := fnv.New32a()
	_, _ = digest.Write([]byte(reader))

	return int((digest.Sum32() + uint32(readerInstance)) % uint32(count))
}
