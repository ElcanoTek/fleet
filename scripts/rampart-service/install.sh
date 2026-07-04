#!/usr/bin/env bash
# install.sh — build and host the Rampart PII detection service with the tools
# a fleet box already has: rootless podman + systemd. One command from zero to
# a supervised, health-checked service; no Node on the host (it lives in the
# container, and the ONNX model is baked in at build time so the service needs
# no runtime network access to Hugging Face).
#
#   scripts/rampart-service/install.sh            # build + install + start
#   scripts/rampart-service/install.sh --uninstall
#
# Then paste the printed URL into Settings → Admin → Feature settings →
# "Rampart service URL" and switch the PII detection engine to Rampart.
#
# Env overrides:
#   RAMPART_PORT=8787        host port (loopback only)
#   RAMPART_IMAGE=localhost/fleet-rampart-service:latest
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${RAMPART_PORT:-8787}"
IMAGE="${RAMPART_IMAGE:-localhost/fleet-rampart-service:latest}"
UNIT_NAME="fleet-rampart"
URL="http://127.0.0.1:${PORT}/v1/redact"

# Rootless when run as a user; system-wide when root.
if [[ "$(id -u)" -eq 0 ]]; then
  SYSTEMCTL=(systemctl)
  UNIT_DIR="/etc/systemd/system"
else
  SYSTEMCTL=(systemctl --user)
  UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
fi

log() { printf '[rampart-install] %s\n' "$*"; }
die() { printf '[rampart-install] ERROR: %s\n' "$*" >&2; exit 1; }

command -v podman >/dev/null 2>&1 || die "podman is required (fleet already needs it for the sandbox)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

if [[ "${1:-}" == "--uninstall" ]]; then
  "${SYSTEMCTL[@]}" disable --now "${UNIT_NAME}.service" 2>/dev/null || true
  rm -f "${UNIT_DIR}/${UNIT_NAME}.service"
  "${SYSTEMCTL[@]}" daemon-reload
  podman rm -f "${UNIT_NAME}" 2>/dev/null || true
  log "uninstalled ${UNIT_NAME}.service (image ${IMAGE} kept; remove with: podman rmi ${IMAGE})"
  exit 0
fi

log "building ${IMAGE} (downloads the ~15 MB ONNX model into the image)…"
podman build -t "${IMAGE}" "${SCRIPT_DIR}"

log "installing ${UNIT_NAME}.service (${UNIT_DIR})"
mkdir -p "${UNIT_DIR}"
cat > "${UNIT_DIR}/${UNIT_NAME}.service" <<UNIT
[Unit]
Description=fleet Rampart PII detection service (docs/PII-REDACTION.md)
Wants=network-online.target
After=network-online.target

[Service]
Restart=always
RestartSec=3
# --replace makes restarts idempotent; loopback publish keeps the service
# private to this box (it sees raw tool output — same trust domain as fleet).
ExecStart=/usr/bin/podman run --rm --replace --name ${UNIT_NAME} \\
  -p 127.0.0.1:${PORT}:8787 ${IMAGE}
ExecStop=/usr/bin/podman stop -t 5 ${UNIT_NAME}

[Install]
WantedBy=default.target
UNIT

"${SYSTEMCTL[@]}" daemon-reload
"${SYSTEMCTL[@]}" enable --now "${UNIT_NAME}.service"

log "waiting for the service to answer…"
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    log "ready."
    log ""
    log "  Rampart service URL:  ${URL}"
    log ""
    log "Paste it into Settings → Admin → Feature settings → 'Rampart service URL',"
    log "switch 'PII detection engine' to Rampart, and click 'Test detection'."
    exit 0
  fi
  sleep 2
done
die "service did not become healthy; check: ${SYSTEMCTL[*]} status ${UNIT_NAME} / podman logs ${UNIT_NAME}"
