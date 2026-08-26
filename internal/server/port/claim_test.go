package port_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestClaimer_Resolve(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Held       []port.Claim
		Mappings   []manifest.Port
		SetupMocks func(*MockRepository)
		Assert     func(*testing.T, []port.Claim)
		ExpectErr  error
	}{
		{
			Name:     "allocates a host port for a mapping naming none",
			Mappings: []manifest.Port{{To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, 8080, claims[0].Container)
				assert.Equal(t, port.ProtocolTCP, claims[0].Protocol)
				assert.True(t, claims[0].Dynamic)
				assert.NotZero(t, claims[0].Host)
			},
		},
		{
			// A mapping naming no protocol asks for TCP, which is what every
			// specification stored before the protocol existed described.
			Name:     "defaults a mapping naming no protocol to tcp",
			Mappings: []manifest.Port{{To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, port.ProtocolTCP, claims[0].Protocol)
			},
		},
		{
			// A DNS server answering on 20000/udp and 20014/tcp reads as an accident,
			// so the mappings of one container port are allocated together.
			Name: "gives both protocols of one port the same host number",
			Mappings: []manifest.Port{
				{To: 53, Protocol: manifest.ProtocolTCP},
				{To: 53, Protocol: manifest.ProtocolUDP},
			},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 2)
				assert.Equal(t, claims[0].Host, claims[1].Host)
				assert.Equal(t, port.ProtocolTCP, claims[0].Protocol)
				assert.Equal(t, port.ProtocolUDP, claims[1].Protocol)
			},
		},
		{
			// The two are separate address spaces, so a port taken on one says
			// nothing about the other.
			Name:     "allocates over udp around a port taken over tcp",
			Mappings: []manifest.Port{{To: 53, Protocol: manifest.ProtocolUDP}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).
					Return(map[string][]int{"tcp": {20000}}, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, port.ProtocolUDP, claims[0].Protocol)
			},
		},
		{
			// The workload's address must not move every time something unrelated
			// about it changes, so an allocation it already holds is kept.
			Name:     "keeps the host port a workload already holds",
			Held:     []port.Claim{{Container: 8080, Host: 20005, Protocol: port.ProtocolTCP, Dynamic: true}},
			Mappings: []manifest.Port{{To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, 20005, claims[0].Host)
			},
		},
		{
			// Renaming a port changes what the specification calls it rather than
			// where it is reached.
			Name:     "keeps the host port when the port is renamed",
			Held:     []port.Claim{{Name: "http", Container: 8080, Host: 20005, Protocol: port.ProtocolTCP, Dynamic: true}},
			Mappings: []manifest.Port{{Name: "web", To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, 20005, claims[0].Host)
				assert.Equal(t, "web", claims[0].Name)
			},
		},
		{
			Name:     "uses a pinned host port as given",
			Mappings: []manifest.Port{{To: 8080, From: 4141}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
				ports.EXPECT().HolderOf(mock.Anything, 4141, "tcp").Return("", false, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, 4141, claims[0].Host)
				assert.False(t, claims[0].Dynamic, "a port the caller chose was not allocated here")
			},
		},
		{
			// A pinned port is taken by this specification whatever else it asks for,
			// so allocation must not hand the same number to another mapping.
			Name: "allocates around a port this specification pins",
			Mappings: []manifest.Port{
				{To: 8080, From: 20000},
				{To: 9090},
			},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
				ports.EXPECT().HolderOf(mock.Anything, 20000, "tcp").Return("", false, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 2)
				assert.Equal(t, 20000, claims[0].Host)
				assert.NotEqual(t, 20000, claims[1].Host)
			},
		},
		{
			Name:     "keeps a pinned port the same workload already holds",
			Mappings: []manifest.Port{{To: 8080, From: 4141}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
				ports.EXPECT().HolderOf(mock.Anything, 4141, "tcp").Return("example", true, nil).Once()
			},
			Assert: func(t *testing.T, claims []port.Claim) {
				require.Len(t, claims, 1)
				assert.Equal(t, 4141, claims[0].Host)
			},
		},
		{
			Name:     "refuses a pinned port another workload holds",
			Mappings: []manifest.Port{{To: 8080, From: 4141}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Once()
				ports.EXPECT().HolderOf(mock.Anything, 4141, "tcp").Return("other", true, nil).Once()
			},
			ExpectErr: port.ErrHostPortTaken,
		},
		{
			// A capacity problem rather than a fault or a bad request: the
			// specification becomes servable when a workload is deleted or the range
			// is widened.
			Name:     "reports an exhausted range as no ports available",
			Mappings: []manifest.Port{{To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).
					Return(map[string][]int{"tcp": {20000, 20001, 20002}}, nil).Once()
			},
			ExpectErr: port.ErrNoPortsAvailable,
		},
		{
			Name:     "reports a repository that cannot be read",
			Mappings: []manifest.Port{{To: 8080}},
			SetupMocks: func(ports *MockRepository) {
				ports.EXPECT().Allocated(mock.Anything).Return(nil, errors.New("boom")).Once()
			},
			ExpectErr: nil,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			ports := NewMockRepository(t)
			tc.SetupMocks(ports)

			claimer := port.NewClaimer(port.ClaimerConfig{
				// A range of three, so the exhaustion case does not depend on
				// allocating twenty thousand ports to get there.
				Allocator: port.New(port.Config{Min: 20000, Max: 20002}),
				Ports:     ports,
			})

			claims, err := claimer.Resolve(t.Context(), "example", tc.Held, tc.Mappings)
			if tc.Assert == nil {
				assert.Error(t, err)
				if tc.ExpectErr != nil {
					assert.ErrorIs(t, err, tc.ExpectErr)
				}

				return
			}

			require.NoError(t, err)
			tc.Assert(t, claims)
		})
	}
}

func TestRequested(t *testing.T) {
	t.Parallel()

	t.Run("drops the host port of an allocation orca made", func(t *testing.T) {
		// Resolution then allocates afresh rather than being handed back the
		// allocation it is being asked to reconsider.
		requested := port.Requested(
			[]manifest.Port{{Name: "http", To: 8080, From: 20005}},
			[]port.Claim{{Container: 8080, Host: 20005, Protocol: port.ProtocolTCP, Dynamic: true}},
		)

		require.Len(t, requested, 1)
		assert.Zero(t, requested[0].From)
		assert.Equal(t, "http", requested[0].Name)
	})

	t.Run("keeps a host port the caller pinned", func(t *testing.T) {
		requested := port.Requested(
			[]manifest.Port{{To: 8080, From: 4141}},
			[]port.Claim{{Container: 8080, Host: 4141, Protocol: port.ProtocolTCP}},
		)

		require.Len(t, requested, 1)
		assert.Equal(t, 4141, requested[0].From)
	})
}

func TestPinned(t *testing.T) {
	t.Parallel()

	assert.False(t, port.Pinned([]manifest.Port{{To: 8080}}))
	assert.True(t, port.Pinned([]manifest.Port{{To: 8080}, {To: 9090, From: 4141}}))
}
