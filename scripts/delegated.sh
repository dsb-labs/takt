#!/usr/bin/env bash

# Runs a command with what takt needs to run exec workloads: a delegated cgroup
# subtree of its own, which is what lets it enforce resource limits, and an
# ambient CAP_SETGID, which is what lets it keep the server's groups from a
# workload. See the "Delegation" and "Confinement" sections of docs/operating.md.
#
# The subtree is always a fresh scope, never the one this shell is in. A
# terminal's own scope looks delegated too, since the user manager hands every
# app scope the controllers, and a server preparing it treats the scope as its
# own: it moves the terminal's processes into the leaf it makes and runs the
# suite's workloads beside them. Terminals have died with the suite that way.

set -e

# Mirrors the server's own probe: a process in groups beyond its primary needs
# CAP_SETGID in its ambient set to drop them from a workload. One in no other
# group needs nothing, since there is nothing to drop.
capable() {
	local ambient

	if [ -z "$(id -G | tr ' ' '\n' | grep -vx "$(id -g)")" ]; then
		return 0
	fi

	ambient=$(awk '/^CapAmb:/ { print $2 }' /proc/self/status)

	# CAP_SETGID is capability number 6.
	[ $((0x$ambient & (1 << 6))) -ne 0 ]
}

# Asks systemd for a scope with a command that does nothing, so a host that
# cannot grant one fails here with the message below rather than as a failing
# command.
grantable() {
	systemd-run --user --scope --quiet -p Delegate=yes true 2>/dev/null
}

# Raised the way the unit raises it for the server, as an ambient capability,
# through capsh as this user. sudo replaces the environment, and its -E is not
# honoured everywhere, so the environment is saved first and restored by the
# shell capsh starts: the user manager's address and the Go cache both live in
# it. A terminal is asked for the password, since the alternative is a suite
# that cannot run. Anything else, which is what a CI runner is, gets sudo -n so
# nothing ever waits on a prompt. pam_cap grants the same thing to a session
# once, and a shell that has it never reaches this. See CONTRIBUTING.md.
raise() {
	local environment
	environment=$(mktemp)
	export -p >"$environment"

	exec sudo $1 capsh --keep=1 --user="$(id -un)" --inh=cap_setgid --addamb=cap_setgid \
		-- -c '. "$1" && rm -f "$1" && shift && exec "$0" "$@"' "$0" "$environment" "${command[@]}"
}

command=("$@")

if ! capable; then
	if [ -n "$TAKT_DELEGATED_CAPSH" ]; then
		echo "sudo and capsh ran, and this shell still holds no ambient CAP_SETGID" >&2
		exit 1
	fi

	export TAKT_DELEGATED_CAPSH=1

	if [ -t 0 ]; then
		echo "exec workloads need CAP_SETGID to drop this user's groups, and this shell has no ambient one" >&2
		echo "sudo is about to ask for your password to raise it for this run alone; pam_cap makes this permanent (see CONTRIBUTING.md)" >&2

		raise ""
	fi

	if sudo -n true 2>/dev/null; then
		raise -n
	fi

	echo "this shell holds supplementary groups and no ambient CAP_SETGID, so exec workloads would inherit the groups" >&2
	echo "grant the capability to your sessions through pam_cap, as CONTRIBUTING.md describes" >&2
	exit 1
fi

# The scope this script made, which the command is now inside.
if [ -n "$TAKT_DELEGATED_SCOPE" ]; then
	exec "$@"
fi

export TAKT_DELEGATED_SCOPE=1

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
