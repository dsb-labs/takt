package wire_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/wire"
)

func TestExecFrameWriter_Write(t *testing.T) {
	t.Parallel()

	t.Run("leads every frame with the stream", func(t *testing.T) {
		var frames [][]byte

		w := wire.ExecFrameWriter{Stream: wire.ExecStderr, Send: func(frame []byte) error {
			frames = append(frames, frame)
			return nil
		}}

		n, err := w.Write([]byte("hello"))
		require.NoError(t, err)

		assert.Equal(t, 5, n)
		assert.Equal(t, [][]byte{[]byte("\x02hello")}, frames)
	})

	t.Run("splits a write larger than a frame", func(t *testing.T) {
		var frames [][]byte

		w := wire.ExecFrameWriter{Stream: wire.ExecStdout, Send: func(frame []byte) error {
			frames = append(frames, frame)
			return nil
		}}

		payload := bytes.Repeat([]byte("a"), wire.ExecFrameSize+1)

		n, err := w.Write(payload)
		require.NoError(t, err)

		assert.Equal(t, len(payload), n)
		require.Len(t, frames, 2)
		assert.Len(t, frames[0], wire.ExecFrameSize+1)
		assert.Equal(t, []byte("\x01a"), frames[1])
	})

	t.Run("reports how much was sent before a failure", func(t *testing.T) {
		w := wire.ExecFrameWriter{Stream: wire.ExecStdin, Send: func([]byte) error {
			return assert.AnError
		}}

		n, err := w.Write([]byte("hello"))
		require.ErrorIs(t, err, assert.AnError)
		assert.Zero(t, n)
	})
}
