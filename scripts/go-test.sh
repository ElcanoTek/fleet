#!/usr/bin/env bash
# scripts/go-test.sh — the tagged Go suite, in one `go test` run.
#
# Every Postgres-backed test gets a database of its own, cloned from a
# migrated template (internal/testdb, #2049 M2), so no package shares a
# database with another and the whole tree runs in one go test,
# with -p and -parallel capped to the test server's free connections (below)
# — there are no DSN groups, no serial lanes and no prebuild step
# any more. The one knob is FLEET_TEST_DATABASE_URL: a maintenance DSN for
# any database on a test server whose role can CREATE DATABASE (unset: the DB
# tests skip). See docs/TESTING.md.
#
# CI and `make test` / `make test-race` / `make test-cover` all call this
# script, so the flags cannot drift between them.
#
# Usage:
#   scripts/go-test.sh
#   scripts/go-test.sh --race
#   scripts/go-test.sh --coverprofile=coverage.out --count=1
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TAGS=(-tags fleet_host_executor)
RACE=()
COUNT=()
COVER=()

usage() {
  sed -n '2,19p' "$0" | sed 's/^# \?//'
}

# Connection budget (see where it is applied, below): each test process holds
# up to PER_PROCESS maintenance connections (internal/testdb: owner lock,
# background clone/drop) and each running DB test up to PER_TEST (its stores'
# small pools, testdb.PoolMaxOpen, are rarely all busy at once).
PER_PROCESS=8
PER_TEST=3

# connection_bound FREE CPUS prints "P PARALLEL": the -p and -parallel that
# keep the suite within three quarters of FREE server connections. -p is
# lowered only when even -parallel 1 does not fit; both stay within 1..CPUS.
connection_bound() {
  local free="$1" cpus="$2" budget p par
  budget=$(( free * 3 / 4 ))
  p="$cpus"
  while (( p > 1 && budget / p < PER_PROCESS + PER_TEST )); do
    p=$(( p - 1 ))
  done
  par=$(( (budget / p - PER_PROCESS) / PER_TEST ))
  (( par < 1 )) && par=1
  (( par > cpus )) && par="$cpus"
  echo "$p $par"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    # For scripts/go_test_script_test.go: print the bound for FREE,CPUS.
    --connection-bound=*) a="${1#*=}"; connection_bound "${a%%,*}" "${a##*,}"; exit 0 ;;
    --race) RACE=(-race); shift ;;
    --count=*) COUNT=(-count="${1#*=}"); shift ;;
    --coverprofile=*) COVER=(-coverprofile="${1#*=}" -covermode=atomic); shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# Capture go list's status ourselves. A process substitution (`< <(go list)`)
# would keep a partial package list and discard a nonzero exit, so `make test`
# could go green over an incomplete graph.
list_file="$(mktemp)"
trap 'rm -f "$list_file"' EXIT
if ! go list "${TAGS[@]}" ./... >"$list_file"; then
  echo "go list failed" >&2
  cat "$list_file" >&2
  exit 2
fi
mapfile -t ALL < "$list_file"
if (( ${#ALL[@]} == 0 )); then
  echo "go list returned no packages" >&2
  exit 2
fi

if [[ -z "${FLEET_TEST_DATABASE_URL:-}" ]]; then
  echo "==> FLEET_TEST_DATABASE_URL is not set: the Postgres-backed tests will SKIP (docs/TESTING.md)" >&2
fi

# Connection budget. `-p` test binaries run at once and each runs up to
# `-parallel` tests at once; both default to GOMAXPROCS, so on a many-core box
# the DB tests alone could open more connections than the server allows (100
# by default) and fail with "too many clients". When the test server answers,
# size both from its free connections (connection_bound, above). Without psql,
# or a reachable server, Go's defaults apply. docs/TEST-DATABASES.md.
BOUND=()
bound_note="default -p"
if [[ -n "${FLEET_TEST_DATABASE_URL:-}" ]] && command -v psql >/dev/null 2>&1; then
  free="$(psql "$FLEET_TEST_DATABASE_URL" -XAtq -c \
    "SELECT current_setting('max_connections')::int - current_setting('superuser_reserved_connections')::int - (SELECT count(*) FROM pg_stat_activity)" 2>/dev/null || true)"
  if [[ "$free" =~ ^[0-9]+$ ]]; then
    cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
    read -r p par < <(connection_bound "$free" "$cpus")
    BOUND=(-p "$p" -parallel "$par")
    bound_note="-p ${p} -parallel ${par} (${free} free connections on the test server)"
  fi
fi

start="$(date +%s)"
echo "==> go test ${#ALL[@]} package(s) at ${bound_note}"
st=0
go test "${TAGS[@]}" "${RACE[@]}" "${COUNT[@]}" "${COVER[@]}" "${BOUND[@]}" "${ALL[@]}" || st=$?
echo "==> go test finished in $(( $(date +%s) - start ))s (exit ${st})"
exit "$st"
