# shellcheck shell=bash
# ^ no shebang: this file is only ever sourced, never executed.
# scripts/lib/podman-migrate.sh — when may doctor run `podman system migrate`?
#
# Sourced by scripts/doctor.sh. The decisions are pure functions of what the
# caller probed, so internal/admincli/scripts_podman_migrate_test.go can pin
# every branch without root, podman or a broken rootless store. The one
# actions (migrate_live_service, migrate_quiesced) use the caller's SERVICE_NAME, SERVICE_USER,
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

# fleet_process_absent — true ONLY when pgrep ran and reported no match (exit
# 1) for a `fleet` process owned by the service user. A missing pgrep, or any
# pgrep error, is not proof of absence: it counts as present, so it can never
# license a migrate. (pgrep: 0 match, 1 no match, 2+ error.)
fleet_process_absent() {
  command -v pgrep >/dev/null 2>&1 || return 1
  local rc=0
  pgrep -u "$SERVICE_USER" -x fleet >/dev/null 2>&1 || rc=$?
  [[ "$rc" == 1 ]]
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
  ! fleet_process_absent
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

# unit_quiesced — true when doctor's own unit is known to be down and nothing
# will bring fleet up behind doctor's back: the unit is loaded, proven
# stopped (inactive/failed — systemd will not auto-restart either), and no
# `fleet` process runs as the service user. A box with no loaded unit is
# never quiesced: whatever supervises fleet there cannot be reserved from
# here, so it could relaunch fleet between this check and a migrate.
unit_quiesced() {
  local load state
  load="$(systemctl show -p LoadState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
  state="$(systemctl show -p ActiveState --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
  [[ "$load" == "loaded" ]] && unit_proven_stopped "$state" && fleet_process_absent
}

# podman_migrate_plan INFO_OK INFO_ERR LIVE_CONTAINERS FLEET_LIVE CAN_RESTART QUIESCED
#   Step 3's decision. INFO_OK is 1 when `podman info` succeeded as the service user,
#   INFO_ERR its stderr; LIVE_CONTAINERS / FLEET_LIVE are informational
#   (the running-container count, "unknown" when listing failed, and
#   fleet_is_live); CAN_RESTART is 1 when this run may and can restart the
#   unit (not --no-restart, unit_restartable); QUIESCED is unit_quiesced.
#   Echoes one of:
#     defer            podman is healthy — there is nothing to reset, so never
#                      migrate here (on any box, live or not: that removes the
#                      check-then-act race with a supervisor doctor cannot
#                      reserve). The step-8 smoke catches a stale pause.
#     migrate          stale pause, and doctor's own unit is quiesced
#     migrate-restart  stale pause while the unit is live: the pool is broken
#                      already, so stop → migrate → start it
#     refuse           stale pause, but doctor controls no unit it could stop
#                      or has proven stopped (--no-restart, another supervisor)
#     none             podman failing some other way; migrate is not the fix
podman_migrate_plan() {
  local info_ok="$1" info_err="$2" can_restart="$5" quiesced="$6"
  if [[ "$info_ok" == "1" ]]; then
    echo defer
  elif is_stale_pause_error "$info_err"; then
    if [[ "$can_restart" == "1" ]]; then echo migrate-restart
    elif [[ "$quiesced" == "1" ]]; then echo migrate
    else echo refuse
    fi
  else
    echo none
  fi
}

# smoke_retry_plan DEFERRED CAN_RESTART LOCAL_POOL SMOKE_ERR QUIESCED
#   Step 8's decision after the sandbox smoke failed. DEFERRED is 1 when step 3
#   skipped migrate; CAN_RESTART / QUIESCED as above; LOCAL_POOL the number of
#   running fleet sandbox containers (chat-sandbox-*) step 3 saw in this
#   store; SMOKE_ERR the failed run's stderr. Only podman's stale-pause error
#   is a reason to reset. Then: "retry" (stop → migrate → start, re-smoke)
#   for a live unit with direct evidence that its pool lives in this store
#   (LOCAL_POOL > 0); "migrate" (reset, re-smoke — no restart) when doctor's
#   unit is quiesced, since nothing live holds a pool; else "report" (fail
#   the smoke, touch nothing). The evidence, not a re-derivation of the
#   daemon's config, is what licenses a restart: a kubernetes-backed fleet
#   keeps no sandbox containers here, so a local podman fault can never
#   restart its control plane — and no config form doctor misreads can.
smoke_retry_plan() {
  local deferred="$1" can_restart="$2" local_pool="$3" smoke_err="$4" quiesced="${5:-0}"
  if [[ "$deferred" != "1" ]] || ! is_stale_pause_error "$smoke_err"; then
    echo report
  elif [[ "$can_restart" == "1" && "$local_pool" =~ ^[1-9][0-9]*$ ]]; then
    echo retry
  elif [[ "$quiesced" == "1" ]]; then
    echo migrate
  else
    echo report
  fi
}

# migrate_quiesced — `podman system migrate` for a box whose unit is
# quiesced (nothing live to disturb). /run/<service user> is the unit's
# RuntimeDirectory=, gone while it is stopped, and podman needs it as
# XDG_RUNTIME_DIR, so it is recreated first. Reports its own failure.
migrate_quiesced() {
  local migrate_err
  if ! install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "/run/${SERVICE_USER}" 2>/dev/null; then
    fail "could not recreate /run/${SERVICE_USER} — podman system migrate NOT run"
    return 1
  fi
  if ! migrate_err="$(run_as_fleet podman system migrate 2>&1 >/dev/null)"; then
    fail "podman system migrate failed as $SERVICE_USER: ${migrate_err##*$'\n'}"
    return 1
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
  if ! unit_proven_stopped "$state" || ! fleet_process_absent; then
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
