package manifest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/orca/pkg/manifest"
)

// TestValidate_Labels pins the label boundaries that are unwieldy as fixtures:
// the exact length caps, the byte counting on values, and the printable
// non-ASCII cases.
func TestValidate_Labels(t *testing.T) {
	t.Parallel()

	spec := func(labels map[string]string) manifest.Spec {
		return manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Count:     1,
			Labels:    labels,
			Container: &manifest.Container{Image: "example/example:latest"},
		}
	}

	t.Run("rejects keys outside the documented shape", func(t *testing.T) {
		t.Parallel()

		for _, key := range []string{
			strings.Repeat("a", 64),
			"",
			"-leading",
			"trailing.",
			"has space",
			"orca.workload",
		} {
			err := manifest.ValidateWorkload(spec(map[string]string{key: "value"}))
			assert.Error(t, err, "accepted the key %q", key)
		}
	})

	t.Run("rejects values outside the documented shape", func(t *testing.T) {
		t.Parallel()

		for _, value := range []string{
			strings.Repeat("v", 257),
			// 129 two-byte runes. The cap counts bytes, so 258 bytes is over
			// the limit even though it reads as 129 characters.
			strings.Repeat("é", 129),
			"a\tb",
			string([]byte{0xff}),
		} {
			err := manifest.ValidateWorkload(spec(map[string]string{"some-key": value}))
			assert.Error(t, err, "accepted the value %q", value)
		}
	})

	t.Run("accepts labels at the boundaries", func(t *testing.T) {
		t.Parallel()

		for name, labels := range map[string]map[string]string{
			"a key at the length cap":       {strings.Repeat("a", 63): "value"},
			"a value at the length cap":     {"some-key": strings.Repeat("v", 256)},
			"the bare key orca":             {"orca": "value"},
			"printable non-ascii in values": {"some-key": "café ☕"},
			"an empty value":                {"some-key": ""},
		} {
			err := manifest.ValidateWorkload(spec(labels))
			assert.NoError(t, err, "rejected %s", name)
		}
	})

	t.Run("accepts exactly the maximum labels", func(t *testing.T) {
		t.Parallel()

		labels := make(map[string]string, 32)
		for i := range 32 {
			labels[fmt.Sprintf("key-%d", i)] = "value"
		}

		assert.NoError(t, manifest.ValidateWorkload(spec(labels)))
	})
}
