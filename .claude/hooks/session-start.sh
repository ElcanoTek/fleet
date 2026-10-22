#!/usr/bin/env bash
# SessionStart hook for Claude Code cloud sessions: install what fleet's tests
# and linters need, so `make lint`, `make test` and the web checks run without
# a manual setup step. Runs only in a remote (cloud) session; a local session
# already has its own toolchain. Synchronous, idempotent and non-interactive:
# every step is a no-op when its result is already in place.
set -euo pipefail

if [[ "${CLAUDE_CODE_REMOTE:-}" != "true" ]]; then
  exit 0
fi

ROOT="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
cd "$ROOT"

# Go modules (the Makefile sets GOTOOLCHAIN=auto, so go.mod's pinned toolchain
# is fetched on demand too).
export GOTOOLCHAIN=auto
go mod download

# golangci-lint at the version CI pins (.github/workflows/ci.yml), built with
# this Go so it understands go.mod's language version.
GOLANGCI_VERSION="v2.14.0"
GOBIN_DIR="$(go env GOPATH)/bin"
if ! "$GOBIN_DIR/golangci-lint" version 2>/dev/null | grep -q "${GOLANGCI_VERSION#v}"; then
  go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_VERSION}"
fi

# The web tree. `npm install` (not `npm ci`) keeps an existing node_modules,
# which is what makes a cached container fast; it still honours the lockfile.
if command -v npm >/dev/null 2>&1; then
  (cd web && npm install --no-audit --no-fund)
else
  echo "session-start: npm not found — the web checks need node (web/.nvmrc)" >&2
fi

# Make the Go tools visible to the session.
if [[ -n "${CLAUDE_ENV_FILE:-}" ]]; then
  echo "export PATH=\"$GOBIN_DIR:\$PATH\"" >> "$CLAUDE_ENV_FILE"
fi
