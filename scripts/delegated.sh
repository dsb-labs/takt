#!/usr/bin/env bash

# Runs a command inside a delegated cgroup subtree, which is what lets orca
# enforce resource limits on exec workloads. A command already inside one runs
# unchanged, so the wrapper costs nothing where the environment is already
# right. See the "Delegation" section of docs/operating.md.

set -e

# Mirrors the server's own probe: the subtree this process is in must offer the
# memory, cpu and pids controllers, and must be writable.
delegated() {
	local path root controllers controller

	path=$(grep '^0::' /proc/self/cgroup | cut -d: -f3-) || return 1
	root="/sys/fs/cgroup${path}"

	# A shell already inside the server's leaf is inside a prepared subtree,
	# and the delegation is the parent.
	if [ "$(basename "$root")" = "main" ]; then
		root=$(dirname "$root")
	fi

	controllers=" $(cat "$root/cgroup.controllers" 2>/dev/null) "
	for controller in memory cpu pids; do
		case "$controllers" in
		*" $controller "*) ;;
		*) return 1 ;;
		esac
	done

	[ -w "$root" ] && [ -w "$root/cgroup.subtree_control" ]
}

if delegated; then
	exec "$@"
fi

# Asked with a command that does nothing, so a host without a systemd user
# session fails here with the message below rather than as a failing command.
if systemd-run --user --scope --quiet -p Delegate=yes true 2>/dev/null; then
	exec systemd-run --user --scope --quiet -p Delegate=yes "$@"
fi

echo "this shell has no delegated cgroup subtree and systemd cannot grant one" >&2
echo "start a systemd user session, or run the command from a delegated shell" >&2
exit 1
