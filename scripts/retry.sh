#!/usr/bin/env bash
# Copyright (c) 2025 ElcanoTek
# SPDX-License-Identifier: MIT
#
# retry.sh — run a network-fetching command, retrying a bounded number of
# times with a linear backoff, for the CI steps whose tool has no retry flag
# of its own (go mod download, go install, npx playwright install, a podman
# build that pulls a base image and ~300 RPMs).
#
#   scripts/retry.sh [--attempts N] [--delay SECONDS] [--what LABEL] -- CMD [ARGS...]
#
# Defaults: 3 attempts, 10s base delay (attempt k waits k*delay before the
# next try), LABEL = the command name. The command's own exit status is
# returned after the last attempt, with a GitHub Actions ::error:: line that
# says what was unreachable, so a red step names its cause.
#
# Only wrap a command that is safe to repeat: a fetch, or a build whose
# finished layers are reused. Retrying cannot weaken an integrity check (a
# checksum mismatch fails every attempt), but it does multiply the time a
# genuinely broken command takes to fail, so keep the bound small. curl and
# apt-get do NOT go through here: they use their own retry options
# (`--retry … --retry-all-errors`, `-o Acquire::Retries=5`), which
# scripts/check_network_retries_test.go pins in every workflow.
set -euo pipefail

attempts=3
delay=10
what=""

usage() {
  echo "usage: $0 [--attempts N] [--delay SECONDS] [--what LABEL] -- CMD [ARGS...]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --attempts) [ $# -ge 2 ] || usage; attempts="$2"; shift 2 ;;
    --delay)    [ $# -ge 2 ] || usage; delay="$2"; shift 2 ;;
    --what)     [ $# -ge 2 ] || usage; what="$2"; shift 2 ;;
    --) shift; break ;;
    *) usage ;;
  esac
done
[ $# -gt 0 ] || usage
case "$attempts" in ''|*[!0-9]*|0) echo "$0: --attempts must be a positive integer" >&2; exit 2 ;; esac
case "$delay" in ''|*[!0-9]*) echo "$0: --delay must be a whole number of seconds" >&2; exit 2 ;; esac
[ -n "$what" ] || what="$1"

attempt=1
while :; do
  rc=0
  "$@" || rc=$?
  if [ "$rc" -eq 0 ]; then
    exit 0
  fi
  if [ "$attempt" -ge "$attempts" ]; then
    echo "::error::${what} failed ${attempts} time(s) (last exit ${rc}) — likely a network or upstream outage reaching it from this runner"
    exit "$rc"
  fi
  wait_s=$(( attempt * delay ))
  echo "::warning::${what} failed (exit ${rc}, attempt ${attempt}/${attempts}); retrying in ${wait_s}s"
  sleep "$wait_s"
  attempt=$(( attempt + 1 ))
done
