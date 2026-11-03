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

FLEET_CHAT_PW_PLACEHOLDER='<chat db password>'
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
  LC_ALL=C awk -v chat_ph="$FLEET_CHAT_PW_PLACEHOLDER" -v sched_ph="$FLEET_SCHED_PW_PLACEHOLDER" '
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
