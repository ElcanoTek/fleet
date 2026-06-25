#!/usr/bin/env bash
# build-native-agent-image.sh — build the fleet-native-agent image.
#
# This is the container the `native-acp` runtime flavor runs the agent
# ORCHESTRATION LOOP inside (cmd/fleet-native-agent). native-acp is the DEFAULT
# flavor (#159): the whole loop runs in this container and delegates every tool /
# MCP call back to the host, so it holds no privileged local executor and never
# shares the fleet process's address space (or its host-held MCP credentials).
#
# The image EXTENDS the sandbox base image (config/default/sandbox/Containerfile.native-agent
# `SANDBOX_BASE` build-arg), so the sandbox image must be built first. It is a
# FLEET artifact (fleet's own agent binary), built from the repo, regardless of
# which client bundle is active; only the base image is client-specific.
#
# This is the canonical build recipe. cmd/fleet's startup preflight refuses to
# boot when native-acp is the default but this image is absent, so a fresh
# deployment MUST build it — scripts/bootstrap.sh calls this; the e2e harness
# (scripts/e2e-boot-server.sh ensure_native_agent) builds the same image the
# same way. Keep the three in sync.
#
# Env:
#   FLEET_NATIVE_AGENT_IMAGE  target tag (default localhost/fleet-native-agent:latest)
#   FLEET_SANDBOX_IMAGE       base image to extend (default localhost/fleet-sandbox:latest)
#
# Usage:
#   scripts/build-native-agent-image.sh
#   FLEET_SANDBOX_IMAGE=ghcr.io/acme/sandbox:latest scripts/build-native-agent-image.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINERFILE="$REPO_ROOT/config/default/sandbox/Containerfile.native-agent"
IMAGE="${FLEET_NATIVE_AGENT_IMAGE:-localhost/fleet-native-agent:latest}"
SANDBOX_BASE="${FLEET_SANDBOX_IMAGE:-localhost/fleet-sandbox:latest}"

if ! command -v podman >/dev/null 2>&1; then
    echo "build-native-agent-image: podman is required" >&2
    exit 1
fi
if [[ ! -f "$CONTAINERFILE" ]]; then
    echo "build-native-agent-image: missing $CONTAINERFILE" >&2
    exit 1
fi

echo "Building ${IMAGE} from ${CONTAINERFILE} (SANDBOX_BASE=${SANDBOX_BASE}) ..."
podman build -t "${IMAGE}" -f "$CONTAINERFILE" --build-arg "SANDBOX_BASE=${SANDBOX_BASE}" "$REPO_ROOT"
echo "Built ${IMAGE}."
echo "Resolved by the fleet process from the bundle (runtimes.native-acp.image); FLEET_NATIVE_AGENT_IMAGE still overrides."
