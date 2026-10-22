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
# FILE KEY VALUE for fleet_migrate_web_env — both scripts' copies delegate to
# envfile.sh's fleet_upsert_env_file, which re-encodes every value). Idempotent: on a box
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

# The env-file codec: values are read the way systemd read them for the old
# unit and written in the form every fleet.env reader agrees on (#1838).
# shellcheck source=envfile.sh
. "$(dirname "${BASH_SOURCE[0]}")/envfile.sh"

# fleet__web_env_get FILE KEY — KEY's value as fleet-web.service saw it
# (systemd EnvironmentFile= rules, last assignment wins); empty when absent.
fleet__web_env_get() { fleet_env_get_systemd "$1" "$2"; }

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

# Where the retired tier's pieces live. Overridable only so the scripts' tests
# can point them at a scratch directory instead of this machine's real ones.
FLEET_WEB_TIER_UNIT_DIR="${FLEET_WEB_TIER_UNIT_DIR:-/etc/systemd/system}"
FLEET_WEB_TIER_SHIM="${FLEET_WEB_TIER_SHIM:-/usr/local/bin/fleet-web-start.sh}"
FLEET_WEB_TIER_BUILD_DIR="${FLEET_WEB_TIER_BUILD_DIR:-/opt/fleet/web}"

# fleet_web_tier_present — 0 when any piece of the retired tier is installed.
fleet_web_tier_present() {
  [[ -f "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service" \
     || -d "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service.d" \
     || -e "$FLEET_WEB_TIER_SHIM" ]] && return 0
  command -v systemctl >/dev/null 2>&1 && systemctl cat fleet-web.service >/dev/null 2>&1
}

# The retirement is split so it is never final before the new process is up:
#   fleet_stop_web_tier      stop + disable the unit, keep every file
#   fleet_restore_web_tier   re-enable + start it (only if this run stopped it)
#   fleet_remove_web_tier    delete the unit, drop-in, shim and Next build
#   fleet_cutover_web_tier   stop, restart fleet, and remove only once fleet
#                            is active — else restore; bootstrap and doctor
# update.sh and fleet-upgrade.sh stop the tier before fleet restarts onto its
# port, and remove it only once the new process is healthy; a failed restart
# (and fleet-upgrade's rollback to the old binary) brings the old UI back.
# Nothing removes the tier before that point: there is deliberately no
# stop-and-remove-in-one helper (#1838 — bootstrap used one after a restore and
# deleted the tier it had just brought back).

# FLEET_WEB_TIER_STOPPED is 1 once fleet_stop_web_tier stopped a running or
# enabled unit in this process — what fleet_restore_web_tier undoes.
FLEET_WEB_TIER_STOPPED=0

# fleet_stop_web_tier — stop and disable fleet-web, keeping its files.
# Returns 1 when there is no tier to stop.
fleet_stop_web_tier() {
  fleet_web_tier_present || return 1
  command -v systemctl >/dev/null 2>&1 || return 0
  if systemctl is-enabled --quiet fleet-web.service 2>/dev/null || systemctl is-active --quiet fleet-web.service 2>/dev/null; then
    FLEET_WEB_TIER_STOPPED=1
  fi
  if systemctl disable --now fleet-web.service >/dev/null 2>&1 \
     || systemctl stop fleet-web.service >/dev/null 2>&1; then
    ok "stopped and disabled fleet-web (the Next.js web tier) — fleet serves the web UI itself now (ADR-0091)"
  fi
  return 0
}

# fleet_restore_web_tier — undo fleet_stop_web_tier: re-enable and start the
# old unit, for a rollback to a binary that does not serve the UI itself.
# A no-op unless this process stopped it and its unit file is still there.
fleet_restore_web_tier() {
  [[ "$FLEET_WEB_TIER_STOPPED" == "1" ]] || return 0
  command -v systemctl >/dev/null 2>&1 || return 0
  systemctl cat fleet-web.service >/dev/null 2>&1 || return 0
  if systemctl enable --now fleet-web.service >/dev/null 2>&1; then
    FLEET_WEB_TIER_STOPPED=0
    ok "re-enabled and started fleet-web (the previous binary does not serve the web UI)"
  else
    warn "could not restart fleet-web — run: sudo systemctl enable --now fleet-web"
  fi
}

# fleet_remove_web_tier [SRC_DIR] — remove the (stopped) unit, its drop-in
# and start shim, and the deployed Next build. SRC_DIR is the source
# checkout, which is never removed even when the unit served from it.
fleet_remove_web_tier() {
  local src="${1:-}" web_dst=""
  fleet_web_tier_present || return 1
  if command -v systemctl >/dev/null 2>&1; then
    web_dst="$(systemctl show -p WorkingDirectory --value fleet-web.service 2>/dev/null || true)"
    systemctl stop fleet-web.service >/dev/null 2>&1 || true
  fi
  rm -f "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service" 2>/dev/null \
    || warn "could not remove $FLEET_WEB_TIER_UNIT_DIR/fleet-web.service (need root?)"
  rm -rf "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service.d" 2>/dev/null || true
  rm -f "$FLEET_WEB_TIER_SHIM" 2>/dev/null || true
  command -v systemctl >/dev/null 2>&1 && { systemctl daemon-reload 2>/dev/null || true; systemctl reset-failed fleet-web.service 2>/dev/null || true; }
  FLEET_WEB_TIER_STOPPED=0
  # The deployed Next build (bootstrap copied web/ there, node_modules and all).
  # Only a directory that looks like that copy, and never the source checkout.
  web_dst="${web_dst:-$FLEET_WEB_TIER_BUILD_DIR}"
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

# fleet_web_tier_wait_active SERVICE — 0 once SERVICE is active, polling for
# up to FLEET_WEB_TIER_READY_WAIT seconds (default 15). fleet.service is
# Type=notify: "active" means READY=1, sent once every listener (the web UI's
# included) is bound and the databases are migrated.
fleet_web_tier_wait_active() {
  local i
  for ((i = 0; i < ${FLEET_WEB_TIER_READY_WAIT:-15}; i++)); do
    systemctl is-active --quiet "$1" 2>/dev/null && return 0
    sleep 1
  done
  systemctl is-active --quiet "$1" 2>/dev/null
}

# fleet_cutover_web_tier SERVICE SRC_DIR — move the web UI from the retired
# tier to fleet in the one safe order: stop the old unit (it holds fleet's
# port), restart SERVICE, and only once SERVICE is active remove the old
# tier's files. If SERVICE does not come up, the old tier is restarted (when
# this process stopped it) and 1 is returned; the caller must then stop with a
# non-zero exit — never go on to report success or touch the tier again.
fleet_cutover_web_tier() {
  local svc="$1" src="${2:-}"
  fleet_stop_web_tier || true
  if systemctl restart "$svc" >/dev/null 2>&1 && fleet_web_tier_wait_active "$svc"; then
    fleet_remove_web_tier "$src" || true
    return 0
  fi
  fleet_restore_web_tier
  return 1
}
