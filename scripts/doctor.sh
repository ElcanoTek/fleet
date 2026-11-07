#!/usr/bin/env bash
# scripts/doctor.sh — diagnose and repair a fleet box.
#
# `fleet doctor` walks every box-level prerequisite fleet depends on and
# FIXES what it can in place; `fleet doctor --check` reports without touching
# anything. The goal is killing the version lottery: boxes provisioned at
# different times drift apart (podman/crun versions with different rootless
# behavior, stale systemd units, missing subuid ranges,
# root-owned podman dirs...) and each drift shows up later as a confusing
# production-only bug. bootstrap.sh and update.sh carry their own inline
# self-heals for the provision/deploy paths; doctor is the standalone,
# run-anytime superset that also covers what those scripts assume is already
# right.
#
# What it does NOT do: git pull or install a release — that's `fleet update`'s
# job. There is no build toolchain to check: boxes install signed release
# binaries (docs/RELEASE-ARTIFACTS.md), so Go and Node are not box dependencies.
# Doctor makes the box the code lands on healthy; update lands the code.
# (`fleet status` stays the quick read-only in-process report; doctor is the
# deep box-level pass with repairs.)
#
# Usage:
#   sudo fleet doctor              diagnose + fix + restart services if needed
#   sudo fleet doctor --check      diagnose only, change nothing, exit 1 if anything is off
#   sudo fleet doctor --no-restart fix but never restart services
#   fleet doctor --dry-run         print the checklist this box would be walked through; touch nothing
#
# Exit codes: 0 = healthy (or everything fixed), 1 = problems remain.

# Tracing off before anything is read: `bash -x` would print the env file's DSNs and other secrets.
{ set +x; } 2>/dev/null
set -euo pipefail

# ── locate this script + its repo root (default SRC_DIR) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
# These MUST match deploy/fleet.service and scripts/bootstrap.sh
# (SERVICE_USER/SERVICE_HOME there).
SERVICE_USER="${FLEET_SERVICE_USER:-fleet}"
SERVICE_HOME="${FLEET_SERVICE_HOME:-/var/lib/fleet}"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
ENV_FILE="${FLEET_ENV_FILE:-/etc/fleet/fleet.env}"
# The retired Next.js tier's env file (ADR-0091). Only READ now: when fleet.env
# lacks APP_SESSION_SECRET, step 5 carries the old tier's value over so existing
# sessions survive. Overridable so the script's tests can point it at a scratch
# file instead of this box's real one; it grants a caller nothing.
WEB_ENV_FILE="${FLEET_WEB_ENV_FILE:-/etc/fleet/fleet-web.env}"
# The scheduled-backup pair shipped in deploy/ and installed by
# scripts/bootstrap.sh --enable-service. Fixed names (unlike the fleet unit,
# which FLEET_SERVICE_NAME renames) because bootstrap installs exactly these —
# internal/boxdoctor probes the same two.
BACKUP_SERVICE="fleet-backup.service"
BACKUP_TIMER="fleet-backup.timer"
MAINT_SERVICE="fleet-maintenance.service"
MAINT_TIMER="fleet-maintenance.timer"

# The fleet-managed Caddyfile (marker, renderer, drift helpers) — shared with
# bootstrap.sh (which writes it) and update.sh (which offers to adopt it).
# shellcheck source=lib/caddyfile.sh
. "$SCRIPT_DIR/lib/caddyfile.sh"
# shellcheck source=lib/bundle.sh
. "$SCRIPT_DIR/lib/bundle.sh"
# Idempotent safe.directory trust for the root-run git calls below.
# shellcheck source=lib/git.sh
. "$SCRIPT_DIR/lib/git.sh"
# Retiring the Next.js web tier (ADR-0091); its ok/warn lines map onto
# doctor's info/advise (defined below, before anything calls it).
# shellcheck source=lib/web-tier.sh
. "$SCRIPT_DIR/lib/web-tier.sh"
ok()   { info "$@"; }
warn() { advise "$@"; }
CHECK_ONLY=0
NO_RESTART=0
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --check)      CHECK_ONLY=1 ;;
    --no-restart) NO_RESTART=1 ;;
    --dry-run)    DRY_RUN=1 ;;
    -h|--help)
      cat <<'EOF'
fleet doctor — diagnose and repair this fleet box

USAGE
  sudo fleet doctor              diagnose + fix + restart services if needed
  sudo fleet doctor --check      diagnose only, change nothing, exit 1 if anything is off
  sudo fleet doctor --no-restart fix but never restart services
  fleet doctor --dry-run         print the checklist; touch nothing (no root needed)

Checks + fixes: the runtime toolchain (git/curl/jq/podman/psql/python3 present —
no Go or Node: boxes install signed release binaries), the installed binary's
build (no host executor, web bundle embedded),
fleet-critical package currency (podman/crun/passt/conmon/...), the rootless-
podman prerequisites of the fleet service user (subuid/subgid, /var/lib/fleet
ownership, containers.conf; a stale pause process is reported with its manual
repair, never reset — the reset deletes a live sandbox pool), systemd unit drift vs
deploy/, the 0600 env files, the fleet-managed /etc/caddy/Caddyfile's layout
(the /v1 API + webhooks must reach the Go backends, not the web tier — rewritten
+ caddy reloaded when drifted) and FLEET_TRUSTED_PROXIES for that front (added
as 127.0.0.1,::1 when absent and the backends are on loopback; an operator's
value is left alone), APP_SESSION_SECRET for the web UI fleet serves, the
retired Next.js fleet-web tier (retired when still installed), service health
(postgresql, fleet, caddy), the /healthz + /readyz probes and https://<domain>/api-info THROUGH
caddy, the scheduled-backup and host-maintenance
timers, free disk on the data dir + image store, and a sandbox smoke (podman run
as the fleet user). Reports when the source checkout is behind upstream but never
pulls or installs — that stays `fleet update`.

`fleet status` is the quick read-only in-process report; doctor is the deep
box-level pass with repairs.
EOF
      exit 0
      ;;
    *) echo "error: unknown argument: $arg (try --help)" >&2; exit 1 ;;
  esac
done

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m'; c_dim=$'\033[2m'; c_red=$'\033[0;31m'
  c_green=$'\033[0;32m'; c_yellow=$'\033[0;33m'; c_cyan=$'\033[0;36m'; c_bold=$'\033[1m'
else
  c_reset=''; c_dim=''; c_red=''; c_green=''; c_yellow=''; c_cyan=''; c_bold=''
fi
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

n_ok=0 n_fixed=0 n_warn=0 n_fail=0
restart_needed=0
podman_recheck=0
stale_pause=0 # step 3 found a stale pause process; step 6 holds its restart
web_tier_cutover=0 # step 4 stopped the retired web tier; step 6 removes or restores it

pass()  { printf '  %s✓%s %s\n' "$c_green" "$c_reset" "$*"; n_ok=$((n_ok+1)); }
fixed() { printf '  %s↻%s %s\n' "$c_cyan" "$c_reset" "$*"; n_fixed=$((n_fixed+1)); }
advise(){ printf '  %s!%s %s\n' "$c_yellow" "$c_reset" "$*"; n_warn=$((n_warn+1)); }
fail()  { printf '  %s✗%s %s\n' "$c_red" "$c_reset" "$*"; n_fail=$((n_fail+1)); }

# env_get KEY [FILE] — read one key from an env file without sourcing it (the
# file holds secrets; sourcing would execute arbitrary content on a tampered
# box). Last assignment wins, surrounding quotes stripped.
env_get() {
  local key="$1" file="${2:-$ENV_FILE}"
  [[ -r "$file" ]] || return 0
  grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true
}

# upsert_env_key FILE KEY VALUE — set one key in an env file in place.
#
# Rewrites every occurrence of the key (not just the first) and appends it if
# absent, and NEVER truncates the file: it carries operator-added keys
# (AUTH_SIGNING_PUBKEY, AUTH_LOGIN_URL, …) that a wholesale rewrite silently
# dropped once already in bootstrap. mktemp next to the target + mv is a
# same-filesystem atomic rename, so a crash mid-write cannot leave the service
# with a half-written env file. Mode is forced to 0600: this file holds the
# backend tokens, and the shipped posture is 0600 root-owned.
#
# ONE awk pass handles both the replace and the append cases, via END. The
# earlier two-branch version used `cat` + `printf` to append, which corrupted
# any file whose last line lacked a trailing newline — the new key was glued
# onto it, destroying that line's value AND failing to set the key
# (`NODE_ENV=productionFLEET_NODE_BIN=…`). awk's print supplies ORS, so this
# normalizes a missing final newline instead of tripping over it. The sibling
# helpers in bootstrap.sh and update.sh already did it this way.
#
# ALL occurrences are collapsed because systemd's EnvironmentFile is LAST-WINS.
# Rewriting only the first left a stale later duplicate in charge, while the
# next read (below, via env_get, which takes the last) would report the value
# we just wrote — a check that passes forever while the box does the wrong thing.
upsert_env_key() {
  local file="$1" key="$2" val="$3" tmp
  [[ -f "$file" ]] || return 1
  tmp="$(mktemp "${file}.XXXXXX")" || return 1
  chmod 0600 "$tmp" || { rm -f "$tmp"; return 1; }
  # Key and value arrive via ENVIRON, not -v: awk performs ESCAPE PROCESSING on
  # -v assignments, so `-v v='a\1b'` silently loses the \1. ENVIRON does not.
  if ! _uwe_key="$key" _uwe_val="$val" awk '
        BEGIN { FS = "="; k = ENVIRON["_uwe_key"]; v = ENVIRON["_uwe_val"]; seen = 0 }
        $1 == k { if (!seen) { print k "=" v; seen = 1 } ; next }   # collapse duplicates
        { print }
        END { if (!seen) print k "=" v }
      ' "$file" > "$tmp"; then
    rm -f "$tmp"; return 1
  fi
  mv -f "$tmp" "$file" || { rm -f "$tmp"; return 1; }
  return 0
}

# run_as_fleet CMD... — run a command as the service user with the SAME
# HOME/XDG_RUNTIME_DIR deploy/fleet.service sets, so doctor probes the exact
# rootless-podman environment the daemon uses (not root's). It also cd's to
# the service HOME first: sudo keeps the caller's cwd, doctor is typically
# invoked from /root — a directory the fleet user cannot enter — and rootless
# podman re-execs inside its user namespace and chdir()s back to the
# inherited cwd, dying with "cannot chdir to /root: Permission denied"
# (learned in production on chat's doctor: info/migrate/pull all failed from
# /root while the already-cd'd smoke passed). Falls back to / (enterable by
# every user) when HOME doesn't exist yet — step 3 may be about to create it.
run_as_fleet() {
  ( cd "$SERVICE_HOME" 2>/dev/null || cd /
    sudo -u "$SERVICE_USER" HOME="$SERVICE_HOME" XDG_RUNTIME_DIR="/run/${SERVICE_USER}" "$@" )
}

# is_stale_pause_error TEXT — true when podman's own error names a stale
# rootless pause process ('invalid internal status, try resetting the pause
# process with "podman system migrate"'), the one condition migrate fixes.
is_stale_pause_error() {
  grep -qiE 'podman system migrate|pause process' <<<"$1"
}

# stale_pause_hint — what to do about a stale pause process, which doctor
# never resets itself. `podman system migrate` stops every running container
# of the service user, and fleet's sandboxes run --rm: under a live fleet it
# deletes the whole warm pool while the process keeps handing out the dead
# handles ("no such container" on every chat turn and task until a restart —
# learned in production on fleetdev). When fleet can safely be stopped is a
# judgment call for an operator or agent, so this is a pointer, not a script.
stale_pause_hint() {
  printf '%s' "when it is safe (nothing running), stop ${SERVICE_NAME}, run podman system migrate as ${SERVICE_USER}, and start ${SERVICE_NAME} again — see docs/OPERATORS.md, \"Stale podman pause process\""
}

# ── dry-run: print the checklist and exit ────────────────────────────────────
# Doctor's real run is condition-driven (it probes, then fixes what the probe
# found), so --dry-run enumerates the plan instead of half-executing it. This
# is also the CI seam: the Go smoke test asserts the plan's load-bearing steps.
if [[ "$DRY_RUN" == "1" ]]; then
  step "fleet doctor --dry-run (src=${SRC_DIR}, service=${SERVICE_NAME}, install=${INSTALL_DIR})"
  info "[dry-run] 1/9 Toolchain: git/curl/jq/podman/psql/python3 present (dnf install) — no Go or Node: the box installs signed release binaries"
  info "[dry-run] 2/9 Package currency: disable broken dnf repos; dnf upgrade fleet-critical packages (podman crun passt conmon containers-common caddy)"
  info "[dry-run] 3/9 Rootless podman: ${SERVICE_USER} user + subuid/subgid ranges, ${SERVICE_HOME} + ~/.config/containers ownership, the client bundle service-owned and outside the checkout (a bare install runs on the staged copy under ${SERVICE_HOME}/bundle), containers.conf (cgroupfs), /run/${SERVICE_USER}, podman info as ${SERVICE_USER} (a stale pause process is reported with what to do — doctor never runs podman system migrate, which deletes a live fleet's sandbox pool)"
  info "[dry-run] 4/9 Installed artifacts: ${SERVICE_NAME}.service + the fleet-backup and fleet-maintenance service/timer pairs' functional drift vs ${SRC_DIR}/deploy (reinstall + daemon-reload), the retired Next.js tier (fleet-web.service, its drop-in, start shim and /opt/fleet/web — stopped, disabled and removed when present; ADR-0091), /etc/profile.d/fleet-motd.sh (login banner hook), removal of the retired fleet-admin shim, /usr/local/bin/fleet symlink → ${INSTALL_DIR}/fleet, the binary present with no host executor and a web bundle (fleet version --build-info), fleet.prev kept for \`fleet release rollback\`"
  info "[dry-run] 5/9 Configuration: ${ENV_FILE} exists root-owned 0600 with OPENROUTER_API_KEY + DB DSNs; APP_SESSION_SECRET set (carried over from ${WEB_ENV_FILE}, else generated, when absent and the web UI is on); ${FLEET_CADDYFILE:-/etc/caddy/Caddyfile} (when fleet-managed) matches scripts/lib/caddyfile.sh — /v1/*, /api-info, agent card, /triggers/* → orchestrator, /webhooks/* → chat (rewrite from the renderer, backup kept, caddy reload) and FLEET_TRUSTED_PROXIES in ${ENV_FILE} (added as ${CADDY_TRUSTED_PROXIES} when absent behind that front and both backends are on loopback; an advisory otherwise; an operator's value is left alone); an operator-managed Caddyfile only gets an advisory when it routes no /v1; an advisory when sshd -T says passwordauthentication/permitrootlogin yes (never edits sshd)"
  info "[dry-run] 6/9 Services: ${SERVICE_NAME} active; postgresql/caddy active when enabled (systemctl start), then /healthz + /readyz respond, then https://<caddy domain>/api-info answers THROUGH caddy (--resolve pinned to 127.0.0.1) when caddy is active, then the running server's record of MCP tool schema findings (fleet mcp schema-issues: invalid = withheld from the model, rewritten = older JSON Schema draft translated) is read and each finding advised"
  info "[dry-run] 7/9 Scheduled maintenance: ${BACKUP_TIMER} installed + enabled + active (advisory when absent) and ${BACKUP_SERVICE}'s last run succeeded; ${MAINT_TIMER} likewise; free space on the data dir + the podman image store above the disk floor; disk hygiene: the root package cache (dnf — filled as root, never reached by the maintenance timer) reclaimed above 1 GiB, the journal capped at 1G via journald.conf.d/60-fleet-journal-cap.conf (persistent + volatile) unless the operator set a limit in journald's merged config, and images left in root's podman store reported"
  info "[dry-run] 8/9 Sandbox smoke: podman run --rm --network=none <sandbox image> true as ${SERVICE_USER}"
  info "[dry-run] 9/9 Release + checkout: the installed release vs the newest published one (fleet release latest), the checkout's release vs the installed binary's, and the paths that make the checkout read '.dirty' (fix stays 'fleet update' — doctor never pulls or installs)"
  info "[dry-run] would restart ${SERVICE_NAME} after a package upgrade, an app-unit reinstall, the web-tier retirement or a new APP_SESSION_SECRET above — a reinstalled fleet-backup unit does not bounce the app (unless --no-restart)"
  exit 0
fi

[[ $EUID -eq 0 ]] || die "run as root: sudo fleet doctor (or --dry-run to preview)"
[[ -x "$INSTALL_DIR/fleet" || -d "$SRC_DIR/.git" || -f "$SRC_DIR/go.mod" ]] \
  || die "no fleet install at $INSTALL_DIR and no checkout at $SRC_DIR (run scripts/bootstrap.sh first)"

HAVE_DNF=0
command -v dnf >/dev/null 2>&1 && HAVE_DNF=1
# Every doctor-issued dnf transaction tolerates unreachable bystander repos:
# strict dnf builds otherwise abort the WHOLE operation when any enabled repo
# fails metadata refresh (seen in production as "Failed to download metadata
# ... Usable URL not found" killing an unrelated upgrade). The repo-health
# check in step 2 additionally disables such repos so plain `dnf` works again
# for the operator.
DNF=(dnf --setopt='*.skip_if_unavailable=1')

# ── 1. toolchain ─────────────────────────────────────────────────────────────
# Runtime tools only. Go and Node are not box dependencies: fleet installs the
# signed release binary (docs/RELEASE-ARTIFACTS.md) and nothing is built on the
# box (ADR-0101).
step "1/9  Toolchain"

doctor_tools=(git curl jq podman psql python3)
for tool in "${doctor_tools[@]}"; do
  if command -v "$tool" >/dev/null 2>&1; then
    pass "$tool present"
  elif [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" == "0" ]]; then
    fail "$tool missing"
  else
    # Map command name -> Fedora package where they differ.
    pkg="$tool"
    case "$tool" in
      psql) pkg=postgresql ;;
    esac
    if "${DNF[@]}" install -y --quiet "$pkg" >/dev/null 2>&1; then
      fixed "$tool installed (dnf $pkg)"
    else
      fail "$tool missing and dnf install $pkg failed"
    fi
  fi
done

# ── 2. fleet-critical packages current ───────────────────────────────────────
step "2/9  Package currency (the version-lottery killer)"

if [[ "$HAVE_DNF" == "0" ]]; then
  advise "no dnf on this host — skipping repo health + package currency (keep podman/crun current with your package manager)"
else
  # Repo health first: a stale bystander repo can abort every plain dnf
  # transaction on strict dnf builds. Doctor's own calls tolerate it via the
  # DNF array above, but the box stays broken for the operator's own
  # `dnf install` — that's drift, so doctor disables the dead repo. Guarded:
  # when EVERY repo fails we assume the network (not the repos) is the problem
  # and touch nothing.
  broken_repos="$(dnf makecache --refresh 2>&1 \
    | sed -n -e 's/.*for repository "\([^"]*\)".*/\1/p' -e "s/.*for repo '\([^']*\)'.*/\1/p" \
    | sort -u || true)"
  if [[ -z "$broken_repos" ]]; then
    pass "all enabled dnf repos respond"
  else
    enabled_count="$(dnf repolist --quiet 2>/dev/null | tail -n +2 | wc -l || echo 0)"
    broken_count="$(echo "$broken_repos" | wc -l)"
    if [[ "$broken_count" -ge "${enabled_count:-0}" ]]; then
      advise "ALL enabled dnf repos fail metadata refresh — network problem, not repo drift; leaving repos alone"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "broken dnf repo(s) will abort plain dnf transactions: $(echo "$broken_repos" | tr '\n' ' ')"
    else
      # config-manager ships in dnf5-plugins (dnf5) / dnf-plugins-core (dnf4).
      if ! dnf config-manager --help >/dev/null 2>&1; then
        "${DNF[@]}" install -y --quiet dnf5-plugins >/dev/null 2>&1 \
          || "${DNF[@]}" install -y --quiet dnf-plugins-core >/dev/null 2>&1 || true
      fi
      for r in $broken_repos; do
        if dnf config-manager setopt "${r}.enabled=0" >/dev/null 2>&1 \
           || dnf config-manager --set-disabled "$r" >/dev/null 2>&1; then
          fixed "disabled broken repo $r (metadata unreachable; was aborting dnf transactions)"
        else
          fail "could not disable broken repo $r — disable manually: dnf config-manager setopt ${r}.enabled=0"
        fi
      done
    fi
  fi

  # The sandbox path is the most version-sensitive part of the stack: rootless
  # podman behavior differs meaningfully across podman/crun/passt/conmon
  # releases (cgroup drivers, netns sysctl handling, userns setup). Rather than
  # debugging per-box combinations, hold the whole fleet at each box's
  # repo-latest. (Go and Node no longer ride along: nothing on the box builds.)
  CRITICAL_PKGS=(podman crun passt conmon containers-common caddy)
  installed_pkgs=()
  for p in "${CRITICAL_PKGS[@]}"; do
    rpm -q "$p" >/dev/null 2>&1 && installed_pkgs+=("$p")
  done
  if [[ "${#installed_pkgs[@]}" -eq 0 ]]; then
    advise "none of the fleet-critical packages are rpm-managed — skipping currency check"
  else
    # `dnf check-update` exits 100 when upgrades are available. sort -u: dnf
    # can list the same package once per providing repo.
    pending="$("${DNF[@]}" check-update --quiet "${installed_pkgs[@]}" 2>/dev/null | awk 'NF>=3 {print $1}' | sort -u || true)"
    if [[ -z "$pending" ]]; then
      pass "fleet-critical packages current: ${installed_pkgs[*]}"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "upgrades available for: $(echo "$pending" | tr '\n' ' ')"
    else
      # shellcheck disable=SC2086 # word-splitting the package list is the point
      if "${DNF[@]}" upgrade -y --quiet $pending >/dev/null 2>&1; then
        fixed "upgraded: $(echo "$pending" | tr '\n' ' ')"
        restart_needed=1
      else
        fail "dnf upgrade failed for: $(echo "$pending" | tr '\n' ' ')"
      fi
    fi
  fi
fi

# ── 3. rootless podman for the service user ─────────────────────────────────
step "3/9  Rootless podman (the ${SERVICE_USER} service user's sandbox)"

if id "$SERVICE_USER" >/dev/null 2>&1; then
  pass "user $SERVICE_USER exists"
elif [[ "$CHECK_ONLY" == "1" ]]; then
  fail "user $SERVICE_USER missing — rerun: scripts/bootstrap.sh --enable-service"
else
  if useradd --system --home-dir "$SERVICE_HOME" --shell /usr/sbin/nologin --no-create-home "$SERVICE_USER" 2>/dev/null; then
    fixed "created service user $SERVICE_USER"
  else
    fail "could not create user $SERVICE_USER — rerun: scripts/bootstrap.sh --enable-service"
  fi
fi

if id "$SERVICE_USER" >/dev/null 2>&1; then
  for map in subuid subgid; do
    if grep -q "^${SERVICE_USER}:" "/etc/$map" 2>/dev/null; then
      pass "/etc/$map has a $SERVICE_USER range"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "/etc/$map missing a $SERVICE_USER range — rootless podman cannot map the userns"
    else
      # Same range bootstrap.sh provisions (setup_service_user).
      echo "${SERVICE_USER}:100000:65536" >> "/etc/$map"
      fixed "/etc/$map range added for $SERVICE_USER"
    fi
  done

  # HOME + podman dirs must exist and be fleet-owned; root-owned leftovers from
  # debugging sessions break every subsequent podman call with "path exists and
  # is not owned by the current user". /run/fleet normally comes from the
  # unit's RuntimeDirectory=, but doctor's own probes below need it now.
  for d in "$SERVICE_HOME" "$SERVICE_HOME/.config/containers" \
           "$SERVICE_HOME/.local/share/containers" "/run/${SERVICE_USER}"; do
    if [[ -d "$d" && "$(stat -c '%U' "$d")" == "$SERVICE_USER" ]]; then
      pass "$d owned by $SERVICE_USER"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "$d missing or not owned by $SERVICE_USER"
    else
      install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$d"
      # chown the whole tree: rootless podman refuses to start when any parent
      # (e.g. ~/.config) is root-owned; a partial chown reproduces the bug.
      case "$d" in "$SERVICE_HOME"/.*) chown -R "$SERVICE_USER":"$SERVICE_USER" "$d" ;; esac
      fixed "$d created/chowned to $SERVICE_USER"
    fi
  done

  # The client bundle the service loads must be one the rootless sandbox can
  # relabel (:z) on mount: owned by the service user, and writable under the
  # unit's ProtectSystem=strict namespace. The fleet checkout is neither, so a
  # bare install pointed at config/default fails every Pool.fill with
  # `lsetxattr … read-only file system` (#1655). bootstrap/update stage a copy
  # under $SERVICE_HOME/bundle; doctor reports, `fleet update` repairs.
  bundle_dir="$(env_get FLEET_CLIENT_CONFIG_DIR "$ENV_FILE")"
  # Podman relabels whenever SELinux is enabled — permissive mode suppresses
  # policy denials, not the lsetxattr — so a read-only or root-owned bundle is
  # a failure there, and only advice where SELinux is disabled or absent (no
  # relabel is attempted, and a world-readable bundle mounts fine).
  bundle_report=advise
  if command -v getenforce >/dev/null 2>&1 && [[ "$(getenforce 2>/dev/null)" =~ ^(Enforcing|Permissive)$ ]]; then
    bundle_report=fail
  fi
  if [[ -z "$bundle_dir" ]]; then
    fail "FLEET_CLIENT_CONFIG_DIR not set in $ENV_FILE — the service resolves the relative default config/default against its working directory (${SERVICE_HOME}) and fails at boot; fix: re-run scripts/bootstrap.sh --enable-service (it stages the generic bundle under ${SERVICE_HOME}/bundle), or set the variable to a bundle the service user owns"
  elif bundle_is_default_in_checkout "$bundle_dir" "$SRC_DIR" || bundle_looks_like_fleet_default "$bundle_dir"; then
    _fix="sudo fleet update"
    command -v rsync >/dev/null 2>&1 || _fix="sudo dnf install rsync (or apt-get install rsync), then sudo fleet update"
    "$bundle_report" "client bundle $bundle_dir is the generic bundle inside a fleet checkout — root-owned and read-only to the unit, so the sandbox's SELinux relabel fails (podman exit 126, lsetxattr EROFS); fix: ${_fix} (stages it under ${SERVICE_HOME}/bundle)"
  elif bundle_is_in_checkout "$bundle_dir" "$SRC_DIR"; then
    "$bundle_report" "client bundle $bundle_dir is inside the fleet checkout — read-only to the unit, so the sandbox's SELinux relabel fails (EROFS); check it out outside the repo (scripts/bootstrap.sh --client-config <git-url> lands it at /opt/fleet/client)"
  elif [[ ! -d "$bundle_dir" ]]; then
    fail "client bundle $bundle_dir (FLEET_CLIENT_CONFIG_DIR) does not exist"
  elif [[ ! -f "$bundle_dir/manifest.yaml" ]]; then
    fail "client bundle $bundle_dir has no manifest.yaml — not a bundle; fix FLEET_CLIENT_CONFIG_DIR in $ENV_FILE"
  elif ! bundle_writable_in_unit "$bundle_dir" "$SERVICE_NAME" "$SERVICE_HOME"; then
    # Owned correctly or not, a path outside the unit's writable set is
    # read-only to the service (ProtectSystem=strict), so the relabel fails.
    "$bundle_report" "client bundle $bundle_dir is outside ${SERVICE_NAME}.service's writable paths (ReadWritePaths, StateDirectory) — read-only under ProtectSystem=strict, so the sandbox's :z relabel fails (lsetxattr EROFS) on an SELinux host (enforcing or permissive); fix: move it under /opt/fleet/client or ${SERVICE_HOME}, or add it to the unit's ReadWritePaths"
  elif _not_owned="$(find "$bundle_dir" ! -user "$SERVICE_USER" -print -quit 2>/dev/null)" && [[ -n "$_not_owned" ]]; then
    # The whole tree, not just its top: one root-owned file (a root-run pull,
    # a hand edit) is enough for the rootless :z relabel to be refused.
    if [[ "$CHECK_ONLY" == "1" ]]; then
      "$bundle_report" "client bundle $bundle_dir has files not owned by $SERVICE_USER (first: ${_not_owned}) — rootless relabel is refused (EPERM); fix: chown -R ${SERVICE_USER}: $bundle_dir"
    else
      if chown -R "$SERVICE_USER": "$bundle_dir"; then
        fixed "client bundle $bundle_dir chowned to $SERVICE_USER"
      else
        # An immutable entry, a root-squashed or read-only filesystem: the
        # repair did not take, so the tree is still not fully owned.
        "$bundle_report" "client bundle $bundle_dir could not be chowned to $SERVICE_USER (first foreign-owned: ${_not_owned}) — rootless relabel is refused (EPERM); fix the filesystem, then: chown -R ${SERVICE_USER}: $bundle_dir"
      fi
    fi
  elif _staged_src="$(bundle_marker_source "$bundle_dir" "$SERVICE_USER")" && [[ -n "$_staged_src" ]]; then
    pass "client bundle $bundle_dir is the staged copy of ${_staged_src}, owned by $SERVICE_USER"
  else
    pass "client bundle $bundle_dir owned by $SERVICE_USER"
  fi

  # containers.conf: cgroupfs avoids needing a systemd user D-Bus session
  # (absent for a nologin system user); file events backend avoids journald
  # perms. Must match what bootstrap.sh writes — a box missing this fails with
  # "sd-bus call: Access denied" on every sandbox launch.
  conf="$SERVICE_HOME/.config/containers/containers.conf"
  if [[ -f "$conf" ]] && grep -q 'cgroup_manager = "cgroupfs"' "$conf" 2>/dev/null; then
    pass "containers.conf pins cgroupfs"
  elif [[ "$CHECK_ONLY" == "1" ]]; then
    fail "$conf missing the cgroupfs cgroup_manager — sandbox launches will fail under systemd"
  else
    install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$(dirname "$conf")"
    cat > "$conf" <<'CONF'
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
CONF
    chown "$SERVICE_USER":"$SERVICE_USER" "$conf"
    fixed "containers.conf written (cgroupfs + file events)"
  fi

  # Stale pause process: a rootless pause container forked inside an old mount
  # namespace pins that namespace and poisons later pulls/runs. The reset is
  # `podman system migrate`, which doctor deliberately never runs: it is NOT a
  # no-op — it deletes a live fleet's sandbox pool (see stale_pause_hint). The
  # probe below detects the condition and prints the repair instead.

  if run_as_fleet podman info >/dev/null 2>&1; then
    pass "rootless podman functional for $SERVICE_USER"
  else
    podman_err="$(run_as_fleet podman info 2>&1 >/dev/null | tail -n1 || true)"
    if is_stale_pause_error "$podman_err"; then
      # Matched BEFORE the post-upgrade deferral below: a stale pause is not
      # the transient an upgrade leaves, and step 6's automatic restart would
      # swap a fleet still serving from its existing pool for a unit that
      # cannot start its sandboxes. stale_pause holds that restart; the
      # repair's own stop/start picks up whatever the restart was for.
      stale_pause=1
      fail "rootless podman as $SERVICE_USER has a stale pause process (${podman_err}) — doctor does not reset it itself, and holds any service restart until it is repaired: $(stale_pause_hint)"
    elif [[ "$restart_needed" == "1" && "$CHECK_ONLY" == "0" && "$NO_RESTART" == "0" ]]; then
      # Step 2 just upgraded the container stack (podman/crun/passt/...)
      # while the running fleet service's sandbox containers still hold the
      # rootless store under the OLD binaries — a transient failure THIS
      # doctor run created itself. Don't fail the box on it: the step-6
      # restart releases the store, and step 7 re-verifies (plus the
      # sandbox smoke re-proves it end to end).
      advise "rootless 'podman info' fails as $SERVICE_USER right after the stack upgrade (${podman_err:-no error output}) — re-verifying after the service restart"
      podman_recheck=1
    else
      fail "rootless 'podman info' fails as $SERVICE_USER: ${podman_err:-no error output} — run: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman info"
    fi
  fi
fi

# ── 4. installed artifacts vs repo ───────────────────────────────────────────
step "4/9  Systemd units + binaries"

units_changed=0
if ! command -v systemctl >/dev/null 2>&1; then
  advise "no systemd on this host — skipping unit checks"
else
  for unit in "${SERVICE_NAME}.service" "$BACKUP_SERVICE" "$BACKUP_TIMER" "$MAINT_SERVICE" "$MAINT_TIMER"; do
    src="$SRC_DIR/deploy/$unit"
    # The generic service name is fleet; a renamed unit has no shipped file.
    [[ "$unit" == "${SERVICE_NAME}.service" && ! -f "$src" ]] && src="$SRC_DIR/deploy/fleet.service"
    dst="/etc/systemd/system/$unit"
    if [[ ! -f "$src" ]]; then
      advise "$unit: no shipped unit under $SRC_DIR/deploy — skipping drift check"
      continue
    fi
    if [[ ! -f "$dst" ]]; then
      # Absent unit is informational: a box may run fleet under another
      # supervisor. bootstrap --enable-service installs it.
      # The backup pair is optional-if-absent too, and step 7 owns that verdict
      # (with the "you may back up at the volume layer" caveat), so saying it
      # here as well would double-count one gap as two advisories.
      case "$unit" in
        "$BACKUP_SERVICE"|"$BACKUP_TIMER"|"$MAINT_SERVICE"|"$MAINT_TIMER") ;;
        *) advise "$unit not installed (scripts/bootstrap.sh --enable-service installs it)" ;;
      esac
      continue
    fi
    # Compare FUNCTIONAL lines only (comments/blank stripped) so a reworded
    # header never nags — same rule as update.sh's drift adoption.
    if diff -q <(grep -vE '^[[:space:]]*(#|$)' "$src") <(grep -vE '^[[:space:]]*(#|$)' "$dst") >/dev/null 2>&1; then
      pass "$unit matches deploy/"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "$unit drifted functionally from deploy/ — review: diff $dst $src"
    else
      install -m 0644 "$src" "$dst"
      fixed "$unit reinstalled from deploy/"
      # Only the app units want a process restart, which is user-visible chat
      # downtime. None of the timer units touch the running server: each oneshot
      # runs only when its timer fires it, and the daemon-reload after this loop
      # is all systemd needs to pick up a rewritten timer's schedule.
      case "$unit" in
        "$BACKUP_SERVICE"|"$BACKUP_TIMER"|"$MAINT_SERVICE"|"$MAINT_TIMER") ;;
        *) restart_needed=1 ;;
      esac
      units_changed=1
    fi
  done
  # The retired Next.js web tier (ADR-0091). The fleet binary serves the web UI
  # itself on the port that unit held (127.0.0.1:3000), so a leftover
  # fleet-web.service is not just dead weight: while it runs, fleet's web
  # listener cannot bind and fleet exits. update.sh retires it on upgrade; this
  # covers a box that was updated by hand. Only STOPPED here (reversibly, so
  # step 6 can start fleet on the freed port); step 6's restart removes its
  # unit, drop-in, start shim and Next build once fleet is active, or restarts
  # it if fleet does not come up (#1838). Never /etc/fleet/fleet-web.env, which
  # holds secrets. A run that will not restart fleet leaves it alone.
  if fleet_web_tier_present; then
    if [[ "$CHECK_ONLY" == "1" ]]; then
      fail "the retired Next.js web tier (fleet-web.service) is still installed — fleet serves the web UI itself on its port now (ADR-0091); a doctor run without --check retires it"
    elif [[ "$NO_RESTART" == "1" || "${stale_pause:-0}" == "1" ]]; then
      advise "the retired Next.js web tier (fleet-web.service) is still installed — it is retired together with a ${SERVICE_NAME} restart, which this run will not do; re-run doctor without --no-restart once the restart is possible"
    else
      fleet_stop_web_tier || true
      web_tier_cutover=1
      restart_needed=1
    fi
  fi
  [[ "$units_changed" == "1" ]] && systemctl daemon-reload

  # The login banner hook (#461). `fleet motd` renders dynamically from the
  # installed binary, so what drifts is never the banner's CONTENT — it is this
  # /etc/profile.d shim, which until now only scripts/bootstrap.sh ever wrote. A
  # box provisioned before the hook existed has none, and an edit to
  # deploy/fleet-motd.sh reached no existing box. Shipped content with no operator-tunable parts, so it is installed
  # rather than merely reported. No restart and no daemon-reload — profile.d is
  # read at the next login.
  # The retired `fleet-admin` shim (ADR-0060). Deleting it from the repo does
  # not remove it from a BOX: bootstrap/update installed a copy at
  # $INSTALL_DIR/fleet-admin and symlinked /usr/local/bin/fleet-admin at it, and
  # nothing ever removed either. Left alone it stays on PATH and still runs —
  # forever, against whatever code was current the day it was last built, which
  # is a worse failure than a missing command. doctor is the repair pass, so it
  # repairs this too. `-e || -L` because removing the target first leaves a
  # DANGLING symlink, for which `-e` is false.
  for stale in "$INSTALL_DIR/fleet-admin" /usr/local/bin/fleet-admin; do
    if [[ -e "$stale" || -L "$stale" ]]; then
      if [[ "$CHECK_ONLY" == "1" ]]; then
        fail "${stale} is the retired fleet-admin shim — stale, never rebuilt, and still on PATH. Use \`fleet <verb>\`; a doctor run without --check removes it."
      else
        rm -f "$stale" && fixed "removed the retired fleet-admin shim: ${stale}" \
          || fail "could not remove ${stale}"
      fi
    fi
  done

  motd_src="$SRC_DIR/deploy/fleet-motd.sh"
  motd_dst="/etc/profile.d/fleet-motd.sh"
  # The MODE is part of "matches", not just the bytes: profile.d is sourced by
  # every login shell as the logging-in user, so a hook that drifted to 0600 is
  # unreadable to everyone but root and the banner stays broken — while a
  # content-only comparison reports it current and neither verb reinstalls it.
  motd_mode() { stat -c '%a' "$1" 2>/dev/null || echo '?'; }
  if [[ -f "$motd_src" && -d /etc/profile.d ]]; then
    if [[ -f "$motd_dst" ]] && cmp -s "$motd_src" "$motd_dst" && [[ "$(motd_mode "$motd_dst")" == "644" ]]; then
      pass "fleet-motd.sh matches deploy/"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "${motd_dst} missing, drifted, or not mode 0644 — the login banner is stale, absent, or unreadable to non-root logins (diff: $motd_dst $motd_src)"
    else
      install -D -m 0644 "$motd_src" "$motd_dst" \
        && fixed "installed ${motd_dst} from deploy/ (login banner)" \
        || fail "could not install ${motd_dst}"
    fi
  fi

fi

if [[ -x "$INSTALL_DIR/fleet" ]]; then
  pass "$INSTALL_DIR/fleet present"
  # The two properties release.yml asserts before it publishes a binary, read
  # back from the one actually installed (a hand-copied or locally built
  # binary is where they would break).
  if _bi="$("$INSTALL_DIR/fleet" version --build-info 2>/dev/null)"; then
    if printf '%s' "$_bi" | grep -Eq '"host_executor":[[:space:]]*true'; then
      fail "$INSTALL_DIR/fleet has the unsandboxed host executor compiled in (fleet_host_executor) — never a release build; reinstall: sudo fleet update --tag <release>"
    else
      pass "$INSTALL_DIR/fleet has no host executor compiled in"
    fi
    if printf '%s' "$_bi" | grep -Eq '"web_bundle":[[:space:]]*""'; then
      fail "$INSTALL_DIR/fleet carries no web bundle (pages answer 503) — reinstall: sudo fleet update"
    fi
  else
    advise "$INSTALL_DIR/fleet predates \`fleet version --build-info\` — the next \`sudo fleet update\` installs a signed release that has it"
  fi
  if [[ -x "$INSTALL_DIR/fleet.prev" ]]; then
    pass "previous binary kept at $INSTALL_DIR/fleet.prev (\`sudo fleet release rollback\` restores it)"
  fi
else
  fail "$INSTALL_DIR/fleet missing — run: fleet update (or scripts/update.sh)"
fi
# bootstrap symlinks /usr/local/bin/fleet → $INSTALL_DIR/fleet so updates are
# picked up without re-linking; a stale copied binary here shadows every update.
if [[ -L /usr/local/bin/fleet && "$(readlink -f /usr/local/bin/fleet 2>/dev/null)" == "$(readlink -f "$INSTALL_DIR/fleet" 2>/dev/null)" ]]; then
  pass "/usr/local/bin/fleet → $INSTALL_DIR/fleet"
elif [[ ! -e /usr/local/bin/fleet && ! -x "$INSTALL_DIR/fleet" ]]; then
  advise "/usr/local/bin/fleet absent (install the binary first, then rerun doctor)"
elif [[ "$CHECK_ONLY" == "1" ]]; then
  fail "/usr/local/bin/fleet is not a symlink to $INSTALL_DIR/fleet (stale copy shadows every update)"
else
  ln -sf "$INSTALL_DIR/fleet" /usr/local/bin/fleet
  fixed "/usr/local/bin/fleet relinked → $INSTALL_DIR/fleet"
fi

# ── 5. configuration ─────────────────────────────────────────────────────────
step "5/9  Configuration"

if [[ ! -f "$ENV_FILE" ]]; then
  fail "$ENV_FILE missing — fleet cannot start (scripts/bootstrap.sh creates it; fleet config set-openrouter-key fills the key)"
else
  pass "$ENV_FILE exists"
  # Unlike chat's .env.local (app-user-owned), fleet's env file stays ROOT-owned
  # 0600: systemd reads it as root and injects the env; the unit then unsets
  # FLEET_ENV_FILE so the unprivileged process never re-opens it.
  perms="$(stat -c '%a %U' "$ENV_FILE")"
  if [[ "$perms" == "600 root" ]]; then
    pass "$ENV_FILE is root-owned 0600"
  elif [[ "$CHECK_ONLY" == "1" ]]; then
    fail "$ENV_FILE is $perms — want root-owned 0600 (it holds every credential)"
  else
    chown root:root "$ENV_FILE"; chmod 0600 "$ENV_FILE"
    fixed "$ENV_FILE chowned root:root, mode 0600"
  fi
  # Truthiness, not non-emptiness: an explicit FLEET_MOCK_MODE=false/0/no in
  # the env file used to read as "mock mode is on" and downgrade a missing
  # model key from ✗ to an advisory on a production box. Same token set the
  # Go side accepts (config truthy()).
  mock_on=0
  for _mv in "$(env_get FLEET_MOCK_MODE)" "${CHAT_MOCK_MODE:-}" "$(env_get CHAT_MOCK_MODE)"; do # LEGACY-ENV-ALIAS: remove after migrate-env
    case "${_mv,,}" in 1|true|yes|on) mock_on=1 ;; esac
  done
  if [[ -n "$(env_get OPENROUTER_API_KEY)" ]]; then
    pass "OPENROUTER_API_KEY set"
  elif [[ "$mock_on" == "1" ]]; then
    advise "OPENROUTER_API_KEY unset but mock mode is on"
  else
    fail "OPENROUTER_API_KEY unset — run: sudo fleet config set-openrouter-key"
  fi
  # One DSN, both stores in its schemas chat and sched (ADR-0098).
  # LEGACY-DB-LAYOUT: remove after consolidation — the per-store branch.
  # Read as fleet reads the file (fleet_env_value): KEY="" is unset, as in Go.
  if [[ -n "$(fleet_env_value "$ENV_FILE" FLEET_CHAT_DATABASE_URL)$(fleet_env_value "$ENV_FILE" FLEET_SCHED_DATABASE_URL)$(fleet_env_value "$ENV_FILE" SCHED_DATABASE_URL)" ]]; then
    _layout=""
    _fleet_bin="${INSTALL_DIR:-/opt/fleet}/fleet"
    [[ -x "$_fleet_bin" ]] || _fleet_bin="$(command -v fleet || true)"
    [[ -n "$_fleet_bin" ]] && _layout="$("$_fleet_bin" db layout --env-file "$ENV_FILE" 2>/dev/null || true)"
    unset _fleet_bin
    if [[ "$_layout" == "schemas" ]]; then
      advise "per-store DSN keys (FLEET_CHAT_/FLEET_SCHED_DATABASE_URL) name ONE database — fleet uses its schemas chat and sched; replace them with DATABASE_URL"
    else
      advise "legacy two-database layout (FLEET_CHAT_DATABASE_URL / FLEET_SCHED_DATABASE_URL) — still supported; move it into one database with: fleet db consolidate   (dry run first; docs/ONE-DATABASE.md)"
    fi
    unset _layout
  elif [[ -n "$(fleet_env_value "$ENV_FILE" DATABASE_URL)" ]]; then
    pass "DATABASE_URL set (one database: schemas chat + sched)"
  else
    fail "no DATABASE_URL in $ENV_FILE — rerun scripts/bootstrap.sh"
  fi
fi

# The web UI's session secret (ADR-0091). fleet serves the browser tier itself
# and signs the elcano_session cookie with APP_SESSION_SECRET; without it every
# password and SSO sign-in fails (fleet logs a WARNING and points here). A box
# upgraded from the Next.js tier normally had it carried over by update.sh; the
# repair takes the old tier's value first (existing sessions stay valid), else
# generates one. Skipped when the web listener is off (FLEET_WEB_ADDR=off).
if [[ -f "$ENV_FILE" && -r "$ENV_FILE" ]]; then
  case "$(env_get FLEET_WEB_ADDR | tr '[:upper:]' '[:lower:]')" in
    off|none|disabled|0)
      pass "web UI disabled (FLEET_WEB_ADDR=off) — no APP_SESSION_SECRET needed"
      ;;
    *)
      if [[ -n "$(env_get APP_SESSION_SECRET)" ]]; then
        pass "APP_SESSION_SECRET set (web sign-in can sign its session cookie)"
      elif [[ "$CHECK_ONLY" == "1" ]]; then
        fail "APP_SESSION_SECRET unset in $ENV_FILE — web sign-in is disabled; a doctor run without --check sets it"
      else
        _app_secret="$(env_get APP_SESSION_SECRET "$WEB_ENV_FILE")"
        _app_from="carried over from ${WEB_ENV_FILE}"
        if [[ -z "$_app_secret" ]]; then
          _app_secret="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48)"
          _app_from="generated"
        fi
        if upsert_env_key "$ENV_FILE" APP_SESSION_SECRET "$_app_secret"; then
          fixed "APP_SESSION_SECRET ${_app_from} into $ENV_FILE (web sign-in)"
          restart_needed=1
        else
          fail "could not write APP_SESSION_SECRET to $ENV_FILE — set it by hand (48 random alphanumerics), then restart ${SERVICE_NAME}"
        fi
        unset _app_secret _app_from
      fi
      ;;
  esac
fi

# ── the TLS front's routing (the fleet-managed Caddyfile) ────────────────────
# A Caddyfile that only knows the web tier sends every documented API URL
# (/v1/…, /api-info, the A2A agent card, /triggers/…, /webhooks/…) to the web
# tier, which 404s them — "the API isn't routing" with every unit green. bootstrap
# renders the file from scripts/lib/caddyfile.sh; a box provisioned before the
# API routes shipped keeps the old layout until something rewrites it. Same
# drift rule as units (functional lines only), and the domain + ACME email are
# read back from the installed file so a rewrite keeps them. A Caddyfile fleet
# did not write is never rewritten: it gets an advisory, and only when it
# routes no /v1 at all.
CADDYFILE="${FLEET_CADDYFILE:-/etc/caddy/Caddyfile}"
caddy_domain=""
if [[ -f "$CADDYFILE" ]]; then
  if caddyfile_is_managed "$CADDYFILE"; then
    caddy_domain="$(caddyfile_domain "$CADDYFILE")"
    if [[ -z "$caddy_domain" ]]; then
      fail "$CADDYFILE is fleet-managed but has no site block — rerun: sudo fleet bootstrap --enable-web --domain <fqdn>"
    else
      caddy_rendered="$(render_fleet_caddyfile "$caddy_domain" "$(caddyfile_acme_email "$CADDYFILE")" \
        "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)" \
        "$(caddy_web_upstream "$(env_get FLEET_WEB_ADDR)")")"
      if diff -q <(caddyfile_functional_body "$CADDYFILE") <(printf '%s\n' "$caddy_rendered" | caddyfile_functional_body) >/dev/null 2>&1; then
        pass "$CADDYFILE matches the shipped layout for ${caddy_domain} (/v1 API + webhooks routed to the Go backends)"
      elif [[ "$CHECK_ONLY" == "1" ]]; then
        fail "$CADDYFILE drifted from the shipped layout (scripts/lib/caddyfile.sh) — API calls to https://${caddy_domain}/v1/… may 404 at the web tier; fix: sudo fleet doctor (rewrites it, backup kept) or sudo fleet update --adopt-units"
      else
        caddy_backup="${CADDYFILE}.fleet-backup.$(date -u +%Y%m%dT%H%M%SZ)"
        if cp -p "$CADDYFILE" "$caddy_backup" 2>/dev/null && printf '%s\n' "$caddy_rendered" > "$CADDYFILE" 2>/dev/null; then
          if command -v caddy >/dev/null 2>&1 && ! caddy validate --adapter caddyfile --config "$CADDYFILE" >/dev/null 2>&1; then
            cp -p "$caddy_backup" "$CADDYFILE"
            fail "the rendered Caddyfile failed \`caddy validate\` — restored ${caddy_backup}; run: caddy validate --adapter caddyfile --config $CADDYFILE"
          else
            if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet caddy.service 2>/dev/null; then
              systemctl reload caddy.service >/dev/null 2>&1 || systemctl restart caddy.service >/dev/null 2>&1 || true
            fi
            fixed "$CADDYFILE rewritten from the shipped layout for ${caddy_domain} (previous file: ${caddy_backup}); caddy reloaded"
          fi
        else
          fail "could not rewrite $CADDYFILE (need root?) — review: diff <(caddyfile_functional_body $CADDYFILE) <(render_fleet_caddyfile ${caddy_domain} | caddyfile_functional_body)"
        fi
      fi
    fi
    # The front's other half: Caddy proxies the public API (/v1/*, /a2a,
    # /triggers/*, /webhooks/*) straight to the Go listeners, so the backends
    # need FLEET_TRUSTED_PROXIES naming it or every API caller looks like
    # Caddy's address (one shared rate-limit bucket; the IP allow/deny filter
    # judging Caddy). Browser traffic goes through the fleet web listener,
    # which calls the backends in-process with no X-Forwarded-For, so it keys
    # on 127.0.0.1 either way. bootstrap writes
    # 127.0.0.1,::1 with the Caddyfile; a box provisioned before it did gets it
    # added here when the key is ABSENT — but only while both upstreams are
    # loopback: Caddy dials a listener moved to an interface address from that
    # interface, and doctor does not guess which. A value the operator set —
    # even an empty one — is theirs and is only reported.
    if [[ ! -f "$ENV_FILE" ]]; then
      : # reported missing above
    elif [[ ! -r "$ENV_FILE" ]]; then
      advise "cannot read $ENV_FILE as uid ${EUID} — re-run with sudo to check FLEET_TRUSTED_PROXIES"
    else
      trusted_state="$(caddy_trusted_proxies_state "$ENV_FILE")"
      trusted_val="$(env_get FLEET_TRUSTED_PROXIES)"
      if ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then
        if [[ "$trusted_state" == "unset" ]]; then
          advise "FLEET_TRUSTED_PROXIES unset in $ENV_FILE, and Caddy proxies to a non-loopback backend (FLEET_SERVER_ADDR=$(env_get FLEET_SERVER_ADDR) FLEET_ORCHESTRATOR_ADDR=$(env_get FLEET_ORCHESTRATOR_ADDR)) — it connects from this host's interface address, which doctor does not guess; set it: sudo fleet config set-env FLEET_TRUSTED_PROXIES --value <address Caddy connects from>"
        else
          pass "FLEET_TRUSTED_PROXIES=${trusted_val} (operator's value; Caddy proxies to a non-loopback backend, so it must name the address Caddy connects from) — left as set"
        fi
      elif [[ "$trusted_state" == "ok" ]]; then
        pass "FLEET_TRUSTED_PROXIES=${trusted_val} — the API backends read the client IP Caddy forwards"
      elif [[ "$trusted_state" == "other" && ",${trusted_val// /}," == *",127.0.0.1,"* ]]; then
        pass "FLEET_TRUSTED_PROXIES=${trusted_val} (operator's value; includes Caddy's loopback peer) — left as set"
      elif [[ "$trusted_state" == "other" ]]; then
        advise "FLEET_TRUSTED_PROXIES is set to '${trusted_val}' in $ENV_FILE, which does not name Caddy's loopback peer — behind this box's Caddy every API caller then shares 127.0.0.1; left as set (operator's choice). The single-box value is ${CADDY_TRUSTED_PROXIES}"
      elif [[ "$CHECK_ONLY" == "1" ]]; then
        fail "FLEET_TRUSTED_PROXIES unset in $ENV_FILE behind the fleet-managed Caddy front — every API caller looks like 127.0.0.1 to the backends (shared rate limits; IP filter judges Caddy); fix: sudo fleet doctor (adds ${CADDY_TRUSTED_PROXIES}) or sudo fleet update --adopt-units"
      elif upsert_env_key "$ENV_FILE" FLEET_TRUSTED_PROXIES "$CADDY_TRUSTED_PROXIES" \
          && [[ "$(caddy_trusted_proxies_state "$ENV_FILE")" == "ok" ]]; then
        fixed "FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES} added to $ENV_FILE — the API backends read X-Forwarded-For from Caddy (and only from Caddy) once ${SERVICE_NAME} restarts"
        restart_needed=1
      else
        fail "could not set FLEET_TRUSTED_PROXIES in $ENV_FILE (need root?) — add: FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES}"
      fi
    fi
  elif caddyfile_routes_api "$CADDYFILE" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then
    pass "$CADDYFILE is operator-managed and routes /v1 to the orchestrator"
  elif caddyfile_is_package_default "$CADDYFILE"; then
    advise "$CADDYFILE is the caddy package's untouched default with no drop-in sites — bootstrap --enable-web --domain <host> replaces it with fleet's TLS front (#1657)"
  else
    advise "$CADDYFILE is not fleet-managed and does not appear to route the API — /v1/*, /api-info, /.well-known/agent-card.json, /a2a, /triggers/* belong on the orchestrator (127.0.0.1:8000) and /webhooks/* on chat (127.0.0.1:8080); see deploy/Caddyfile"
  fi
fi

# ── sshd: key-only? ──────────────────────────────────────────────────────────
# Vultr's cloud-init turns SSH password login on; bootstrap's key-only drop-in
# (scripts/lib/ssh.sh) is the safety net. Advisory only — doctor never edits
# sshd. Silent without sshd; `sshd -T` needs root (the host keys).
if command -v sshd >/dev/null 2>&1 && [[ $EUID -eq 0 ]]; then
  if ! sshd_eff="$(sshd -T 2>/dev/null)" || [[ -z "$sshd_eff" ]]; then
    advise "sshd -T failed — cannot tell whether sshd accepts passwords (run: sshd -t)"
  elif grep -qiE '^(passwordauthentication|permitrootlogin) yes$' <<<"$sshd_eff"; then
    advise "sshd accepts passwords — see production-support README, Access: SSH keys"
  else
    pass "sshd is key-only (no password login)"
  fi
fi

# ── 6. services ──────────────────────────────────────────────────────────────
step "6/9  Services"

if ! command -v systemctl >/dev/null 2>&1; then
  advise "no systemd — skipping service checks (verify your supervisor runs 'fleet serve')"
else
  # Optional tiers (postgresql, caddy) gate on `is-enabled`, which
  # is the operator-intent signal: a merely-INSTALLED unit proves only that
  # the rpm is present (e.g. the caddy package ships caddy.service on a box
  # fronted by something else), and an external-Postgres box legitimately
  # leaves postgresql disabled. Only the core fleet unit is unconditional.
  for svc in postgresql.service "${SERVICE_NAME}.service" caddy.service; do
    if ! systemctl cat "$svc" >/dev/null 2>&1; then
      if [[ "$svc" == "${SERVICE_NAME}.service" ]]; then
        fail "$svc not installed — run: scripts/bootstrap.sh --enable-service"
      else
        advise "$svc not installed (optional tier)"
      fi
      continue
    fi
    if systemctl is-active --quiet "$svc"; then
      pass "$svc active"
      if ! systemctl is-enabled --quiet "$svc" 2>/dev/null; then
        advise "$svc is active but not enabled — it will not start on boot: systemctl enable ${svc%.service}"
      fi
      continue
    fi
    if [[ "$svc" != "${SERVICE_NAME}.service" ]] && ! systemctl is-enabled --quiet "$svc" 2>/dev/null; then
      advise "$svc installed but not enabled — leaving it alone (enable it if this box should run it: systemctl enable --now ${svc%.service})"
      continue
    fi
    if [[ "$CHECK_ONLY" == "1" ]]; then
      fail "$svc enabled but not active"
    elif systemctl start "$svc" >/dev/null 2>&1 && systemctl is-active --quiet "$svc"; then
      fixed "$svc started"
    else
      fail "$svc failed to start — journalctl -u ${svc%.service} -n 50"
    fi
  done

  if [[ "$restart_needed" == "1" && "${stale_pause:-0}" == "1" ]]; then
    advise "fixes applied that want a restart — held: the rootless store has a stale pause process (step 3), so ${SERVICE_NAME} could not start its sandboxes; repairing it restarts fleet"
  elif [[ "$restart_needed" == "1" && "$CHECK_ONLY" == "0" && "$NO_RESTART" == "0" ]]; then
    # Branch on the restart's own result: reporting "restarted" (and counting
    # a fix) when the restart failed let a box end "✓ repaired" while the OLD
    # process was still the one answering /healthz.
    # With the retired web tier stopped in step 4, fleet must also reach
    # active (READY=1: every listener bound) before that tier is removed.
    if systemctl restart "${SERVICE_NAME}.service" 2>/dev/null \
       && { [[ "$web_tier_cutover" == "0" ]] || fleet_web_tier_wait_active "${SERVICE_NAME}.service"; }; then
      fixed "services restarted to pick up fixes"
      if [[ "$web_tier_cutover" == "1" ]] && fleet_remove_web_tier "$SRC_DIR"; then
        fixed "retired the Next.js web tier (fleet-web) — fleet serves the web UI on 127.0.0.1:3000"
      fi
    else
      fail "${SERVICE_NAME}.service failed to restart after fixes — journalctl -u ${SERVICE_NAME} -n 50"
      if [[ "$web_tier_cutover" == "1" ]]; then
        fleet_restore_web_tier
        fail "the retired Next.js web tier (fleet-web) was NOT removed (restarted if it had been running) — fix ${SERVICE_NAME}, then re-run doctor to retire it"
      fi
    fi
  elif [[ "$restart_needed" == "1" && "$NO_RESTART" == "1" ]]; then
    advise "fixes applied that want a restart — run: sudo fleet restart"
  fi
fi

# /healthz answers on the chat listener; /readyz (the deep probe: DBs + sandbox)
# is intercepted on the same server. FLEET_SERVER_ADDR defaults to 127.0.0.1:8080.
server_addr="$(env_get FLEET_SERVER_ADDR)"
server_addr="${server_addr:-127.0.0.1:8080}"
for probe in healthz readyz; do
  probe_ok=0
  # Give a just-(re)started server time: readyz gates on DB migration + sandbox
  # pool warm-up (the unit's TimeoutStartSec is 120s; 30s covers the common case).
  tries=1; [[ "$CHECK_ONLY" == "0" ]] && tries=30
  for ((i=0; i<tries; i++)); do
    if curl -fsS --max-time 10 "http://${server_addr}/${probe}" >/dev/null 2>&1; then
      probe_ok=1; break
    fi
    [[ "$tries" -gt 1 ]] && sleep 1
  done
  if [[ "$probe_ok" == "1" ]]; then
    pass "http://${server_addr}/${probe} responds"
  else
    fail "http://${server_addr}/${probe} not responding — fleet logs"
  fi
done

# The public API THROUGH the TLS front. /healthz above proves the backend is up
# on loopback; this proves caddy routes to it: /api-info is unauthenticated,
# unversioned-forever and answered by the orchestrator only, so anything but
# its JSON here is the web tier catching a path that should never have reached
# it. --resolve pins the domain to this box so the probe tests OUR caddy, not
# DNS or a NAT that can't hairpin.
if [[ -n "$caddy_domain" ]] && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet caddy.service 2>/dev/null; then
  api_ok=0
  tries=1; [[ "$CHECK_ONLY" == "0" ]] && tries=10
  for ((i=0; i<tries; i++)); do
    if curl -fsS --max-time 10 --resolve "${caddy_domain}:443:127.0.0.1" "https://${caddy_domain}/api-info" 2>/dev/null | grep -q '"api_version"'; then
      api_ok=1; break
    fi
    [[ "$tries" -gt 1 ]] && sleep 1
  done
  if [[ "$api_ok" == "1" ]]; then
    pass "https://${caddy_domain}/api-info answers through caddy — the /v1 API reaches the orchestrator"
  else
    fail "https://${caddy_domain}/api-info does not answer through caddy — API clients get the web tier's 404 (or there is no cert yet): journalctl -u caddy -n 50; curl -sv --resolve ${caddy_domain}:443:127.0.0.1 https://${caddy_domain}/api-info"
  fi
fi

# ── MCP tool schemas (read from the running server) ──────────────────────────
# Model providers validate every tool's input schema as JSON Schema draft
# 2020-12, and Anthropic rejects the WHOLE request when one tool fails, so one
# connector with an outdated schema used to break every Claude turn in any
# conversation that had it on (pages#111). Fleet now translates older-draft
# constructs and withholds a tool that is still invalid; both are recorded by
# the running server as its turns build tool lists. Doctor READS that record
# (fleet mcp schema-issues) instead of probing the connectors itself: probing
# would spawn every stdio connector as root, and root-owned files in the
# bundle break the sandbox relabel. Findings are advisories, not failures —
# the box is healthy; a connector needs updating.
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "${SERVICE_NAME}.service" 2>/dev/null; then
  fleet_cli="$INSTALL_DIR/fleet"
  [[ -x "$fleet_cli" ]] || fleet_cli="$(command -v fleet 2>/dev/null || true)"
  if [[ -z "$fleet_cli" ]] || ! command -v jq >/dev/null 2>&1; then
    advise "MCP tool schema check skipped (needs the fleet binary and jq)"
  elif [[ -z "$(env_get ADMIN_API_KEY)" && -z "${ADMIN_API_KEY:-}" ]]; then
    advise "MCP tool schema check skipped: ADMIN_API_KEY unset in $ENV_FILE (fleet mcp schema-issues needs it)"
  elif ! schema_json="$("$fleet_cli" mcp schema-issues --json 2>/dev/null)" || ! jq -e '.issues' >/dev/null 2>&1 <<<"$schema_json"; then
    advise "could not read the MCP tool schema record — run: fleet mcp schema-issues"
  else
    schema_since="$(jq -r '.since' <<<"$schema_json")"
    schema_total="$(jq '.issues | length' <<<"$schema_json")"
    if [[ "$schema_total" == "0" ]]; then
      pass "no MCP tool schema issues recorded by turns since ${schema_since}"
    else
      while IFS=$'\t' read -r s_status s_server s_tool s_detail; do
        if [[ "$s_status" == "invalid" ]]; then
          advise "MCP tool mcp_${s_server}_${s_tool} is WITHHELD from the model: its input schema is invalid JSON Schema draft 2020-12 (${s_detail}) — the ${s_server} connector must fix it"
        else
          advise "MCP tool mcp_${s_server}_${s_tool} uses older JSON Schema drafts; Fleet translates it and it works, but the ${s_server} connector should update (${s_detail})"
        fi
      done < <(jq -r '.issues[] | [.status, .server, .tool, (.detail | gsub("[\t\n]"; " "))] | @tsv' <<<"$schema_json")
      info "details: fleet mcp schema-issues (record since ${schema_since})"
    fi
  fi
fi

# ── 7. scheduled maintenance + disk headroom ─────────────────────────────────
step "7/9  Scheduled maintenance + disk headroom"

# An ABSENT timer is an advisory, never a failure, and doctor does not install
# it: a same-host pg_dump protects against logical loss, but an operator who
# snapshots the volume or the hypervisor has a stronger answer and is not
# misconfigured — installing backups on a box whose operator declined them
# (bootstrap --no-backup-timer) would be doctor overreaching. A timer whose LAST
# RUN FAILED is a genuine fault: the oneshot exits non-zero when a dump fails its
# integrity check, and a timer failing for a week is worse than no timer at all,
# because the box looks covered. Kept in lockstep with internal/boxdoctor's
# checkBackups, which reaches the same verdicts from inside the process.
if ! command -v systemctl >/dev/null 2>&1; then
  advise "no systemd — cannot check for a backup timer (schedule 'fleet backup --db=all --prune' with cron)"
elif ! systemctl cat "$BACKUP_TIMER" >/dev/null 2>&1 || ! systemctl cat "$BACKUP_SERVICE" >/dev/null 2>&1; then
  # Both halves must be present: a timer whose service unit is missing fires
  # into nothing, which would otherwise read as "backups are configured".
  # The hint installs just this pair rather than re-bootstrapping: on an
  # already-provisioned box, bootstrap also rebuilds binaries and re-provisions
  # Postgres, which is not what "I want a backup timer" should cost.
  advise "no ${BACKUP_TIMER} + ${BACKUP_SERVICE} pair installed — nothing on this box dumps the databases; install + enable it with: sudo fleet timers install --backup (ignore this if you back up at the volume/hypervisor layer)"
elif ! systemctl is-enabled --quiet "$BACKUP_TIMER" 2>/dev/null; then
  advise "${BACKUP_TIMER} installed but not enabled — it will never fire: systemctl enable --now ${BACKUP_TIMER}"
elif ! systemctl is-active --quiet "$BACKUP_TIMER" 2>/dev/null; then
  # is-enabled only reads the install symlink. A timer that is enabled but
  # STOPPED (enabled without --now and not rebooted since, an `enable --now`
  # whose start half failed, someone stopping it) never fires, and its service's
  # Result stays "success" — exactly the false clean this step exists to remove.
  advise "${BACKUP_TIMER} is enabled but not active — it will not fire until it is started: systemctl start ${BACKUP_TIMER}"
else
  # Result is systemd's verdict on the LAST run: "success" for a clean run and
  # also for a unit that has never run yet; anything else (exit-code, timeout,
  # signal, …) means the last backup did not complete.
  backup_result="$(systemctl show -p Result --value "$BACKUP_SERVICE" 2>/dev/null || true)"
  if [[ -n "$backup_result" && "$backup_result" != "success" ]]; then
    fail "${BACKUP_SERVICE} last run FAILED (Result=${backup_result}) — no dump is being written: journalctl -u ${BACKUP_SERVICE%.service} -n 50"
  else
    pass "${BACKUP_TIMER} enabled and active, no failed run recorded"
  fi
fi

# The host-maintenance pair: same posture as the backup timer above — absent is
# an advisory (an operator may prune their container store another way), a
# FAILED last run is a genuine fault, because the thing it prunes (dangling
# sandbox image layers, ~1.3 GB per rebuild) accumulates silently until the disk
# check below starts failing.
if command -v systemctl >/dev/null 2>&1; then
  if ! systemctl cat "$MAINT_TIMER" >/dev/null 2>&1; then
    advise "no ${MAINT_TIMER} + ${MAINT_SERVICE} pair installed — nothing prunes stale podman image layers on this box; install + enable it with: sudo fleet timers install --maintenance (ignore this if you prune the container store yourself)"
  elif ! systemctl is-enabled --quiet "$MAINT_TIMER" 2>/dev/null; then
    advise "${MAINT_TIMER} is installed but NOT enabled — it will not fire: systemctl enable --now ${MAINT_TIMER}"
  elif ! systemctl is-active --quiet "$MAINT_TIMER" 2>/dev/null; then
    advise "${MAINT_TIMER} is enabled but not active — it will not fire until it is started: systemctl start ${MAINT_TIMER}"
  else
    maint_result="$(systemctl show -p Result --value "$MAINT_SERVICE" 2>/dev/null || true)"
    if [[ -n "$maint_result" && "$maint_result" != "success" ]]; then
      fail "${MAINT_SERVICE} last run FAILED (Result=${maint_result}) — stale image layers are accumulating: journalctl -u ${MAINT_SERVICE%.service} -n 50"
    else
      pass "${MAINT_TIMER} enabled and active, no failed run recorded"
    fi
  fi
fi

# Disk hygiene: the trees that filled every production box and that NO
# scheduled job reclaims (measured across five boxes: 2–6 GiB of root caches
# each, journals at 1.3–4 GiB, stale images in root's podman store).
#
# They escape the maintenance timer for one reason: fleet-maintenance runs
# `fleet cleanup` as the SERVICE user, so its `podman image prune` sweeps that
# user's store — while dnf (and, before signed releases, the on-box Go/npm
# build) runs as ROOT. Doctor runs as root, so it is the pass that can.
# Everything here is a cache or a log: nothing touches databases, workspaces,
# the bundle or the service user's image store. It runs BEFORE the headroom
# verdict below, so a box these trees had filled is judged on the space left
# after the reclaim, not failed for a problem this pass just fixed.
human_bytes() { numfmt --to=iec --suffix=B "${1:-0}" 2>/dev/null || echo "${1:-0}B"; }
dir_bytes() {
  local p="$1" b=""
  # -D follows the root itself when it is a symlink (a cache moved to another
  # volume) without following links inside the tree.
  [[ -n "$p" && -d "$p" ]] && b="$(du -sxbD "$p" 2>/dev/null | awk '{print $1}' || true)"
  echo "${b:-0}"
}

# Root package cache (dnf). The Go build/module and npm caches this check also
# reclaimed went with the on-box build (ADR-0101): a release box has neither
# toolchain, so it has neither cache. Reclaimed above a threshold rather than on
# every run, so a doctor run does not cost the next dnf transaction a full
# metadata re-download for a few hundred MB.
ROOT_CACHE_RECLAIM_BYTES=$((1024 * 1024 * 1024))
# measure_root_package_cache sets rc_total (bytes) and rc_summary; called again
# after a reclaim so the verdict reports what is left, not what was attempted.
measure_root_package_cache() {
  local dnfcache="" b
  rc_total=0 rc_summary="none"
  # dnf5's system_cachedir is configurable; ask dnf for the active one and fall
  # back to the defaults (dnf5, then dnf4) when it cannot say.
  if [[ "$HAVE_DNF" == "1" ]]; then
    dnfcache="$(dnf --dump-main-config 2>/dev/null | sed -nE 's/^system_cachedir[[:space:]]*=[[:space:]]*//p' | head -n1 || true)"
  fi
  if [[ -z "$dnfcache" ]]; then
    dnfcache=/var/cache/libdnf5
    [[ -d "$dnfcache" ]] || dnfcache=/var/cache/dnf
  fi
  b="$(dir_bytes "$dnfcache")"
  if (( b > 0 )); then
    rc_total="$b"
    rc_summary="dnf cache $(human_bytes "$b")"
  fi
  return 0
}
check_root_package_cache() {
  local before before_summary
  measure_root_package_cache
  if (( rc_total < ROOT_CACHE_RECLAIM_BYTES )); then
    pass "root package cache $(human_bytes "$rc_total") (below the $(human_bytes "$ROOT_CACHE_RECLAIM_BYTES") reclaim threshold)"
    return 0
  fi
  if [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" != "1" ]]; then
    advise "root package cache holds $(human_bytes "$rc_total") (${rc_summary}) — the maintenance timer (which runs as ${SERVICE_USER}) never reclaims it; a doctor run without --check does: dnf clean all"
    return 0
  fi
  before="$rc_total" before_summary="$rc_summary"
  # Best-effort (doctor runs under set -e); the re-measure below, not the exit
  # code, decides the verdict — a dnf lock or a read-only cache leaves bytes
  # behind whatever the command claimed.
  dnf clean all >/dev/null 2>&1 || true
  measure_root_package_cache
  if (( rc_total < ROOT_CACHE_RECLAIM_BYTES )); then
    fixed "reclaimed the root package cache: $(human_bytes "$before") -> $(human_bytes "$rc_total") (was: ${before_summary}) — the next dnf transaction re-downloads the metadata it needs"
  else
    fail "root package cache reclaim left $(human_bytes "$rc_total") (${rc_summary}; was $(human_bytes "$before")) — dnf clean all did not free it (dnf lock? read-only cache?)"
  fi
}
check_root_package_cache

# Journal cap. journald's default ceiling is 10% of the filesystem (≤ 4 GiB),
# taken from the same volume as the databases and the image store. Doctor
# installs the shipped 1 GiB cap (persistent AND volatile storage) only where
# the operator has set no size limit of their own — an explicit choice, larger
# or smaller, is theirs to make.
# The cap is 1 GiB (deploy/journald.conf.d/60-fleet-journal-cap.conf). It bounds
# archived + active files together but rotation is lazy, so a capped journal can
# sit a little over; past this slack it was not reclaimed.
JOURNAL_CAP_SLACK_BYTES=$((1280 * 1024 * 1024))
# LC_ALL=C: the size is parsed out of journalctl's prose, which is localized.
journal_usage() { LC_ALL=C journalctl --disk-usage 2>/dev/null | sed -nE 's/.* take up ([^ ]+) .*/\1/p' | tail -n1 || true; }
journal_bytes() { local u; u="$(journal_usage)"; [[ -n "$u" ]] && numfmt --from=iec "${u%B}" 2>/dev/null || true; }
# reclaim_journal rotates (vacuum only removes ARCHIVED files; --disk-usage
# also counts the active ones) and vacuums, then re-measures: success means the
# journal is actually within the cap, not that the command exited 0.
reclaim_journal() {
  local b
  journalctl --rotate --vacuum-size=1G >/dev/null 2>&1 || true
  b="$(journal_bytes)"
  [[ -n "$b" ]] && (( b <= JOURNAL_CAP_SLACK_BYTES ))
}
# journald_loaded_since reports whether the running systemd-journald started
# at or after FILE was last written — i.e. whether it has read it. An unknown
# start time or mtime counts as not loaded: a restart is cheap, a false "capped"
# lets the journal regrow to the default ceiling.
journald_loaded_since() {
  local started mtime
  started="$(systemctl show -p ActiveEnterTimestamp --value systemd-journald 2>/dev/null || true)"
  [[ -n "$started" ]] && started="$(date -d "$started" +%s 2>/dev/null || true)"
  mtime="$(stat -c %Y "$1" 2>/dev/null || true)"
  [[ -n "$started" && -n "$mtime" ]] && (( started >= mtime ))
}
check_journal_cap() {
  local src="$SRC_DIR/deploy/journald.conf.d/60-fleet-journal-cap.conf"
  local dst="/etc/systemd/journald.conf.d/60-fleet-journal-cap.conf"
  local usage bytes operator="" operator_files="" f loaded merged
  command -v journalctl >/dev/null 2>&1 && command -v systemd-analyze >/dev/null 2>&1 && [[ -f "$src" ]] || return 0
  usage="$(journal_usage)" bytes="$(journal_bytes)"
  # The operator's own limit, read from journald's MERGED configuration rather
  # than raw files: cat-config applies systemd's precedence, so a vendor drop-in
  # shadowed by a same-named /etc file or a /dev/null link never appears, and
  # whitespace around '=' is the parser's problem, not ours. Any System/
  # RuntimeMaxUse from a file other than fleet's is an explicit choice — the
  # operator's, or a vendor's in /usr/lib (still a limit, not journald's 10%
  # default) — and fleet's 60- drop-in would override one set in journald.conf
  # or a lower-numbered drop-in, so theirs wins and ours goes. Every file that sets one is
  # collected, in precedence order (the last is the one in effect): journald
  # has loaded the operator's limit only if it started after all of them.
  # An unreadable merged config is not "no limit": fail closed rather than
  # install over a limit doctor could not see.
  if ! merged="$(systemd-analyze cat-config systemd/journald.conf 2>/dev/null)"; then
    advise "could not read journald's merged configuration (systemd-analyze cat-config systemd/journald.conf) — journal cap left unchanged"
    return 0
  fi
  operator_files="$(awk -v ours="$dst" '
    /^# \// { file = substr($0, 3); next }
    /^[[:space:]]*(System|Runtime)MaxUse[[:space:]]*=/ && file != ours && !seen[file]++ { print file }' <<<"$merged")"
  operator="$(tail -n1 <<<"$operator_files")"
  if [[ -n "$operator" ]]; then
    if [[ ! -f "$dst" ]]; then
      # Their files, their restart: doctor only says when journald predates one.
      loaded=1
      while IFS= read -r f; do
        journald_loaded_since "$f" || loaded=0
      done <<<"$operator_files"
      if [[ "$loaded" == "1" ]]; then
        pass "journal size limit set explicitly in ${operator} — not fleet's; doctor defers to it (using ${usage:-?})"
      else
        advise "journal size limit set explicitly in ${operator}, but systemd-journald started before it (or another file setting a limit) was written and is still on its old limit — apply it with: systemctl restart systemd-journald"
      fi
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      advise "${dst} overrides the explicit journal size limit in ${operator} — a doctor run without --check removes fleet's cap so theirs applies"
    elif ! rm -f "$dst"; then
      fail "could not remove ${dst} (it overrides the explicit journal size limit in ${operator})"
    elif [[ "$NO_RESTART" == "1" ]]; then
      fixed "removed ${dst} so the explicit limit in ${operator} applies"
      advise "  systemd-journald restart held by --no-restart — it applies on: systemctl restart systemd-journald"
    elif systemctl restart systemd-journald; then
      fixed "removed ${dst} so the explicit limit in ${operator} applies"
    else
      fail "removed ${dst} but could not restart systemd-journald to apply the explicit limit"
    fi
    return 0
  fi
  if [[ -f "$dst" ]] && diff -q <(grep -vE '^[[:space:]]*(#|$)' "$src") <(grep -vE '^[[:space:]]*(#|$)' "$dst") >/dev/null 2>&1; then
    # Installed is not the same as in effect. journald reads the cap only at
    # start, so a file written after it started (a --no-restart run, or a hand
    # copy) is not loaded however small the journal is right now; and a run
    # whose vacuum failed leaves the old size behind.
    loaded=1
    if ! journald_loaded_since "$dst"; then
      loaded=0
      if [[ "$CHECK_ONLY" == "1" ]]; then
        advise "${dst##*/} is installed but systemd-journald has not restarted since, so it is still on its default limit — a doctor run without --check restarts it: systemctl restart systemd-journald"
      elif [[ "$NO_RESTART" == "1" ]]; then
        advise "${dst##*/} is installed but systemd-journald has not restarted since; restart held by --no-restart — the cap applies on: systemctl restart systemd-journald"
      elif systemctl restart systemd-journald; then
        loaded=1
        fixed "restarted systemd-journald to load ${dst##*/} (it was installed after journald started)"
      else
        fail "${dst##*/} is installed but systemd-journald could not be restarted to load it: systemctl restart systemd-journald"
      fi
    fi
    if [[ -z "$bytes" ]]; then
      advise "could not measure the journal (journalctl --disk-usage), so the 1G cap is unconfirmed"
    elif (( bytes <= JOURNAL_CAP_SLACK_BYTES )); then
      if [[ "$loaded" == "1" ]]; then pass "journal capped by ${dst##*/} (using ${usage:-?})"; fi
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      advise "journal is ${usage} despite ${dst##*/} — a doctor run without --check rotates and vacuums it"
    elif reclaim_journal; then
      fixed "journal was ${usage} despite ${dst##*/} — rotated and vacuumed to $(journal_usage)"
    else
      fail "journal is still $(journal_usage) after rotate + vacuum despite ${dst##*/}: journalctl --rotate --vacuum-size=1G"
    fi
    return 0
  fi
  if [[ "$CHECK_ONLY" == "1" ]]; then
    advise "journal uncapped beyond journald's 10%-of-disk default, or ${dst##*/} drifted (using ${usage:-?}) — a doctor run without --check installs ${dst} (1G, persistent and volatile)"
    return 0
  fi
  if ! install -D -m 0644 "$src" "$dst"; then
    fail "could not install ${dst}"
    return 0
  fi
  if [[ "$NO_RESTART" == "1" ]]; then
    # journald reads the cap only on restart; the vacuum below works without one.
    advise "installed ${dst}; systemd-journald restart held by --no-restart — the cap applies on: systemctl restart systemd-journald"
  elif ! systemctl restart systemd-journald; then
    fail "installed ${dst} but could not restart systemd-journald to apply it"
    return 0
  fi
  if reclaim_journal; then
    fixed "installed ${dst} (1G cap) and reclaimed the journal (was ${usage:-?}, now $(journal_usage))"
  else
    fail "installed ${dst} (1G cap) but the journal is still $(journal_usage) after rotate + vacuum: journalctl --rotate --vacuum-size=1G"
  fi
}
check_journal_cap

# Root's podman store. fleet's sandboxes run in the SERVICE user's rootless
# store, so images in root's store are leftovers — typically sandbox builds from
# before the rootless move (two production boxes held ~1.3 GiB each). Advisory
# only: an operator may run podman as root for something of their own, and an
# image with no container is not proof it is unwanted.
check_root_podman_store() {
  local images containers size graphroot
  [[ "$SERVICE_USER" != "root" ]] && command -v podman >/dev/null 2>&1 || return 0
  images="$(podman images -a -q 2>/dev/null | wc -l || true)"
  (( images > 0 )) || return 0
  containers="$(podman ps -aq 2>/dev/null | wc -l || true)"
  # The CONFIGURED graph root (storage.conf may move it), the same store the
  # image/container counts above came from.
  graphroot="$(podman info --format '{{.Store.GraphRoot}}' 2>/dev/null || true)"
  size="$(dir_bytes "${graphroot:-/var/lib/containers/storage}")"
  advise "root's podman store holds ${images} image(s) and ${containers} container(s), $(human_bytes "$size") — fleet sandboxes run in ${SERVICE_USER}'s rootless store, so these are not fleet's"
  advise "  review: podman images -a   —   reclaim if nothing of yours uses them: podman system prune -a"
}
check_root_podman_store

# Disk headroom. Thresholds mirror internal/boxdoctor's checkDisk (85% warn /
# 95% fail) so the box-level pass and the in-process /admin/doctor report reach
# the same verdict, and both name the same remedy. Measured on the two trees
# that actually fill: the data dir (databases, uploads, workspaces) and the
# service user's rootless image store.
disk_floor="$(env_get FLEET_DISK_MIN_FREE_PERCENT)"
disk_floor="${disk_floor:-5}"
check_disk_headroom() {
  local label="$1" path="$2" used avail
  [[ -d "$path" ]] || return 0
  # df -P keeps the POSIX single-line format regardless of long device names.
  used="$(df -P "$path" 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')"
  avail="$(df -Ph "$path" 2>/dev/null | awk 'NR==2 {print $4}')"
  [[ -n "$used" ]] || { advise "could not measure free space on ${path}"; return 0; }
  if   (( used >= 95 )); then
    fail "${label} (${path}) is ${used}% full, ${avail} free — run: sudo fleet cleanup, and check 'systemctl status ${MAINT_TIMER}'"
  elif (( used >= 85 )); then
    advise "${label} (${path}) is ${used}% full, ${avail} free — consider: sudo fleet cleanup"
  else
    pass "${label} (${path}) ${used}% full, ${avail} free"
  fi
  # Below the CONFIGURED floor the running process stops claiming scheduled
  # tasks (FLEET_DISK_MIN_FREE_PERCENT), which is a different and more urgent
  # statement than "the disk is getting full" — say it explicitly.
  if [[ "$disk_floor" =~ ^[0-9]+$ ]] && (( disk_floor > 0 && 100 - used < disk_floor )); then
    fail "${label} is below the configured ${disk_floor}% free floor — fleet is HOLDING BACK scheduled tasks until space is reclaimed (FLEET_DISK_MIN_FREE_PERCENT)"
  fi
}
data_dir="$(env_get FLEET_DATA_DIR)"
[[ -z "$data_dir" ]] && data_dir="$SERVICE_HOME/data"
check_disk_headroom "data dir" "$data_dir"
check_disk_headroom "podman image store" "$SERVICE_HOME/.local/share/containers"

# The client bundle checkout is the THIRD tree that fills, and the only one
# nothing reclaims: `fleet cleanup` and the maintenance timer sweep the data dir,
# and neither has ever known about the bundle.
#
# It fills because fleet used to launch every stdio MCP server with its cwd set
# to the bundle root, so a relative output_dir — passed by an agent, or defaulted
# by a connector — wrote report files straight into the operator's git checkout.
# One box accumulated dozens of client CSV/XLSX/PDF files plus downloads/,
# reports/, sources/ and workspace/ that way. fleet now launches those
# subprocesses in a managed workspace instead, so this stops growing on a current
# build — but the fix removes nothing already there, and the files are untracked
# customer data sitting in a git repo.
#
# Reported against `git status`, not a name list, so it catches whatever an agent
# actually named. Advisory, never a failure: doctor does not delete an operator's
# files, and some of this residue is a real client report someone may still want.
check_bundle_residue() {
  local dir count bytes human
  dir="$(env_get FLEET_CLIENT_CONFIG_DIR)"
  [[ -n "$dir" && -e "$dir/.git" ]] || return 0
  command -v git >/dev/null 2>&1 || return 0
  # Read-only, and it must stay that way: doctor runs as root, and a plain
  # `git status` opportunistically rewrites .git/index to refresh its stat
  # cache — leaving a root-owned index that the ownership check above then
  # flags (rootless :z relabel refused) on the NEXT doctor run, a problem
  # doctor itself created. GIT_OPTIONAL_LOCKS=0 is git's documented switch for
  # "don't write the index from a read-only command". safe.directory goes on
  # the command line (a protected scope git honours) rather than
  # `git config --global --add`, which appended a duplicate line to root's
  # ~/.gitconfig on every run.
  # -uall lists files inside untracked dirs, so a single `reports/` holding 300
  # CSVs reads as 300 rather than 1. --ignored is deliberately NOT passed: a
  # bundle that has taken the .gitignore safety net would otherwise report clean
  # while still filling the disk.
  mapfile -t _residue < <(GIT_OPTIONAL_LOCKS=0 git -c safe.directory="$dir" -C "$dir" status --porcelain -uall --ignored=no 2>/dev/null | awk '$1=="??"{sub(/^\?\? /,""); print}')
  count="${#_residue[@]}"
  if (( count == 0 )); then
    pass "client bundle checkout is clean (${dir})"
    return 0
  fi
  bytes=0
  for f in "${_residue[@]}"; do
    # git quotes paths containing spaces/UTF-8; strip the quotes so du can stat
    # the real name. Unreadable or vanished entries just contribute 0.
    f="${f%\"}"; f="${f#\"}"
    bytes=$(( bytes + $(du -sb "$dir/$f" 2>/dev/null | awk '{print $1}' || echo 0) ))
  done
  human="$(numfmt --to=iec --suffix=B "$bytes" 2>/dev/null || echo "${bytes}B")"
  advise "client bundle checkout holds ${count} untracked file(s), ${human} (${dir})"
  advise "  MCP connector output written into the bundle; nothing reclaims it and it is one 'git add -A' from being committed"
  advise "  review: git -C ${dir} status --porcelain -uall   —   then remove: sudo -u ${SERVICE_USER} git -C ${dir} clean -nd   (drop -n to apply)"
}
check_bundle_residue

# ── 8. sandbox smoke ─────────────────────────────────────────────────────────
step "8/9  Sandbox smoke"

# Deferred from step 3: the post-upgrade `podman info` failure should have
# cleared once step 6 restarted the services off the old container stack.
if [[ "${podman_recheck:-0}" == "1" ]]; then
  if run_as_fleet podman info >/dev/null 2>&1; then
    fixed "rootless podman functional for $SERVICE_USER (recovered by the post-upgrade service restart)"
  else
    podman_err="$(run_as_fleet podman info 2>&1 >/dev/null | tail -n1 || true)"
    if is_stale_pause_error "$podman_err"; then
      fail "rootless podman as $SERVICE_USER STILL fails after the restart: a stale pause process (${podman_err}) — doctor does not reset it itself: $(stale_pause_hint)"
    else
      fail "rootless 'podman info' STILL fails as $SERVICE_USER after the restart: ${podman_err:-no error output} — run: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman info"
    fi
  fi
fi

# Resolve the image ref the daemon would use, with the SAME precedence as the
# process (clientconfig ResolvedImageRef), bootstrap and update: env file wins,
# else the bundle's sandbox.image (a prebuilt/registry ref — the Kubernetes and
# registry-publish path), else its sandbox.tag (build-on-box), else the
# default tag. Reading only sandbox.tag here made every registry-image bundle
# fail this check forever: doctor said "build it: sudo fleet update", and
# update correctly skipped the build because the manifest names an image.
sandbox_img="$(env_get FLEET_SANDBOX_IMAGE)"
[[ -z "$sandbox_img" ]] && sandbox_img="$(env_get CHAT_SANDBOX_IMAGE)" # LEGACY-ENV-ALIAS: remove after migrate-env
sandbox_img_prebuilt=0
if [[ -z "$sandbox_img" ]]; then
  bundle_dir="$(env_get FLEET_CLIENT_CONFIG_DIR)"
  [[ -z "$bundle_dir" && -d "$INSTALL_DIR/client" ]] && bundle_dir="$INSTALL_DIR/client"
  [[ -z "$bundle_dir" ]] && bundle_dir="$SRC_DIR/config/default"
  # manifest_sandbox_scalar KEY — the scalar under the sandbox: block, with a
  # bare ${VAR} / ${VAR:-default} interpolated against the process env (the
  # only shapes the default bundle uses; mirrors bootstrap's
  # resolve_sandbox_image — keep them in sync).
  manifest_sandbox_scalar() {
    local key="$1" raw
    raw="$(awk -v key="$key" '
      /^sandbox:[[:space:]]*$/ { b=1; next }
      /^[^[:space:]]/          { b=0 }
      b && $0 ~ "^[[:space:]]+" key ":" { sub("^[[:space:]]+" key ":[[:space:]]*",""); sub(/[[:space:]]+#.*$/,""); gsub(/^["'\'']|["'\'']$/,""); print; exit }
    ' "$bundle_dir/manifest.yaml" 2>/dev/null)"
    if [[ "$raw" =~ ^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$ ]]; then
      local var="${BASH_REMATCH[1]}" def="${BASH_REMATCH[3]}"
      printf '%s' "${!var:-$def}"
    else
      printf '%s' "$raw"
    fi
  }
  sandbox_img="$(manifest_sandbox_scalar image)"
  if [[ -n "$sandbox_img" ]]; then
    sandbox_img_prebuilt=1
  else
    sandbox_img="$(manifest_sandbox_scalar tag)"
    sandbox_img="${sandbox_img:-localhost/fleet-sandbox:latest}"
  fi
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1 || ! command -v podman >/dev/null 2>&1; then
  advise "skipping sandbox smoke (needs the $SERVICE_USER user + podman)"
elif run_as_fleet podman image exists "$sandbox_img" 2>/dev/null; then
  # The definitive check: launch the image in the exact rootless environment
  # the daemon uses. --network=none mirrors the runtime's default isolation.
  if smoke_err="$(run_as_fleet timeout 120 podman run --rm --network=none "$sandbox_img" true 2>&1 >/dev/null)"; then
    pass "sandbox smoke passed ($sandbox_img runs as $SERVICE_USER)"
  elif is_stale_pause_error "$smoke_err"; then
    fail "sandbox image $sandbox_img NOT runnable as $SERVICE_USER: a stale rootless pause process (${smoke_err##*$'\n'}) — doctor does not reset it itself: $(stale_pause_hint)"
  else
    fail "sandbox image $sandbox_img present but NOT runnable as $SERVICE_USER — tool calls will break; rerun verbosely: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman run --rm $sandbox_img true"
  fi
elif [[ "$sandbox_img_prebuilt" == "1" ]]; then
  fail "sandbox image $sandbox_img (the bundle's sandbox.image) is not in fleet's rootless store — pull it as $SERVICE_USER: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman pull $sandbox_img"
else
  fail "sandbox image $sandbox_img missing from ${SERVICE_USER}'s rootless store — build it: sudo fleet update (or sudo FLEET_CLIENT_CONFIG_DIR=<bundle> scripts/build-sandbox-image.sh; a root run builds into ${SERVICE_USER}'s store)"
fi

# ── 9. source freshness + build identity ─────────────────────────────────────
step "9/9  Release + checkout"

# Report-only in every mode: installing a release is `fleet update`'s job, and
# doctor silently kicking off a deploy would be a surprise.
installed_stamp=""
if [[ -x "$INSTALL_DIR/fleet" ]]; then
  # `fleet version` prints "fleet <version> (<revision>)" or, when the version
  # already names the commit, "fleet <version>": word two either way.
  installed_stamp="$("$INSTALL_DIR/fleet" version 2>/dev/null | awk 'NR==1 {print $2}')"
  if latest="$("$INSTALL_DIR/fleet" release latest 2>/dev/null)"; then
    if [[ "v${installed_stamp}" == "$latest" ]]; then
      pass "installed fleet is ${latest}, the newest published release"
    else
      advise "installed fleet is ${installed_stamp:-unknown}; ${latest} is published — run: sudo fleet update"
    fi
  else
    advise "could not ask GitHub for the newest release (offline, or ${INSTALL_DIR}/fleet predates \`fleet release\`)"
  fi
fi
if [[ -e "$SRC_DIR/.git" ]]; then
  git_trust_dir "$SRC_DIR"
  # The checkout carries the units, scripts and generic bundle the binary was
  # released with; `fleet update` moves it to the release it installs. A
  # mismatch means one of the two was changed by hand.
  if [[ -f "$SRC_DIR/scripts/version.sh" && -n "$installed_stamp" ]]; then
    checkout_release="$(cd "$SRC_DIR" && bash scripts/version.sh current 2>/dev/null || echo unknown)"
    if [[ "$installed_stamp" == "$checkout_release" ]]; then
      pass "checkout ${SRC_DIR} is at the installed release (${checkout_release})"
    else
      advise "installed fleet is ${installed_stamp} but ${SRC_DIR} is at release ${checkout_release} — run: sudo fleet update --no-pull (reinstalls the checkout's release) or sudo fleet update"
    fi
  fi

  # A modified tree stamps `.dirty` into every build. On a deployed box that is
  # almost never an operator edit — it is a tracked file some build step rewrote
  # (web/next-env.d.ts, until it was gitignored) — so the paths are named rather
  # than leaving the operator to guess what "dirty" means. Untracked files count:
  # they are build inputs too (same rule as scripts/version.sh).
  dirty_paths="$(git -C "$SRC_DIR" status --porcelain --untracked-files=normal 2>/dev/null || true)"
  if [[ -n "$dirty_paths" ]]; then
    n_dirty="$(printf '%s\n' "$dirty_paths" | wc -l | tr -d ' ')"
    dirty_sample="$(printf '%s\n' "$dirty_paths" | head -3 | awk '{print $NF}' | paste -sd ' ' -)"
    [[ "$n_dirty" -gt 3 ]] && dirty_sample="${dirty_sample} …"
    advise "checkout has ${n_dirty} modified/untracked path(s) — the units, scripts and generic bundle it ships may not be the release's: ${dirty_sample} (inspect: git -C ${SRC_DIR} status)"
  else
    pass "checkout is clean"
  fi
else
  advise "no git checkout at $SRC_DIR — 'fleet update' will not work on this box"
fi

# ── summary ──────────────────────────────────────────────────────────────────
echo
if [[ "$n_fail" -gt 0 ]]; then
  printf '%s✗ doctor: %d ok, %d fixed, %d advisories, %d PROBLEMS%s\n' \
    "$c_red" "$n_ok" "$n_fixed" "$n_warn" "$n_fail" "$c_reset"
  exit 1
elif [[ "$n_fixed" -gt 0 ]]; then
  printf '%s✓ doctor: %d ok, %d fixed, %d advisories — box repaired%s\n' \
    "$c_green" "$n_ok" "$n_fixed" "$n_warn" "$c_reset"
else
  printf '%s✓ doctor: %d ok, %d advisories — box healthy%s\n' \
    "$c_green" "$n_ok" "$n_warn" "$c_reset"
fi
