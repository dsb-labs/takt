#!/usr/bin/env bash

# Runs a command inside a delegated cgroup subtree, which is what lets takt
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

# Asks systemd for a scope with a command that does nothing, so a host that
# cannot grant one fails here with the message below rather than as a failing
# command.
grantable() {
	systemd-run --user --scope --quiet -p Delegate=yes true 2>/dev/null
}

if delegated; then
	exec "$@"
fi

if grantable; then
	exec systemd-run --user --scope --quiet -p Delegate=yes "$@"
fi

# A user with no login session has no user manager for systemd-run to talk to,
# which is what a CI runner is. Lingering starts one. Attempted with sudo -n so
# this acts only where sudo needs no password, and never prompts a person in
# the middle of a make target.
if sudo -n loginctl enable-linger "$(whoami)" 2>/dev/null; then
	export XDG_RUNTIME_DIR="/run/user/$(id -u)"
	export DBUS_SESSION_BUS_ADDRESS="unix:path=${XDG_RUNTIME_DIR}/bus"

	for _ in $(seq 1 20); do
		[ -S "${XDG_RUNTIME_DIR}/bus" ] && break
		sleep 0.5
	done

	if grantable; then
		exec systemd-run --user --scope --quiet -p Delegate=yes "$@"
	fi
fi

echo "this shell has no delegated cgroup subtree and systemd cannot grant one" >&2
echo "start a systemd user session, or run the command from a delegated shell" >&2
exit 1
