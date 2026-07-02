#!/usr/bin/env bash
# scripts/update.sh — in-place update for an existing fleet install.
#
# Pulls the fleet checkout AND the client-config bundle checkout, rebuilds the
# fleet binary + the Next web app, rebuilds the sandbox image ONLY when the
# bundle's Containerfile changed, then restarts the systemd unit. Services
# self-migrate on restart, so this script NEVER runs application migrations.
#
# Invoked by `fleet-admin update`, but also runnable directly on the host.
#
# Patterned after moc's + gig's scripts/update.sh, including the "re-exec the
# fresh copy when update.sh itself changed during the pull" trick: bash holds the
# pre-update inode of this file open, so a fix to update.sh would otherwise only
# take effect on the NEXT update. When the pull changes update.sh we re-exec the
# new copy in rebuild-only mode.
#
# Flags / env (flags win over env):
#   --src <dir>            fleet source checkout   (env SRC_DIR, default this repo)
#   --client-config <dir>  client bundle checkout  (env FLEET_CLIENT_CONFIG_DIR,
#                          default ./config/default — the in-repo generic bundle,
#                          which has no separate checkout to pull, so its pull is
#                          skipped)
#   --service <name>       systemd unit to restart (env FLEET_SERVICE_NAME, default fleet)
#   --pin <sha-or-tag>     advance the client bundle ONLY to this ref instead of
#                          tracking its branch (env FLEET_CLIENT_CONFIG_PIN; else
#                          the pin bootstrap persisted under the state dir). Set
#                          FLEET_CLIENT_CONFIG_VERIFY=1 to verify-tag/-commit the
#                          ref (fail-closed) when a signing key is configured.
#   --no-pull              skip git fetch/ff; just rebuild the current checkout(s)
#   --branch <name>        override the branch fast-forwarded in SRC_DIR (env FLEET_UPDATE_BRANCH)
#   --yes / -y             skip the confirm prompt (env FLEET_UPDATE_YES=1)
#   --dry-run              print the plan; build/restart nothing
#   -h | --help            this help
#
# Re-run safe (idempotent): when nothing changed it exits early; the web/binary
# builds are deterministic from the checkout; the sandbox rebuild is gated on the
# Containerfile hash so the ~2-3min image build is skipped when unchanged.

set -euo pipefail

# ── locate this script + its repo root (default SRC_DIR) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
CLIENT_DIR="${FLEET_CLIENT_CONFIG_DIR:-$REPO_ROOT/config/default}"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
# Where the running unit's binaries live. Resolved (in order): --install-dir /
# $FLEET_INSTALL_DIR, else the dir of the unit's ExecStart, else /opt/fleet. The
# freshly built $SRC_DIR/{fleet,fleet-admin} are installed here so the restart
# actually runs the new code (a build alone leaves the live ExecStart untouched).
INSTALL_DIR="${FLEET_INSTALL_DIR:-}"
NO_PULL="${FLEET_UPDATE_NO_PULL:-0}"
ASSUME_YES="${FLEET_UPDATE_YES:-0}"
BRANCH_OVERRIDE="${FLEET_UPDATE_BRANCH:-}"
# Client-config bundle pin: an explicit env/flag pin wins; otherwise the pin
# bootstrap persisted under the state dir is used (update.sh does NOT source the
# 0600 env file, so the state file is the durable bootstrap→update channel).
# When set, the bundle checkout advances ONLY to this ref instead of tracking
# the remote default branch. FLEET_CLIENT_CONFIG_VERIFY=1 additionally
# verify-tag/verify-commit the ref (fail-closed) when a signing key is set up.
CLIENT_CONFIG_PIN="${FLEET_CLIENT_CONFIG_PIN:-}"
CLIENT_CONFIG_VERIFY="${FLEET_CLIENT_CONFIG_VERIFY:-}"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src)            shift; [[ $# -gt 0 ]] || { echo "error: --src needs a dir" >&2; exit 1; }; SRC_DIR="$1" ;;
    --src=*)          SRC_DIR="${1#*=}" ;;
    --client-config)  shift; [[ $# -gt 0 ]] || { echo "error: --client-config needs a dir" >&2; exit 1; }; CLIENT_DIR="$1" ;;
    --client-config=*) CLIENT_DIR="${1#*=}" ;;
    --service)        shift; [[ $# -gt 0 ]] || { echo "error: --service needs a name" >&2; exit 1; }; SERVICE_NAME="$1" ;;
    --service=*)      SERVICE_NAME="${1#*=}" ;;
    --install-dir)    shift; [[ $# -gt 0 ]] || { echo "error: --install-dir needs a dir" >&2; exit 1; }; INSTALL_DIR="$1" ;;
    --install-dir=*)  INSTALL_DIR="${1#*=}" ;;
    --branch)         shift; [[ $# -gt 0 ]] || { echo "error: --branch needs a name" >&2; exit 1; }; BRANCH_OVERRIDE="$1" ;;
    --branch=*)       BRANCH_OVERRIDE="${1#*=}" ;;
    --pin)            shift; [[ $# -gt 0 ]] || { echo "error: --pin needs a sha-or-tag" >&2; exit 1; }; CLIENT_CONFIG_PIN="$1" ;;
    --pin=*)          CLIENT_CONFIG_PIN="${1#*=}" ;;
    --no-pull)        NO_PULL=1 ;;
    --yes|-y)         ASSUME_YES=1 ;;
    --dry-run)        DRY_RUN=1 ;;
    -h|--help)        sed -n '2,39p' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

# Fall back to the bootstrap-persisted pin (state file) when none was passed
# explicitly. SRC_DIR is final here, so the state dir resolves the same way it
# did at bootstrap time.
if [[ -z "$CLIENT_CONFIG_PIN" ]]; then
  _pin_file="${FLEET_STATE_DIR:-$SRC_DIR/.fleet-state}/client-config.pin"
  [[ -f "$_pin_file" ]] && CLIENT_CONFIG_PIN="$(tr -d '[:space:]' < "$_pin_file")"
fi

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
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }

[[ -d "$SRC_DIR/.git" ]] || die "no fleet source checkout at $SRC_DIR (run scripts/bootstrap.sh first)"

step "fleet update (src=${SRC_DIR}, client=${CLIENT_DIR}, service=${SERVICE_NAME}, no-pull=${NO_PULL}, dry-run=${DRY_RUN})"

# ── 1. pull the fleet checkout ────────────────────────────────────────────
step "1/5  Updating the fleet checkout"
cd "$SRC_DIR"
git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true

before_sha="$(git rev-parse HEAD)"

if [[ "$NO_PULL" == "1" ]]; then
  after_sha="$before_sha"
  # Restored across a self-re-exec so the final summary still shows the real
  # old → new range (see the re-exec block below).
  before_sha="${FLEET_UPDATE_BASE_SHA:-$before_sha}"
  target_branch="$(git rev-parse --abbrev-ref HEAD)"
  ok "rebuild-only mode — skipping fetch, building ${after_sha:0:12}"
else
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would: git fetch origin && fast-forward the current branch"
    after_sha="$before_sha"
    target_branch="$(git rev-parse --abbrev-ref HEAD)"
  else
    git fetch --quiet origin
    target_branch="${BRANCH_OVERRIDE:-$(git rev-parse --abbrev-ref HEAD)}"
    # Refuse to act on a detached HEAD: we need a named branch both so
    # origin/$target_branch resolves and so the checkout stays reattached.
    [[ "$target_branch" != "HEAD" ]] \
      || die "$SRC_DIR is on a detached HEAD — reattach first: git -C $SRC_DIR checkout main"
    after_sha="$(git rev-parse "origin/$target_branch" 2>/dev/null)" \
      || die "origin/$target_branch not found — did the remote branch get renamed or deleted?"

    if [[ "$before_sha" == "$after_sha" ]]; then
      ok "already on ${after_sha:0:12} — fleet checkout up to date"
    else
      say
      printf '%s  incoming commits:%s\n' "$c_dim" "$c_reset"
      git --no-pager log --oneline --no-decorate "${before_sha}..${after_sha}" | sed 's/^/    /'
      say

      if [[ "$ASSUME_YES" != "1" ]]; then
        count="$(git rev-list --count "${before_sha}..${after_sha}")"
        printf '%s?%s Apply %s%d%s commits — %s..%s? %s(y/N)%s ' \
          "$c_cyan" "$c_reset" "$c_bold" "$count" "$c_reset" \
          "${before_sha:0:12}" "${after_sha:0:12}" "$c_dim" "$c_reset"
        read -r answer
        case "${answer,,}" in
          y|yes) ;;
          *) warn "cancelled"; exit 1 ;;
        esac
      fi

      # Fast-forward the local branch instead of detaching HEAD. For a
      # production checkout this is always a clean ff; for a dev-box checkout,
      # --ff-only refuses on divergence so unpushed commits surface loudly.
      git checkout --quiet "$target_branch" \
        || die "cannot switch to $target_branch — uncommitted changes in $SRC_DIR"
      git merge --ff-only --quiet "$after_sha" \
        || die "cannot fast-forward $target_branch to ${after_sha:0:12} — $SRC_DIR has unpushed commits or diverged; push/reset first"

      # The shell running this script read the PRE-update file (bash holds the
      # old inode across the checkout above), so a fix to update.sh itself would
      # otherwise only take effect on the NEXT update. If this update changed
      # update.sh, re-exec the fresh copy in rebuild-only mode (which skips the
      # pull, so it won't loop).
      if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
        warn "update.sh changed in this update — re-executing the new version"
        exec env FLEET_UPDATE_NO_PULL=1 FLEET_UPDATE_YES=1 \
          FLEET_UPDATE_BASE_SHA="$before_sha" \
          FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" \
          FLEET_CLIENT_CONFIG_PIN="$CLIENT_CONFIG_PIN" \
          FLEET_CLIENT_CONFIG_VERIFY="$CLIENT_CONFIG_VERIFY" \
          FLEET_SERVICE_NAME="$SERVICE_NAME" \
          FLEET_INSTALL_DIR="$INSTALL_DIR" \
          FLEET_UPDATE_BRANCH="$BRANCH_OVERRIDE" \
          bash "$SRC_DIR/scripts/update.sh"
      fi
    fi
  fi
fi

# ── 2. pull the client-config bundle checkout ─────────────────────────────
step "2/5  Updating the client-config bundle"
if [[ "$CLIENT_DIR" == "$SRC_DIR/config/default" || "$CLIENT_DIR" == "config/default" ]]; then
  info "using the in-repo generic bundle (config/default) — no separate checkout to pull."
elif [[ ! -d "$CLIENT_DIR/.git" ]]; then
  info "client config at ${CLIENT_DIR} is not a git checkout — leaving as-is."
elif [[ "$NO_PULL" == "1" ]]; then
  info "rebuild-only mode — skipping client-config pull."
elif [[ "$DRY_RUN" == "1" ]]; then
  if [[ -n "$CLIENT_CONFIG_PIN" ]]; then
    info "[dry-run] pinned: would git -C ${CLIENT_DIR} fetch --tags && checkout ${CLIENT_CONFIG_PIN}"
  else
    info "[dry-run] would: git -C ${CLIENT_DIR} pull --ff-only"
  fi
elif [[ -n "$CLIENT_CONFIG_PIN" ]]; then
  # Pinned: advance ONLY to the configured ref (a deliberate operator action),
  # never a silent fast-forward to whatever HEAD became.
  git config --global --add safe.directory "$CLIENT_DIR" 2>/dev/null || true
  if ! git -C "$CLIENT_DIR" fetch --quiet --tags origin; then
    warn "git fetch failed in ${CLIENT_DIR} — checking out the pinned ref from the existing objects"
  fi
  if [[ -n "$CLIENT_CONFIG_VERIFY" ]]; then
    # Opt-in supply-chain verification: fail CLOSED if the pinned tag/commit is
    # not validly signed (requires a configured signing key / allowed-signers).
    if git -C "$CLIENT_DIR" verify-tag "$CLIENT_CONFIG_PIN" 2>/dev/null \
      || git -C "$CLIENT_DIR" verify-commit "$CLIENT_CONFIG_PIN" 2>/dev/null; then
      ok "verified signature on pinned ref ${CLIENT_CONFIG_PIN}"
    else
      die "FLEET_CLIENT_CONFIG_VERIFY is set but ${CLIENT_CONFIG_PIN} is not a validly signed tag/commit — refusing to advance the bundle"
    fi
  fi
  if git -C "$CLIENT_DIR" checkout --quiet "$CLIENT_CONFIG_PIN"; then
    ok "client config pinned to ${CLIENT_CONFIG_PIN} (${CLIENT_DIR})"
  else
    warn "could not check out pinned ref ${CLIENT_CONFIG_PIN} in ${CLIENT_DIR} — leaving the existing checkout"
  fi
else
  git config --global --add safe.directory "$CLIENT_DIR" 2>/dev/null || true
  if git -C "$CLIENT_DIR" pull --ff-only --quiet; then
    ok "client config pulled (${CLIENT_DIR})"
  else
    warn "could not fast-forward ${CLIENT_DIR} — leaving the existing checkout"
  fi
fi

# ── record the pre-build sandbox Containerfile hash (for the change gate) ──
# Compare a stored hash of the bundle's Containerfile before/after the pulls so
# the ~2-3min image build only runs when the Containerfile actually changed.
sandbox_cf="$CLIENT_DIR/sandbox/Containerfile"
hash_file() { [[ -f "$1" ]] && sha256sum "$1" | awk '{print $1}' || printf 'absent'; }
STATE_DIR="${FLEET_STATE_DIR:-$SRC_DIR/.fleet-state}"
STAMP_FILE="$STATE_DIR/sandbox-containerfile.sha256"
cf_now="$(hash_file "$sandbox_cf")"
cf_prev="absent"
[[ -f "$STAMP_FILE" ]] && cf_prev="$(cat "$STAMP_FILE")"

# ── 3. build the fleet binary + the web app ───────────────────────────────
step "3/5  Building the fleet binary + web app"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: (cd ${SRC_DIR} && make build)  → ${SRC_DIR}/fleet + fleet-admin"
  info "[dry-run] would install fleet + fleet-admin → ${INSTALL_DIR:-<unit ExecStart dir, else /opt/fleet>}"
  info "[dry-run] would run: (cd ${SRC_DIR}/web && npm ci && npm run build)"
else
  ( cd "$SRC_DIR" && make build ) || die "make build failed — live binary left in place"
  [[ -x "$SRC_DIR/fleet" && -x "$SRC_DIR/fleet-admin" ]] \
    || die "make build did not emit ${SRC_DIR}/fleet + ${SRC_DIR}/fleet-admin"
  ok "fleet + fleet-admin binaries built"

  # Install the freshly built binaries to the unit's ExecStart location so the
  # restart below actually runs the NEW code. Without this the build is a no-op
  # against the live deployment.
  if [[ -z "$INSTALL_DIR" ]]; then
    exec_start="$(systemctl show -p ExecStart --value "${SERVICE_NAME}.service" 2>/dev/null | awk '{print $1}')"
    if [[ -n "$exec_start" && -x "$(dirname "$exec_start")" ]]; then
      INSTALL_DIR="$(dirname "$exec_start")"
    else
      INSTALL_DIR="/opt/fleet"
    fi
  fi
  # Skip the copy when we'd install onto ourselves (dev box running from $SRC_DIR).
  if [[ "$(cd "$INSTALL_DIR" 2>/dev/null && pwd || echo "$INSTALL_DIR")" == "$SRC_DIR" ]]; then
    info "install dir == source checkout (${SRC_DIR}) — running in place, no copy needed."
  elif install -D -m 0755 "$SRC_DIR/fleet" "$INSTALL_DIR/fleet" 2>/dev/null \
       && install -D -m 0755 "$SRC_DIR/fleet-admin" "$INSTALL_DIR/fleet-admin" 2>/dev/null; then
    ok "installed fleet + fleet-admin → ${INSTALL_DIR}"
  else
    die "could not install binaries into ${INSTALL_DIR} (need root? set --install-dir or FLEET_INSTALL_DIR) — live binary left in place"
  fi

  if [[ -f "$SRC_DIR/web/package.json" ]]; then
    ( cd "$SRC_DIR/web" && npm ci && npm run build ) || die "web build failed"
    ok "web app built"
  else
    warn "no web/package.json under ${SRC_DIR} — skipping web build."
  fi
fi

# ── 4. rebuild the sandbox image ONLY if the Containerfile changed ─────────
step "4/5  Rebuilding the sandbox image (only if the Containerfile changed)"
if [[ "$cf_now" == "absent" ]]; then
  info "no ${sandbox_cf} — bundle ships no sandbox Containerfile (or uses a prebuilt image); skipping."
elif [[ "$cf_now" == "$cf_prev" && "$NO_PULL" != "1" ]]; then
  ok "sandbox Containerfile unchanged (${cf_now:0:12}) — skipping the image build."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} scripts/build-sandbox-image.sh"
  info "[dry-run] would record the new Containerfile hash in ${STAMP_FILE}"
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — skipping sandbox build (install podman, then run scripts/build-sandbox-image.sh)."
else
  if [[ "$cf_prev" == "absent" ]]; then
    info "no stored Containerfile hash yet — building the sandbox image to establish the baseline."
  else
    info "Containerfile changed (${cf_prev:0:12} → ${cf_now:0:12}) — rebuilding the sandbox image."
  fi
  if FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" "$SCRIPT_DIR/build-sandbox-image.sh"; then
    mkdir -p "$STATE_DIR"
    printf '%s\n' "$cf_now" > "$STAMP_FILE"
    ok "sandbox image rebuilt; recorded hash ${cf_now:0:12}"
    # Each rebuild strands the previous image's layers as dangling cruft
    # (~1.3 GB per rebuild); prune them so regular updates can't fill the
    # disk. Dangling-only: any still-tagged image is untouched. Best-effort.
    if podman image prune -f >/dev/null 2>&1; then
      ok "pruned dangling image layers left by the rebuild (fleet cleanup does more)."
    fi
  else
    warn "sandbox image build failed — run scripts/build-sandbox-image.sh manually before restarting."
  fi
fi

# ── 5. restart the service (services self-migrate on start) ────────────────
step "5/5  Restarting the ${SERVICE_NAME} service"
info "application migrations run inside each service on start — update.sh runs none."
if ! command -v systemctl >/dev/null 2>&1; then
  warn "systemctl not found — restart ${SERVICE_NAME} manually (no systemd on this box)."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: systemctl restart ${SERVICE_NAME}"
elif ! systemctl cat "${SERVICE_NAME}.service" >/dev/null 2>&1; then
  warn "${SERVICE_NAME}.service is not installed — start fleet manually or run scripts/bootstrap.sh --enable-service."
else
  systemctl restart "$SERVICE_NAME" || die "systemctl restart ${SERVICE_NAME} failed — journalctl -u ${SERVICE_NAME} -n 50"
  # Brief health check on the unit state.
  healthy=0
  for _ in 1 2 3 4 5 6 7 8; do
    if [[ "$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)" == "active" ]]; then
      healthy=1; break
    fi
    sleep 1
  done
  if [[ "$healthy" == "1" ]]; then
    ok "${SERVICE_NAME} is active"
  else
    die "${SERVICE_NAME} did not come back up — journalctl -u ${SERVICE_NAME} -n 50"
  fi
fi

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
if [[ "$before_sha" == "$after_sha" ]]; then
  printf '%s ✓ fleet rebuilt at %s%s\n' "$c_bold" "${after_sha:0:12}" "$c_reset"
else
  printf '%s ✓ fleet updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
fi
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Health:    ${c_dim}fleet-admin status${c_reset}"
say "  Logs:      ${c_dim}journalctl -u ${SERVICE_NAME} -n 50${c_reset}"
if [[ "$before_sha" != "$after_sha" ]]; then
  say "  Roll back: ${c_dim}cd $SRC_DIR && git checkout $before_sha && scripts/update.sh --no-pull${c_reset}"
fi
