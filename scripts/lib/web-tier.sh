#!/usr/bin/env bash
# scripts/lib/web-tier.sh — retire the Next.js web tier (ADR-0091).
#
# Until ADR-0091 the browser UI was a separate Next.js service, fleet-web.service
# (node, /opt/fleet/web, /etc/fleet/fleet-web.env), in front of the fleet
# binary. The fleet binary now serves the UI itself on the same loopback port
# (FLEET_WEB_ADDR, default 127.0.0.1:3000), so on an upgraded box the old unit
# must be stopped and removed BEFORE fleet restarts, or the new listener cannot
# bind. update.sh calls this right before its restart; doctor.sh offers it.
#
# Sourced, not executed. Callers provide ok/warn/info. Idempotent: on a box
# that never had the web tier, or already retired it, it does nothing and
# returns 1 ("nothing to do").
#
# Kept deliberately: /etc/fleet/fleet-web.env (update.sh copies its browser
# keys into fleet.env first; the file holds secrets, so deleting it is the
# operator's call) and the fleet-web system user (removing a user is not an
# upgrade's business). Both are named in the output.

# fleet_web_tier_present — 0 when any piece of the retired tier is installed.
fleet_web_tier_present() {
  [[ -f /etc/systemd/system/fleet-web.service \
     || -d /etc/systemd/system/fleet-web.service.d \
     || -e /usr/local/bin/fleet-web-start.sh ]] && return 0
  command -v systemctl >/dev/null 2>&1 && systemctl cat fleet-web.service >/dev/null 2>&1
}

# fleet_retire_web_tier [SRC_DIR] — stop, disable and remove the retired unit,
# its drop-in and start shim, and the deployed Next build. SRC_DIR is the
# source checkout, which is never removed even when the unit served from it.
fleet_retire_web_tier() {
  local src="${1:-}" web_dst=""
  fleet_web_tier_present || return 1
  if command -v systemctl >/dev/null 2>&1; then
    web_dst="$(systemctl show -p WorkingDirectory --value fleet-web.service 2>/dev/null || true)"
    if systemctl disable --now fleet-web.service >/dev/null 2>&1 \
       || systemctl stop fleet-web.service >/dev/null 2>&1; then
      ok "stopped and disabled fleet-web (the Next.js web tier) — fleet serves the web UI itself now (ADR-0091)"
    fi
  fi
  rm -f /etc/systemd/system/fleet-web.service 2>/dev/null \
    || warn "could not remove /etc/systemd/system/fleet-web.service (need root?)"
  rm -rf /etc/systemd/system/fleet-web.service.d 2>/dev/null || true
  rm -f /usr/local/bin/fleet-web-start.sh 2>/dev/null || true
  command -v systemctl >/dev/null 2>&1 && { systemctl daemon-reload 2>/dev/null || true; systemctl reset-failed fleet-web.service 2>/dev/null || true; }
  # The deployed Next build (bootstrap copied web/ there, node_modules and all).
  # Only a directory that looks like that copy, and never the source checkout.
  web_dst="${web_dst:-/opt/fleet/web}"
  if [[ -d "$web_dst" && -f "$web_dst/package.json" ]] \
     && [[ "$(cd "$web_dst" && pwd -P)" != "$(cd "${src:-/nonexistent}" 2>/dev/null && pwd -P)/web" ]] \
     && [[ -d "$web_dst/.next" || -d "$web_dst/node_modules" ]]; then
    if rm -rf "$web_dst" 2>/dev/null; then
      ok "removed the old Next.js build at ${web_dst}"
    else
      warn "could not remove ${web_dst} (need root?) — it is unused now and safe to delete"
    fi
  fi
  if [[ -f /etc/fleet/fleet-web.env ]]; then
    info "/etc/fleet/fleet-web.env is no longer read (its sign-in keys live in fleet.env now); delete it once you have confirmed sign-in works"
  fi
  if id -u fleet-web >/dev/null 2>&1; then
    info "the fleet-web system user is no longer used; remove it with: userdel fleet-web"
  fi
  return 0
}
