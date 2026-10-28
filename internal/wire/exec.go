package wire

// The streams a binary frame on an exec connection may carry. Every binary frame
// opens with one of these bytes, and the rest of the frame is that stream's bytes.
//
// One connection carries three streams in two directions, and the websocket itself
// distinguishes only text from binary. The leading byte is what tells the command's
// standard output from its standard error on the way out, as docker's own
// multiplexing does, and names the operator's input on the way in.
const (
	// ExecStdin marks a frame carrying the operator's input, sent by the client.
	ExecStdin byte = 0
	// ExecStdout marks a frame carrying the command's standard output, sent by the
	// server.
	ExecStdout byte = 1
	// ExecStderr marks a frame carrying the command's standard error, sent by the
	// server.
	ExecStderr byte = 2
)

// ExecFrameSize is the most bytes of a stream one binary frame carries after its
// leading byte. With the byte, a frame is exactly the size a websocket library
// accepts before anyone raises its limit, so neither side has to.
const ExecFrameSize = 32*1024 - 1

type (
	// The ExecMessageType type names what a text frame on an exec connection says.
	ExecMessageType string

	// The ExecMessage type is a text frame on an exec connection, which is how the
	// two sides say something about the command rather than carrying its bytes.
	//
	// The client sends ExecStart once, first, and ExecCloseStdin once when the
	// operator's input has ended. The server sends ExecExit or ExecError once,
	// last, and then closes the connection.
	ExecMessage struct {
		// What the message says.
		Type ExecMessageType `json:"type"`
		// The command to run, with its arguments. Set on ExecStart.
		Command []string `json:"command,omitempty"`
		// Whether the client will send the operator's input. Set on ExecStart. A
		// command not given input sees one that has ended from the start.
		Stdin bool `json:"stdin,omitempty"`
		// The status the command exited with. Set on ExecExit. A pointer because
		// zero is a status, and has to be told apart from the field being unset.
		ExitCode *int `json:"exitCode,omitempty"`
		// Why the command did not run, or did not finish. Set on ExecError.
		Error string `json:"error,omitempty"`
	}

	// The ExecFrameWriter type frames what is written to it as binary frames on one
	// stream, each no larger than ExecFrameSize, through the function it is given.
	//
	// It is an io.Writer so a command's output can be copied straight into it, and
	// so the operator's input can be copied straight out through one.
	ExecFrameWriter struct {
		// The stream the frames carry.
		Stream byte
		// Sends one framed message.
		Send func(frame []byte) error
	}
)

const (
	// ExecStart asks the server to run the command the message names.
	ExecStart ExecMessageType = "start"
	// ExecCloseStdin says the operator's input has ended.
	ExecCloseStdin ExecMessageType = "closeStdin"
	// ExecExit says the command ended, and with which status.
	ExecExit ExecMessageType = "exit"
	// ExecError says the command did not run, or did not finish, and why.
	ExecError ExecMessageType = "error"
)

// Write frames p as one or more binary frames on the writer's stream.
func (w ExecFrameWriter) Write(p []byte) (int, error) {
	for written := 0; written < len(p); {
		chunk := p[written:min(written+ExecFrameSize, len(p))]

		frame := make([]byte, 0, len(chunk)+1)
		frame = append(frame, w.Stream)
		frame = append(frame, chunk...)

		if err := w.Send(frame); err != nil {
			return written, err
		}

		written += len(chunk)
	}

	return len(p), nil
}
