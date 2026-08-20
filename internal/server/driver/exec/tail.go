package exec

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// How much of the end of a file is read to find the last lines of it. A log line is
// short, so this covers a generous tail while bounding what a request costs: the
// caller names how many lines it wants, and the file may be arbitrarily large.
const tailWindow = 1 << 20

// tailFile writes the last lines of a file, or nothing when it does not exist.
//
// The end of the file is read rather than the whole of it, so the cost of a request is
// set by what was asked for rather than by how long the workload has been running. A
// file larger than the window yields its last lines, which is what a tail means.
func tailFile(out io.Writer, path string, lines int) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A workload that has produced no output yet, or one this driver does not
			// run. Neither is a failure.
			return nil
		}

		return fmt.Errorf("failed to open workload output: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to measure workload output: %w", err)
	}

	size := info.Size()
	offset := max(size-tailWindow, 0)

	window := make([]byte, size-offset)
	// ReadAt reports the end of the file when it reads fewer bytes than asked for,
	// which happens when the file is truncated between measuring and reading it.
	if _, err = file.ReadAt(window, offset); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to read workload output: %w", err)
	}

	if _, err = out.Write(lastLines(window, lines)); err != nil {
		return fmt.Errorf("failed to write workload output: %w", err)
	}

	return nil
}

// lastLines returns the final lines of a buffer.
//
// A trailing newline is not a line of its own, so it is stepped over before counting
// backwards. Anything shorter than the count asked for is returned whole.
func lastLines(data []byte, lines int) []byte {
	if len(data) == 0 || lines <= 0 {
		return nil
	}

	end := len(data)
	if data[end-1] == '\n' {
		end--
	}

	count := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] != '\n' {
			continue
		}

		count++
		if count == lines {
			return data[i+1:]
		}
	}

	return data
}
