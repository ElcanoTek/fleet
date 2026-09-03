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
# publisher's pipeline's job, not this box's). When the tag cannot be resolved
# at all, the presence and freshness gates both go blind and the step says so
# loudly instead of reporting the image up to date. It then rebuilds the
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
#                          interactive prompt (env FLEET_UPDATE_ADOPT_UNITS=1).
#                          Also rewrites a fleet-managed /etc/caddy/Caddyfile
#                          whose layout drifted from scripts/lib/caddyfile.sh
#                          (e.g. one that predates the /v1 API routes) and
#                          reloads caddy — same consent rule, same prompt.
#   --yes / -y             skip the confirm prompt (env FLEET_UPDATE_YES=1)
#   --no-timers            don't offer to install a missing fleet-backup /
#                          fleet-maintenance timer pair, and don't hint about
#                          it (env FLEET_UPDATE_OFFER_TIMERS=0) — for boxes
#                          that deliberately run without them
#   --no-node-repair       don't hand a node shortfall to `doctor.sh --node`;
#                          fail with the repair command instead (env
#                          FLEET_UPDATE_NODE_REPAIR=0) — for boxes whose node
#                          is managed outside dnf (nvm, nodesource, an image)
#   --dry-run              print the plan; build/restart nothing. Always exits 0
#                          — it reports whether the plan is BLOCKED (e.g. the
#                          node floor is unmet) in the plan itself, the same
#                          contract bootstrap/doctor/fleet-upgrade follow. Use
#                          --check when you want that answer as an exit code.
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
# shellcheck source=lib/node-version.sh
. "$SCRIPT_DIR/lib/node-version.sh"
# The fleet-managed Caddyfile (marker, renderer, drift helpers) — the same
# implementation bootstrap.sh writes from and doctor.sh repairs against.
# shellcheck source=lib/caddyfile.sh
. "$SCRIPT_DIR/lib/caddyfile.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
# Client bundle dir: env/flag wins; else the dir bootstrap persisted under the
# state dir (resolved after arg parsing, alongside the pin); else the in-repo
# generic bundle. Without the state-file fallback, an interactive `fleet
# update` on a box whose FLEET_CLIENT_CONFIG_DIR lives only in the 0600
# systemd env file (which this script deliberately does NOT source) would
# silently update against the generic bundle instead of the client checkout.
CLIENT_DIR="${FLEET_CLIENT_CONFIG_DIR:-}"
# Was the bundle dir named EXPLICITLY (this shell's env, or --client-config), or
# resolved from a fallback? It decides who wins when the service's own env file
# names a different bundle: an explicit operator choice is honoured (with a
# warning), a fallback is corrected to whatever the running service actually
# reads. Without that reconcile, `fleet update` happily pulls one checkout while
# fleet loads another and the operator is told the update succeeded.
# The self-update re-exec ALWAYS exports FLEET_CLIENT_CONFIG_DIR (it has to —
# the re-exec passes no argv), so the env var alone would read as "explicit" on
# the re-exec'd run even when the first run merely fell back to that dir. The
# re-exec therefore carries the original answer in FLEET_CLIENT_CONFIG_EXPLICIT
# and it wins over the inference below.
CLIENT_DIR_EXPLICIT=0
[[ -n "$CLIENT_DIR" ]] && CLIENT_DIR_EXPLICIT=1
[[ -n "${FLEET_CLIENT_CONFIG_EXPLICIT:-}" ]] && CLIENT_DIR_EXPLICIT="$FLEET_CLIENT_CONFIG_EXPLICIT"
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
# Offer to install a MISSING fleet-backup / fleet-maintenance timer pair after
# the drift check (interactive y/N; see below). --no-timers / the env var
# silences the offer AND the non-interactive hint for boxes that deliberately
# run without them (volume-layer backups, an external prune) so a declined
# timer doesn't nag on every update.
OFFER_TIMERS="${FLEET_UPDATE_OFFER_TIMERS:-1}"

# Hand a node shortfall to the repair path (scripts/doctor.sh --node) instead of
# dying. ON by default, and that is a deliberate asymmetry with --adopt-units:
# adopting a unit OVERWRITES an operator's hand-edit, so it needs consent, while
# this only adds a parallel-installable interpreter package and stamps
# FLEET_NODE_BIN — a value this script already rewrites unconditionally further
# down (the "pointed fleet-web at ..." block). Refusing to install the
# interpreter it is about to point the tier at would be incoherent. Boxes whose
# node comes from nvm/nodesource/an image opt out with --no-node-repair.
NODE_REPAIR="${FLEET_UPDATE_NODE_REPAIR:-1}"
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
    --client-config)  shift; [[ $# -gt 0 ]] || { echo "error: --client-config needs a dir" >&2; exit 1; }; CLIENT_DIR="$1"; CLIENT_DIR_EXPLICIT=1 ;;
    --client-config=*) CLIENT_DIR="${1#*=}"; CLIENT_DIR_EXPLICIT=1 ;;
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
    --no-timers)      OFFER_TIMERS=0 ;;
    --no-node-repair) NODE_REPAIR=0 ;;
    -h|--help)        awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
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

# Set by the node gate below; declared here so the later FLEET_NODE_BIN stamp
# and build-PATH prefix are safe under `set -u` on a checkout with no web/.
node_bin_resolved=""
node_major_want=""
npm_cli_resolved=""
DRY_RUN_BLOCKED=0

# node_probe — resolve the interpreter THIS checkout's web tier needs, and set
# node_major_want + node_bin_resolved from what was actually found on the box.
#
# Read-only and side-effect free, so the plan printer (--dry-run) and the real
# run share one implementation: the whole point of scripts/lib/node-version.sh
# is that "which node?" is answered in exactly one place, and a dry run that
# answered it differently from the run it previews would be worse than not
# answering at all.
#
# npm is probed too, and separately, because on Fedora it is a separate package
# (nodejs<major>-npm) whose shebang names its interpreter ABSOLUTELY — so
# "a node 24 is installed" does not imply "npm will build on node 24", and the
# gate's promise is about the build. See fleet_resolve_npm_cli.
#
# An unresolvable npm is a BLOCKER only for a versioned interpreter. That is the
# parallel-stream layout, where the bare `npm` provably belongs to a different
# interpreter and the missing package has a name. On a single-node layout
# (Debian/nodesource/nvm) the same miss means npm ships in a shape this probe
# cannot read — and there the `node` pin in the build PATH is enough, because
# npm's shebang on those distros really is the relative `env node`. Refusing
# that box would be inventing a blocker out of an unread file.
#
# Returns: 0 resolved · 1 nothing on this box qualifies · 2 web/.nvmrc unreadable
#          · 3 a versioned node resolved but its npm did not.
node_probe() {
  node_bin_resolved=""
  npm_cli_resolved=""
  node_major_want="$(fleet_node_major_want "$SRC_DIR" || true)"
  [[ -n "$node_major_want" ]] || return 2
  node_bin_resolved="$(fleet_resolve_node_bin "$node_major_want")" || { node_bin_resolved=""; return 1; }
  npm_cli_resolved="$(fleet_resolve_npm_cli "$node_bin_resolved")" || npm_cli_resolved=""
  if [[ -z "$npm_cli_resolved" && "${node_bin_resolved##*/}" =~ ^node-[0-9]+$ ]]; then
    return 3
  fi
  return 0
}

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
      #
      # The re-exec passes NO argv, so EVERY setting a command-line flag can
      # change must be restated here as its env equivalent. A flag left out of
      # this list is silently downgraded to its default on exactly the run that
      # pulled the fix — the hardest run to notice it on, and one an operator
      # only gets once. (Flags whose absence is intentional: --no-pull, which
      # would skip the bundle pull the re-exec exists to preserve, and
      # --dry-run, which never reaches here because it skips the fast-forward.
      # --src needs no forwarding: SRC_DIR re-derives from the script path the
      # exec below names. Env-only knobs — FLEET_ENV_FILE, FLEET_STATE_DIR —
      # are inherited by `env` without being named.)
      if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
        warn "update.sh changed in this update — re-executing the new version"
        exec env FLEET_UPDATE_REEXEC=1 FLEET_UPDATE_YES=1 \
          FLEET_UPDATE_BASE_SHA="$before_sha" \
          FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" \
          FLEET_CLIENT_CONFIG_EXPLICIT="$CLIENT_DIR_EXPLICIT" \
          FLEET_CLIENT_CONFIG_PIN="$CLIENT_CONFIG_PIN" \
          FLEET_CLIENT_CONFIG_VERIFY="$CLIENT_CONFIG_VERIFY" \
          FLEET_SERVICE_NAME="$SERVICE_NAME" \
          FLEET_INSTALL_DIR="$INSTALL_DIR" \
          FLEET_UPDATE_BRANCH="$BRANCH_OVERRIDE" \
          FLEET_SANDBOX_MAX_AGE_DAYS="$SANDBOX_MAX_AGE_DAYS" \
          FLEET_UPDATE_ADOPT_UNITS="$ADOPT_UNITS" \
          FLEET_UPDATE_OFFER_TIMERS="$OFFER_TIMERS" \
          FLEET_UPDATE_NODE_REPAIR="$NODE_REPAIR" \
          bash "$SRC_DIR/scripts/update.sh"
      fi
    fi
  fi
fi

# ── 2. pull the client-config bundle checkout ─────────────────────────────
# The service's env file (the unit's EnvironmentFile=). Never sourced — read
# key-by-key via env_get. Resolved HERE, before the bundle step, because the
# reconcile below needs it: it is the only place that records which bundle the
# RUNNING service actually loads.
backend_env_file="${FLEET_ENV_FILE:-/etc/fleet/fleet.env}"

# Normalize a path for comparison: resolve symlinks when we can, else just drop
# a trailing slash. Comparison only — never used as the path we operate on.
norm_dir() {
  local d="${1%/}"
  [[ -z "$d" ]] && { printf '%s' ""; return 0; }
  if command -v realpath >/dev/null 2>&1; then
    realpath -m "$d" 2>/dev/null || printf '%s' "$d"
  else
    printf '%s' "$d"
  fi
}

# Reconcile against the service's configured bundle. fleet reads
# FLEET_CLIENT_CONFIG_DIR from the 0600 env file; this script deliberately does
# not source that file, so its own resolution (env → --client-config → state
# file → the in-repo generic bundle) can land on a DIFFERENT checkout. When it
# does, `fleet update` pulls a bundle nobody loads, prints "✓ client config
# pulled", and the operator sees stale connector copy in the UI with no error
# anywhere. Fallback-resolved → adopt what the service reads. Explicitly named →
# the operator wins, but never silently.
svc_client_dir="$(env_get FLEET_CLIENT_CONFIG_DIR "$backend_env_file")"
if [[ -n "$svc_client_dir" && "$(norm_dir "$svc_client_dir")" != "$(norm_dir "$CLIENT_DIR")" ]]; then
  if [[ "$CLIENT_DIR_EXPLICIT" == "1" ]]; then
    warn "bundle mismatch: you asked to update ${CLIENT_DIR}, but ${SERVICE_NAME} loads ${svc_client_dir} (${backend_env_file})."
    warn "  honouring your explicit choice — the service will NOT see changes pulled into ${CLIENT_DIR}."
  elif [[ -d "$svc_client_dir" ]]; then
    info "bundle dir reconciled: ${SERVICE_NAME} loads ${svc_client_dir} (${backend_env_file}), not the ${CLIENT_DIR} this script resolved."
    CLIENT_DIR="$svc_client_dir"
    ok "updating the bundle the service actually reads: ${CLIENT_DIR}"
  else
    warn "${SERVICE_NAME} is configured for bundle ${svc_client_dir} (${backend_env_file}), but that directory does not exist on this box."
    warn "  continuing with ${CLIENT_DIR} — fix FLEET_CLIENT_CONFIG_DIR or the checkout, or the service is loading a bundle you cannot update."
  fi
fi

# Bundle freshness is reported at the END alongside fleet's own SHA. A stale
# bundle is not a build failure, so it must not abort the update — but it is the
# difference between "fleet updated" and "your deployment updated", and burying
# it in step 2 of 5 is how it goes unnoticed.
BUNDLE_STALE=0
BUNDLE_STALE_WHY=""
bundle_sha_before=""
[[ -e "$CLIENT_DIR/.git" ]] && bundle_sha_before="$(git -C "$CLIENT_DIR" rev-parse HEAD 2>/dev/null || true)"

step "2/5  Updating the client-config bundle"
if [[ "$CLIENT_DIR" == "$SRC_DIR/config/default" || "$CLIENT_DIR" == "config/default" ]]; then
  info "using the in-repo generic bundle (config/default) — no separate checkout to pull."
  # Only a finding when the box is SUPPOSED to run a client bundle. A generic
  # install legitimately has none; a client box that fell back to it here is
  # about to update against content its service never loads.
  if [[ -n "$svc_client_dir" ]]; then
    BUNDLE_STALE=1
    BUNDLE_STALE_WHY="fell back to the generic bundle while ${SERVICE_NAME} loads ${svc_client_dir}"
  fi
elif [[ ! -e "$CLIENT_DIR/.git" ]]; then
  info "client config at ${CLIENT_DIR} is not a git checkout — leaving as-is."
  BUNDLE_STALE=1
  BUNDLE_STALE_WHY="${CLIENT_DIR} is not a git checkout, so nothing can be pulled into it"
elif [[ "$NO_PULL" == "1" ]]; then
  info "rebuild-only mode — skipping client-config pull."
  BUNDLE_STALE=1
  BUNDLE_STALE_WHY="--no-pull / FLEET_UPDATE_NO_PULL skipped the bundle pull"
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
    # A pin is a deliberate hold, but it is ALSO the most common reason a
    # bundle never picks up a change the operator just merged, so say when the
    # pin is behind its own upstream rather than only that it was applied.
    if git -C "$CLIENT_DIR" rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then
      _pin_behind="$(git -C "$CLIENT_DIR" rev-list --count 'HEAD..@{upstream}' 2>/dev/null || echo 0)"
      if [[ "${_pin_behind:-0}" != "0" ]]; then
        BUNDLE_STALE=1
        BUNDLE_STALE_WHY="pinned to ${CLIENT_CONFIG_PIN}, which is ${_pin_behind} commit(s) behind its upstream"
      fi
    fi
  else
    warn "could not check out pinned ref ${CLIENT_CONFIG_PIN} in ${CLIENT_DIR} — leaving the existing checkout"
    BUNDLE_STALE=1
    BUNDLE_STALE_WHY="pinned ref ${CLIENT_CONFIG_PIN} could not be checked out"
  fi
else
  git config --global --add safe.directory "$CLIENT_DIR" 2>/dev/null || true
  # NOT --quiet: when a fast-forward is refused, git's own message is the whole
  # diagnosis (detached HEAD, no upstream, diverged, local edits, wrong branch).
  # Swallowing it left the operator a bare "could not fast-forward" and no way
  # to tell which of those it was.
  _pull_err_file="$(mktemp 2>/dev/null || printf '/dev/null')"
  if git -C "$CLIENT_DIR" pull --ff-only >"$_pull_err_file" 2>&1; then
    ok "client config pulled (${CLIENT_DIR})"
  else
    warn "could not fast-forward ${CLIENT_DIR} — leaving the existing checkout. git said:"
    [[ -s "$_pull_err_file" ]] && sed 's/^/    /' "$_pull_err_file" >&2
    warn "  usual causes: the checkout is on a feature branch or detached HEAD, has local edits,"
    warn "  or has commits the remote does not. Inspect: git -C ${CLIENT_DIR} status -sb"
    BUNDLE_STALE=1
    BUNDLE_STALE_WHY="git pull --ff-only was refused in ${CLIENT_DIR}"
  fi
  [[ "$_pull_err_file" != "/dev/null" ]] && rm -f "$_pull_err_file"
fi

# Report what the bundle actually IS now, and whether it moved. "✓ client config
# pulled" on an already-current checkout and on one that advanced 40 commits are
# the same line; the SHA pair is what lets an operator confirm the change they
# just merged is on the box.
if [[ -e "$CLIENT_DIR/.git" ]]; then
  bundle_sha_after="$(git -C "$CLIENT_DIR" rev-parse HEAD 2>/dev/null || true)"
  bundle_branch="$(git -C "$CLIENT_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
  if [[ -n "$bundle_sha_before" && "$bundle_sha_before" != "$bundle_sha_after" ]]; then
    ok "bundle ${bundle_branch}: ${bundle_sha_before:0:12} → ${bundle_sha_after:0:12}"
  else
    info "bundle ${bundle_branch}: unchanged at ${bundle_sha_after:0:12}"
  fi
  # Behind its upstream after all of the above → the pull did not take, whatever
  # it printed.
  if [[ "$BUNDLE_STALE" == "0" ]] && git -C "$CLIENT_DIR" rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then
    _bundle_behind="$(git -C "$CLIENT_DIR" rev-list --count 'HEAD..@{upstream}' 2>/dev/null || echo 0)"
    if [[ "${_bundle_behind:-0}" != "0" ]]; then
      BUNDLE_STALE=1
      BUNDLE_STALE_WHY="${CLIENT_DIR} is still ${_bundle_behind} commit(s) behind its upstream after the pull"
    fi
  fi

  # On a NON-DEFAULT branch. This is the case an "is it behind its upstream?"
  # check cannot see and the one that actually bit us: a bundle parked on a
  # feature branch tracks that branch, so `git pull --ff-only` SUCCEEDS, the
  # step prints "client config pulled", and the checkout is nonetheless dozens
  # of commits behind the branch every merge lands on. Deliberately tracking a
  # branch is a legitimate operator choice, so this names the situation and the
  # distance rather than treating it as an error — but it never stays silent,
  # because indistinguishable-from-current is precisely the failure.
  if true; then
    _bundle_default="$(git -C "$CLIENT_DIR" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
    _bundle_default="${_bundle_default#origin/}"
    if [[ -n "$_bundle_default" && -n "$bundle_branch" && "$bundle_branch" != "$_bundle_default" && "$bundle_branch" != "HEAD" ]]; then
      _behind_default="$(git -C "$CLIENT_DIR" rev-list --count "HEAD..origin/${_bundle_default}" 2>/dev/null || echo 0)"
      if [[ "${_behind_default:-0}" != "0" ]]; then
        BUNDLE_STALE=1
        BUNDLE_STALE_WHY="${CLIENT_DIR} is on branch ${bundle_branch}, not ${_bundle_default} — current with its own branch but ${_behind_default} commit(s) behind ${_bundle_default}"
      else
        info "bundle is on ${bundle_branch}, not ${_bundle_default} (not behind it — deliberate branch tracking)"
      fi
    fi
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
# Keep the resolver's stderr instead of discarding it. --print-tag ALWAYS
# prints a name:tag when it runs at all (it falls back to
# localhost/fleet-sandbox:latest when the manifest names no sandbox.tag), so an
# EMPTY answer means the script itself could not run — missing, not executable,
# unreadable bundle path. That blinds both store-aware gates in step 3, so the
# operator needs the reason, not just the symptom.
# (/dev/null fallback: a box that cannot mktemp still resolves the tag — the
# reason string is a nicety, never a reason to abort an update.)
ref_err_file="$(mktemp 2>/dev/null || printf '/dev/null')"
ref_now="$(FLEET_CLIENT_CONFIG_DIR="$CLIENT_DIR" "$SCRIPT_DIR/build-sandbox-image.sh" --print-tag 2>"$ref_err_file" || true)"
ref_err=""
if [[ "$ref_err_file" != "/dev/null" ]]; then
  ref_err="$(head -n 1 "$ref_err_file" 2>/dev/null || true)"
  rm -f "$ref_err_file"
fi
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

# ── the node gate ───────────────────────────────────────────────────────────
# Placed HERE — after both pulls, before the first EXPENSIVE and destructive
# step — for two reasons that pull in opposite directions:
#
#   * It cannot run any earlier. The major is declared in the CHECKOUT's
#     web/.nvmrc, so a pre-pull gate reads the OLD floor. On a box provisioned
#     before .nvmrc existed that is not a rounding error: the old floor was a
#     hardcoded 20, node 22 satisfies it, and every pre-pull check says the box
#     is fine. Post-pull is the earliest point at which "which node does this
#     release need?" is even a well-posed question.
#   * It must not run any later. It used to sit inside step 4, i.e. AFTER
#     step 3 had potentially spent 2-3 minutes rebuilding the sandbox image and
#     then pruned the superseded layers — and it still printed "nothing has been
#     built or installed yet". This script already states the standard it was
#     violating: a gate that can abort must run before the first side effect,
#     or it is not a gate.
#
# The checkout fast-forward in step 1 is unavoidably already done by now, so the
# refusal messages below say that rather than claiming an untouched box.
if [[ -f "$SRC_DIR/web/package.json" ]]; then
  step "Node gate (the web tier's interpreter)"
  _np_rc=0; node_probe || _np_rc=$?
  # What a refusal below can honestly claim about this box. Step 1 fast-forwards
  # the checkout — unless --no-pull, in which case nothing has been touched at
  # all. Saying "the checkout was fast-forwarded" on a --no-pull run would be
  # the same species of untrue status line this gate exists to stop printing.
  if [[ "$NO_PULL" == "1" ]]; then
    _state="nothing has been pulled, built, installed or restarted"
  else
    _state="the fleet + client-bundle checkouts were fast-forwarded; nothing was built, installed or restarted"
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    # Do NOT merely claim "would resolve node" — resolve it. `--dry-run` is the
    # one command an operator runs to ask "will this work on my box?", and the
    # node gate is what aborts the run. The previous plan printed
    # "would resolve node >= web/.nvmrc" without ever calling the resolver, so
    # on a box the real run refuses to touch it printed a clean checklist, a
    # green banner and exit 0 — the same "reported a thing that did not happen"
    # fault the rest of this change set exists to kill. node_probe is pure and
    # root-free, so the plan can afford to answer the question for real.
    if (( _np_rc == 0 )); then
      ok "[dry-run] node gate PASSES: ${node_bin_resolved} ($("$node_bin_resolved" -v)) >= ${node_major_want} (web/.nvmrc) — resolved on this box, not assumed"
      if [[ -n "$npm_cli_resolved" ]]; then
        ok "[dry-run] npm gate PASSES: the build would run ${npm_cli_resolved} on ${node_bin_resolved} — resolved on this box, not assumed"
      else
        info "[dry-run] npm gate: no npm-cli.js resolvable for ${node_bin_resolved} (a single-node layout), so the build would use \`npm\` from PATH ($(command -v npm 2>/dev/null || echo none)) under a pinned \`node\`"
      fi
    elif (( _np_rc == 2 )); then
      warn "[dry-run] BLOCKER: cannot read the node major from ${SRC_DIR}/web/.nvmrc — the real run stops here"
      DRY_RUN_BLOCKED=1
    elif (( _np_rc == 3 )); then
      # A blocker in its own right: bare `npm` on this box belongs to the
      # DEFAULT stream and its shebang pins it there, so the build would run on
      # the old major however the node question was answered.
      warn "[dry-run] BLOCKER: node ${node_bin_resolved} qualifies but no npm belongs to it — install nodejs${node_major_want}-npm"
      DRY_RUN_BLOCKED=1
      info "[dry-run] for a scriptable answer use: fleet update --check   (exits non-zero on this)"
    else
      warn "[dry-run] BLOCKER: no node >= ${node_major_want} (web/.nvmrc) — have $(node -v 2>/dev/null || echo none)"
      if [[ "$NODE_REPAIR" != "1" ]]; then
        info "[dry-run] --no-node-repair is set, so the real run stops here — fix it with: sudo fleet doctor --node"
      elif [[ $EUID -ne 0 ]]; then
        info "[dry-run] the real run would repair it via scripts/doctor.sh --node, but that needs root and this is uid ${EUID}"
      else
        info "[dry-run] the real run would repair it first: scripts/doctor.sh --node, then re-resolve and refuse if it is still short"
      fi
      info "[dry-run] for a scriptable answer use: fleet update --check   (exits non-zero on this)"
      DRY_RUN_BLOCKED=1
    fi
  else
    (( _np_rc != 2 )) \
      || die "cannot read the node major from ${SRC_DIR}/web/.nvmrc — expected something like '24' (${_state})"

    if (( _np_rc != 0 )); then
      # The box is a major behind (or has the interpreter and not its npm).
      # Dying here was the old behavior, and it made
      # the documented order load-bearing: `fleet update` stopped, and the
      # operator had to already know the fix lived in a different verb — a verb
      # that, run FIRST on a box provisioned before web/.nvmrc existed, is a
      # no-op, because it too reads the floor from the un-pulled checkout.
      #
      # update.sh is an updater, not a provisioner, so it does NOT grow its own
      # `dnf install`: it hands the shortfall to the ONE place that owns the
      # node install (scripts/doctor.sh --node) and then re-resolves. Scoped to
      # --node deliberately — a full doctor run would adopt drifted units, a
      # write this script only ever performs behind explicit consent
      # (--adopt-units), and laundering that through the node repair would be a
      # consent bypass.
      if (( _np_rc == 3 )); then
        # Same repair, different missing package: doctor --node installs
        # nodejs<major> AND nodejs<major>-npm, which is exactly what is short
        # here. Saying "no node" in this case would send the operator looking
        # for a problem they do not have.
        warn "node ${node_bin_resolved} ($("$node_bin_resolved" -v)) qualifies, but no npm belongs to it — the bare \`npm\` on this box is the default stream's and its shebang pins it there."
      else
        warn "no node >= ${node_major_want} (web/.nvmrc) — have $(node -v 2>/dev/null || echo none)."
      fi
      _doctor="$SCRIPT_DIR/doctor.sh"
      if [[ "$NODE_REPAIR" != "1" ]]; then
        warn "  --no-node-repair is set, so this run will not install it."
        warn "  fix it:  sudo fleet doctor --node    (then re-run this update)"
        die "refusing to build the web tier on an unsupported node — ${_state}"
      elif [[ ! -r "$_doctor" ]]; then
        warn "  the repair path ${_doctor} is missing from this checkout."
        warn "  fix it:  sudo dnf install nodejs${node_major_want} nodejs${node_major_want}-npm"  # both: node and its npm are separate packages
        die "refusing to build the web tier on an unsupported node — ${_state}"
      elif [[ $EUID -ne 0 ]]; then
        warn "  repairing it needs root, and this update is running as uid ${EUID}."
        warn "  fix it:  sudo fleet doctor --node    (then re-run this update)"
        die "refusing to build the web tier on an unsupported node — ${_state}"
      fi

      info "repairing the node toolchain first: scripts/doctor.sh --node (${_state})"
      SRC_DIR="$SRC_DIR" bash "$_doctor" --node || true
      hash -r   # a just-installed /usr/bin/node-NN must be visible to this shell

      # Re-RESOLVE. doctor's exit code is not the claim being made here — the
      # claim is "this box can now build the web tier", and only re-running the
      # resolver against the box proves that. Same rule as the resolved
      # ExecStart and resolved TimeoutStopFailureMode assertions.
      node_probe \
        || die "still no node >= ${node_major_want} with a matching npm after scripts/doctor.sh --node — inspect 'dnf list nodejs*' / 'dnf repolist'; ${_state}"
      ok "node toolchain repaired in place — no re-run needed"
    fi
    ok "web tier will build+run on ${node_bin_resolved} ($("$node_bin_resolved" -v))"
    # Named separately from the interpreter because it is a separate claim, and
    # the one that was silently false: the gate resolved node 24, the build ran
    # npm 22 (absolute shebang), and npm rejected the engine it was handed.
    if [[ -n "$npm_cli_resolved" ]]; then
      ok "web tier will build with ${npm_cli_resolved} — pinned to that interpreter, not to PATH"
    else
      info "no npm-cli.js resolvable for ${node_bin_resolved} — the build will use \`npm\` from PATH under a pinned \`node\`; the read-back below reports which interpreter it actually used"
    fi
  fi
fi

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
  # Set when a gate could not reach an answer (as opposed to answering "no
  # rebuild needed"). It must never read as "up to date" at the end of this
  # step: nothing here learned whether the service's image exists, or how old
  # it is.
  sandbox_unverified=""
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
      error)
        # Reported once, below: an inconclusive probe is the same blind spot as
        # an unresolvable tag, and it must not be followed by an "up to date".
        sandbox_unverified="podman could not read the image store for ${ref_now} as ${service_user:-$(id -un)}"
        ;;
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
  else
    # ref_now empty: the resolver never produced a tag, so the two gates that
    # need one — the store-presence probe and the max-age freshness backstop —
    # were both unreachable. This used to fall out of the chain with no reason
    # set and print a reassuring "up to date" line that showed the tag as
    # unresolved right there in the same breath, which is how a weeks-old image
    # sits on a box that updates cleanly every day. Not fatal: an unresolvable
    # tag is no evidence the running image is broken, and dying here would
    # strand an otherwise good update.
    sandbox_unverified="the image tag could not be resolved${ref_err:+ (${ref_err})}"
  fi
  if [[ -z "$build_reason" ]]; then
    if [[ -n "$sandbox_unverified" ]]; then
      warn "sandbox image NOT verified and NOT rebuilt — ${sandbox_unverified}."
      warn "  neither the store-presence check nor the ${SANDBOX_MAX_AGE_DAYS}-day freshness backstop reached an answer, so the ${SERVICE_NAME} service keeps whatever image its store already holds, of unknown age."
      if [[ -z "$ref_now" ]]; then
        warn "  diagnose the tag: FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} ${SCRIPT_DIR}/build-sandbox-image.sh --print-tag"
      fi
      warn "  check the box: fleet doctor   —   rebuild by hand: sudo FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} FLEET_SERVICE_NAME=${SERVICE_NAME} ${SCRIPT_DIR}/build-sandbox-image.sh"
    else
      ok "sandbox image up to date (Containerfile ${cf_now:0:12}, ${ref_now}) — skipping the image build."
    fi
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
  info "[dry-run] would refresh /usr/local/bin/fleet-web-start.sh and set FLEET_NODE_BIN in /etc/fleet/fleet-web.env"
  info "[dry-run] would run: (cd ${SRC_DIR}/web && npm ci && npm run build) with the NEXT_PUBLIC_* stamps from /etc/fleet/fleet-web.env, on the node+npm the gate resolved above"
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

    # ExecStart points at this shim; a stale copy would send the tier to the
    # wrong interpreter. Shipped content with no operator-tunable parts, so it
    # is refreshed unconditionally rather than gated behind --adopt-units.
    if [[ -f "$SRC_DIR/deploy/fleet-web-start.sh" ]]; then
      if install -D -m 0755 "$SRC_DIR/deploy/fleet-web-start.sh" /usr/local/bin/fleet-web-start.sh 2>/dev/null; then
        ok "refreshed /usr/local/bin/fleet-web-start.sh"
      else
        warn "could not refresh /usr/local/bin/fleet-web-start.sh (need root?) — fleet-web may not start"
      fi
    fi
    # Point the tier at the resolved interpreter. Unset means the shim falls
    # back to `node` on PATH — Fedora's default stream, i.e. possibly the old
    # major — so this is the line that makes the upgrade actually take effect.
    if [[ -f /etc/fleet/fleet-web.env ]]; then
      # tail -n1 + quote strip: systemd's EnvironmentFile is last-wins, and a
      # hand-written FLEET_NODE_BIN="/usr/bin/node-24" must not read as drift.
      # Read the CURRENT value with the same last-wins + quote-strip rule
      # systemd's EnvironmentFile uses, so a hand-written
      # FLEET_NODE_BIN="/usr/bin/node-24" does not read as drift.
      _read_nb() {
        grep -E '^FLEET_NODE_BIN=' /etc/fleet/fleet-web.env 2>/dev/null \
          | tail -n1 | cut -d= -f2- | sed -e 's/^["'"'"']//' -e 's/["'"'"']$//'
      }
      _cur_nb="$(_read_nb)"
      if [[ "$_cur_nb" != "$node_bin_resolved" ]]; then
        if ! upsert_env_file /etc/fleet/fleet-web.env FLEET_NODE_BIN "$node_bin_resolved"; then
          warn "could not set FLEET_NODE_BIN in /etc/fleet/fleet-web.env"
        # Read it BACK rather than trusting the writer's exit code. upsert
        # returning 0 proves a file was written; it does not prove the tier now
        # resolves to this interpreter (a later duplicate line wins under
        # systemd's last-wins rule, and a value that is not executable resolves
        # to nothing). doctor.sh asserts this the same way — and this is the
        # very line doctor's comment points at, so leaving it on the writer's
        # return code would have made the design note's "every new success claim
        # is a read-back" untrue in the one place it named.
        elif [[ "$(_read_nb)" == "$node_bin_resolved" && -x "$node_bin_resolved" ]]; then
          ok "pointed fleet-web at ${node_bin_resolved} — read back from /etc/fleet/fleet-web.env"
        else
          warn "wrote FLEET_NODE_BIN=${node_bin_resolved} but /etc/fleet/fleet-web.env reads back $(_read_nb) — inspect it by hand"
        fi
      fi
    fi

    # Rebuild with the same NEXT_PUBLIC_* stamps bootstrap baked in (Next
    # inlines them into the browser bundle at build time — a bare rebuild
    # silently drops the public origin + app name). They are client-visible
    # by definition, so grepping just those keys from the 0600 web env file
    # leaks no secret — the file is still never sourced. The build id needs
    # no stamp: next.config.ts derives it from the checkout's git SHA.
    # PATH is set so `node`, `npm` and `npx` all resolve to the interpreter the
    # gate picked. The bare-`npm` build used whatever its own shebang named — on
    # Fedora the DEFAULT stream, absolutely pathed — so the tier was built on the
    # old major while this script reported the new one, and npm printed
    # EBADENGINE against web/package.json's `"node": ">=24"` in the middle of an
    # otherwise green update. Resolved once so the shim directory it may create
    # is removed rather than leaked on every update, including on the failure
    # path. See scripts/lib/node-version.sh.
    _build_path="$(fleet_node_build_path "$node_bin_resolved")"
    # Ask npm which interpreter it is actually running under, rather than
    # inferring it from the PATH we just built. The pin is a claim about a
    # subprocess's shebang resolution; only npm's own answer settles it, and
    # `node -v` under the same PATH — what this used to read — cannot: it is the
    # symlink we made, so it reported success for the one component that was
    # already correct while the broken one went unmeasured.
    _build_node="$(fleet_npm_node_version "$_build_path" "$SRC_DIR/web" || true)"
    _build_major="$(fleet_node_version_major "${_build_node:-}" || true)"
    if [[ -z "$_build_node" ]]; then
      warn "could not read the node version npm runs under (\`npm version --json\` gave nothing) — building anyway, but the interpreter is UNVERIFIED"
      _build_node="unverified"
    elif [[ -n "$_build_major" ]] && (( _build_major < node_major_want )); then
      fleet_node_build_path_cleanup "$_build_path" "$node_bin_resolved"
      warn "npm would build the web tier on ${_build_node}, below the ${node_major_want} declared in web/.nvmrc."
      warn "  fix it:  sudo dnf install nodejs${node_major_want}-npm    (then re-run this update)"
      die "refusing to build the web tier with an npm pinned to an unsupported node — binaries are installed, the web tier is untouched"
    fi
    if ! ( cd "$SRC_DIR/web" \
        && export PATH="$_build_path" \
        && export NEXT_PUBLIC_PUBLIC_ORIGIN="$web_origin" NEXT_PUBLIC_APP_NAME="$web_app_name" \
        && npm ci && npm run build ); then
      fleet_node_build_path_cleanup "$_build_path" "$node_bin_resolved"
      die "web build failed"
    fi
    fleet_node_build_path_cleanup "$_build_path" "$node_bin_resolved"
    # Name the interpreter the build ACTUALLY ran under, read back from npm
    # itself, rather than the one the gate intended it to run under.
    ok "web app built on ${_build_node} (origin=${web_origin:-<default>}, app=${web_app_name:-<default>})"

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
# The backup + maintenance timer pairs adopt the same way but need NO restart
# (see below); a box without one installed — bootstrap's --no-backup-timer /
# --no-maintenance-timer, or volume-layer backups — is skipped by the
# both-files-exist check, never force-installed (a MISSING pair gets an
# explicit install offer after this loop instead).
NEED_DAEMON_RELOAD=0
if command -v systemctl >/dev/null 2>&1; then
  for unit in fleet.service fleet-web.service fleet-backup.service fleet-backup.timer fleet-maintenance.service fleet-maintenance.timer; do
    installed="/etc/systemd/system/$unit"
    shipped="$SRC_DIR/deploy/$unit"
    [[ -f "$installed" && -f "$shipped" ]] || continue
    # Adopting a timer-pair unit must not add any restart: each oneshot runs
    # only when its timer fires it, and the step-5 daemon-reload is all systemd
    # needs to re-arm a rewritten timer's schedule — the same rule doctor.sh
    # applies. (Restarting the backup oneshot would even run a backup right now.)
    is_timer_unit=0
    case "$unit" in fleet-backup.*|fleet-maintenance.*) is_timer_unit=1 ;; esac
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
        fleet-backup.timer|fleet-maintenance.timer)     _then="no restart — the reload re-arms the timer" ;;
        fleet-backup.service|fleet-maintenance.service) _then="no restart — the next timer fire uses the new definition" ;;
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
        # fleet-web's drop-in is part of the shipped unit's shutdown behavior
        # (it beats Fedora's global abort-on-timeout drop-in); adopt it with
        # the unit. No restart implication beyond the unit adoption itself.
        if [[ "$unit" == "fleet-web.service" && -f "$SRC_DIR/deploy/fleet-web.service.d/10-timeout-kill.conf" ]]; then
          if install -D -m 0644 "$SRC_DIR/deploy/fleet-web.service.d/10-timeout-kill.conf" \
            /etc/systemd/system/fleet-web.service.d/10-timeout-kill.conf 2>/dev/null; then
            ok "installed fleet-web.service.d/10-timeout-kill.conf"
          else
            warn "could not install the fleet-web drop-in (need root?) — Fedora's global abort drop-in still overrides TimeoutStopFailureMode"
          fi
        fi
      else
        die "could not install $shipped → $installed (need root? re-run with sudo, or adopt manually) — live unit left in place"
      fi
    else
      # Declined, non-interactive, or --yes: keep the actionable manual hint so
      # nothing is lost and the operator can adopt out of band.
      warn "  review: diff $installed $shipped"
      if [[ "$is_timer_unit" == "1" ]]; then
        # No restart in this hint: the reload alone re-arms the timer, and
        # restarting the backup oneshot would run a backup immediately.
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
  # The fleet-web drop-in can be missing even when the unit itself matches
  # (box adopted the unit before the drop-in shipped). Without it, Fedora's
  # global abort-on-timeout drop-in overrides the unit's
  # TimeoutStopFailureMode=kill. Same consent rule as units: adopt only under
  # --adopt-units, else hint. A drop-in install needs no app restart.
  dropin_shipped="$SRC_DIR/deploy/fleet-web.service.d/10-timeout-kill.conf"
  dropin_installed="/etc/systemd/system/fleet-web.service.d/10-timeout-kill.conf"
  # Comment-only churn must not nag, same rule the unit loop above applies via
  # unit_functional_body(): byte-identical → nothing to say; identical once
  # comments are stripped → the directive is already in effect, so say nothing;
  # only a real functional difference is worth a warning. Without this, editing
  # the drop-in's header (11 of its 13 lines are comments) would claim Fedora's
  # abort drop-in is overriding us on a box where it is not.
  dropin_needs_work=0
  if [[ -f /etc/systemd/system/fleet-web.service && -f "$dropin_shipped" ]] \
     && ! cmp -s "$dropin_shipped" "$dropin_installed" 2>/dev/null; then
    if [[ -f "$dropin_installed" ]] && [[ -z "$(diff \
          <(unit_functional_body "$dropin_installed") \
          <(unit_functional_body "$dropin_shipped") 2>/dev/null || true)" ]]; then
      info "fleet-web.service.d/10-timeout-kill.conf differs from deploy/ only in comments/whitespace — no functional change, leaving as-is."
    else
      dropin_needs_work=1
    fi
  fi
  if [[ "$dropin_needs_work" == "1" ]]; then
    if [[ "$ADOPT_UNITS" == "1" ]]; then
      if install -D -m 0644 "$dropin_shipped" "$dropin_installed" 2>/dev/null; then
        ok "installed fleet-web.service.d/10-timeout-kill.conf"
        NEED_DAEMON_RELOAD=1
      else
        warn "could not install the fleet-web drop-in (need root?)"
      fi
    else
      warn "fleet-web.service.d/10-timeout-kill.conf missing or drifted — Fedora's global abort drop-in overrides the unit's TimeoutStopFailureMode=kill."
      warn "  adopt:  install -D -m 0644 $dropin_shipped $dropin_installed && systemctl daemon-reload"
      warn "  or re-run: fleet update --adopt-units"
    fi
  fi

  # Assert the RESOLVED ExecStart. Everything above can succeed — shim
  # refreshed, FLEET_NODE_BIN stamped, build done on the right node — while the
  # live unit still points at the old /usr/bin/node ExecStart, because adopting
  # a drifted unit needs --adopt-units or an interactive yes. Saying nothing
  # there would report a node upgrade that the serving process never got.
  if [[ -f /etc/systemd/system/fleet-web.service ]]; then
    _live_exec="$(systemctl show -p ExecStart --value fleet-web.service 2>/dev/null || true)"
    case "$_live_exec" in
      *fleet-web-start.sh*) : ;;
      "") : ;;   # unit unknown to systemd (never enabled) — not our claim to make
      *)
        warn "fleet-web's live ExecStart is not the shipped shim — the node work above is NOT in effect for the running tier."
        warn "  live: ${_live_exec}"
        warn "  adopt it: sudo fleet update --adopt-units   (or: sudo fleet doctor)"
        ;;
    esac
  fi

  # ── Caddyfile drift (the fleet-managed TLS front) ──
  # bootstrap renders /etc/caddy/Caddyfile from scripts/lib/caddyfile.sh and
  # this script never rewrote it, so a routing fix in a new release (the /v1
  # API + inbound webhooks proxied to the Go backends instead of 404ing at the
  # web tier) did not reach an already-provisioned box on its own — the same
  # gap the unit loop above closes for deploy/*.service, and the same consent
  # rule: adopt under --adopt-units or an interactive yes, else hint. The
  # domain + ACME email are read back from the installed file so a rewrite
  # keeps them. A Caddyfile fleet did not write (no marker) is never touched.
  CADDYFILE="${FLEET_CADDYFILE:-/etc/caddy/Caddyfile}"
  if [[ -f "$CADDYFILE" ]] && caddyfile_is_managed "$CADDYFILE"; then
    caddy_domain="$(caddyfile_domain "$CADDYFILE")"
    if [[ -z "$caddy_domain" ]]; then
      warn "$CADDYFILE is fleet-managed but has no site block — rerun: sudo fleet bootstrap --enable-web --domain <fqdn>"
    else
      caddy_rendered="$(render_fleet_caddyfile "$caddy_domain" "$(caddyfile_acme_email "$CADDYFILE")" \
        "$(env_get FLEET_SERVER_ADDR "$backend_env_file")" "$(env_get FLEET_ORCHESTRATOR_ADDR "$backend_env_file")")"
      caddy_diff="$(diff -u --label "$CADDYFILE (installed)" --label "scripts/lib/caddyfile.sh (shipped)" \
        <(caddyfile_functional_body "$CADDYFILE") <(printf '%s\n' "$caddy_rendered" | caddyfile_functional_body) 2>/dev/null || true)"
      if [[ -n "$caddy_diff" ]]; then
        warn "$CADDYFILE differs functionally from the layout this release ships — the /v1 API + inbound webhooks may not be routed to the Go backends (API clients get the web tier's 404)."
        do_adopt=0
        if [[ "$DRY_RUN" == "1" ]]; then
          info "[dry-run] would offer to rewrite $CADDYFILE for ${caddy_domain} from scripts/lib/caddyfile.sh (timestamped backup, caddy validate) and reload caddy"
          show_unit_diff "$caddy_diff"
        elif [[ "$ADOPT_UNITS" == "1" ]]; then
          do_adopt=1
        elif [[ -t 0 && "$ASSUME_YES" != "1" ]]; then
          show_unit_diff "$caddy_diff"
          printf '%s?%s Rewrite %s%s%s for %s from the shipped layout? %s(timestamped backup, caddy validate, systemctl reload caddy) (y/N)%s ' \
            "$c_cyan" "$c_reset" "$c_bold" "$CADDYFILE" "$c_reset" "$caddy_domain" "$c_dim" "$c_reset"
          read -r answer
          case "${answer,,}" in y|yes) do_adopt=1 ;; esac
        fi
        if [[ "$do_adopt" == "1" ]]; then
          caddy_backup="${CADDYFILE}.fleet-backup.$(date -u +%Y%m%dT%H%M%SZ)"
          if cp -p "$CADDYFILE" "$caddy_backup" 2>/dev/null && printf '%s\n' "$caddy_rendered" > "$CADDYFILE" 2>/dev/null; then
            if command -v caddy >/dev/null 2>&1 && ! caddy validate --adapter caddyfile --config "$CADDYFILE" >/dev/null 2>&1; then
              cp -p "$caddy_backup" "$CADDYFILE"
              warn "the rendered Caddyfile failed \`caddy validate\` — restored ${caddy_backup}; run: caddy validate --adapter caddyfile --config $CADDYFILE"
            else
              if systemctl is-active --quiet caddy.service 2>/dev/null; then
                systemctl reload caddy.service >/dev/null 2>&1 || systemctl restart caddy.service >/dev/null 2>&1 \
                  || warn "caddy reload failed — journalctl -u caddy -n 50"
              fi
              ok "rewrote $CADDYFILE for ${caddy_domain} (previous file: ${caddy_backup}); caddy reloaded — the /v1 API + webhooks now route to the Go backends"
            fi
          else
            warn "could not rewrite $CADDYFILE (need root?) — previous file left in place"
          fi
        elif [[ "$DRY_RUN" != "1" ]]; then
          warn "  review: diff $CADDYFILE <(bash -c '. $SRC_DIR/scripts/lib/caddyfile.sh; render_fleet_caddyfile ${caddy_domain}')"
          warn "  adopt:  sudo fleet update --adopt-units   (or: sudo fleet doctor, which rewrites a drifted fleet-managed Caddyfile without asking)"
        fi
      fi
    fi
  fi
fi

# ── offer to install missing scheduled-maintenance timer pairs ──────────────
# The drift loop above reconciles only units that are ALREADY installed; a box
# provisioned before the timers shipped (or with bootstrap's --no-*-timer
# opt-outs) has none, and until now the only path to them was copy-pasting the
# install hint out of `fleet doctor`. An interactive update asks once per
# missing pair, defaulting to No — an operator who backs up at the volume layer
# or prunes the container store another way is not misconfigured — and a
# non-interactive run just prints the one-liner. --no-timers (env
# FLEET_UPDATE_OFFER_TIMERS=0) silences both so a deliberate decline never
# nags. The install itself is `fleet timers install` — the binary this update
# just installed — so there is ONE install implementation, shared with the
# doctor hint, not a second copy here.
if command -v systemctl >/dev/null 2>&1 && [[ "$OFFER_TIMERS" != "0" ]]; then
  # The freshly installed binary; on an in-place dev box INSTALL_DIR == SRC_DIR
  # so this still resolves. PATH fallback covers the dry-run path (INSTALL_DIR
  # may be unresolved there) — dry-run never executes it anyway.
  fleet_bin="${INSTALL_DIR:-/opt/fleet}/fleet"
  [[ -x "$fleet_bin" ]] || fleet_bin="fleet"
  for _pair in \
    "backup|daily database dumps at 02:00|skip if you back up at the volume/hypervisor layer" \
    "maintenance|daily podman layer + build-cache prune at 03:30|skip if you prune the container store yourself"; do
    IFS='|' read -r _name _what _skip <<<"$_pair"
    # Only a fully-missing pair is offered: a half-installed or drifted pair
    # was already handled (or hinted about) by the drift loop above.
    if systemctl cat "fleet-${_name}.service" >/dev/null 2>&1 || systemctl cat "fleet-${_name}.timer" >/dev/null 2>&1; then
      continue
    fi
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would offer to install + enable the fleet-${_name} service/timer pair (${_what}) via: fleet timers install --${_name}"
    elif [[ -t 0 && "$ASSUME_YES" != "1" ]]; then
      printf '%s?%s Install + enable %sfleet-%s.timer%s? %s(%s; %s) (y/N)%s ' \
        "$c_cyan" "$c_reset" "$c_bold" "$_name" "$c_reset" "$c_dim" "$_what" "$_skip" "$c_reset"
      read -r answer
      case "${answer,,}" in
        y|yes)
          if "$fleet_bin" timers install "--${_name}" --src "$SRC_DIR"; then
            ok "fleet-${_name}.timer installed + enabled (${_what})"
          else
            warn "could not install the fleet-${_name} pair — try by hand: sudo fleet timers install --${_name}"
          fi
          ;;
        *) info "skipped — install later with: sudo fleet timers install --${_name} (or silence this offer: fleet update --no-timers)" ;;
      esac
    else
      info "no fleet-${_name}.timer on this box (${_what}) — install with: sudo fleet timers install --${_name} (${_skip}; --no-timers silences this)"
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
# Reported before the dry-run early exit below: --dry-run is what a cautious
# operator runs FIRST, and "is my bundle actually current?" is exactly the
# question it should answer. Printing this only on a real run would hide the
# finding from the run most likely to be looking for it.
report_bundle_state() {
  if [[ "$BUNDLE_STALE" == "1" ]]; then
    warn "the client-config bundle did NOT advance: ${BUNDLE_STALE_WHY}."
    warn "  fleet is new, your bundle is not — connector names/descriptions, personas, protocols and"
    warn "  the MCP catalog all come from the bundle, so the UI will still show the old ones."
    warn "  Inspect: git -C ${CLIENT_DIR} status -sb"
    warn "  If it is on the wrong branch, put it back on the default and re-run: sudo fleet update"
    say
  elif [[ -n "${bundle_sha_after:-}" ]]; then
    if [[ -n "$bundle_sha_before" && "$bundle_sha_before" != "${bundle_sha_after:-}" ]]; then
      say "  ${c_green}✓${c_reset} bundle ${CLIENT_DIR} updated ${bundle_sha_before:0:12} → ${bundle_sha_after:0:12}"
    else
      say "  ${c_dim}» bundle ${CLIENT_DIR} already current at ${bundle_sha_after:0:12}${c_reset}"
    fi
    say
  fi
}
if [[ "$DRY_RUN" == "1" ]]; then
  report_bundle_state
fi
# A dry run must not sign off with the same green "rebuilt/updated" banner a
# real run prints — it built nothing, and on a box the node gate would stop it
# reported a blocker three steps ago. Saying so here is the difference between
# a plan and a claim.
if [[ "$DRY_RUN" == "1" ]]; then
  if [[ "$DRY_RUN_BLOCKED" == "1" ]]; then
    printf '%s═══════════════════════════════════════════════%s\n' "$c_yellow" "$c_reset"
    printf '%s ! dry run: plan printed, and a BLOCKER above would stop the real run%s\n' "$c_bold" "$c_reset"
    printf '%s═══════════════════════════════════════════════%s\n' "$c_yellow" "$c_reset"
  else
    printf '%s═══════════════════════════════════════════════%s\n' "$c_dim" "$c_reset"
    printf '%s » dry run: plan only — nothing was built, installed or restarted%s\n' "$c_bold" "$c_reset"
    printf '%s═══════════════════════════════════════════════%s\n' "$c_dim" "$c_reset"
  fi
  say
  exit 0
fi
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
if [[ "$before_sha" == "$after_sha" ]]; then
  printf '%s ✓ fleet rebuilt at %s%s\n' "$c_bold" "${after_sha:0:12}" "$c_reset"
else
  printf '%s ✓ fleet updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
fi
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
# The banner above reports FLEET's sha. A deployment is fleet + its bundle, and
# connector copy, personas, protocols and the MCP catalog all live in the
# bundle — so an update that advanced fleet while the bundle stood still is a
# half-update, and saying "✓ fleet updated" alone is how that goes unnoticed.
report_bundle_state
say "  Health:    ${c_dim}fleet-admin status${c_reset}"
say "  Logs:      ${c_dim}journalctl -u ${SERVICE_NAME} -n 50${c_reset}"
if [[ "$before_sha" != "$after_sha" ]]; then
  say "  Roll back: ${c_dim}cd $SRC_DIR && git checkout $before_sha && scripts/update.sh --no-pull${c_reset}"
fi
