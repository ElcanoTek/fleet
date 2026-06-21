#!/usr/bin/env bash
# scripts/build-sandbox-image.sh — build the per-turn sandbox image.
#
# Builds deploy/sandbox.Containerfile into a local Podman image tagged
# `localhost/elcanotek-chat-sandbox:<tag>`. The chat-server reads the
# image reference from CHAT_SANDBOX_IMAGE; provision.sh sets that to the
# tag we build here.
#
# Usage:
#   scripts/build-sandbox-image.sh                # tag=dev
#   scripts/build-sandbox-image.sh v1             # tag=v1
#   IMAGE_NAME=foo scripts/build-sandbox-image.sh # different name
#
# This is intentionally a local build, not a registry pull. Most
# ad-agency customer boxes don't have a dedicated registry account,
# and shipping a Containerfile + build script keeps the supply chain
# auditable on the customer side.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINERFILE="$REPO_ROOT/deploy/sandbox.Containerfile"

if [[ ! -f "$CONTAINERFILE" ]]; then
    echo "build-sandbox-image: missing $CONTAINERFILE" >&2
    exit 1
fi

if ! command -v podman >/dev/null 2>&1; then
    echo "build-sandbox-image: podman is required" >&2
    exit 1
fi

TAG="${1:-dev}"
IMAGE_NAME="${IMAGE_NAME:-localhost/elcanotek-chat-sandbox}"
FULL="${IMAGE_NAME}:${TAG}"

echo "Building $FULL from $CONTAINERFILE"
podman build \
    --pull=newer \
    --file="$CONTAINERFILE" \
    --tag="$FULL" \
    "$REPO_ROOT"

echo
echo "Built $FULL"
echo "Set CHAT_SANDBOX_IMAGE=$FULL in /opt/chat/.env.local to enable per-turn containerization."
