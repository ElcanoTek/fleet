#!/usr/bin/env bash
# test-db-setup.sh — check a Postgres server for the Go test suite, and clean
# up after killed runs.
#
# The DB tests need ONE thing: FLEET_TEST_DATABASE_URL, a DSN for any database
# on a server whose role can CREATE DATABASE. Every DB test clones a database
# of its own from a migrated template (internal/testdb, #2049 M2), named
# fleet_tmpl_* (templates) and fleet_t_* (per-test), so checkouts and worktrees
# on different migration sets share one server safely — there are no
# per-branch database pairs any more.
#
# Usage:
#   scripts/test-db-setup.sh            # preflight; prints the export line
#   eval "$(scripts/test-db-setup.sh | tail -1)"
#   scripts/test-db-setup.sh --gc       # drop leftovers of dead test runs
#   scripts/test-db-setup.sh --gc --templates   # …and every template too
#
# The DSN is FLEET_TEST_DATABASE_URL when set, else the docs' local default
# (role fleet / password fleet on localhost, database postgres).
#
# --gc drops only databases named fleet_t_* / fleet_tmpl_build_* whose owning
# test process is gone (it holds no testdb owner lock — see internal/testdb),
# never one a live run is using, and never any other database. --templates
# also drops every fleet_tmpl_* template; the next run rebuilds what it needs.
# Run --templates when no test run is active (a live run would rebuild a
# template it loses, but pays the migration again).
set -euo pipefail

DSN="${FLEET_TEST_DATABASE_URL:-postgres://fleet:fleet@localhost:5432/postgres?sslmode=disable}"

# Must match internal/testdb: lockNSOwner, the owner-lock namespace (0x666c7474).
OWNER_NS=1718383732

psqlq() { psql "$DSN" -X -v ON_ERROR_STOP=1 -qAt "$@"; }

if [[ "${1:-}" == "--gc" ]]; then
  templates=0
  [[ "${2:-}" == "--templates" ]] && templates=1
  # One SQL statement lists the dead owners' databases: per-test and build
  # names carry the owner id, and pg_locks (cluster-wide) shows live owners.
  mapfile -t dead < <(psqlq -c "
    WITH live AS (
      SELECT lpad(to_hex(objid::bigint), 8, '0') AS owner FROM pg_locks
       WHERE locktype = 'advisory' AND granted AND objsubid = 2 AND classid = ${OWNER_NS}::bigint::oid
    ), ours AS (
      SELECT datname,
             coalesce(substring(datname FROM '^fleet_t_([0-9a-f]{8})_[0-9]+(?:_[a-z0-9_]+)?\$'),
                      substring(datname FROM '^fleet_tmpl_build_[a-z]+_[0-9a-f]{12}_([0-9a-f]{8})\$')) AS owner
        FROM pg_database
       WHERE starts_with(datname, 'fleet_t_') OR starts_with(datname, 'fleet_tmpl_build_')
    )
    SELECT datname FROM ours WHERE owner IS NOT NULL AND owner NOT IN (SELECT owner FROM live)")
  if [[ "$templates" -eq 1 ]]; then
    mapfile -t tmpl < <(psqlq -c "SELECT datname FROM pg_database WHERE starts_with(datname, 'fleet_tmpl_') AND NOT starts_with(datname, 'fleet_tmpl_build_')")
    for db in "${tmpl[@]}"; do
      psqlq -c "ALTER DATABASE \"${db}\" WITH IS_TEMPLATE false" -c "DROP DATABASE IF EXISTS \"${db}\" WITH (FORCE)"
      echo "dropped template ${db}"
    done
  fi
  for db in "${dead[@]}"; do
    psqlq -c "DROP DATABASE IF EXISTS \"${db}\" WITH (FORCE)"
    echo "dropped ${db}"
  done
  echo "gc: dropped ${#dead[@]} leftover database(s)$([[ "$templates" -eq 1 ]] && echo " and ${#tmpl[@]} template(s)")"
  exit 0
fi

if [[ $# -gt 0 ]]; then
  sed -n '2,27p' "$0" | sed 's/^# \?//' >&2
  exit 2
fi

# redact strips credentials from text that may hold a DSN: a URL's whole
# userinfo (user and password) and any password=… key (key=value DSNs, or a
# query parameter).
redact() {
  printf '%s' "$1" | sed -E \
    -e 's#([A-Za-z][A-Za-z0-9+.-]*://)[^@/?[:space:]]*@#\1***@#g' \
    -e 's/(password=)[^[:space:]&]*/\1***/g'
}

# Preflight: the server answers, the role can create databases.
if ! out="$(psqlq -c "SELECT current_user, rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user" 2>&1)"; then
  echo "cannot connect with $(redact "$DSN"): $(redact "$out")" >&2
  echo "start Postgres, create the role (CREATE ROLE fleet LOGIN PASSWORD 'fleet' CREATEDB), or set FLEET_TEST_DATABASE_URL" >&2
  exit 1
fi
role="${out%%|*}"
if [[ "${out##*|}" != "t" ]]; then
  echo "role ${role} cannot create databases; every DB test gets its own. Run: ALTER ROLE ${role} CREATEDB" >&2
  exit 1
fi
echo "ok: role ${role} can create databases" >&2
echo "export FLEET_TEST_DATABASE_URL='${DSN}'"
