#!/usr/bin/env bash
# scripts/go-test.sh — the tagged Go suite, in one `go test` run.
#
# Every Postgres-backed test gets a database of its own, cloned from a
# migrated template (internal/testdb, #2049 M2), so no package shares a
# database with another and the whole tree runs at Go's default package
# parallelism — there are no DSN groups, no serial lanes and no prebuild step
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

while [[ $# -gt 0 ]]; do
  case "$1" in
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

start="$(date +%s)"
echo "==> go test ${#ALL[@]} package(s) at default -p"
st=0
go test "${TAGS[@]}" "${RACE[@]}" "${COUNT[@]}" "${COVER[@]}" "${ALL[@]}" || st=$?
echo "==> go test finished in $(( $(date +%s) - start ))s (exit ${st})"
exit "$st"
