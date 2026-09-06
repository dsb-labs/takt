package resolve_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/resolve"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestAddressResolver_Address(t *testing.T) {
	t.Parallel()

	published := []database.Port{
		{WorkloadID: "workload-one", Name: "pg", Container: 5432, Host: 20432, Protocol: "tcp", Dynamic: true},
		{WorkloadID: "workload-one", Name: "metrics", Container: 9090, Host: 20090, Protocol: "tcp", Dynamic: true},
	}

	tt := []struct {
		Name      string
		Reference manifest.Reference
		Ports     []database.Port
		Expected  string
		ExpectErr error
	}{
		{
			Name:      "resolves a reference naming a port",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "pg"},
			Ports:     published,
			Expected:  "10.0.0.5:20432",
		},
		{
			// The number is the other way of writing the same thing.
			Name:      "resolves a reference naming a port by number",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "5432"},
			Ports:     published,
			Expected:  "10.0.0.5:20432",
		},
		{
			Name:      "resolves a reference naming the second port",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "metrics"},
			Ports:     published,
			Expected:  "10.0.0.5:20090",
		},
		{
			// Composing an address whose port the operator already knows should not
			// mean naming that port twice.
			Name:      "resolves a reference naming no port to the host alone",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres"},
			Ports:     published,
			Expected:  "10.0.0.5",
		},
		{
			Name:      "reports a port the workload does not publish",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "http"},
			Ports:     published,
			ExpectErr: resolve.ErrPortNotPublished,
		},
		{
			// A workload publishing nothing is reachable at no address, so the
			// reference could never mean anything — including the form that names no
			// port.
			Name:      "reports a workload publishing no ports",
			Reference: manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres"},
			ExpectErr: resolve.ErrPortNotPublished,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			workloads, ports := NewMockWorkloadLocator(t), NewMockPortLocator(t)

			workloads.EXPECT().Get(mock.Anything, "postgres").
				Return(database.Workload{ID: "workload-one", Name: "postgres"}, nil)
			ports.EXPECT().List(mock.Anything, "workload-one").Return(tc.Ports, nil)

			address, err := newTestAddressResolver(t, workloads, ports).Address(t.Context(), tc.Reference, "reader", 0)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, address)
		})
	}

	t.Run("spreads a reader's instances across the target's", func(t *testing.T) {
		workloads, ports := NewMockWorkloadLocator(t), NewMockPortLocator(t)

		workloads.EXPECT().Get(mock.Anything, "postgres").
			Return(database.Workload{ID: "workload-one", Name: "postgres"}, nil)

		// Three instances publishing the same container port at three host ports.
		ports.EXPECT().List(mock.Anything, "workload-one").Return([]database.Port{
			{WorkloadID: "workload-one", Instance: 0, Name: "pg", Container: 5432, Host: 20432, Protocol: "tcp", Dynamic: true},
			{WorkloadID: "workload-one", Instance: 1, Name: "pg", Container: 5432, Host: 20433, Protocol: "tcp", Dynamic: true},
			{WorkloadID: "workload-one", Instance: 2, Name: "pg", Container: 5432, Host: 20434, Protocol: "tcp", Dynamic: true},
		}, nil)

		svc := newTestAddressResolver(t, workloads, ports)
		reference := manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "pg"}

		// Three instances of one reader land one on each target instance, because
		// the reader's own index offsets the choice.
		seen := make(map[string]struct{}, 3)
		for readerInstance := range 3 {
			address, err := svc.Address(t.Context(), reference, "api", readerInstance)
			require.NoError(t, err)

			seen[address] = struct{}{}
		}

		assert.Len(t, seen, 3, "each reader instance reached a target instance of its own")

		// Deterministic: the same reader resolves the same address every time, so a
		// dry run and an apply cannot disagree.
		first, err := svc.Address(t.Context(), reference, "api", 0)
		require.NoError(t, err)

		again, err := svc.Address(t.Context(), reference, "api", 0)
		require.NoError(t, err)
		assert.Equal(t, first, again)
	})

	t.Run("reports a workload that does not exist", func(t *testing.T) {
		workloads, ports := NewMockWorkloadLocator(t), NewMockPortLocator(t)

		workloads.EXPECT().Get(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound)

		_, err := newTestAddressResolver(t, workloads, ports).
			Address(t.Context(), manifest.Reference{Kind: manifest.KindWorkload, Name: "nope"}, "reader", 0)
		assert.ErrorIs(t, err, database.ErrWorkloadNotFound)
	})
}

func newTestAddressResolver(t *testing.T, workloads resolve.WorkloadLocator, ports resolve.PortLocator) *resolve.AddressResolver {
	t.Helper()

	return resolve.NewAddressResolver(resolve.AddressResolverConfig{
		Logger:    newTestLogger(t),
		Workloads: workloads,
		Ports:     ports,
		Address:   "10.0.0.5",
	})
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
