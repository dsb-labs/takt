package loadtest_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/loadtest"
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

func TestReport_Summarise(t *testing.T) {
	t.Parallel()

	report := loadtest.Report{
		Scenario:    "example",
		Workloads:   8,
		Applied:     loadtest.Duration(13 * time.Millisecond),
		Converged:   loadtest.Duration(2 * time.Second),
		Running:     7,
		Unconverged: []string{"example-container-0007"},
		Operations:  120,
		Latencies: []loadtest.Latency{{
			Operation: "workload.get",
			Count:     100,
			Failed:    2,
			Reasons:   []string{"server responded with Internal Server Error"},
			Cancelled: 1,
			P50:       loadtest.Duration(500 * time.Microsecond),
		}},
		Leaks: []string{"mounts/files holds 3 entries after teardown"},
	}

	var out bytes.Buffer
	report.Summarise(&out)

	summary := out.String()

	assert.Contains(t, summary, "example: 8 workloads applied in 13ms")
	assert.Contains(t, summary, "workload.get")
	assert.Contains(t, summary, "server responded with Internal Server Error")
	assert.Contains(t, summary, "example-container-0007")
	assert.Contains(t, summary, "mounts/files holds 3 entries after teardown")
}

// TestReport_Summarise_Clean covers the counts being printed when they are zero. Their
// absence would read as the checks not having run, which is the opposite of what a
// clean run means.
func TestReport_Summarise_Clean(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	loadtest.Report{Scenario: "example"}.Summarise(&out)

	assert.Contains(t, out.String(), "unconverged: 0")
	assert.Contains(t, out.String(), "leaks: 0")
}
