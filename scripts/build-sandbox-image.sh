#!/usr/bin/env bash
# build-sandbox-image.sh — build the fleet sandbox image.
#
# This is the one container image that agent tool calls (bash, run_python)
# execute inside — per-turn for interactive chat, per-exec-burst for scheduled
# tasks — over a persistent same-path workspace, under hardened rootless flags
# (--read-only --cap-drop=ALL --security-opt=no-new-privileges, etc.).
#
# The fleet process reads the image reference from $FLEET_SANDBOX_IMAGE
# (default localhost/fleet-sandbox:latest). This is a local build, not a
# registry pull: the Containerfile + this script keep the supply chain
# auditable on the deployment box (most boxes have no private registry account).
#
# Usage:
#   scripts/build-sandbox-image.sh                 # → localhost/fleet-sandbox:latest
#   scripts/build-sandbox-image.sh v1              # → localhost/fleet-sandbox:v1
#   IMAGE_NAME=ghcr.io/elcanotek/sandbox scripts/build-sandbox-image.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINERFILE="$REPO_ROOT/images/sandbox/Containerfile"
IMAGE_NAME="${IMAGE_NAME:-localhost/fleet-sandbox}"
TAG="${1:-latest}"

if [[ ! -f "$CONTAINERFILE" ]]; then
    echo "build-sandbox-image: missing $CONTAINERFILE" >&2
    exit 1
fi
if ! command -v podman >/dev/null 2>&1; then
    echo "build-sandbox-image: podman is required" >&2
    exit 1
fi

echo "Building ${IMAGE_NAME}:${TAG} from images/sandbox/Containerfile ..."
podman build -t "${IMAGE_NAME}:${TAG}" -f "$CONTAINERFILE" "$REPO_ROOT/images/sandbox"
echo "Built ${IMAGE_NAME}:${TAG}."
echo "Set FLEET_SANDBOX_IMAGE=${IMAGE_NAME}:${TAG} for the fleet process."
