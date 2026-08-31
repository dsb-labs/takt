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
		// The index of the workload instance the claim belongs to. Each instance
		// settles the same mappings on host ports of its own.
		Instance int
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
		// specification. A dynamic port may be reallocated if it proves unusable.
		// A pinned one may not.
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

	// The Allocation interface describes how a Claimer obtains a host port for a
	// mapping that names none.
	//
	// The Allocator in this package satisfies it. It is an interface so that a
	// caller's tests can settle ports against something deterministic, since the
	// real allocator picks at random and binds a socket to prove a port is free.
	Allocation interface {
		// Allocate should return a host port that is free on every protocol named,
		// avoiding the ports already taken on each of them.
		Allocate(protocols []Protocol, taken map[Protocol][]int) (int, error)
	}

	// The Claimer type settles a workload's ports on the host ports they are reached
	// at, allocating one for every mapping that did not ask for a particular port.
	Claimer struct {
		allocator Allocation
		ports     Repository
	}

	// The ClaimerConfig type contains fields used to construct a Claimer.
	ClaimerConfig struct {
		// The allocator used to choose a host port for a mapping that names none.
		Allocator Allocation
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
// allocated is returned without it, so resolution allocates afresh. A pinned one
// keeps it.
func Requested(mappings []manifest.Port, held []Claim) []manifest.Port {
	allocated := make(map[key]struct{}, len(held))
	for _, claim := range held {
		// The stored specification carries the first instance's resolutions, so
		// what was originally asked for is recovered against that instance alone.
		if claim.Dynamic && claim.Instance == 0 {
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

// Resolve settles every mapping on a host port for each of the workload's
// instances, keeping the allocations every instance already holds.
//
// A port's protocol is part of its identity here as it is in the schema, since TCP
// and UDP are separate address spaces: what is taken on one says nothing about the
// other, and resolving them against a single set would refuse ports that are free.
func (c *Claimer) Resolve(ctx context.Context, workload string, existing []Claim, mappings []manifest.Port, count int) ([]Claim, error) {
	// One host port cannot reach more than one instance, so a specification
	// pinning a port resolves for a single instance only. Validation refuses the
	// combination before it gets here, and this guards the invariant if it ever
	// does not.
	if count > 1 && Pinned(mappings) {
		return nil, fmt.Errorf("%w: a pinned host port cannot serve %d instances", ErrHostPortTaken, count)
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

	resolved := make([]Claim, 0, count*len(mappings))

	// Instance by instance, so each one keeps what it held and allocates around
	// what every earlier one settled: allocate appends its choices to taken.
	for instance := range count {
		held := heldBy(existing, instance)

		allocations, err := c.allocate(held, taken, mappings)
		if err != nil {
			return nil, err
		}

		for _, mapping := range mappings {
			claim, err := c.resolve(ctx, workload, held, allocations, mapping)
			if err != nil {
				return nil, err
			}

			claim.Instance = instance
			resolved = append(resolved, claim)
		}
	}

	return resolved, nil
}

// Preview settles every mapping it can without allocating anything, and reports
// whether any mapping still needs a host port. A mapping that does is returned with
// no host port rather than with an invented one.
//
// This is what a caller reporting on an apply it is not performing uses. Allocation
// writes: it reads what is promised, chooses from what is left, and the caller then
// claims it. A preview that allocated would move a workload's address, or consume a
// port, while claiming to change nothing.
//
// The mappings are settled through the same code the real resolution uses, so the two
// cannot disagree about which port a workload keeps or which pinned port is refused.
// A pinned port another workload holds is still an error here, because that is the
// answer the caller asked for.
func (c *Claimer) Preview(ctx context.Context, workload string, existing []Claim, mappings []manifest.Port, count int) ([]Claim, bool, error) {
	if count > 1 && Pinned(mappings) {
		return nil, false, fmt.Errorf("%w: a pinned host port cannot serve %d instances", ErrHostPortTaken, count)
	}

	var pending bool

	claims := make([]Claim, 0, count*len(mappings))

	for instance := range count {
		held := heldBy(existing, instance)

		for _, mapping := range mappings {
			// No allocations, so a mapping that is neither pinned nor already held is
			// settled on nothing.
			claim, err := c.resolve(ctx, workload, held, nil, mapping)
			if err != nil {
				return nil, false, err
			}

			if claim.Host == 0 {
				pending = true
			}

			claim.Instance = instance
			claims = append(claims, claim)
		}
	}

	return claims, pending, nil
}

// heldBy reports the claims one of a workload's instances already holds, keyed by
// the port and protocol each one settles.
func heldBy(existing []Claim, instance int) map[key]Claim {
	held := make(map[key]Claim, len(existing))
	for _, claim := range existing {
		if claim.Instance != instance {
			continue
		}

		held[key{claim.Container, claim.Protocol}] = claim
	}

	return held
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

// Resolved returns spec with every port's host side filled in from the first
// instance's claims.
//
// The resolved ports are part of the specification that gets hashed, which is what
// makes a reallocated port replace the container running on the old one: to the
// reconciler it is simply a specification that has changed. Only the first
// instance's ports are written, because they are the workload's advertised
// address: a later instance's port moving replaces that instance alone, not the
// whole workload.
func Resolved(spec manifest.Spec, claims []Claim) manifest.Spec {
	if len(spec.Ports) == 0 || len(claims) == 0 {
		return spec
	}

	settled := make(map[key]Claim, len(claims))
	for _, claim := range claims {
		if claim.Instance != 0 {
			continue
		}

		settled[key{claim.Container, claim.Protocol}] = claim
	}

	mappings := make([]manifest.Port, 0, len(claims))
	for _, mapping := range spec.Ports {
		protocol := protocolOf(mapping)

		claim, ok := settled[key{mapping.To, protocol}]
		if !ok {
			continue
		}

		mappings = append(mappings, manifest.Port{
			Name:     mapping.Name,
			To:       mapping.To,
			From:     claim.Host,
			Protocol: manifest.Protocol(protocol),
		})
	}

	// The specification is taken by value, so assigning the mappings here replaces
	// only this copy's slice header and leaves the caller's alone.
	spec.Ports = mappings

	return spec
}
