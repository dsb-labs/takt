//go:build linux

package exec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// How often the driver looks at the output of every workload naming a cap.
//
// The process writes to its file directly, so the driver only learns the file has
// grown by looking. A second bounds how far past the cap a chatty workload gets to
// what it writes in one, and a stat per capped workload each second costs nothing
// next to the work the workload is doing.
const rotateInterval = time.Second

// rotate looks at the output of every workload naming a cap and rotates what has
// reached it.
//
// Failures are logged rather than returned. Nothing calls this that could act on an
// error, and one workload's unrotatable output should not stop the rest being looked
// at.
func (d *Driver) rotate() {
	ids, err := d.known()
	if err != nil {
		d.logger.With("error", err).Warn("failed to list workloads for output rotation")

		return
	}

	for _, id := range ids {
		records, err := d.records(id)
		if err != nil {
			d.logger.With("id", id, "error", err).Warn("failed to list instances for output rotation")

			continue
		}

		for _, r := range records {
			recorded, err := d.states.read(r.path)
			// An ended or retained process writes nothing more, so its output cannot
			// grow past where it is.
			if err != nil || recorded.LogMaxSize == 0 || recorded.Ended || retained(r.path) {
				continue
			}

			dir, err := d.place(d.workloads, id, r.instance, recorded.Version)
			if err != nil {
				continue
			}

			rotated, err := rotateFile(filepath.Join(dir, outputFile), recorded.LogMaxSize, recorded.LogMaxFiles)
			if err != nil {
				d.logger.With("workload", recorded.Workload, "instance", r.instance, "error", err).Warn("failed to rotate workload output")

				continue
			}

			if rotated {
				d.logger.With("workload", recorded.Workload, "instance", r.instance).Debug("workload output rotated")
			}
		}
	}
}

// rotateFile rotates the file at path when it has reached maxSize, keeping maxFiles
// files including the one being written. It reports whether it rotated.
//
// The process holds the file open with O_APPEND and keeps writing through the
// rotation, so the file cannot be renamed away from under it: writes would keep
// reaching the old inode. It is copied aside and truncated in place instead, which
// an appending writer lands correctly after. Whatever the process writes between the
// end of the copy and the truncate is lost, which is the price of rotating a file
// takt never sits in the write path of.
func rotateFile(path string, maxSize int64, maxFiles int) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A workload that has written nothing yet.
			return false, nil
		}

		return false, fmt.Errorf("failed to measure workload output: %w", pathless(err))
	}

	if info.Size() < maxSize {
		return false, nil
	}

	// The oldest file goes and the rest move up one, so the copy about to be made
	// lands in the first slot. A cap of one file keeps nothing older, so the output
	// is only truncated.
	if maxFiles > 1 {
		if err = os.Remove(rotated(path, maxFiles-1)); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("failed to remove rotated output: %w", pathless(err))
		}

		for i := maxFiles - 2; i >= 1; i-- {
			if err = os.Rename(rotated(path, i), rotated(path, i+1)); err != nil && !os.IsNotExist(err) {
				return false, fmt.Errorf("failed to move rotated output: %w", pathless(err))
			}
		}

		if err = copyFile(path, rotated(path, 1)); err != nil {
			return false, err
		}
	}

	if err = os.Truncate(path, 0); err != nil {
		return false, fmt.Errorf("failed to truncate workload output: %w", pathless(err))
	}

	return true, nil
}

// rotated names the i-th rotated file beside path, counted from one as the most
// recently rotated.
func rotated(path string, i int) string {
	return path + "." + strconv.Itoa(i)
}

// removeRotated removes every rotated file beside path.
//
// The files are numbered from one without gaps, so the first number missing is the
// end of them.
func removeRotated(path string) error {
	for i := 1; ; i++ {
		err := os.Remove(rotated(path, i))
		if os.IsNotExist(err) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("failed to remove rotated output: %w", pathless(err))
		}
	}
}

// copyFile copies the contents of src into a new file at dst, replacing one there.
func copyFile(src, dst string) error {
	from, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open workload output: %w", pathless(err))
	}
	defer from.Close()

	to, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create rotated output: %w", pathless(err))
	}

	if _, err = io.Copy(to, from); err != nil {
		_ = to.Close()

		return fmt.Errorf("failed to copy workload output: %w", pathless(err))
	}

	if err = to.Close(); err != nil {
		return fmt.Errorf("failed to write rotated output: %w", pathless(err))
	}

	return nil
}
