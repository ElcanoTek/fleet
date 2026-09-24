# shellcheck shell=bash
# ^ no shebang: this file is only ever sourced, never executed.
# scripts/lib/podman-migrate.sh — when may doctor run `podman system migrate`?
#
# Sourced by scripts/doctor.sh. The decisions are pure functions of what the
# caller probed, so internal/admincli/scripts_podman_migrate_test.go can pin
# every branch without root, podman or a broken rootless store.
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
#   precedence and normalization: FLEET_SANDBOX_BACKEND (trimmed, lowercased),
#   else the bundle's sandbox.backend, else podman. An unrecognized value
#   echoes as-is (the daemon refuses to boot on it, so no pool is live);
#   callers treat everything but "kubernetes" as the podman store.
resolve_sandbox_backend() {
  local raw
  raw="$(tr -d '[:space:]' <<<"$1" | tr '[:upper:]' '[:lower:]')"
  [[ -z "$raw" ]] && raw="$(tr -d '[:space:]' <<<"$2" | tr '[:upper:]' '[:lower:]')"
  echo "${raw:-podman}"
}

# podman_migrate_plan BACKEND INFO_OK INFO_ERR LIVE_CONTAINERS FLEET_LIVE CAN_RESTART
#   Step 3's decision. INFO_OK is 1 when `podman info` succeeded as the service
#   user, INFO_ERR its stderr; LIVE_CONTAINERS the count of the user's running
#   containers ("unknown" when the listing failed); FLEET_LIVE 1 when a fleet
#   process may hold a pool; CAN_RESTART 1 when this run may AND can restart
#   the service right after (not --no-restart, a systemd-managed unit).
#   Echoes one of:
#     migrate          nothing live to stop (or the kubernetes backend, whose
#                      sandboxes are pods, not this store's containers)
#     defer            podman healthy but something is live — skip; the
#                      step-8 smoke decides whether a reset is needed
#     migrate-restart  stale pause while live: the pool is broken already, so
#                      migrate and restart the service to rebuild it
#     refuse           stale pause while live, but no restart is possible —
#                      migrating would leave the process holding dead handles
#     none             podman failing some other way; migrate is not the fix
podman_migrate_plan() {
  local backend="$1" info_ok="$2" info_err="$3" live="$4" fleet_live="$5" can_restart="$6"
  if [[ "$backend" == "kubernetes" ]]; then
    echo migrate
  elif [[ "$info_ok" == "1" ]]; then
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
#   stale-pause error on the podman backend with a restart available; else
#   "report" (fail the smoke, leave the live pool alone).
smoke_retry_plan() {
  local deferred="$1" can_restart="$2" backend="$3" smoke_err="$4"
  if [[ "$deferred" == "1" && "$can_restart" == "1" && "$backend" != "kubernetes" ]] \
     && is_stale_pause_error "$smoke_err"; then
    echo retry
  else
    echo report
  fi
}
