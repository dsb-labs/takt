package service

import (
	"context"
	"sync"
	"time"

	"github.com/docker/go-units"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Usage type describes what one instance is consuming, beside the limits
	// its specification asked for.
	//
	// The pair travels together because neither half answers the question on its
	// own: an operator wants to know whether a workload is about to be stopped for
	// exceeding a limit, and that is what a figure means against the limit it
	// answers to. A limit of zero is no limit, which is what an unlimited workload
	// asked for.
	Usage struct {
		// The memory the instance is using, in bytes.
		Memory uint64
		// The memory the instance may use, in bytes.
		MemoryLimit uint64
		// The processors the instance is using, as the average since the previous
		// reading. Nil until there are two readings to compute it from, since a
		// rate needs a pair and the first of them has no partner.
		CPU *float64
		// The processors the instance may use.
		CPULimit float64
		// The processes and threads the instance is running.
		Pids int
		// The processes and threads the instance may run.
		PidsLimit int
	}

	// The usageSamples type remembers the last usage reading of each instance, so
	// that the next one can be turned into a rate.
	//
	// Held by the service rather than by a driver, because a rate needs two
	// readings and a driver is asked for one. It is memory only: a rate spanning a
	// restart would be computed against a counter that reset with it.
	usageSamples struct {
		mux     sync.Mutex
		samples map[string]usageSample
	}

	// The usageSample type is one remembered reading, which is the processor time
	// an instance had consumed and when the runtime said so.
	usageSample struct {
		cpu time.Duration
		at  time.Time
	}
)

// How old the previous reading of an instance may be and still be worth computing a
// processor rate against.
//
// A rate is the average between two readings, so a partner from long ago describes a
// window that has since passed rather than what the instance is doing now. Generous
// against the interval a view polls at, so a caller that pauses briefly still gets a
// rate rather than a gap.
const usageSampleTTL = time.Minute

// usage asks the workload's driver what each of its instances is consuming, and
// writes each reading onto the instance it describes, beside the limit the
// specification asked for.
//
// The limits come from the specification rather than from the runtime, because a
// runtime reports the host's capacity as the limit of a workload that asked for
// none: an operator reading "212M of 31G" learns nothing about a workload that is
// not limited, where no limit at all says exactly what is true.
//
// Asked for here rather than while observing, because observing happens on every
// apply, delete, stop, start and restart, and a reading costs a call to the
// runtime per instance. Only a caller that will show the figures pays for them.
//
// Failures are reported rather than returned, for the reason an observation's are:
// a workload is still worth reading when the runtime cannot say what it is using.
func (s *WorkloadService) usage(ctx context.Context, row database.Workload, spec manifest.Spec, instances []Instance) {
	runtime, ok := s.drivers[row.Runtime]
	if !ok || len(instances) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	readings, err := runtime.Usage(ctx, row.ID, row.Name)
	if err != nil {
		s.logger.With("error", err, "runtime", runtime.Name(), "workload", row.Name).
			Error("failed to read instance usage")

		return
	}

	limits := usageLimits(spec.Resources)

	for i := range instances {
		reading, ok := readings[instances[i].ID]
		if !ok {
			continue
		}

		usage := limits
		usage.Memory = reading.Memory
		usage.Pids = reading.Pids
		usage.CPU = s.samples.rate(row.Name+"/"+instances[i].ID, reading)

		instances[i].Usage = usage
	}
}

// usageLimits reports the limits a specification asked for, as the figures a
// reading is compared against. A limit the specification does not name is left at
// zero, which reads as unlimited in the way the runtimes read it.
//
// The memory size was proved to parse when the specification was accepted, so one
// that does not now means the stored specification and the rules have diverged.
// Nothing useful can be reported for it, and a limit reported wrongly would be
// worse than none.
func usageLimits(resources *manifest.Resources) Usage {
	if resources == nil {
		return Usage{}
	}

	var limits Usage

	if resources.Memory != "" {
		if memory, err := units.RAMInBytes(resources.Memory); err == nil {
			limits.MemoryLimit = uint64(memory)
		}
	}

	limits.CPULimit = resources.CPU
	limits.PidsLimit = resources.Pids

	return limits
}

// newUsageSamples returns an empty usageSamples.
func newUsageSamples() *usageSamples {
	return &usageSamples{samples: make(map[string]usageSample)}
}

// rate reports the processors an instance is using, from the reading just taken and
// the one before it, and remembers this reading for the next call.
//
// Nil until there is a pair to compute from, which the first reading of an instance
// never is, and again when the reading before it is old enough that the average
// between them would describe a window nobody asked about. A caller shows nothing
// rather than a rate it cannot interpret, and the reading after this one has a
// recent partner to work from.
func (u *usageSamples) rate(key string, reading driver.Usage) *float64 {
	u.mux.Lock()
	defer u.mux.Unlock()

	previous, ok := u.samples[key]

	u.samples[key] = usageSample{cpu: reading.CPU, at: reading.At}

	// Kept here rather than on a timer: the map only grows when a workload is
	// read, so the read that grows it is the one that can afford to tidy it. An
	// instance that has gone is never asked about again, and what it left behind
	// would otherwise be held for the life of the server.
	for name, sample := range u.samples {
		if reading.At.Sub(sample.at) > usageSampleTTL {
			delete(u.samples, name)
		}
	}

	if !ok {
		return nil
	}

	elapsed := reading.At.Sub(previous.at)
	if elapsed <= 0 || elapsed > usageSampleTTL || reading.CPU < previous.cpu {
		return nil
	}

	rate := float64(reading.CPU-previous.cpu) / float64(elapsed)

	return &rate
}
