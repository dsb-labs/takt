package port

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/orca/pkg/manifest"
)

var (
	// ErrHostPortTaken is returned when a specification pins a host port that
	// another workload already holds.
	ErrHostPortTaken = errors.New("host port already in use")
	// ErrNoPortsAvailable is returned when no host port is free for a workload that
	// needs one allocated.
	ErrNoPortsAvailable = errors.New("no host port available")
)

type (
	// The Claim type is the host port one of a workload's ports was settled on.
	//
	// It is what claiming produces and what the caller persists. A claim carries no
	// workload identifier, because the caller writing it knows which workload it
	// belongs to and this package does not.
	Claim struct {
		// What the specification called the port, which is how the rest of a
		// manifest refers to it. Empty for a port the specification did not name.
		Name string
		// The port the workload listens on inside its runtime.
		Container int
		// The host port that reaches it.
		Host int
		// The transport protocol the port is published on.
		Protocol Protocol
		// Whether the host port was allocated here rather than pinned by the
		// specification. A dynamic port may be reallocated if it proves unusable; a
		// pinned one may not.
		Dynamic bool
	}

	// The Repository interface describes what claiming reads about the ports every
	// workload already holds.
	//
	// Narrower than the port repository it is satisfied by: settling a workload's
	// ports needs to know what is taken and who holds a given one, and nothing else.
	Repository interface {
		// HolderOf should name the workload the given host port is allocated to,
		// reporting false when no workload holds it.
		HolderOf(ctx context.Context, host int, protocol string) (string, bool, error)
		// Allocated should return every host port allocated to any workload, keyed
		// by the protocol it is allocated on.
		Allocated(ctx context.Context) (map[string][]int, error)
	}

	// The Claimer type settles a workload's ports on the host ports they are reached
	// at, allocating one for every mapping that did not ask for a particular port.
	Claimer struct {
		allocator *Allocator
		ports     Repository
	}

	// The ClaimerConfig type contains fields used to construct a Claimer.
	ClaimerConfig struct {
		// The allocator used to choose a host port for a mapping that names none.
		Allocator *Allocator
		// Where the ports every workload already holds are read from.
		Ports Repository
	}

	// The key type identifies one of a workload's ports. The protocol is part of it
	// because TCP and UDP are separate address spaces, so 53/tcp and 53/udp are
	// different ports.
	key struct {
		container int
		protocol  Protocol
	}
)

// NewClaimer returns a Claimer that allocates through the given allocator.
func NewClaimer(config ClaimerConfig) *Claimer {
	return &Claimer{
		allocator: config.Allocator,
		ports:     config.Ports,
	}
}

// Pinned reports whether any mapping names a host port explicitly, which decides
// whether a claim collision is the caller's problem or orca's to retry.
func Pinned(mappings []manifest.Port) bool {
	for _, mapping := range mappings {
		if mapping.From != 0 {
			return true
		}
	}

	return false
}

// Requested recovers what a specification originally asked for from the stored one,
// whose host ports have already been resolved. A mapping whose host port was
// allocated is returned without it, so resolution allocates afresh; a pinned one
// keeps it.
func Requested(mappings []manifest.Port, held []Claim) []manifest.Port {
	allocated := make(map[key]struct{}, len(held))
	for _, claim := range held {
		if claim.Dynamic {
			allocated[key{claim.Container, claim.Protocol}] = struct{}{}
		}
	}

	requested := make([]manifest.Port, 0, len(mappings))

	for _, mapping := range mappings {
		if _, ok := allocated[key{mapping.To, protocolOf(mapping)}]; ok {
			requested = append(requested, manifest.Port{Name: mapping.Name, To: mapping.To, Protocol: mapping.Protocol})
			continue
		}

		requested = append(requested, mapping)
	}

	return requested
}

// Resolve settles every mapping on a host port, keeping the allocations the workload
// already holds.
//
// A port's protocol is part of its identity here as it is in the schema, since TCP
// and UDP are separate address spaces: what is taken on one says nothing about the
// other, and resolving them against a single set would refuse ports that are free.
func (c *Claimer) Resolve(ctx context.Context, workload string, existing []Claim, mappings []manifest.Port) ([]Claim, error) {
	held := make(map[key]Claim, len(existing))
	for _, claim := range existing {
		held[key{claim.Container, claim.Protocol}] = claim
	}

	// Ports already promised to any workload are off limits, along with the ones
	// resolved so far in this specification.
	allocated, err := c.ports.Allocated(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read allocated ports: %w", err)
	}

	taken := make(map[Protocol][]int, len(allocated))
	for protocol, ports := range allocated {
		taken[Protocol(protocol)] = ports
	}

	// A pinned port is taken by this specification whatever else it asks for, so it
	// is off limits before anything is allocated around it.
	for _, mapping := range mappings {
		if mapping.From != 0 {
			protocol := protocolOf(mapping)
			taken[protocol] = append(taken[protocol], mapping.From)
		}
	}

	allocations, err := c.allocate(held, taken, mappings)
	if err != nil {
		return nil, err
	}

	resolved := make([]Claim, 0, len(mappings))
	for _, mapping := range mappings {
		claim, err := c.resolve(ctx, workload, held, allocations, mapping)
		if err != nil {
			return nil, err
		}

		resolved = append(resolved, claim)
	}

	return resolved, nil
}

// allocate chooses a host port for every mapping that needs one, reporting them by
// the port and protocol they belong to.
//
// The mappings of one container port are allocated together, so a workload publishing
// 53 over both protocols is reached at the same number on each rather than at two
// unrelated ones. Allocating them separately would work — the address spaces are
// independent — but a DNS server answering on 20000/udp and 20014/tcp reads as an
// accident.
func (c *Claimer) allocate(held map[key]Claim, taken map[Protocol][]int, mappings []manifest.Port) (map[key]int, error) {
	var order []int

	groups := make(map[int][]Protocol)

	for _, mapping := range mappings {
		// A pinned port was chosen by the caller, and one the workload already holds
		// stays where it is so that its address doesn't move every time something
		// unrelated about the workload changes.
		if mapping.From != 0 {
			continue
		}

		protocol := protocolOf(mapping)
		if previous, ok := held[key{mapping.To, protocol}]; ok && previous.Dynamic {
			continue
		}

		if _, ok := groups[mapping.To]; !ok {
			order = append(order, mapping.To)
		}

		groups[mapping.To] = append(groups[mapping.To], protocol)
	}

	allocations := make(map[key]int, len(order))

	for _, container := range order {
		protocols := groups[container]

		host, err := c.allocator.Allocate(protocols, taken)
		if err != nil {
			if errors.Is(err, ErrRangeExhausted) {
				// Every port orca may allocate is in use. The request was valid and
				// will become servable when a workload is deleted or the range
				// widened, so it is reported as a capacity problem rather than a
				// fault or a bad request.
				return nil, fmt.Errorf("%w for %d: %v", ErrNoPortsAvailable, container, err)
			}

			return nil, fmt.Errorf("failed to allocate host port for %d: %w", container, err)
		}

		for _, protocol := range protocols {
			allocations[key{container, protocol}] = host
			taken[protocol] = append(taken[protocol], host)
		}
	}

	return allocations, nil
}

// resolve settles one mapping on the host port it will be reached at.
func (c *Claimer) resolve(
	ctx context.Context,
	workload string,
	held map[key]Claim,
	allocations map[key]int,
	mapping manifest.Port,
) (Claim, error) {
	protocol := protocolOf(mapping)

	// A pinned host port is a decision orca must not quietly override, so it is
	// used as given once nothing else holds it.
	if mapping.From != 0 {
		holder, isHeld, err := c.ports.HolderOf(ctx, mapping.From, string(protocol))
		switch {
		case err != nil:
			return Claim{}, fmt.Errorf("failed to look up host port: %w", err)
		case isHeld && holder != workload:
			return Claim{}, fmt.Errorf("%w: %d/%s is used by workload %q",
				ErrHostPortTaken, mapping.From, protocol, holder)
		}

		return Claim{
			Name:      mapping.Name,
			Container: mapping.To,
			Host:      mapping.From,
			Protocol:  protocol,
		}, nil
	}

	// An existing allocation is kept so that the workload's address doesn't move
	// every time something unrelated about it changes. Only the host port is kept:
	// renaming a port is a change to what the specification calls it rather than a
	// reason to move where it is reached.
	if previous, ok := held[key{mapping.To, protocol}]; ok && previous.Dynamic {
		previous.Name = mapping.Name

		return previous, nil
	}

	return Claim{
		Name:      mapping.Name,
		Container: mapping.To,
		Host:      allocations[key{mapping.To, protocol}],
		Protocol:  protocol,
		Dynamic:   true,
	}, nil
}

// protocolOf reports which protocol a mapping publishes on.
//
// A mapping naming none asks for TCP, which is what every specification stored before
// the protocol existed described.
func protocolOf(mapping manifest.Port) Protocol {
	if mapping.Protocol == "" {
		return ProtocolTCP
	}

	return Protocol(mapping.Protocol)
}
