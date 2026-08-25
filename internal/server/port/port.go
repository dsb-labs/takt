// Package port provides allocation of host ports to workloads.
//
// Allocation lives above the driver boundary rather than inside a driver. A host
// port that orca chose is a decision it can record, report and keep stable, which is
// what makes it usable as an address; leaving the choice to the runtime would mean
// only discovering the address afterwards, and would have to be reimplemented by
// every driver whose runtime has no allocator of its own.
package port

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"

	"go.opentelemetry.io/otel/metric"
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
// The search starts at a random point in the range and wraps, rather than scanning
// from the bottom every time. Scanning from the bottom made concurrent allocations
// contend maximally: several callers reading the same set of taken ports would all
// choose the same lowest free one, and all but one would lose the race to claim it.
// Starting at different points means they rarely pick the same port at all.
//
// Returns ErrRangeExhausted when nothing in the range is available.
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

	size := a.max - a.min + 1
	offset := rand.IntN(size)

	for i := range size {
		candidate := a.min + (offset+i)%size

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

// RegisterMetrics registers gauges describing the allocator's pool: the number of
// host ports in the configured range, and the number currently allocated.
//
// The allocated function is asked once per scrape rather than once per pass, so
// its cost lands on the reader — for the repository behind it, one read of the
// allocations it already records.
func (a *Allocator) RegisterMetrics(meter metric.Meter, allocated func(ctx context.Context) ([]int, error)) error {
	capacity, err := meter.Int64ObservableGauge("orca.ports.capacity",
		metric.WithDescription("The number of host ports in the configured range."),
		metric.WithUnit("{port}"))
	if err != nil {
		return fmt.Errorf("failed to build the port capacity gauge: %w", err)
	}

	used, err := meter.Int64ObservableGauge("orca.ports.used",
		metric.WithDescription("The number of host ports currently allocated to workloads."),
		metric.WithUnit("{port}"))
	if err != nil {
		return fmt.Errorf("failed to build the port usage gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		observer.ObserveInt64(capacity, int64(a.max-a.min+1))

		ports, err := allocated(ctx)
		if err != nil {
			return fmt.Errorf("failed to count allocated ports: %w", err)
		}

		observer.ObserveInt64(used, int64(len(ports)))

		return nil
	}, capacity, used)
	if err != nil {
		return fmt.Errorf("failed to register the port gauges: %w", err)
	}

	return nil
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
