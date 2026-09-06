package client_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/client"
)

func TestFromContext(t *testing.T) {
	t.Parallel()

	t.Run("returns the client the context carries", func(t *testing.T) {
		c, err := client.New("http://localhost:7373")
		require.NoError(t, err)

		ctx := client.NewContext(t.Context(), c)
		assert.Same(t, c, client.FromContext(ctx))
	})

	t.Run("returns nil when the context carries none", func(t *testing.T) {
		assert.Nil(t, client.FromContext(t.Context()))
	})
}
