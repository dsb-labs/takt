package exec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// ErrNotConfinable is returned when the kernel does not offer the Landlock support an
// exec workload is confined with.
//
// There is deliberately no degraded mode. A confinement that silently did nothing on
// some hosts would be a guarantee nothing could reason about, and every mention of it
// would have to be qualified.
var ErrNotConfinable = errors.New("kernel does not support confining exec workloads")

const (
	// The argument that makes orca confine itself and exec a command rather than run
	// the command line it was given.
	//
	// Landlock restricts the calling thread, so a ruleset has to be applied after fork
	// and before exec. Go's os/exec offers no hook there, so orca execs itself with
	// this argument, the child confines itself, and it then becomes the workload's
	// command. Two leading underscores because this is orca talking to itself: it is
	// not a subcommand, takes no part in the CLI, and nothing outside this package
	// should ever pass it.
	confineArg = "__confine"
	// The descriptor the ruleset is read from.
	//
	// A pipe rather than an argument or a variable. An argument is readable from
	// /proc/<pid>/cmdline by anything on the host, and the environment belongs to the
	// workload, which is given only what it asked for.
	rulesetFD = 3
	// The descriptor the reason confinement failed is written to.
	//
	// Marked close-on-exec, so a successful exec closes it and the parent reads end of
	// file. Anything read before then is why the workload is not running. Without this
	// a path the kernel refused would surface as an unexplained exit code rather than
	// as a start failure naming the path.
	statusFD = 4
	// The Landlock ABI a host has to offer.
	//
	// Three rather than one, because a read-only path is only genuinely read-only from
	// the third: below it truncation is unrestricted, so a workload could empty a file
	// it cannot write. Two adds only the reparenting right, which nothing here asks
	// for. This is Linux 6.2 and above.
	requiredABI = 3
)

// The paths every workload may read and execute from, which is what makes a command
// able to run at all.
//
// A dynamically linked program needs its interpreter and its libraries, and almost
// every program reads something under /etc. These are the host's own files rather than
// orca's: nothing orca stores lives here, so opening them discloses nothing about
// other workloads.
//
// Missing entries are ignored rather than refused. Which of these a host has depends on
// its distribution and its architecture, and one that is not there grants nothing.
var systemPaths = []string{
	"/bin",
	"/sbin",
	"/usr",
	"/lib",
	"/lib32",
	"/lib64",
	"/libx32",
	"/etc",
	"/opt",
	// Read-only, so a workload learns about the host it runs on and can read its own
	// process. The kernel still refuses another process's entries, so this is not a way
	// to read a second workload's environment.
	"/proc",
	"/sys",
}

// The devices a workload may read and write.
//
// Named individually rather than by granting /dev, which holds the host's disks. These
// are the ones a program uses without thinking about it: discarding output, reading
// randomness, writing to its terminal.
//
// There is deliberately no grant for /tmp. It is shared by everything running as this
// user, so granting it would let one workload read what another wrote there — which is
// most of what confinement exists to stop. A workload with something to write has its
// own directory, and one that insists on a temporary directory can be told to use it
// with TMPDIR.
var devicePaths = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/full",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
}

// The ruleset type is what a workload is confined to, sent from the server to the
// process that applies it.
type ruleset struct {
	// The command to become once the ruleset is applied.
	Command []string `json:"command"`
	// The directories the workload may read and write: its own, and its volumes.
	Write []string `json:"write"`
	// The directories the workload may read and execute from.
	Read []string `json:"read"`
	// The individual files the workload may read, which is what a mounted value is,
	// and the command's own binary.
	Files []string `json:"files"`
}

// Confine turns this process into a workload's confined command, when it was started
// to be one, and otherwise returns so the caller carries on.
//
// Called before anything else a binary does, because this process is not running orca:
// it is about to become the workload. Anything the caller would otherwise set up would
// be discarded by the exec a moment later.
//
// It does not return in the case it exists for. Either the process becomes the
// workload's command, or it reports why it could not and exits.
func Confine() {
	if len(os.Args) < 2 || os.Args[1] != confineArg {
		return
	}

	status := os.NewFile(statusFD, "status")

	// Closed by the exec below, which is how the server learns the command is running.
	// Set here rather than by the server, because the descriptor has to survive the
	// fork and not the exec.
	syscall.CloseOnExec(statusFD)

	if err := confine(os.NewFile(rulesetFD, "ruleset")); err != nil {
		// The only report there is. This process holds the workload's output, so
		// writing there would put orca's own failure inside the workload's log.
		fmt.Fprint(status, err.Error())

		os.Exit(1)
	}
}

// confine applies the ruleset on the given descriptor to this process and becomes the
// command it names.
//
// Returns only on failure. The exec replaces this process, so there is nothing to
// return to once it succeeds.
func confine(in *os.File) error {
	var rs ruleset
	if err := json.NewDecoder(in).Decode(&rs); err != nil {
		return fmt.Errorf("failed to read the ruleset: %w", err)
	}

	if err := in.Close(); err != nil {
		return fmt.Errorf("failed to close the ruleset: %w", err)
	}

	if len(rs.Command) == 0 {
		return fmt.Errorf("%w: no command", ErrNotExecWorkload)
	}

	if err := restrict(rs); err != nil {
		return fmt.Errorf("failed to confine the workload: %w", err)
	}

	// The environment is this process's own, which the server set to the workload's
	// when it started the confinement. Nothing is added or removed here.
	if err := syscall.Exec(rs.Command[0], rs.Command, os.Environ()); err != nil {
		return fmt.Errorf("failed to execute %s: %w", rs.Command[0], err)
	}

	return nil
}

// restrict applies a ruleset to this process, after which the kernel refuses it
// everything the ruleset does not name.
//
// Applied without best-effort, so a kernel offering less than the ruleset asks for
// fails here rather than confining the workload to less than it was promised.
//
// The reparenting right is not asked for. Moving a file within a granted directory
// works without it, moving one between two of them is not something a workload needs
// orca's help to avoid, and asking for it falls back to no confinement at all on
// kernels below 5.19 — which is the one outcome this must not produce.
func restrict(rs ruleset) error {
	rules := []landlock.Rule{
		// The host's own files, and the devices a program expects to find. Missing
		// entries are ignored: which of these a host has varies, and one that is not
		// there grants nothing.
		landlock.RODirs(append(systemPaths, rs.Read...)...).IgnoreIfMissing(),
		landlock.RWFiles(devicePaths...).IgnoreIfMissing(),
	}

	// The workload's own directories, which have to be there: the driver created the
	// working directory and resolved every volume before starting this. One that is
	// missing means the workload would run without somewhere it was told it had, so it
	// is reported rather than ignored.
	if len(rs.Write) > 0 {
		rules = append(rules, landlock.RWDirs(rs.Write...))
	}

	// The command's binary and the values the workload mounts, each granted as a file
	// rather than by opening the directory holding it. A mounted secret lives beside
	// every other workload's, so granting the directory would hand over all of them.
	if len(rs.Files) > 0 {
		rules = append(rules, landlock.ROFiles(rs.Files...))
	}

	return landlock.V3.RestrictPaths(rules...)
}

// Confinable reports whether this host can confine a workload at all, returning
// ErrNotConfinable when it cannot.
//
// Asked before a workload is started rather than discovered while starting one, so a
// host that cannot confine says so instead of leaving a half-created workload behind.
//
// Exported so that a caller can report the same refusal the driver would, without
// having to start a workload to find out.
func Confinable() error {
	abi, err := ll.LandlockGetABIVersion()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotConfinable, err)
	}

	if abi < requiredABI {
		return fmt.Errorf("%w: landlock abi %d is available, %d is required (linux 6.2 or later)",
			ErrNotConfinable, abi, requiredABI)
	}

	return nil
}
