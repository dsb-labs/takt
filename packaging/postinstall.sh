#!/bin/sh
# Create the takt system user and make systemd read the new unit. The
# service is not enabled or started: granting docker socket access is a
# deliberate operator step, so a fresh install cannot serve yet.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
fi
