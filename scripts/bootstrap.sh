#!/usr/bin/env bash
# scripts/bootstrap.sh — fleet DB + credential-store bootstrap for fleet.
#
# Merges chat's + moc's bootstrap into ONE script with a --postgres=local|external
# branch (default local). It provisions the ONE cluster, TWO databases (chat +
# sched) layout the unified `fleet` process expects, and ensures the 0600
# credential env file exists. It NEVER runs application migrations — each service
# self-migrates on first start (chat's advisory-lock runner; sched's
# golang-migrate).
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
# (--chat-db-name/--chat-db-user/...) or adoption (--adopt-existing-chat-db /
# --adopt-existing-sched-db, which NEVER creates or alters the role — no
# password rotation — and requires the operator to supply a working DSN).
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
#   scripts/bootstrap.sh --enable-web [--domain fleet.example.com]  # build + serve the web tier (TLS via Caddy with --domain)
#
# Explicit flags always win — each prompt is asked only when its flag was not
# given, and non-TTY runs skip every prompt (flag/default behavior unchanged).
#
# End-to-end flow (every run): ensure 0600 env file → ensure the client bundle is
# in place (--client-config) → build the sandbox image from the bundle → provision
# both chat+sched roles/databases (local) or validate DSNs (external) → write the
# resolved DSNs + FLEET_CLIENT_CONFIG_DIR into the env file → optionally enable +
# start the systemd unit → install + enable the daily database-backup and
# host-maintenance timers (--no-backup-timer / --no-maintenance-timer opt out).
#
# Branch A (local):  install + init a local cluster, create the two owner roles
#                    and two databases idempotently via psql \gexec, sslmode=disable.
# Branch B (external): skip install; validate the provided DSNs with SELECT 1 and
#                    assume the roles/dbs are pre-provisioned (opt-in superuser
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
#   --enable-web               build + deploy the Next.js web tier and enable
#                              fleet-web (implies --enable-service). Email+password
#                              login against the bundle's chat users.
#   --domain <fqdn>            with --enable-web: front it with Caddy + automatic
#                              TLS for <fqdn> (installs Caddy, opens 80/443). The
#                              Caddyfile routes the browser to the web tier and
#                              the public API (/v1/*, /api-info, the A2A agent
#                              card, /triggers/*, /webhooks/*) to the Go backends
#                              (scripts/lib/caddyfile.sh; see deploy/Caddyfile).
#   --admin <email[,email...]> register these emails as full admins (web login +
#                              chat-admin + Operations Center) at the end of an
#                              --enable-service run; passwords are prompted per
#                              email (hidden). Interactive runs are prompted for
#                              the list too, so the flag is optional.
#   --auth-pubkey <val|@file>  enable Elcano SSO in the web tier: the
#                              AUTH_SIGNING_PUBKEY value (or @file containing it;
#                              both accept the `auth pubkey` output line
#                              verbatim). Validated as a 32-byte Ed25519 key.
#                              Interactive --enable-web runs are prompted.
#   --chat-db-name <name>      chat database name (default chat; same as env
#                              CHAT_DB_NAME — the flag wins). Use a fresh name
#                              (e.g. fleet_chat) when a legacy app already owns
#                              the default.
#   --chat-db-user <name>      chat owner role (default chat; = CHAT_DB_USER).
#   --sched-db-name <name>     sched database name (default sched; = SCHED_DB_NAME).
#   --sched-db-user <name>     sched owner role (default sched; = SCHED_DB_USER).
#   --adopt-existing-chat-db   local mode: do NOT create/alter the chat role or
#                              database (no password rotation, ever). Use the
#                              DSN the operator supplies in FLEET_CHAT_DATABASE_URL
#                              (env or already in the env file) verbatim; it is
#                              validated with SELECT 1. fleet will run ITS OWN
#                              migrations on that database at first start — only
#                              adopt a database that is (or is meant to become)
#                              fleet's. The supported legacy-data path is
#                              export → import (docs/LEGACY-IMPORT.md).
#   --adopt-existing-sched-db  same, for the sched database
#                              (FLEET_SCHED_DATABASE_URL).
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
#                              than truncating an existing Caddy config.
#   --dry-run                  print the plan; touch nothing.
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
#   CHAT_DB_NAME            chat database name (default chat)
#   CHAT_DB_USER            chat owner role  (default chat)
#   CHAT_DB_PASSWORD        chat role password (local: generated if unset)
#   SCHED_DB_NAME           sched database name (default sched)
#   SCHED_DB_USER           sched owner role (default sched)
#   SCHED_DB_PASSWORD       sched role password (local: generated if unset)
#   FLEET_CHAT_DATABASE_URL external chat DSN (external mode; also the adopted
#                           DSN under --adopt-existing-chat-db)
#   FLEET_SCHED_DATABASE_URL external sched DSN (external mode; also the adopted
#                           DSN under --adopt-existing-sched-db)
#   FLEET_DB_SUPERUSER_URL  external superuser DSN for opt-in role/db creation
#   FLEET_BACKUP_DIR        absolute directory the backup timer writes dumps to
#                           (default /var/backups/fleet; created 0700 root-owned —
#                           a dump holds every conversation, task and user row)
#   FLEET_BACKUP_RETENTION_DAYS  the timer's --prune cutoff: dumps older than this
#                           many days are deleted after a successful run (default 30)
#   FLEET_WEB_APP_NAME      web UI app name (--enable-web; default Fleet)
#   FLEET_ACME_EMAIL        Let's Encrypt account email for Caddy (--domain; optional)
set -euo pipefail

# Resolve this script's repo root so --enable-service can build + install the
# binary and unit files regardless of the caller's cwd (fleet-admin invokes it
# from elsewhere). The DB/env/bundle steps still use repo-relative defaults.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# The fleet-managed Caddyfile: marker, renderer, foreign-file detection. ONE
# implementation shared with update.sh + doctor.sh so the TLS front's routing
# cannot drift between the script that writes it and the ones that check it.
# shellcheck source=lib/caddyfile.sh
. "$SCRIPT_DIR/lib/caddyfile.sh"

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
# DB safety knobs (see the header): adoption skips provisioning for that pair
# (no CREATE, no ALTER — a pre-existing role's password is NEVER rotated) and
# takes the operator's DSN verbatim; the *_ARG name/user flags override the
# CHAT_DB_*/SCHED_DB_* env defaults so a colliding legacy name can be dodged
# without env archaeology.
ADOPT_CHAT_DB=0
ADOPT_SCHED_DB=0
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
CHAT_DB_NAME_ARG=""
CHAT_DB_USER_ARG=""
SCHED_DB_NAME_ARG=""
SCHED_DB_USER_ARG=""

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
    --chat-db-name)      shift; [[ $# -gt 0 ]] || { echo "error: --chat-db-name needs a name" >&2; exit 1; }; CHAT_DB_NAME_ARG="$1" ;;
    --chat-db-name=*)    CHAT_DB_NAME_ARG="${1#*=}" ;;
    --chat-db-user)      shift; [[ $# -gt 0 ]] || { echo "error: --chat-db-user needs a name" >&2; exit 1; }; CHAT_DB_USER_ARG="$1" ;;
    --chat-db-user=*)    CHAT_DB_USER_ARG="${1#*=}" ;;
    --sched-db-name)     shift; [[ $# -gt 0 ]] || { echo "error: --sched-db-name needs a name" >&2; exit 1; }; SCHED_DB_NAME_ARG="$1" ;;
    --sched-db-name=*)   SCHED_DB_NAME_ARG="${1#*=}" ;;
    --sched-db-user)     shift; [[ $# -gt 0 ]] || { echo "error: --sched-db-user needs a name" >&2; exit 1; }; SCHED_DB_USER_ARG="$1" ;;
    --sched-db-user=*)   SCHED_DB_USER_ARG="${1#*=}" ;;
    --adopt-existing-chat-db)  ADOPT_CHAT_DB=1 ;;
    --adopt-existing-sched-db) ADOPT_SCHED_DB=1 ;;
    --no-backup-timer)   ENABLE_BACKUP_TIMER=0 ;;
    --no-maintenance-timer) ENABLE_MAINTENANCE_TIMER=0 ;;
    --force-caddy)       FORCE_CADDY=1 ;;
    --dry-run)           DRY_RUN=1 ;;
    -h|--help)
      # Derived, not a hardcoded range: print the header comment block and stop
      # at the first non-`#` line. A fixed range silently truncates the help the
      # moment the header grows — which had already dropped the last 6 lines
      # here, and shipped raw shell code out of fleet-upgrade.sh's --help.
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
    printf 'Deploy the web UI (Next.js chat + Operations Center)? [Y/n] '
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
# DB names/roles: flag > env > default. The values are interpolated into the
# provisioning SQL below, so restrict them to plain identifiers.
CHAT_DB_NAME="${CHAT_DB_NAME_ARG:-${CHAT_DB_NAME:-chat}}"
CHAT_DB_USER="${CHAT_DB_USER_ARG:-${CHAT_DB_USER:-chat}}"
SCHED_DB_NAME="${SCHED_DB_NAME_ARG:-${SCHED_DB_NAME:-sched}}"
SCHED_DB_USER="${SCHED_DB_USER_ARG:-${SCHED_DB_USER:-sched}}"
for _ident in "$CHAT_DB_NAME" "$CHAT_DB_USER" "$SCHED_DB_NAME" "$SCHED_DB_USER"; do
  [[ "$_ident" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] \
    || die "database/role name '${_ident}' must be a plain identifier ([A-Za-z_][A-Za-z0-9_]*)"
done
unset _ident
# Adoption only makes sense against the locally provisioned cluster; external
# mode ALREADY takes the operator's DSNs verbatim and never creates or alters
# roles (opt-in FLEET_DB_SUPERUSER_URL creates missing DATABASES only).
if [[ "$POSTGRES_MODE" == "external" && ( "$ADOPT_CHAT_DB" == "1" || "$ADOPT_SCHED_DB" == "1" ) ]]; then
  die "--adopt-existing-*-db applies to --postgres=local; external mode always uses your DSNs and never touches roles"
fi
# Adoption needs the operator's DSN up front (process env, or already recorded
# in the env file). Fail in the first second, not after a multi-minute install.
ADOPTED_CHAT_URL=""
ADOPTED_SCHED_URL=""
if [[ "$ADOPT_CHAT_DB" == "1" ]]; then
  ADOPTED_CHAT_URL="${FLEET_CHAT_DATABASE_URL:-$(grep '^FLEET_CHAT_DATABASE_URL=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)}"
  [[ -n "$ADOPTED_CHAT_URL" ]] \
    || die "--adopt-existing-chat-db needs the existing database's working DSN: set FLEET_CHAT_DATABASE_URL (env, or already present in ${ENV_FILE}). This script never creates or alters an adopted role — supply its CURRENT password."
fi
if [[ "$ADOPT_SCHED_DB" == "1" ]]; then
  ADOPTED_SCHED_URL="${FLEET_SCHED_DATABASE_URL:-$(grep '^FLEET_SCHED_DATABASE_URL=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)}"
  [[ -n "$ADOPTED_SCHED_URL" ]] \
    || die "--adopt-existing-sched-db needs the existing database's working DSN: set FLEET_SCHED_DATABASE_URL (env, or already present in ${ENV_FILE}). This script never creates or alters an adopted role — supply its CURRENT password."
fi

gen_pass() { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── Caddyfile ownership marker ──
# The Caddyfile this script writes carries this marker as its first line. An
# existing /etc/caddy/Caddyfile WITHOUT it belongs to someone else (a legacy
# chat/moc deploy, a hand-rolled config) — overwriting it would silently
# destroy their vhosts, so we refuse unless --force-caddy, and even then keep a
# timestamped backup. Checked fail-fast here (before any provisioning work) and
# again at write time inside deploy_web_tier.
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
# internal/creds.SetEnvKey). No-ops under --dry-run. Values are written verbatim;
# DSNs may contain '&'/'/' so we avoid sed substitution and rewrite via awk.
upsert_env_file() {
  local file="$1" key="$2" value="$3"
  if [[ "$DRY_RUN" == "1" ]]; then
    # Never echo secret values in the plan — show the key only.
    info "[dry-run] would set ${key}=… in ${file}"
    return 0
  fi
  [[ -f "$file" ]] || install -D -m 0600 /dev/null "$file"
  local tmp
  tmp="$(mktemp "${file}.XXXXXX")"
  KEY="$key" VALUE="$value" awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
    {
      line = $0
      eq = index(line, "=")
      if (!done && eq > 0 && substr(line, 1, eq - 1) == k) {
        print k "=" v
        done = 1
        next
      }
      print line
    }
    END { if (!done) print k "=" v }
  ' "$file" > "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$file"
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

# deploy_web_tier — build + run the Next.js web tier, and (with --domain) front it
# with Caddy TLS. Self-contained email+password login against the bundle's chat
# users (fleet admin add / fleet chat user add ...); the external Elcano SSO turns
# on when AUTH_SIGNING_PUBKEY is provided (--auth-pubkey, an interactive paste, or
# carried over from a previous run's fleet-web.env).
deploy_web_tier() {
  local web_src="$REPO_ROOT/web" web_dst="/opt/fleet/web" web_env="/etc/fleet/fleet-web.env"
  local origin app_name build_id
  [[ -n "$WEB_DOMAIN" ]] && origin="https://$WEB_DOMAIN" || origin="http://localhost:3000"
  app_name="${FLEET_WEB_APP_NAME:-Fleet}"
  build_id="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo prod)"

  [[ -d "$web_src" ]] || { warn "no web/ in the checkout — skipping web tier."; return; }
  command -v npm >/dev/null 2>&1 || { warn "npm not found — skipping web tier (install nodejs)."; return; }

  # ExecStart points at this shim (it resolves the node interpreter and execs
  # next — see deploy/fleet-web-start.sh). Installed FIRST, before any of the
  # early `return`s below: on a fresh box the unit is written unconditionally
  # later in this script, so if the build fails and we bail before installing
  # the shim, systemd is left with an ExecStart that does not exist — and with
  # Restart=always/RestartSec=5s that is a permanent 203/EXEC restart loop.
  # The old ExecStart (/usr/bin/node) could not fail this way because the path
  # always existed. Shipped content, no operator-tunable parts, so overwriting
  # is safe and unconditional.
  if [[ -f "$REPO_ROOT/deploy/fleet-web-start.sh" ]]; then
    install -D -m 0755 "$REPO_ROOT/deploy/fleet-web-start.sh" /usr/local/bin/fleet-web-start.sh \
      && ok "installed /usr/local/bin/fleet-web-start.sh" \
      || warn "could not install fleet-web-start.sh — fleet-web will not start without it"
  fi

  # Resolve the interpreter the unit will run BEFORE building, and fail loudly
  # rather than building against one node and serving with another.
  local node_bin node_ver npm_cli
  if node_bin="$(fleet_resolve_node_bin "$NODE_MAJOR")"; then
    node_ver="$("$node_bin" -v 2>/dev/null || echo unknown)"
    ok "web tier will run ${node_bin} (${node_ver})"
  else
    warn "no node >= $NODE_MAJOR found (web/.nvmrc) — the web tier needs one."
    warn "  install it: sudo dnf install nodejs$NODE_MAJOR   (then re-run, or: sudo fleet doctor)"
    warn "  skipping the web tier rather than building it against an unsupported node."
    return
  fi
  # And the npm that belongs to it, separately: on Fedora npm is its own package
  # and its shebang names its interpreter ABSOLUTELY, so a resolved node 24 says
  # nothing about which node `npm ci` will run under. FLEET_DEPS asks for
  # nodejs${NODE_MAJOR}-npm, so a miss here means that install did not take.
  #
  # A miss is fatal only for a VERSIONED interpreter — the parallel-stream case,
  # where the bare `npm` provably belongs to another one and the missing package
  # has a name. On a single-node layout the same miss just means npm ships in a
  # shape the probe cannot read, and there the `node` pin below is enough; the
  # read-back still reports what npm ran under either way.
  if npm_cli="$(fleet_resolve_npm_cli "$node_bin")"; then
    ok "web tier will build with ${npm_cli} — pinned to ${node_bin}, not to PATH"
  elif [[ "${node_bin##*/}" =~ ^node-[0-9]+$ ]]; then
    warn "no npm belongs to ${node_bin} — the bare \`npm\` on this box is the default stream's and its shebang pins it there."
    warn "  install it: sudo dnf install nodejs${NODE_MAJOR}-npm   (then re-run, or: sudo fleet doctor --node)"
    warn "  skipping the web tier rather than building it with an npm pinned to an unsupported node."
    return
  else
    info "no npm-cli.js resolvable for ${node_bin} — the build will use \`npm\` from PATH under a pinned \`node\`"
  fi

  # Shared secrets the web↔backend link needs; generate-if-absent then load them
  # into the already-started backend.
  step "Web tier: ensuring shared secrets in ${ENV_FILE} + reloading backend"
  ensure_env_secret FLEET_SERVER_TOKEN 32
  ensure_env_secret ADMIN_API_KEY 32
  # Remote-MCP OAuth callbacks are registered by the backend, not Next.js. Keep
  # its stable callback origin identical to the public origin compiled into the
  # web tier; otherwise --domain deployments can accidentally retain localhost.
  upsert_env FLEET_PUBLIC_BASE_URL "$origin"
  ensure_env_b64_key FLEET_MCP_OAUTH_ENCRYPTION_KEY 32
  systemctl try-restart "$SERVICE_NAME" >/dev/null 2>&1 || true

  step "Web tier: building the Next.js app (origin=${origin}, node=${node_ver})"
  # PATH is set so `node`, `npm` and `npx` all resolve to the interpreter we
  # picked. A `node` symlink alone was not enough: npm's shebang is an ABSOLUTE
  # `#!/usr/bin/node-<major>` on Fedora, so `npm ci` kept using the DEFAULT
  # stream and the tier was built on 22 and served on 24 while this function
  # printed that it would run 24 — exactly the claimed-but-not-done failure this
  # code exists to remove. Resolved ONCE and reused, so the shim directory it
  # may create is removed rather than leaked on every provision.
  local build_path build_node
  build_path="$(fleet_node_build_path "$node_bin")"
  # Read the interpreter back from npm, not from `node -v` under the same PATH:
  # that symlink is ours, so it could only ever confirm the half that already
  # worked. Below the floor is a refusal, not a warning — building the tier on
  # the old major is the bug, and npm says so itself with EBADENGINE.
  build_node="$(fleet_npm_node_version "$build_path" "$web_src" || true)"
  if [[ -z "$build_node" ]]; then
    warn "could not read the node version npm runs under — building anyway, but the interpreter is UNVERIFIED"
    build_node="unverified"
  elif [[ "$(fleet_node_version_major "$build_node" || echo 0)" -lt "$NODE_MAJOR" ]]; then
    fleet_node_build_path_cleanup "$build_path" "$node_bin"
    warn "npm would build the web tier on ${build_node}, below the ${NODE_MAJOR} declared in web/.nvmrc."
    warn "  install it: sudo dnf install nodejs${NODE_MAJOR}-npm   (then re-run, or: sudo fleet doctor --node)"
    warn "  skipping the web tier rather than building it with an npm pinned to an unsupported node."
    return
  fi
  if ( cd "$web_src" && PATH="$build_path" \
        NEXT_PUBLIC_PUBLIC_ORIGIN="$origin" NEXT_PUBLIC_APP_NAME="$app_name" \
        NEXT_PUBLIC_BUILD_ID="$build_id" sh -c 'npm ci && npm run build' ); then
    ok "web app built on ${build_node}"
    fleet_node_build_path_cleanup "$build_path" "$node_bin"
  else
    fleet_node_build_path_cleanup "$build_path" "$node_bin"
    warn "web build failed — skipping the rest of the web tier."; return
  fi

  install -d "$web_dst" && cp -a "$web_src/." "$web_dst/" && ok "deployed web app → ${web_dst}"


  # fleet-web.service runs as the dedicated unprivileged fleet-web user (it is
  # the public-facing process). Create the user idempotently and hand it the
  # one path next writes at runtime (.next/ — the runtime cache); everything
  # else stays root-owned and read-only to the unit.
  if ! id -u fleet-web >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
      useradd --system --home-dir /var/lib/fleet-web --shell /usr/sbin/nologin \
        --no-create-home fleet-web && ok "created system user fleet-web"
    else
      warn "useradd not found — create the fleet-web user manually (see deploy/fleet-web.service)."
    fi
  fi
  if id -u fleet-web >/dev/null 2>&1 && [[ -d "$web_dst/.next" ]]; then
    chown -R fleet-web:fleet-web "$web_dst/.next" || warn "chown ${web_dst}/.next to fleet-web failed"
  fi

  # Write the 0600 web env by UPSERTING each key — never truncate/rewrite the
  # whole file: it may carry operator-added keys beyond the bootstrap set (e.g.
  # AUTH_SIGNING_PUBKEY / AUTH_LOGIN_URL / AUTH_COOKIE_DOMAIN for the external
  # SSO), and a wholesale rewrite silently dropped them on every re-run.
  # Chat/orchestrator tokens mirror the backend env; APP_SESSION_SECRET is
  # generate-if-absent (rotating it logs everyone out).
  local chat_token app_secret
  chat_token="$(grep '^FLEET_SERVER_TOKEN=' "$ENV_FILE" 2>/dev/null | cut -d= -f2-)"
  if [[ -f "$web_env" ]] && grep -q '^APP_SESSION_SECRET=' "$web_env"; then
    app_secret="$(grep '^APP_SESSION_SECRET=' "$web_env" | cut -d= -f2-)"
  else
    app_secret="$(gen_secret 48)"
  fi
  upsert_env_file "$web_env" CHAT_SERVER_URL           "http://127.0.0.1:8080"
  upsert_env_file "$web_env" CHAT_SERVER_TOKEN         "$chat_token"
  upsert_env_file "$web_env" ORCHESTRATOR_SERVER_URL   "http://127.0.0.1:8000"
  # ORCHESTRATOR_SERVER_TOKEN must be the SAME shared secret as
  # CHAT_SERVER_TOKEN (FLEET_SERVER_TOKEN): the orchestrator's header-trust
  # path (AdminOrUserAuthMiddleware) verifies X-Orchestrator-Server-Token
  # against Config.SharedToken, fail-closed. This line used to write
  # ADMIN_API_KEY here, which the middleware never accepts on that header —
  # every bootstrap re-run then 403'd the whole Operations Center for
  # cookie-authenticated users until the env file was hand-repaired.
  upsert_env_file "$web_env" ORCHESTRATOR_SERVER_TOKEN "$chat_token"
  upsert_env_file "$web_env" APP_SESSION_SECRET        "$app_secret"
  upsert_env_file "$web_env" PORT                      "3000"
  upsert_env_file "$web_env" NODE_ENV                  "production"
  # The interpreter resolved above, pinned explicitly. Without this the shim
  # falls back to `node` on PATH, which on Fedora is the DEFAULT stream — i.e.
  # possibly an older major than the one just installed.
  upsert_env_file "$web_env" FLEET_NODE_BIN            "$node_bin"
  upsert_env_file "$web_env" NEXT_PUBLIC_PUBLIC_ORIGIN "$origin"
  upsert_env_file "$web_env" NEXT_PUBLIC_APP_NAME      "$app_name"
  upsert_env_file "$web_env" NEXT_PUBLIC_BUILD_ID      "$build_id"

  # Elcano SSO (AUTH_SIGNING_PUBKEY): --auth-pubkey wins (value or @file); else
  # an existing key is simply LEFT IN PLACE (the upsert-style writes above never
  # truncate the file, so operator-added AUTH_* keys survive re-runs on their
  # own — no carry-over machinery needed); else offer an interactive paste.
  # Every path that WRITES validates via normalize_auth_pubkey — a bad key DIES
  # rather than deploying a web tier whose SSO fails on every login.
  local auth_raw="" auth_pubkey=""
  if [[ -n "$AUTH_PUBKEY_ARG" ]]; then
    auth_raw="$AUTH_PUBKEY_ARG"
    if [[ "$auth_raw" == @* ]]; then
      auth_raw="$(cat "${auth_raw#@}")" || die "--auth-pubkey: cannot read ${AUTH_PUBKEY_ARG#@}"
    fi
    auth_pubkey="$(normalize_auth_pubkey "$auth_raw")" \
      || die "--auth-pubkey is not a valid AUTH_SIGNING_PUBKEY (want the standard-base64 32-byte Ed25519 key from \`auth pubkey\`)"
    upsert_env_file "$web_env" AUTH_SIGNING_PUBKEY "$auth_pubkey"
    ok "Elcano SSO key accepted (--auth-pubkey)"
  elif grep -q '^AUTH_SIGNING_PUBKEY=' "$web_env" 2>/dev/null; then
    info "existing AUTH_SIGNING_PUBKEY left in place (Elcano SSO stays enabled)."
  elif [[ -t 0 ]]; then
    printf 'Optional Elcano SSO key — email+password login works without it (paste the `auth pubkey` line, or Enter to skip): '
    read -r auth_raw
    if [[ -n "$auth_raw" ]]; then
      auth_pubkey="$(normalize_auth_pubkey "$auth_raw")" \
        || die "that is not a valid AUTH_SIGNING_PUBKEY (want the standard-base64 32-byte Ed25519 key from \`auth pubkey\`)"
      upsert_env_file "$web_env" AUTH_SIGNING_PUBKEY "$auth_pubkey"
      ok "Elcano SSO key accepted"
    else
      info "skipped — enable SSO later with: fleet config set-auth-pubkey"
    fi
  fi

  if grep -q '^AUTH_SIGNING_PUBKEY=' "$web_env" 2>/dev/null; then
    ok "wrote ${web_env} (0600; operator-added keys preserved; Elcano SSO enabled)"
  else
    ok "wrote ${web_env} (0600; operator-added keys preserved; email+password login only)"
  fi

  systemctl daemon-reload || true
  if systemctl enable --now fleet-web >/dev/null 2>&1; then
    ok "fleet-web enabled + started (Next.js on 127.0.0.1:3000)"
  else
    warn "could not enable/start fleet-web — check: journalctl -u fleet-web -n 50"
  fi

  if [[ -z "$WEB_DOMAIN" ]]; then
    # The start script binds 127.0.0.1 unless FLEET_WEB_HOST overrides it, so
    # "loopback-only" holds even on boxes without firewalld. EVERY custom proxy
    # front — same host or not — needs the real public origin: it decides the
    # web tier's redirect targets and Secure-cookie flag (web/src/app/lib/
    # auth.ts), and Next inlines NEXT_PUBLIC_* at build time, so editing the
    # env file alone changes nothing until web/ is rebuilt. Only a proxy on
    # ANOTHER host additionally needs the wider bind.
    info "no --domain → web is loopback-only on :3000; front it with your own TLS proxy for a public URL."
    info "  any custom proxy (same host too): set NEXT_PUBLIC_PUBLIC_ORIGIN=https://<domain> in ${web_env}, then rebuild web/ + restart fleet-web (scripts/update.sh does both) — the origin is baked in at build time, so editing the env file alone is not enough."
    info "  proxy on ANOTHER host: additionally set FLEET_WEB_HOST=0.0.0.0 in ${web_env} (the web tier binds 127.0.0.1 otherwise)."
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
  if caddyfile_is_foreign; then
    if [[ "$FORCE_CADDY" != "1" ]]; then
      die "refusing to overwrite /etc/caddy/Caddyfile (not written by this script) — merge manually or re-run with --force-caddy"
    fi
    local caddy_backup
    caddy_backup="/etc/caddy/Caddyfile.fleet-backup.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p /etc/caddy/Caddyfile "$caddy_backup" || die "could not back up /etc/caddy/Caddyfile to ${caddy_backup}"
    warn "--force-caddy: OVERWRITING /etc/caddy/Caddyfile — previous config saved to ${caddy_backup}"
    warn "any other sites/vhosts it served are DOWN until you merge them back in and reload caddy."
  fi
  # One renderer (scripts/lib/caddyfile.sh) writes the whole file: the web
  # tier as the default upstream, the public API (/v1/*, /api-info, the A2A
  # agent card, /triggers/*) to the orchestrator and /webhooks/* to the chat
  # listener, with the Next-proxy header-trust channel stripped on both. The
  # upstream addresses follow the env file when an operator moved a listener.
  render_fleet_caddyfile "$WEB_DOMAIN" "${FLEET_ACME_EMAIL:-}" \
    "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)" \
    > /etc/caddy/Caddyfile
  if command -v caddy >/dev/null 2>&1 && ! caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
    warn "caddy validate rejected the rendered /etc/caddy/Caddyfile — check: caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile"
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
# multi-minute npm build.
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

# ── system dependencies: the build + runtime + sandbox toolchain ──
# So `git clone … && bash scripts/bootstrap.sh …` provisions a BARE box end to
# end (the chat/moc experience): Go (build the binary), Node (build/run the web
# app), podman (the execution sandbox), python3 + pip (host-side Python MCP
# servers), plus git/curl/jq/gcc. Postgres-server is installed per-mode below
# (local only). Non-Fedora hosts: install these yourself, then re-run.
#
# slirp4netns is here because FLEET_DEFAULT_NETWORK_MODE=allowlisted requires
# it: podman >= 5.0 defaults to pasta and a stock modern host ships pasta
# WITHOUT slirp4netns, which makes every allowlisted container start fail
# (fleet now preflights that at boot and fails closed — see ADR-0012). Cheap
# to install unconditionally so the mode is available if an operator picks it.
step "Installing system dependencies (build + runtime + sandbox toolchain)"
# The node major is declared ONCE, in web/.nvmrc, and read here. CI reads the
# same file via actions/setup-node's node-version-file, so the version the box
# runs and the version CI tests cannot drift apart silently — which is exactly
# what happened before: CI pinned '22' across six jobs in four workflow files
# while the box took whatever `dnf install nodejs` happened to mean.
# shellcheck source=lib/node-version.sh
. "$SCRIPT_DIR/lib/node-version.sh"
NODE_MAJOR="$(fleet_node_major_want "$REPO_ROOT" || true)"
# No hardcoded fallback: .nvmrc can legitimately hold `lts/*` or `20.11.0`,
# which actions/setup-node understands and a naive integer check does not — and
# silently defaulting is how CI and the box diverge in the first place.
[[ -n "$NODE_MAJOR" ]] || die "cannot read the node major from ${REPO_ROOT}/web/.nvmrc — expected something like '24'"
# Ask for the VERSIONED package (nodejs24), not the unversioned `nodejs`.
# Fedora's node streams are parallel-installable and `nodejs` resolves to
# whichever the release designated default — so `dnf install nodejs` on F44
# gives 22 and can never reach 24, however many times it is re-run.
# nodejs${NODE_MAJOR}-npm, NOT bare `npm`: the unversioned npm package belongs
# to the DEFAULT stream, so asking for it drags the older interpreter onto the
# box and lets it own /usr/bin/npm — which is what would then build the app.
FLEET_DEPS=(git curl jq golang "nodejs${NODE_MAJOR}" "nodejs${NODE_MAJOR}-npm" python3 python3-pip gcc podman slirp4netns)
if command -v dnf >/dev/null 2>&1; then
  if ! run dnf install -y "${FLEET_DEPS[@]}"; then
    # A distro without a versioned stream (or one that names it differently)
    # should still get a working box; the floor is then doctor.sh's to report.
    warn "installing nodejs${NODE_MAJOR} failed — falling back to the unversioned nodejs package."
    warn "  the web tier needs node >= ${NODE_MAJOR}; \`sudo fleet doctor\` will report the shortfall."
    FLEET_DEPS=(git curl jq golang nodejs npm python3 python3-pip gcc podman slirp4netns)  # unversioned fallback
    run dnf install -y "${FLEET_DEPS[@]}" || warn "dependency install failed — install these by hand: ${FLEET_DEPS[*]}"
  fi
  [[ "$DRY_RUN" == "1" ]] || ok "system dependencies present (${FLEET_DEPS[*]})"
else
  warn "dnf not found — skipping dependency install. Ensure these are present before continuing: ${FLEET_DEPS[*]}"
  warn "  the web tier needs node >= ${NODE_MAJOR} (declared in web/.nvmrc)."
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
  if [[ "${CLIENT_CONFIG_DIR}" == "config/default" ]]; then
    info "using the GENERIC default bundle (neutral branding, no MCP connectors)."
    info "for a branded deploy, check out a client repo and set FLEET_CLIENT_CONFIG_DIR to it."
  fi
else
  warn "no manifest.yaml at ${CLIENT_CONFIG_DIR} — fleet will fail to start until"
  warn "FLEET_CLIENT_CONFIG_DIR points at a valid bundle (a dir with manifest.yaml)."
fi

# ── bundle ownership for the rootless sandbox (--enable-service path) ──
# The sandbox bind-mounts bundle dirs (protocols/ personas/ skills/ system_prompts/)
# into the container with SELinux relabeling (:Z); the rootless service user can
# only relabel files it OWNS. Chown the CHECKOUT to the service user — skip the
# in-repo default bundle (chowning the repo would be wrong, and the service can't
# read a bundle under /root anyway given ProtectHome).
if [[ "$ENABLE_SERVICE" == "1" && "$DRY_RUN" != "1" && -d "$CLIENT_CONFIG_DIR" \
      && "$CLIENT_CONFIG_DIR" != "config/default" && "$CLIENT_CONFIG_DIR" != "$REPO_ROOT"/* ]]; then
  if chown -R "$SERVICE_USER":"$SERVICE_USER" "$CLIENT_CONFIG_DIR"; then
    ok "bundle ${CLIENT_CONFIG_DIR} owned by ${SERVICE_USER} (so rootless :Z relabel is permitted)"
  else
    warn "could not chown ${CLIENT_CONFIG_DIR} to ${SERVICE_USER} — sandbox :Z relabel may fail (EPERM)."
  fi
fi

# resolve_sandbox_image MANIFEST — print the bundle's resolved sandbox.image, the
# SAME way the Go loader (internal/clientconfig) does: extract the scalar under
# the sandbox: block, then interpolate a bare ${VAR:-default} / ${VAR} reference
# against the process env. An empty result => build-on-box (the default-bundle
# value "${FLEET_SANDBOX_IMAGE:-}" resolves to empty when the var is unset).
# Mirrored in scripts/update.sh — keep the two copies in sync.
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
    local var="${BASH_REMATCH[1]}" def="${BASH_REMATCH[3]}"
    printf '%s' "${!var:-$def}"
  else
    printf '%s' "$raw"
  fi
}

# ── sandbox image (per-client bundle artifact; build-on-box default) ──
# The execution sandbox is a per-client bundle artifact: the Containerfile lives
# in the bundle at <bundle>/sandbox/Containerfile. DEFAULT = build it on this box
# (auditable supply chain). REGISTRY PUBLISH is opt-in: a client sets
# sandbox.image in its manifest to a prebuilt ref and fleet pulls/uses that
# instead — in which case skip the on-box build here.
step "Building the sandbox image from the bundle (build-on-box default)"
SANDBOX_CONTAINERFILE="${CLIENT_CONFIG_DIR}/sandbox/Containerfile"
SANDBOX_IMAGE_REF="$(resolve_sandbox_image "${CLIENT_CONFIG_DIR}/manifest.yaml")"
if [[ -n "$SANDBOX_IMAGE_REF" ]]; then
  info "manifest resolves sandbox.image=${SANDBOX_IMAGE_REF} — using a prebuilt/registry image; skipping on-box build."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: FLEET_CLIENT_CONFIG_DIR=${CLIENT_CONFIG_DIR} scripts/build-sandbox-image.sh"
elif [[ ! -f "$SANDBOX_CONTAINERFILE" ]]; then
  warn "no ${SANDBOX_CONTAINERFILE} — bundle ships no sandbox Containerfile; set sandbox.image or add one."
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — skipping sandbox build (install podman, then run scripts/build-sandbox-image.sh)."
elif [[ "$ENABLE_SERVICE" == "1" ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
  # systemd path: build INTO the service user's rootless image store, so the
  # User=fleet unit finds the image (root's rootful store is a separate namespace).
  SANDBOX_TAG="$(sandbox_manifest_tag "${CLIENT_CONFIG_DIR}/manifest.yaml")"
  info "building as ${SERVICE_USER} (rootless) → ${SANDBOX_TAG}"
  # --pull=newer matches build-sandbox-image.sh: refresh an unpinned base from
  # the registry when upstream published a newer one, instead of silently
  # reusing whatever stale copy is already in the store (offline-safe — podman
  # suppresses the pull error when a local copy of the base exists).
  if runuser -u "$SERVICE_USER" -- sh -c "cd '${SERVICE_HOME}' && HOME='${SERVICE_HOME}' XDG_RUNTIME_DIR='/run/${SERVICE_USER}' podman build --pull=newer -t '${SANDBOX_TAG}' -f '${SANDBOX_CONTAINERFILE}' '${CLIENT_CONFIG_DIR}/sandbox'"; then
    ok "sandbox image ${SANDBOX_TAG} built into ${SERVICE_USER}'s rootless store"
  else
    warn "rootless sandbox build (as ${SERVICE_USER}) failed — fleet will have no runnable sandbox image."
  fi
else
  if FLEET_CLIENT_CONFIG_DIR="${CLIENT_CONFIG_DIR}" "$(dirname "$0")/build-sandbox-image.sh"; then
    ok "sandbox image built from ${SANDBOX_CONTAINERFILE}"
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
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: python3 -m pip install --break-system-packages -r ${CLIENT_CONFIG_DIR}/mcp/requirements.txt"
  elif command -v python3 >/dev/null 2>&1; then
    if python3 -m pip install --break-system-packages -r "${CLIENT_CONFIG_DIR}/mcp/requirements.txt"; then
      ok "bundle MCP Python deps installed (host-side servers can start)"
    else
      warn "pip install of ${CLIENT_CONFIG_DIR}/mcp/requirements.txt failed — host-side MCP servers may not start."
    fi
  else
    warn "python3 not found — install ${CLIENT_CONFIG_DIR}/mcp/requirements.txt manually."
  fi
fi

# ── Postgres provisioning ──
# env_dsn_password KEY — extract the password from an existing postgres:// DSN
# already recorded in $ENV_FILE (empty when the file/key/password is absent).
# A re-run on a provisioned box MUST reuse the live password: without this, a
# freshly generated password was written into the env-file DSN while the
# CREATE ROLE below was skipped (role exists), leaving a DSN the cluster
# rejects — the "idempotent re-run" broke DB auth on the next restart.
env_dsn_password() {
  local url
  url="$(grep "^$1=" "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)"
  if [[ "$url" =~ ^postgres(ql)?://[^:/@]+:([^@/]+)@ ]]; then
    printf '%s' "${BASH_REMATCH[2]}"
  fi
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
  url="$(grep "^$1=" "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)"
  re="^postgres(ql)?://$2:[^@]*@[^/]+/$3(\?.*)?$"
  [[ "$url" =~ $re ]]
}

# guard_preexisting LABEL ROLE DB ENVKEY ADOPTFLAG NAMEFLAG — refuse to
# provision over a role/database that already exists on the local cluster but
# is NOT recorded in the env file by a previous run of this script. Without
# this, defaults colliding with a legacy install (role/db "chat") would
# silently ALTER the legacy role's password — locking out the still-installed
# legacy server and any later legacy export — and then run fleet's migrations
# on the legacy database. The operator must choose: adopt it explicitly, or
# pick fresh names.
guard_preexisting() {
  local label="$1" role="$2" db="$3" envkey="$4" adoptflag="$5" nameflag="$6"
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
    "    fresh names:  ${nameflag} fleet_${label} (and the matching --${label}-db-user; any unused names)" \
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
  CHAT_DB_PASSWORD="${CHAT_DB_PASSWORD:-$(env_dsn_password FLEET_CHAT_DATABASE_URL)}"
  CHAT_DB_PASSWORD="${CHAT_DB_PASSWORD:-$(gen_pass)}"
  SCHED_DB_PASSWORD="${SCHED_DB_PASSWORD:-$(env_dsn_password FLEET_SCHED_DATABASE_URL)}"
  SCHED_DB_PASSWORD="${SCHED_DB_PASSWORD:-$(gen_pass)}"

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

  step "Creating roles + databases idempotently (chat + sched)"
  # Refuse to touch a pre-existing role/db this script did not provision (a
  # legacy chat/moc install owning the default names is exactly the trap).
  # Adopted pairs skip both the guard and provisioning entirely.
  [[ "$ADOPT_CHAT_DB"  == "1" ]] || guard_preexisting chat  "$CHAT_DB_USER"  "$CHAT_DB_NAME"  FLEET_CHAT_DATABASE_URL  --adopt-existing-chat-db  --chat-db-name
  [[ "$ADOPT_SCHED_DB" == "1" ]] || guard_preexisting sched "$SCHED_DB_USER" "$SCHED_DB_NAME" FLEET_SCHED_DATABASE_URL --adopt-existing-sched-db --sched-db-name
  # CREATE-if-missing, then ALTER to converge the password: safe because the
  # guard above guarantees any role reaching this point either does not exist
  # yet (we create it) or was created by a previous run of this script (its DSN
  # is recorded in the env file, whose password we reuse). The ALTER therefore
  # only ever applies to roles this script itself provisioned — it can never
  # rotate a pre-existing (e.g. legacy) role's password.
  PSQL_SQL=""
  if [[ "$ADOPT_CHAT_DB" == "1" ]]; then
    info "chat: --adopt-existing-chat-db — skipping role/database provisioning (no CREATE, no ALTER, no password change)."
  else
    PSQL_SQL+=$(cat <<SQL
SELECT 'CREATE ROLE ${CHAT_DB_USER} LOGIN PASSWORD ''${CHAT_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${CHAT_DB_USER}')\gexec
ALTER ROLE ${CHAT_DB_USER} WITH LOGIN PASSWORD '${CHAT_DB_PASSWORD}';
SELECT 'CREATE DATABASE ${CHAT_DB_NAME} OWNER ${CHAT_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${CHAT_DB_NAME}')\gexec
SQL
)
    PSQL_SQL+=$'\n'
  fi
  if [[ "$ADOPT_SCHED_DB" == "1" ]]; then
    info "sched: --adopt-existing-sched-db — skipping role/database provisioning (no CREATE, no ALTER, no password change)."
  else
    PSQL_SQL+=$(cat <<SQL
SELECT 'CREATE ROLE ${SCHED_DB_USER} LOGIN PASSWORD ''${SCHED_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${SCHED_DB_USER}')\gexec
ALTER ROLE ${SCHED_DB_USER} WITH LOGIN PASSWORD '${SCHED_DB_PASSWORD}';
SELECT 'CREATE DATABASE ${SCHED_DB_NAME} OWNER ${SCHED_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${SCHED_DB_NAME}')\gexec
SQL
)
    PSQL_SQL+=$'\n'
  fi
  if [[ -z "$PSQL_SQL" ]]; then
    info "both databases adopted — nothing to provision."
  elif [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run as postgres:"
    printf '%s\n' "$PSQL_SQL" | sed 's/^/    /'
  else
    printf '%s\n' "$PSQL_SQL" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 >/dev/null \
      || die "role/database provisioning failed"
  fi

  # Resolve the DSNs: an adopted pair keeps the operator's DSN verbatim
  # (validated with SELECT 1 — with the role's CURRENT password, since we never
  # touch it); a provisioned pair gets the loopback DSN we just converged.
  if [[ "$ADOPT_CHAT_DB" == "1" ]]; then
    CHAT_URL="$ADOPTED_CHAT_URL"
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would validate the adopted chat DSN with: psql '<chat dsn>' -c 'SELECT 1'"
    else
      psql -v ON_ERROR_STOP=1 "$CHAT_URL" -c "SELECT 1" >/dev/null \
        || die "adopted chat DSN failed SELECT 1 — supply the existing role's working DSN in FLEET_CHAT_DATABASE_URL (this script never rotates its password)"
      ok "chat DB adopted (existing role/database, SELECT 1 ok; password untouched)"
    fi
  else
    CHAT_URL="postgres://${CHAT_DB_USER}:${CHAT_DB_PASSWORD}@127.0.0.1:5432/${CHAT_DB_NAME}?sslmode=${SSLMODE}"
    ok "chat DB:  ${CHAT_DB_NAME} (owner ${CHAT_DB_USER}), sslmode=${SSLMODE}"
  fi
  if [[ "$ADOPT_SCHED_DB" == "1" ]]; then
    SCHED_URL="$ADOPTED_SCHED_URL"
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would validate the adopted sched DSN with: psql '<sched dsn>' -c 'SELECT 1'"
    else
      psql -v ON_ERROR_STOP=1 "$SCHED_URL" -c "SELECT 1" >/dev/null \
        || die "adopted sched DSN failed SELECT 1 — supply the existing role's working DSN in FLEET_SCHED_DATABASE_URL (this script never rotates its password)"
      ok "sched DB adopted (existing role/database, SELECT 1 ok; password untouched)"
    fi
  else
    SCHED_URL="postgres://${SCHED_DB_USER}:${SCHED_DB_PASSWORD}@127.0.0.1:5432/${SCHED_DB_NAME}?sslmode=${SSLMODE}"
    ok "sched DB: ${SCHED_DB_NAME} (owner ${SCHED_DB_USER}), sslmode=${SSLMODE}"
  fi

else
  SSLMODE="require"
  step "Branch B (external): validate pre-provisioned DSNs (SELECT 1)"
  CHAT_URL="${FLEET_CHAT_DATABASE_URL:-}"
  SCHED_URL="${FLEET_SCHED_DATABASE_URL:-}"
  [[ -n "$CHAT_URL" ]]  || die "external mode needs FLEET_CHAT_DATABASE_URL"
  [[ -n "$SCHED_URL" ]] || die "external mode needs FLEET_SCHED_DATABASE_URL"

  # Opt-in superuser provisioning of roles/dbs.
  if [[ -n "${FLEET_DB_SUPERUSER_URL:-}" ]]; then
    step "Opt-in: provisioning roles/dbs via FLEET_DB_SUPERUSER_URL"
    SU_SQL=$(cat <<SQL
SELECT 'CREATE DATABASE ${CHAT_DB_NAME}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${CHAT_DB_NAME}')\gexec
SELECT 'CREATE DATABASE ${SCHED_DB_NAME}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${SCHED_DB_NAME}')\gexec
SQL
)
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would run superuser SQL against FLEET_DB_SUPERUSER_URL"
    else
      printf '%s\n' "$SU_SQL" | psql -v ON_ERROR_STOP=1 "$FLEET_DB_SUPERUSER_URL" >/dev/null \
        || die "superuser provisioning failed"
    fi
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: psql '<chat dsn>' -c 'SELECT 1'"
    info "[dry-run] would run: psql '<sched dsn>' -c 'SELECT 1'"
  else
    psql -v ON_ERROR_STOP=1 "$CHAT_URL"  -c "SELECT 1" >/dev/null || die "chat DSN failed SELECT 1"
    psql -v ON_ERROR_STOP=1 "$SCHED_URL" -c "SELECT 1" >/dev/null || die "sched DSN failed SELECT 1"
    ok "both external DSNs answered SELECT 1 (sslmode=${SSLMODE} expected in the DSN)"
  fi
fi

# ── write/refresh the env file (0600) ──
# Persist the resolved DSNs + the client-bundle dir so the fleet process and
# fleet-admin read them from the SAME 0600 file deploy/fleet.service EnvironmentFiles.
# Idempotent: re-running rewrites these keys in place. Local-mode passwords are
# reused from the env file's existing DSNs (or the operator's pre-set
# CHAT_DB_PASSWORD/SCHED_DB_PASSWORD), and the cluster role is ALTERed to match —
# the env file and the cluster never disagree after a run.
step "Writing connection settings into ${ENV_FILE} (0600)"
upsert_env FLEET_CHAT_DATABASE_URL "$CHAT_URL"
upsert_env FLEET_SCHED_DATABASE_URL "$SCHED_URL"
upsert_env FLEET_CLIENT_CONFIG_DIR "$CLIENT_CONFIG_DIR"
# Point config.Load at the same file so process-env and config-loaded values match.
upsert_env FLEET_ENV_FILE "$ENV_FILE"
if [[ "$DRY_RUN" != "1" ]]; then
  ok "wrote FLEET_CHAT_DATABASE_URL / FLEET_SCHED_DATABASE_URL / FLEET_CLIENT_CONFIG_DIR / FLEET_ENV_FILE"
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

# ── optionally build + install the binary + unit, then enable + start it ──
# fleet-admin bootstrap/update operate on a SOURCE CHECKOUT (this repo) and
# install the built artifacts to FLEET_INSTALL_DIR (default /opt/fleet) — the
# location deploy/fleet.service's ExecStart points at.
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Building + installing the fleet binary, then enabling ${SERVICE_NAME}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: (cd ${REPO_ROOT} && make build)  → fleet + fleet-admin"
    info "[dry-run] would install fleet + fleet-admin → ${INSTALL_DIR}"
    info "[dry-run] would symlink /usr/local/bin/fleet (+ fleet-admin) → ${INSTALL_DIR} (operator PATH)"
    info "[dry-run] would install deploy/fleet-motd.sh → /etc/profile.d/fleet-motd.sh (login banner)"
    info "[dry-run] would install deploy/fleet.service + deploy/fleet-web.service → /etc/systemd/system"
    info "[dry-run] would run: systemctl daemon-reload && systemctl enable --now ${SERVICE_NAME}"
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-service (no systemd on this box)."
  else
    # 1. Build the deployable artifacts from this checkout (needs Go on the box).
    if command -v go >/dev/null 2>&1 || command -v make >/dev/null 2>&1; then
      # GOTOOLCHAIN=auto: Fedora's `golang` lags go.mod's pinned version, so let
      # the toolchain fetch the required Go rather than failing an opaque build.
      if ( cd "$REPO_ROOT" && GOTOOLCHAIN=auto make build ) && [[ -x "$REPO_ROOT/fleet" && -x "$REPO_ROOT/fleet-admin" ]]; then
        install -D -m 0755 "$REPO_ROOT/fleet"       "$INSTALL_DIR/fleet"
        install -D -m 0755 "$REPO_ROOT/fleet-admin" "$INSTALL_DIR/fleet-admin"
        ok "installed fleet + fleet-admin → ${INSTALL_DIR}"
        # Put `fleet` on the operator's PATH (#461). deploy/fleet.service's
        # ExecStart points at $INSTALL_DIR/fleet, but an operator typing `fleet
        # status` / `fleet update` / `fleet chat` needs it on PATH too — that gap
        # was the "fleet isn't installed" symptom. Symlink (not copy) so a later
        # update of $INSTALL_DIR/fleet is reflected with no second install. The
        # fleet-admin shim is linked alongside for one deprecation release.
        if [[ -d /usr/local/bin ]] || install -d /usr/local/bin 2>/dev/null; then
          ln -sf "$INSTALL_DIR/fleet"       /usr/local/bin/fleet       && info "linked /usr/local/bin/fleet → ${INSTALL_DIR}/fleet"
          ln -sf "$INSTALL_DIR/fleet-admin" /usr/local/bin/fleet-admin || true
        fi
        # MOTD (#461): a login banner like the sibling chat repo's. profile.d runs
        # `fleet motd` (version + service state + commands; no secrets) on an
        # interactive login. Best-effort — never fail the install on it.
        if [[ -d /etc/profile.d || -w /etc ]] && [[ -f "$REPO_ROOT/deploy/fleet-motd.sh" ]]; then
          install -D -m 0644 "$REPO_ROOT/deploy/fleet-motd.sh" /etc/profile.d/fleet-motd.sh \
            && info "installed /etc/profile.d/fleet-motd.sh (login banner)" || true
        fi
      else
        die "make build failed or produced no artifacts — install Go and retry"
      fi
    elif [[ -x "$INSTALL_DIR/fleet" ]]; then
      warn "no Go toolchain — using the existing ${INSTALL_DIR}/fleet (build + install manually to update it)."
    else
      die "no Go toolchain and no binary at ${INSTALL_DIR}/fleet — install Go (or pre-build) then re-run"
    fi
    # 2. Install the unit files from this checkout if not already present.
    for unit in fleet.service fleet-web.service; do
      if [[ -f "$REPO_ROOT/deploy/$unit" ]] && ! systemctl cat "$unit" >/dev/null 2>&1; then
        install -D -m 0644 "$REPO_ROOT/deploy/$unit" "/etc/systemd/system/$unit"
        info "installed /etc/systemd/system/$unit"
      fi
    done
    # fleet-web's drop-in (TimeoutStopFailureMode=kill at the precedence level
    # that beats Fedora's global abort-on-timeout drop-in) is installed
    # SEPARATELY from the unit, not nested in the "unit was absent" branch
    # above. bootstrap is re-runnable by design, and a box provisioned before
    # this file shipped already has fleet-web.service — so gating the drop-in
    # on a fresh unit install meant the one case that needs it never got it.
    # Unlike the unit, overwriting it is safe: it is a single directive with no
    # operator-tunable content (unit drift stays doctor.sh / update.sh's job).
    web_dropin="deploy/fleet-web.service.d/10-timeout-kill.conf"
    if [[ -f "$REPO_ROOT/$web_dropin" ]] && systemctl cat fleet-web.service >/dev/null 2>&1; then
      install -D -m 0644 "$REPO_ROOT/$web_dropin" \
        /etc/systemd/system/fleet-web.service.d/10-timeout-kill.conf
      info "installed /etc/systemd/system/fleet-web.service.d/10-timeout-kill.conf"
    fi
    # 3. daemon-reload + enable the backend unit. The web unit (fleet-web)
    #    additionally needs the built Next app at /opt/fleet/web + its 0600
    #    env file, so we install it but leave enabling it to the operator.
    systemctl daemon-reload || warn "systemctl daemon-reload failed"
    # Assert the RESOLVED ExecStart, not the file we may or may not have just
    # written. The loop above installs a unit only when one is ABSENT (unit
    # drift is doctor.sh / update.sh's job, deliberately), so on an
    # already-provisioned box every piece of the node work above can land — the
    # shim, FLEET_NODE_BIN, the versioned interpreter — while fleet-web still
    # runs the OLD ExecStart pointing straight at /usr/bin/node. Reporting
    # "enabled + started" there would be the same lie this change set exists to
    # remove: a thing configured, reported, and not in effect.
    if systemctl cat fleet-web.service >/dev/null 2>&1; then
      _live_exec="$(systemctl show -p ExecStart --value fleet-web.service 2>/dev/null || true)"
      case "$_live_exec" in
        *fleet-web-start.sh*) : ;;   # shipped ExecStart is live
        *)
          warn "fleet-web's live ExecStart is not the shipped shim — the node work above is NOT in effect for the running tier."
          warn "  live: ${_live_exec:-<unknown>}"
          warn "  adopt the shipped unit: sudo fleet doctor   (or: sudo fleet update --adopt-units)"
          ;;
      esac
    fi
    if systemctl enable --now "${SERVICE_NAME}" >/dev/null 2>&1; then
      ok "${SERVICE_NAME} enabled + started (services self-migrate on start)"
    else
      warn "could not enable/start ${SERVICE_NAME} — check: journalctl -u ${SERVICE_NAME} -n 50"
    fi
    if [[ "$ENABLE_WEB" != "1" ]]; then
      info "web tier: re-run with --enable-web [--domain <fqdn>] to build + serve it,"
      info "          or by hand (cd web && npm ci && npm run build → /opt/fleet/web, fill fleet-web.env, enable fleet-web)."
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
      info "[dry-run] would build web/ for https://${WEB_DOMAIN} → /opt/fleet/web, write fleet-web.env, enable fleet-web, install Caddy + open 80/443."
      info "[dry-run] would write /etc/caddy/Caddyfile from scripts/lib/caddyfile.sh: https://${WEB_DOMAIN} → web tier (127.0.0.1:3000); /v1/*, /api-info, /.well-known/agent-card.json, /a2a, /triggers/* → orchestrator (127.0.0.1:8000); /webhooks/* → chat (127.0.0.1:8080); then reload caddy (an already-running caddy is reloaded, not just enabled)."
    else
      info "[dry-run] would build web/ for http://localhost:3000 → /opt/fleet/web, write fleet-web.env, enable fleet-web (loopback only; no --domain → no Caddy)."
    fi
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-web (no systemd on this box)."
  else
    deploy_web_tier
  fi
fi

# ── register admin users (interactive; or --admin email[,email...]) ─────────
# One `fleet admin add` per email provisions the FULL admin across both user
# planes (web login + chat-admin + Operations Center; passwords prompted hidden
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
          if FLEET_CHAT_DATABASE_URL="$CHAT_URL" FLEET_SCHED_DATABASE_URL="$SCHED_URL" \
             "$_fleet_bin" admin add "$_email"; then
            ok "admin ${_email} registered (web login + Operations Center)"
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
info "Update in place later:  fleet update      (--check for a read-only 'commits behind'; or scripts/update.sh)"
ok "bootstrap complete (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"
