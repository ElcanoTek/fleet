#!/usr/bin/env bash
# scripts/fleet-upgrade.sh — drain-and-restart upgrade for an existing fleet install.
#
# A safer companion to scripts/update.sh for production boxes. update.sh pulls,
# builds, installs, and `systemctl restart`s in one shot; this script wraps that
# same install+restart with two operator-grade guarantees update.sh does NOT give:
#
#   1. BACKUP + AUTO-ROLLBACK. The live fleet + fleet-admin binaries are copied
#      aside BEFORE the new ones are installed. If the new process fails to pass
#      /readyz after restart, this script reinstalls the backups and restarts —
#      so a bad build self-heals to the last-known-good binary instead of leaving
#      the box in a crash loop.
#
#   2. A REAL READINESS GATE. After the restart it polls the new process's
#      /readyz probe (the same one a load balancer / systemd watchdog uses: DB +
#      sandbox reachable AND not draining) before declaring success — not just
#      `systemctl is-active`, which goes green the instant the binary execs,
#      before it can actually serve.
#
# The DRAIN itself is NOT implemented here: it already lives in the binary.
# `systemctl restart` sends SIGTERM, and cmd/fleet handles SIGTERM gracefully —
# it stops admitting new work (/healthz + /readyz → 503 so a load balancer drains
# it), lets in-flight chat turns AND scheduled tasks finish within
# FLEET_SHUTDOWN_GRACE_SECONDS (default 30s), then force-cancels stragglers and
# exits 0. Under Type=notify the restart blocks until the NEW process emits
# READY=1 (both listeners bound) or TimeoutStartSec elapses. This script's job is
# to gate + roll back AROUND that built-in drain, not to reimplement it.
#
# ── Honesty: is this zero-downtime? ──────────────────────────────────────────
# No — it is "zero-downtime-ish" / brief-blip. fleet is a SINGLE process on ONE
# box (no rolling fleet of replicas behind a load balancer), so there is an
# unavoidable window — from when the old process finishes draining + exits until
# the new one binds its listeners and passes /readyz — during which new requests
# get a 503 (while draining) or a connection refusal (during the swap). What IS
# graceful: in-flight work is DRAINED, not killed — active chat turns and running
# scheduled tasks finish (up to the grace budget) instead of being interrupted,
# and the binary self-rolls-back if the new version can't serve. For true
# zero-downtime you would need a second instance + a front proxy that fails over;
# that is out of scope for the single-big-box deployment posture (see README
# "Deploy").
#
# Invoke directly on the host. It does NOT pull (run `git pull` /
# `scripts/update.sh` first, or pass --no-build to upgrade to the already-checked-
# out source); it builds from the current checkout and swaps the live binaries.
#
# Flags / env (flags win over env):
#   --src <dir>           fleet source checkout       (env SRC_DIR, default this repo)
#   --service <name>      systemd unit to restart     (env FLEET_SERVICE_NAME, default fleet)
#   --install-dir <dir>   where the live binaries live (env FLEET_INSTALL_DIR; else
#                         the unit's ExecStart dir, else /opt/fleet)
#   --health-url <url>    readiness probe to gate on  (env FLEET_HEALTH_URL; else
#                         derived from FLEET_SERVER_ADDR, default
#                         http://127.0.0.1:8080/readyz)
#   --health-timeout <s>  seconds to wait for /readyz after restart (env
#                         FLEET_HEALTH_TIMEOUT, default 90)
#   --no-build            skip `make build`; swap the binaries already in --src
#   --no-rollback         do NOT auto-restore the backup on a failed health gate
#                         (leave the new binary in place for debugging)
#   --yes / -y            skip the confirm prompt    (env FLEET_UPGRADE_YES=1)
#   --dry-run             print the plan; build/install/restart nothing
#   -h | --help           this help
#
# Exit codes: 0 upgraded + healthy · 1 aborted/failed (rolled back unless
# --no-rollback) · 2 usage.

set -euo pipefail

# ── locate this script + its repo root (default SRC_DIR) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
INSTALL_DIR="${FLEET_INSTALL_DIR:-}"
HEALTH_URL="${FLEET_HEALTH_URL:-}"
HEALTH_TIMEOUT="${FLEET_HEALTH_TIMEOUT:-90}"
NO_BUILD=0
NO_ROLLBACK=0
ASSUME_YES="${FLEET_UPGRADE_YES:-0}"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src)             shift; [[ $# -gt 0 ]] || { echo "error: --src needs a dir" >&2; exit 2; }; SRC_DIR="$1" ;;
    --src=*)           SRC_DIR="${1#*=}" ;;
    --service)         shift; [[ $# -gt 0 ]] || { echo "error: --service needs a name" >&2; exit 2; }; SERVICE_NAME="$1" ;;
    --service=*)       SERVICE_NAME="${1#*=}" ;;
    --install-dir)     shift; [[ $# -gt 0 ]] || { echo "error: --install-dir needs a dir" >&2; exit 2; }; INSTALL_DIR="$1" ;;
    --install-dir=*)   INSTALL_DIR="${1#*=}" ;;
    --health-url)      shift; [[ $# -gt 0 ]] || { echo "error: --health-url needs a url" >&2; exit 2; }; HEALTH_URL="$1" ;;
    --health-url=*)    HEALTH_URL="${1#*=}" ;;
    --health-timeout)  shift; [[ $# -gt 0 ]] || { echo "error: --health-timeout needs seconds" >&2; exit 2; }; HEALTH_TIMEOUT="$1" ;;
    --health-timeout=*) HEALTH_TIMEOUT="${1#*=}" ;;
    --no-build)        NO_BUILD=1 ;;
    --no-rollback)     NO_ROLLBACK=1 ;;
    --yes|-y)          ASSUME_YES=1 ;;
    --dry-run)         DRY_RUN=1 ;;
    -h|--help)         awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m'; c_dim=$'\033[2m'; c_red=$'\033[0;31m'
  c_green=$'\033[0;32m'; c_yellow=$'\033[0;33m'; c_cyan=$'\033[0;36m'; c_bold=$'\033[1m'
else
  c_reset=''; c_dim=''; c_red=''; c_green=''; c_yellow=''; c_cyan=''; c_bold=''
fi
say()  { printf '%s\n' "$*"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

# require_go_toolchain — same guard as update.sh, and for the same reason: the
# Makefile exports GOTOOLCHAIN=auto so go.mod's exact pinned patch release is
# fetched rather than required on the box, but a Go older than 1.21 predates
# GOTOOLCHAIN and cannot do that fetch. Name that case instead of letting
# `make build` fail on a raw version-mismatch line.
require_go_toolchain() {
  command -v go >/dev/null 2>&1 \
    || die "go not found on PATH — install Go (1.21 or newer) and re-run; the build fetches the exact pinned toolchain itself, so the distro's version does not have to match go.mod"
  local goversion minor pinned
  goversion="$(go env GOVERSION 2>/dev/null || true)"   # e.g. "go1.26.2"
  [[ "$goversion" == go1.* ]] || return 0
  minor="${goversion#go1.}"; minor="${minor%%.*}"
  [[ "$minor" =~ ^[0-9]+$ ]] || return 0
  if (( minor < 21 )); then
    pinned="$(awk '/^go /{print $2; exit}' "$SRC_DIR/go.mod" 2>/dev/null || true)"
    die "installed Go is ${goversion}, which predates GOTOOLCHAIN (added in 1.21) and so cannot fetch the pinned toolchain${pinned:+ (go.mod pins $pinned)} — upgrade Go and re-run; live binaries left in place"
  fi
}
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }

[[ -d "$SRC_DIR" ]] || die "fleet source dir not found: $SRC_DIR"

# ── resolve the readiness URL from FLEET_SERVER_ADDR when not given ──────────
# cmd/fleet serves /readyz on the chat listener (default 127.0.0.1:8080). Mirror
# addrOr(cfg.Addr, ":8080"): a bare ":8080" form binds all interfaces, so we
# probe loopback. /readyz is the right gate — it is 503 while draining AND fails
# if a critical DB pool is down, so a green /readyz means the NEW process can
# actually serve, not merely that the binary exec'd (which `systemctl is-active`
# would report instantly).
if [[ -z "$HEALTH_URL" ]]; then
  addr="${FLEET_SERVER_ADDR:-127.0.0.1:8080}"
  case "$addr" in
    :*) addr="127.0.0.1${addr}" ;;        # ":8080" → "127.0.0.1:8080"
    0.0.0.0:*) addr="127.0.0.1:${addr##*:}" ;;
  esac
  HEALTH_URL="http://${addr}/readyz"
fi

step "fleet upgrade (src=${SRC_DIR}, service=${SERVICE_NAME}, health=${HEALTH_URL}, dry-run=${DRY_RUN})"
info "this drains in-flight work (SIGTERM → graceful, inside the binary), swaps the"
info "binaries, restarts, and gates on /readyz — rolling back the binary on failure."
warn "NOT zero-downtime: a single-process box has a brief request blip during the swap."

# ── confirm ──────────────────────────────────────────────────────────────────
if [[ "$ASSUME_YES" != "1" && "$DRY_RUN" != "1" && -t 0 ]]; then
  printf '%s?%s Drain + restart %s%s%s now? %s(y/N)%s ' \
    "$c_cyan" "$c_reset" "$c_bold" "$SERVICE_NAME" "$c_reset" "$c_dim" "$c_reset"
  read -r answer
  case "${answer,,}" in
    y|yes) ;;
    *) warn "cancelled"; exit 1 ;;
  esac
fi

# ── 1. build the new binaries (unless --no-build) ────────────────────────────
step "1/5  Building fleet + fleet-admin"
if [[ "$NO_BUILD" == "1" ]]; then
  info "--no-build: using the binaries already in ${SRC_DIR}"
  if [[ "$DRY_RUN" != "1" && ( ! -x "$SRC_DIR/fleet" || ! -x "$SRC_DIR/fleet-admin" ) ]]; then
    die "--no-build but ${SRC_DIR}/fleet or fleet-admin is missing — build first (make build)"
  fi
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: (cd ${SRC_DIR} && make build)  → ${SRC_DIR}/fleet + fleet-admin"
else
  require_go_toolchain
  ( cd "$SRC_DIR" && make build ) || die "make build failed — live binaries left in place"
  [[ -x "$SRC_DIR/fleet" && -x "$SRC_DIR/fleet-admin" ]] \
    || die "make build did not emit ${SRC_DIR}/fleet + ${SRC_DIR}/fleet-admin"
  ok "built fleet + fleet-admin"
fi

# ── resolve INSTALL_DIR (the live binaries' location) ────────────────────────
# Same resolution order as update.sh: --install-dir / $FLEET_INSTALL_DIR, else the
# dir of the unit's ExecStart, else /opt/fleet. The new binaries are installed
# here so the restart actually runs the new code.
if [[ -z "$INSTALL_DIR" ]]; then
  if command -v systemctl >/dev/null 2>&1; then
    # `systemctl show -p ExecStart --value` prints an exec-command struct
    # ("{ path=/opt/fleet/fleet ; argv[]=... }"), NOT a bare path — extract
    # the path= field. The old `awk '{print $1}'` grabbed the literal "{" and
    # resolved INSTALL_DIR to the operator's cwd, so the swap (and therefore
    # the backup/rollback guarantee) never touched /opt/fleet.
    # `|| true`: this whole pipeline is best-effort probing, not a check that
    # must succeed. Under `set -e` + `pipefail` a non-zero exit anywhere in it
    # aborts the upgrade — and `systemctl show` exits non-zero whenever it can
    # not reach systemd, which is every container where the systemctl binary
    # exists but PID 1 is not systemd (`head -n1` can also SIGPIPE it). The
    # fallback below already handles "no path found"; swallowing the status is
    # what lets it be reached instead of dying two steps into the upgrade.
    exec_start="$(systemctl show -p ExecStart --value "${SERVICE_NAME}.service" 2>/dev/null \
      | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n1 || true)"
  else
    exec_start=""
  fi
  if [[ "$exec_start" == /* && -x "$(dirname "$exec_start")" ]]; then
    INSTALL_DIR="$(dirname "$exec_start")"
  else
    INSTALL_DIR="/opt/fleet"
  fi
fi
info "live binaries: ${INSTALL_DIR}/{fleet,fleet-admin}"

# Detect the in-place dev case (source == install dir): there is nothing to back
# up or swap, so rollback is impossible. update.sh skips the copy here too.
IN_PLACE=0
if [[ "$(cd "$INSTALL_DIR" 2>/dev/null && pwd || echo "$INSTALL_DIR")" == "$(cd "$SRC_DIR" 2>/dev/null && pwd || echo "$SRC_DIR")" ]]; then
  IN_PLACE=1
fi

# ── 2. back up the live binaries (for rollback) ──────────────────────────────
step "2/5  Backing up the live binaries"
BACKUP_DIR=""
if [[ "$IN_PLACE" == "1" ]]; then
  info "install dir == source checkout (${SRC_DIR}) — running in place; no backup/swap."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would copy ${INSTALL_DIR}/{fleet,fleet-admin} → ${INSTALL_DIR}/.fleet-upgrade-backup/"
elif [[ -x "$INSTALL_DIR/fleet" ]]; then
  BACKUP_DIR="$INSTALL_DIR/.fleet-upgrade-backup"
  install -d -m 0755 "$BACKUP_DIR" || die "cannot create backup dir ${BACKUP_DIR} (need root? set --install-dir)"
  cp -p "$INSTALL_DIR/fleet" "$BACKUP_DIR/fleet" || die "cannot back up ${INSTALL_DIR}/fleet"
  [[ -x "$INSTALL_DIR/fleet-admin" ]] && cp -p "$INSTALL_DIR/fleet-admin" "$BACKUP_DIR/fleet-admin"
  ok "backed up live binaries → ${BACKUP_DIR}"
else
  warn "no existing ${INSTALL_DIR}/fleet to back up — proceeding, but rollback will be unavailable."
fi

# ── 3. install the new binaries ──────────────────────────────────────────────
step "3/5  Installing the new binaries"
if [[ "$IN_PLACE" == "1" ]]; then
  info "running in place — the freshly built binaries are already at ${INSTALL_DIR}."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would install ${SRC_DIR}/{fleet,fleet-admin} → ${INSTALL_DIR}/ (0755)"
elif install -D -m 0755 "$SRC_DIR/fleet" "$INSTALL_DIR/fleet" \
     && install -D -m 0755 "$SRC_DIR/fleet-admin" "$INSTALL_DIR/fleet-admin"; then
  ok "installed new binaries → ${INSTALL_DIR}"
else
  die "could not install binaries into ${INSTALL_DIR} (need root? set --install-dir)"
fi

# ── restart helper (used for the upgrade restart AND any rollback restart) ───
restart_service() {
  systemctl restart "$SERVICE_NAME"
}

# restart_web_tier — bring fleet-web back after a backend restart, and assert it.
#
# deploy/fleet-web.service carries `BindsTo=fleet.service`, so `systemctl
# restart fleet` STOPS the web tier and systemd does not start it again on its
# own. scripts/update.sh has restarted it explicitly for exactly this reason
# since #654; this script did not — it restarted the backend, gated on the
# BACKEND's /readyz, and then printed "fleet upgraded + healthy" while the web
# tier was down. The readiness gate never covered fleet-web and still does not,
# so the claim has to come from the unit's own resolved state.
#
# WEB_TIER_UP records what was actually observed, so the final banner can say
# what it verified instead of asserting a health it never measured. A missing
# unit is not a failure: plenty of boxes run the backend alone.
WEB_TIER_UP="n/a"
restart_web_tier() {
  command -v systemctl >/dev/null 2>&1 || return 0
  systemctl cat fleet-web.service >/dev/null 2>&1 || return 0
  if ! systemctl restart fleet-web 2>/dev/null; then
    WEB_TIER_UP="no"
    warn "could not restart fleet-web (BindsTo=${SERVICE_NAME}.service stopped it) — check: journalctl -u fleet-web -n 50"
    return 0
  fi
  # Read the resolved state back rather than trusting the restart's exit code —
  # a unit can accept the restart and then fail its ExecStart.
  local i
  for i in 1 2 3 4 5 6 7 8; do
    if [[ "$(systemctl is-active fleet-web 2>/dev/null || true)" == "active" ]]; then
      WEB_TIER_UP="yes"; ok "fleet-web is active again (systemctl is-active)"; return 0
    fi
    sleep 1
  done
  WEB_TIER_UP="no"
  warn "fleet-web did not come back active within 8s — check: journalctl -u fleet-web -n 50"
}

# ── rollback helper: restore the backup binaries + restart ───────────────────
rollback() {
  if [[ "$NO_ROLLBACK" == "1" ]]; then
    warn "--no-rollback: leaving the new (failing) binary in place for debugging."
    warn "restore manually: install -m0755 ${BACKUP_DIR:-<backup>}/fleet ${INSTALL_DIR}/fleet && systemctl restart ${SERVICE_NAME}"
    return
  fi
  if [[ -z "$BACKUP_DIR" || ! -x "$BACKUP_DIR/fleet" ]]; then
    warn "no backup binary available — cannot auto-roll-back. Investigate: journalctl -u ${SERVICE_NAME} -n 50"
    return
  fi
  step "Rolling back to the previous binary (${BACKUP_DIR})"
  if install -m 0755 "$BACKUP_DIR/fleet" "$INSTALL_DIR/fleet" \
     && { [[ ! -x "$BACKUP_DIR/fleet-admin" ]] || install -m 0755 "$BACKUP_DIR/fleet-admin" "$INSTALL_DIR/fleet-admin"; }; then
    ok "restored previous binaries"
  else
    warn "could not restore the backup binaries — restore manually from ${BACKUP_DIR}"
    return
  fi
  if restart_service; then
    ok "restarted ${SERVICE_NAME} on the previous binary"
    restart_web_tier   # the rollback restart stopped the BindsTo'd web tier too
  else
    warn "rollback restart failed — journalctl -u ${SERVICE_NAME} -n 50"
  fi
}

# ── 4. restart (SIGTERM → graceful drain happens inside the binary) ──────────
step "4/5  Restarting ${SERVICE_NAME} (drains in-flight work on SIGTERM)"
info "the worker pool + chat server drain within FLEET_SHUTDOWN_GRACE_SECONDS (default 30s);"
info "TimeoutStopSec in the unit must exceed that grace so systemd waits out the drain."
if ! command -v systemctl >/dev/null 2>&1; then
  warn "systemctl not found — restart ${SERVICE_NAME} manually (no systemd on this box)."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: systemctl restart ${SERVICE_NAME}  (SIGTERM → graceful drain → exit, then start new)"
elif ! systemctl cat "${SERVICE_NAME}.service" >/dev/null 2>&1; then
  warn "${SERVICE_NAME}.service is not installed — start fleet manually or run scripts/bootstrap.sh --enable-service."
else
  if ! restart_service; then
    warn "systemctl restart ${SERVICE_NAME} failed"
    rollback
    die "restart failed — see journalctl -u ${SERVICE_NAME} -n 50"
  fi
  ok "restart issued (old process drained + exited; new process starting)"
fi

# ── 5. health-gate the NEW process on /readyz; roll back on failure ──────────
step "5/5  Gating on readiness (${HEALTH_URL}, up to ${HEALTH_TIMEOUT}s)"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would poll ${HEALTH_URL} until 2xx (≤ ${HEALTH_TIMEOUT}s), else roll back."
  info "[dry-run] would then restart fleet-web (BindsTo=${SERVICE_NAME}.service stops it) and assert systemctl is-active"
elif ! command -v systemctl >/dev/null 2>&1; then
  info "no systemd — skipping the readiness gate (nothing was restarted by this script)."
elif ! command -v curl >/dev/null 2>&1; then
  warn "curl not found — cannot poll ${HEALTH_URL}. Verify health manually: fleet-admin status"
else
  healthy=0
  start="$(date +%s)"
  while true; do
    # /readyz: 2xx (200/207) = serving. 503 = draining or a critical dep down —
    # keep waiting until the timeout. curl -f makes non-2xx a non-zero exit.
    if curl -fsS -o /dev/null --max-time 5 "$HEALTH_URL" 2>/dev/null; then
      healthy=1; break
    fi
    now="$(date +%s)"
    if (( now - start >= HEALTH_TIMEOUT )); then
      break
    fi
    sleep 2
  done
  if [[ "$healthy" == "1" ]]; then
    ok "new process is ready (${HEALTH_URL} → 2xx)"
    restart_web_tier
  else
    warn "new process did NOT pass ${HEALTH_URL} within ${HEALTH_TIMEOUT}s"
    rollback
    die "upgrade aborted — new version failed the readiness gate (journalctl -u ${SERVICE_NAME} -n 50)"
  fi
fi

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
if [[ "$DRY_RUN" == "1" ]]; then
  printf '%s ✓ fleet upgrade plan printed (dry-run — nothing changed)%s\n' "$c_bold" "$c_reset"
else
  case "$WEB_TIER_UP" in
    yes) printf '%s ✓ fleet upgraded + healthy (backend /readyz 2xx, fleet-web active)%s\n' "$c_bold" "$c_reset" ;;
    n/a) printf '%s ✓ fleet upgraded + healthy (backend /readyz 2xx; no fleet-web unit on this box)%s\n' "$c_bold" "$c_reset" ;;
    # Not "healthy": the backend passed its gate and the web tier did not come
    # back. Saying otherwise is the exact fault this script's own readiness gate
    # exists to prevent, one tier over.
    *)   printf '%s ! fleet backend upgraded + healthy, but fleet-web is NOT active — see the warning above%s\n' "$c_bold" "$c_reset" ;;
  esac
fi
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Health:    ${c_dim}fleet-admin status${c_reset}"
say "  Logs:      ${c_dim}journalctl -u ${SERVICE_NAME} -n 50${c_reset}"
if [[ -n "$BACKUP_DIR" && "$DRY_RUN" != "1" ]]; then
  say "  Roll back: ${c_dim}install -m0755 ${BACKUP_DIR}/fleet ${INSTALL_DIR}/fleet && systemctl restart ${SERVICE_NAME}${c_reset}"
fi
