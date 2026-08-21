#!/bin/sh
# deploy/fleet-web-start.sh — resolve the node interpreter, then become next.
#
# Why this exists: Fedora's node packages are PARALLEL-INSTALLABLE. Installing
# nodejs24 gives you /usr/bin/node-24, but /usr/bin/node keeps pointing at
# whichever stream Fedora designated the default for that release (22 on F44).
# So `dnf install nodejs24` alone would leave the web tier still running 22 —
# the same shape of bug as a systemd directive that never takes effect.
#
# The unit cannot solve this itself: systemd does NOT expand a variable used as
# the executable (`ExecStart=${FLEET_NODE_BIN} …` is read as a literal path,
# verified with systemd-analyze), so ExecStart must name a fixed program. This
# script is that fixed program, and it resolves the interpreter at start time.
#
# THIS IS NOT THE npm WRAPPER COMING BACK. npm's crash (see
# docs/WEB-TIER-SHUTDOWN.md) came from npm STAYING ALIVE as a supervisor: it
# lingered in the cgroup and forwarded SIGTERM to a child it had already
# reaped, segfaulting in uv_kill. This script `exec`s, so the shell is REPLACED
# by node — after the exec there is exactly one process in the cgroup, and
# SIGTERM reaches node with nothing in between to relay it. No supervisor, no
# extra pid, no signal forwarding.
#
# Resolution order:
#   1. $FLEET_NODE_BIN — set in /etc/fleet/fleet-web.env by bootstrap/doctor,
#      which pick the newest node satisfying web/.nvmrc. This is the normal path.
#   2. `node` on PATH — the portable fallback for distros that ship a single
#      unversioned node (Debian/Ubuntu, nodesource) and for hand-rolled setups.
#
# Version POLICY deliberately does not live here: scripts/doctor.sh owns the
# floor (read from web/.nvmrc) and is what warns or repairs. This script only
# reports which interpreter it used, so the journal answers "what node is
# actually serving?" — the question that made the original crash hard to pin
# on a version.

set -eu

node_bin="${FLEET_NODE_BIN:-}"
if [ -n "$node_bin" ]; then
  if ! [ -x "$node_bin" ]; then
    echo "fleet-web: FLEET_NODE_BIN=$node_bin is not executable" >&2
    exit 1
  fi
else
  node_bin="$(command -v node 2>/dev/null || true)"
  if [ -z "$node_bin" ]; then
    echo "fleet-web: no node found (set FLEET_NODE_BIN in /etc/fleet/fleet-web.env," \
         "or install node — see scripts/bootstrap.sh)" >&2
    exit 1
  fi
fi

# One line in the journal naming the interpreter and the app dir. Cheap, and it
# turns "which node is serving?" from an investigation into a grep.
echo "fleet-web: starting with $node_bin ($("$node_bin" -v 2>/dev/null || echo 'version unknown')) in $(pwd)"

# -H keeps the bind loopback-only; the unit defaults FLEET_WEB_HOST and the env
# file may override it. PORT is read by `next start` from the environment, as
# it always has been.
exec "$node_bin" node_modules/next/dist/bin/next start -H "${FLEET_WEB_HOST:-127.0.0.1}"
