# shellcheck shell=bash
# scripts/lib/pgprovision.sh — the local Postgres roles + databases bootstrap
# provisions, without ever printing their passwords.
#
# Sourced (not executed). Sourced by bootstrap.sh only.
#
# On an installed box the passwords are the live ones, read back out of the
# env file's DSNs, so neither the --dry-run plan nor a failed real run may show
# them: the plan is built with placeholders, and psql's errors (which can quote
# the failing statement, "LINE 1: ALTER ROLE ... PASSWORD '...'") are shown
# with each password replaced by the same placeholder.

# Never traced, whoever sources this: `set -x` would print the passwords these functions take.
{ set +x; } 2>/dev/null

# What the plan and a redacted psql error show in place of each password.
FLEET_DB_PW_PLACEHOLDER='<db password>'
# LEGACY-DB-LAYOUT: remove after consolidation — the per-store placeholders.
FLEET_CHAT_PW_PLACEHOLDER='<chat db password>'
# Kept apart from the line above: gitleaks reads a bare next line as its value.
FLEET_SCHED_PW_PLACEHOLDER='<sched db password>'

# fleet_role_db_sql USER PASSWORD DB — the SQL that provisions one role and
# database: CREATE ROLE if missing, ALTER it to PASSWORD, CREATE DATABASE if
# missing. Called with the real password to provision, and with a placeholder
# for the plan.
fleet_role_db_sql() {
  cat <<SQL
SELECT 'CREATE ROLE ${1} LOGIN PASSWORD ''${2}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${1}')\gexec
ALTER ROLE ${1} WITH LOGIN PASSWORD '${2}';
SELECT 'CREATE DATABASE ${3} OWNER ${1}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${3}')\gexec
SQL
}

# fleet_dsn_role_db DSN — print "ROLE DATABASE" from a postgres:// URL DSN
# whose user and database are lower-case plain identifiers (safe to quote
# into SQL); return 1 for anything else.
fleet_dsn_role_db() {
  local re='^postgres(ql)?://([a-z_][a-z0-9_]*)(:[^@]*)?@[^/?#]*/([a-z_][a-z0-9_]*)([?#].*)?$'
  [[ "$1" =~ $re ]] || return 1
  printf '%s %s\n' "${BASH_REMATCH[2]}" "${BASH_REMATCH[4]}"
}

# fleet_dsn_is_local_default DSN — true when a postgres:// URL DSN names the
# local cluster bootstrap provisions: loopback or the default socket, port
# 5432 (or none), no host/port query override.
fleet_dsn_is_local_default() {
  local re='^postgres(ql)?://[^@/?#]*@(127\.0\.0\.1|localhost|\[::1\])?(:5432)?/[^?#]*([?#].*)?$'
  [[ "$1" =~ $re ]] || return 1
  [[ "$1" != *[?\&]host=* && "$1" != *[?\&]hostaddr=* ]] || return 1
  [[ "$1" != *[?\&]port=* || "$1" == *[?\&]port=5432* ]]
}

# fleet_external_db_sql ROLE DATABASE — the superuser SQL external mode runs
# with FLEET_DB_SUPERUSER_URL: create the database when missing, and grant
# the role CONNECT + CREATE on it, so it can create the chat and sched
# schemas even in a database the superuser owns. Names are validated by
# fleet_dsn_role_db.
fleet_external_db_sql() {
  cat <<SQL
SELECT 'CREATE DATABASE "${2}"'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${2}')\gexec
GRANT CONNECT, CREATE ON DATABASE "${2}" TO "${1}";
SQL
}

# fleet_db_store_readiness DSN — can fleet's stores run in the database DSN
# names, as DSN's role? Returns
#   0  yes: for each of the schemas chat and sched, the role has CREATE and
#      USAGE on it when it exists, or CREATE on the database when it does
#      not (each store creates its schema on first start);
#   1  no: a privilege is missing;
#   2  no: the database's public schema holds a fleet store
#      (public.schema_migrations) — a legacy two-database install's
#      database, which the stores refuse to run next to;
#   3  the check could not run (psql failed).
fleet_db_store_readiness() {
  local out
  out="$(psql -X -v ON_ERROR_STOP=1 -tAq -F '|' "$1" -c "SELECT
      to_regclass('public.schema_migrations') IS NOT NULL,
      bool_and(CASE WHEN n.oid IS NULL THEN has_database_privilege(current_database(), 'CREATE')
                    ELSE has_schema_privilege(n.oid, 'CREATE') AND has_schema_privilege(n.oid, 'USAGE') END)
    FROM (VALUES ('chat'), ('sched')) AS s(name)
    LEFT JOIN pg_namespace n ON n.nspname = s.name" 2>/dev/null)" || return 3
  case "$out" in
    t\|*) return 2 ;;
    f\|t) return 0 ;;
    f\|f) return 1 ;;
    *) return 3 ;;
  esac
}

# fleet_redact_db_passwords < TEXT — TEXT with every occurrence of the
# passwords in FLEET_REDACT_CHAT_PW and FLEET_REDACT_SCHED_PW replaced by their
# placeholders. The passwords come from the environment, never argv, and are
# matched literally, byte for byte (index/substr under LC_ALL=C, never a regex,
# so no character is special). The longer one is replaced first, so a password
# that contains the other is still replaced whole. A password with a line break
# cannot be matched line by line, so then nothing is shown but a notice.
fleet_redact_db_passwords() {
  if [[ "${FLEET_REDACT_CHAT_PW:-}" == *$'\n'* || "${FLEET_REDACT_SCHED_PW:-}" == *$'\n'* ]]; then
    printf '%s\n' "(psql's error output withheld: a database password contains a line break)"
    return 0
  fi
  LC_ALL=C awk -v chat_ph="${FLEET_REDACT_CHAT_PH:-$FLEET_CHAT_PW_PLACEHOLDER}" -v sched_ph="${FLEET_REDACT_SCHED_PH:-$FLEET_SCHED_PW_PLACEHOLDER}" '
    function literal(s, find, rep,    out, i) {
      if (find == "") return s
      out = ""
      while ((i = index(s, find)) > 0) {
        out = out substr(s, 1, i - 1) rep
        s = substr(s, i + length(find))
      }
      return out s
    }
    BEGIN {
      first = ENVIRON["FLEET_REDACT_CHAT_PW"];   first_ph = chat_ph
      second = ENVIRON["FLEET_REDACT_SCHED_PW"]; second_ph = sched_ph
      if (length(second) > length(first)) {
        t = first; first = second; second = t
        t = first_ph; first_ph = second_ph; second_ph = t
      }
    }
    { print literal(literal($0, first, first_ph), second, second_ph) }
  '
}

# fleet_psql_provision CHAT_PASSWORD SCHED_PASSWORD < SQL — run the SQL as the
# postgres user, stopping at the first error, and return psql's status. Its
# stdout is discarded. Its stderr is captured in memory, never in a file (a
# temp file would outlive a SIGTERM or SIGKILL, which skip an EXIT trap), and
# on failure is shown on stderr through fleet_redact_db_passwords.
# VERBOSITY=terse already keeps libpq from quoting the statement; the
# redaction is what guarantees it.
fleet_psql_provision() {
  local err rc=0
  err="$(runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v VERBOSITY=terse 2>&1 >/dev/null)" || rc=$?
  if (( rc != 0 )); then
    printf '%s\n' "$err" | FLEET_REDACT_CHAT_PW="$1" FLEET_REDACT_SCHED_PW="$2" fleet_redact_db_passwords >&2 || true
  fi
  return "$rc"
}

# fleet_psql_provision_one PASSWORD < SQL — fleet_psql_provision for the one
# role and database of the one-database layout: its password is shown as
# FLEET_DB_PW_PLACEHOLDER in a redacted error.
fleet_psql_provision_one() {
  FLEET_REDACT_CHAT_PH="$FLEET_DB_PW_PLACEHOLDER" fleet_psql_provision "$1" ""
}
