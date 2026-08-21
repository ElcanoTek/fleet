#!/usr/bin/env bash
# scripts/doctor.sh — diagnose and repair a fleet box.
#
# `fleet doctor` walks every box-level prerequisite fleet depends on and
# FIXES what it can in place; `fleet doctor --check` reports without touching
# anything. The goal is killing the version lottery: boxes provisioned at
# different times drift apart (Node 20 vs 22, podman/crun versions with
# different rootless behavior, stale systemd units, missing subuid ranges,
# root-owned podman dirs...) and each drift shows up later as a confusing
# production-only bug. bootstrap.sh and update.sh carry their own inline
# self-heals for the provision/deploy paths; doctor is the standalone,
# run-anytime superset that also covers what those scripts assume is already
# right.
#
# What it does NOT do: git pull or rebuild — that's `fleet update`'s job.
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

set -euo pipefail

# ── locate this script + its repo root (default SRC_DIR) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SRC_DIR="${SRC_DIR:-$REPO_ROOT}"
# These MUST match deploy/fleet.service + deploy/fleet-web.service and
# scripts/bootstrap.sh (SERVICE_USER/SERVICE_HOME there).
SERVICE_USER="${FLEET_SERVICE_USER:-fleet}"
SERVICE_HOME="${FLEET_SERVICE_HOME:-/var/lib/fleet}"
WEB_USER="fleet-web"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
ENV_FILE="${FLEET_ENV_FILE:-/etc/fleet/fleet.env}"
WEB_ENV_FILE="/etc/fleet/fleet-web.env"
# The scheduled-backup pair shipped in deploy/ and installed by
# scripts/bootstrap.sh --enable-service. Fixed names (unlike the fleet unit,
# which FLEET_SERVICE_NAME renames) because bootstrap installs exactly these —
# internal/boxdoctor probes the same two.
BACKUP_SERVICE="fleet-backup.service"
BACKUP_TIMER="fleet-backup.timer"
MAINT_SERVICE="fleet-maintenance.service"
MAINT_TIMER="fleet-maintenance.timer"

# Node floor: Next.js 16 (web/) requires Node >= 20; Fedora's repo nodejs
# satisfies it. Doctor upgrades via dnf only — fleet does not use NodeSource.
NODE_FLOOR=20

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

Checks + fixes: toolchain floors (Node >= 20, go/git/podman/psql present),
fleet-critical package currency (podman/crun/passt/conmon/...), the rootless-
podman prerequisites of the fleet service user (subuid/subgid, /var/lib/fleet
ownership, containers.conf, stale pause namespaces), systemd unit drift vs
deploy/, the 0600 env files, service health (postgresql, fleet, fleet-web,
caddy), the /healthz + /readyz probes, the scheduled-backup and host-maintenance
timers, free disk on the data dir + image store, and a sandbox smoke (podman run
as the fleet user). Reports when the source checkout is behind upstream but never
pulls or rebuilds — that stays `fleet update`.

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

# ── dry-run: print the checklist and exit ────────────────────────────────────
# Doctor's real run is condition-driven (it probes, then fixes what the probe
# found), so --dry-run enumerates the plan instead of half-executing it. This
# is also the CI seam: the Go smoke test asserts the plan's load-bearing steps.
if [[ "$DRY_RUN" == "1" ]]; then
  step "fleet doctor --dry-run (src=${SRC_DIR}, service=${SERVICE_NAME}, install=${INSTALL_DIR})"
  info "[dry-run] 1/9 Toolchain: node >= ${NODE_FLOOR} (dnf upgrade nodejs), go/git/curl/jq/podman/psql/npm present (dnf install)"
  info "[dry-run] 2/9 Package currency: disable broken dnf repos; dnf upgrade fleet-critical packages (podman crun passt conmon containers-common golang nodejs caddy)"
  info "[dry-run] 3/9 Rootless podman: ${SERVICE_USER} user + subuid/subgid ranges, ${SERVICE_HOME} + ~/.config/containers ownership, containers.conf (cgroupfs), /run/${SERVICE_USER}, podman system migrate, podman info as ${SERVICE_USER}"
  info "[dry-run] 4/9 Installed artifacts: ${SERVICE_NAME}.service + fleet-web.service + the fleet-backup and fleet-maintenance service/timer pairs' functional drift vs ${SRC_DIR}/deploy (reinstall + daemon-reload), /usr/local/bin/fleet symlink → ${INSTALL_DIR}/fleet, binaries present"
  info "[dry-run] 5/9 Configuration: ${ENV_FILE} exists root-owned 0600 with OPENROUTER_API_KEY + DB DSNs; ${WEB_ENV_FILE} 0600 when fleet-web is installed"
  info "[dry-run] 6/9 Services: ${SERVICE_NAME} active; postgresql/fleet-web/caddy active when enabled (systemctl start), then /healthz + /readyz respond"
  info "[dry-run] 7/9 Scheduled maintenance: ${BACKUP_TIMER} installed + enabled + active (advisory when absent) and ${BACKUP_SERVICE}'s last run succeeded; ${MAINT_TIMER} likewise; free space on the data dir + the podman image store above the disk floor"
  info "[dry-run] 8/9 Sandbox smoke: podman run --rm --network=none <sandbox image> true as ${SERVICE_USER}"
  info "[dry-run] 9/9 Source freshness: report commits behind upstream (fix stays 'fleet update' — doctor never pulls or rebuilds)"
  info "[dry-run] would restart ${SERVICE_NAME} + fleet-web after a toolchain/package upgrade or an app-unit reinstall above — a reinstalled fleet-backup unit does not bounce the app (unless --no-restart)"
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
step "1/9  Toolchain"

node_major="$(node -v 2>/dev/null | cut -dv -f2 | cut -d. -f1 || true)"
if [[ "${node_major:-0}" -ge "$NODE_FLOOR" ]]; then
  pass "node $(node -v)"
elif [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" == "0" ]]; then
  fail "node $(node -v 2>/dev/null || echo missing) — need >= $NODE_FLOOR for the web tier (Next.js 16)"
else
  # `dnf upgrade`, NOT `dnf install`: dnf5 treats install of an installed
  # package as "nothing to do" (exit 0) instead of upgrading — exactly the
  # silent drift doctor exists to catch, so the result is re-verified.
  if rpm -q nodejs >/dev/null 2>&1; then
    "${DNF[@]}" upgrade -y --quiet nodejs >/dev/null 2>&1 || true
  else
    "${DNF[@]}" install -y --quiet nodejs >/dev/null 2>&1 || true
  fi
  hash -r
  node_major="$(node -v 2>/dev/null | cut -dv -f2 | cut -d. -f1 || true)"
  if [[ "${node_major:-0}" -ge "$NODE_FLOOR" ]]; then
    fixed "node upgraded to $(node -v)"
    restart_needed=1
  else
    fail "node is still $(node -v 2>/dev/null || echo missing) after dnf upgrade — inspect 'dnf repolist'"
  fi
fi

for tool in go git curl jq podman psql npm python3; do
  if command -v "$tool" >/dev/null 2>&1; then
    pass "$tool present"
  elif [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" == "0" ]]; then
    fail "$tool missing"
  else
    # Map command name -> Fedora package where they differ.
    pkg="$tool"
    case "$tool" in
      go)   pkg=golang ;;
      psql) pkg=postgresql ;;
      npm)  pkg=nodejs ;;
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
  advise "no dnf on this host — skipping repo health + package currency (keep podman/crun/nodejs current with your package manager)"
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
  # repo-latest. golang/nodejs ride along because update.sh builds with the
  # box's toolchain.
  CRITICAL_PKGS=(podman crun passt conmon containers-common golang nodejs caddy)
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
  # namespace pins that namespace and poisons later pulls/runs. migrate is the
  # documented reset and a no-op when everything is fine.
  if [[ "$CHECK_ONLY" == "0" ]]; then
    run_as_fleet podman system migrate >/dev/null 2>&1 || true
    pass "podman system migrate run (clears stale pause namespaces)"
  fi

  if run_as_fleet podman info >/dev/null 2>&1; then
    pass "rootless podman functional for $SERVICE_USER"
  else
    podman_err="$(run_as_fleet podman info 2>&1 >/dev/null | tail -n1 || true)"
    if [[ "$restart_needed" == "1" && "$CHECK_ONLY" == "0" && "$NO_RESTART" == "0" ]]; then
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
  for unit in "${SERVICE_NAME}.service" fleet-web.service "$BACKUP_SERVICE" "$BACKUP_TIMER" "$MAINT_SERVICE" "$MAINT_TIMER"; do
    src="$SRC_DIR/deploy/$unit"
    # The generic service name is fleet; a renamed unit has no shipped file.
    [[ "$unit" == "${SERVICE_NAME}.service" && ! -f "$src" ]] && src="$SRC_DIR/deploy/fleet.service"
    dst="/etc/systemd/system/$unit"
    if [[ ! -f "$src" ]]; then
      advise "$unit: no shipped unit under $SRC_DIR/deploy — skipping drift check"
      continue
    fi
    if [[ ! -f "$dst" ]]; then
      # Absent unit is informational: fleet-web is optional, and a box may run
      # fleet under another supervisor. bootstrap --enable-service installs it.
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
  [[ "$units_changed" == "1" ]] && systemctl daemon-reload
fi

if [[ -x "$INSTALL_DIR/fleet" ]]; then
  pass "$INSTALL_DIR/fleet present"
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
  mock="$(env_get FLEET_MOCK_MODE)${CHAT_MOCK_MODE:-}$(env_get CHAT_MOCK_MODE)"
  if [[ -n "$(env_get OPENROUTER_API_KEY)" ]]; then
    pass "OPENROUTER_API_KEY set"
  elif [[ -n "$mock" ]]; then
    advise "OPENROUTER_API_KEY unset but mock mode is on"
  else
    fail "OPENROUTER_API_KEY unset — run: sudo fleet config set-openrouter-key"
  fi
  if [[ -n "$(env_get FLEET_CHAT_DATABASE_URL)" && -n "$(env_get FLEET_SCHED_DATABASE_URL)" ]]; then
    pass "chat + sched database DSNs set"
  elif [[ -n "$(env_get DATABASE_URL)" ]]; then
    advise "only DATABASE_URL set — chat + sched should use SEPARATE databases (FLEET_CHAT_DATABASE_URL / FLEET_SCHED_DATABASE_URL)"
  else
    fail "no database DSNs in $ENV_FILE — rerun scripts/bootstrap.sh"
  fi
fi

if command -v systemctl >/dev/null 2>&1 && systemctl cat fleet-web.service >/dev/null 2>&1; then
  if [[ ! -f "$WEB_ENV_FILE" ]]; then
    fail "$WEB_ENV_FILE missing but fleet-web.service is installed — rerun: scripts/bootstrap.sh --enable-web"
  elif [[ "$(stat -c '%a' "$WEB_ENV_FILE")" == "600" ]]; then
    pass "$WEB_ENV_FILE exists, mode 0600"
  elif [[ "$CHECK_ONLY" == "1" ]]; then
    fail "$WEB_ENV_FILE is mode $(stat -c '%a' "$WEB_ENV_FILE") — want 0600 (it holds the session secret)"
  else
    chmod 0600 "$WEB_ENV_FILE"
    fixed "$WEB_ENV_FILE tightened to 0600"
  fi
fi

# ── 6. services ──────────────────────────────────────────────────────────────
step "6/9  Services"

if ! command -v systemctl >/dev/null 2>&1; then
  advise "no systemd — skipping service checks (verify your supervisor runs 'fleet serve')"
else
  # Optional tiers (postgresql, fleet-web, caddy) gate on `is-enabled`, which
  # is the operator-intent signal: a merely-INSTALLED unit proves only that
  # the rpm is present (e.g. the caddy package ships caddy.service on a box
  # fronted by something else), and an external-Postgres box legitimately
  # leaves postgresql disabled. Only the core fleet unit is unconditional.
  for svc in postgresql.service "${SERVICE_NAME}.service" fleet-web.service caddy.service; do
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

  if [[ "$restart_needed" == "1" && "$CHECK_ONLY" == "0" && "$NO_RESTART" == "0" ]]; then
    systemctl restart "${SERVICE_NAME}.service" 2>/dev/null || true
    systemctl try-restart fleet-web.service 2>/dev/null || true
    fixed "services restarted to pick up fixes"
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
  advise "no ${BACKUP_TIMER} + ${BACKUP_SERVICE} pair installed — nothing on this box dumps the databases; install it with: install -m 0644 -t /etc/systemd/system ${SRC_DIR}/deploy/fleet-backup.{service,timer} && systemctl daemon-reload && systemctl enable --now ${BACKUP_TIMER} (ignore this if you back up at the volume/hypervisor layer)"
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
    advise "no ${MAINT_TIMER} + ${MAINT_SERVICE} pair installed — nothing prunes stale podman image layers on this box; install it with: install -m 0644 -t /etc/systemd/system ${SRC_DIR}/deploy/fleet-maintenance.{service,timer} && systemctl daemon-reload && systemctl enable --now ${MAINT_TIMER} (ignore this if you prune the container store yourself)"
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

# ── 8. sandbox smoke ─────────────────────────────────────────────────────────
step "8/9  Sandbox smoke"

# Deferred from step 3: the post-upgrade `podman info` failure should have
# cleared once step 6 restarted the services off the old container stack.
if [[ "${podman_recheck:-0}" == "1" ]]; then
  if run_as_fleet podman info >/dev/null 2>&1; then
    fixed "rootless podman functional for $SERVICE_USER (recovered by the post-upgrade service restart)"
  else
    podman_err="$(run_as_fleet podman info 2>&1 >/dev/null | tail -n1 || true)"
    fail "rootless 'podman info' STILL fails as $SERVICE_USER after the restart: ${podman_err:-no error output} — run: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman info"
  fi
fi

# Resolve the image ref the daemon would use: env file wins, else the client
# bundle's sandbox.tag (same precedence as the process — see admincli
# checkSandbox / clientconfig ResolvedImageRef). The image is built ON-BOX into
# the fleet user's rootless store (bootstrap/update), never pulled here.
sandbox_img="$(env_get FLEET_SANDBOX_IMAGE)"
[[ -z "$sandbox_img" ]] && sandbox_img="$(env_get CHAT_SANDBOX_IMAGE)"
if [[ -z "$sandbox_img" ]]; then
  bundle_dir="$(env_get FLEET_CLIENT_CONFIG_DIR)"
  [[ -z "$bundle_dir" && -d "$INSTALL_DIR/client" ]] && bundle_dir="$INSTALL_DIR/client"
  [[ -z "$bundle_dir" ]] && bundle_dir="$SRC_DIR/config/default"
  # Same minimal manifest read as bootstrap's sandbox_manifest_tag.
  sandbox_img="$(awk '
    /^sandbox:[[:space:]]*$/ { b=1; next }
    /^[^[:space:]]/          { b=0 }
    b && /^[[:space:]]+tag:/ { sub("^[[:space:]]+tag:[[:space:]]*",""); sub(/[[:space:]]+#.*$/,""); gsub(/^["'\'']|["'\'']$/,""); print; exit }
  ' "$bundle_dir/manifest.yaml" 2>/dev/null)"
  sandbox_img="${sandbox_img:-localhost/fleet-sandbox:latest}"
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1 || ! command -v podman >/dev/null 2>&1; then
  advise "skipping sandbox smoke (needs the $SERVICE_USER user + podman)"
elif run_as_fleet podman image exists "$sandbox_img" 2>/dev/null; then
  # The definitive check: launch the image in the exact rootless environment
  # the daemon uses. --network=none mirrors the runtime's default isolation.
  if run_as_fleet timeout 120 podman run --rm --network=none "$sandbox_img" true >/dev/null 2>&1; then
    pass "sandbox smoke passed ($sandbox_img runs as $SERVICE_USER)"
  else
    fail "sandbox image $sandbox_img present but NOT runnable as $SERVICE_USER — tool calls will break; rerun verbosely: cd $SERVICE_HOME && sudo -u $SERVICE_USER HOME=$SERVICE_HOME XDG_RUNTIME_DIR=/run/$SERVICE_USER podman run --rm $sandbox_img true"
  fi
else
  fail "sandbox image $sandbox_img missing from ${SERVICE_USER}'s rootless store — build it: sudo fleet update (or sudo FLEET_CLIENT_CONFIG_DIR=<bundle> scripts/build-sandbox-image.sh; a root run builds into ${SERVICE_USER}'s store)"
fi

# ── 9. source freshness ──────────────────────────────────────────────────────
step "9/9  Source freshness"

# Report-only in every mode: pulling and rebuilding is `fleet update`'s job,
# and doctor silently kicking off a deploy would be a surprise.
if [[ -e "$SRC_DIR/.git" ]]; then
  git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true
  if git -C "$SRC_DIR" fetch --quiet 2>/dev/null; then
    behind="$(git -C "$SRC_DIR" rev-list --count 'HEAD..@{upstream}' 2>/dev/null || echo 0)"
    if [[ "${behind:-0}" -gt 0 ]]; then
      advise "source checkout is $behind commit(s) behind — run: sudo fleet update"
    else
      pass "source checkout is current with upstream"
    fi
  else
    advise "could not fetch $SRC_DIR — network or remote auth issue"
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
