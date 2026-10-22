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
# Sourced, not executed. Callers provide ok/warn/info (and upsert_env_file
# FILE KEY VALUE for fleet_migrate_web_env). Idempotent: on a box
# that never had the web tier, or already retired it, it does nothing and
# returns 1 ("nothing to do").
#
# Kept deliberately: /etc/fleet/fleet-web.env (update.sh copies its browser
# keys into fleet.env first; the file holds secrets, so deleting it is the
# operator's call) and the fleet-web system user (removing a user is not an
# upgrade's business). Both are named in the output.

# FLEET_WEB_ENV_CARRY_KEYS — the keys of the retired tier's env file that the
# fleet binary reads unchanged (internal/webui/config.go ConfigFromEnv). ONE
# list, used by both update.sh and bootstrap.sh, so the two can never drift.
# Audited against every key fleet-web.env ever held (main's web/src
# process.env reads plus what bootstrap wrote):
#   carried as-is ... the keys below
#   mapped .......... NEXT_PUBLIC_PUBLIC_ORIGIN -> FLEET_PUBLIC_BASE_URL;
#                     FLEET_WEB_HOST + PORT -> FLEET_WEB_ADDR (non-default only)
#   dropped ......... CHAT_SERVER_URL, ORCHESTRATOR_SERVER_URL (the backends are
#                     in-process now); CHAT_SERVER_TOKEN, ORCHESTRATOR_SERVER_TOKEN
#                     (the binary uses its own FLEET_SERVER_TOKEN; an old box may
#                     even hold a wrong ORCHESTRATOR_SERVER_TOKEN — an old
#                     bootstrap wrote ADMIN_API_KEY there — so it is never carried); NODE_ENV,
#                     FLEET_NODE_BIN, NEXT_PUBLIC_BUILD_ID, BUILD_ID (build/run
#                     plumbing of the node tier); NEXT_PUBLIC_APP_NAME (the name
#                     comes from the bundle's branding).
FLEET_WEB_ENV_CARRY_KEYS=(
  APP_SESSION_SECRET
  AUTH_SIGNING_PUBKEY AUTH_SIGNING_PREVIOUS_PUBKEYS
  AUTH_COOKIE_NAME AUTH_COOKIE_DOMAIN AUTH_LOGIN_URL
  CATWALK_URL
  FLEET_OIDC_ISSUER FLEET_OIDC_CLIENT_ID FLEET_OIDC_CLIENT_SECRET
  FLEET_OIDC_SCOPES FLEET_OIDC_ALLOWED_DOMAINS FLEET_OIDC_BUTTON_LABEL
  FLEET_OIDC_REDIRECT_URI FLEET_OIDC_AUTO_START
)

# fleet__web_env_get FILE KEY — the LAST assignment of KEY (systemd's
# EnvironmentFile precedence), surrounding quotes stripped; empty when absent.
fleet__web_env_get() {
  local v
  v="$(grep -E "^${2}=" "$1" 2>/dev/null | tail -n1 | cut -d= -f2-)" || true
  v="${v%\"}"; v="${v#\"}"; v="${v%\'}"; v="${v#\'}"
  printf '%s' "$v"
}

# fleet_migrate_web_env OLD_ENV NEW_ENV — carry the retired tier's settings
# into the server env file. Never overwrites a key NEW_ENV already sets (the
# operator's value wins, and re-runs are no-ops). Prints how many keys it
# wrote; returns 0 even when there was nothing to do.
fleet_migrate_web_env() {
  local old="$1" new="$2" k v n=0 host port
  [[ -r "$old" ]] || return 0
  for k in "${FLEET_WEB_ENV_CARRY_KEYS[@]}"; do
    grep -q "^${k}=" "$new" 2>/dev/null && continue
    v="$(fleet__web_env_get "$old" "$k")"
    [[ -n "$v" ]] || continue
    if upsert_env_file "$new" "$k" "$v"; then n=$((n + 1)); else warn "could not copy ${k} into ${new} — set it there by hand"; fi
  done
  # The canonical origin, under the binary's name for it.
  if ! grep -q '^FLEET_PUBLIC_BASE_URL=' "$new" 2>/dev/null; then
    v="$(fleet__web_env_get "$old" NEXT_PUBLIC_PUBLIC_ORIGIN)"
    if [[ -n "$v" ]] && upsert_env_file "$new" FLEET_PUBLIC_BASE_URL "$v"; then n=$((n + 1)); fi
  fi
  # The old tier's bind: only a non-default one needs carrying (fleet's
  # default is the address the old unit used, 127.0.0.1:3000).
  host="$(fleet__web_env_get "$old" FLEET_WEB_HOST)"; port="$(fleet__web_env_get "$old" PORT)"
  host="${host:-127.0.0.1}"; port="${port:-3000}"
  if [[ "${host}:${port}" != "127.0.0.1:3000" ]] && ! grep -q '^FLEET_WEB_ADDR=' "$new" 2>/dev/null; then
    [[ "$host" == *:* && "$host" != \[* ]] && host="[${host}]"
    if upsert_env_file "$new" FLEET_WEB_ADDR "${host}:${port}"; then n=$((n + 1)); fi
  fi
  (( n > 0 )) && ok "carried ${n} browser-tier setting(s) from ${old} into ${new}"
  return 0
}

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
