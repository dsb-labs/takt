package loadtest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/loadtest"
)

// TestDuration_MarshalJSON covers the reason the type exists. A report is read by
// somebody deciding whether something got slower, and a count of nanoseconds does not
// answer that.
func TestDuration_MarshalJSON(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(loadtest.Duration(1403122 * time.Nanosecond))
	require.NoError(t, err)
	assert.JSONEq(t, `"1.403ms"`, string(encoded))
}

// TestReport_CarriesTheScenario covers a report saying which run produced it. Read on
// its own — out of an artifact, or a week later — a report with no name and no purpose
// is a page of numbers.
func TestReport_CarriesTheScenario(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(loadtest.Report{
		Scenario:    "smoke",
		Description: "A few of everything, in seconds.",
	})
	require.NoError(t, err)

	assert.Contains(t, string(encoded), `"Scenario":"smoke"`)
	assert.Contains(t, string(encoded), `"Description":"A few of everything, in seconds."`)
}

func TestReport_Failed(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name   string
		Report loadtest.Report
		Failed bool
	}{
		{
			Name: "a clean run",
			Report: loadtest.Report{
				Latencies: []loadtest.Latency{{Operation: "workload.get", Count: 10}},
			},
		},
		{
			Name: "an operation that failed",
			Report: loadtest.Report{
				Latencies: []loadtest.Latency{{Operation: "workload.get", Count: 10, Failed: 1}},
			},
			Failed: true,
		},
		{
			Name:   "a workload that never ran",
			Report: loadtest.Report{Unconverged: []string{"example"}},
			Failed: true,
		},
		{
			Name:   "something left behind",
			Report: loadtest.Report{Leaks: []string{"mounts/files holds 3 entries after teardown"}},
			Failed: true,
		},
		{
			// A request the run itself cancelled by ending is not the server
			// refusing anything, and counting it would fail every run that churns.
			Name: "requests the run cancelled",
			Report: loadtest.Report{
				Latencies: []loadtest.Latency{{Operation: "workload.get", Count: 10, Cancelled: 3}},
			},
		},
		{
			// Latency is reported rather than judged. How fast a machine is says
			// nothing about whether the code is right.
			Name: "a slow run",
			Report: loadtest.Report{
				Latencies: []loadtest.Latency{{
					Operation: "workload.get",
					Count:     10,
					P99:       loadtest.Duration(30 * time.Second),
				}},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.Failed, tc.Report.Failed())
		})
	}
}
