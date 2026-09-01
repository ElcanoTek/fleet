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
# Prerequisites: python3 >= 3.11, uv (https://docs.astral.sh/uv/), and the
# conformance rig below. The TCK sends NO credentials and drives SUT task
# scenarios through messageId prefixes, so it talks to fleet through
# scripts/a2a-tck-shim (auth injection + scenario markers) with cmd/fake-llm
# as the model seam — see that shim's header for the full rationale.
#
# The rig, in order (one shell each or backgrounded):
#
#   1. go run ./cmd/fake-llm                        # FAKE_LLM_ADDR=127.0.0.1:18090
#   2. fleet serve with at least:
#        FLEET_A2A_ENABLED=1
#        OPENROUTER_BASE_URL=http://127.0.0.1:18090   (the fake-LLM seam)
#        FLEET_PUBLIC_BASE_URL=http://127.0.0.1:18001 (the SHIM address — the
#                                                      card must route the TCK
#                                                      through it)
#        FLEET_MCP_OAUTH_ENCRYPTION_KEY=<base64 32B>  (enables push configs)
#        FLEET_A2A_PUSH_ALLOW_PRIVATE=1               (TCK receiver = localhost)
#        FLEET_A2A_UNARY_WAIT_SECONDS=25              (inside the TCK's 30s
#                                                      client read timeout)
#        FLEET_SCHED_TICK_SECONDS=2                   (sub-30s task dispatch:
#                                                      the TCK's blocking sends
#                                                      must see the outcome
#                                                      INSIDE the unary wait)
#        FLEET_TASK_MODEL=<any>                       (fake-LLM accepts all)
#   3. mint a task key:  POST /v1/keys {"name":"tck","type":"task"} (admin key)
#   4. TCK_API_KEY=<that key> go run ./scripts/a2a-tck-shim
#   5. scripts/a2a-tck.sh http://127.0.0.1:18001     # point the TCK at the SHIM
#
# Send one tck-complete-task warm-up first (or expect the first scenario test
# to skip): the initial task claim pays the warm-pool cold start.
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
