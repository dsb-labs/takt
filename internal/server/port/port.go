// Package port provides allocation of host ports to workloads, and settles a
// workload's ports on the ones it is reached at.
//
// Allocation lives above the driver boundary rather than inside a driver. A host
// port that takt chose is a decision it can record, report and keep stable, which is
// what makes it usable as an address. Leaving the choice to the runtime would mean
// only discovering the address afterwards, and would have to be reimplemented by
// every driver whose runtime has no allocator of its own.
//
// Claiming sits here rather than in the service that writes the result. A pinned port
// and an allocated one are settled by the same rules, and those rules are about ports
// rather than about workloads: which address spaces are separate, which allocation a
// workload keeps, and which mappings have to land on the same number.
package port

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"

	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

// The name this package's instruments are recorded under, which describes the code
// declaring them rather than whatever assembles the server.
const scope = "github.com/dsb-labs/takt/internal/server/port"

var (
	// ErrRangeExhausted is returned when no port in the configured range is free.
	ErrRangeExhausted = errors.New("no free port in range")
)

type (
	// The Protocol type names the transport protocol a host port is taken on.
	//
	// The two are separate address spaces, so an allocation is only ever a claim on
	// one of them: 20000/tcp says nothing about whether 20000/udp is free.
	Protocol string

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
		// The provider the pool gauges are recorded against. May be nil, in which
		// case nothing is recorded.
		MeterProvider metric.MeterProvider
		// Reports every host port allocated to any workload, keyed by the protocol
		// it is allocated on. Read once per scrape to report how full the range is.
		// May be nil, in which case usage is not reported.
		Allocated func(ctx context.Context) (map[string][]int, error)
	}
)

const (
	// ProtocolTCP is the protocol a port is published on unless the workload asks
	// for the other.
	ProtocolTCP Protocol = "tcp"
	// ProtocolUDP is the other address space, which a workload speaking DNS,
	// WireGuard, syslog, NTP or a game protocol publishes on.
	ProtocolUDP Protocol = "udp"
)

const (
	// DefaultMin is the lowest port allocated when none is configured.
	DefaultMin = 20000
	// DefaultMax is the highest port allocated when none is configured.
	//
	// The default range sits below the ephemeral ports the kernel and docker hand
	// out for themselves, so takt's allocations don't collide with a port the
	// system was about to use for something else.
	DefaultMax = 32000
)

// New returns an Allocator that hands out ports from the range in config.
func New(config Config) *Allocator {
	allocator := &Allocator{min: config.Min, max: config.Max}

	// Registered here rather than by the caller. Gauges describing the pool are the
	// allocator's own business, and a caller that had to remember a second call
	// after constructing one could forget it — as it could get the reader's shape
	// wrong, which is how the usage gauge came to count protocols.
	//
	// A failure costs the metrics rather than the allocator, and is reported through
	// the OpenTelemetry error handler like every other refused instrument.
	if config.Allocated != nil {
		allocator.registerMetrics(telemetry.Meter(config.MeterProvider, scope), config.Allocated)
	}

	return allocator
}

// Allocate returns a host port that is free on every protocol named, avoiding both
// the ports already taken on those protocols and any port something on the host is
// already listening on.
//
// More than one protocol is asked for when a workload publishes the same port over
// both, which DNS does. The two allocations are independent as far as the address
// spaces are concerned, but landing them on one host port is what makes 53/tcp and
// 53/udp reachable at the same number rather than at two unrelated ones.
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
// nothing outside takt is holding it in the meantime. The check makes that race
// unlikely rather than impossible, and the caller is expected to cope with a bind
// that fails anyway.
func (a *Allocator) Allocate(protocols []Protocol, taken map[Protocol][]int) (int, error) {
	claimed := make(map[Protocol]map[int]struct{}, len(protocols))
	for _, protocol := range protocols {
		claimed[protocol] = make(map[int]struct{}, len(taken[protocol]))
		for _, port := range taken[protocol] {
			claimed[protocol][port] = struct{}{}
		}
	}

	size := a.max - a.min + 1
	offset := rand.IntN(size)

	for i := range size {
		candidate := a.min + (offset+i)%size

		if usable(protocols, claimed, candidate) {
			return candidate, nil
		}
	}

	return 0, fmt.Errorf("%w: %d-%d", ErrRangeExhausted, a.min, a.max)
}

// usable reports whether a candidate port is free on every protocol asked for, both
// as far as takt's own allocations go and on the host itself.
func usable(protocols []Protocol, claimed map[Protocol]map[int]struct{}, candidate int) bool {
	for _, protocol := range protocols {
		if _, ok := claimed[protocol][candidate]; ok {
			return false
		}

		if !Available(protocol, candidate) {
			return false
		}
	}

	return true
}

// Available reports whether nothing on the host is currently listening on the given
// port over the given protocol.
//
// The check is a real bind rather than a lookup, because that is the only thing that
// accounts for every listener: processes takt knows nothing about, containers other
// tooling started, and sockets held by the system. The port is released immediately,
// so this establishes that the port was free a moment ago rather than reserving it.
//
// Each protocol is probed on its own socket type. Probing UDP with a TCP listen would
// report a free port as taken and a taken one as free, since the two spaces are
// unrelated.
func Available(protocol Protocol, port int) bool {
	address := net.JoinHostPort("", strconv.Itoa(port))

	if protocol == ProtocolUDP {
		conn, err := net.ListenPacket("udp", address)
		if err != nil {
			return false
		}

		return conn.Close() == nil
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}

	return listener.Close() == nil
}
