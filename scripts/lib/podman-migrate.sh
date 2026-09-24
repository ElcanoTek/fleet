# shellcheck shell=bash
# ^ no shebang: this file is only ever sourced, never executed.
# scripts/lib/podman-migrate.sh — when may doctor run `podman system migrate`?
#
# Sourced by scripts/doctor.sh. The decisions are pure functions of what the
# caller probed, so internal/admincli/scripts_podman_migrate_test.go can pin
# every branch without root, podman or a broken rootless store. The one
# action, migrate_live_service, uses the caller's SERVICE_NAME, SERVICE_USER,
# run_as_fleet and fixed/fail reporters (and systemctl, pgrep), which the test
# stubs.
#
# Why this needs a gate at all: migrate is the documented reset for a stale
# rootless pause process (one forked in an old mount namespace pins it and
# poisons later pulls/runs), but it is NOT a no-op on a live box. It stops
# every running container of the user, and fleet's sandboxes run --rm, so it
# deletes the running service's whole warm pool while the process keeps
# handing out the dead handles: "no such container" on every chat turn and
# task until a restart. Learned in production on fleetdev.

# is_stale_pause_error TEXT — true when podman's own error names the condition
# migrate resets: a rootless pause process that is gone or was forked in an
# old namespace. podman says so itself ('invalid internal status, try
# resetting the pause process with "podman system migrate"'). The destructive
# reset keys on that signature and nothing broader: a launch that fails for
# disk or PID exhaustion leaves the live pool serving, and migrating then
# would turn a degraded box into a full tool outage.
is_stale_pause_error() {
  grep -qiE 'podman system migrate|pause process' <<<"$1"
}

# resolve_sandbox_backend ENV_VALUE MANIFEST_VALUE
#   The sandbox backend the daemon will run, with sandbox.ResolveBackend's
#   precedence and normalization: FLEET_SANDBOX_BACKEND (ends trimmed, lowercased),
#   else the bundle's sandbox.backend, else podman. An unrecognized value
#   echoes as-is (the daemon refuses to boot on it, so no pool is live).
#   The backend only ever RESTRICTS what doctor does (step 8 never restarts a
#   kubernetes control plane over a local podman fault); it never licenses a
#   migrate. So a backend doctor misreads can cost a skipped repair or an
#   unneeded restart, but never a deleted pool.
resolve_sandbox_backend() {
  local raw
  raw="$(_trim_lower "$1")"
  [[ -z "$raw" ]] && raw="$(_trim_lower "$2")"
  echo "${raw:-podman}"
}

# _trim_lower TEXT — strings.ToLower(strings.TrimSpace(TEXT)): the ENDS only.
# Internal whitespace is kept, so a drifted "pod man" stays an unknown value
# (which restricts) instead of becoming a valid podman.
_trim_lower() {
  local v="$1"
  v="${v#"${v%%[![:space:]]*}"}"
  v="${v%"${v##*[![:space:]]}"}"
  printf '%s' "${v,,}"
}

# unit_proven_stopped ACTIVE_STATE — true only for a unit systemd reports as
# exactly inactive or failed. A negative `is-active` is NOT a stop: it also
# covers "deactivating" (still running) and "activating" — including the
# Restart=always auto-restart delay, where systemd is about to start fleet
# again. Both liveness and the helper's stop check use this, so a migrate is
# never taken on a unit that is merely between runs. An empty state (no
# systemd, or no such unit) is left to the caller's process check.
unit_proven_stopped() {
  [[ "$1" == "inactive" || "$1" == "failed" ]]
}

# fleet_is_live — true when a fleet process may hold a warm sandbox pool: the
# unit is not proven stopped (unit_proven_stopped), or a `fleet` process runs
# as the service user (the unit's
# ExecStart, or the same binary under another supervisor). Errs toward live:
# a false "live" only skips a reset, a false "not live" deletes a pool.
fleet_is_live() {
  local state
  if command -v systemctl >/dev/null 2>&1; then
    # Live unless PROVEN stopped: "activating" (the Restart=always delay)
    # and "deactivating" count, since systemd is about to run fleet again or
    # still is. A missing unit reports inactive.
    state="$(systemctl show -p ActiveState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
    unit_proven_stopped "${state:-inactive}" || return 0
  fi
  pgrep -u "$SERVICE_USER" -x fleet >/dev/null 2>&1
}

# unit_restartable — true when doctor may stop and start the fleet UNIT to
# rebuild the pool: the unit is loaded AND is the live one (not proven
# stopped — so "activating", the Restart=always delay, counts; a unit stuck
# restarting on a stale store is exactly the one to repair). An installed but
# inactive unit is excluded even when a fleet process is live: that process
# belongs to another supervisor, and starting the unit would run a second
# fleet. Callers add their own --no-restart check.
unit_restartable() {
  local load state
  load="$(systemctl show -p LoadState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
  state="$(systemctl show -p ActiveState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
  [[ "$load" == "loaded" ]] && ! unit_proven_stopped "$state"
}

# podman_migrate_plan BACKEND INFO_OK INFO_ERR LIVE_CONTAINERS FLEET_LIVE CAN_RESTART
#   Step 3's decision. BACKEND is accepted for symmetry with smoke_retry_plan
#   but deliberately NOT consulted: liveness is the gate on every backend, so
#   a misread backend can never turn into a migrate under a live pool. INFO_OK is 1 when `podman info` succeeded as the service
#   user, INFO_ERR its stderr; LIVE_CONTAINERS the count of the user's running
#   containers ("unknown" when the listing failed); FLEET_LIVE 1 when a fleet
#   process may hold a pool; CAN_RESTART 1 when this run may AND can restart
#   the service right after (not --no-restart, a systemd-managed unit).
#   Echoes one of:
#     migrate          nothing live to stop
#     defer            podman healthy but something is live — skip; the
#                      step-8 smoke decides whether a reset is needed
#     migrate-restart  stale pause while live: the pool is broken already, so
#                      migrate and restart the service to rebuild it
#     refuse           stale pause while live, but no restart is possible —
#                      migrating would leave the process holding dead handles
#     none             podman failing some other way; migrate is not the fix
podman_migrate_plan() {
  local info_ok="$2" info_err="$3" live="$4" fleet_live="$5" can_restart="$6"
  if [[ "$info_ok" == "1" ]]; then
    # A listing that failed proves nothing is safe to stop: count it as live.
    if [[ "$live" != "0" || "$fleet_live" == "1" ]]; then echo defer; else echo migrate; fi
  elif is_stale_pause_error "$info_err"; then
    if [[ "$fleet_live" != "1" ]]; then echo migrate
    elif [[ "$can_restart" == "1" ]]; then echo migrate-restart
    else echo refuse
    fi
  else
    echo none
  fi
}

# smoke_retry_plan DEFERRED CAN_RESTART BACKEND SMOKE_ERR
#   Step 8's decision after the sandbox smoke failed. DEFERRED is 1 when step 3
#   skipped migrate; CAN_RESTART as above; SMOKE_ERR the failed run's stderr.
#   Echoes "retry" (migrate, restart the service, re-smoke) only for podman's
#   stale-pause error on a backend resolved as EXACTLY "podman", with a
#   restart available; else "report" (fail the smoke, leave the live pool
#   alone). Fail-closed on the backend: kubernetes, an unrecognized value, or
#   a manifest expression doctor could not interpolate all restrict — doctor
#   need not mirror every interpolation form to stay safe.
smoke_retry_plan() {
  local deferred="$1" can_restart="$2" backend="$3" smoke_err="$4"
  if [[ "$deferred" == "1" && "$can_restart" == "1" && "$backend" == "podman" ]] \
     && is_stale_pause_error "$smoke_err"; then
    echo retry
  else
    echo report
  fi
}

# migrate_live_service — `podman system migrate` under a live, systemd-managed
# fleet, as ONE stop → migrate → start. Stopping first means no process ever
# holds handles to containers migrate deleted: not for the steps between a
# migrate and a later restart, and not after a run interrupted there (a
# stopped unit is visible to every health check; a live one with a dead pool
# answers /healthz while failing every tool call). fleet-web has
# BindsTo=fleet.service, so the stop takes it down and starting fleet does not
# bring it back — it is started again here when it was running before.
# The gate is the unit's STATE after the stop, not the stop job's exit code,
# and it must be PROVEN stopped: ActiveState exactly inactive or failed (a
# negative is-active also covers "deactivating", a unit still running) and no
# `fleet` process left for the service user. Anything else aborts — migrating
# under a live process is the exact deletion this helper exists to prevent. A
# stop that "failed" (a timeout after systemd killed it) but left the unit
# proven-stopped proceeds, so fleet is migrated AND started again rather than
# left down. /run/<service user> is the unit's RuntimeDirectory=, which
# systemd REMOVES on stop, and it is the XDG_RUNTIME_DIR every podman call
# needs — so it is recreated (as step 3 does) before migrate, else migrate
# fails with "lstat /run/fleet: no such file or directory" (found on fleetdev;
# systemd takes the directory over again on start). Whatever happens, it ends by starting fleet (and fleet-web, if it
# was running) again — an abort included, since its stop was already issued.
# Reports its own failures (callers report only success) and returns non-zero
# when migrate did not run, migrate itself failed, or fleet did not come back.
migrate_live_service() {
  local web_was_active=0 rc=0 state migrate_err
  systemctl is-active --quiet fleet-web.service 2>/dev/null && web_was_active=1
  # From the stop on, an interruption (Ctrl-C, SIGTERM from `fleet doctor`,
  # a dropped SSH session) must still bring fleet back: nothing else would.
  # bash runs the trap once the current foreground command returns.
  # shellcheck disable=SC2064 # expand web_was_active now, not at signal time
  trap "_migrate_live_restore $web_was_active; trap - INT TERM HUP; exit 130" INT TERM HUP
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  state="$(systemctl show -p ActiveState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
  if ! unit_proven_stopped "$state" \
     || pgrep -u "$SERVICE_USER" -x fleet >/dev/null 2>&1; then
    fail "${SERVICE_NAME}.service did not stop (ActiveState=${state:-unknown}) — podman system migrate NOT run (it would delete the live sandbox pool); journalctl -u ${SERVICE_NAME} -n 50"
    rc=1
  elif ! install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "/run/${SERVICE_USER}" 2>/dev/null; then
    fail "could not recreate /run/${SERVICE_USER} — podman system migrate NOT run"
    rc=1
  elif ! migrate_err="$(run_as_fleet podman system migrate 2>&1 >/dev/null)"; then
    # Keep podman's own diagnostic; the restore below still runs.
    fail "podman system migrate failed as $SERVICE_USER: ${migrate_err##*$'\n'}"
    rc=1
  fi
  # Every path restores the service, including an aborted one: the stop was
  # already issued and may still complete, and nothing after doctor's step 6
  # would start fleet again. start waits out a pending stop, and is a no-op on
  # a unit that never went down.
  _migrate_live_restore "$web_was_active" || rc=1
  trap - INT TERM HUP
  return "$rc"
}

# _migrate_live_restore WEB_WAS_ACTIVE — start fleet, and fleet-web when it
# was running (BindsTo: stopping fleet took it down; starting fleet does not
# bring it back). Shared by the normal path and the interruption trap.
_migrate_live_restore() {
  local rc=0
  if ! systemctl start "${SERVICE_NAME}.service" 2>/dev/null; then
    fail "${SERVICE_NAME}.service did not start again — journalctl -u ${SERVICE_NAME} -n 50"
    rc=1
  fi
  if [[ "$1" == "1" ]] && ! systemctl is-active --quiet fleet-web.service 2>/dev/null; then
    if systemctl start fleet-web.service 2>/dev/null && systemctl is-active --quiet fleet-web.service 2>/dev/null; then
      fixed "fleet-web.service started again after the ${SERVICE_NAME} restart"
    else
      fail "fleet-web.service is down after the ${SERVICE_NAME} restart — journalctl -u fleet-web -n 50"
    fi
  fi
  return "$rc"
}
