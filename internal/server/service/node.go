package service

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Where the kernel reports the host's memory, load and boot time. Read on
// every request rather than cached: each is one small file, and a cached
// figure would be one more thing to explain when it disagrees with top.
const (
	procMeminfo = "/proc/meminfo"
	procLoadavg = "/proc/loadavg"
	procStat    = "/proc/stat"
)

type (
	// The Node type describes the machine the server runs on: what it is, and
	// what it has. The capacity figures are the host's, not what takt's
	// workloads consume; those are reported per instance.
	Node struct {
		// The host's name.
		Hostname string
		// The operating system, as the binary was built for it.
		OS string
		// The processor architecture, as the binary was built for it.
		Arch string
		// The kernel release.
		Kernel string
		// How many processors the host has.
		CPUs int
		// The version of the takt binary serving the request.
		Version string
		// When the server process started.
		StartedAt time.Time
		// When the host booted.
		BootedAt time.Time
		// The host's memory.
		Memory NodeMemory
		// The host's load averages.
		Load NodeLoad
		// The filesystems takt writes to.
		Disks NodeDisks
	}

	// The NodeMemory type describes the host's memory, in bytes.
	NodeMemory struct {
		// How much memory the host has.
		Total int
		// How much of it is in use, leaving out the page cache the kernel
		// reclaims before it refuses an allocation.
		Used int
	}

	// The NodeLoad type carries the host's load averages.
	NodeLoad struct {
		// The load averaged over the last minute.
		One float64
		// The load averaged over the last five minutes.
		Five float64
		// The load averaged over the last fifteen minutes.
		Fifteen float64
	}

	// The NodeDisks type describes the filesystems under the directories takt
	// writes to. Both are one filesystem unless the operator mounted something
	// at the volumes directory, which is the setup a box holding large volumes
	// has.
	NodeDisks struct {
		// The filesystem under the data directory.
		Data NodeDisk
		// The filesystem under the volumes directory.
		Volumes NodeDisk
	}

	// The NodeDisk type describes the filesystem under a directory, in bytes.
	NodeDisk struct {
		// The directory the figures describe, as configured. Reported even
		// when the directory has not been created yet.
		Path string
		// The size of the filesystem.
		Total int
		// How much of it an unprivileged writer can still use.
		Free int
	}

	// The NodeService type reports the machine the server runs on.
	NodeService struct {
		dataDirectory    string
		volumesDirectory string
		version          string
		startedAt        time.Time
	}

	// The NodeServiceConfig type contains fields used to construct a
	// NodeService.
	NodeServiceConfig struct {
		// The directory takt keeps its state in.
		DataDirectory string
		// The directory holding every volume.
		VolumesDirectory string
		// The version of the running binary.
		Version string
		// When the server process started.
		StartedAt time.Time
	}
)

// NewNodeService returns a new instance of the NodeService type.
func NewNodeService(config NodeServiceConfig) *NodeService {
	return &NodeService{
		dataDirectory:    config.DataDirectory,
		volumesDirectory: config.VolumesDirectory,
		version:          config.Version,
		startedAt:        config.StartedAt,
	}
}

// Get reports the machine the server runs on as it stands now.
//
// The figures are read from the kernel rather than from a runtime's daemon,
// so they describe the whole box rather than one driver's share of it.
func (s *NodeService) Get() (Node, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Node{}, fmt.Errorf("failed to read hostname: %w", err)
	}

	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return Node{}, fmt.Errorf("failed to read kernel release: %w", err)
	}

	memory, err := readProc(procMeminfo, parseMeminfo)
	if err != nil {
		return Node{}, fmt.Errorf("failed to read host memory: %w", err)
	}

	load, err := readProc(procLoadavg, parseLoadavg)
	if err != nil {
		return Node{}, fmt.Errorf("failed to read load averages: %w", err)
	}

	bootedAt, err := readProc(procStat, parseBootTime)
	if err != nil {
		return Node{}, fmt.Errorf("failed to read boot time: %w", err)
	}

	data, err := statfs(s.dataDirectory)
	if err != nil {
		return Node{}, fmt.Errorf("failed to read the data directory's filesystem: %w", err)
	}

	volumes, err := statfs(s.volumesDirectory)
	if err != nil {
		return Node{}, fmt.Errorf("failed to read the volumes directory's filesystem: %w", err)
	}

	return Node{
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Kernel:    unix.ByteSliceToString(uts.Release[:]),
		CPUs:      runtime.NumCPU(),
		Version:   s.version,
		StartedAt: s.startedAt,
		BootedAt:  bootedAt,
		Memory:    memory,
		Load:      load,
		Disks:     NodeDisks{Data: data, Volumes: volumes},
	}, nil
}

// readProc opens a procfs file and hands it to a parser.
func readProc[T any](path string, parse func(io.Reader) (T, error)) (T, error) {
	file, err := os.Open(path)
	if err != nil {
		var zero T

		return zero, err
	}
	defer file.Close()

	return parse(file)
}

// parseMeminfo reads the total and available memory from /proc/meminfo, whose
// lines are "Name:   <value> kB".
//
// Used is total minus available rather than total minus free: the kernel
// reclaims the page cache before it refuses an allocation, so free understates
// what a workload could still take, and available is the kernel's own estimate
// of that.
func parseMeminfo(r io.Reader) (NodeMemory, error) {
	var total, available int
	var haveTotal, haveAvailable bool

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		var target *int
		var have *bool
		switch fields[0] {
		case "MemTotal:":
			target, have = &total, &haveTotal
		case "MemAvailable:":
			target, have = &available, &haveAvailable
		default:
			continue
		}

		kilobytes, err := strconv.Atoi(fields[1])
		if err != nil {
			return NodeMemory{}, fmt.Errorf("failed to parse %s: %w", fields[0], err)
		}

		*target, *have = kilobytes*1024, true
	}

	if err := scanner.Err(); err != nil {
		return NodeMemory{}, err
	}

	if !haveTotal || !haveAvailable {
		return NodeMemory{}, errors.New("MemTotal or MemAvailable is missing")
	}

	return NodeMemory{Total: total, Used: total - available}, nil
}

// parseLoadavg reads the three load averages that open /proc/loadavg.
func parseLoadavg(r io.Reader) (NodeLoad, error) {
	content, err := io.ReadAll(r)
	if err != nil {
		return NodeLoad{}, err
	}

	fields := strings.Fields(string(content))
	if len(fields) < 3 {
		return NodeLoad{}, errors.New("fewer than three fields")
	}

	var averages [3]float64
	for i := range averages {
		averages[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return NodeLoad{}, fmt.Errorf("failed to parse field %d: %w", i, err)
		}
	}

	return NodeLoad{One: averages[0], Five: averages[1], Fifteen: averages[2]}, nil
}

// parseBootTime reads the boot instant from the btime line of /proc/stat.
//
// The instant rather than /proc/uptime, which is the same fact at centisecond
// resolution relative to now: subtracting it from the clock would give a boot
// time that drifts by a few milliseconds on every read.
func parseBootTime(r io.Reader) (time.Time, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("btime ")) {
			continue
		}

		seconds, err := strconv.ParseInt(strings.TrimSpace(string(line[len("btime "):])), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse btime: %w", err)
		}

		return time.Unix(seconds, 0), nil
	}

	if err := scanner.Err(); err != nil {
		return time.Time{}, err
	}

	return time.Time{}, errors.New("btime is missing")
}

// statfs reports the filesystem under a directory.
//
// A directory that does not exist yet is reported through its nearest existing
// ancestor, since that is the filesystem it will land on. The volumes directory
// is created by the first volume rather than at startup, and a read must not be
// the thing that decides the layout.
func statfs(path string) (NodeDisk, error) {
	var stat unix.Statfs_t

	probe := path
	for {
		err := unix.Statfs(probe, &stat)
		if err == nil {
			break
		}

		parent := filepath.Dir(probe)
		if !errors.Is(err, fs.ErrNotExist) || parent == probe {
			return NodeDisk{}, err
		}

		probe = parent
	}

	return NodeDisk{
		Path:  path,
		Total: int(stat.Blocks) * int(stat.Bsize),
		Free:  int(stat.Bavail) * int(stat.Bsize),
	}, nil
}
