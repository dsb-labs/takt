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
	// Whether the process has ended, and how.
	Ended bool `json:"ended"`
	// The exit code, meaningful only when Ended.
	ExitCode int `json:"exitCode"`
}

// readState reads the record for one instance.
func readState(path string) (state, error) {
	data, err := os.ReadFile(filepath.Join(path, stateFile))
	if err != nil {
		return state{}, fmt.Errorf("failed to read instance state: %w", err)
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
		return fmt.Errorf("failed to write instance state: %w", err)
	}

	if err = os.Rename(tmp, filepath.Join(path, stateFile)); err != nil {
		return fmt.Errorf("failed to replace instance state: %w", err)
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
