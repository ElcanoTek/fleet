#!/usr/bin/env bash
# scripts/bootstrap.sh — fleet DB + credential-store bootstrap for fleet.
#
# Merges chat's + moc's bootstrap into ONE script with a --postgres=local|external
# branch (default local). It provisions ONE role and ONE database (default
# fleet/fleet; the chat and sched stores live in its schemas chat and sched,
# ADR-0098), writes DATABASE_URL, and ensures the 0600 credential env file
# exists. It NEVER runs application migrations — each store self-migrates on
# first start (chat's advisory-lock runner; sched's golang-migrate-compatible
# runner), creating its schema.
#
# An env file that still carries the retired per-store keys
# FLEET_CHAT_DATABASE_URL / FLEET_SCHED_DATABASE_URL / SCHED_DATABASE_URL (the
# two-database layout, removed in #2049) is refused: fleet reads DATABASE_URL
# only (docs/ONE-DATABASE.md).
#
# It is IDEMPOTENT end to end: re-runs converge on the same state (roles/dbs are
# created only when missing via \gexec; the env file is refreshed in place; the
# sandbox image is rebuilt; the client bundle checkout is updated or left as-is).
#
# It REFUSES to touch a Postgres role/database it did not provision: a role or
# database matching the configured names that already exists on the cluster —
# and is not recorded in this script's env file from a previous run — is treated
# as someone else's (e.g. a legacy chat/moc install) and the script stops with
# instructions. The operator must choose explicitly: fresh names
# (--db-name/--db-user) or adoption (--adopt-existing-db, which NEVER creates
# or alters the role — no password rotation — and requires the operator to
# supply a working DSN).
#
# Usage:
#   scripts/bootstrap.sh                             # INTERACTIVE on a terminal: prompts for the
#                                                    # systemd-service / web-UI / domain choices, then
#                                                    # the OpenRouter key, the Elcano SSO key (web), and
#                                                    # admin users — a fresh box end-to-end, no flags.
#   scripts/bootstrap.sh --postgres=local            # dnf+initdb+pg_hba+\gexec, sslmode=disable
#   scripts/bootstrap.sh --postgres=external         # validate DSNs with SELECT 1, sslmode=require
#   scripts/bootstrap.sh --postgres=local --dry-run  # print the plan, touch nothing (prompts still ask,
#                                                    # so the plan reflects your answers; pipe stdin to skip)
#   scripts/bootstrap.sh --client-config <git-url[#<sha-or-tag>]|path>   # check out / point at a client bundle
#   scripts/bootstrap.sh --enable-service            # systemctl enable --now fleet at the end
#   scripts/bootstrap.sh --enable-service --no-backup-timer  # ...without the daily database-backup timer
#   scripts/bootstrap.sh --enable-service --no-maintenance-timer  # ...without the daily host-maintenance timer
#   scripts/bootstrap.sh --enable-web [--domain fleet.example.com]  # serve the web UI through Caddy (TLS with --domain)
#
# Explicit flags always win — each prompt is asked only when its flag was not
# given, and non-TTY runs skip every prompt (flag/default behavior unchanged).
#
# End-to-end flow (every run): install the runtime packages (no Go, no Node) →
# download + verify the signed fleet release (docs/RELEASE-ARTIFACTS.md) →
# ensure 0600 env file → ensure the client bundle is in place (--client-config)
# → the sandbox image (the release's signed image, or a custom bundle
# Containerfile built on top of it) → provision the role + database (local) or
# validate the DSN (external) → write DATABASE_URL + FLEET_CLIENT_CONFIG_DIR
# into the env file → optionally enable +
# start the systemd unit → install + enable the daily database-backup and
# host-maintenance timers (--no-backup-timer / --no-maintenance-timer opt out).
#
# Branch A (local):  install + init a local cluster, create the owner role and
#                    the database idempotently via psql \gexec, sslmode=disable.
# Branch B (external): skip install; validate DATABASE_URL with SELECT 1 and
#                    assume the role/db is pre-provisioned (opt-in superuser
#                    create via FLEET_DB_SUPERUSER_URL), sslmode=require.
#
# Flags:
#   --postgres=local|external  provisioning mode (default local).
#   --client-config <git-url[#<sha-or-tag>]|path>
#                              a git URL (cloned to a stable location) or an
#                              existing path (pointed at directly). Sets
#                              FLEET_CLIENT_CONFIG_DIR in the env file. An
#                              optional #<sha-or-tag> pins the checkout to that
#                              ref (recorded under the state dir) so `update`
#                              advances only to it instead of tracking HEAD.
#   --enable-service           systemctl enable --now the fleet unit at the end.
#   --enable-web               set up the web UI fleet serves on 127.0.0.1:3000
#                              (implies --enable-service). Email+password
#                              login against the bundle's chat users.
#   --domain <fqdn>            with --enable-web: front it with Caddy + automatic
#                              TLS for <fqdn> (installs Caddy, opens 80/443). The
#                              Caddyfile routes the browser to the web tier and
#                              the public API (/v1/*, /api-info, the A2A agent
#                              card, /triggers/*, /webhooks/*) to the Go backends
#                              (scripts/lib/caddyfile.sh; see deploy/Caddyfile),
#                              and FLEET_TRUSTED_PROXIES=127.0.0.1,::1 is set in
#                              the env file when absent (and both backends are
#                              on loopback) so the API backends read the client
#                              IP Caddy forwards.
#   --admin <email[,email...]> register these emails as full admins (web login +
#                              chat-admin + Background tasks) at the end of an
#                              --enable-service run; passwords are prompted per
#                              email (hidden). Interactive runs are prompted for
#                              the list too, so the flag is optional.
#   --auth-pubkey <val|@file>  enable Elcano SSO in the web tier: the
#                              AUTH_SIGNING_PUBKEY value (or @file containing it;
#                              both accept the `auth pubkey` output line
#                              verbatim). Validated as a 32-byte Ed25519 key.
#                              Interactive --enable-web runs are prompted.
#   --db-name <name>           the database (default fleet). Use a fresh name
#                              when something else already owns the default.
#   --db-user <name>           its owner role (default fleet).
#   --adopt-existing-db        local mode: do NOT create/alter the role or
#                              database (no password rotation, ever). Use the
#                              DSN the operator supplies in DATABASE_URL (env or
#                              already in the env file) verbatim; it is
#                              validated with SELECT 1.
#   --no-maintenance-timer     do NOT install/enable the daily host-maintenance
#                              timer (deploy/fleet-maintenance.service + .timer),
#                              which prunes dangling podman image layers and the
#                              Go build caches. The fleet process's own hourly
#                              in-process sweep runs regardless.
#   --no-backup-timer          do NOT install/enable the daily database-backup
#                              timer (deploy/fleet-backup.service + .timer) on an
#                              --enable-service run. It is installed by default
#                              because a deployment with no backups at all is the
#                              worse default; opt out when you back up at the
#                              volume/hypervisor layer or ship dumps offsite with
#                              your own tooling.
#   --force-caddy              with --enable-web --domain: allow overwriting an
#                              /etc/caddy/Caddyfile this script did not write.
#                              A timestamped backup is kept and a merge warning
#                              printed. Without it the script refuses rather
#                              than truncating an existing Caddy config — except
#                              the caddy package's untouched stock file, which
#                              is replaced with a copy kept beside it (#1657).
#   --tag <vYYYY.MM.DD.N>      install this signed release (default: the newest
#                              PUBLISHED one). The checkout is moved to that
#                              tag first, and bootstrap re-runs from it, so
#                              units and scripts match the binary. Verified
#                              before use:
#                              signature by fleet's release workflow, checksums,
#                              tag, architecture, no host executor.
#                              A published release is REQUIRED: bootstrap never
#                              builds fleet on the box (--from-source and
#                              FLEET_BOOTSTRAP_FROM_SOURCE were removed,
#                              ADR-0101), so with no release reachable it stops.
#   --dry-run                  print the plan; touch nothing.
#
# With --enable-web --domain <fqdn>, bootstrap also names the box <fqdn> when
# its hostname is an unset cloud default (vultr.guest, localhost,
# localhost.localdomain): hostnamectl, cloud-init preserve_hostname, and the
# /etc/hosts loopback lines. Any other hostname is left alone with a hint.
# FLEET_SET_HOSTNAME=1 forces the rename; FLEET_SET_HOSTNAME=0 skips it.
#
# When cockpit-ws is installed, bootstrap removes Cockpit (dnf -y remove
# 'cockpit*'); FLEET_KEEP_COCKPIT=1 keeps it.
#
# SSH is made key-only: /etc/ssh/sshd_config.d/01-no-password-login.conf
# (PasswordAuthentication no, PermitRootLogin prohibit-password), checked with
# sshd -t, then sshd is reloaded. Skipped with a warning when no authorized key
# is installed (it would lock everyone out); FLEET_KEEP_SSH_PASSWORDS=1 skips it.
#
# Env knobs (all optional; sensible local defaults):
#   FLEET_ENV_FILE          credential env file to write/refresh (default: /etc/fleet/fleet.env
#                           under --enable-service — matches deploy/fleet.service —
#                           else .env.local for local/dev runs)
#   FLEET_CLIENT_CONFIG_DIR client config bundle dir (default ./config/default —
#                           the generic bundle baked into the repo). Point at a
#                           checked-out client repo (e.g. /opt/fleet/client) for a
#                           branded deploy with its own MCP catalog + prompts.
#                           --client-config is the operator-friendly way to set it.
#   FLEET_CLIENT_CONFIG_CHECKOUT  stable dir a cloned client repo lands in when
#                           --client-config is a git URL (default /opt/fleet/client,
#                           or ./.fleet-client when /opt is not writable).
#   FLEET_SERVICE_NAME      systemd unit to enable/start (default fleet)
#   DATABASE_URL            the DSN (external mode; also the adopted DSN under
#                           --adopt-existing-db)
#   FLEET_DB_SUPERUSER_URL  external superuser DSN for opt-in role/db creation
#   FLEET_BACKUP_DIR        absolute directory the backup timer writes dumps to
#                           (default /var/backups/fleet; created 0700 root-owned —
#                           a dump holds every conversation, task and user row)
#   FLEET_BACKUP_RETENTION_DAYS  the timer's --prune cutoff: dumps older than this
#                           many days are deleted after a successful run (default 30)
#   FLEET_ACME_EMAIL        Let's Encrypt account email for Caddy (--domain; optional)
# Tracing off before anything is read: `bash -x` would print the DB passwords and other secrets.
{ set +x; } 2>/dev/null
set -euo pipefail

# Resolve this script's repo root so --enable-service can build + install the
# binary and unit files regardless of the caller's cwd (`fleet bootstrap` invokes it
# from elsewhere). The DB/env/bundle steps still use repo-relative defaults.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# Signed-release download + verification: bootstrap has no fleet yet, so the
# first binary is verified here (same pins as `fleet release`, ADR-0100).
# shellcheck source=lib/release.sh
. "$SCRIPT_DIR/lib/release.sh"
# The fleet-managed Caddyfile: marker, renderer, foreign-file detection. ONE
# implementation shared with update.sh + doctor.sh so the TLS front's routing
# cannot drift between the script that writes it and the ones that check it.
# shellcheck source=lib/caddyfile.sh
. "$SCRIPT_DIR/lib/caddyfile.sh"
# shellcheck source=lib/bundle.sh
. "$SCRIPT_DIR/lib/bundle.sh"
# shellcheck source=lib/sandbox-bundle.sh
. "$SCRIPT_DIR/lib/sandbox-bundle.sh"
# shellcheck source=lib/retired-bundle.sh
. "$SCRIPT_DIR/lib/retired-bundle.sh"
# Uninstall Cockpit (bootstrap only).
# shellcheck source=lib/cockpit.sh
. "$SCRIPT_DIR/lib/cockpit.sh"
# Key-only SSH safety net (bootstrap only; update never touches sshd).
# shellcheck source=lib/ssh.sh
. "$SCRIPT_DIR/lib/ssh.sh"
# Name the box after its --domain (bootstrap only; update never renames).
# shellcheck source=lib/hostname.sh
. "$SCRIPT_DIR/lib/hostname.sh"
# The local Postgres roles + databases, never printing their passwords.
# shellcheck source=lib/pgprovision.sh
. "$SCRIPT_DIR/lib/pgprovision.sh"
# Retiring the Next.js web tier on a box provisioned before ADR-0091.
# shellcheck source=lib/web-tier.sh
. "$SCRIPT_DIR/lib/web-tier.sh"
# fleet_pgx_unescape: a DSN component decoded as pgx decodes it (env_dsn_password).
# shellcheck source=lib/idle.sh
. "$SCRIPT_DIR/lib/idle.sh"

POSTGRES_MODE="local"
DRY_RUN=0
CLIENT_CONFIG_ARG=""
ENABLE_SERVICE=0
ENABLE_WEB=0
WEB_DOMAIN=""
# --admin: comma-separated emails registered as full admins (both planes) at the
# end of an --enable-service run; interactive runs are also PROMPTED when unset.
ADMIN_EMAILS_ARG=""
# --auth-pubkey: the Elcano SSO verification key (AUTH_SIGNING_PUBKEY) for the
# web tier — a value or @file, both accepting the `auth pubkey` output line
# verbatim. Interactive --enable-web runs are prompted when unset.
AUTH_PUBKEY_ARG=""
# The daily database-backup timer is installed by default on the --enable-service
# path (a deployment silently carrying no backups is the worse default);
# --no-backup-timer is the opt-out for boxes backed up at the volume/hypervisor
# layer or by the operator's own tooling.
ENABLE_BACKUP_TIMER=1
# --no-maintenance-timer is the opt-out for boxes that prune their container
# store some other way. Default ON: a box nobody told about maintenance is
# exactly the box that fills its disk with stale sandbox image layers.
ENABLE_MAINTENANCE_TIMER=1
FORCE_CADDY=0
# DB safety knobs (see the header): --db-name/--db-user pick fresh names when
# something else already owns the defaults; adoption skips provisioning (no
# CREATE, no ALTER — a pre-existing role's password is NEVER rotated) and
# takes the operator's DSN verbatim.
DB_NAME_ARG=""
DB_USER_ARG=""
ADOPT_DB=0
RELEASE_TAG="${FLEET_RELEASE_TAG:-}"
# The on-box build is gone (ADR-0101): refuse a request for it by name rather
# than quietly installing a release the caller did not ask for.
FROM_SOURCE_RETIRED="bootstrap.sh no longer builds fleet on the box: --from-source / FLEET_BOOTSTRAP_FROM_SOURCE=1 were removed (ADR-0101). Bootstrap installs a published, signed release (the newest, or --tag vYYYY.MM.DD.N). To run unreleased code, build it on a development machine with \`make build\`."
case "${FLEET_BOOTSTRAP_FROM_SOURCE:-0}" in
  0|"") ;;
  *) echo "error: ${FROM_SOURCE_RETIRED}" >&2; exit 1 ;;
esac
# The argv as given, for the re-exec after the checkout moves to the release.
ORIG_ARGS=("$@")

while [[ $# -gt 0 ]]; do
  case "$1" in
    --postgres=local)    POSTGRES_MODE="local" ;;
    --postgres=external) POSTGRES_MODE="external" ;;
    --postgres=*)        echo "error: --postgres must be local|external" >&2; exit 1 ;;
    --client-config)     shift; [[ $# -gt 0 ]] || { echo "error: --client-config needs a git-url|path" >&2; exit 1; }; CLIENT_CONFIG_ARG="$1" ;;
    --client-config=*)   CLIENT_CONFIG_ARG="${1#*=}" ;;
    --enable-service)    ENABLE_SERVICE=1 ;;
    --enable-web)        ENABLE_WEB=1; ENABLE_SERVICE=1 ;;  # web proxies to the backend → enable it too
    --domain)            shift; [[ $# -gt 0 ]] || { echo "error: --domain needs an FQDN" >&2; exit 1; }; WEB_DOMAIN="$1" ;;
    --domain=*)          WEB_DOMAIN="${1#*=}" ;;
    --admin)             shift; [[ $# -gt 0 ]] || { echo "error: --admin needs email[,email...]" >&2; exit 1; }; ADMIN_EMAILS_ARG="$1" ;;
    --admin=*)           ADMIN_EMAILS_ARG="${1#*=}" ;;
    --auth-pubkey)       shift; [[ $# -gt 0 ]] || { echo "error: --auth-pubkey needs a value or @file" >&2; exit 1; }; AUTH_PUBKEY_ARG="$1" ;;
    --auth-pubkey=*)     AUTH_PUBKEY_ARG="${1#*=}" ;;
    --db-name)           shift; [[ $# -gt 0 ]] || { echo "error: --db-name needs a name" >&2; exit 1; }; DB_NAME_ARG="$1" ;;
    --db-name=*)         DB_NAME_ARG="${1#*=}" ;;
    --db-user)           shift; [[ $# -gt 0 ]] || { echo "error: --db-user needs a name" >&2; exit 1; }; DB_USER_ARG="$1" ;;
    --db-user=*)         DB_USER_ARG="${1#*=}" ;;
    --adopt-existing-db) ADOPT_DB=1 ;;
    --no-backup-timer)   ENABLE_BACKUP_TIMER=0 ;;
    --no-maintenance-timer) ENABLE_MAINTENANCE_TIMER=0 ;;
    --force-caddy)       FORCE_CADDY=1 ;;
    --tag)               shift; [[ $# -gt 0 ]] || { echo "error: --tag needs a release tag" >&2; exit 1; }; RELEASE_TAG="$1" ;;
    --tag=*)             RELEASE_TAG="${1#*=}" ;;
    --from-source)       echo "error: ${FROM_SOURCE_RETIRED}" >&2; exit 1 ;;
    --dry-run)           DRY_RUN=1 ;;
    -h|--help)
      # Derived, not a hardcoded range: print the header comment block and stop
      # at the first non-`#` line. A fixed range silently truncates the help the
      # moment the header grows — which had already dropped the last 6 lines
      # here, and shipped raw shell code out of the (since removed)
      # fleet-upgrade.sh's --help.
      awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m'; c_green=$'\033[0;32m'; c_yellow=$'\033[0;33m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
else
  c_reset=''; c_green=''; c_yellow=''; c_bold=''; c_dim=''
fi
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
die()  { printf '✗ %s\n' "$*" >&2; exit 1; }
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }

# env_get KEY [FILE] — read one key from an env file without sourcing it (the
# file holds secrets; sourcing would execute arbitrary content on a tampered
# box). Last assignment wins, surrounding quotes stripped: /etc/fleet/fleet.env
# is a systemd EnvironmentFile=, where FLEET_BACKUP_DIR="/mnt/x" is legal and
# the unit sees the unquoted value — a re-run reading the quotes back verbatim
# would die on its own absolute-path validation below. Same helper as
# doctor.sh's env_get; keep the two in sync.
env_get() {
  local key="$1" file="${2:-$ENV_FILE}"
  [[ -r "$file" ]] || return 0
  grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true
}

# ── interactive deployment selection (TTY only; flags always win) ────────────
# A bare `scripts/bootstrap.sh` on a terminal walks the operator through the
# three deployment choices instead of requiring flag archaeology: systemd
# service? web UI? public domain? Explicit flags skip their prompt; non-TTY
# runs (CI, pipes) skip all of them and keep the flag/default behavior, so
# scripted invocations are unchanged. Deliberately ALSO prompts under --dry-run:
# the answers change which plan is printed, which is exactly what a dry-run
# preview is for (and nothing is executed either way). This must run BEFORE the
# ENV_FILE resolution below, which branches on ENABLE_SERVICE.
if [[ -t 0 ]]; then
  if [[ "$ENABLE_SERVICE" != "1" ]] && command -v systemctl >/dev/null 2>&1; then
    printf 'Run fleet as a systemd service (build + install + enable now)? [Y/n] '
    read -r _ans
    case "${_ans,,}" in n|no) info "dev mode — env goes to .env.local; start with: fleet serve" ;; *) ENABLE_SERVICE=1 ;; esac
  fi
  if [[ "$ENABLE_SERVICE" == "1" && "$ENABLE_WEB" != "1" ]]; then
    printf 'Set up the web UI (chat + Background tasks)? [Y/n] '
    read -r _ans
    case "${_ans,,}" in n|no) ;; *) ENABLE_WEB=1 ;; esac
  fi
  if [[ "$ENABLE_WEB" == "1" && -z "$WEB_DOMAIN" ]]; then
    printf 'Public domain for automatic TLS (e.g. fleet.example.com; blank = loopback-only :3000): '
    read -r WEB_DOMAIN
    WEB_DOMAIN="$(printf '%s' "$WEB_DOMAIN" | tr -d '[:space:]')"
  fi
  unset _ans
fi

# Env file default: an explicit FLEET_ENV_FILE always wins. Otherwise, under
# --enable-service (the systemd path) default to /etc/fleet/fleet.env — the path
# deploy/fleet.service EnvironmentFiles — so the documented one-command deploy
# writes credentials where the unit actually reads them (not a stray ./.env.local
# the service can't see under ProtectHome). Plain local/dev runs keep .env.local.
if [[ -n "${FLEET_ENV_FILE:-}" ]]; then
  ENV_FILE="$FLEET_ENV_FILE"
elif [[ "$ENABLE_SERVICE" == "1" ]]; then
  ENV_FILE="/etc/fleet/fleet.env"
else
  ENV_FILE=".env.local"
fi
# Absolute from the start: the systemd unit runs with WorkingDirectory=
# /var/lib/fleet, so a RELATIVE dir written into the env file resolves there and
# the service dies at boot ("client config bundle ... no such file or
# directory"). The in-repo default anchors to this checkout; a relative env
# value resolves against the caller's CWD when it exists, else the repo root.
CLIENT_CONFIG_DIR="${FLEET_CLIENT_CONFIG_DIR:-$REPO_ROOT/config/default}"
if [[ "$CLIENT_CONFIG_DIR" != /* ]]; then
  CLIENT_CONFIG_DIR="$(cd "$CLIENT_CONFIG_DIR" 2>/dev/null && pwd || printf '%s/%s' "$REPO_ROOT" "$CLIENT_CONFIG_DIR")"
fi
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
# Scheduled-backup settings, written into the env file so `fleet backup` (and the
# timer's ExecStart, which passes no --out) resolve the same directory.
# Precedence is process env > the value already in the env file > default, the
# same way the adopted DSNs resolve further down: a re-run must not reset a
# backup directory or retention the operator moved (dumps would silently start
# landing somewhere else).
BACKUP_DIR="${FLEET_BACKUP_DIR:-$(env_get FLEET_BACKUP_DIR "$ENV_FILE")}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/fleet}"
BACKUP_RETENTION_DAYS="${FLEET_BACKUP_RETENTION_DAYS:-$(env_get FLEET_BACKUP_RETENTION_DAYS "$ENV_FILE")}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-30}"
# Validated only on the runs that WRITE these keys and install the unit that
# reads them. A dev run installs no timer, so a relative FLEET_BACKUP_DIR
# exported for a local `fleet backup` is that caller's business, not a reason to
# refuse. The path must be ABSOLUTE once it lands in the env file: a relative
# value there resolves against the unit's working directory ("/" for the
# oneshot), which is not where dumps belong.
if [[ "$ENABLE_SERVICE" == "1" && "$ENABLE_BACKUP_TIMER" == "1" ]]; then
  [[ "$BACKUP_DIR" == /* ]] || die "FLEET_BACKUP_DIR must be an absolute path (got '${BACKUP_DIR}')"
  [[ "$BACKUP_RETENTION_DAYS" =~ ^[1-9][0-9]*$ ]] \
    || die "FLEET_BACKUP_RETENTION_DAYS must be a positive integer (got '${BACKUP_RETENTION_DAYS}')"
fi
# The database this run provisions: ONE database (ADR-0098), configured by
# DATABASE_URL alone. An env file (or, in external mode, an environment) that
# still sets a retired per-store key is refused — the two-database layout was
# removed (#2049), and fleet would refuse to boot with it. The env file is
# read with fleet's own parser (fleet_env_value: what config.ResolveDSNs
# sees), so a key that is present but empty — FLEET_CHAT_DATABASE_URL="" —
# does not count, exactly as in Go.
_env_or_file() { # KEY — the process env in external mode (its inputs), else the env file
  if [[ "$POSTGRES_MODE" == "external" && -n "${!1:-}" ]]; then printf '%s' "${!1}"; else fleet_env_value "$ENV_FILE" "$1"; fi
}
for _k in FLEET_CHAT_DATABASE_URL FLEET_SCHED_DATABASE_URL SCHED_DATABASE_URL; do
  [[ -z "$(_env_or_file "$_k")" ]] \
    || die "${_k} is no longer supported: fleet keeps both stores in ONE database, configured by DATABASE_URL alone (the two-database layout was removed, ADR-0098) — set DATABASE_URL to that database and delete ${_k} from ${ENV_FILE} (docs/ONE-DATABASE.md)"
done
unset _k
FLEET_DB_NAME="${DB_NAME_ARG:-fleet}"
FLEET_DB_USER="${DB_USER_ARG:-fleet}"
# A re-run converges on the database a previous run recorded: the role and
# database come from the env file's DATABASE_URL, never reset to the
# defaults, and that DSN is kept as written. A --db-name/--db-user that
# disagrees with it is refused rather than silently switching databases.
RECORDED_DB_URL=""
if [[ "$POSTGRES_MODE" == "local" && "$ADOPT_DB" != "1" ]]; then
  RECORDED_DB_URL="$(fleet_env_value "$ENV_FILE" DATABASE_URL)"
fi
if [[ -n "$RECORDED_DB_URL" ]]; then
  _rd="$(fleet_dsn_role_db "$RECORDED_DB_URL")" \
    || die "cannot tell the role and database from the DATABASE_URL in ${ENV_FILE} (want postgres://<role>:<password>@127.0.0.1:5432/<database> with lower-case names); fix it there, or use --adopt-existing-db to keep it as it is"
  fleet_dsn_is_local_default "$RECORDED_DB_URL" \
    || die "the DATABASE_URL in ${ENV_FILE} does not name the local cluster on port 5432, which --postgres=local provisions; use --postgres=external (or --adopt-existing-db) for it"
  [[ -z "$DB_USER_ARG" || "$DB_USER_ARG" == "${_rd%% *}" ]] \
    || die "--db-user ${DB_USER_ARG} conflicts with role '${_rd%% *}' in the DATABASE_URL recorded in ${ENV_FILE}; bootstrap does not switch an installed box to another role — drop the flag, or change DATABASE_URL yourself"
  [[ -z "$DB_NAME_ARG" || "$DB_NAME_ARG" == "${_rd#* }" ]] \
    || die "--db-name ${DB_NAME_ARG} conflicts with database '${_rd#* }' in the DATABASE_URL recorded in ${ENV_FILE}; bootstrap does not switch an installed box to another database — drop the flag, or change DATABASE_URL yourself"
  FLEET_DB_USER="${_rd%% *}"
  FLEET_DB_NAME="${_rd#* }"
  unset _rd
fi
# Lower-case only: the names go unquoted into the provisioning SQL, where
# Postgres folds them to lower case — "Fleet" would create role/db "fleet"
# while the DSN written below says "Fleet", which then fails to connect.
for _ident in "$FLEET_DB_NAME" "$FLEET_DB_USER"; do
  [[ "$_ident" =~ ^[a-z_][a-z0-9_]*$ ]] \
    || die "database/role name '${_ident}' must be a lower-case plain identifier ([a-z_][a-z0-9_]*)"
done
if [[ "$POSTGRES_MODE" == "external" && "$ADOPT_DB" == "1" ]]; then
  die "--adopt-existing-db applies to --postgres=local; external mode always uses your DSN and never touches roles"
fi
ADOPTED_DB_URL=""
if [[ "$ADOPT_DB" == "1" ]]; then
  ADOPTED_DB_URL="${DATABASE_URL:-$(fleet_env_value "$ENV_FILE" DATABASE_URL)}"
  [[ -n "$ADOPTED_DB_URL" ]] \
    || die "--adopt-existing-db needs the existing database's working DSN: set DATABASE_URL (env, or already present in ${ENV_FILE}). This script never creates or alters an adopted role — supply its CURRENT password."
fi

gen_pass() { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# require_schema_privilege DSN WHAT [HINT] — die unless DSN's role can run
# fleet's stores there: each store creates its schema (chat, sched) on first
# start, so a role that passes SELECT 1 but lacks CREATE on the database would
# be reported ready and then fail at boot (lib/pgprovision.sh
# fleet_db_store_readiness): it refuses a role that cannot create a missing
# store schema, does not own an existing one (or what is in it), or a database
# with a legacy store in public. Every path that accepts a DSN it did not
# provision itself — external mode, --adopt-existing-db — calls it.
require_schema_privilege() {
  local rc=0 found
  found="$(fleet_db_store_readiness "$1")" || rc=$?
  case "$rc" in
    0) return 0 ;;
    4) die "${2}'s role must OWN the existing store schemas (chat, sched) and everything in them, but found: ${found} — transfer them (ALTER SCHEMA … OWNER TO <role>, ALTER TABLE … OWNER TO <role>), or drop the schemas so fleet creates them${3:+, $3}" ;;
    2) die "${2} names a database whose public schema already holds a fleet store (a database of the removed two-database layout) — fleet will not run its stores next to it. Point DATABASE_URL at the one database with the chat and sched schemas, or at a new database (docs/ONE-DATABASE.md)" ;;
    1) die "${2}'s role cannot run fleet's stores there: it needs CREATE on the database for a schema (chat, sched) that does not exist yet (GRANT CREATE ON DATABASE <db> TO <role>), and must own each that does${3:+, $3}" ;;
    *) die "${2}: could not check the role's privileges (psql failed)" ;;
  esac
}

# ── Caddyfile ownership marker ──
# The Caddyfile this script writes carries this marker as its first line. An
# existing /etc/caddy/Caddyfile WITHOUT it belongs to someone else (a legacy
# chat/moc deploy, a hand-rolled config) — overwriting it would silently
# destroy their vhosts, so we refuse unless --force-caddy, and even then keep a
# timestamped backup. The one file that is not "someone else's" is the caddy
# package's own untouched default, dropped by `dnf install caddy`: recognised
# by the digest the package recorded (caddyfile_is_package_default) and
# replaced, with a copy kept (#1657). Checked fail-fast here (before any
# provisioning work) and again at write time inside configure_web_tier.
# CADDY_MARKER + caddyfile_is_foreign come from scripts/lib/caddyfile.sh
# (sourced above); the marker text is stable so every box bootstrap ever
# provisioned keeps being recognised as fleet-managed.
if [[ "$ENABLE_WEB" == "1" && -n "$WEB_DOMAIN" && "$FORCE_CADDY" != "1" ]] && caddyfile_is_foreign; then
  if [[ "$DRY_RUN" == "1" ]]; then
    warn "/etc/caddy/Caddyfile exists and was not written by this script — a real run would REFUSE here."
    warn "merge your existing Caddy config manually, or re-run with --force-caddy (a timestamped backup is kept)."
  else
    die "refusing to overwrite /etc/caddy/Caddyfile: it exists and was not written by this script.
  It likely carries another app's vhosts (e.g. a legacy chat/moc deploy) — overwriting would take them down.
  Either merge the fleet site into it yourself (see deploy/Caddyfile), or re-run with --force-caddy
  to overwrite it (the previous file is saved as /etc/caddy/Caddyfile.fleet-backup.<timestamp>)."
  fi
fi

# upsert_env_file FILE KEY VALUE — idempotently set KEY=VALUE in FILE, replacing
# an existing KEY= line in place and preserving comments/unrelated lines (mirrors
# internal/creds.SetEnvKey). No-ops under --dry-run. The value is encoded by
# lib/envfile.sh so systemd, fleet's own parser and env_get all read back the
# exact bytes; a value no encoding can carry is refused, never written mangled.
upsert_env_file() {
  if [[ "$DRY_RUN" == "1" ]]; then
    # Never echo secret values in the plan — show the key only.
    info "[dry-run] would set ${2}=… in ${1}"
    return 0
  fi
  fleet_upsert_env_file "$1" "$2" "$3" && return 0
  warn "could not write ${2} to ${1}: the value has a line break, or a ' together with a \" or \\, which no env-file quoting carries — set it by hand"
  return 1
}

# upsert_env KEY VALUE — upsert into the main credential env file ($ENV_FILE).
upsert_env() { upsert_env_file "$ENV_FILE" "$1" "$2"; }

# ── dedicated service-user + rootless-Podman helpers (systemd/--enable-service) ──
# deploy/fleet.service runs as a FIXED system user (User=fleet), NOT DynamicUser:
# the execution sandbox is rootless Podman, which a transient DynamicUser cannot
# drive (no /etc/subuid range, no HOME). These helpers provision that user and its
# rootless-Podman prerequisites so the unit's sandbox actually runs.
SERVICE_USER="fleet"          # MUST match deploy/fleet.service (User=/Group=fleet)
SERVICE_HOME="/var/lib/fleet" # MUST match the unit's StateDirectory / HOME

setup_service_user() {
  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$SERVICE_HOME" --shell /usr/sbin/nologin --no-create-home "$SERVICE_USER"
    ok "created service user ${SERVICE_USER}"
  else
    info "service user ${SERVICE_USER} present"
  fi
  # subuid/subgid ranges — rootless Podman maps container uids/gids into these.
  grep -q "^${SERVICE_USER}:" /etc/subuid || echo "${SERVICE_USER}:100000:65536" >> /etc/subuid
  grep -q "^${SERVICE_USER}:" /etc/subgid || echo "${SERVICE_USER}:100000:65536" >> /etc/subgid
  # HOME (rootless image store lives at ~/.local/share/containers) + runtime dir.
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "$SERVICE_HOME"
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "/run/${SERVICE_USER}"
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "${SERVICE_HOME}/.config/containers"
  # cgroupfs avoids needing a systemd user D-Bus session (absent for a system
  # service / non-login user, which otherwise fails with "sd-bus call: Access
  # denied ... interactive authentication"); file events backend avoids journald perms.
  cat > "${SERVICE_HOME}/.config/containers/containers.conf" <<'CONF'
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
CONF
  # chown the WHOLE .config tree: `install -d` leaves the intermediate ~/.config
  # root-owned, and rootless Podman refuses to start if $HOME/.config is not owned
  # by the calling user ("path .../.config exists and it is not owned by the
  # current user"), which fails the build/run below.
  chown -R "$SERVICE_USER":"$SERVICE_USER" "${SERVICE_HOME}/.config"
  ok "rootless Podman configured for ${SERVICE_USER} (subuid/subgid + HOME + cgroupfs)"
}

# sandbox_manifest_tag MANIFEST — read the bundle's sandbox.tag (the on-box image
# name); mirrors build-sandbox-image.sh's default. Used to build into the service
# user's rootless store.
sandbox_manifest_tag() {
  local f="$1" t
  t="$(awk '
    /^sandbox:[[:space:]]*$/ { b=1; next }
    /^[^[:space:]]/          { b=0 }
    b && /^[[:space:]]+tag:/ { sub("^[[:space:]]+tag:[[:space:]]*",""); sub(/[[:space:]]+#.*$/,""); gsub(/^["'\'']|["'\'']$/,""); print; exit }
  ' "$f" 2>/dev/null)"
  printf '%s' "${t:-localhost/fleet-sandbox:latest}"
}

# ── web-tier helpers (--enable-web) ──────────────────────────────────────────
gen_secret() { head -c "${1:-32}" /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "${1:-32}"; }

# ensure_env_secret KEY LEN — set KEY=<random> in $ENV_FILE only when ABSENT, so
# re-runs never rotate a shared secret (rotating FLEET_SERVER_TOKEN would 403 the
# web↔backend link; rotating APP_SESSION_SECRET would log everyone out).
ensure_env_secret() {
  local key="$1" len="${2:-32}"
  [[ -f "$ENV_FILE" ]] && grep -q "^${key}=" "$ENV_FILE" && return 0
  upsert_env "$key" "$(gen_secret "$len")"
}

# ensure_env_b64_key KEY BYTES — generate a standard-base64 binary key once.
# Unlike gen_secret (an alphanumeric application token), encryption settings
# are decoded by fleet and therefore need an exact decoded byte length.
ensure_env_b64_key() {
  local key="$1" bytes="${2:-32}"
  [[ -f "$ENV_FILE" ]] && grep -q "^${key}=" "$ENV_FILE" && return 0
  upsert_env "$key" "$(head -c "$bytes" /dev/urandom | base64 | tr -d '\n')"
}

# normalize_auth_pubkey VALUE — echo the bare base64 AUTH_SIGNING_PUBKEY, or fail.
# Accepts the `auth pubkey` output line (AUTH_SIGNING_PUBKEY=<b64>) or the bare
# key, strips quotes/whitespace, and requires the decode to be exactly 32 bytes
# (an Ed25519 public key) — mirroring `fleet config set-auth-pubkey`, so a bad
# paste fails HERE, not as silent SSO login failures later.
normalize_auth_pubkey() {
  local v="$1" n
  v="${v#AUTH_SIGNING_PUBKEY=}"
  v="$(printf '%s' "$v" | tr -d '[:space:]')"
  v="${v%\"}"; v="${v#\"}"; v="${v%\'}"; v="${v#\'}"
  [[ -n "$v" ]] || return 1
  n="$(printf '%s' "$v" | base64 -d 2>/dev/null | wc -c)" || n=0
  [[ "$n" == "32" ]] || return 1
  printf '%s' "$v"
}

# cutover_web_tier_or_die — fleet_cutover_web_tier (scripts/lib/web-tier.sh)
# for this box, ending the run on failure. By then the old Next.js tier is
# restored (when it was running) and its files are intact; carrying on would
# retire it again and report a success that leaves the browser UI down.
cutover_web_tier_or_die() {
  fleet_cutover_web_tier "$SERVICE_NAME" "$REPO_ROOT" && return 0
  local kept="left installed"
  systemctl is-active --quiet fleet-web.service 2>/dev/null && kept="restarted, so the browser UI keeps working"
  die "${SERVICE_NAME} did not come up on the new binary — the old web tier (fleet-web) was ${kept}; nothing was removed.
  Find the cause: journalctl -u ${SERVICE_NAME} -n 50 — then re-run this bootstrap, which retires the old tier once ${SERVICE_NAME} starts."
}

# configure_web_tier — the browser UI's settings in fleet.env, and (with
# --domain) a Caddy TLS front. The fleet binary serves the UI itself on
# FLEET_WEB_ADDR (default 127.0.0.1:3000, ADR-0091); this only supplies what
# sign-in needs: APP_SESSION_SECRET (generate-if-absent — rotating it logs
# everyone out), the public origin, and optionally the Elcano SSO key
# (AUTH_SIGNING_PUBKEY: --auth-pubkey, an interactive paste, or already set).
# Email+password login works against the bundle's chat users without SSO.
configure_web_tier() {
  local origin old_env="${FLEET_WEB_ENV_FILE:-/etc/fleet/fleet-web.env}"
  [[ -n "$WEB_DOMAIN" ]] && origin="https://$WEB_DOMAIN" || origin="http://localhost:3000"

  step "Web tier: ensuring sign-in settings in ${ENV_FILE}"
  # A box provisioned before ADR-0091 kept the browser keys in the retired
  # tier's own env file; carry the ones fleet.env lacks (the same
  # APP_SESSION_SECRET keeps existing sessions valid) before generating any.
  # The same key list update.sh uses (scripts/lib/web-tier.sh), so the two
  # paths cannot drift: session secret, SSO/cookie keys, OIDC, catwalk, plus
  # the origin and a non-default bind under their new names.
  fleet_migrate_web_env "$old_env" "$ENV_FILE"
  ensure_env_secret FLEET_SERVER_TOKEN 32
  ensure_env_secret ADMIN_API_KEY 32
  ensure_env_secret APP_SESSION_SECRET 48
  # The canonical public origin: redirect targets, the Secure-cookie decision
  # and the remote-MCP OAuth callbacks all derive from it. Read at runtime, so
  # changing it later is an env edit + restart, no rebuild.
  upsert_env FLEET_PUBLIC_BASE_URL "$origin"
  ensure_env_b64_key FLEET_MCP_OAUTH_ENCRYPTION_KEY 32

  # Elcano SSO (AUTH_SIGNING_PUBKEY): --auth-pubkey wins (value or @file); else
  # an existing key is LEFT IN PLACE; else offer an interactive paste. Every
  # path that WRITES validates via normalize_auth_pubkey — a bad key DIES
  # rather than deploying a web tier whose SSO fails on every login.
  local auth_raw="" auth_pubkey=""
  if [[ -n "$AUTH_PUBKEY_ARG" ]]; then
    auth_raw="$AUTH_PUBKEY_ARG"
    if [[ "$auth_raw" == @* ]]; then
      auth_raw="$(cat "${auth_raw#@}")" || die "--auth-pubkey: cannot read ${AUTH_PUBKEY_ARG#@}"
    fi
    auth_pubkey="$(normalize_auth_pubkey "$auth_raw")" \
      || die "--auth-pubkey is not a valid AUTH_SIGNING_PUBKEY (want the standard-base64 32-byte Ed25519 key from \`auth pubkey\`)"
    upsert_env AUTH_SIGNING_PUBKEY "$auth_pubkey"
    ok "Elcano SSO key accepted (--auth-pubkey)"
  elif grep -q '^AUTH_SIGNING_PUBKEY=' "$ENV_FILE" 2>/dev/null; then
    info "existing AUTH_SIGNING_PUBKEY left in place (Elcano SSO stays enabled)."
  elif [[ -t 0 ]]; then
    printf 'Optional Elcano SSO key — email+password login works without it (paste the `auth pubkey` line, or Enter to skip): '
    read -r auth_raw
    if [[ -n "$auth_raw" ]]; then
      auth_pubkey="$(normalize_auth_pubkey "$auth_raw")" \
        || die "that is not a valid AUTH_SIGNING_PUBKEY (want the standard-base64 32-byte Ed25519 key from \`auth pubkey\`)"
      upsert_env AUTH_SIGNING_PUBKEY "$auth_pubkey"
      ok "Elcano SSO key accepted"
    else
      info "skipped — enable SSO later with: fleet config set-auth-pubkey"
    fi
  fi
  if grep -q '^AUTH_SIGNING_PUBKEY=' "$ENV_FILE" 2>/dev/null; then
    ok "web sign-in configured in ${ENV_FILE} (origin ${origin}; Elcano SSO enabled)"
  else
    ok "web sign-in configured in ${ENV_FILE} (origin ${origin}; email+password login only)"
  fi

  # Restart fleet so it reads the settings above. On a re-run over a box
  # provisioned before ADR-0091 the retired Next.js unit may still hold fleet's
  # port: the cutover stops it, restarts fleet and removes it only once fleet
  # is up — a failure restores it and ends the run (#1838).
  if fleet_web_tier_present; then
    cutover_web_tier_or_die
  elif systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 && fleet_web_tier_wait_active "$SERVICE_NAME" \
      || die "${SERVICE_NAME} did not come back up after the web sign-in settings changed — journalctl -u ${SERVICE_NAME} -n 50"
  fi
  ok "fleet serves the web UI on 127.0.0.1:3000 (FLEET_WEB_ADDR)"

  if [[ -z "$WEB_DOMAIN" ]]; then
    # Loopback-only by default (FLEET_WEB_ADDR=127.0.0.1:3000), so this holds
    # even on boxes without firewalld. EVERY custom proxy front — same host or
    # not — needs the real public origin: it decides redirect targets and the
    # Secure-cookie flag. Only a proxy on ANOTHER host needs the wider bind.
    info "no --domain → web is loopback-only on :3000; front it with your own TLS proxy for a public URL."
    info "  any custom proxy (same host too): set FLEET_PUBLIC_BASE_URL=https://<domain> in ${ENV_FILE}, then: systemctl restart ${SERVICE_NAME}"
    info "  proxy on ANOTHER host: additionally set FLEET_WEB_ADDR=0.0.0.0:3000 in ${ENV_FILE} (fleet binds 127.0.0.1:3000 otherwise)."
    return
  fi
  step "Web tier: Caddy TLS reverse proxy for ${WEB_DOMAIN}"
  if command -v dnf >/dev/null 2>&1 && ! command -v caddy >/dev/null 2>&1; then
    dnf install -y caddy >/dev/null 2>&1 || warn "dnf install caddy failed — install Caddy manually."
  fi
  command -v caddy >/dev/null 2>&1 || { warn "caddy not found — skipping TLS front (web still on :3000)."; return; }
  install -d /etc/caddy
  # Never truncate a Caddyfile this script did not write (the fail-fast check
  # at the top already covers the common path; this write-time guard also
  # covers a file that appeared mid-run, e.g. dropped by `dnf install caddy`).
  local caddy_backup
  if caddyfile_is_foreign; then
    if [[ "$FORCE_CADDY" != "1" ]]; then
      die "refusing to overwrite /etc/caddy/Caddyfile (not written by this script) — merge manually or re-run with --force-caddy"
    fi
    caddy_backup="/etc/caddy/Caddyfile.fleet-backup.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p /etc/caddy/Caddyfile "$caddy_backup" || die "could not back up /etc/caddy/Caddyfile to ${caddy_backup}"
    warn "--force-caddy: OVERWRITING /etc/caddy/Caddyfile — previous config saved to ${caddy_backup}"
    warn "any other sites/vhosts it served are DOWN until you merge them back in and reload caddy."
  elif caddyfile_is_package_default; then
    # The caddy package's stock file (Fedora ships one; `dnf install caddy`
    # above drops it): untouched, and its imports (Fedora's Caddyfile.d
    # drop-ins) match no file, so it carries only the package's welcome
    # page. It is replaced, with a copy kept beside it (#1657).
    caddy_backup="/etc/caddy/Caddyfile.fleet-backup.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p /etc/caddy/Caddyfile "$caddy_backup" || die "could not back up /etc/caddy/Caddyfile to ${caddy_backup}"
    info "replacing the caddy package's stock Caddyfile (unmodified default, no drop-in sites) — copy kept at ${caddy_backup}"
  fi
  # One renderer (scripts/lib/caddyfile.sh) writes the whole file: the web
  # tier as the default upstream, the public API (/v1/*, /api-info, the A2A
  # agent card, /triggers/*) to the orchestrator and /webhooks/* to the chat
  # listener, with the web tier's header-trust channel stripped on both. The
  # upstream addresses follow the env file when an operator moved a listener.
  render_fleet_caddyfile "$WEB_DOMAIN" "${FLEET_ACME_EMAIL:-}" \
    "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)" \
    "$(caddy_web_upstream "$(env_get FLEET_WEB_ADDR)")" \
    > /etc/caddy/Caddyfile
  if command -v caddy >/dev/null 2>&1 && ! caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
    warn "caddy validate rejected the rendered /etc/caddy/Caddyfile — check: caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile"
  fi
  # Caddy proxies the public API (/v1/*, /api-info, /a2a, /triggers/*,
  # /webhooks/*) straight to the Go listeners, so the backends must trust it
  # for X-Forwarded-For or every API caller looks like Caddy's address (one
  # shared rate-limit bucket; the IP allow/deny filter judging Caddy). Browser
  # traffic is NOT helped by this: the fleet web listener calls the backends
  # in-process without X-Forwarded-For, so it keys on 127.0.0.1 either way. 127.0.0.1,::1 is only
  # right while both upstreams are loopback — a listener moved to an interface
  # address is dialled from that interface, so then nothing is written and the
  # operator names the address. Written only when the key is ABSENT: a value
  # the operator set — even an empty one — is theirs. The backend is restarted
  # so the running process reads it, and a failed restart is said out loud.
  if ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then
    if [[ "$(caddy_trusted_proxies_state "$ENV_FILE")" == "unset" ]]; then
      warn "Caddy proxies to a non-loopback backend (FLEET_SERVER_ADDR=$(env_get FLEET_SERVER_ADDR) FLEET_ORCHESTRATOR_ADDR=$(env_get FLEET_ORCHESTRATOR_ADDR)), so it connects from this host's interface address, not loopback — FLEET_TRUSTED_PROXIES was NOT written."
      warn "  set it to the address Caddy connects from: sudo fleet config set-env FLEET_TRUSTED_PROXIES --value <ip> && sudo fleet restart"
    else
      info "FLEET_TRUSTED_PROXIES already set in ${ENV_FILE} ($(env_get FLEET_TRUSTED_PROXIES)) — leaving it (Caddy proxies to a non-loopback backend: it must name the address Caddy connects from)"
    fi
  elif [[ "$(caddy_trusted_proxies_state "$ENV_FILE")" == "unset" ]]; then
    upsert_env FLEET_TRUSTED_PROXIES "$CADDY_TRUSTED_PROXIES"
    if systemctl try-restart "$SERVICE_NAME" >/dev/null 2>&1; then
      ok "FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES} in ${ENV_FILE} (${SERVICE_NAME} restarted if it was running) — the backends read X-Forwarded-For from Caddy (and only from Caddy)"
    else
      warn "wrote FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES} to ${ENV_FILE}, but restarting ${SERVICE_NAME} FAILED — the running process does not have it: journalctl -u ${SERVICE_NAME} -n 50, then: sudo fleet restart"
    fi
  else
    info "FLEET_TRUSTED_PROXIES already set in ${ENV_FILE} ($(env_get FLEET_TRUSTED_PROXIES)) — leaving it"
  fi
  if command -v firewall-cmd >/dev/null 2>&1; then
    firewall-cmd --add-service=http --add-service=https --permanent >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
  fi
  # A fresh `dnf install caddy` creates /var/lib/caddy (the caddy user's cert/account
  # storage). Ensure it exists when caddy was already installed but its storage dir is
  # missing — otherwise caddy fails ACME with "mkdir /var/lib/caddy: permission denied".
  if id caddy >/dev/null 2>&1 && [[ ! -d /var/lib/caddy ]]; then
    install -d -o caddy -g caddy -m 0700 /var/lib/caddy
  fi
  # `enable --now` is a no-op on an already-running caddy, so a re-run that
  # rewrote the Caddyfile (a routing fix, a new domain) must RELOAD it too —
  # otherwise the old config keeps serving and "the API isn't routing" survives
  # the very bootstrap that fixed it.
  systemctl enable caddy >/dev/null 2>&1 || true
  if systemctl is-active --quiet caddy; then
    systemctl reload caddy >/dev/null 2>&1 || systemctl restart caddy >/dev/null 2>&1 || true
  else
    systemctl start caddy >/dev/null 2>&1 || true
  fi
  if systemctl is-active caddy >/dev/null 2>&1; then
    ok "caddy serving https://${WEB_DOMAIN} (Let's Encrypt; requires inbound 80/443 reachable)"
    ok "  browser → web tier (:3000); /v1/*, /api-info, agent card, /triggers/* → orchestrator; /webhooks/* → chat (X-API-Key / signed webhooks only — header-trust stripped)"
  else
    warn "caddy not active — check: journalctl -u caddy -n 50"
  fi
}

step "fleet bootstrap (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"

# Fail fast on a bad --auth-pubkey BEFORE any provisioning work — the web-tier
# write also validates (belt-and-braces, and it re-reads an @file that may have
# changed), but a typo'd key should die here in the first second, not after a
# multi-minute release download and sandbox pull.
if [[ -n "$AUTH_PUBKEY_ARG" ]]; then
  _apk_raw="$AUTH_PUBKEY_ARG"
  if [[ "$_apk_raw" == @* ]]; then
    _apk_raw="$(cat "${_apk_raw#@}")" || die "--auth-pubkey: cannot read ${AUTH_PUBKEY_ARG#@}"
  fi
  normalize_auth_pubkey "$_apk_raw" >/dev/null \
    || die "--auth-pubkey is not a valid AUTH_SIGNING_PUBKEY (want the standard-base64 32-byte Ed25519 key from \`auth pubkey\`)"
  ok "Elcano SSO key validated"
  unset _apk_raw
fi

# ── which release, and the checkout at it ──
# Default: the newest PUBLISHED release (GitHub's "latest" names a release only
# once its assets are uploaded), never a tag merely present in this checkout —
# a fresh tag's artifacts may still be building, and an old one's may be past
# the retention window. --tag picks another. Either way the checkout moves to
# that tag, so the units, scripts and generic bundle bootstrap installs are the
# ones the binary was released with; this script then re-runs from the moved
# checkout (bash reads a script as it goes, so it must not keep running the
# old copy of itself). A box that cannot reach a published release stops here:
# there is no on-box build to fall back to (ADR-0101).
if [[ "${FLEET_BOOTSTRAP_ALIGNED:-0}" != "1" ]]; then
  [[ -z "$RELEASE_TAG" || "$RELEASE_TAG" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] \
    || die "--tag must be a release tag like v2026.11.07.3, got: ${RELEASE_TAG}"
  step "Release: ${RELEASE_TAG:-the newest published one}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would resolve ${RELEASE_TAG:-the newest published release (GitHub API)}, move ${REPO_ROOT} to that tag and re-run bootstrap from it"
  else
    if [[ -z "$RELEASE_TAG" ]]; then
      RELEASE_TAG="$(fleet_release_latest_tag)" \
        || die "could not ask GitHub which release is newest (offline? rate-limited? none published yet?) — bootstrap installs only a published, signed release and cannot build fleet on the box (ADR-0101); retry once GitHub is reachable, or name one: --tag vYYYY.MM.DD.N. Nothing was installed"
    fi
    git -C "$REPO_ROOT" fetch --quiet --tags origin 2>/dev/null \
      || warn "could not fetch tags into ${REPO_ROOT}; using the tags it has"
    _tag_sha="$(git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/${RELEASE_TAG}^{commit}" 2>/dev/null)" \
      || die "release ${RELEASE_TAG} is not a tag in ${REPO_ROOT} — is it a clone of ElcanoTek/fleet?"
    # The SIGNED manifest first: nothing from the tag is checked out, let
    # alone executed, until SHA256SUMS carries the release identity's
    # signature and release.json — matching it — names this tag and the
    # commit it resolves to here. Verified with the pinned cosign of the
    # checkout the operator cloned and is running now.
    fleet_release_verify_manifest "$REPO_ROOT" "$RELEASE_TAG" "$_tag_sha" "${FLEET_RELEASE_CACHE_DIR:-/var/cache/fleet}" \
      || die "release ${RELEASE_TAG} could not be verified (above) — the checkout was NOT moved and nothing was installed"
    if [[ "$(git -C "$REPO_ROOT" rev-parse HEAD)" != "$_tag_sha" ]]; then
      git -C "$REPO_ROOT" diff --quiet HEAD -- \
        || die "${REPO_ROOT} has uncommitted changes — commit or discard them, then re-run (bootstrap moves the checkout to ${RELEASE_TAG})"
      _branch="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
      if [[ "$_branch" == "HEAD" ]]; then
        git -C "$REPO_ROOT" checkout --quiet --detach "$_tag_sha" || die "cannot check out ${RELEASE_TAG}"
      else
        git -C "$REPO_ROOT" merge-base --is-ancestor HEAD "origin/${_branch}" 2>/dev/null \
          || die "${REPO_ROOT} has commits that are not on origin/${_branch}; moving it to ${RELEASE_TAG} would strand them — push or reset them first (bootstrap installs releases only; run unreleased code on a development machine)"
        git -C "$REPO_ROOT" checkout --quiet -B "$_branch" "$_tag_sha" || die "cannot move ${_branch} to ${RELEASE_TAG}"
      fi
      ok "checkout moved to ${RELEASE_TAG} (${_tag_sha:0:12}) — re-running bootstrap from it"
      exec env FLEET_BOOTSTRAP_ALIGNED=1 FLEET_RELEASE_TAG="$RELEASE_TAG" bash "$REPO_ROOT/scripts/bootstrap.sh" "${ORIG_ARGS[@]}"
    fi
    ok "checkout ${REPO_ROOT} is at ${RELEASE_TAG}"
  fi
fi

# ── system dependencies: the runtime + sandbox toolchain ──
# So `git clone … && bash scripts/bootstrap.sh …` provisions a BARE box end to
# end: podman (the execution sandbox), python3 + pip (host-side Python MCP
# servers), chromium-headless (built-in browser automation), plus
# git/curl/jq/rsync/gcc. No Go and no Node: the fleet binary is a signed release
# artifact with the web UI embedded (docs/RELEASE-ARTIFACTS.md, ADR-0100).
# Postgres-server is installed per-mode below (local only). Non-Fedora hosts:
# install these yourself, then re-run.
#
# slirp4netns is here because FLEET_DEFAULT_NETWORK_MODE=allowlisted requires
# it: podman >= 5.0 defaults to pasta and a stock modern host ships pasta
# WITHOUT slirp4netns, which makes every allowlisted container start fail
# (fleet now preflights that at boot and fails closed — see ADR-0012). Cheap
# to install unconditionally so the mode is available if an operator picks it.
step "Installing system dependencies (runtime + sandbox toolchain)"
FLEET_DEPS=(git curl jq rsync python3 python3-pip chromium-headless gcc podman slirp4netns)
if command -v dnf >/dev/null 2>&1; then
  run dnf install -y "${FLEET_DEPS[@]}" || warn "dependency install failed — install these by hand: ${FLEET_DEPS[*]}"
  [[ "$DRY_RUN" == "1" ]] || ok "system dependencies present (${FLEET_DEPS[*]})"
else
  warn "dnf not found — skipping dependency install. Ensure these are present before continuing: ${FLEET_DEPS[*]}"
fi

# ── the fleet release: download + verify BEFORE anything uses it ──
# The binary every later step runs (`fleet release sandbox` below, the service
# with --enable-service). Verified by scripts/lib/release.sh with the pinned
# cosign: signature by fleet's release workflow on main, checksums, the
# release tag, the architecture — and then by the binary's own build info (no
# host executor, web bundle embedded). There is no unverified fallback.
FLEET_RELEASE_BIN=""
RELEASE_DIR=""
# RELEASE_TAG was resolved (and the checkout moved to it) above.
[[ -z "$RELEASE_TAG" || "$RELEASE_TAG" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] \
  || die "--tag must be a release tag like v2026.11.07.3, got: ${RELEASE_TAG}"
step "Fetching + verifying the fleet release ${RELEASE_TAG:-(newest published)}"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would download fleet-linux-$(fleet_release_arch 2>/dev/null || echo '<arch>'), SHA256SUMS and release.json for ${RELEASE_TAG:-the newest published release}, and verify them before use: the pinned cosign (internal/release/cosign.lock), the signature by ${FLEET_RELEASE_IDENTITY}, checksums, the release tag, no host executor"
else
  # Private staging under the root-owned release cache (never under the
  # checkout, which another user may own), the same place `fleet update`
  # stages.
  install -d -m 0700 "${FLEET_RELEASE_CACHE_DIR:-/var/cache/fleet}" "${FLEET_RELEASE_CACHE_DIR:-/var/cache/fleet}/release"
  RELEASE_DIR="${FLEET_RELEASE_CACHE_DIR:-/var/cache/fleet}/release/${RELEASE_TAG}"
  rm -rf "$RELEASE_DIR"
  _rel_commit="$(git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/${RELEASE_TAG}^{commit}" 2>/dev/null)" \
    || die "release ${RELEASE_TAG} is not a tag in ${REPO_ROOT}"
  FLEET_RELEASE_BIN="$(fleet_release_fetch "$REPO_ROOT" "$RELEASE_TAG" "$RELEASE_DIR" "${FLEET_RELEASE_CACHE_DIR:-/var/cache/fleet}" "$_rel_commit")" \
    || die "release ${RELEASE_TAG} could not be downloaded and verified (above) — nothing was installed, and bootstrap has no on-box build to fall back to (ADR-0101). (A --tag older than the newest ten releases has no downloadable assets any more.)"
  "$FLEET_RELEASE_BIN" version --build-info | grep -Eq '"host_executor":[[:space:]]*false' \
    || die "the verified binary reports the host executor compiled in — refusing it"
  "$FLEET_RELEASE_BIN" version --web >/dev/null || die "the verified binary carries no web bundle — refusing it"
  ok "verified ${RELEASE_TAG}: $("$FLEET_RELEASE_BIN" version)"
fi

# The Vultr Fedora image ships Cockpit's root-capable web login on :9090; our boxes are administered over ssh only.
cockpit_uninstall

# Vultr's cloud-init turns SSH password login on; make it key-only unless no
# key is installed yet (scripts/lib/ssh.sh). Never fatal.
ssh_key_only

# ── host identity: hostname = the public domain ───────────────────────────
# A Vultr box comes up as "vultr.guest", so every prompt reads root@vultr.
# With --enable-web --domain, rename it to that FQDN — only from an unset
# cloud-image default (vultr.guest, localhost[.localdomain]); any other name
# is left alone with a hint. FLEET_SET_HOSTNAME=1 forces, =0 skips
# (scripts/lib/hostname.sh). Never fatal.
if [[ "$ENABLE_WEB" == "1" && -n "$WEB_DOMAIN" ]]; then
  step "Setting the hostname to ${WEB_DOMAIN} (from an unset cloud default only)"
  fleet_set_hostname "$WEB_DOMAIN"
fi

# ── dedicated service user + rootless-Podman setup (--enable-service path) ──
# Done early (before the sandbox build) because the image is built INTO this
# user's rootless store and the unit runs as it.
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Provisioning the ${SERVICE_USER} service user + rootless Podman (the sandbox runs under it)"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would create user ${SERVICE_USER} (+subuid/subgid), HOME ${SERVICE_HOME}, /run/${SERVICE_USER}, and ~/.config/containers/containers.conf (cgroupfs)"
  elif command -v useradd >/dev/null 2>&1; then
    setup_service_user
  else
    warn "useradd not found — create the '${SERVICE_USER}' user + subuid/subgid + cgroupfs containers.conf manually (see deploy/fleet.service)."
  fi
fi

# ── credential env file (0600) ──
step "Ensuring credential env file ${ENV_FILE} (0600)"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would create ${ENV_FILE} (0600) if missing"
else
  if [[ ! -f "$ENV_FILE" ]]; then
    install -D -m 0600 /dev/null "$ENV_FILE"
    ok "created ${ENV_FILE} (0600)"
  else
    chmod 0600 "$ENV_FILE"
    ok "${ENV_FILE} present (mode set to 0600)"
  fi
fi

# ── client config bundle: resolve --client-config (clone url / point at path) ──
# A git URL is cloned (or pulled if already cloned) into a stable checkout dir; a
# path is pointed at directly. Either way CLIENT_CONFIG_DIR is updated and later
# persisted to the env file. Idempotent: re-running pulls an existing clone.
if [[ -n "$CLIENT_CONFIG_ARG" ]]; then
  step "Resolving client config (--client-config ${CLIENT_CONFIG_ARG})"
  if [[ "$CLIENT_CONFIG_ARG" == *://* || "$CLIENT_CONFIG_ARG" == *@*:* ]]; then
    # Looks like a git URL (scheme:// or scp-style git@host:path). An optional
    # trailing `#<sha-or-tag>` pins the checkout to that exact ref. A URL
    # fragment is invalid in a clone URL, so '#' is an unambiguous pin delimiter
    # here — and we split it ONLY in the URL branch, never for a path (a path
    # could legitimately contain '#').
    CLIENT_CONFIG_REF=""
    if [[ "$CLIENT_CONFIG_ARG" == *#* ]]; then
      CLIENT_CONFIG_REF="${CLIENT_CONFIG_ARG##*#}"
      CLIENT_CONFIG_ARG="${CLIENT_CONFIG_ARG%#*}"
    fi
    CHECKOUT="${FLEET_CLIENT_CONFIG_CHECKOUT:-/opt/fleet/client}"
    if [[ "$DRY_RUN" != "1" && -z "${FLEET_CLIENT_CONFIG_CHECKOUT:-}" ]]; then
      # Fall back to a repo-local checkout when /opt is not writable.
      if ! mkdir -p "$(dirname "$CHECKOUT")" 2>/dev/null || [[ ! -w "$(dirname "$CHECKOUT")" ]]; then
        CHECKOUT="./.fleet-client"
        warn "/opt not writable — cloning client config into ${CHECKOUT} instead"
      fi
    fi
    if ! command -v git >/dev/null 2>&1; then
      die "git is required to clone a --client-config URL (install git or pass a path)"
    fi
    if [[ "$DRY_RUN" == "1" ]]; then
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        info "[dry-run] would clone ${CLIENT_CONFIG_ARG} into ${CHECKOUT} and checkout pinned ref ${CLIENT_CONFIG_REF}"
      else
        info "[dry-run] would clone/pull ${CLIENT_CONFIG_ARG} into ${CHECKOUT}"
      fi
    elif [[ -d "${CHECKOUT}/.git" ]]; then
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        info "client config already cloned at ${CHECKOUT} — fetching + pinning to ${CLIENT_CONFIG_REF}"
        git -C "$CHECKOUT" fetch --quiet --tags origin || warn "git fetch failed in ${CHECKOUT}"
        git -C "$CHECKOUT" checkout --quiet "$CLIENT_CONFIG_REF" || die "git checkout ${CLIENT_CONFIG_REF} failed in ${CHECKOUT}"
      else
        info "client config already cloned at ${CHECKOUT} — pulling latest"
        git -C "$CHECKOUT" pull --ff-only --quiet || warn "git pull failed in ${CHECKOUT} (leaving existing checkout)"
      fi
    else
      run mkdir -p "$(dirname "$CHECKOUT")"
      git clone --quiet "$CLIENT_CONFIG_ARG" "$CHECKOUT" || die "git clone ${CLIENT_CONFIG_ARG} failed"
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        # Full clone then checkout, so a 40-char SHA works as uniformly as a tag
        # (a bare `clone --branch <sha>` cannot resolve a raw commit).
        git -C "$CHECKOUT" checkout --quiet "$CLIENT_CONFIG_REF" || die "git checkout ${CLIENT_CONFIG_REF} failed in ${CHECKOUT}"
      fi
      ok "cloned client config into ${CHECKOUT}"
    fi
    # Persist the pin to the state dir so `update` re-applies it without sourcing
    # the env file (update.sh reads from the inherited env / state file, not the
    # 0600 env file). A no-pin bootstrap clears any stale pin so the checkout
    # returns to branch-tracking on the next update.
    if [[ "$DRY_RUN" != "1" ]]; then
      STATE_DIR="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        mkdir -p "$STATE_DIR" && printf '%s\n' "$CLIENT_CONFIG_REF" > "$STATE_DIR/client-config.pin"
      else
        rm -f "$STATE_DIR/client-config.pin" 2>/dev/null || true
      fi
    fi
    CLIENT_CONFIG_DIR="$CHECKOUT"
  else
    # A path: point at it directly (must exist unless dry-run). Absolutized for
    # the same reason as the default above — the env file must never carry a
    # CWD-relative bundle path.
    if [[ "$DRY_RUN" != "1" && ! -d "$CLIENT_CONFIG_ARG" ]]; then
      die "--client-config path ${CLIENT_CONFIG_ARG} does not exist"
    fi
    CLIENT_CONFIG_DIR="$CLIENT_CONFIG_ARG"
    if [[ "$CLIENT_CONFIG_DIR" != /* ]]; then
      CLIENT_CONFIG_DIR="$(cd "$CLIENT_CONFIG_DIR" 2>/dev/null && pwd || printf '%s' "$CLIENT_CONFIG_DIR")"
    fi
    ok "using client config at ${CLIENT_CONFIG_DIR}"
  fi
fi

# ── persist the resolved client dir for `fleet update` ──
# The env file is 0600 root-only and read ONLY by systemd; update.sh never
# sources it (by design), so without this an interactive `fleet update` would
# silently fall back to the in-repo GENERIC bundle and skip pulling the client
# checkout. Mirror the client-config.pin channel: record the dir under the
# state dir; a generic-bundle bootstrap clears it so re-runs converge.
if [[ "$DRY_RUN" != "1" ]]; then
  STATE_DIR="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
  if [[ "$CLIENT_CONFIG_DIR" != "config/default" && "$CLIENT_CONFIG_DIR" != "$REPO_ROOT/config/default" ]]; then
    _abs_client_dir="$(cd "$CLIENT_CONFIG_DIR" 2>/dev/null && pwd || printf '%s' "$CLIENT_CONFIG_DIR")"
    mkdir -p "$STATE_DIR" && printf '%s\n' "$_abs_client_dir" > "$STATE_DIR/client-config.dir"
  else
    rm -f "$STATE_DIR/client-config.dir" 2>/dev/null || true
  fi
fi

# ── client config bundle ──
step "Checking client config bundle (FLEET_CLIENT_CONFIG_DIR=${CLIENT_CONFIG_DIR})"
if [[ -f "${CLIENT_CONFIG_DIR}/manifest.yaml" ]]; then
  ok "client bundle manifest found at ${CLIENT_CONFIG_DIR}/manifest.yaml"
  # CLIENT_CONFIG_DIR was absolutized above, so compare against the absolute
  # in-repo default (the relative form never matched, and this notice never
  # printed).
  if [[ "${CLIENT_CONFIG_DIR}" == "${REPO_ROOT}/config/default" ]]; then
    info "using the GENERIC default bundle (neutral branding, no MCP connectors)."
    info "for a branded deploy, check out a client repo and set FLEET_CLIENT_CONFIG_DIR to it."
  fi
else
  warn "no manifest.yaml at ${CLIENT_CONFIG_DIR} — fleet will fail to start until"
  warn "FLEET_CLIENT_CONFIG_DIR points at a valid bundle (a dir with manifest.yaml)."
fi

# ── bare install: stage the in-repo default bundle where the service can use it (#1655) ──
# The sandbox relabels bundle dirs (:z) on mount, which needs a source the
# service user owns AND can write under the unit's ProtectSystem=strict mount
# namespace. The checkout is neither (root-owned, read-only there), so a bare
# --enable-service install runs on a service-owned copy under the state dir,
# refreshed by every bootstrap and update run. See scripts/lib/bundle.sh.
if [[ "$ENABLE_SERVICE" == "1" ]] && bundle_is_default_in_checkout "$CLIENT_CONFIG_DIR" "$REPO_ROOT"; then
  _stage="$(default_bundle_stage "$SERVICE_HOME")"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would stage ${CLIENT_CONFIG_DIR} → ${_stage} (owned by ${SERVICE_USER}; the sandbox's SELinux relabel needs a writable, service-owned source) and set FLEET_CLIENT_CONFIG_DIR to it"
    # CLIENT_CONFIG_DIR stays on the source: the staged copy does not exist in
    # a dry run, and the rest of the plan reads the bundle's manifest (sandbox
    # image, host-side MCP requirements), whose content the copy would match.
  elif stage_default_bundle "$CLIENT_CONFIG_DIR" "$_stage" "$SERVICE_USER"; then
    ok "default bundle staged: ${CLIENT_CONFIG_DIR} → ${_stage} (owned by ${SERVICE_USER}; FLEET_CLIENT_CONFIG_DIR will point here)"
    CLIENT_CONFIG_DIR="$_stage"
    # The state-file step above cleared client-config.dir for the generic
    # bundle; the service now runs the staged copy, so record that instead —
    # it is what `fleet update --check` and update's fallback resolution read.
    _dir_state="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
    mkdir -p "$_dir_state" && printf '%s\n' "$_stage" > "$_dir_state/client-config.dir" || warn "could not record ${_stage} in ${_dir_state}/client-config.dir"
  else
    # Fatal, before the env file or the unit is written: carrying on would
    # persist the checkout as the bundle and start a service whose sandbox
    # cannot mount it on an enforcing host — the failure staging exists to end.
    _rc=$?
    if [[ "$_rc" == "2" ]]; then
      die "${_stage} holds a bundle that is not a staged copy — leaving it alone. Point FLEET_CLIENT_CONFIG_DIR (or --client-config) at it, or move it aside and re-run."
    else
      die "could not stage the default bundle to ${_stage} (see the error above) — without it the sandbox's SELinux relabel of ${CLIENT_CONFIG_DIR} fails (EROFS) on an enforcing host. Fix the cause and re-run."
    fi
  fi
elif [[ "$ENABLE_SERVICE" == "1" ]] && bundle_is_in_checkout "$CLIENT_CONFIG_DIR" "$REPO_ROOT"; then
  warn "client bundle ${CLIENT_CONFIG_DIR} is inside the fleet checkout: root-owned and read-only to the unit, so the sandbox's SELinux relabel will fail (EROFS) on an enforcing host."
  warn "  check it out outside the repo (--client-config <git-url>, which lands at /opt/fleet/client) — only the generic config/default is staged automatically."
fi

# ── bundle ownership for the rootless sandbox (--enable-service path) ──
# The sandbox bind-mounts bundle dirs (protocols/ personas/ skills/ system_prompts/)
# into the container with SELinux relabeling (:z); the rootless service user can
# only relabel files it OWNS. Chown the CHECKOUT to the service user — skip the
# in-repo default bundle (chowning the repo would be wrong, and the service can't
# read a bundle under /root anyway given ProtectHome).
if [[ "$ENABLE_SERVICE" == "1" && "$DRY_RUN" != "1" && -d "$CLIENT_CONFIG_DIR" \
      && "$CLIENT_CONFIG_DIR" != "config/default" && "$CLIENT_CONFIG_DIR" != "$REPO_ROOT"/* ]]; then
  if chown -R "$SERVICE_USER":"$SERVICE_USER" "$CLIENT_CONFIG_DIR"; then
    ok "bundle ${CLIENT_CONFIG_DIR} owned by ${SERVICE_USER} (so rootless :z relabel is permitted)"
  else
    warn "could not chown ${CLIENT_CONFIG_DIR} to ${SERVICE_USER} — sandbox :z relabel may fail (EPERM)."
  fi
fi

# resolve_sandbox_image MANIFEST — print the bundle's resolved sandbox.image,
# the SAME way the Go loader (internal/clientconfig) does, interpolated
# against the process env (fleet_sandbox_resolve_scalar, shared with update.sh
# and doctor.sh). An empty result => build-on-box (the default-bundle value
# "${FLEET_SANDBOX_IMAGE:-}" resolves to empty when the var is unset).
# A reference the loader would refuse (unset ${X}, or ${X:?m}) stops the
# install with the loader's reason: fleet would not start with this bundle.
resolve_sandbox_image() {
  [[ -f "$1" ]] || return 0
  fleet_sandbox_resolve_scalar image "$1" fleet_shell_env_value || die "client bundle $1: fleet refuses this sandbox.image at startup (see above)"
}

# ── removed CHAT_/CUTLASS_ variables (ADR-0106) ──
# The verified release binary reads them as unset, so a bundle that still
# uses one may not load: its read-only scan refuses the install up front
# (scripts/lib/retired-bundle.sh).
if [[ -n "${FLEET_RELEASE_BIN:-}" && -x "${FLEET_RELEASE_BIN}" ]]; then
  fleet_retired_bundle_check "$FLEET_RELEASE_BIN" "$CLIENT_CONFIG_DIR" \
    || die "the client bundle ${CLIENT_CONFIG_DIR} uses removed CHAT_/CUTLASS_ variables (above) — nothing was installed; rename these in the client bundle and re-run"
fi

# ── sandbox image ──
# The release publishes the generic bundle's sandbox image, signed. A bundle
# that ships no Containerfile, or the generic one unchanged, runs that image:
# verified (`fleet release sandbox`), pulled BY DIGEST into the store the
# service reads, tagged as the bundle's sandbox.tag. A bundle with its own
# Containerfile is built here — layered on that verified image when it
# declares `ARG FLEET_SANDBOX_BASE` (docs/RELEASE-ARTIFACTS.md). A bundle that
# names sandbox.image in its manifest uses that image; nothing happens here.
step "Sandbox image"
# The manifest's sandbox.containerfile when set, as build-sandbox-image.sh
# resolves it (scripts/lib/sandbox-bundle.sh).
SANDBOX_CONTAINERFILE="$(fleet_sandbox_containerfile "$CLIENT_CONFIG_DIR")"
SANDBOX_IMAGE_REF="$(resolve_sandbox_image "${CLIENT_CONFIG_DIR}/manifest.yaml")"
SANDBOX_TAG="$(sandbox_manifest_tag "${CLIENT_CONFIG_DIR}/manifest.yaml")"
sandbox_published=0
if fleet_sandbox_uses_release_image "$CLIENT_CONFIG_DIR" "$REPO_ROOT/config/default"; then
  sandbox_published=1
fi
if [[ -z "$SANDBOX_IMAGE_REF" && ! -f "$SANDBOX_CONTAINERFILE" ]] && fleet_sandbox_containerfile_named "$CLIENT_CONFIG_DIR"; then
  die "the bundle's manifest names sandbox.containerfile ${SANDBOX_CONTAINERFILE}, which does not exist — fix the bundle (the generic release image is never used in place of a bundle's own Containerfile)"
fi
# sandbox_podman CMD… — podman in the store the service reads: the service
# user's rootless store on the --enable-service path, else the invoking user's.
sandbox_podman() {
  if [[ "$ENABLE_SERVICE" == "1" ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
    ( cd "$SERVICE_HOME" 2>/dev/null || cd /
      runuser -u "$SERVICE_USER" -- env HOME="$SERVICE_HOME" XDG_RUNTIME_DIR="/run/${SERVICE_USER}" podman "$@" )
  else
    podman "$@"
  fi
}
if [[ -n "$SANDBOX_IMAGE_REF" ]]; then
  info "manifest resolves sandbox.image=${SANDBOX_IMAGE_REF} — using a prebuilt/registry image; nothing to pull or build here."
elif [[ "$DRY_RUN" == "1" ]]; then
  if [[ "$sandbox_published" == "1" ]]; then
    info "[dry-run] would verify the release's signed sandbox image (fleet release sandbox), pull it BY DIGEST and tag it ${SANDBOX_TAG}"
  else
    info "[dry-run] would run: FLEET_CLIENT_CONFIG_DIR=${CLIENT_CONFIG_DIR} scripts/build-sandbox-image.sh (layered on the release's verified image when the Containerfile declares FLEET_SANDBOX_BASE)"
  fi
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — no sandbox image (install podman, then run: sudo fleet update --no-pull)."
elif [[ "$sandbox_published" == "1" ]]; then
  if ! SANDBOX_PUB_REF="$("$FLEET_RELEASE_BIN" release sandbox --tag "$RELEASE_TAG" --dir "$RELEASE_DIR")"; then
    die "the release's sandbox image did not verify (above) — refusing to set up an unverified sandbox"
  elif sandbox_podman pull "$SANDBOX_PUB_REF" && sandbox_podman tag "$SANDBOX_PUB_REF" "$SANDBOX_TAG"; then
    STATE_DIR_B="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
    mkdir -p "$STATE_DIR_B" && printf '%s\n' "$SANDBOX_PUB_REF" > "$STATE_DIR_B/sandbox-image.digest" \
      && printf '%s\n' "$SANDBOX_TAG" > "$STATE_DIR_B/sandbox-image.ref" || true
    ok "sandbox image ${SANDBOX_TAG} = ${SANDBOX_PUB_REF} (verified, pulled by digest)"
  else
    warn "pulling ${SANDBOX_PUB_REF} failed — fleet will have no runnable sandbox image; retry: sudo fleet update --no-pull"
  fi
else
  sandbox_base=""
  # What an on-box build uses: the bundle's Containerfile, or — for a bundle
  # that ships none and names none — the generic one, resolved by the helper
  # build-sandbox-image.sh uses too, so both build paths build the same thing.
  SANDBOX_BUILD_CF="$(fleet_sandbox_build_containerfile "$CLIENT_CONFIG_DIR" "$REPO_ROOT/config/default")"
  if grep -q 'FLEET_SANDBOX_BASE' "$SANDBOX_BUILD_CF"; then
    sandbox_base="$("$FLEET_RELEASE_BIN" release sandbox --tag "$RELEASE_TAG" --dir "$RELEASE_DIR")" \
      || die "the release's sandbox image (this bundle's FLEET_SANDBOX_BASE) did not verify (above)"
    info "the bundle layers on the release's verified sandbox image ${sandbox_base}"
  fi
  # build-sandbox-image.sh builds as the unit's User= when the unit exists;
  # before the unit is installed (a first bootstrap), build as the service user
  # directly so the image lands in the store the service will read.
  if [[ "$ENABLE_SERVICE" == "1" ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
    info "building as ${SERVICE_USER} (rootless) → ${SANDBOX_TAG}"
    _base_args=()
    [[ -n "$sandbox_base" ]] && _base_args=(--build-arg "FLEET_SANDBOX_BASE=${sandbox_base}")
    if sandbox_podman build --pull=newer "${_base_args[@]}" -t "$SANDBOX_TAG" -f "$SANDBOX_BUILD_CF" "$(dirname "$SANDBOX_BUILD_CF")"; then
      ok "sandbox image ${SANDBOX_TAG} built from ${SANDBOX_BUILD_CF} into ${SERVICE_USER}'s rootless store"
    else
      # Enabling the service without a sandbox image starts a fleet whose
      # every tool call fails: stop here instead.
      die "rootless sandbox build (as ${SERVICE_USER}) of ${SANDBOX_BUILD_CF} failed — not enabling ${SERVICE_NAME} without a runnable sandbox image"
    fi
  elif FLEET_CLIENT_CONFIG_DIR="${CLIENT_CONFIG_DIR}" FLEET_SANDBOX_BASE="$sandbox_base" "$(dirname "$0")/build-sandbox-image.sh"; then
    ok "sandbox image built from ${SANDBOX_BUILD_CF}"
  elif [[ "$ENABLE_SERVICE" == "1" ]]; then
    die "sandbox image build failed — not enabling ${SERVICE_NAME} without a runnable sandbox image"
  else
    warn "sandbox image build failed — run scripts/build-sandbox-image.sh manually before starting fleet."
  fi
fi

# ── host-side MCP server Python deps (the active bundle's requirements) ──
# fleet runs the bundle's MCP servers host-side as `python3 <bundle>/mcp/*.py`, so
# their Python deps must be importable by the system python3 the service user runs.
# Generic: installs whatever THIS bundle's mcp/requirements.txt lists (no
# bundle-specific package names here). --break-system-packages is for Fedora's
# PEP-668 externally-managed python; harmless to drop on other distros.
if [[ "$ENABLE_SERVICE" == "1" && -f "${CLIENT_CONFIG_DIR}/mcp/requirements.txt" ]]; then
  step "Installing the bundle's host-side MCP Python deps (${CLIENT_CONFIG_DIR}/mcp/requirements.txt)"
  _mcp_state_dir="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: python3 -m pip install --break-system-packages -r ${CLIENT_CONFIG_DIR}/mcp/requirements.txt"
    info "[dry-run] would record the mcp/requirements.txt hash under ${_mcp_state_dir}/mcp-requirements.sha256"
  elif command -v python3 >/dev/null 2>&1; then
    if python3 -m pip install --break-system-packages -r "${CLIENT_CONFIG_DIR}/mcp/requirements.txt"; then
      mkdir -p "$_mcp_state_dir"
      sha256sum "${CLIENT_CONFIG_DIR}/mcp/requirements.txt" | awk '{print $1}' > "$_mcp_state_dir/mcp-requirements.sha256" 2>/dev/null || true
      ok "bundle MCP Python deps installed (host-side servers can start)"
    else
      warn "pip install of ${CLIENT_CONFIG_DIR}/mcp/requirements.txt failed — host-side MCP servers may not start."
    fi
  else
    warn "python3 not found — install ${CLIENT_CONFIG_DIR}/mcp/requirements.txt manually."
  fi
fi

# ── host-side Playwright browser automation runtime ──
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would ensure python3 playwright package is installed for built-in browser automation"
  elif command -v python3 >/dev/null 2>&1; then
    if ! python3 -c "import playwright" >/dev/null 2>&1; then
      python3 -m pip install --break-system-packages playwright >/dev/null 2>&1 || warn "pip install playwright failed — install python3 playwright for built-in browser automation."
    fi
  fi
fi

# ── Postgres provisioning ──
# env_dsn_password KEY — extract the password from an existing postgres:// DSN
# already recorded in $ENV_FILE (empty when the file/key/password is absent).
# A re-run on a provisioned box MUST reuse the live password: without this, a
# freshly generated password was written into the env-file DSN while the
# CREATE ROLE below was skipped (role exists), leaving a DSN the cluster
# rejects — the "idempotent re-run" broke DB auth on the next restart.
#
# #2055 review round 6: the password the driver sends is pgx's — a
# ?password= query parameter overrides the userinfo one (the last occurrence
# winning), so it is the live password when present.
env_dsn_password() {
  local url rest pw="" q p v
  url="$(fleet_env_value "$ENV_FILE" "$1")"
  [[ "$url" == postgres://* || "$url" == postgresql://* ]] || return 0
  rest="${url#*://}"
  if [[ "$url" =~ ^postgres(ql)?://[^:/@]+:([^@/]+)@ ]]; then
    # The DSN carries the password percent-encoded; the role gets it decoded,
    # exactly as the driver will send it.
    pw="${BASH_REMATCH[2]}"
    printf -v pw '%b' "${pw//%/\\x}"
  fi
  [[ "${rest%%/*}" == *@* ]] && rest="${rest#*@}"
  if [[ "$rest" == *"?"* ]]; then
    q="${rest#*\?}"
    local -a pairs=()
    IFS='&' read -r -a pairs <<< "$q"
    for p in "${pairs[@]}"; do
      [[ "${p%%=*}" == "password" && "$p" == *=* ]] || continue
      v="$(fleet_pgx_unescape "${p#*=}")" && pw="$v"
    done
  fi
  printf '%s' "$pw"
}

# pg_exists role|db NAME — probe the LOCAL cluster (as postgres) for an
# existing role/database. Echoes yes|no|unknown; unknown = the probe could not
# run at all (no runuser/postgres user, cluster not up — e.g. a --dry-run on a
# box whose cluster was never initialized).
pg_exists() {
  local q out
  case "$1" in
    role) q="SELECT 1 FROM pg_roles WHERE rolname='$2'" ;;
    db)   q="SELECT 1 FROM pg_database WHERE datname='$2'" ;;
    *)    echo unknown; return ;;
  esac
  if ! command -v runuser >/dev/null 2>&1 || ! id postgres >/dev/null 2>&1; then
    echo unknown; return
  fi
  if out="$(runuser -u postgres -- psql -tAc "$q" 2>/dev/null)"; then
    if [[ "$out" == "1" ]]; then echo yes; else echo no; fi
  else
    echo unknown
  fi
}

# env_file_records KEY USER DB — true when the env file already carries KEY= a
# DSN for USER@…/DB, i.e. a PREVIOUS run of this script provisioned that pair;
# re-using its password and converging the role is the normal idempotent path.
env_file_records() {
  local url re
  # fleet_env_value, like env_dsn_password: what fleet itself reads (last
  # assignment, export/comments/quotes handled) — the DSN the server uses.
  url="$(fleet_env_value "$ENV_FILE" "$1")"
  re="^postgres(ql)?://$2:[^@]*@[^/]+/$3(\?.*)?$"
  [[ "$url" =~ $re ]]
}

# guard_preexisting LABEL ROLE DB ENVKEY ADOPTFLAG NAMEFLAG [USERFLAG] — refuse to
# provision over a role/database that already exists on the local cluster but
# is NOT recorded in the env file by a previous run of this script. Without
# this, defaults colliding with a legacy install (role/db "chat") would
# silently ALTER the legacy role's password — locking out the still-installed
# legacy server and any later legacy export — and then run fleet's migrations
# on the legacy database. The operator must choose: adopt it explicitly, or
# pick fresh names.
guard_preexisting() {
  local label="$1" role="$2" db="$3" envkey="$4" adoptflag="$5" nameflag="$6" userflag="${7:-}"
  if env_file_records "$envkey" "$role" "$db"; then
    info "${label}: ${envkey} in ${ENV_FILE} already records ${role}@…/${db} (provisioned by a previous run) — converging as usual."
    return 0
  fi
  local role_exists db_exists
  role_exists="$(pg_exists role "$role")"
  db_exists="$(pg_exists db "$db")"
  if [[ "$role_exists" != "yes" && "$db_exists" != "yes" ]]; then
    if [[ "$role_exists" == "unknown" || "$db_exists" == "unknown" ]]; then
      if [[ "$DRY_RUN" == "1" ]]; then
        info "${label}: cluster not probeable yet — a real run re-checks for a pre-existing role/db after starting it."
      else
        warn "${label}: could not probe the cluster for a pre-existing role/db — provisioning below will fail if it is unreachable."
      fi
    fi
    return 0
  fi
  local found=""
  [[ "$role_exists" == "yes" ]] && found="role '${role}'"
  if [[ "$db_exists" == "yes" ]]; then
    [[ -n "$found" ]] && found+=" and "
    found+="database '${db}'"
  fi
  local msg
  msg="$(printf '%s\n' \
    "Postgres ${found} already exist(s) on this cluster but was NOT provisioned by this script" \
    "  (no matching ${envkey} DSN in ${ENV_FILE}). It likely belongs to something else — e.g. a" \
    "  legacy chat/moc install. Proceeding would rotate that role's password (locking out whatever" \
    "  still uses it, including a later legacy export) and run fleet's migrations on that database." \
    "  Choose explicitly, then re-run:" \
    "    fresh names:  ${nameflag} fleet_${label} (and the matching ${userflag:-"--${label}-db-user"}; any unused names)" \
    "    adopt it:     ${adoptflag} with ${envkey}=<working DSN> (never creates/alters the role)" \
    "  Migrating legacy data? Use fresh names + export/import — see docs/CUTOVER.md.")"
  if [[ "$DRY_RUN" == "1" ]]; then
    warn "${label}: a real run would REFUSE here:"
    printf '%s\n' "$msg" | sed 's/^/    /' >&2
    return 0
  fi
  die "$msg"
}

if [[ "$POSTGRES_MODE" == "local" ]]; then
  SSLMODE="disable"
  FLEET_DB_PASSWORD="$(env_dsn_password DATABASE_URL)"
  FLEET_DB_PASSWORD="${FLEET_DB_PASSWORD:-$(gen_pass)}"

  step "Branch A (local): install + init a local Postgres cluster"
  if command -v dnf >/dev/null 2>&1; then
    run dnf install -y postgresql-server postgresql
  else
    warn "dnf not found — skipping package install (assuming postgresql-server present)"
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would initdb (if needed), set pg_hba scram-sha-256 on loopback, systemctl enable --now postgresql"
  else
    if [[ ! -s /var/lib/pgsql/data/PG_VERSION ]]; then
      info "initializing data directory"
      if command -v postgresql-setup >/dev/null 2>&1; then
        postgresql-setup --initdb >/dev/null 2>&1 || runuser -u postgres -- /usr/bin/initdb -D /var/lib/pgsql/data
      else
        runuser -u postgres -- /usr/bin/initdb -D /var/lib/pgsql/data
      fi
    fi
    if command -v systemctl >/dev/null 2>&1; then
      systemctl enable --now postgresql >/dev/null 2>&1 || warn "could not start postgresql via systemctl (already running?)"
    fi

    # Default Fedora/RHEL initdb authenticates loopback TCP with `ident`, which
    # REJECTS the password DSN fleet connects with (postgres://chat:…@127.0.0.1).
    # Rewrite the loopback host lines to scram-sha-256 so first boot authenticates
    # (chat/moc bootstrap did this; fleet must too). local peer is left intact so
    # the `runuser -u postgres psql` role provisioning below still works.
    PG_HBA="$(runuser -u postgres -- psql -tAc 'SHOW hba_file' 2>/dev/null || true)"
    [[ -n "$PG_HBA" && -f "$PG_HBA" ]] || PG_HBA="/var/lib/pgsql/data/pg_hba.conf"
    if [[ -f "$PG_HBA" ]]; then
      if grep -qE '^[[:space:]]*host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1/32|::1/128)[[:space:]]+(ident|md5|trust|peer)' "$PG_HBA"; then
        sed -i -E 's#^([[:space:]]*host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1/32|::1/128)[[:space:]]+)(ident|md5|trust|peer)#\1scram-sha-256#' "$PG_HBA"
        systemctl reload postgresql >/dev/null 2>&1 || warn "could not reload postgresql after pg_hba rewrite"
        ok "pg_hba: loopback host auth set to scram-sha-256 (${PG_HBA})"
      else
        info "pg_hba loopback host lines already scram-sha-256 (or non-default) — left as-is"
      fi
    else
      warn "could not locate pg_hba.conf — verify loopback host auth allows password (scram-sha-256) manually"
    fi
  fi

  step "Creating the role + database idempotently (${FLEET_DB_USER}/${FLEET_DB_NAME}; the stores use its schemas chat and sched)"
  # Refuse to touch a pre-existing role/db this script did not provision; an
  # adopted database skips both the guard and provisioning entirely. The
  # CREATE-if-missing + ALTER converge only a role this script provisioned
  # (its DSN is in the env file, whose password is reused), never another's.
  if [[ "$ADOPT_DB" == "1" ]]; then
    info "--adopt-existing-db — skipping role/database provisioning (no CREATE, no ALTER, no password change)."
    DB_URL="$ADOPTED_DB_URL"
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would validate the adopted DSN with: psql '<dsn>' -c 'SELECT 1', then check the role may create the chat and sched schemas"
    else
      psql -v ON_ERROR_STOP=1 "$DB_URL" -c "SELECT 1" >/dev/null \
        || die "adopted DSN failed SELECT 1 — supply the existing role's working DSN in DATABASE_URL (this script never rotates its password)"
      require_schema_privilege "$DB_URL" "the adopted DATABASE_URL"
      ok "database adopted (existing role/database, SELECT 1 ok, may create the chat and sched schemas; password untouched)"
    fi
  else
    guard_preexisting db "$FLEET_DB_USER" "$FLEET_DB_NAME" DATABASE_URL --adopt-existing-db --db-name --db-user
    # A single quote would end the password's SQL literal early (see below).
    if [[ "$FLEET_DB_PASSWORD" == *"'"* ]]; then
      if [[ "$DRY_RUN" == "1" ]]; then
        warn "the database password contains a single quote, which the provisioning SQL cannot carry; a real run would REFUSE here."
      else
        die "the database password (in the env file's DATABASE_URL) contains a single quote, which the provisioning SQL cannot carry; set a password without one."
      fi
    fi
    PSQL_SQL="$(fleet_role_db_sql "$FLEET_DB_USER" "$FLEET_DB_PASSWORD" "$FLEET_DB_NAME")"
    PSQL_PLAN="$(fleet_role_db_sql "$FLEET_DB_USER" "$FLEET_DB_PW_PLACEHOLDER" "$FLEET_DB_NAME")"
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would run as postgres:"
      printf '%s\n' "$PSQL_PLAN" | sed 's/^/    /'
    else
      printf '%s\n' "$PSQL_SQL" | fleet_psql_provision "$FLEET_DB_PASSWORD" \
        || die "role/database provisioning failed"
    fi
    if [[ -n "$RECORDED_DB_URL" && -n "$(env_dsn_password DATABASE_URL)" ]]; then
      DB_URL="$RECORDED_DB_URL" # a re-run keeps the recorded DSN as written; the role was converged to its password
    else
      DB_URL="postgres://${FLEET_DB_USER}:${FLEET_DB_PASSWORD}@127.0.0.1:5432/${FLEET_DB_NAME}?sslmode=${SSLMODE}"
    fi
    ok "database: ${FLEET_DB_NAME} (owner ${FLEET_DB_USER}), sslmode=${SSLMODE}"
  fi

else
  SSLMODE="require"
  step "Branch B (external): validate the pre-provisioned DATABASE_URL (SELECT 1 + schema privilege)"
  # The process env first, then the env file — a re-run converges on the DSN
  # a previous run recorded, like every other persisted setting.
  DB_URL="${DATABASE_URL:-$(fleet_env_value "$ENV_FILE" DATABASE_URL)}"
  [[ -n "$DB_URL" ]] || die "external mode needs DATABASE_URL (in the environment, or already in ${ENV_FILE})"
  if [[ -n "${FLEET_DB_SUPERUSER_URL:-}" ]]; then
    step "Opt-in: creating the database and granting the role CREATE on it via FLEET_DB_SUPERUSER_URL"
    # The role and database come from DATABASE_URL itself, so the database
    # created is the one fleet will use; the grant lets the role create the
    # chat and sched schemas in a database the superuser owns.
    _role_db="$(fleet_dsn_role_db "$DB_URL")" \
      || die "FLEET_DB_SUPERUSER_URL needs a DATABASE_URL of the form postgres://<role>:…@host/<database> with a lower-case role and database name"
    SU_SQL="$(fleet_external_db_sql "${_role_db%% *}" "${_role_db#* }")"
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would run as FLEET_DB_SUPERUSER_URL:"
      printf '%s\n' "$SU_SQL" | sed 's/^/    /'
    else
      printf '%s\n' "$SU_SQL" | psql -v ON_ERROR_STOP=1 "$FLEET_DB_SUPERUSER_URL" >/dev/null \
        || die "superuser provisioning failed"
    fi
    unset _role_db
  fi
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: psql '<dsn>' -c 'SELECT 1', then check the role may create the chat and sched schemas"
  else
    psql -v ON_ERROR_STOP=1 "$DB_URL" -c "SELECT 1" >/dev/null || die "DATABASE_URL failed SELECT 1"
    # fleet's stores create their schemas (chat, sched) on first start; a role
    # without CREATE on the database would pass SELECT 1 here and then fail
    # at boot. Refuse now instead.
    require_schema_privilege "$DB_URL" "DATABASE_URL" \
      "or set FLEET_DB_SUPERUSER_URL so bootstrap grants it"
    ok "DATABASE_URL answered SELECT 1 and may create the chat and sched schemas (sslmode=${SSLMODE} expected in the DSN)"
  fi
fi

# ── write/refresh the env file (0600) ──
# Persist the resolved DSNs + the client-bundle dir so the fleet process and
# the operator CLI reads them from the SAME 0600 file deploy/fleet.service EnvironmentFiles.
# Idempotent: re-running rewrites these keys in place. The local-mode password
# is reused from the env file's existing DATABASE_URL, and the cluster role is
# ALTERed to match — the env file and the cluster never disagree after a run.
step "Writing connection settings into ${ENV_FILE} (0600)"
upsert_env DATABASE_URL "$DB_URL"
upsert_env FLEET_CLIENT_CONFIG_DIR "$CLIENT_CONFIG_DIR"
# Point config.Load at the same file so process-env and config-loaded values match.
upsert_env FLEET_ENV_FILE "$ENV_FILE"
if [[ "$DRY_RUN" != "1" ]]; then
  ok "wrote DATABASE_URL / FLEET_CLIENT_CONFIG_DIR / FLEET_ENV_FILE"
  # OPENROUTER_API_KEY: prompt for it interactively (hidden) when absent, so the
  # documented one-command deploy ends with a runnable service instead of a
  # "remember to add the key" homework item. Skippable (blank), non-TTY runs
  # keep the reminder, and an existing key is never re-prompted (idempotent).
  if grep -q '^OPENROUTER_API_KEY=' "$ENV_FILE" 2>/dev/null; then
    info "OPENROUTER_API_KEY already set in ${ENV_FILE} — leaving it."
  elif [[ -t 0 ]]; then
    printf 'OpenRouter API key (from https://openrouter.ai/keys; blank to skip): '
    read -rs _or_key; printf '\n'
    if [[ -n "$_or_key" ]]; then
      upsert_env OPENROUTER_API_KEY "$_or_key"
      ok "wrote OPENROUTER_API_KEY"
    else
      info "skipped — set it later with: fleet config set-openrouter-key"
    fi
    unset _or_key
  else
    info "remember to add OPENROUTER_API_KEY (fleet config set-openrouter-key) and the bundle's MCP connector credentials."
  fi
  info "if the bundle's default persona differs from 'assistant', set FLEET_PERSONA_DEFAULT=<name> and FLEET_PERSONA=personas/<name>.yaml in ${ENV_FILE}."
fi

# ── optionally install the binary + unit, then enable + start it ──
# The verified release binary goes to FLEET_INSTALL_DIR (default /opt/fleet) —
# the location deploy/fleet.service's ExecStart points at. The checkout still
# supplies the units, scripts and the generic bundle.
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Installing the fleet binary, then enabling ${SERVICE_NAME}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would install fleet ${RELEASE_TAG:-(the newest published release)} →${INSTALL_DIR} (fleet release install --no-restart: atomic rename; a different binary already there is kept as fleet.prev, the same one is left as it is)"
    info "[dry-run] would symlink /usr/local/bin/fleet → ${INSTALL_DIR} (operator PATH)"
    info "[dry-run] would install deploy/fleet-motd.sh → /etc/profile.d/fleet-motd.sh (login banner)"
    info "[dry-run] would install deploy/fleet.service → /etc/systemd/system"
    if fleet_web_tier_present; then
      info "[dry-run] would retire the Next.js web tier (ADR-0091): stop + disable fleet-web, remove its unit, drop-in, start shim and /opt/fleet/web"
    fi
    info "[dry-run] would run: systemctl daemon-reload && systemctl enable --now ${SERVICE_NAME}"
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-service (no systemd on this box)."
  else
    # 1. The binary: the verified release.
    [[ -x "$FLEET_RELEASE_BIN" ]] \
      || die "no verified fleet binary to install — re-run bootstrap (it downloads and verifies the release)"
    # Atomic: stage beside the live binary, keep any existing one as
    # fleet.prev (what `fleet release rollback` restores), rename over — and
    # when the live binary already IS this release, swap nothing and leave
    # fleet.prev alone. It goes through the same Go install `fleet update`
    # uses (`fleet release install --no-restart`, run from the verified
    # binary, which re-verifies the staged files from open descriptors); the
    # unit is enabled and started below.
    "$FLEET_RELEASE_BIN" release install --tag "$RELEASE_TAG" --dir "$RELEASE_DIR" \
      --install-dir "$INSTALL_DIR" --service "$SERVICE_NAME" --no-restart \
      || die "could not install fleet ${RELEASE_TAG} into ${INSTALL_DIR} (above)"
    ok "installed fleet ${RELEASE_TAG} → ${INSTALL_DIR}"
    # Put `fleet` on the operator's PATH (#461). deploy/fleet.service's
    # ExecStart points at $INSTALL_DIR/fleet, but an operator typing `fleet
    # status` / `fleet update` / `fleet chat` needs it on PATH too — that gap
    # was the "fleet isn't installed" symptom. Symlink (not copy) so a later
    # update of $INSTALL_DIR/fleet is reflected with no second install.
    if [[ -d /usr/local/bin ]] || install -d /usr/local/bin 2>/dev/null; then
      ln -sf "$INSTALL_DIR/fleet" /usr/local/bin/fleet && info "linked /usr/local/bin/fleet → ${INSTALL_DIR}/fleet"
    fi
    # MOTD (#461): a login banner like the sibling chat repo's. profile.d runs
    # `fleet motd` (version + service state + commands; no secrets) on an
    # interactive login. Best-effort — never fail the install on it.
    if [[ -d /etc/profile.d || -w /etc ]] && [[ -f "$REPO_ROOT/deploy/fleet-motd.sh" ]]; then
      install -D -m 0644 "$REPO_ROOT/deploy/fleet-motd.sh" /etc/profile.d/fleet-motd.sh \
        && info "installed /etc/profile.d/fleet-motd.sh (login banner)" || true
    fi
    # 2. Install the unit file from this checkout if not already present.
    if [[ -f "$REPO_ROOT/deploy/fleet.service" ]] && ! systemctl cat fleet.service >/dev/null 2>&1; then
      install -D -m 0644 "$REPO_ROOT/deploy/fleet.service" /etc/systemd/system/fleet.service
      info "installed /etc/systemd/system/fleet.service"
    fi
    # The binary just installed serves the web UI on 127.0.0.1:3000 — the port
    # the retired Next.js unit (fleet-web) held. On a re-run over a box
    # provisioned before ADR-0091, stop that unit first or fleet cannot bind;
    # its files go only once fleet is up on the new binary (below).
    fleet_stop_web_tier || true
    # 3. daemon-reload + enable the backend unit (which serves the web UI too).
    systemctl daemon-reload || warn "systemctl daemon-reload failed"
    if systemctl enable --now "${SERVICE_NAME}" >/dev/null 2>&1; then
      ok "${SERVICE_NAME} enabled + started (services self-migrate on start)"
    else
      warn "could not enable/start ${SERVICE_NAME} — check: journalctl -u ${SERVICE_NAME} -n 50"
    fi
    # The old web tier was stopped above: an already-running fleet is still
    # the OLD binary (enable --now does not restart it), which serves no UI —
    # restart it onto the new one and remove the old tier only once it is up.
    # If it does not come up, the old tier is restored and bootstrap STOPS
    # here: nothing later may retire it again or report success (#1838).
    if fleet_web_tier_present; then
      cutover_web_tier_or_die
    fi
    if [[ "$ENABLE_WEB" != "1" ]]; then
      info "web UI: fleet serves it on 127.0.0.1:3000; re-run with --enable-web [--domain <fqdn>] to set up sign-in"
      info "        (APP_SESSION_SECRET, public origin, optional SSO) and a Caddy TLS front."
    fi
  fi
fi

# ── scheduled database backups (systemd timer; --no-backup-timer opts out) ──
# fleet ships the units in deploy/ (so they are version-controlled and covered by
# doctor's unit-drift check) and installs them here: bootstrap already owns the
# other units, the firewall and the Postgres cluster, and a box that was never
# told about backups is exactly the box that has none. Idempotent like the rest:
# an already-installed unit is left alone (doctor owns drift) and `enable --now`
# on a live timer is a no-op. Runs after the binary install above because the
# unit's ExecStart is /usr/local/bin/fleet.
if [[ "$ENABLE_SERVICE" == "1" && "$ENABLE_BACKUP_TIMER" == "1" ]]; then
  step "Scheduled database backups (daily fleet-backup.timer → ${BACKUP_DIR})"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would create ${BACKUP_DIR} if missing (0700 root-owned — a dump holds every conversation, task and user row)"
    info "[dry-run] would set FLEET_BACKUP_DIR + FLEET_BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS} in ${ENV_FILE}"
    info "[dry-run] would install deploy/fleet-backup.service + deploy/fleet-backup.timer → /etc/systemd/system"
    info "[dry-run] would run: systemctl enable --now fleet-backup.timer (daily 02:00; --no-backup-timer opts out)"
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping the backup timer (schedule 'fleet backup --db=all --prune' with cron instead)."
  else
    if [[ -d "$BACKUP_DIR" ]]; then
      # Never re-mode a directory that already exists: FLEET_BACKUP_DIR may point
      # at a shared mount whose permissions are the operator's call. Say so
      # instead — and note the unit's UMask=0077 keeps the dump FILES owner-only
      # regardless of the directory.
      _backup_mode="$(stat -c '%a' "$BACKUP_DIR" 2>/dev/null || echo unknown)"
      if [[ "$_backup_mode" == "700" ]]; then
        info "${BACKUP_DIR} present (0700)"
      else
        warn "${BACKUP_DIR} is mode ${_backup_mode}, not 0700 — dumps hold every conversation, task and user row; leaving it as you set it."
      fi
      unset _backup_mode
    else
      install -d -o root -g root -m 0700 "$BACKUP_DIR"
      ok "created ${BACKUP_DIR} (0700 root-owned — a dump holds every conversation, task and user row)"
    fi
    upsert_env FLEET_BACKUP_DIR "$BACKUP_DIR"
    upsert_env FLEET_BACKUP_RETENTION_DAYS "$BACKUP_RETENTION_DAYS"
    for unit in fleet-backup.service fleet-backup.timer; do
      if [[ -f "$REPO_ROOT/deploy/$unit" ]] && ! systemctl cat "$unit" >/dev/null 2>&1; then
        install -D -m 0644 "$REPO_ROOT/deploy/$unit" "/etc/systemd/system/$unit"
        info "installed /etc/systemd/system/$unit"
      fi
    done
    systemctl daemon-reload || warn "systemctl daemon-reload failed"
    if systemctl enable --now fleet-backup.timer >/dev/null 2>&1; then
      ok "fleet-backup.timer enabled (daily 02:00 → ${BACKUP_DIR}, ${BACKUP_RETENTION_DAYS}-day retention)"
    else
      warn "could not enable fleet-backup.timer — check: systemctl status fleet-backup.timer"
    fi
  fi
elif [[ "$ENABLE_SERVICE" == "1" ]]; then
  info "--no-backup-timer: no scheduled backup on this box. Back up at the volume/hypervisor layer, or schedule 'fleet backup --db=all --prune' yourself (docs/BACKUP_RESTORE.md). 'fleet doctor' will keep advising that no timer is installed."
fi

# ── host maintenance (systemd timer; --no-maintenance-timer opts out) ────────
# The fleet process reclaims its own data (chat retention, attachments,
# workspaces, worktrees) on an hourly in-process loop. The ONE thing that loop
# deliberately leaves alone is podman's image store: every sandbox rebuild
# strands the previous ~1.3 GB image's layers, and a whole-store prune belongs
# to an operator-scheduled window, not to a goroutine inside the serving
# process. Same install discipline as the backup timer above — version-
# controlled units in deploy/, idempotent install, doctor owns drift.
if [[ "$ENABLE_SERVICE" == "1" && "$ENABLE_MAINTENANCE_TIMER" == "1" ]]; then
  step "Host maintenance (daily fleet-maintenance.timer → podman layer + build cache prune)"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would install deploy/fleet-maintenance.service + deploy/fleet-maintenance.timer → /etc/systemd/system"
    info "[dry-run] would run: systemctl enable --now fleet-maintenance.timer (daily 03:30; --no-maintenance-timer opts out)"
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping the maintenance timer (schedule 'fleet cleanup' with cron instead)."
  else
    for unit in fleet-maintenance.service fleet-maintenance.timer; do
      if [[ -f "$REPO_ROOT/deploy/$unit" ]] && ! systemctl cat "$unit" >/dev/null 2>&1; then
        install -D -m 0644 "$REPO_ROOT/deploy/$unit" "/etc/systemd/system/$unit"
        info "installed /etc/systemd/system/$unit"
      fi
    done
    systemctl daemon-reload || warn "systemctl daemon-reload failed"
    if systemctl enable --now fleet-maintenance.timer >/dev/null 2>&1; then
      ok "fleet-maintenance.timer enabled (daily 03:30 — prunes dangling podman layers + Go build caches)"
    else
      warn "could not enable fleet-maintenance.timer — check: systemctl status fleet-maintenance.timer"
    fi
  fi
elif [[ "$ENABLE_SERVICE" == "1" ]]; then
  info "--no-maintenance-timer: no scheduled host maintenance on this box. Stale sandbox image layers will accumulate — run 'sudo fleet cleanup' periodically (docs/MAINTENANCE.md). The process's own hourly data sweep still runs."
fi

# ── web tier + Caddy TLS (opt-in via --enable-web / --domain) ──
if [[ "$ENABLE_WEB" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    step "Web tier (--enable-web): plan"
    info "[dry-run] would ensure FLEET_SERVER_TOKEN + ADMIN_API_KEY in ${ENV_FILE} (generate-if-absent) + reload backend."
    if [[ -n "$WEB_DOMAIN" ]]; then
      info "[dry-run] would set APP_SESSION_SECRET (if absent) + FLEET_PUBLIC_BASE_URL=https://${WEB_DOMAIN} in ${ENV_FILE}, restart ${SERVICE_NAME} (it serves the web UI), install Caddy + open 80/443."
      info "[dry-run] would write /etc/caddy/Caddyfile from scripts/lib/caddyfile.sh: https://${WEB_DOMAIN} → web tier (127.0.0.1:3000); /v1/*, /api-info, /.well-known/agent-card.json, /a2a, /triggers/* → orchestrator (127.0.0.1:8000); /webhooks/* → chat (127.0.0.1:8080); then reload caddy (an already-running caddy is reloaded, not just enabled)."
      if ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then
        info "[dry-run] would NOT write FLEET_TRUSTED_PROXIES: Caddy proxies to a non-loopback backend (FLEET_SERVER_ADDR / FLEET_ORCHESTRATOR_ADDR), so set it to the address Caddy connects from"
      elif [[ "$(caddy_trusted_proxies_state "$ENV_FILE")" == "unset" ]]; then
        info "[dry-run] would set FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES} in ${ENV_FILE} (Caddy is the loopback proxy in front of both backends) + restart ${SERVICE_NAME}"
      else
        info "[dry-run] FLEET_TRUSTED_PROXIES already set in ${ENV_FILE} — would leave it"
      fi
      if caddyfile_is_package_default; then
        info "[dry-run] would replace the caddy package's stock /etc/caddy/Caddyfile (unmodified default, no drop-in sites) — a copy kept beside it"
      fi
    else
      info "[dry-run] would set APP_SESSION_SECRET (if absent) + FLEET_PUBLIC_BASE_URL=http://localhost:3000 in ${ENV_FILE}, restart ${SERVICE_NAME} (it serves the web UI; loopback only; no --domain → no Caddy)."
    fi
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-web (no systemd on this box)."
  else
    configure_web_tier
  fi
fi

# ── register admin users (interactive; or --admin email[,email...]) ─────────
# One `fleet admin add` per email provisions the FULL admin across both user
# planes (web login + chat-admin + Background tasks; passwords prompted hidden
# by the binary). Needs the service RUNNING first: the users tables exist only
# after each service's self-migration, and Type=notify holds "active" until
# READY=1 — which fires after migrations — so wait-for-active is the precise
# "DBs are migrated" signal.
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Admin users"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would wait for ${SERVICE_NAME} to be active, then register admins (--admin or interactive prompt) via: fleet admin add <email>"
  elif [[ ! -t 0 && -z "$ADMIN_EMAILS_ARG" ]]; then
    info "non-interactive and no --admin — register admins later with: fleet admin add <email>"
  elif [[ ! -t 0 ]]; then
    warn "--admin needs an interactive terminal for the password prompts — register later with: fleet admin add <email>"
  elif ! command -v systemctl >/dev/null 2>&1 || ! systemctl cat "${SERVICE_NAME}.service" >/dev/null 2>&1; then
    info "${SERVICE_NAME}.service not installed — register admins later with: fleet admin add <email>"
  else
    _active=0
    for _ in $(seq 1 60); do
      [[ "$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)" == "active" ]] && { _active=1; break; }
      sleep 2
    done
    if [[ "$_active" != "1" ]]; then
      warn "${SERVICE_NAME} not active yet — skipping admin registration (run fleet admin add <email> once it is up)."
    else
      _admin_emails="$ADMIN_EMAILS_ARG"
      if [[ -z "$_admin_emails" ]]; then
        printf 'Admin email(s), comma-separated (blank to skip): '
        read -r _admin_emails
      fi
      _fleet_bin="$INSTALL_DIR/fleet"; [[ -x "$_fleet_bin" ]] || _fleet_bin="$(command -v fleet || true)"
      if [[ -z "$_admin_emails" ]]; then
        info "skipped — register admins later with: fleet admin add <email>"
      elif [[ -z "$_fleet_bin" ]]; then
        warn "fleet binary not found — register admins manually: fleet admin add <email>"
      else
        IFS=',' read -ra _emails <<< "$_admin_emails"
        for _email in "${_emails[@]}"; do
          _email="$(printf '%s' "$_email" | tr -d '[:space:]')"
          [[ -n "$_email" ]] || continue
          # The binary prompts for the password (hidden, double-entry) on the
          # same TTY; DSNs are handed over explicitly so this works before the
          # operator's shell has any fleet env.
          _db_env=(DATABASE_URL="${DB_URL:-}")
          if env "${_db_env[@]}" "$_fleet_bin" admin add "$_email"; then
            ok "admin ${_email} registered (web login + Background tasks)"
          else
            warn "could not register ${_email} — retry with: fleet admin add ${_email}"
          fi
        done
      fi
    fi
  fi
fi

step "Reminders"
info "Migrations are NOT run here — each service self-migrates on first start."
info "Backups: a same-host dump protects against LOGICAL loss (bad migration, accidental delete),"
info "         not the loss of this host or volume, and it does not capture attachment/upload"
info "         files. Copy dumps offsite yourself — see docs/BACKUP_RESTORE.md."
info "Set MCP account secrets post-bootstrap: fleet mcp account set <server> <account> --secret KEY=-"
info "Check health any time:  fleet status     (also: fleet logs, fleet motd, fleet chat)"
info "Update in place later:  fleet update      (installs the newest signed release; --check is read-only)"
info "Roll back an update:    fleet release rollback   (restores the previous binary, fleet.prev)"
ok "bootstrap complete (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"
