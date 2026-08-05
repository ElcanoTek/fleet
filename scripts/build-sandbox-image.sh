#!/usr/bin/env bash
# build-sandbox-image.sh — build the fleet sandbox image FROM THE CLIENT BUNDLE.
#
# This is the one container image that agent tool calls (bash, run_python)
# execute inside — per-turn for interactive chat, per-exec-burst for scheduled
# tasks — over a persistent same-path workspace, under hardened rootless flags
# (--read-only --cap-drop=ALL --security-opt=no-new-privileges, etc.).
#
# The sandbox is a PER-CLIENT CONFIG-BUNDLE artifact: the Containerfile lives in
# the bundle at <bundle>/sandbox/Containerfile, so each client's XYZ-config
# repo ships its own sandbox flavor (and pins its own base digest). This script
# builds whatever bundle FLEET_CLIENT_CONFIG_DIR points at (default
# config/default, the generic bundle baked into the repo).
#
# DEFAULT = build-on-box: the Containerfile + this script keep the supply chain
# auditable on the deployment box (most boxes have no private registry account).
# REGISTRY PUBLISH is opt-in per client: set `sandbox.image` in the bundle's
# manifest.yaml to a prebuilt ref and the fleet process pulls/uses that instead
# (see internal/clientconfig Sandbox()).
#
# The image TAG defaults to the bundle manifest's `sandbox.tag`
# (localhost/fleet-sandbox:latest in the generic bundle). Override with an
# explicit IMAGE_NAME and/or a positional tag arg.
#
# The fleet process reads the resolved image reference from the bundle
# (clientconfig.Sandbox().ResolvedImageRef()); an explicit FLEET_SANDBOX_IMAGE
# in the process env still overrides.
#
# Run AS ROOT on a box with the systemd unit installed (the `sudo fleet update`
# path), the build is re-run as the unit's User= so the image lands in the
# SERVICE user's rootless store — root's rootful store is a separate namespace
# the User=fleet unit can never see. FLEET_SERVICE_NAME selects the unit
# (default fleet); without systemd/the unit the invoking user's store is used.
#
# Usage:
#   scripts/build-sandbox-image.sh                 # → manifest sandbox.tag (default localhost/fleet-sandbox:latest)
#   scripts/build-sandbox-image.sh v1              # → <image-name>:v1
#   scripts/build-sandbox-image.sh --print-tag     # print the resolved <image-name>:<tag>; no build, no podman
#   IMAGE_NAME=ghcr.io/your-org/sandbox scripts/build-sandbox-image.sh   # tag for a client's own registry
#   FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client scripts/build-sandbox-image.sh   # build a client bundle's sandbox
set -euo pipefail

PRINT_TAG=0
if [[ "${1:-}" == "--print-tag" ]]; then
    PRINT_TAG=1
    shift
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUNDLE_DIR="${FLEET_CLIENT_CONFIG_DIR:-$REPO_ROOT/config/default}"
MANIFEST="$BUNDLE_DIR/manifest.yaml"

# containerfile: manifest sandbox.containerfile (relative to the bundle) when
# present, else the conventional default path inside the bundle.
manifest_value() {
    # Extract a scalar under the top-level `sandbox:` block from the manifest.
    # Tiny purpose-built parser (no yq dependency): finds the `sandbox:` line,
    # then the first `  <key>:` under it, strips quotes/inline-comments. Good
    # enough for the flat scalars this block carries; the Go loader is the
    # authoritative parser the process consumes.
    local key="$1" file="$2"
    [[ -f "$file" ]] || return 0
    awk -v key="$key" '
        /^sandbox:[[:space:]]*$/ { in_block=1; next }
        /^[^[:space:]]/ { in_block=0 }
        in_block && $0 ~ "^[[:space:]]+" key ":" {
            sub("^[[:space:]]+" key ":[[:space:]]*", "")
            sub(/[[:space:]]+#.*$/, "")            # strip inline comment
            gsub(/^["'\'']|["'\'']$/, "")          # strip surrounding quotes
            print
            exit
        }
    ' "$file"
}

CF_REL="$(manifest_value containerfile "$MANIFEST")"
CF_REL="${CF_REL:-sandbox/Containerfile}"
CONTAINERFILE="$BUNDLE_DIR/$CF_REL"

# Image name/tag: explicit IMAGE_NAME wins; else derive from the manifest's
# sandbox.tag (name:tag). Positional $1 overrides the tag.
MANIFEST_TAG="$(manifest_value tag "$MANIFEST")"
MANIFEST_TAG="${MANIFEST_TAG:-localhost/fleet-sandbox:latest}"
if [[ -n "${IMAGE_NAME:-}" ]]; then
    IMAGE_NAME="$IMAGE_NAME"
    TAG="${1:-latest}"
else
    IMAGE_NAME="${MANIFEST_TAG%:*}"
    TAG="${1:-${MANIFEST_TAG##*:}}"
fi

# Machine-readable resolution for callers that gate on the tag (update.sh's
# rebuild gate): the exact ref a build would produce, from the SAME manifest
# read the build uses. No podman, no side effects.
if [[ "$PRINT_TAG" == "1" ]]; then
    printf '%s:%s\n' "$IMAGE_NAME" "$TAG"
    exit 0
fi

if [[ ! -f "$CONTAINERFILE" ]]; then
    echo "build-sandbox-image: missing $CONTAINERFILE" >&2
    echo "  (FLEET_CLIENT_CONFIG_DIR=$BUNDLE_DIR; the bundle must ship $CF_REL)" >&2
    exit 1
fi
if ! command -v podman >/dev/null 2>&1; then
    echo "build-sandbox-image: podman is required" >&2
    exit 1
fi

BUILD_CONTEXT="$(dirname "$CONTAINERFILE")"

# A root build must not land in root's rootful store: deploy/fleet.service runs
# the process as its User=, and that rootless account's image store under
# ~/.local/share/containers is a separate namespace — an image built by root is
# invisible to the service. Build as the unit's user with the HOME/
# XDG_RUNTIME_DIR the unit sets (a nologin system account has no login session,
# and /run/<user> is tmpfs — absent after a reboot until the unit's
# RuntimeDirectory= recreates it). cd to that HOME first: rootless podman
# re-execs inside its user namespace and chdir()s back to the inherited cwd,
# which the service user may not be able to enter (e.g. /root). Mirrors
# bootstrap.sh's systemd-path build (which cannot delegate here: it runs before
# the unit is installed) and doctor.sh's run_as_fleet.
BUILD_USER=""
if [[ $EUID -eq 0 ]] && command -v systemctl >/dev/null 2>&1; then
    BUILD_USER="$(systemctl show -p User --value "${FLEET_SERVICE_NAME:-fleet}.service" 2>/dev/null || true)"
fi
if [[ -n "$BUILD_USER" && "$BUILD_USER" != "root" ]] && id -u "$BUILD_USER" >/dev/null 2>&1; then
    BUILD_HOME="$(getent passwd "$BUILD_USER" | cut -d: -f6)"
    install -d -o "$BUILD_USER" -g "$BUILD_USER" -m 0700 "/run/${BUILD_USER}"
    echo "Building ${IMAGE_NAME}:${TAG} from ${CONTAINERFILE} (as ${BUILD_USER} — the unit's rootless store) ..."
    (
        cd "$BUILD_HOME" 2>/dev/null || cd /
        runuser -u "$BUILD_USER" -- env HOME="$BUILD_HOME" XDG_RUNTIME_DIR="/run/${BUILD_USER}" \
            podman build -t "${IMAGE_NAME}:${TAG}" -f "$CONTAINERFILE" "$BUILD_CONTEXT"
    )
else
    echo "Building ${IMAGE_NAME}:${TAG} from ${CONTAINERFILE} ..."
    podman build -t "${IMAGE_NAME}:${TAG}" -f "$CONTAINERFILE" "$BUILD_CONTEXT"
fi
echo "Built ${IMAGE_NAME}:${TAG}."
echo "Resolved by the fleet process from the bundle (sandbox.tag); FLEET_SANDBOX_IMAGE still overrides."
