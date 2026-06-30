#!/bin/sh
# /etc/profile.d/fleet-motd.sh — print the fleet login banner (#461).
#
# Installed by scripts/bootstrap.sh. On an interactive login it runs `fleet motd`,
# which prints the build version, the systemd service state, and the handful of
# operator commands — no database, no config, no secrets. Bounded to 2s so a
# slow/missing systemd can never hang the shell, and a no-op when `fleet` isn't
# on PATH or the session isn't a TTY.
#
# This mirrors the sibling chat repo's MOTD, but renders dynamically from the
# installed binary (so the version + service state are always current) instead
# of baking a static /etc/motd at install time.

# Only for interactive terminals.
[ -t 1 ] || return 0 2>/dev/null || exit 0

# Only when the unified CLI is installed.
command -v fleet >/dev/null 2>&1 || return 0 2>/dev/null || exit 0

if command -v timeout >/dev/null 2>&1; then
	timeout 2s fleet motd 2>/dev/null || true
else
	fleet motd 2>/dev/null || true
fi
