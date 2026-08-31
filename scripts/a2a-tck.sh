#!/usr/bin/env bash
# Copyright (c) 2026 ElcanoTek
# SPDX-License-Identifier: MIT
#
# Run the official A2A Technology Compatibility Kit against a live fleet
# server (#1279). NOT a CI gate in Phase 1 — the blocking gate is the Go test
# suite (internal/a2a, internal/sched/handlers/a2a_test.go); this script is
# the pre-release cross-check. Run it, then attach reports/compatibility.json
# to the PR touching the A2A surface.
#
# Prerequisites: python3 >= 3.11, uv (https://docs.astral.sh/uv/), and a fleet
# server booted with FLEET_A2A_ENABLED=1 (the fake-LLM seam is fine — the TCK
# exercises the protocol, not the model).
#
# Usage:
#   scripts/a2a-tck.sh http://127.0.0.1:8000
#
# Note the TCK's own spec pin (v1.0.0) trails fleet's (v1.0.1,
# internal/a2a.SpecVersion); MUST-level results are comparable, but check a
# surprising failure against the v1.0.1 proto before treating it as a fleet bug.
set -euo pipefail

SUT_HOST="${1:?usage: scripts/a2a-tck.sh <fleet-orchestrator-base-url>}"

# NOT pinned: upstream has no release tags yet, so every run gets whatever
# a2a-tck main is that day — results can drift between runs for upstream
# reasons. Before wiring this into CI, replace "main" with a reviewed SHA
# (and re-check tck/requirements/registry.py for new MUSTs when bumping).
TCK_REPO="https://github.com/a2aproject/a2a-tck"
TCK_COMMIT="main"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

echo "==> cloning a2a-tck @ ${TCK_COMMIT}"
git clone --quiet --depth 1 "$TCK_REPO" "$workdir/tck"
cd "$workdir/tck" || exit 1
git checkout --quiet "$TCK_COMMIT"

echo "==> installing (uv)"
uv venv --quiet
uv pip install --quiet -e .

echo "==> running MUST-level JSON-RPC compatibility against ${SUT_HOST}"
uv run ./run_tck.py --sut-host "$SUT_HOST" --transport jsonrpc --level must

echo "==> reports in $workdir/tck/reports (copying compatibility.json here)"
cp reports/compatibility.json "$OLDPWD/a2a-tck-compatibility.json" 2>/dev/null || true
echo "wrote a2a-tck-compatibility.json"
