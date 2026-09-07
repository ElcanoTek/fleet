#!/usr/bin/env bash
# scripts/go-test.sh — tagged Go suite with DSN-aware package parallelism.
#
# A global `go test -p 1 ./...` serializes ALL ~68 packages because two groups
# share a Postgres DSN:
#
#   chat  — internal/store + internal/httpapi TRUNCATE fleet_chat_test
#           (FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL)
#   sched — internal/sched/* + internal/runner share fleet_sched_test
#           (DATABASE_URL) via TRUNCATE / pg_advisory_lock(1)
#   both  — internal/admincli + cmd/fleet touch BOTH databases
#
# The chat and sched migration systems both use a table named schema_migrations
# with incompatible schemas, so they MUST point at separate databases (ADR-0005).
# That also means the chat-serial and sched-serial groups can run at the same
# time: they cannot collide. Packages that use neither DSN run at Go's default
# package parallelism. admincli and cmd/fleet wait until both serial groups
# finish, because they share both DSNs.
#
# CI and `make test` / `make test-race` / `make test-cover` all call this script
# so the partition cannot drift between them.
#
# Usage:
#   scripts/go-test.sh
#   scripts/go-test.sh --race
#   scripts/go-test.sh --coverprofile=coverage.out --count=1
#   scripts/go-test.sh --print-groups
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TAGS=(-tags fleet_host_executor)
RACE=()
COUNT=()
COVER_OUT=""
PRINT_GROUPS=0

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \?//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --race) RACE=(-race); shift ;;
    --count=*) COUNT=(-count="${1#*=}"); shift ;;
    --coverprofile=*) COVER_OUT="${1#*=}"; shift ;;
    --print-groups) PRINT_GROUPS=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

classify() {
  local pkg="$1"
  case "$pkg" in
    github.com/ElcanoTek/fleet/internal/store|github.com/ElcanoTek/fleet/internal/store/*)
      echo chat ;;
    github.com/ElcanoTek/fleet/internal/httpapi|github.com/ElcanoTek/fleet/internal/httpapi/*)
      echo chat ;;
    github.com/ElcanoTek/fleet/internal/admincli|github.com/ElcanoTek/fleet/internal/admincli/*)
      echo both ;;
    github.com/ElcanoTek/fleet/cmd/fleet)
      echo both ;;
    github.com/ElcanoTek/fleet/internal/runner|github.com/ElcanoTek/fleet/internal/runner/*)
      echo sched ;;
    github.com/ElcanoTek/fleet/internal/sched|github.com/ElcanoTek/fleet/internal/sched/*)
      echo sched ;;
    *)
      echo independent ;;
  esac
}

mapfile -t ALL < <(go list "${TAGS[@]}" ./...)
if (( ${#ALL[@]} == 0 )); then
  echo "go list returned no packages" >&2
  exit 2
fi

INDEPENDENT=()
CHAT=()
SCHED=()
BOTH=()
for pkg in "${ALL[@]}"; do
  case "$(classify "$pkg")" in
    chat) CHAT+=("$pkg") ;;
    sched) SCHED+=("$pkg") ;;
    both) BOTH+=("$pkg") ;;
    *) INDEPENDENT+=("$pkg") ;;
  esac
done

if [[ "$PRINT_GROUPS" -eq 1 ]]; then
  for pkg in "${INDEPENDENT[@]}"; do printf 'independent\t%s\n' "$pkg"; done
  for pkg in "${CHAT[@]}"; do printf 'chat\t%s\n' "$pkg"; done
  for pkg in "${SCHED[@]}"; do printf 'sched\t%s\n' "$pkg"; done
  for pkg in "${BOTH[@]}"; do printf 'both\t%s\n' "$pkg"; done
  exit 0
fi

declare -A seen=()
partition_ok=1
for pkg in "${INDEPENDENT[@]}" "${CHAT[@]}" "${SCHED[@]}" "${BOTH[@]}"; do
  if [[ -n "${seen[$pkg]:-}" ]]; then
    echo "package classified twice: $pkg" >&2
    partition_ok=0
  fi
  seen[$pkg]=1
done
for pkg in "${ALL[@]}"; do
  if [[ -z "${seen[$pkg]:-}" ]]; then
    echo "package not classified: $pkg" >&2
    partition_ok=0
  fi
done
if [[ "$partition_ok" -ne 1 ]]; then
  exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

run_group() {
  local name="$1"
  local serial="$2"
  local logfile="$3"
  shift 3
  local pkgs=("$@")
  local extra=()
  local cover_file=""
  if [[ "$serial" -eq 1 ]]; then
    extra+=(-p 1)
  fi
  if [[ -n "$COVER_OUT" ]]; then
    cover_file="$tmp/cover.${name}.out"
    extra+=(-coverprofile="$cover_file" -covermode=atomic)
  fi
  local start st elapsed pflag
  if [[ "$serial" -eq 1 ]]; then
    pflag='-p 1'
  else
    pflag='default -p'
  fi
  start="$(date +%s)"
  # `set +e`: a failing `go test` must not trip the script-level `set -e`
  # before we record the status; the other groups should still finish.
  set +e
  {
    echo "==> go test [${name}] ${#pkgs[@]} package(s) ${pflag}"
    go test "${TAGS[@]}" "${RACE[@]}" "${COUNT[@]}" "${extra[@]}" "${pkgs[@]}"
  } >"$logfile" 2>&1
  st=$?
  set -e
  elapsed=$(( $(date +%s) - start ))
  echo "==> [${name}] finished in ${elapsed}s (exit ${st})" >>"$logfile"
  return "$st"
}

status_independent=0
status_chat=0
status_sched=0
status_both=0

# Wave 1: independent packages at default parallelism, overlapping the two
# DSN-serial groups (they use different databases, ADR-0005).
run_group independent 0 "$tmp/independent.log" "${INDEPENDENT[@]}" &
pid_independent=$!
run_group chat 1 "$tmp/chat.log" "${CHAT[@]}" &
pid_chat=$!
run_group sched 1 "$tmp/sched.log" "${SCHED[@]}" &
pid_sched=$!

wait "$pid_independent" || status_independent=$?
wait "$pid_chat" || status_chat=$?
wait "$pid_sched" || status_sched=$?

# Wave 2: packages that share BOTH DSNs, after the serial groups have released
# the databases.
run_group both 1 "$tmp/both.log" "${BOTH[@]}" || status_both=$?

# Replay group logs in a stable order so a failure is readable even when the
# groups ran concurrently.
echo
echo "----- independent packages -----"
cat "$tmp/independent.log"
echo
echo "----- chat-serial packages (FLEET_TEST_DATABASE_URL) -----"
cat "$tmp/chat.log"
echo
echo "----- sched-serial packages (DATABASE_URL) -----"
cat "$tmp/sched.log"
echo
echo "----- both-DSN packages (admincli, cmd/fleet) -----"
cat "$tmp/both.log"

if [[ -n "$COVER_OUT" ]]; then
  shopt -s nullglob
  cover_files=("$tmp"/cover.*.out)
  if (( ${#cover_files[@]} > 0 )); then
    {
      echo "mode: atomic"
      grep -h -v '^mode:' "${cover_files[@]}"
    } >"$COVER_OUT"
  fi
  shopt -u nullglob
fi

fail=0
for pair in "independent:${status_independent}" "chat:${status_chat}" "sched:${status_sched}" "both:${status_both}"; do
  name="${pair%%:*}"
  st="${pair#*:}"
  if [[ "$st" -ne 0 ]]; then
    echo "go test [${name}] failed (exit ${st})" >&2
    fail=1
  fi
done
exit "$fail"
