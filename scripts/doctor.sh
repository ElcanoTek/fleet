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
#   sudo fleet doctor --node       repair ONLY the node toolchain (step 1's node
#                                  blocks: install nodejs<major> + -npm per
#                                  web/.nvmrc, stamp FLEET_NODE_BIN, assert the
#                                  resolved value) and exit
#   fleet doctor --node --check    read-only node readiness probe; no root needed
#   fleet doctor --dry-run         print the checklist this box would be walked through; touch nothing
#
# Why --node exists: scripts/update.sh is an updater, not a provisioner, so it
# must not grow its own `dnf install nodejs`. But an update that dies because
# the box is a major behind sends the operator away to find the repair command
# themselves. --node is the narrow seam between the two: update.sh calls THIS
# code path (one implementation of the node install, the same one bootstrap and
# a full doctor run use) instead of duplicating it, and gets back a box it can
# build on. It is deliberately scoped to the node blocks — a full doctor run
# from inside update would perform unit adoption, which `fleet update` gates
# behind explicit consent (--adopt-units), so calling it would launder a
# consent-gated write through an unrelated command.
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
# shellcheck disable=SC2034  # unread here on purpose: this block documents the
# service-account contract that deploy/fleet-web.service and bootstrap.sh must
# match, and dropping the name would remove the anchor the comment above names.
WEB_USER="fleet-web"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
ENV_FILE="${FLEET_ENV_FILE:-/etc/fleet/fleet.env}"
# Overridable so the script's own tests can point the stamp check at a scratch
# file instead of this box's real one. Without that the node checks read machine
# GLOBAL state, and a stale /etc/fleet/fleet-web.env makes them pass or fail for
# reasons that have nothing to do with the code under test. It grants a caller
# nothing: every path that writes it already requires root.
WEB_ENV_FILE="${FLEET_WEB_ENV_FILE:-/etc/fleet/fleet-web.env}"
# The scheduled-backup pair shipped in deploy/ and installed by
# scripts/bootstrap.sh --enable-service. Fixed names (unlike the fleet unit,
# which FLEET_SERVICE_NAME renames) because bootstrap installs exactly these —
# internal/boxdoctor probes the same two.
BACKUP_SERVICE="fleet-backup.service"
BACKUP_TIMER="fleet-backup.timer"
MAINT_SERVICE="fleet-maintenance.service"
MAINT_TIMER="fleet-maintenance.timer"

# Node floor: read from web/.nvmrc, the ONE place the target major is declared
# (CI reads the same file via actions/setup-node's node-version-file). Hardcoding
# it here is what let CI test '22' while this script's floor said 20 and the box
# ran whatever `dnf install nodejs` meant. Doctor installs via dnf only — fleet
# does not use NodeSource.
# shellcheck source=lib/node-version.sh
. "$SCRIPT_DIR/lib/node-version.sh"
# The fleet-managed Caddyfile (marker, renderer, drift helpers) — shared with
# bootstrap.sh (which writes it) and update.sh (which offers to adopt it).
# shellcheck source=lib/caddyfile.sh
. "$SCRIPT_DIR/lib/caddyfile.sh"
NODE_FLOOR="$(fleet_node_major_want "$SRC_DIR" || true)"
if [[ -z "$NODE_FLOOR" ]]; then
  # No silent default. A hardcoded fallback would point at whatever major was
  # current when this line was written — i.e. it would rot into targeting the
  # OLD node exactly when .nvmrc cannot be read. Better to say so: every node
  # check below is skipped rather than run against a guess.
  NODE_FLOOR=""
fi

CHECK_ONLY=0
NO_RESTART=0
DRY_RUN=0
NODE_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --check)      CHECK_ONLY=1 ;;
    --no-restart) NO_RESTART=1 ;;
    --dry-run)    DRY_RUN=1 ;;
    --node)       NODE_ONLY=1 ;;
    -h|--help)
      cat <<'EOF'
fleet doctor — diagnose and repair this fleet box

USAGE
  sudo fleet doctor              diagnose + fix + restart services if needed
  sudo fleet doctor --check      diagnose only, change nothing, exit 1 if anything is off
  sudo fleet doctor --no-restart fix but never restart services
  sudo fleet doctor --node       repair ONLY the node toolchain, then exit
  fleet doctor --node --check    read-only node readiness probe (no root needed)
  fleet doctor --dry-run         print the checklist; touch nothing (no root needed)

Checks + fixes: toolchain floors (node >= the major in web/.nvmrc — the ONE
place it is declared, so this text cannot drift from it; go/git/podman/psql present),
fleet-critical package currency (podman/crun/passt/conmon/...), the rootless-
podman prerequisites of the fleet service user (subuid/subgid, /var/lib/fleet
ownership, containers.conf, stale pause namespaces), systemd unit drift vs
deploy/, the 0600 env files, the fleet-managed /etc/caddy/Caddyfile's layout
(the /v1 API + webhooks must reach the Go backends, not the web tier — rewritten
+ caddy reloaded when drifted), service health (postgresql, fleet, fleet-web,
caddy), the /healthz + /readyz probes and https://<domain>/api-info THROUGH
caddy, the scheduled-backup and host-maintenance
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

# upsert_web_env KEY VALUE — set one key in /etc/fleet/fleet-web.env in place.
#
# Rewrites every occurrence of the key (not just the first) and appends it if
# absent, and NEVER truncates the file: it carries operator-added keys
# (AUTH_SIGNING_PUBKEY, AUTH_LOGIN_URL, …) that a wholesale rewrite silently
# dropped once already in bootstrap. mktemp next to the target + mv is a
# same-filesystem atomic rename, so a crash mid-write cannot leave the web tier
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
upsert_web_env() {
  local key="$1" val="$2" file="$WEB_ENV_FILE" tmp
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

# ── dry-run: print the checklist and exit ────────────────────────────────────
# Doctor's real run is condition-driven (it probes, then fixes what the probe
# found), so --dry-run enumerates the plan instead of half-executing it. This
# is also the CI seam: the Go smoke test asserts the plan's load-bearing steps.
if [[ "$DRY_RUN" == "1" && "$NODE_ONLY" == "1" ]]; then
  step "fleet doctor --node --dry-run (src=${SRC_DIR})"
  if [[ -n "$NODE_FLOOR" ]]; then
    info "[dry-run] node >= ${NODE_FLOOR}: dnf install nodejs${NODE_FLOOR} nodejs${NODE_FLOOR}-npm (the VERSIONED stream; \`dnf upgrade nodejs\` cannot cross a major), then point fleet-web at it via FLEET_NODE_BIN in ${WEB_ENV_FILE} and assert the RESOLVED interpreter"
  else
    # Naming "nodejs-npm" and calling it the versioned stream would be a lie in
    # exactly the case where nothing can be resolved.
    info "[dry-run] node: cannot read the major from ${SRC_DIR}/web/.nvmrc, so there is no versioned package to name — the real run reports that and changes nothing"
  fi
  info "[dry-run] nothing else — --node is scoped to the node toolchain; a full \`sudo fleet doctor\` walks all 9 steps"
  exit 0
fi
if [[ "$DRY_RUN" == "1" ]]; then
  step "fleet doctor --dry-run (src=${SRC_DIR}, service=${SERVICE_NAME}, install=${INSTALL_DIR})"
  info "[dry-run] 1/9 Toolchain: node >= ${NODE_FLOOR:-<web/.nvmrc>} (dnf install nodejs${NODE_FLOOR} — the VERSIONED stream; \`dnf upgrade nodejs\` cannot cross a major), then point fleet-web at it via FLEET_NODE_BIN in ${WEB_ENV_FILE}; go/git/curl/jq/podman/psql/npm present (dnf install)"
  info "[dry-run] 2/9 Package currency: disable broken dnf repos; dnf upgrade fleet-critical packages (podman crun passt conmon containers-common golang nodejs nodejs${NODE_FLOOR} caddy)"
  info "[dry-run] 3/9 Rootless podman: ${SERVICE_USER} user + subuid/subgid ranges, ${SERVICE_HOME} + ~/.config/containers ownership, containers.conf (cgroupfs), /run/${SERVICE_USER}, podman system migrate, podman info as ${SERVICE_USER}"
  info "[dry-run] 4/9 Installed artifacts: ${SERVICE_NAME}.service + fleet-web.service + the fleet-backup and fleet-maintenance service/timer pairs' functional drift vs ${SRC_DIR}/deploy (reinstall + daemon-reload), /usr/local/bin/fleet-web-start.sh (fleet-web's ExecStart shim) and fleet-web.service.d/10-timeout-kill.conf, then assert the RESOLVED TimeoutStopFailureMode, /usr/local/bin/fleet symlink → ${INSTALL_DIR}/fleet, binaries present"
  info "[dry-run] 5/9 Configuration: ${ENV_FILE} exists root-owned 0600 with OPENROUTER_API_KEY + DB DSNs; ${WEB_ENV_FILE} 0600 when fleet-web is installed; ${FLEET_CADDYFILE:-/etc/caddy/Caddyfile} (when fleet-managed) matches scripts/lib/caddyfile.sh — /v1/*, /api-info, agent card, /triggers/* → orchestrator, /webhooks/* → chat (rewrite from the renderer, backup kept, caddy reload); an operator-managed Caddyfile only gets an advisory when it routes no /v1"
  info "[dry-run] 6/9 Services: ${SERVICE_NAME} active; postgresql/fleet-web/caddy active when enabled (systemctl start), then /healthz + /readyz respond, then https://<caddy domain>/api-info answers THROUGH caddy (--resolve pinned to 127.0.0.1) when caddy is active"
  info "[dry-run] 7/9 Scheduled maintenance: ${BACKUP_TIMER} installed + enabled + active (advisory when absent) and ${BACKUP_SERVICE}'s last run succeeded; ${MAINT_TIMER} likewise; free space on the data dir + the podman image store above the disk floor"
  info "[dry-run] 8/9 Sandbox smoke: podman run --rm --network=none <sandbox image> true as ${SERVICE_USER}"
  info "[dry-run] 9/9 Source freshness: report commits behind upstream (fix stays 'fleet update' — doctor never pulls or rebuilds)"
  info "[dry-run] would restart ${SERVICE_NAME} + fleet-web after a toolchain/package upgrade or an app-unit reinstall above — a reinstalled fleet-backup unit does not bounce the app (unless --no-restart)"
  exit 0
fi

# `--node --check` is the one read-only path that must work unprivileged: it is
# what `fleet update --check` calls, and that is documented as a dev-box probe
# needing no root. It resolves an interpreter from PATH and reads
# /etc/fleet/fleet-web.env through env_get, which returns empty (not an error)
# when the 0600 file is unreadable — so a non-root run degrades to "cannot see
# the stamp" rather than lying about it.
if ! [[ "$NODE_ONLY" == "1" && "$CHECK_ONLY" == "1" ]]; then
  [[ $EUID -eq 0 ]] || die "run as root: sudo fleet doctor (or --dry-run to preview)"
fi
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
if [[ "$NODE_ONLY" == "1" ]]; then
  step "Toolchain — node only (--node)"
else
  step "1/9  Toolchain"
fi

if [[ -z "$NODE_FLOOR" ]]; then
  advise "cannot read the node major from ${SRC_DIR}/web/.nvmrc — skipping the node checks (no guessed default)"
  node_bin=""
else
node_bin="$(fleet_resolve_node_bin "$NODE_FLOOR" || true)"
if [[ -n "$node_bin" ]]; then
  pass "node $("$node_bin" -v) at ${node_bin} (>= $NODE_FLOOR, per web/.nvmrc)"
elif [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" == "0" ]]; then
  fail "no node >= $NODE_FLOOR (web/.nvmrc) — have $(node -v 2>/dev/null || echo none); the web tier needs it"
else
  # Install the VERSIONED package. `dnf upgrade nodejs` cannot cross a major
  # stream — it keeps you on whatever `nodejs` resolves to, which is precisely
  # why a box sat on 22 through repeated doctor runs. Streams are
  # parallel-installable, so this adds the new major without removing the old.
  # Both packages, always. On Fedora `nodejs<major>` does NOT pull npm, and the
  # tool loop below only installs it when `command -v npm` FAILS — which it does
  # not on the target box, because the OLD stream's npm is already on PATH and
  # satisfies the check while belonging to the wrong interpreter. That is the
  # same parallel-stream trap the versioned node install exists to dodge, and it
  # left `--node` installing only half of what its own plan line, the header,
  # DOCTOR.md and the CHANGELOG all said it installed. bootstrap.sh has asked
  # for both since it learned about the versioned stream.
  "${DNF[@]}" install -y --quiet "nodejs${NODE_FLOOR}" "nodejs${NODE_FLOOR}-npm" >/dev/null 2>&1 \
    || "${DNF[@]}" install -y --quiet "nodejs${NODE_FLOOR}" >/dev/null 2>&1 || true
  hash -r
  node_bin="$(fleet_resolve_node_bin "$NODE_FLOOR" || true)"
  if [[ -n "$node_bin" ]]; then
    fixed "installed node $("$node_bin" -v) at ${node_bin}"
    restart_needed=1
  else
    # Fall back to the unversioned package before giving up: a distro that does
    # not carry a versioned stream may still ship a new enough default.
    if rpm -q nodejs >/dev/null 2>&1; then
      "${DNF[@]}" upgrade -y --quiet nodejs >/dev/null 2>&1 || true
    else
      "${DNF[@]}" install -y --quiet nodejs >/dev/null 2>&1 || true
    fi
    hash -r
    node_bin="$(fleet_resolve_node_bin "$NODE_FLOOR" || true)"
    if [[ -n "$node_bin" ]]; then
      fixed "installed node $("$node_bin" -v) at ${node_bin}"
      restart_needed=1
    else
      fail "no node >= $NODE_FLOOR after installing nodejs${NODE_FLOOR} and nodejs — inspect 'dnf repolist' / 'dnf list nodejs*'"
    fi
  fi
fi

fi

# Whatever node exists, the tier only USES the one FLEET_NODE_BIN names (the
# shim falls back to PATH, i.e. Fedora's default stream, when it is unset). So
# assert the effective value, not merely that a good node is installed
# somewhere — the same resolved-value rule the stop-policy check follows below.
if [[ -n "$node_bin" && -f "$WEB_ENV_FILE" && ! -r "$WEB_ENV_FILE" ]]; then
  # The file is there but this process cannot read it — the unprivileged
  # `--node --check` path (which `fleet update --check` calls). env_get returns
  # empty for an unreadable file, which is indistinguishable from "unset", so
  # without this branch a correctly-stamped box reports
  # "FLEET_NODE_BIN is unset ... the tier would serve on the wrong major" and
  # `fleet update --check` turns that into a non-zero exit. Reporting a fault
  # you could not observe is the same error as reporting a success you could
  # not observe.
  advise "cannot read ${WEB_ENV_FILE} as uid ${EUID} (it is 0600 root-owned) — re-run with sudo to check the FLEET_NODE_BIN stamp"
elif [[ -n "$node_bin" && -f "$WEB_ENV_FILE" ]]; then
  # env_get, not grep -m1: it takes the LAST assignment (matching systemd's
  # EnvironmentFile precedence) and strips surrounding quotes, so a hand-written
  # FLEET_NODE_BIN="/usr/bin/node-24" is not reported as a broken config.
  cur_node_bin="$(env_get FLEET_NODE_BIN "$WEB_ENV_FILE")"
  cur_major=""
  [[ -n "$cur_node_bin" && -x "$cur_node_bin" ]] && \
    cur_major="$("$cur_node_bin" -v 2>/dev/null | sed 's/^v//' | cut -d. -f1)"
  if [[ "${cur_major:-0}" -ge "$NODE_FLOOR" ]]; then
    pass "fleet-web runs ${cur_node_bin} ($("$cur_node_bin" -v))"
  elif [[ "$CHECK_ONLY" == "1" ]]; then
    fail "fleet-web's FLEET_NODE_BIN is ${cur_node_bin:-unset} — not node >= $NODE_FLOOR; the tier would serve on the wrong major"
  else
    if ! upsert_web_env FLEET_NODE_BIN "$node_bin"; then
      fail "could not set FLEET_NODE_BIN in ${WEB_ENV_FILE} — add: FLEET_NODE_BIN=${node_bin}"
    else
      # Read the value BACK through the same last-wins reader systemd's
      # EnvironmentFile uses, rather than trusting upsert_web_env's exit code.
      # The writer returning 0 only proves a file was written; it does not
      # prove the tier now resolves to this interpreter. Same rule as the
      # TimeoutStopFailureMode assertion below: claim what the system
      # resolved, never the value you wrote.
      _nb_readback="$(env_get FLEET_NODE_BIN "$WEB_ENV_FILE")"
      _nb_major=""
      [[ -n "$_nb_readback" && -x "$_nb_readback" ]] && \
        _nb_major="$("$_nb_readback" -v 2>/dev/null | sed 's/^v//' | cut -d. -f1)"
      if [[ "$_nb_readback" == "$node_bin" && "${_nb_major:-0}" -ge "$NODE_FLOOR" ]]; then
        fixed "pointed fleet-web at ${_nb_readback} ($("$_nb_readback" -v)) — read back from ${WEB_ENV_FILE}"
        restart_needed=1
      else
        fail "wrote FLEET_NODE_BIN=${node_bin} but ${WEB_ENV_FILE} reads back ${_nb_readback:-<unset>} — inspect it by hand"
      fi
    fi
  fi
fi

# The npm that BELONGS to node_bin, checked separately from the `command -v npm`
# probe in the tool loop below. That probe only asks whether SOME npm exists, and
# on a box with parallel streams the one on PATH belongs to the OLD interpreter
# — Fedora rewrites npm's shebang to an absolute `#!/usr/bin/node-<major>`, so it
# stays on that major whatever PATH says. That is what built the web tier on
# node 22 through an update whose own gate had resolved node 24, and it surfaced
# only as npm's EBADENGINE warning mid-build. Same parallel-stream trap as the
# versioned node install, one package over — and the tool loop cannot see it.
if [[ -n "$node_bin" ]]; then
  npm_cli="$(fleet_resolve_npm_cli "$node_bin" || true)"
  # Only a VERSIONED interpreter lets us name the exact package that is short.
  # On a single-node layout (Debian/nodesource/nvm) an unresolvable npm-cli.js
  # means npm is shipped in a shape this check cannot read, which is an
  # observation, not a diagnosis — so it advises rather than failing.
  node_bin_is_versioned=0
  [[ "${node_bin##*/}" =~ ^node-[0-9]+$ ]] && node_bin_is_versioned=1
  if [[ -n "$npm_cli" ]]; then
    pass "npm for ${node_bin}: ${npm_cli} (the build pins this pair, not PATH)"
  elif [[ "$node_bin_is_versioned" == "0" ]]; then
    advise "cannot resolve an npm-cli.js for ${node_bin} — the web tier build will use \`npm\` from PATH ($(command -v npm 2>/dev/null || echo none))"
  elif [[ "$CHECK_ONLY" == "1" || "$HAVE_DNF" == "0" ]]; then
    fail "no npm belongs to ${node_bin} — install nodejs${NODE_FLOOR}-npm; until then the web tier builds on $(node -v 2>/dev/null || echo 'the default stream')"
  else
    "${DNF[@]}" install -y --quiet "nodejs${NODE_FLOOR}-npm" >/dev/null 2>&1 || true
    hash -r
    npm_cli="$(fleet_resolve_npm_cli "$node_bin" || true)"
    # No restart_needed=1: the unit runs `next start` under FLEET_NODE_BIN and
    # never invokes npm, so this repair changes the next BUILD, not the running
    # tier. Claiming a restart would apply it would be the wrong claim.
    if [[ -n "$npm_cli" ]]; then
      fixed "installed the npm that belongs to ${node_bin} (${npm_cli})"
    else
      fail "no npm belongs to ${node_bin} after installing nodejs${NODE_FLOOR}-npm — inspect 'dnf list nodejs*'"
    fi
  fi
fi

doctor_tools=(go git curl jq podman psql npm python3)
# --node covers the node TOOLCHAIN, and on Fedora npm is a separate package
# (nodejs<major>-npm) — an update that resolves node 24 and then cannot run
# `npm ci` is not a repaired box. The other entries are out of scope here.
[[ "$NODE_ONLY" == "1" ]] && doctor_tools=(npm)
for tool in "${doctor_tools[@]}"; do
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
      # NOT plain `nodejs`: that is the DEFAULT stream, so installing it to
      # satisfy a missing npm drags the older interpreter back onto the box and
      # re-points /usr/bin/node at it — undoing the versioned install above.
      npm)  pkg="nodejs${NODE_FLOOR}-npm" ;;
    esac
    if "${DNF[@]}" install -y --quiet "$pkg" >/dev/null 2>&1; then
      fixed "$tool installed (dnf $pkg)"
    else
      fail "$tool missing and dnf install $pkg failed"
    fi
  fi
done

# ── --node: scoped exit ──────────────────────────────────────────────────────
# Everything above is the shared step-1 node code; nothing below it is in scope
# for --node. The summary re-resolves from scratch instead of reporting the
# variables set above: an install that "succeeded" but left no qualifying
# interpreter on PATH must read as a failure here, not as a repair.
if [[ "$NODE_ONLY" == "1" ]]; then
  echo
  if [[ -z "$NODE_FLOOR" ]]; then
    printf '%s✗ doctor --node: cannot read the node major from %s/web/.nvmrc%s\n' \
      "$c_red" "$SRC_DIR" "$c_reset"
    exit 1
  fi
  _final_bin="$(fleet_resolve_node_bin "$NODE_FLOOR" || true)"
  if [[ -z "$_final_bin" ]]; then
    printf '%s✗ doctor --node: no node >= %s on this box (have %s)%s\n' \
      "$c_red" "$NODE_FLOOR" "$(node -v 2>/dev/null || echo none)" "$c_reset"
    exit 1
  fi
  if [[ "$n_fail" -gt 0 ]]; then
    printf '%s✗ doctor --node: node %s at %s, but %d problem(s) remain above%s\n' \
      "$c_red" "$("$_final_bin" -v)" "$_final_bin" "$n_fail" "$c_reset"
    exit 1
  fi
  # Re-resolved from scratch for the same reason the interpreter is: `--node`
  # advertises node AND its npm, `fleet update --check` turns this exit code
  # into its own, and an npm that does not belong to _final_bin means the next
  # update builds the tier on the old major. A versioned interpreter is the only
  # case where the missing package is nameable; elsewhere the tool loop's plain
  # `npm present` check is all this can honestly stand on.
  if [[ "${_final_bin##*/}" =~ ^node-[0-9]+$ ]] && ! fleet_resolve_npm_cli "$_final_bin" >/dev/null 2>&1; then
    printf '%s✗ doctor --node: node %s at %s, but no npm belongs to it (install nodejs%s-npm)%s\n' \
      "$c_red" "$("$_final_bin" -v)" "$_final_bin" "$NODE_FLOOR" "$c_reset"
    exit 1
  fi
  # A scoped run exits before the restart block a full pass would reach, so it
  # has to say so itself. Swapping the interpreter under a RUNNING fleet-web
  # changes nothing until the unit restarts, and this exit line is what
  # update.sh and the docs send operators to — reporting "repaired" while the
  # live tier still serves on the old major would be a fix that is not in
  # effect. update.sh's own restart (step 5) covers the in-update path, so the
  # advice is only for a standalone run.
  if [[ "$restart_needed" == "1" && "$CHECK_ONLY" != "1" ]]; then
    advise "fleet-web is still running the old interpreter — apply it: sudo systemctl restart fleet-web  (a full \`sudo fleet doctor\` restarts for you; \`sudo fleet update\` restarts at step 5)"
  fi
  printf '%s✓ doctor --node: node %s at %s (>= %s, per web/.nvmrc), %d fixed%s\n' \
    "$c_green" "$("$_final_bin" -v)" "$_final_bin" "$NODE_FLOOR" "$n_fixed" "$c_reset"
  exit 0
fi

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
  # nodejs${NODE_FLOOR} rides along with plain nodejs: on Fedora they are
  # SEPARATE parallel-installable packages, so keeping `nodejs` current does
  # nothing for the versioned stream the web tier actually runs.
  CRITICAL_PKGS=(podman crun passt conmon containers-common golang nodejs "nodejs${NODE_FLOOR}" caddy)
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
  # fleet-web's ExecStart shim. Shipped content with no operator-tunable parts,
  # and the unit will not start without it, so it is installed/refreshed rather
  # than merely reported. A stale copy is worse than a missing one: it would
  # resolve the node interpreter by older rules than the release intends.
  web_shim_src="$SRC_DIR/deploy/fleet-web-start.sh"
  web_shim_dst="/usr/local/bin/fleet-web-start.sh"
  if [[ -f "$web_shim_src" && -f /etc/systemd/system/fleet-web.service ]]; then
    if [[ -x "$web_shim_dst" ]] && cmp -s "$web_shim_src" "$web_shim_dst"; then
      pass "fleet-web-start.sh matches deploy/"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "${web_shim_dst} missing or drifted — fleet-web's ExecStart points at it (diff: $web_shim_dst $web_shim_src)"
    else
      install -D -m 0755 "$web_shim_src" "$web_shim_dst" \
        && { fixed "installed ${web_shim_dst} from deploy/"; restart_needed=1; } \
        || fail "could not install ${web_shim_dst}"
    fi
  fi
  # fleet-web's companion drop-in (deploy/fleet-web.service.d/) restates
  # TimeoutStopFailureMode=kill at the precedence level that beats Fedora's
  # global service.d abort drop-in. What it buys: a stop that overruns its
  # deadline is SIGKILLed rather than SIGABRTed. It is NOT what keeps the core
  # dump away — the unit's LimitCORE=0 does that on its own, for every signal
  # (systemd-coredump stores a dump only when the process's RLIMIT_CORE is
  # sufficient). So a missing drop-in is a correctness/hygiene gap, not a
  # return of the 130 MB-per-restart dump pile.
  # A drop-in swap needs no app restart; daemon-reload suffices.
  web_dropin_src="$SRC_DIR/deploy/fleet-web.service.d/10-timeout-kill.conf"
  web_dropin_dst="/etc/systemd/system/fleet-web.service.d/10-timeout-kill.conf"
  if [[ -f "$web_dropin_src" && -f /etc/systemd/system/fleet-web.service ]]; then
    if [[ -f "$web_dropin_dst" ]] && diff -q <(grep -vE '^[[:space:]]*(#|$)' "$web_dropin_src") <(grep -vE '^[[:space:]]*(#|$)' "$web_dropin_dst") >/dev/null 2>&1; then
      pass "fleet-web.service.d/10-timeout-kill.conf matches deploy/"
    elif [[ "$CHECK_ONLY" == "1" ]]; then
      fail "fleet-web.service.d/10-timeout-kill.conf missing or drifted — review: diff $web_dropin_dst $web_dropin_src"
    else
      install -D -m 0644 "$web_dropin_src" "$web_dropin_dst"
      fixed "fleet-web.service.d/10-timeout-kill.conf installed from deploy/"
      units_changed=1
    fi
  fi
  [[ "$units_changed" == "1" ]] && systemctl daemon-reload

  # Now that any reload has happened, check what systemd ACTUALLY resolved —
  # not what we wrote. Every earlier check here compares FILES, which is a
  # proxy: the whole reason this drop-in exists is that a unit body can say
  # `kill` while systemd resolves `abort` (a global /usr/lib service.d drop-in
  # wins over a unit body, and that shipped undetected once already). File
  # equality also cannot see a stale checkout that made the block above skip
  # silently, a drop-in installed to the wrong directory, or a later-sorting
  # drop-in in the same directory overriding ours. The resolved value can see
  # all of them, so it is the thing worth asserting.
  if [[ -f /etc/systemd/system/fleet-web.service ]]; then
    effective_tsfm="$(systemctl show -p TimeoutStopFailureMode --value fleet-web.service 2>/dev/null || true)"
    case "$effective_tsfm" in
      kill)
        pass "fleet-web TimeoutStopFailureMode resolves to kill (an overrun stop is SIGKILLed, not SIGABRTed)"
        ;;
      "")
        # Pre-246 systemd has no such property; nothing to verify or fix.
        advise "this systemd does not expose TimeoutStopFailureMode — an overrun fleet-web stop uses the distro default"
        ;;
      *)
        # Name the resolved value rather than guessing its consequence: `abort`
        # dumps core, `terminate` (systemd's own default) just re-sends SIGTERM
        # and can leave a wedged process behind. Neither is what we ship.
        fail "fleet-web TimeoutStopFailureMode resolves to '${effective_tsfm}', not kill — an overrun stop will not be SIGKILLed"
        advise "  the shipped drop-in is not winning; inspect the full picture: systemctl cat fleet-web"
        advise "  (a global /usr/lib/systemd/system/service.d/ drop-in, or a later-sorting file in"
        advise "   /etc/systemd/system/fleet-web.service.d/, overrides the unit body)"
        ;;
    esac
  fi
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

# ── the TLS front's routing (the fleet-managed Caddyfile) ────────────────────
# A Caddyfile that only knows the web tier sends every documented API URL
# (/v1/…, /api-info, the A2A agent card, /triggers/…, /webhooks/…) to Next.js,
# which 404s them — "the API isn't routing" with every unit green. bootstrap
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
        "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)")"
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
  elif caddyfile_routes_api "$CADDYFILE" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then
    pass "$CADDYFILE is operator-managed and routes /v1 to the orchestrator"
  else
    advise "$CADDYFILE is not fleet-managed and does not appear to route the API — /v1/*, /api-info, /.well-known/agent-card.json, /a2a, /triggers/* belong on the orchestrator (127.0.0.1:8000) and /webhooks/* on chat (127.0.0.1:8080); see deploy/Caddyfile"
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
  git config --global --add safe.directory "$dir" 2>/dev/null || true
  # -uall lists files inside untracked dirs, so a single `reports/` holding 300
  # CSVs reads as 300 rather than 1. --ignored is deliberately NOT passed: a
  # bundle that has taken the .gitignore safety net would otherwise report clean
  # while still filling the disk.
  mapfile -t _residue < <(git -C "$dir" status --porcelain -uall --ignored=no 2>/dev/null | awk '$1=="??"{sub(/^\?\? /,""); print}')
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
