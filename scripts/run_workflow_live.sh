#!/usr/bin/env bash
# run_workflow_live.sh — run a SINGLE task YAML to completion locally via cutlass,
# fleet's one-shot harness, with a live-tailable log and an isolated workspace.
#
# This is operator convenience around `go run ./cmd/cutlass`: it ensures the
# client-bundle sandbox image exists (so agent tool calls run in the real
# rootless-Podman container, not host-mode), mints a fresh per-run workspace so
# the run never collides with the server's shared workspace/, and points a stable
# `latest.log` symlink at the run so you can `tail -f` it.
#
# It runs the SAME governed scheduled core the production scheduler uses
# (agentcore.Run, Mode=Scheduled) — this is a local front-end, not a second,
# weaker execution path.
#
# Usage:
#   scripts/run_workflow_live.sh path/to/task.yaml
#
# Requirements: a real OPENROUTER_API_KEY in the environment / .env file, podman,
# and a client bundle (FLEET_CLIENT_CONFIG_DIR, default the in-repo config/default).
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <task.yaml>" >&2
  exit 1
fi
TASK_YAML="$1"
if [[ ! -f "$TASK_YAML" ]]; then
  echo "error: task file not found: $TASK_YAML" >&2
  exit 1
fi

# Resolve the repo root from this script's location (mirrors build-sandbox-image.sh).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Ensure the sandbox image exists (build-on-box default). Reuse the canonical
# builder rather than duplicating the podman build; it builds whatever bundle
# FLEET_CLIENT_CONFIG_DIR points at. Skip with FLEET_SKIP_SANDBOX_BUILD=1 when the
# image is already present (or to run host-mode via FLEET_MOCK_MODE=1).
if [[ "${FLEET_SKIP_SANDBOX_BUILD:-0}" != "1" ]]; then
  "$SCRIPT_DIR/build-sandbox-image.sh"
fi

# A fresh per-run workspace under .fleet-runs/ (gitignored territory), plus a
# stable latest.* symlink set for tailing.
RUNS_DIR="${FLEET_RUNS_DIR:-$REPO_ROOT/.fleet-runs}"
mkdir -p "$RUNS_DIR"
RUN_DIR="$(mktemp -d "$RUNS_DIR/run-XXXXXX")"
LOG_PATH="$RUN_DIR/session.json"

ln -sfn "$RUN_DIR" "$RUNS_DIR/latest"
ln -sfn "$LOG_PATH" "$RUNS_DIR/latest.json"

echo "cutlass: workspace=$RUN_DIR" >&2
echo "cutlass: tail the log with:  tail -f $RUNS_DIR/latest.json" >&2

cd "$REPO_ROOT"
exec go run ./cmd/cutlass --workspace "$RUN_DIR" --log "$LOG_PATH" "$TASK_YAML"
