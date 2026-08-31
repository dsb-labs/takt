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

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/pkg/manifest"
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
	// The host is the same for every workload, since orca publishes their ports on
	// this one machine. What differs is the port, which orca may have chosen and may
	// revise, and which is the reason a workload's address is worth referencing
	// rather than writing down.
	AddressResolver struct {
		logger    *slog.Logger
		workloads WorkloadLocator
		ports     PortLocator
		address   string
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
	row, err := r.workloads.Get(ctx, reference.Name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return "", fmt.Errorf("%w: %s", database.ErrWorkloadNotFound, reference.Name)
	case err != nil:
		return "", fmt.Errorf("failed to load workload: %w", err)
	}

	published, err := r.ports.List(ctx, row.ID)
	if err != nil {
		return "", fmt.Errorf("failed to read workload ports: %w", err)
	}

	if len(published) == 0 {
		return "", fmt.Errorf("%w: workload %s publishes no ports", ErrPortNotPublished, reference.Name)
	}

	if reference.Port == "" {
		return r.address, nil
	}

	// The count is read from the rows rather than by decoding the specification:
	// every instance holds rows, so the highest index says how many there are, and
	// the rows are what is being chosen between anyway.
	count := 1
	for _, port := range published {
		if port.Instance >= count {
			count = port.Instance + 1
		}
	}

	slot := pick(reader, readerInstance, count)

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
