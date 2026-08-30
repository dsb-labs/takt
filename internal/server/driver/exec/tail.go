//go:build linux

package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// How much of the end of a file is read to find the last lines of it. A log line is
// short, so this covers a generous tail while bounding what a request costs: the
// caller names how many lines it wants, and the file may be arbitrarily large.
const tailWindow = 1 << 20

// How long the driver waits between looks at a file it is following.
//
// The runtime offers nothing to subscribe to, so a follow is a poll, and this is what
// it costs against how soon a line arrives. A quarter of a second reads as immediate to
// somebody watching a workload start, and a stat four times a second is nothing next to
// the work the workload itself is doing.
const followInterval = 250 * time.Millisecond

// tailFile writes the last lines of a file, or nothing when it does not exist. It
// returns the offset it read up to, which is where a follow of the same file carries
// on from.
//
// The end of the file is read rather than the whole of it, so the cost of a request is
// set by what was asked for rather than by how long the workload has been running. A
// file larger than the window yields its last lines, which is what a tail means.
func tailFile(out io.Writer, path string, lines int) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A workload that has produced no output yet, or one this driver does not
			// run. Neither is a failure. A follow starts from the beginning of the file
			// the workload has yet to write.
			return 0, nil
		}

		return 0, fmt.Errorf("failed to open workload output: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to measure workload output: %w", err)
	}

	size := info.Size()
	offset := max(size-tailWindow, 0)

	window := make([]byte, size-offset)
	// ReadAt reports the end of the file when it reads fewer bytes than asked for,
	// which happens when the file is truncated between measuring and reading it.
	if _, err = file.ReadAt(window, offset); err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("failed to read workload output: %w", err)
	}

	if _, err = out.Write(lastLines(window, lines)); err != nil {
		return 0, fmt.Errorf("failed to write workload output: %w", err)
	}

	return size, nil
}

// followFile writes a file's output as it is appended, starting from offset, until the
// instance writing it has ended or the caller goes away.
//
// The ended function is asked before the file is read, not after. A process can exit
// between the two, and asking first means the lines it wrote on the way out are still
// returned. Asking afterwards would drop them, which is the opposite of what somebody
// watching a workload fail needs.
//
// A cancelled follow returns no error. The caller who stopped listening is the ordinary
// way for this to end, and there is nobody left to report a failure to.
func followFile(ctx context.Context, out io.Writer, path string, offset int64, ended func() bool) error {
	for {
		done := ended()

		next, err := copyFrom(out, path, offset)
		if err != nil {
			return err
		}

		offset = next

		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followInterval):
		}
	}
}

// copyFrom writes what a file holds beyond offset, returning the offset it reached.
//
// A file shorter than the offset has been replaced rather than appended to: stopping a
// workload moves its output aside, and the attempt that follows opens a file of its
// own. Reading it from the beginning is what a follow of the workload means.
func copyFrom(out io.Writer, path string, offset int64) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A workload that has yet to write anything. The follow waits for it
			// rather than ending on a file that is about to exist.
			return offset, nil
		}

		return offset, fmt.Errorf("failed to open workload output: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return offset, fmt.Errorf("failed to measure workload output: %w", err)
	}

	if info.Size() < offset {
		offset = 0
	}

	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("failed to read workload output: %w", err)
	}

	written, err := io.Copy(out, file)
	if err != nil {
		return offset + written, fmt.Errorf("failed to write workload output: %w", err)
	}

	return offset + written, nil
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
