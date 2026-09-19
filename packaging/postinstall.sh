#!/bin/sh
# Create the takt system user and make systemd read the new unit. The
# service is not enabled or started: granting docker socket access is a
# deliberate operator step, so a fresh install cannot serve yet.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers
fi

# The config may hold an OIDC client secret, so only root and the takt user
# read it. The group is applied here rather than by the package, since the
# user does not exist until sysusers has run.
if getent group takt >/dev/null 2>&1; then
	chgrp takt /etc/takt /etc/takt/config.toml
	chmod 0750 /etc/takt
	chmod 0640 /etc/takt/config.toml
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
fi
