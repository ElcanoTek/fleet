#!/usr/bin/env bash
# test-db-setup.sh — create an ISOLATED pair of test databases for this
# checkout/branch and print the export lines that point the suite at them.
#
# Why: the chat + sched test suites migrate and truncate their databases, so
# two checkouts (or a main checkout plus an agent worktree on a feature branch
# with a NEWER migration) sharing the default fleet_chat_test/fleet_sched_test
# pair corrupt each other — the older build refuses to run against the newer
# schema version ("database is at schema version N+1 … refusing to downgrade")
# and concurrent truncates race. One suffix per parallel workstream fixes it.
#
# Usage:
#   scripts/test-db-setup.sh [suffix]      # default suffix: current branch name
#   eval "$(scripts/test-db-setup.sh mybranch | tail -3)"   # apply the exports
#   scripts/test-db-setup.sh --drop <suffix>                # clean up after
#
# Requires a local Postgres where role `fleet` (password `fleet`) can create
# databases — the same assumption docs/TESTING.md makes.
set -euo pipefail

PGURL="postgres://fleet:fleet@localhost:5432/postgres?sslmode=disable"

sanitize() {
    # A branch like feat/web-push-292 → feat_web_push_292 (valid db-name chars).
    printf '%s' "$1" | tr -c 'a-zA-Z0-9' '_' | tr '[:upper:]' '[:lower:]' | cut -c1-40
}

if [[ "${1:-}" == "--drop" ]]; then
    suffix="$(sanitize "${2:?usage: test-db-setup.sh --drop <suffix>}")"
    for db in "fleet_chat_test_${suffix}" "fleet_sched_test_${suffix}"; do
        psql "$PGURL" -qc "DROP DATABASE IF EXISTS ${db}" && echo "dropped ${db}"
    done
    exit 0
fi

raw="${1:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo local)}"
suffix="$(sanitize "$raw")"
chat_db="fleet_chat_test_${suffix}"
sched_db="fleet_sched_test_${suffix}"

for db in "$chat_db" "$sched_db"; do
    if ! psql "$PGURL" -qtc "SELECT 1 FROM pg_database WHERE datname = '${db}'" | grep -q 1; then
        psql "$PGURL" -qc "CREATE DATABASE ${db} OWNER fleet"
        echo "created ${db}" >&2
    else
        echo "exists  ${db}" >&2
    fi
done

# The last three lines are the eval-able payload (see usage above).
echo "export FLEET_TEST_DATABASE_URL='postgres://fleet:fleet@localhost:5432/${chat_db}?sslmode=disable'"
echo "export CHAT_TEST_DATABASE_URL='postgres://fleet:fleet@localhost:5432/${chat_db}?sslmode=disable'"
echo "export DATABASE_URL='postgres://fleet:fleet@localhost:5432/${sched_db}?sslmode=disable'"
