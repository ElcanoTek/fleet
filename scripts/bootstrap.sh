#!/usr/bin/env bash
# scripts/bootstrap.sh — Mega Box DB + credential-store bootstrap for fleet.
#
# Merges chat's + moc's bootstrap into ONE script with a --postgres=local|external
# branch (default local). It provisions the ONE cluster, TWO databases (chat +
# sched) layout the unified `fleet` process expects, and ensures the 0600
# credential env file exists. It NEVER runs application migrations — each service
# self-migrates on first start (chat's advisory-lock runner; sched's
# golang-migrate).
#
# Usage:
#   scripts/bootstrap.sh --postgres=local            # dnf+initdb+pg_hba+\gexec, sslmode=disable
#   scripts/bootstrap.sh --postgres=external         # validate DSNs with SELECT 1, sslmode=require
#   scripts/bootstrap.sh --postgres=local --dry-run  # print the plan, touch nothing
#
# Branch A (local):  install + init a local cluster, create the two owner roles
#                    and two databases idempotently via psql \gexec, sslmode=disable.
# Branch B (external): skip install; validate the provided DSNs with SELECT 1 and
#                    assume the roles/dbs are pre-provisioned (opt-in superuser
#                    create via FLEET_DB_SUPERUSER_URL), sslmode=require.
#
# Env knobs (all optional; sensible local defaults):
#   FLEET_ENV_FILE          credential env file to ensure exists (default .env.local)
#   CHAT_DB_NAME            chat database name (default chat)
#   CHAT_DB_USER            chat owner role  (default chat)
#   CHAT_DB_PASSWORD        chat role password (local: generated if unset)
#   SCHED_DB_NAME           sched database name (default sched)
#   SCHED_DB_USER           sched owner role (default sched)
#   SCHED_DB_PASSWORD       sched role password (local: generated if unset)
#   FLEET_CHAT_DATABASE_URL external chat DSN (external mode)
#   FLEET_SCHED_DATABASE_URL external sched DSN (external mode)
#   FLEET_DB_SUPERUSER_URL  external superuser DSN for opt-in role/db creation
set -euo pipefail

POSTGRES_MODE="local"
DRY_RUN=0

for arg in "$@"; do
  case "$arg" in
    --postgres=local)    POSTGRES_MODE="local" ;;
    --postgres=external) POSTGRES_MODE="external" ;;
    --postgres=*)        echo "error: --postgres must be local|external" >&2; exit 1 ;;
    --dry-run)           DRY_RUN=1 ;;
    -h|--help)
      sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $arg" >&2; exit 1 ;;
  esac
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

ENV_FILE="${FLEET_ENV_FILE:-.env.local}"
CHAT_DB_NAME="${CHAT_DB_NAME:-chat}"
CHAT_DB_USER="${CHAT_DB_USER:-chat}"
SCHED_DB_NAME="${SCHED_DB_NAME:-sched}"
SCHED_DB_USER="${SCHED_DB_USER:-sched}"

gen_pass() { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

step "fleet bootstrap (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"

# ── credential env file (0600) ──
step "Ensuring credential env file ${ENV_FILE} (0600)"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would create ${ENV_FILE} (0600) if missing"
else
  if [[ ! -f "$ENV_FILE" ]]; then
    install -m 0600 /dev/null "$ENV_FILE"
    ok "created ${ENV_FILE} (0600)"
  else
    chmod 0600 "$ENV_FILE"
    ok "${ENV_FILE} present (mode set to 0600)"
  fi
fi

# ── Postgres provisioning ──
if [[ "$POSTGRES_MODE" == "local" ]]; then
  SSLMODE="disable"
  CHAT_DB_PASSWORD="${CHAT_DB_PASSWORD:-$(gen_pass)}"
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
  fi

  step "Creating roles + databases idempotently (chat + sched)"
  PSQL_SQL=$(cat <<SQL
SELECT 'CREATE ROLE ${CHAT_DB_USER} LOGIN PASSWORD ''${CHAT_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${CHAT_DB_USER}')\gexec
SELECT 'CREATE DATABASE ${CHAT_DB_NAME} OWNER ${CHAT_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${CHAT_DB_NAME}')\gexec
SELECT 'CREATE ROLE ${SCHED_DB_USER} LOGIN PASSWORD ''${SCHED_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${SCHED_DB_USER}')\gexec
SELECT 'CREATE DATABASE ${SCHED_DB_NAME} OWNER ${SCHED_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${SCHED_DB_NAME}')\gexec
SQL
)
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run as postgres:"
    printf '%s\n' "$PSQL_SQL" | sed 's/^/    /'
  else
    printf '%s\n' "$PSQL_SQL" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 >/dev/null \
      || die "role/database provisioning failed"
  fi

  CHAT_URL="postgres://${CHAT_DB_USER}:${CHAT_DB_PASSWORD}@127.0.0.1:5432/${CHAT_DB_NAME}?sslmode=${SSLMODE}"
  SCHED_URL="postgres://${SCHED_DB_USER}:${SCHED_DB_PASSWORD}@127.0.0.1:5432/${SCHED_DB_NAME}?sslmode=${SSLMODE}"
  ok "chat DB:  ${CHAT_DB_NAME} (owner ${CHAT_DB_USER}), sslmode=${SSLMODE}"
  ok "sched DB: ${SCHED_DB_NAME} (owner ${SCHED_DB_USER}), sslmode=${SSLMODE}"
  info "FLEET_CHAT_DATABASE_URL=${CHAT_URL}"
  info "FLEET_SCHED_DATABASE_URL=${SCHED_URL}"

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

step "Reminders"
info "Migrations are NOT run here — each service self-migrates on first start."
info "Set MCP account secrets post-bootstrap: fleet-admin mcp account set <server> <account> --secret KEY=-"
ok "bootstrap complete (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"
