package event_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/event"
)

func TestMessage(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Reason   event.Reason
		Fields   event.Fields
		Expected string
	}{
		{
			Name:     "names the image a pull is fetching",
			Reason:   event.ImagePulling,
			Fields:   event.Fields{Reference: "alpine:3"},
			Expected: "Pulling image alpine:3",
		},
		{
			Name:     "carries the reason a reference could not be resolved",
			Reason:   event.ReferenceUnresolved,
			Fields:   event.Fields{Reference: "api", Error: "no address"},
			Expected: "Waiting for api to resolve: no address",
		},
		{
			Name:     "describes a reference the event could not name",
			Reason:   event.ReferenceUnresolved,
			Fields:   event.Fields{Error: "no address"},
			Expected: "Waiting for a reference to resolve: no address",
		},
		{
			Name:     "renders a backoff delay as a duration",
			Reason:   event.RestartPaced,
			Fields:   event.Fields{Delay: 90 * time.Second, Count: 4},
			Expected: "Waiting 1m30s before restart 4",
		},
		{
			Name:     "says which hash a replacement is moving to",
			Reason:   event.HashMoved,
			Fields:   event.Fields{Instance: 2, Hash: "abc123"},
			Expected: "Replacing instance 2, the specification hash moved to abc123",
		},
		{
			Name:     "counts consecutive health check failures",
			Reason:   event.HealthCheckFailing,
			Fields:   event.Fields{Count: 3, Error: "connection refused"},
			Expected: "Health check failed 3 times in a row: connection refused",
		},
		{
			Name:     "takes no fields where the reason says everything",
			Reason:   event.Suspended,
			Expected: "Workload suspended",
		},
		{
			Name:     "lists the host ports an instance gave up",
			Reason:   event.PortsAbandoned,
			Fields:   event.Fields{Ports: []int{20001, 20002}},
			Expected: "Gave up host ports 20001, 20002",
		},
		{
			Name:     "keeps the singular for one abandoned port",
			Reason:   event.PortsAbandoned,
			Fields:   event.Fields{Ports: []int{20001}},
			Expected: "Gave up host port 20001",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.Expected, event.Message(tc.Reason, event.Encode(tc.Fields)))
		})
	}

	t.Run("reports a clean exit apart from an unrecorded one", func(t *testing.T) {
		t.Parallel()

		// Zero is the status a clean exit reports, so it has to survive the
		// encoding rather than read as the field never having been set.
		clean := 0
		assert.Equal(t, "Instance 0 exited with status 0",
			event.Message(event.InstanceExited, event.Encode(event.Fields{ExitCode: &clean})))

		assert.Equal(t, "Instance 0 exited with status unknown",
			event.Message(event.InstanceExited, event.Encode(event.Fields{})))
	})

	t.Run("renders a reason it does not know as itself", func(t *testing.T) {
		t.Parallel()

		// An event written by a newer server reads as its reason rather than as
		// nothing, which is what an operator running a mixed pair sees.
		assert.Equal(t, "somethingNewer", event.Message("somethingNewer", []byte(`{}`)))
	})

	t.Run("renders the reason alone when the data cannot be read", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "imagePulling", event.Message(event.ImagePulling, []byte(`not json`)))
	})

	t.Run("renders a reason carrying no data", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "Specification applied", event.Message(event.Applied, nil))
	})
}

func TestEncode(t *testing.T) {
	t.Parallel()

	t.Run("omits the fields a reason does not use", func(t *testing.T) {
		t.Parallel()

		assert.JSONEq(t, `{"reference":"alpine:3"}`, string(event.Encode(event.Fields{Reference: "alpine:3"})))
	})

	t.Run("encodes the same fields the same way every time", func(t *testing.T) {
		t.Parallel()

		// Events coalesce on their encoded data, so two sightings of one thing have
		// to produce the same bytes or they store as two rows.
		fields := event.Fields{Reference: "alpine:3", Instance: 1, Ports: []int{20001}}

		first := event.Encode(fields)
		for range 10 {
			require.Equal(t, string(first), string(event.Encode(fields)))
		}
	})
}
