package cli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/pkg/cli"
)

func TestFromContext(t *testing.T) {
	t.Parallel()

	t.Run("returns the settings the context carries", func(t *testing.T) {
		settings := cli.Config{Address: "https://takt.example", Token: "token"}

		ctx := cli.NewContext(t.Context(), settings)
		assert.Equal(t, settings, cli.FromContext(ctx))
	})

	t.Run("returns the zero settings when the context carries none", func(t *testing.T) {
		assert.Zero(t, cli.FromContext(t.Context()))
	})
}
