#!/usr/bin/env bash
# scripts/update.sh — in-place upgrade for an existing /opt/chat install.
#
# Philosophy: build in a staging dir, swap atomically, restart. If the
# build fails, nothing gets touched. Preserves .env.local, data/, and
# the Postgres database (which lives in /var/lib/pgsql/data, outside
# anything this script touches).
#
# Invoked by the `chat update` subcommand. Can also be run directly on
# the host with no args.
#
# Non-interactive by design — prints what it's about to do, asks for a
# single yes/no unless CHAT_UPDATE_YES=1, then runs.
#
# Env overrides:
#   SRC_DIR            where to pull from (default: /opt/chat-src)
#   APP_DIR            where to deploy    (default: /opt/chat)
#   CHAT_UPDATE_YES=1  skip the confirm prompt
#   CHAT_UPDATE_BRANCH override the branch checked out in SRC_DIR
#   CHAT_UPDATE_NO_PULL=1  skip fetch/fast-forward; just rebuild current checkout

set -euo pipefail

SRC_DIR="${SRC_DIR:-/opt/chat-src}"
APP_DIR="${APP_DIR:-/opt/chat}"
APP_USER="${APP_USER:-chat}"

# Colors — TTY-gated, same discipline as bootstrap.sh.
if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
fi

say()  { printf '%s\n' "$*"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root: sudo chat update"

[[ -d "$SRC_DIR/.git" ]] || die "no git checkout at $SRC_DIR"
[[ -d "$APP_DIR" ]]      || die "no existing install at $APP_DIR (did you skip bootstrap?)"

# ── 1. fetch ────────────────────────────────────────────────────────────
step "1/4  Fetching latest from $SRC_DIR"

cd "$SRC_DIR"
# Work around 'safe.directory' by telling git we're good. $SRC_DIR is
# owned by root since bootstrap cloned it; git 2.35+ requires this.
git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true

before_sha="$(git rev-parse HEAD)"

if [[ "${CHAT_UPDATE_NO_PULL:-0}" == "1" ]]; then
  after_sha="$before_sha"
  # Set when a pull re-execs the fresh script (below) so the final
  # summary still shows the real old → new range.
  before_sha="${CHAT_UPDATE_BASE_SHA:-$before_sha}"
  ok "rebuild-only mode — skipping fetch, building ${after_sha:0:12}"
  say
else
  git fetch --quiet origin

  # Resolve target branch.  If HEAD is attached we simply follow it.
  # If detached, try to recover from a local branch at this commit;
  # otherwise fall back to the repo's default branch (origin/HEAD).
  current_branch="$(git rev-parse --abbrev-ref HEAD)"
  if [[ -n "${CHAT_UPDATE_BRANCH:-}" ]]; then
    target_branch="$CHAT_UPDATE_BRANCH"
  elif [[ "$current_branch" != "HEAD" ]]; then
    target_branch="$current_branch"
  else
    # HEAD is detached — look for a local branch that already points here.
    mapfile -t matching < <(git branch --points-at HEAD --format='%(refname:short)')
    if [[ ${#matching[@]} -eq 1 ]]; then
      target_branch="${matching[0]}"
      warn "HEAD is detached — recovering tracked branch '$target_branch'"
    elif [[ ${#matching[@]} -gt 1 ]]; then
      target_branch="${matching[0]}"
      warn "HEAD is detached — multiple local branches match; using '$target_branch'"
    else
      target_branch="$(git rev-parse --abbrev-ref origin/HEAD | sed 's|^origin/||')"
      warn "HEAD is detached — defaulting to '$target_branch'"
      warn "  (set CHAT_UPDATE_BRANCH to override)"
    fi
  fi
  target_ref="origin/$target_branch"
  after_sha="$(git rev-parse "$target_ref")"

  if [[ "$before_sha" == "$after_sha" ]]; then
    ok "already on ${after_sha:0:12} — nothing to update"
    exit 0
  fi

  # Preview what's changing. `git log a..b` gives a readable "incoming".
  say
  printf '%s  incoming commits:%s\n' "$c_dim" "$c_reset"
  git --no-pager log --oneline --no-decorate "${before_sha}..${after_sha}" | sed 's/^/    /'
  say

  # ── 2. confirm ──────────────────────────────────────────────────────────
  if [[ "${CHAT_UPDATE_YES:-0}" != "1" ]]; then
    count="$(git rev-list --count "${before_sha}..${after_sha}")"
    printf '%s?%s Apply %s%d%s commits — %s..%s? %s(y/N)%s ' \
      "$c_cyan" "$c_reset" \
      "$c_bold" "$count" "$c_reset" \
      "${before_sha:0:12}" "${after_sha:0:12}" \
      "$c_dim" "$c_reset"
    read -r answer
    if [[ "${answer,,}" != "y" && "${answer,,}" != "yes" ]]; then
      warn "cancelled"
      exit 1
    fi
  fi

  # Stay on a local branch — never detach HEAD.  If the branch already
  # exists, fast-forward it; otherwise create it from the fetched ref.
  if git show-ref --quiet --verify "refs/heads/$target_branch"; then
    git checkout --quiet "$target_branch"
    git merge --ff-only "$target_ref" || die "$target_branch has diverged from $target_ref — resolve manually"
  else
    git checkout --quiet -b "$target_branch" "$target_ref"
  fi

  # The shell running this script read the PRE-update file (bash holds the
  # old inode across the checkout above), so a fix to update.sh itself
  # would otherwise only take effect on the NEXT update. If this update
  # changed update.sh, re-exec the fresh copy in rebuild-only mode.
  if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
    warn "update.sh changed in this update — re-executing the new version"
    exec env CHAT_UPDATE_NO_PULL=1 CHAT_UPDATE_YES=1 \
      CHAT_UPDATE_BASE_SHA="$before_sha" bash "$SRC_DIR/scripts/update.sh"
  fi
fi

# ── 3. build in a staging dir ───────────────────────────────────────────
step "2/4  Building new artifacts (staging)"

STAGING="$(mktemp -d)"

# services_down tracks the stop→start window. If the script dies after
# stopping the units (missing env, dnf failure, registry-unreachable
# podman pull) but before restarting them, the box would otherwise sit
# hard-down until an operator notices and runs `systemctl start`. The
# cleanup trap brings the OLD binaries back up — a failed update becomes
# a no-op instead of an outage. Cleared right after the successful start.
services_down=0
cleanup() {
  rm -rf "$STAGING"
  if [[ "$services_down" == "1" ]]; then
    printf '%s✗ update aborted with services stopped — restarting previous version%s\n' \
      "$c_red" "$c_reset" >&2
    systemctl start chat-server.service chat-web.service || true
  fi
}
trap cleanup EXIT

# Rsync source into staging with the same exclusions as bootstrap, plus
# we bring in existing node_modules/.next to speed up incremental builds.
# /workspace is a runtime scratch dir declared in the systemd unit's
# ReadWritePaths=; if rsync --delete nukes it, the next chat-server boot
# fails at the NAMESPACE step.
rsync -a --delete \
  --exclude='/.git' \
  --exclude='/data' \
  --exclude='/workspace' \
  --exclude='/.env.local' \
  --exclude='/bin' \
  --exclude='/.local' \
  --exclude='/.config' \
  "$SRC_DIR/" "$STAGING/"
# Seed caches from the live install so npm + next don't redo everything.
for cache in node_modules .next; do
  if [[ -d "$APP_DIR/$cache" ]]; then
    cp -a "$APP_DIR/$cache" "$STAGING/" 2>/dev/null || true
  fi
done

# Build the Go binaries + Next.js bundle AS the chat user (so artifact
# ownership matches the final install).
chown -R "$APP_USER:$APP_USER" "$STAGING"

sudo -u "$APP_USER" -H bash -c "
  set -euo pipefail
  # Opt out of Next.js telemetry during the build step.
  export NEXT_TELEMETRY_DISABLED=1
  # Pin BUILD_ID to the incoming SHA so next.config.ts's cache-busting
  # identifier is traceable back to a specific commit. STAGING doesn't
  # have .git (rsynced with --exclude=/.git), so next.config.ts's
  # fallback 'git rev-parse' would fail and land on a random hex — still
  # cache-busting-correct but unhelpful when debugging which deploy a
  # client is stuck on.
  export BUILD_ID='$after_sha'
  cd '$STAGING/server'
  GOTOOLCHAIN=auto go build -o '$STAGING/bin/chat-server'   ./cmd/chat-server
  GOTOOLCHAIN=auto go build -o '$STAGING/bin/chat-admin'    ./cmd/chat-admin
  GOTOOLCHAIN=auto go build -o '$STAGING/bin/sandbox-probe' ./cmd/sandbox-probe
  cd '$STAGING'
  npm install --no-audit --no-fund --loglevel=warn
  npm run build
"
ok "staging build complete (BUILD_ID=${after_sha:0:12})"

# Python deps for the MCP subprocesses (run as host stdio children of
# chat-server). The run_python tool itself runs INSIDE the per-turn
# sandbox container — those deps live in the container image, not here.
#
# uv pip install --reinstall: writes into the system site-packages with
# proper transitive resolution. We previously tried `uv pip sync` for
# its automatic uninstall-of-dropped-deps property, but sync expects a
# fully-resolved lockfile (pip-compile output) — fed a hand-written
# constraints file like requirements.txt, sync skips transitive
# resolution and fails at import time on anything pinned by a transitive
# (e.g. jmespath, which botocore needs). install --reinstall keeps the
# resolution; the explicit uv pip uninstall block below handles cleanup
# of dropped deps.
#
# --break-system-packages: PEP 668 ships an "EXTERNALLY-MANAGED"
# marker on Fedora/Debian that blocks system-level pip installs by
# default. We explicitly bypass it because chat-server needs these
# libraries available to its (hardened, ProtectSystem=strict) systemd
# sandbox, and a venv would add complexity that doesn't pay for itself
# here.
#
# --reinstall: uv's default `pip install -r requirements.txt` short-
# circuits when every listed package looks satisfied, which misses
# half-installed transitive trees on new Pythons. Forcing reinstall
# costs ~30s per update but guarantees no torn environment ships.
uv pip install --system --no-cache --break-system-packages --reinstall \
  -r "$STAGING/server/requirements.txt" \
  || die "python deps install (uv) failed — chat-server will not boot; see output above"

# Drop packages that used to be host-side run_python kernel deps. With
# run_python now exclusively inside the per-turn container image (which
# has its own Python + Jupyter kernel), these are dead weight on the
# host — uninstalling here reclaims ~50MB plus a few native wheels.
# Idempotent: if a package isn't installed (already-converged box,
# fresh bootstrap), uv returns non-zero quietly and the `|| true` lets
# us proceed. We can drop this block once the fleet has all converged.
uv pip uninstall --system --break-system-packages \
  ipykernel ipython jupyter-client pyzmq pandas numpy >/dev/null 2>&1 || true
ok "python deps installed (uv)"

# `uv pip install --break-system-packages` on Fedora's PEP 668 python
# creates /usr/local/lib{,64}/pythonX.Y/ from scratch under sudo's 0077
# umask — which makes the tree unreadable to non-root. Root's import
# check below still passes, but the 'chat' user (which the service
# runs as) then hits ModuleNotFoundError on every MCP subprocess spawn.
# Fix both /lib (pure-Python) and /lib64 (compiled extensions like
# aiohttp's _frozenlist) so every user's python can traverse it.
for d in /usr/local/lib/python3.* /usr/local/lib64/python3.*; do
  [[ -d "$d" ]] && chmod -R o+rX "$d"
done

# Sanity: verify the critical imports succeed under the system python
# AS THE CHAT USER — root-side imports aren't sufficient because the
# service spawns python3 as 'chat', which has different sys.path and
# filesystem-access characteristics. If a package installed to the
# wrong site-packages (parallel Python version, --user dir hidden by
# ProtectHome, or 0700-perms on /usr/local/lib/pythonX.Y), this
# catches it BEFORE the swap so the live service keeps running on the
# old binary. Probe one symbol from each MCP subprocess dep — Jupyter
# kernel stack is no longer host-side and not part of this check.
python3 -c "import aioboto3, mcp.server.fastmcp, sendgrid, httpx, html5lib" \
  || die "post-install import check failed — aioboto3 / mcp / sendgrid / httpx / html5lib must be importable under $(python3 -c 'import sys;print(sys.executable)')"
sudo -u "$APP_USER" /usr/bin/python3 -c "import aioboto3, mcp.server.fastmcp, sendgrid, httpx, html5lib" \
  || die "post-install import check failed for user '$APP_USER' — check /usr/local/lib/python3.*/site-packages perms"

# ── 4. atomic swap + restart ────────────────────────────────────────────
step "3/4  Swapping in and restarting services"

# Read the sandbox image up front so both the pull (below) and the smoke
# test (in step 4/4) see the same value, and so a stray edit to
# .env.local mid-update can't desync them.
# Strip surrounding single OR double quotes — the Go config loader
# accepts either, so a value quoted in .env.local must parse the same way
# here or the pull below fails on a value chat-server itself would accept.
SANDBOX_IMAGE_FROM_ENV="$(grep -E '^CHAT_SANDBOX_IMAGE=' "$APP_DIR/.env.local" 2>/dev/null | tail -n1 | cut -d= -f2- | tr -d "\"'" || true)"

# Stop the actual service units so the binaries aren't mid-serve during
# the swap. `stop chat.target` only stops the target itself unless every
# child has PartOf=chat.target — and we can't rely on that until this
# update has propagated to every box. Hitting the units is unambiguous.
systemctl stop chat-server.service chat-web.service || true
services_down=1

# Refresh the sandbox image during the planned downtime (after services
# stop, before they come back up). Three reasons:
#
#   1. Security: image updates ride along with `chat update` instead of
#      sitting stale until the operator remembers to rerun
#      `chat sandbox default`. The sandbox image pulls in dnf-managed
#      CVE patches whenever it's rebuilt.
#   2. Determinism: the smoke test in step 4/4 exercises the image that
#      just landed — not whatever the operator pulled last week.
#   3. Cache warming: the post-restart Pool.fill triggers podman's
#      ID-mapped layer chown on the first `podman run` after a pull
#      (the chowned layer cache is keyed by source layer ID, so a new
#      pull always invalidates it). Doing the pull HERE keeps the
#      one-time chown cost off a real user's first turn.
#
# Pull as the chat user via the same incantation sandbox-mode.sh uses,
# so storage/config land in the same /opt/chat/.local/.config tree
# rootless podman expects. `cd $APP_DIR` first because sudo preserves
# CWD by default — if the operator ran `chat update` from a directory
# the chat user can't read (e.g. /root), podman's setup phase would
# `cannot chdir` before it ever talks to the registry. Stderr is left
# visible so progress + auth/network errors aren't swallowed; stdout is
# silenced because the layer-by-layer digest dump is noisy.
#
# Containers are mandatory in this build — chat-server refuses to start
# without CHAT_SANDBOX_IMAGE. We treat a missing env value as a
# misconfigured install and fail update, rather than letting a stale
# config slide through.
if [[ -z "$SANDBOX_IMAGE_FROM_ENV" ]]; then
  die "CHAT_SANDBOX_IMAGE not set in $APP_DIR/.env.local — chat-server will not start. Run: sudo chat sandbox default"
fi

# Install + configure rootless-podman if the operator's box predates
# the containers-only requirement. Boxes that went through bootstrap
# AFTER PR #95 already have podman and the rootless prereqs; older
# boxes have CHAT_SANDBOX_IMAGE in .env.local but no podman binary
# (the dnf install line in bootstrap.sh hadn't been added when they
# were provisioned). Without this self-heal the next podman pull
# below dies with "podman: command not found", which is a confusing
# error mode for the operator — they can see they're configured for
# containers, but chat-server won't start.
#
# Mirrors sandbox-mode.sh:ensure_prereqs intentionally — we don't
# share via a sourced helper because update.sh runs as root in a
# minimal env and the operational coupling between the two scripts
# is light enough that duplicating ~10 lines is cheaper than
# introducing a shared lib. All operations are idempotent.
if ! command -v podman >/dev/null 2>&1; then
  printf '  %sinstalling podman (missing on this box, needed for per-turn sandbox)%s\n' "$c_dim" "$c_reset"
  dnf install -y --quiet podman || die "dnf install podman failed — install manually then re-run"
fi
if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
  usermod --add-subuids 100000-165535 "$APP_USER"
fi
if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
  usermod --add-subgids 100000-165535 "$APP_USER"
fi
loginctl enable-linger "$APP_USER" >/dev/null 2>&1 || true
install -d -o "$APP_USER" -g "$APP_USER" \
  "$APP_DIR/.local/share/containers" \
  "$APP_DIR/.config/containers"
chown -R "$APP_USER:$APP_USER" "$APP_DIR/.local" "$APP_DIR/.config"

# Reset the chat user's rootless podman pause container before pulling.
# Rationale: rootless podman keeps a long-lived pause process (catatonit)
# under the chat user's lingering systemd user manager; that process
# holds the user AND mount namespaces every subsequent `podman` call
# joins. Whoever forked the pause first wins — and on a running box that
# was chat-server.service, whose ProtectSystem=strict + ReadWritePaths=
# leave /var/tmp read-only inside the namespace. containers/image uses
# /var/tmp as the big-files staging dir for pulls, so the pull errors
# with `EROFS: mkdir /var/tmp/container_images_storage…` — even though
# /var/tmp is rw on the host and rw to a fresh chat-user shell. The
# `systemctl stop` above doesn't help: linger-owned processes survive
# the service stop. `podman system migrate` is the documented way to
# kill the pause and reset the rootless network namespace; on next
# podman invocation a fresh pause forks from THIS shell (root, no
# hardening), so /var/tmp is writable. Idempotent.
sudo -u "$APP_USER" -H podman system migrate >/dev/null 2>&1 || true

printf '  %srefreshing sandbox image %s …%s\n' "$c_dim" "$SANDBOX_IMAGE_FROM_ENV" "$c_reset"
if ! ( cd "$APP_DIR" && sudo -u "$APP_USER" -H podman pull "$SANDBOX_IMAGE_FROM_ENV" >/dev/null ); then
  die "podman pull $SANDBOX_IMAGE_FROM_ENV failed — see error above; common causes: registry unreachable / auth required / rootless storage borked under /opt/chat/.local"
fi
ok "sandbox image refreshed"

# Copy new source tree over (excluding data/, .env.local, existing bin/,
# the workspace scratch dir, and the rootless-podman home dirs — see
# note on the staging rsync above).
#
# /.local and /.config are NOT in the source tree but ARE in $APP_DIR
# at runtime (created by sandbox-mode.sh's ensure_prereqs to host
# rootless podman's storage and config). Without these excludes,
# `rsync --delete` would nuke them on every update — every Pool.fill
# would then fail (`stat /opt/chat/.config: no such file or directory`),
# and since there's no host-mode fallback anymore, every chat would
# error with "sandbox unavailable" until the operator reran
# `chat sandbox default` (which calls ensure_prereqs again). The
# excludes keep the rootless-podman home stable across updates.
rsync -a --delete \
  --exclude='/.git' \
  --exclude='/data' \
  --exclude='/workspace' \
  --exclude='/.env.local' \
  --exclude='/bin' \
  --exclude='/.local' \
  --exclude='/.config' \
  "$STAGING/" "$APP_DIR/"

# Belt-and-suspenders: ensure the runtime dirs declared in the systemd
# unit's ReadWritePaths= exist and are owned by chat. Cheap idempotent
# mkdirs so older boxes that pre-date /workspace get it on first update.
install -d -o "$APP_USER" -g "$APP_USER" "$APP_DIR/workspace" "$APP_DIR/data/audit"

# Re-assert the rootless-podman home dirs in case this is the first
# update on a box where they got wiped by an older update.sh (the bug
# the rsync excludes above closes prospectively). Mirrors the subset
# of sandbox-mode.sh's ensure_prereqs that's specific to runtime state
# — we deliberately don't redo subuid/subgid or enable-linger here
# because those persist across updates and re-running `usermod
# --add-subuids` would no-op or noisily warn.
#
# Gated on CHAT_SANDBOX_IMAGE so host-mode-only operators (no podman)
# don't accumulate empty .local/.config trees they don't use.
if grep -q '^CHAT_SANDBOX_IMAGE=' "$APP_DIR/.env.local" 2>/dev/null; then
  install -d -o "$APP_USER" -g "$APP_USER" \
    "$APP_DIR/.local/share/containers" \
    "$APP_DIR/.config/containers"
  # `install -d` only sets ownership on dirs it CREATES. If .local /
  # .config existed already but with wrong ownership (e.g. created by
  # hand as root mid-debugging), rootless podman would refuse to write
  # into them ("path exists and is not owned by the current user").
  # chown to be safe — same belt-and-suspenders sandbox-mode.sh does.
  chown -R "$APP_USER:$APP_USER" "$APP_DIR/.local" "$APP_DIR/.config"
fi

# Seed the outgoing-email From address on boxes bootstrapped before it
# existed (bootstrap.sh writes it for new installs). Both email MCPs
# default to this value in code too; writing it into .env.local makes it
# visible and editable instead of implicit. Append-if-missing so an
# operator's customized value is never touched.
_from_lines=""
for _from_var in SENDGRID_FROM_EMAIL MAILBUX_FROM_EMAIL; do
  if ! grep -q "^${_from_var}=" "$APP_DIR/.env.local" 2>/dev/null; then
    _from_lines="${_from_lines}${_from_var}=\"victoria@elcanotek.com\"\n"
  fi
done
if [[ -n "$_from_lines" ]]; then
  {
    printf '\n# Default From address for outgoing email (added by update.sh)\n'
    printf '%b' "$_from_lines"
  } >> "$APP_DIR/.env.local"
fi

# Clean up the obsolete top-level symlinks (see matching block in
# bootstrap.sh). These used to expose server/{personas,protocols,...}
# at the app root, but they broke resolveBaseDir and are now
# redundant — per-conversation workspaces carry their own scoped
# symlinks created by EnsureWorkspaceDir.
for d in personas protocols system_prompts; do
  if [[ -L "$APP_DIR/$d" ]]; then
    rm -f "$APP_DIR/$d"
  fi
done
# Move the freshly-built binaries into place.
install -o "$APP_USER" -g "$APP_USER" -m 0755 "$STAGING/bin/chat-server"   "$APP_DIR/bin/chat-server"
install -o "$APP_USER" -g "$APP_USER" -m 0755 "$STAGING/bin/chat-admin"    "$APP_DIR/bin/chat-admin"
install -o "$APP_USER" -g "$APP_USER" -m 0755 "$STAGING/bin/sandbox-probe" "$APP_DIR/bin/sandbox-probe"
# Refresh the Next.js production bundle (Next is fussy about .next so we
# rsync the full tree into place).
rm -rf "$APP_DIR/.next"
cp -a "$STAGING/.next" "$APP_DIR/.next"
chown -R "$APP_USER:$APP_USER" "$APP_DIR/.next" "$APP_DIR/node_modules" 2>/dev/null || true

# Reinstall the systemd units in case they changed between versions.
install -m 0644 "$APP_DIR/deploy/chat-server.service" /etc/systemd/system/
install -m 0644 "$APP_DIR/deploy/chat-web.service"    /etc/systemd/system/
install -m 0644 "$APP_DIR/deploy/chat.target"         /etc/systemd/system/
install -m 0755 "$APP_DIR/deploy/chat-cli"            /usr/local/bin/chat
systemctl daemon-reload

# Start the service units explicitly. `start chat.target` would also
# work for fresh starts, but being explicit avoids the asymmetry with
# the stop above.
systemctl start chat-server.service chat-web.service
services_down=0
ok "services restarted"

# ── 5. health check ─────────────────────────────────────────────────────
step "4/4  Health check"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    ok "chat-server healthy"
    break
  fi
  sleep 1
  if [[ "$i" == "10" ]]; then
    die "chat-server didn't come back up — check: journalctl -u chat-server -n 50"
  fi
done

# Sandbox smoke test. /healthz only proves the HTTP listener is up; it
# does NOT exercise the per-turn rootless-podman path. A deploy that
# breaks bash + run_python connectors (e.g. the system.slice cgroup-driver
# regression that motivated this check) would still pass /healthz and
# then break every user's first tool call.
#
# We run sandbox-probe as the chat user — same uid, same rootless-podman
# storage at /opt/chat/.local, same cgroup delegation, same image — so
# Pool.Take + Pool.TakeContainer get exercised against the real podman
# environment users hit on real turns. We do NOT try to mirror
# chat-server.service's full systemd hardening profile here: the live
# service starting up successfully (verified by /healthz above) is the
# authoritative proof that the hardening loaded.
#
# Containers are mandatory in this build; the image-pull step above
# already failed if SANDBOX_IMAGE_FROM_ENV was unset, so we can run
# unconditionally here.
step "  sandbox smoke (bash + run_python + supporting docs, normal + lockdown)"
# SANDBOX_SUPPORTING lists the host dirs that should bind-mount into
# the per-turn container at the SAME absolute path. The agent's
# tools/workspace.go drops symlinks pointing at these into every
# per-conversation workspace; without same-path mounting the symlinks
# dangle inside the container and `cat personas/foo.yaml` from bash
# in lockdown chats fails — even though host-side view_file works.
# The smoke probe additionally verifies it can `cat` a real file from
# one of these dirs from inside the container, so a missing or broken
# bind mount trips the script's exit-non-zero before users hit the bug.
if ( cd "$APP_DIR" && sudo -u "$APP_USER" -H \
       SANDBOX_IMAGE="$SANDBOX_IMAGE_FROM_ENV" \
       SANDBOX_SUPPORTING="$APP_DIR/server/personas:$APP_DIR/server/protocols:$APP_DIR/server/system_prompts" \
       "$APP_DIR/bin/sandbox-probe" ) 2>&1 | sed 's/^/    /'; then
  ok "sandbox smoke passed (NORMAL + LOCKDOWN, bash + python + docs)"
else
  die "sandbox smoke failed — bash/run_python would not work for users; rolling back is recommended"
fi

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Logs:  ${c_dim}chat logs${c_reset}"
say "  Roll back: cd $SRC_DIR && sudo git checkout $before_sha && sudo chat update"
