#!/usr/bin/env bash
# scripts/start.sh — boot chat-server (Go) + Next.js dev server together.
#
# Expects a populated .env.local in the repo root. The Go binary is rebuilt
# from sources on every invocation (fast; vendored deps cached in $GOMODCACHE).
#
# On Ctrl-C or exit, both processes are torn down. Logs stream interleaved
# to the controlling terminal.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ ! -f .env.local ]; then
  echo "missing .env.local — copy .env.local.example to .env.local and fill in values" >&2
  exit 1
fi

# Export the .env.local values for the Go server subprocess. The Next.js dev
# server reads .env.local on its own.
set -a
# shellcheck disable=SC1091
. ./.env.local
set +a

# Build chat-server
echo "==> building chat-server"
mkdir -p bin
(cd server && GOTOOLCHAIN=auto go build -o ../bin/chat-server ./cmd/chat-server)

# Install Python deps for the MCP subprocesses. We use `uv pip install`
# (not `uv pip sync`) because sync expects a fully-resolved lockfile and
# silently skips transitive resolution against our hand-written
# constraints file — that broke jmespath (a botocore transitive) on the
# first dev attempt. install resolves transitives correctly.
if [ -f server/requirements.txt ]; then
  echo "==> ensuring Python deps (via uv — pip's resolver is way too slow)"
  # --break-system-packages bypasses PEP 668's EXTERNALLY-MANAGED marker
  # that Fedora/Debian ship on recent Python. Without it, system-level
  # installs silently fail and MCP subprocesses crash on missing imports.
  if command -v uv >/dev/null 2>&1; then
    uv pip install --system --no-cache --break-system-packages -r server/requirements.txt || \
      echo "warn: uv install failed; MCP servers may not start" >&2
    # One-shot cleanup of host-side run_python kernel deps that moved
    # into the sandbox container image. Idempotent (no-op once gone).
    uv pip uninstall --system --break-system-packages \
      ipykernel ipython jupyter-client pyzmq pandas numpy >/dev/null 2>&1 || true
  else
    echo "warn: uv not found, falling back to pip (slower + flakier)"
    python3 -m pip install --break-system-packages --user -r server/requirements.txt || \
      echo "warn: pip install failed; MCP servers may not start" >&2
  fi
fi

# Data dir
mkdir -p data

# Boot chat-server in the background from server/ (so it picks up assets there).
echo "==> starting chat-server on ${CHAT_SERVER_ADDR:-127.0.0.1:8080}"
(
  cd server
  exec ../bin/chat-server -env ../.env.local
) &
SERVER_PID=$!

cleanup() {
  echo "==> shutting down"
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Boot Next.js in the foreground.
echo "==> starting Next.js"
exec npm run dev
