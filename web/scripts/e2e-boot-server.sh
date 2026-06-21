#!/usr/bin/env bash
# scripts/e2e-boot-server.sh — build (if needed) + run chat-server for e2e.
#
# Playwright's webServer spawns this. We:
#   1. Spin up a throwaway Postgres cluster scoped to this run.
#   2. Rebuild the binary if sources changed (cheap with cached module cache).
#   3. Exec chat-server with DATABASE_URL pointed at the scratch cluster,
#      inheriting the other ambient env (CHAT_MOCK_MODE, CHAT_SERVER_ADDR,
#      CHAT_SERVER_TOKEN, …) set by playwright.config.ts.
#
# Must exec chat-server in the foreground so Playwright can tear it down.
# The Postgres cleanup runs via EXIT trap so orphaned processes + data
# dirs don't accumulate across Playwright runs.

set -euo pipefail
cd "$(dirname "$0")/.."

# Resolve pg_ctl / initdb. Fedora installs them under /usr/bin; other
# distros may hide them under /usr/lib/postgresql/<version>/bin.
PG_BIN="$(command -v pg_ctl >/dev/null && dirname "$(command -v pg_ctl)" || true)"
if [[ -z "$PG_BIN" ]]; then
  for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin; do
    [[ -x "$d/pg_ctl" ]] && PG_BIN="$d" && break
  done
fi
if [[ -z "$PG_BIN" || ! -x "$PG_BIN/pg_ctl" ]]; then
  echo "e2e-boot-server: pg_ctl not found — install postgresql-server (dnf install postgresql-server)" >&2
  exit 1
fi

DATA_DIR="${CHAT_DATA_DIR:-/tmp/chat-e2e-data}"
PG_DIR="${CHAT_E2E_PGDATA:-/tmp/chat-e2e-pgdata}"
PG_PORT="${CHAT_E2E_PGPORT:-55433}"
PG_LOG="${PG_DIR}.log"

# Postgres refuses to run as root for security. If we're root, drop to
# the `postgres` system user (which dnf install postgresql-server
# creates) and make sure the data dir is writable by them. A non-root
# caller (a developer running the suite locally) runs the tools
# directly as themselves.
if [[ $EUID -eq 0 ]]; then
  PG_SUDO=(runuser -u postgres --)
  # postgres user needs to be able to create the data dir. /tmp is
  # everyone-writable, so just chown after mkdir.
else
  PG_SUDO=()
fi

pg_run() {
  if [[ ${#PG_SUDO[@]} -gt 0 ]]; then
    "${PG_SUDO[@]}" "$@"
  else
    "$@"
  fi
}

# Kill any leftover Postgres from an aborted prior run BEFORE wiping
# the data dir — a live backend process holding open file handles on
# pg_control makes the next initdb/pg_ctl combination fail with
# mismatched-WAL-identifier panics. Playwright's hard kill on test
# exit sometimes outruns our EXIT trap, so we can't rely on the trap
# having done the right thing last time.
pkill -9 -f "postgres -D $PG_DIR" 2>/dev/null || true
# Give the kernel a tick to actually release the files.
sleep 0.3

rm -rf "$DATA_DIR" "$PG_DIR" "$PG_LOG"
mkdir -p "$DATA_DIR"
# Container-mode sandbox bind-mounts the workspace root, so podman
# statfs's it during run. If the dir is missing every container start
# fails opaquely with "no such file or directory". chat-server itself
# self-creates this when it boots (agent.go), but pre-creating is
# cheaper than relying on a fresh binary.
if [[ -n "${CHAT_WORKSPACE_ROOT:-}" ]]; then
  mkdir -p "$CHAT_WORKSPACE_ROOT"
fi
# Pre-create PG_DIR as postgres-owned if we're root, otherwise initdb
# below creates it itself.
if [[ $EUID -eq 0 ]]; then
  install -d -o postgres -g postgres -m 0700 "$PG_DIR"
fi

# Silent initdb; surface stderr on failure. --auth-host=trust keeps the
# e2e setup password-free; everything is bound to localhost.
pg_run "$PG_BIN/initdb" -D "$PG_DIR" -U postgres \
  --auth-host=trust --auth-local=trust >/dev/null 2>&1 || {
  echo "e2e-boot-server: initdb failed" >&2
  pg_run "$PG_BIN/initdb" -D "$PG_DIR" -U postgres --auth-host=trust --auth-local=trust >&2 || true
  exit 1
}

# Bind to localhost only, pick a non-default port so we don't collide
# with a dev Postgres on :5432. fsync=off etc. are safe for a scratch
# cluster that gets wiped after every run. logging_collector=off keeps
# startup output on stderr where pg_ctl can see it; the default in
# Postgres 18 is to redirect into a sub-dir logfile that pg_ctl -w
# can't observe, so the wait appears to hang.
pg_run tee -a "$PG_DIR/postgresql.conf" >/dev/null <<EOF
listen_addresses = '127.0.0.1'
port = $PG_PORT
unix_socket_directories = '$PG_DIR'
fsync = off
synchronous_commit = off
full_page_writes = off
logging_collector = off
EOF

# pg_ctl writes $PG_LOG, so it must be writable by whoever's running
# it. Remove any stale log from a prior run so `install` creates it
# fresh with the right owner — overwriting a file owned by another
# user across the root/postgres boundary hits permission-denied under
# sandbox namespaces even though root normally bypasses DAC.
if [[ $EUID -eq 0 ]]; then
  rm -f "$PG_LOG"
  install -m 0644 -o postgres -g postgres /dev/null "$PG_LOG"
fi

pg_run "$PG_BIN/pg_ctl" -D "$PG_DIR" -l "$PG_LOG" -w start >/dev/null 2>&1 || {
  echo "e2e-boot-server: pg_ctl start failed — log:" >&2
  tail -n 50 "$PG_LOG" >&2 || true
  exit 1
}

cleanup() {
  [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null || true
  pg_run "$PG_BIN/pg_ctl" -D "$PG_DIR" -m fast stop >/dev/null 2>&1 || true
  rm -rf "$PG_DIR" "$PG_LOG"
}
trap cleanup EXIT

"$PG_BIN/psql" -h 127.0.0.1 -p "$PG_PORT" -U postgres -v ON_ERROR_STOP=1 <<EOF >/dev/null
CREATE ROLE chat LOGIN PASSWORD 'chat';
CREATE DATABASE chat OWNER chat;
EOF

export DATABASE_URL="postgres://chat:chat@127.0.0.1:${PG_PORT}/chat?sslmode=disable"

BIN="$(pwd)/bin/chat-server"
ADMIN="$(pwd)/bin/chat-admin"
if [[ ! -x "$BIN" ]] || [[ ! -x "$ADMIN" ]]; then
  (cd server && GOTOOLCHAIN=auto go build -o ../bin/chat-server ./cmd/chat-server)
  (cd server && GOTOOLCHAIN=auto go build -o ../bin/chat-admin  ./cmd/chat-admin)
fi

# Start chat-server in the background, provision the test user via
# chat-admin once /healthz reports ready, then hand control over.
"$BIN" &
SERVER_PID=$!

for _ in $(seq 1 50); do
  curl -fsS --max-time 1 "http://${CHAT_SERVER_ADDR:-127.0.0.1:8080}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done

# Seed the test user — idempotent-ish: if already exists, ignore the error.
"$ADMIN" user add "${E2E_TEST_EMAIL:-e2e@example.com}" \
  --password "${E2E_TEST_PASSWORD:-e2e-test-password}" >/dev/null 2>&1 || true

# Server is already running; wait on it (this is what Playwright polls).
wait $SERVER_PID
