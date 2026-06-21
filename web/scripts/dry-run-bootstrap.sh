#!/usr/bin/env bash
# scripts/dry-run-bootstrap.sh — run bootstrap.sh inside a throwaway
# fedora:latest container. Catches real-world install bugs (wrong dnf
# package names, stale paths, flag-parsing regressions, broken build
# steps) without needing an actual VM.
#
# What it skips: systemd, firewalld, Caddy — none of those work in a
# vanilla container. CHAT_BOOTSTRAP_DRY_RUN=1 signals bootstrap.sh to
# skip those branches while still exercising everything else.
#
# Interactive vs. non-interactive: bootstrap.sh prompts humans for
# answers, but every prompt also honors a CHAT_BOOTSTRAP_<NAME> env var.
# We use those so the container boots hands-off (TTY would need
# `podman run --tty` which blocks stdin piping — messy).
#
# Requires: podman on the host. ~2GB disk for the Fedora image cache.
# ~5-10min wall clock, dominated by the Next.js production build.
#
# Usage:
#   scripts/dry-run-bootstrap.sh
#
# Env overrides:
#   IMAGE                       base image (default: fedora:latest)
#   CHAT_DRY_OPENROUTER_KEY     OpenRouter key (default: a placeholder;
#                               validation will warn but continue)
#   KEEP=1                      keep the container for manual poking

set -euo pipefail

IMAGE="${IMAGE:-fedora:latest}"
OR_KEY="${CHAT_DRY_OPENROUTER_KEY:-sk-or-v1-dry-run-placeholder}"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CTR_NAME="chat-dry-run-$$"

echo "==> building Fedora dry-run container: $CTR_NAME from $IMAGE"

rm_flag="--rm"
[[ "${KEEP:-0}" == "1" ]] && rm_flag=""

podman run \
  $rm_flag \
  --name "$CTR_NAME" \
  --volume "$REPO:/src:ro,z" \
  --env CHAT_BOOTSTRAP_DRY_RUN=1 \
  --env CHAT_BOOTSTRAP_NON_INTERACTIVE=1 \
  --env CHAT_BOOTSTRAP_OPENROUTER_KEY="$OR_KEY" \
  --env CHAT_BOOTSTRAP_HOSTNAME=localhost \
  --env CHAT_BOOTSTRAP_USERS="dryrun@example.com,ops@example.com" \
  --env TERM=xterm \
  "$IMAGE" \
  bash -c '
    set -euo pipefail

    # Mirror the repo into a writable path — bootstrap rsyncs + builds
    # from here, and /src is read-only.
    cp -a /src /opt/chat-src
    cd /opt/chat-src

    # Fedora base images ship without rsync or openssl, both of which
    # bootstrap.sh relies on. Pre-install so the script makes it past
    # step 1.
    dnf install -y --quiet rsync openssl >/dev/null

    exec bash scripts/bootstrap.sh
  '

rc=$?

echo
if [[ $rc -eq 0 ]]; then
  echo "==> bootstrap.sh completed in container (rc=0)"
else
  echo "==> bootstrap.sh FAILED in container (rc=$rc)"
fi

if [[ "${KEEP:-0}" == "1" && $rc -eq 0 ]]; then
  echo "==> KEEP=1 — container preserved. To poke around:"
  echo "    podman exec -it $CTR_NAME bash"
  echo "    podman rm -f $CTR_NAME   # when done"
fi

exit $rc
