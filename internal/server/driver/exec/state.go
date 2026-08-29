package exec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The state type is what the driver records about one instance, and is the whole of
// its bookkeeping.
//
// It is written to the workload's directory rather than to the database, because the
// server persists desired state only and asks a driver what is actually running. A
// pidfile is the runtime's own record in the same way a container label is docker's.
type state struct {
	// The name of the workload the process belongs to.
	//
	// The record carries it because the directory holding the record does not: the
	// directory is named for the workload's identifier, so nothing has to parse a path
	// to learn a name, and a name never has to be safe as a path component.
	Workload string `json:"workload"`
	// The process the driver started.
	PID int `json:"pid"`
	// When the kernel says that process started, in clock ticks since boot.
	//
	// Recorded because a pid alone does not identify a process: the kernel reuses
	// them, so a pid that is alive after a restart may belong to something else
	// entirely. The pair is what makes the record trustworthy.
	StartTicks uint64 `json:"startTicks"`
	// The hash of the specification the process was started from.
	SpecHash string `json:"specHash"`
	// The version of the specification the process was started from.
	Version int `json:"version"`
	// When the driver started the process.
	StartedAt time.Time `json:"startedAt"`
	// Where the cgroup enforcing the process's resource limits is, empty for a
	// workload that asked for none.
	//
	// Recorded so that the cgroup can be removed by a server that did not create
	// it: the process outlives the server, and its limits have to outlive it too.
	Cgroup string `json:"cgroup,omitempty"`
	// Whether the process has ended, and how.
	Ended bool `json:"ended"`
	// The exit code, meaningful only when Ended.
	ExitCode int `json:"exitCode"`
}

// readState reads the record for one instance.
func readState(path string) (state, error) {
	data, err := os.ReadFile(filepath.Join(path, stateFile))
	if err != nil {
		return state{}, fmt.Errorf("failed to read instance state: %w", pathless(err))
	}

	var s state
	if err = json.Unmarshal(data, &s); err != nil {
		return state{}, fmt.Errorf("failed to decode instance state: %w", err)
	}

	return s, nil
}

// writeState records an instance, replacing whatever was there.
//
// The file is written whole and moved into place, so a reader either sees the previous
// record or the new one. A partial write would be read as a corrupt record for a
// workload that is running perfectly well.
func writeState(path string, s state) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to encode instance state: %w", err)
	}

	tmp := filepath.Join(path, stateFile+".tmp")
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("failed to write instance state: %w", pathless(err))
	}

	if err = os.Rename(tmp, filepath.Join(path, stateFile)); err != nil {
		return fmt.Errorf("failed to replace instance state: %w", pathless(err))
	}

	return nil
}

// retained reports whether a record describes an attempt that was stopped and kept,
// rather than one that ended on its own and is waiting to be restarted.
//
// A file beside the record rather than a field in it, because the record is rewritten by
// the goroutine waiting on the process: a flag written into it as the workload is
// replaced is lost the moment that goroutine collects the exit code. Separate files mean
// the two writers never touch the same one and no ordering has to hold.
func retained(path string) bool {
	_, err := os.Stat(filepath.Join(path, retainedFile))

	return err == nil
}

// retain marks a record as describing an attempt that was kept.
func retain(path string) error {
	if err := os.WriteFile(filepath.Join(path, retainedFile), nil, 0o600); err != nil {
		return fmt.Errorf("failed to mark instance state as retained: %w", pathless(err))
	}

	return nil
}

// unretain clears the mark, for a version directory an attempt is being started in.
//
// A restart at an unchanged version reuses the directory the previous attempt stopped
// in, so the mark left there describes the attempt that has just been replaced rather
// than the one about to run. Left in place it would report a running process as kept.
func unretain(path string) error {
	if err := os.Remove(filepath.Join(path, retainedFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clear the retained mark: %w", pathless(err))
	}

	return nil
}

// alive reports whether the recorded process is still running and is still the same
// process.
//
// Signalling zero asks the kernel about a pid without affecting it. That alone is not
// enough: a pid is reused, so a live pid may be something else that happens to hold
// the number now. Comparing the start time is what tells those apart.
func (s state) alive() bool {
	if s.PID <= 0 {
		return false
	}

	if err := syscall.Kill(s.PID, 0); err != nil {
		return false
	}

	ticks, err := startTicks(s.PID)
	if err != nil {
		// The process answered a signal a moment ago, so failing to read its start
		// time means it has since gone rather than that it was never there.
		return false
	}

	return ticks == s.StartTicks
}

// startTicks reads when a process started, in clock ticks since boot.
//
// Field 22 of /proc/<pid>/stat is the value, counted from the start of the line. The
// second field is the executable name in parentheses and may itself contain spaces or
// parentheses, so the fields are counted from after the last closing parenthesis
// rather than by splitting the whole line.
func startTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, fmt.Errorf("failed to read process stat: %w", err)
	}

	line := string(data)

	end := strings.LastIndex(line, ")")
	if end < 0 || end+2 > len(line) {
		return 0, fmt.Errorf("process stat for %d is malformed", pid)
	}

	fields := strings.Fields(line[end+2:])

	// Field 22 overall is the twentieth after the executable name, which is the
	// second field.
	const startTime = 19

	if len(fields) <= startTime {
		return 0, fmt.Errorf("process stat for %d has too few fields", pid)
	}

	ticks, err := strconv.ParseUint(fields[startTime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse process start time: %w", err)
	}

	return ticks, nil
}
