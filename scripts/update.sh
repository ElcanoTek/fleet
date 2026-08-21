#!/usr/bin/env bash
# scripts/update.sh — in-place update for an existing fleet install.
#
# Pulls the fleet checkout AND the client-config bundle checkout, rebuilds the
# sandbox image when the bundle's Containerfile or resolved image tag changed,
# the tag is missing from the service user's image store, OR the installed
# image is older than FLEET_SANDBOX_MAX_AGE_DAYS (default 7; 0 disables) — the
# freshness backstop that keeps an unchanged bundle from serving a weeks-old
# image full of unpatched base/package CVEs (skipped entirely when the bundle
# resolves sandbox.image to a prebuilt ref: registry freshness is the
# publisher's pipeline's job, not this box's), rebuilds the
# fleet binary + the Next web app and deploys the web build into the fleet-web
# unit's WorkingDirectory (a build left only in the checkout never reaches the
# browser), then restarts the systemd units (fleet, then fleet-web when
# installed). The sandbox gate runs BEFORE anything is installed on purpose:
# its fail-closed abort (missing image, failed build) must leave the box
# coherent — old binaries on disk, old service running — rather than a
# half-updated box where new code is installed but the update claims nothing
# changed. Services self-migrate on restart, so this script NEVER runs
# application migrations.
#
# Invoked by `fleet-admin update`, but also runnable directly on the host.
#
# Patterned after moc's + gig's scripts/update.sh, including the "re-exec the
# fresh copy when update.sh itself changed during the pull" trick: bash holds the
# pre-update inode of this file open, so a fix to update.sh would otherwise only
# take effect on the NEXT update. When the pull changes update.sh we re-exec the
# new copy with FLEET_UPDATE_REEXEC=1 — the fleet checkout is already
# fast-forwarded, so the re-exec skips only the SRC fetch + self-update detection
# (so it can't loop) and then runs the rest normally, INCLUDING the client-config
# bundle pull. (It is deliberately NOT re-exec'd in --no-pull mode, which would
# also skip the bundle pull — that was a bug where a self-updating run left the
# client bundle stale.)
#
# Flags / env (flags win over env):
#   --src <dir>            fleet source checkout   (env SRC_DIR, default this repo)
#   --client-config <dir>  client bundle checkout  (env FLEET_CLIENT_CONFIG_DIR,
#                          else the dir bootstrap persisted under the state dir,
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
#   --sandbox-max-age <d>  rebuild the sandbox image when the installed one is
#                          <d> or more days old, even if nothing else changed
#                          (env FLEET_SANDBOX_MAX_AGE_DAYS, default 7; 0 disables).
#                          An age-triggered rebuild runs with --no-cache so the
#                          base image AND the package layers actually refresh.
#   --branch <name>        override the branch fast-forwarded in SRC_DIR (env FLEET_UPDATE_BRANCH)
#   --adopt-units          adopt a shipped deploy/*.service that differs
#                          functionally from the installed unit, WITHOUT the
#                          interactive prompt (env FLEET_UPDATE_ADOPT_UNITS=1)
#   --yes / -y             skip the confirm prompt (env FLEET_UPDATE_YES=1)
#   --dry-run              print the plan; build/restart nothing
#   -h | --help            this help
#
# Re-run safe (idempotent): when nothing changed it exits early; the web/binary
# builds are deterministic from the checkout; the sandbox rebuild is gated on the
# Containerfile hash + resolved image tag (and the tag's presence in the service
# user's store) so the ~2-3min image build is skipped when unchanged — except
# the max-age freshness backstop above, which deliberately re-fires once the
# installed image ages past the threshold.

set -euo pipefail

# ── locate this script + its repo root (default SRC_DIR) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
# Client bundle dir: env/flag wins; else the dir bootstrap persisted under the
# state dir (resolved after arg parsing, alongside the pin); else the in-repo
# generic bundle. Without the state-file fallback, an interactive `fleet
# update` on a box whose FLEET_CLIENT_CONFIG_DIR lives only in the 0600
# systemd env file (which this script deliberately does NOT source) would
# silently update against the generic bundle instead of the client checkout.
CLIENT_DIR="${FLEET_CLIENT_CONFIG_DIR:-}"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
# Where the running unit's binaries live. Resolved (in order): --install-dir /
# $FLEET_INSTALL_DIR, else the dir of the unit's ExecStart, else /opt/fleet. The
# freshly built $SRC_DIR/{fleet,fleet-admin} are installed here so the restart
# actually runs the new code (a build alone leaves the live ExecStart untouched).
INSTALL_DIR="${FLEET_INSTALL_DIR:-}"
NO_PULL="${FLEET_UPDATE_NO_PULL:-0}"
# REEXEC is an INTERNAL marker set only by the self-update re-exec below (never a
# user flag). It means "the fleet checkout is ALREADY fast-forwarded to the
# target — skip re-fetching it (and the self-update detection, so we don't loop),
# but otherwise run a NORMAL update: the client-config bundle still pulls, the
# sandbox-rebuild gate still compares Containerfile hashes." Distinct from
# NO_PULL, which is the user's "rebuild the current checkouts, pull NOTHING".
# Conflating the two used to make the re-exec skip the client-bundle pull too, so
# an update that changed update.sh itself never advanced the bundle.
REEXEC="${FLEET_UPDATE_REEXEC:-0}"
# Skip the SRC (fleet checkout) fetch when the user asked for no-pull OR when we
# were re-exec'd after already fast-forwarding it.
SKIP_SRC_FETCH=0
[[ "$NO_PULL" == "1" || "$REEXEC" == "1" ]] && SKIP_SRC_FETCH=1
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
# Adopt a shipped deploy/*.service that drifted from the installed unit WITHOUT
# prompting. Interactive runs ask instead (see the drift check below); this env/
# flag is the unattended opt-in for automation. `--yes` alone never adopts units
# (it only skips the commit-range confirm) so an unattended update can't silently
# clobber an operator's hand-edited unit.
ADOPT_UNITS="${FLEET_UPDATE_ADOPT_UNITS:-0}"
# Sandbox image freshness backstop: rebuild the on-box sandbox image when the
# one in the service user's store is this many days old or older, even when the
# Containerfile and tag are unchanged. Without it a box whose bundle goes quiet
# serves the same image for months — stale base layers, unpatched package CVEs
# (the Grype CI gate scans a FRESH build; only a rebuild brings a deployed box
# up to what CI vouched for). 0 disables the backstop. Age-triggered rebuilds
# pass --no-cache through to build-sandbox-image.sh so every layer re-runs —
# a cached rebuild against an unmoved base would produce the same image with
# the same old creation date and the gate would spin forever doing nothing.
SANDBOX_MAX_AGE_DAYS="${FLEET_SANDBOX_MAX_AGE_DAYS:-7}"
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
    --sandbox-max-age)   shift; [[ $# -gt 0 ]] || { echo "error: --sandbox-max-age needs a day count" >&2; exit 1; }; SANDBOX_MAX_AGE_DAYS="$1" ;;
    --sandbox-max-age=*) SANDBOX_MAX_AGE_DAYS="${1#*=}" ;;
    --no-pull)        NO_PULL=1 ;;
    --yes|-y)         ASSUME_YES=1 ;;
    --dry-run)        DRY_RUN=1 ;;
    --adopt-units)    ADOPT_UNITS=1 ;;
    -h|--help)        sed -n '2,60p' "$0"; exit 0 ;;
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

# Fall back to the bootstrap-persisted client bundle dir (state file) when none
# was passed via env or --client-config, then to the in-repo generic bundle.
if [[ -z "$CLIENT_DIR" ]]; then
  _dir_file="${FLEET_STATE_DIR:-$SRC_DIR/.fleet-state}/client-config.dir"
  [[ -f "$_dir_file" ]] && CLIENT_DIR="$(tr -d '[:space:]' < "$_dir_file")"
fi
CLIENT_DIR="${CLIENT_DIR:-$REPO_ROOT/config/default}"

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

# Validate up front: the value gates a multi-GB rebuild, so a typo must die
# here, not silently disable the backstop (or arithmetic-error mid-update).
[[ "$SANDBOX_MAX_AGE_DAYS" =~ ^[0-9]+$ ]] \
  || die "--sandbox-max-age / FLEET_SANDBOX_MAX_AGE_DAYS must be a non-negative integer day count (0 disables), got: ${SANDBOX_MAX_AGE_DAYS}"

# require_go_toolchain — fail the build step with an actionable message rather
# than an opaque one.
#
# The Makefile exports GOTOOLCHAIN=auto, so the operator does NOT need go.mod's
# exact pinned patch release installed: Go fetches it. What Go cannot do is
# bootstrap that fetch from a toolchain older than 1.21, which is when
# GOTOOLCHAIN was introduced. That is the one case worth naming up front —
# otherwise `make build` dies on a raw version-mismatch line the operator has to
# decode into "upgrade Go".
#
# A GOVERSION we can't parse (a devel/source build) is not treated as a failure:
# the build itself is the real check, and refusing to run on an unrecognized
# string would be worse than letting it proceed.
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
    die "installed Go is ${goversion}, which predates GOTOOLCHAIN (added in 1.21) and so cannot fetch the pinned toolchain${pinned:+ (go.mod pins $pinned)} — upgrade Go and re-run; live binary left in place"
  fi
}
# upsert_env_file FILE KEY VALUE — update one key without sourcing the secrets
# file or disturbing operator-managed entries.
upsert_env_file() {
  local file="$1" key="$2" value="$3" tmp
  [[ -f "$file" ]] || install -D -m 0600 /dev/null "$file"
  tmp="$(mktemp "${file}.XXXXXX")"
  KEY="$key" VALUE="$value" awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
    {
      eq = index($0, "=")
      if (!done && eq > 0 && substr($0, 1, eq - 1) == k) {
        print k "=" v; done = 1; next
      }
      print
    }
    END { if (!done) print k "=" v }
  ' "$file" > "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$file"
}
# env_get KEY [FILE] — read one key from an env file without sourcing it (the
# file holds secrets; sourcing would execute arbitrary content on a tampered
# box). Last assignment wins, surrounding quotes stripped. Same helper as
# doctor.sh's env_get; keep the copies in sync (callers here always pass FILE
# explicitly — update.sh has no $ENV_FILE default).
env_get() {
  local key="$1" file="${2:-$ENV_FILE}"
  [[ -r "$file" ]] || return 0
  grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true
}
# Print a unit diff indented under the warning, capped so a pathological
# divergence can't flood the terminal (functional bodies are tiny in practice).
show_unit_diff() {
  printf '%s\n' "$1" | head -n 80 | sed 's/^/    /'
  [[ "$(printf '%s\n' "$1" | wc -l)" -gt 80 ]] && info "    … (diff truncated; run the review command below for the full diff)"
  say
}
# Emit the FUNCTIONAL body of a unit file — comments and blank lines stripped —
# so cosmetic header churn between releases is not mistaken for a real change.
unit_functional_body() { grep -vE '^[[:space:]]*(#|$)' "$1" 2>/dev/null || true; }

# -e, not -d: in a git WORKTREE .git is a pointer file, not a directory, and
# the dry-run smoke test (TestUpdateDryRunSmoke) runs from agent worktrees per
# the scripts/test-db-setup.sh workflow. Either shape is a valid checkout for
# the `git -C` operations below.
[[ -e "$SRC_DIR/.git" ]] || die "no fleet source checkout at $SRC_DIR (run scripts/bootstrap.sh first)"

if [[ "$REEXEC" == "1" ]]; then
  step "fleet update (src=${SRC_DIR}, client=${CLIENT_DIR}, service=${SERVICE_NAME}, post-self-update re-exec, dry-run=${DRY_RUN})"
else
  step "fleet update (src=${SRC_DIR}, client=${CLIENT_DIR}, service=${SERVICE_NAME}, no-pull=${NO_PULL}, dry-run=${DRY_RUN})"
fi

# ── 1. pull the fleet checkout ────────────────────────────────────────────
step "1/5  Updating the fleet checkout"
cd "$SRC_DIR"
git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true

before_sha="$(git rev-parse HEAD)"

if [[ "$SKIP_SRC_FETCH" == "1" ]]; then
  after_sha="$before_sha"
  # Restored across a self-re-exec so the final summary still shows the real
  # old → new range (see the re-exec block below).
  before_sha="${FLEET_UPDATE_BASE_SHA:-$before_sha}"
  target_branch="$(git rev-parse --abbrev-ref HEAD)"
  if [[ "$REEXEC" == "1" ]]; then
    ok "post-self-update re-exec — fleet checkout already at ${after_sha:0:12}; continuing (client bundle still pulls)"
  else
    ok "rebuild-only mode — skipping fetch, building ${after_sha:0:12}"
  fi
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
      # update.sh, re-exec the fresh copy with FLEET_UPDATE_REEXEC=1: the fleet
      # checkout is already fast-forwarded, so the new copy skips ONLY the SRC
      # fetch + self-update detection (no loop) and then runs the rest of the
      # update normally — crucially still pulling the client-config bundle
      # (NOT set to NO_PULL, which would skip it).
      if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
        warn "update.sh changed in this update — re-executing the new version"
        exec env FLEET_UPDATE_REEXEC=1 FLEET_UPDATE_YES=1 \
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

# The unit's User= — the rootless account whose podman image store the running
# service reads (root's rootful store is a separate namespace). Resolved once
# for the bundle chown below and the step-3 store probe/prune; empty when the
# unit (or systemd) is absent, in which case podman runs as the invoking user —
# the same rule build-sandbox-image.sh applies to the build itself.
service_user=""
if command -v systemctl >/dev/null 2>&1; then
  service_user="$(systemctl show -p User --value "${SERVICE_NAME}.service" 2>/dev/null || true)"
fi

# Re-apply service-user ownership after the pull, mirroring bootstrap.sh: this
# script runs as root, so every file the pull creates/rewrites comes out
# root-owned — and the sandbox bind-mounts bundle dirs with an SELinux relabel
# (:z), which rootless podman may only apply to files the service user OWNS.
# One root-owned protocols/*.yaml is enough to kill every container start with
# `lsetxattr … operation not permitted` (exit 126) until the next chown.
# Skipped for the in-repo generic bundle (chowning the fleet checkout would be
# wrong); runs even in rebuild-only mode to heal a checkout a previous
# root-run pull broke. Idempotent.
if [[ -d "$CLIENT_DIR/.git" && "$CLIENT_DIR" != "$SRC_DIR"/* && "$CLIENT_DIR" != "config/default" ]]; then
  bundle_owner="${service_user:-fleet}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would chown -R ${bundle_owner}: ${CLIENT_DIR} (rootless sandbox relabel needs service-user ownership)"
  elif chown -R "$bundle_owner": "$CLIENT_DIR" 2>/dev/null; then
    ok "bundle ${CLIENT_DIR} owned by ${bundle_owner} (so the rootless sandbox :z relabel is permitted)"
  else
    warn "could not chown ${CLIENT_DIR} to ${bundle_owner} — sandbox relabel may fail (EPERM) on files the pull rewrote"
  fi
fi

# The service's env file (the unit's EnvironmentFile=). Never sourced — read
# key-by-key via env_get, both for the sandbox-image resolution below and the
# origin reconcile in the build step.
backend_env_file="${FLEET_ENV_FILE:-/etc/fleet/fleet.env}"

# ── record the pre-build sandbox gate inputs (Containerfile hash + tag) ──
# Compare a stored hash of the bundle's Containerfile AND the image tag the
# build would produce so the ~2-3min image build only runs when either changed.
# The tag matters as much as the content: a bundle that renames sandbox.tag
# with an unchanged Containerfile still needs a build, or the service resolves
# a tag that exists nowhere and every sandboxed tool call fails at runtime
# (fleet does not verify the image at boot). The tag is resolved by
# build-sandbox-image.sh --print-tag — the same manifest read the build uses.
sandbox_cf="$CLIENT_DIR/sandbox/Containerfile"
hash_file() { [[ -f "$1" ]] && sha256sum "$1" | awk '{print $1}' || printf 'absent'; }
STATE_DIR="${FLEET_STATE_DIR:-$SRC_DIR/.fleet-state}"
STAMP_FILE="$STATE_DIR/sandbox-containerfile.sha256"
REF_FILE="$STATE_DIR/sandbox-image.ref"
cf_now="$(hash_file "$sandbox_cf")"
cf_prev="absent"
[[ -f "$STAMP_FILE" ]] && cf_prev="$(cat "$STAMP_FILE")"
ref_now="$(FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" "$SCRIPT_DIR/build-sandbox-image.sh" --print-tag 2>/dev/null || true)"
ref_prev=""
[[ -f "$REF_FILE" ]] && ref_prev="$(tr -d '[:space:]' < "$REF_FILE")"

# resolve_sandbox_image MANIFEST — print the bundle's resolved sandbox.image,
# the SAME way the running SERVICE does: extract the scalar under the sandbox:
# block (the awk mirrors bootstrap.sh — keep that part in sync), then
# interpolate a bare ${VAR:-default} / ${VAR} reference against the service's
# env file (env_get), NOT this shell's environment. The service resolves the
# var from EnvironmentFile=$backend_env_file, which update.sh deliberately
# never sources — resolving from the shell diverged both ways: a var set only
# in the env file forced a pointless on-box build every update (and could trip
# the missing-image die spuriously), while a var exported only in the
# operator's shell skipped the sandbox gate entirely, absence probe included.
# (bootstrap.sh legitimately interpolates from ITS shell: at bootstrap time
# the operator's env is the source the env file is being written from.)
# A non-empty result means the service pulls/uses that prebuilt ref (image WINS
# over tag) and never reads an on-box build, so the sandbox step skips
# entirely; empty — the generic bundle's "${FLEET_SANDBOX_IMAGE:-}" with the
# var absent from the env file — means build-on-box.
resolve_sandbox_image() {
  local file="$1" raw
  [[ -f "$file" ]] || return 0
  raw="$(awk '
    /^sandbox:[[:space:]]*$/ { in_block=1; next }
    /^[^[:space:]]/          { in_block=0 }
    in_block && $0 ~ "^[[:space:]]+image:" {
      sub("^[[:space:]]+image:[[:space:]]*", "")
      sub(/[[:space:]]+#.*$/, "")
      gsub(/^["'\'']|["'\'']$/, "")
      print; exit
    }
  ' "$file")"
  # Interpolate a single leading ${VAR} or ${VAR:-default} (the only shapes the
  # default bundle uses). Anything else is treated as a literal image ref.
  if [[ "$raw" =~ ^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$ ]]; then
    local var="${BASH_REMATCH[1]}" def="${BASH_REMATCH[3]}" val
    val="$(env_get "$var" "$backend_env_file")"
    printf '%s' "${val:-$def}"
  else
    printf '%s' "$raw"
  fi
}
sandbox_image_ref="$(resolve_sandbox_image "$CLIENT_DIR/manifest.yaml")"

# sandbox_podman CMD… — podman against the image store the service actually
# reads: as the unit's User= when running as root and that user exists, else
# the invoking user (matching where build-sandbox-image.sh lands the build).
# cd first: rootless podman re-execs inside its user namespace and chdir()s
# back to the inherited cwd, which the service user may not be able to enter
# (e.g. /root).
sandbox_podman() {
  local home
  if [[ $EUID -eq 0 && -n "$service_user" && "$service_user" != "root" ]] && id -u "$service_user" >/dev/null 2>&1; then
    home="$(getent passwd "$service_user" | cut -d: -f6)"
    # /run/<user> is tmpfs and only exists while the unit runs (its
    # RuntimeDirectory= recreates it) — pre-create it the way
    # build-sandbox-image.sh does before the build, or probing a STOPPED
    # unit's store fails environmentally during exactly the maintenance
    # windows updates run in.
    install -d -o "$service_user" -g "$service_user" -m 0700 "/run/${service_user}"
    ( cd "$home" 2>/dev/null || cd /
      runuser -u "$service_user" -- env HOME="$home" XDG_RUNTIME_DIR="/run/${service_user}" podman "$@" )
  else
    podman "$@"
  fi
}

# sandbox_image_state REF — print "present", "absent", or "error". `podman
# image exists` distinguishes these by exit code (0 = found, 1 = not found,
# 125 = could not access the store at all); collapsing them into one boolean
# made every environmental failure read as a missing image. Callers must act
# only on a POSITIVE answer: "error" justifies neither a multi-GB rebuild nor
# a restart into an unverified store.
sandbox_image_state() {
  local rc=0
  sandbox_podman image exists "$1" >/dev/null 2>&1 || rc=$?
  case "$rc" in
    0) printf 'present' ;;
    1) printf 'absent' ;;
    *) printf 'error' ;;
  esac
}

# sandbox_image_age_days REF — print the whole days since the image was built,
# or nothing when the creation time can't be read (missing image, template
# unsupported, store unreachable). Callers treat empty as "unknown" and skip
# the age gate — an unanswerable probe must not trigger a rebuild, the same
# only-act-on-a-positive-answer rule sandbox_image_state applies.
sandbox_image_age_days() {
  local created now
  created="$(sandbox_podman image inspect --format '{{.Created.Unix}}' "$1" 2>/dev/null | tr -d '[:space:]')"
  [[ "$created" =~ ^[0-9]+$ ]] || return 0
  now="$(date +%s)"
  (( now >= created )) || { printf '0'; return 0; }
  printf '%s' $(( (now - created) / 86400 ))
}

# ── 3. rebuild the sandbox image when its gate inputs changed ───────────────
step "3/5  Rebuilding the sandbox image (when the Containerfile/tag changed or the image went stale)"
if [[ -n "$sandbox_image_ref" ]]; then
  # image wins over tag (internal/clientconfig): the service pulls/uses the
  # prebuilt ref and never reads an on-box build, so gating on sandbox.tag
  # here would burn one pointless multi-GB build the first update after a
  # client switches to registry images. Same skip bootstrap.sh applies.
  info "manifest resolves sandbox.image=${sandbox_image_ref} — using a prebuilt/registry image; skipping the on-box build."
elif [[ "$cf_now" == "absent" ]]; then
  info "no ${sandbox_cf} — bundle ships no sandbox Containerfile; skipping (set sandbox.image or add one)."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would rebuild if the Containerfile hash or resolved tag (${ref_now:-unresolved}) changed, the tag is missing from the service user's store, or the installed image is ${SANDBOX_MAX_AGE_DAYS}+ days old (FLEET_SANDBOX_MAX_AGE_DAYS; 0 disables the freshness backstop)"
  info "[dry-run] would run: FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} scripts/build-sandbox-image.sh (--no-cache when age-triggered)"
  info "[dry-run] would record the new Containerfile hash + tag under ${STATE_DIR}"
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — skipping sandbox build (install podman, then run scripts/build-sandbox-image.sh)."
else
  build_reason=""
  # Age-triggered rebuilds run --no-cache (via FLEET_SANDBOX_BUILD_NO_CACHE):
  # with a cached build against an unmoved base, podman would emit the SAME
  # image with the same old creation date and the backstop would re-fire every
  # update while refreshing nothing. Every other reason keeps the layer cache —
  # there the trigger itself guarantees a real change.
  sandbox_build_no_cache=0
  if [[ "$NO_PULL" == "1" ]]; then
    build_reason="rebuild-only mode"
  elif [[ "$cf_prev" == "absent" ]]; then
    build_reason="no stored Containerfile hash yet — establishing the baseline"
  elif [[ "$cf_now" != "$cf_prev" ]]; then
    build_reason="Containerfile changed (${cf_prev:0:12} → ${cf_now:0:12})"
  elif [[ -n "$ref_now" && -n "$ref_prev" && "$ref_now" != "$ref_prev" ]]; then
    # An empty ref_prev is a pre-tag-stamp box, not a rename — the store probe
    # below still covers it without forcing a fleet-wide rebuild.
    build_reason="image tag renamed (${ref_prev} → ${ref_now})"
  elif [[ -n "$ref_now" ]]; then
    # Catches what the stamp files cannot see: a pruned/lost store, a renamed
    # tag never built, or a pre-fix build that landed in root's store. Only a
    # positive "absent" answer forces the build — a probe that failed
    # environmentally proves nothing about the image and must not trigger a
    # spurious full rebuild.
    case "$(sandbox_image_state "$ref_now")" in
      absent) build_reason="${ref_now} missing from the sandbox image store" ;;
      error)  warn "could not probe the sandbox image store for ${ref_now} (podman failed as ${service_user:-$(id -un)}) — leaving the image as-is; check with: fleet doctor" ;;
      present)
        # Freshness backstop: an unchanged bundle must not mean a frozen
        # image — the base layers and packages inside it keep aging (and
        # accumulating published CVEs) no matter how stable the Containerfile
        # is. Only a readable creation time triggers (empty = unknown = skip).
        if (( SANDBOX_MAX_AGE_DAYS > 0 )); then
          image_age_days="$(sandbox_image_age_days "$ref_now")"
          if [[ -n "$image_age_days" ]] && (( image_age_days >= SANDBOX_MAX_AGE_DAYS )); then
            build_reason="${ref_now} was built ${image_age_days} days ago (max age ${SANDBOX_MAX_AGE_DAYS}d) — refreshing the base image + package layers"
            sandbox_build_no_cache=1
          fi
        fi
        ;;
    esac
  fi
  if [[ -z "$build_reason" ]]; then
    ok "sandbox image up to date (Containerfile ${cf_now:0:12}, ${ref_now:-tag unresolved}) — skipping the image build."
  else
    info "${build_reason} — building the sandbox image."
    if FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" FLEET_SERVICE_NAME="$SERVICE_NAME" \
       FLEET_SANDBOX_BUILD_NO_CACHE="$sandbox_build_no_cache" "$SCRIPT_DIR/build-sandbox-image.sh"; then
      mkdir -p "$STATE_DIR"
      printf '%s\n' "$cf_now" > "$STAMP_FILE"
      [[ -n "$ref_now" ]] && printf '%s\n' "$ref_now" > "$REF_FILE"
      ok "sandbox image rebuilt (${ref_now:-see build output}); recorded hash ${cf_now:0:12}"
      # Each rebuild strands the previous image's layers as dangling cruft
      # (~1.3 GB per rebuild) in the store the build targeted; prune THAT
      # store so regular updates can't fill the disk. Dangling-only: any
      # still-tagged image is untouched. Best-effort.
      if sandbox_podman image prune -f >/dev/null 2>&1; then
        ok "pruned dangling image layers left by the rebuild (fleet cleanup does more)."
      fi
    else
      # A failed build is survivable ONLY while the resolved ref still exists
      # in the service user's store — the Containerfile-changed-under-the-
      # same-tag case, where the previous image is stale but serviceable.
      # Anything else (tag renamed, store pruned/lost, probe unanswerable)
      # means the step-5 restart would bring the box up reporting healthy
      # while every sandboxed tool call fails, so refuse to continue. This
      # gate runs BEFORE the install step on purpose: dying here leaves the
      # box coherent — old binaries on disk, old service running — instead of
      # new code installed with the update claiming nothing changed.
      if [[ -n "$ref_now" && "$(sandbox_image_state "$ref_now")" == "present" ]]; then
        warn "sandbox image build failed — the restarted service keeps running the existing (now stale) ${ref_now}; rebuild soon: sudo FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} FLEET_SERVICE_NAME=${SERVICE_NAME} ${SCRIPT_DIR}/build-sandbox-image.sh"
      else
        warn "nothing was installed and the ${SERVICE_NAME} service was NOT restarted — the box keeps running the pre-update binaries with the bundle it loaded at boot."
        warn "recover: fix the build error above, then run: sudo FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} FLEET_SERVICE_NAME=${SERVICE_NAME} ${SCRIPT_DIR}/build-sandbox-image.sh"
        warn "finish:  fleet update --no-pull   (runs the remaining update steps + the restart)"
        die "sandbox image build failed and ${ref_now:-the resolved image} is not known to exist in the service user's store — refusing to install an update whose every sandboxed tool call would fail"
      fi
    fi
  fi
fi

# ── 4. build the fleet binary + the web app ───────────────────────────────
step "4/5  Building the fleet binary + web app"
# Reconcile the backend's OAuth callback origin from the public, non-secret web
# build stamp bootstrap persisted. This repairs existing installations during a
# normal update and keeps both tiers byte-identical without trusting request
# headers. The encryption key is generated once when absent.
web_env_file="/etc/fleet/fleet-web.env"
web_origin=""; web_app_name=""
if [[ -r "$web_env_file" ]]; then
  web_origin="$(grep '^NEXT_PUBLIC_PUBLIC_ORIGIN=' "$web_env_file" | cut -d= -f2- || true)"
  web_app_name="$(grep '^NEXT_PUBLIC_APP_NAME=' "$web_env_file" | cut -d= -f2- || true)"
fi
if [[ -n "$web_origin" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would reconcile FLEET_PUBLIC_BASE_URL + MCP encryption key in ${backend_env_file}"
  else
    upsert_env_file "$backend_env_file" FLEET_PUBLIC_BASE_URL "$web_origin"
    if ! grep -q '^FLEET_MCP_OAUTH_ENCRYPTION_KEY=' "$backend_env_file"; then
      upsert_env_file "$backend_env_file" FLEET_MCP_OAUTH_ENCRYPTION_KEY \
        "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
    fi
    ok "remote-MCP callback origin reconciled → ${web_origin}"
  fi
fi
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: (cd ${SRC_DIR} && make build)  → ${SRC_DIR}/fleet + fleet-admin"
  info "[dry-run] would install fleet + fleet-admin → ${INSTALL_DIR:-<unit ExecStart dir, else /opt/fleet>}"
  info "[dry-run] would run: (cd ${SRC_DIR}/web && npm ci && npm run build) with the NEXT_PUBLIC_* stamps from /etc/fleet/fleet-web.env"
  info "[dry-run] would deploy the web build → the fleet-web unit's WorkingDirectory (else /opt/fleet/web)"
else
  require_go_toolchain
  ( cd "$SRC_DIR" && make build ) || die "make build failed — live binary left in place"
  [[ -x "$SRC_DIR/fleet" && -x "$SRC_DIR/fleet-admin" ]] \
    || die "make build did not emit ${SRC_DIR}/fleet + ${SRC_DIR}/fleet-admin"
  ok "fleet + fleet-admin binaries built"

  # Install the freshly built binaries to the unit's ExecStart location so the
  # restart below actually runs the NEW code. Without this the build is a no-op
  # against the live deployment.
  if [[ -z "$INSTALL_DIR" ]]; then
    # `systemctl show -p ExecStart --value` prints an exec-command struct
    # ("{ path=/opt/fleet/fleet ; argv[]=... }"), NOT a bare path — extract the
    # path= field. The old `awk '{print $1}'` grabbed the literal "{", made
    # dirname yield "." (== $SRC_DIR after the cd above), and the install was
    # silently skipped as "in place": the restart re-ran the OLD binary.
    # `|| true` for the same reason as fleet-upgrade.sh: under `set -e` +
    # pipefail a `systemctl show` that cannot reach systemd (any container
    # carrying the binary without systemd as PID 1) would abort the update
    # rather than fall through to the /opt/fleet default below.
    exec_start="$(systemctl show -p ExecStart --value "${SERVICE_NAME}.service" 2>/dev/null \
      | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n1 || true)"
    if [[ "$exec_start" == /* && -x "$(dirname "$exec_start")" ]]; then
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
    # Rebuild with the same NEXT_PUBLIC_* stamps bootstrap baked in (Next
    # inlines them into the browser bundle at build time — a bare rebuild
    # silently drops the public origin + app name). They are client-visible
    # by definition, so grepping just those keys from the 0600 web env file
    # leaks no secret — the file is still never sourced. The build id needs
    # no stamp: next.config.ts derives it from the checkout's git SHA.
    ( cd "$SRC_DIR/web" \
        && export NEXT_PUBLIC_PUBLIC_ORIGIN="$web_origin" NEXT_PUBLIC_APP_NAME="$web_app_name" \
        && npm ci && npm run build ) || die "web build failed"
    ok "web app built (origin=${web_origin:-<default>}, app=${web_app_name:-<default>})"

    # Deploy the build to where fleet-web actually serves from (bootstrap's
    # deploy_web_tier copies to the unit's WorkingDirectory, /opt/fleet/web by
    # default). Without this copy + the restart in step 5, an update rebuilds
    # the app in the checkout and the live site keeps serving the old bundle.
    if systemctl cat fleet-web.service >/dev/null 2>&1; then
      web_dst="$(systemctl show -p WorkingDirectory --value fleet-web.service 2>/dev/null)"
      web_dst="${web_dst:-/opt/fleet/web}"
      if [[ "$(cd "$web_dst" 2>/dev/null && pwd || echo "$web_dst")" == "$SRC_DIR/web" ]]; then
        info "fleet-web runs from the source checkout (${web_dst}) — build is already in place."
      elif install -d "$web_dst" 2>/dev/null && cp -a "$SRC_DIR/web/." "$web_dst/"; then
        ok "deployed web app → ${web_dst}"
        # The cp above re-roots ownership; hand .next (Next's runtime cache —
        # the unit's only ReadWritePaths entry) back to the unit's User= so a
        # non-root fleet-web keeps working after every update. No-op for the
        # legacy root unit (User= empty).
        web_user="$(systemctl show -p User --value fleet-web.service 2>/dev/null)"
        if [[ -n "$web_user" && -d "$web_dst/.next" ]] && id -u "$web_user" >/dev/null 2>&1; then
          chown -R "$web_user:" "$web_dst/.next" 2>/dev/null \
            || warn "chown ${web_dst}/.next to ${web_user} failed — fleet-web may not write its cache"
        fi
      else
        die "could not deploy the web build into ${web_dst} (need root?) — live web app left in place"
      fi
    else
      info "fleet-web.service not installed — leaving the build in ${SRC_DIR}/web."
    fi
  else
    warn "no web/package.json under ${SRC_DIR} — skipping web build."
  fi
fi

# ── unit-file drift check + optional adoption ───────────────────────────────
# bootstrap installs deploy/*.service only when absent and this script never
# rewrites them, so a unit fix shipped in a new release does NOT reach an
# already-provisioned box on its own. We compare FUNCTIONAL lines only (comments
# and blank lines stripped) so a reworded header never nags. On real drift an
# interactive run shows the diff and offers to adopt the shipped unit in ONE step
# (prerequisites + install + daemon-reload here; the step-5 restart applies it);
# a non-interactive run falls back to the actionable manual hint unless
# --adopt-units is set. Overwriting is always gated on explicit consent (a y/N
# answer, or --adopt-units) so an update can never silently clobber an operator
# hand-edit. Drop-ins under /etc/systemd/system/<unit>.d/ survive either path.
# The backup pair adopts the same way but needs NO restart (see below); a box
# without it installed — bootstrap --no-backup-timer, or volume-layer backups —
# is skipped by the both-files-exist check, never force-installed.
NEED_DAEMON_RELOAD=0
if command -v systemctl >/dev/null 2>&1; then
  for unit in fleet.service fleet-web.service fleet-backup.service fleet-backup.timer; do
    installed="/etc/systemd/system/$unit"
    shipped="$SRC_DIR/deploy/$unit"
    [[ -f "$installed" && -f "$shipped" ]] || continue
    # Adopting a backup unit must not add any restart: the oneshot runs only
    # when the timer fires it, and the step-5 daemon-reload is all systemd
    # needs to re-arm a rewritten timer's schedule — the same rule doctor.sh
    # applies. (Restarting the oneshot would even run a backup right now.)
    is_backup_unit=0
    case "$unit" in fleet-backup.service|fleet-backup.timer) is_backup_unit=1 ;; esac
    # Byte-identical → nothing to do.
    cmp -s "$shipped" "$installed" && continue
    # Empty functional diff → only comments/whitespace changed; note it and move
    # on rather than raising a scary "a unit fix may not be live" on doc churn.
    funcdiff="$(diff -u --label "$installed (installed)" --label "$shipped (shipped)" \
      <(unit_functional_body "$installed") <(unit_functional_body "$shipped") 2>/dev/null || true)"
    if [[ -z "$funcdiff" ]]; then
      info "$unit differs from deploy/$unit only in comments/whitespace — no functional change, leaving as-is."
      continue
    fi

    warn "$unit differs functionally from the shipped deploy/$unit — a unit fix in this release may not be live."

    # The shipped fleet-web.service runs as a dedicated fleet-web user (#654);
    # that user is created by `bootstrap --enable-web`, NOT by an update. On a
    # box provisioned before #654 the user may not exist yet, so adopting the
    # unit and restarting would fail with status=217/USER. Detect that so the
    # adopt path (and the manual hint) create it first.
    web_needs_user=0
    web_dst=""
    if [[ "$unit" == "fleet-web.service" ]] && grep -q '^User=fleet-web' "$shipped" 2>/dev/null && ! id -u fleet-web >/dev/null 2>&1; then
      web_needs_user=1
      web_dst="$(systemctl show -p WorkingDirectory --value fleet-web.service 2>/dev/null)"
      web_dst="${web_dst:-/opt/fleet/web}"
    fi

    # Decide whether to adopt: --adopt-units adopts unattended; otherwise an
    # interactive TTY (and not --yes, which means "don't ask me") gets a y/N
    # prompt. Everything else falls through to the warn-only manual hint.
    do_adopt=0
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would offer to adopt $shipped → $installed"
      [[ "$web_needs_user" == "1" ]] && info "[dry-run] would first create the fleet-web user + chown ${web_dst}/.next"
      show_unit_diff "$funcdiff"
    elif [[ "$ADOPT_UNITS" == "1" ]]; then
      do_adopt=1
    elif [[ -t 0 && "$ASSUME_YES" != "1" ]]; then
      show_unit_diff "$funcdiff"
      _extra=""; [[ "$web_needs_user" == "1" ]] && _extra=" + create the fleet-web user"
      _then="restart in step 5"
      case "$unit" in
        fleet-backup.timer)   _then="no restart — the reload re-arms the timer" ;;
        fleet-backup.service) _then="no restart — the next timer fire uses the new definition" ;;
      esac
      printf '%s?%s Adopt the shipped %s%s%s? %s(install%s, daemon-reload, %s) (y/N)%s ' \
        "$c_cyan" "$c_reset" "$c_bold" "$unit" "$c_reset" "$c_dim" "$_extra" "$_then" "$c_reset"
      read -r answer
      case "${answer,,}" in y|yes) do_adopt=1 ;; esac
    fi

    if [[ "$do_adopt" == "1" ]]; then
      # Prerequisite first so the step-5 restart of an adopted fleet-web.service
      # doesn't die 217/USER on a pre-#654 box.
      if [[ "$web_needs_user" == "1" ]]; then
        if useradd --system --home-dir /var/lib/fleet-web --shell /usr/sbin/nologin --no-create-home fleet-web 2>/dev/null; then
          ok "created the fleet-web system user"
        else
          warn "could not create the fleet-web user (need root?) — the fleet-web restart may fail; create it manually"
        fi
        if [[ -d "$web_dst/.next" ]] && chown -R fleet-web: "$web_dst/.next" 2>/dev/null; then
          ok "chowned ${web_dst}/.next to fleet-web"
        fi
      fi
      if install -m 0644 "$shipped" "$installed" 2>/dev/null; then
        ok "adopted $shipped → $installed"
        NEED_DAEMON_RELOAD=1
      else
        die "could not install $shipped → $installed (need root? re-run with sudo, or adopt manually) — live unit left in place"
      fi
    else
      # Declined, non-interactive, or --yes: keep the actionable manual hint so
      # nothing is lost and the operator can adopt out of band.
      warn "  review: diff $installed $shipped"
      if [[ "$is_backup_unit" == "1" ]]; then
        # No restart in this hint: the reload alone re-arms the timer, and
        # restarting the oneshot would run a backup immediately.
        warn "  adopt:  install -m 0644 $shipped $installed && systemctl daemon-reload"
      else
        warn "  adopt:  install -m 0644 $shipped $installed && systemctl daemon-reload && systemctl restart ${unit%.service}"
      fi
      warn "  or re-run: fleet update --adopt-units   (adopts every drifted unit)"
      if [[ "$web_needs_user" == "1" ]]; then
        warn "  first (fleet-web runs as a non-root user now): useradd --system --home-dir /var/lib/fleet-web --shell /usr/sbin/nologin --no-create-home fleet-web && chown -R fleet-web:fleet-web ${web_dst}/.next"
      fi
    fi
  done
fi

# ── 5. restart the service (services self-migrate on start) ────────────────
step "5/5  Restarting the ${SERVICE_NAME} service"
info "application migrations run inside each service on start — update.sh runs none."
# Reload systemd once if we adopted any unit above, so the restarts below run the
# freshly-installed unit rather than the cached old definition.
if [[ "$NEED_DAEMON_RELOAD" == "1" ]] && command -v systemctl >/dev/null 2>&1; then
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: systemctl daemon-reload (adopted unit file(s))"
  elif systemctl daemon-reload 2>/dev/null; then
    ok "systemctl daemon-reload — adopted unit file(s) now live"
  else
    warn "systemctl daemon-reload failed (need root?) — run it before the restart, or the old unit stays cached"
  fi
fi
if ! command -v systemctl >/dev/null 2>&1; then
  warn "systemctl not found — restart ${SERVICE_NAME} manually (no systemd on this box)."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: systemctl restart ${SERVICE_NAME}"
  info "[dry-run] would run: systemctl restart fleet-web (when the unit is installed)"
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

  # Restart the web tier so it serves the freshly deployed build. Explicit
  # rather than left to dependency propagation: fleet-web BindsTo fleet.service,
  # so the restart above already STOPPED it — an explicit start-or-restart both
  # brings it back and picks up the new .next output.
  if systemctl cat fleet-web.service >/dev/null 2>&1; then
    if systemctl restart fleet-web 2>/dev/null; then
      ok "fleet-web restarted on the new build"
    else
      warn "could not restart fleet-web — check: journalctl -u fleet-web -n 50"
    fi
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
