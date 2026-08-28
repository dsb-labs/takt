package loadtest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
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
		// The scenario that produced the report.
		Scenario string
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

// Summarise writes the report as a table, for the person watching rather than for
// whatever is parsing the JSON.
//
// The two go to different streams, so a run can be read and piped at the same time.
func (r Report) Summarise(w io.Writer) {
	fmt.Fprintf(w, "\n%s: %d workloads applied in %s, %d running after %s\n",
		r.Scenario, r.Workloads, r.Applied, r.Running, r.Converged)

	if r.Operations > 0 {
		fmt.Fprintf(w, "%d operations\n", r.Operations)
	}

	fmt.Fprintln(w)

	for _, latency := range r.Latencies {
		fmt.Fprintf(w, "  %-26s n=%-7d fail=%-6d cancel=%-6d p50=%-10s p95=%-10s p99=%-10s max=%s\n",
			latency.Operation, latency.Count, latency.Failed, latency.Cancelled,
			latency.P50, latency.P95, latency.P99, latency.Max)

		for _, reason := range latency.Reasons {
			fmt.Fprintf(w, "      %s\n", reason)
		}
	}

	// Printed even when empty, because "nothing was left behind" is the answer the
	// check exists to give and its absence would read as the check not having run.
	fmt.Fprintf(w, "\nunconverged: %d\n", len(r.Unconverged))
	for _, name := range r.Unconverged {
		fmt.Fprintf(w, "  %s\n", name)
	}

	fmt.Fprintf(w, "leaks: %d\n", len(r.Leaks))
	for _, leak := range r.Leaks {
		fmt.Fprintf(w, "  %s\n", leak)
	}
}
