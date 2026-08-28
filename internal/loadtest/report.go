package loadtest

import (
	"cmp"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

type (
	// The Duration type is a duration that reports itself as the string a reader
	// thinks in rather than as a count of nanoseconds.
	//
	// A report is read by a person deciding whether something got slower, and
	// "1.4ms" answers that where 1403122 does not.
	Duration time.Duration

	// The Report type is what a run produces: how long each operation took, what
	// failed, and what was left behind.
	Report struct {
		// The scenario that produced the report, and what it says it is for. Both
		// are here so that a report read on its own says which run produced it and
		// what that run was trying to do.
		Scenario    string
		Description string
		// How many workloads it applied.
		Workloads int
		// How long applying the whole fleet took.
		Applied Duration
		// How long the fleet took to converge after that, and how many did.
		Converged   Duration
		Running     int
		Unconverged []string
		// How many operations the churn performed.
		Operations int
		// One entry per kind of operation, named and ordered.
		Latencies []Latency
		// What the run left behind, when it was given somewhere to look.
		Leaks []string
	}

	// The Latency type summarises one kind of operation.
	Latency struct {
		// What the operation is called.
		Operation string
		// How many were performed.
		Count int
		// How many failed, and the distinct reasons they gave.
		Failed  int
		Reasons []string
		// How many the run itself cancelled by ending, which is not a failure of
		// the server. A request in flight when the churn window closes is stopped
		// by the harness, and counting it as a failure would bury the ones that are.
		Cancelled int
		// The distribution, in the units a reader thinks in.
		P50 Duration
		P95 Duration
		P99 Duration
		Max Duration
	}
)

// MarshalJSON writes the duration as a string, rounded to the precision anything
// measured over a network is meaningful to.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).Round(time.Microsecond).String())
}

// String returns the duration as it is written in a report.
func (d Duration) String() string {
	return time.Duration(d).Round(time.Microsecond).String()
}

// Failed reports whether the run found anything wrong: a request the server refused,
// a workload that never ran, or something left behind.
//
// Latency is deliberately not part of this. How fast a machine is says nothing about
// whether the code is right, and a threshold written into a scenario would fail on
// somebody else's laptop rather than reporting a regression.
func (r Report) Failed() bool {
	if len(r.Unconverged) > 0 || len(r.Leaks) > 0 {
		return true
	}

	for _, latency := range r.Latencies {
		if latency.Failed > 0 {
			return true
		}
	}

	return false
}

// The samples type collects durations and failures for one kind of operation, from
// however many workers are running it.
type samples struct {
	mu        sync.Mutex
	durations []time.Duration
	reasons   map[string]int
	cancelled int
}

// The collector type holds the samples of every operation a run performed.
type collector struct {
	mu sync.Mutex
	by map[string]*samples
}

func newCollector() *collector {
	return &collector{by: make(map[string]*samples)}
}

// measure runs fn, recording how long it took and how it ended under the given name.
func (c *collector) measure(name string, fn func() error) error {
	started := time.Now()
	err := fn()

	c.samples(name).record(time.Since(started), err)

	return err
}

func (c *collector) samples(name string) *samples {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.by[name] == nil {
		c.by[name] = &samples{reasons: make(map[string]int)}
	}

	return c.by[name]
}

// report summarises everything collected, ordered by operation so that two runs of
// one scenario print their lines in the same order.
func (c *collector) report() ([]Latency, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	latencies := make([]Latency, 0, len(c.by))
	total := 0

	for name, s := range c.by {
		latency := s.summarise(name)
		latencies = append(latencies, latency)
		total += latency.Count
	}

	slices.SortFunc(latencies, func(a, b Latency) int {
		return cmp.Compare(a.Operation, b.Operation)
	})

	return latencies, total
}

func (s *samples) record(took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.durations = append(s.durations, took)

	if err == nil {
		return
	}

	// A request the run cancelled by ending is not a failure of the server.
	if cancelled(err) {
		s.cancelled++

		return
	}

	s.reasons[truncate(err.Error())]++
}

func (s *samples) summarise(name string) Latency {
	s.mu.Lock()
	defer s.mu.Unlock()

	latency := Latency{Operation: name, Count: len(s.durations), Cancelled: s.cancelled}

	for reason, count := range s.reasons {
		latency.Failed += count
		latency.Reasons = append(latency.Reasons, reason)
	}

	slices.Sort(latency.Reasons)

	if len(s.durations) == 0 {
		return latency
	}

	slices.Sort(s.durations)

	latency.P50 = Duration(quantile(s.durations, 0.50))
	latency.P95 = Duration(quantile(s.durations, 0.95))
	latency.P99 = Duration(quantile(s.durations, 0.99))
	latency.Max = Duration(s.durations[len(s.durations)-1])

	return latency
}

// quantile returns the sample at the given point of a sorted set.
func quantile(sorted []time.Duration, q float64) time.Duration {
	i := int(float64(len(sorted)) * q)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}

	return sorted[i]
}

// truncate shortens a failure so that a report is readable when one operation failed
// thousands of times with a long message.
func truncate(reason string) string {
	const limit = 160

	if len(reason) <= limit {
		return reason
	}

	return reason[:limit]
}
