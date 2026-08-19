// Package port provides allocation of host ports to workloads.
//
// Allocation lives above the driver boundary rather than inside a driver. A host
// port that orca chose is a decision it can record, report and keep stable, which is
// what makes it usable as an address; leaving the choice to the runtime would mean
// only discovering the address afterwards, and would have to be reimplemented by
// every driver whose runtime has no allocator of its own.
package port

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

var (
	// ErrRangeExhausted is returned when no port in the configured range is free.
	ErrRangeExhausted = errors.New("no free port in range")
)

type (
	// The Allocator type hands out host ports from a configured range.
	Allocator struct {
		min, max int
	}

	// The Config type contains fields used to construct an Allocator.
	Config struct {
		// The lowest host port that may be allocated.
		Min int
		// The highest host port that may be allocated.
		Max int
	}
)

const (
	// DefaultMin is the lowest port allocated when none is configured.
	DefaultMin = 20000
	// DefaultMax is the highest port allocated when none is configured.
	//
	// The default range sits below the ephemeral ports the kernel and docker hand
	// out for themselves, so orca's allocations don't collide with a port the
	// system was about to use for something else.
	DefaultMax = 32000
)

// New returns an Allocator that hands out ports from the range in config.
func New(config Config) *Allocator {
	return &Allocator{min: config.Min, max: config.Max}
}

// Allocate returns a free host port, avoiding both the ports in taken and any port
// something on the host is already listening on.
//
// Ports are offered in ascending order so that a given set of workloads tends to
// receive the same ports across restarts, which makes them predictable enough to
// write down. Returns ErrRangeExhausted when nothing in the range is available.
//
// A port that is free here can still be taken by the time a runtime binds it, since
// nothing outside orca is holding it in the meantime. The check makes that race
// unlikely rather than impossible, and the caller is expected to cope with a bind
// that fails anyway.
func (a *Allocator) Allocate(taken []int) (int, error) {
	claimed := make(map[int]struct{}, len(taken))
	for _, port := range taken {
		claimed[port] = struct{}{}
	}

	for candidate := a.min; candidate <= a.max; candidate++ {
		if _, ok := claimed[candidate]; ok {
			continue
		}

		if !Available(candidate) {
			continue
		}

		return candidate, nil
	}

	return 0, fmt.Errorf("%w: %d-%d", ErrRangeExhausted, a.min, a.max)
}

// Available reports whether nothing on the host is currently listening on the given
// port.
//
// The check is a real bind rather than a lookup, because that is the only thing that
// accounts for every listener: processes orca knows nothing about, containers other
// tooling started, and sockets held by the system. The port is released immediately,
// so this establishes that the port was free a moment ago rather than reserving it.
func Available(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return false
	}

	return listener.Close() == nil
}
