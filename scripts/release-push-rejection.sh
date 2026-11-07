#!/usr/bin/env bash
# scripts/release-push-rejection.sh PUSH_LOG — classify why `git push` of a
# release tag failed, from its output, for release.yml's tag job:
#
#   contention           another run took the ordinal: the ref already exists
#                        (`[rejected] … (already exists)`), or a
#                        non-fast-forward / stale-ref rejection. Retrying with
#                        the next ordinal fixes it.
#   workflow-permission  GitHub refused a GitHub App token creating a ref that
#                        would add or change a workflow file relative to the
#                        default branch ("refusing to allow a GitHub App to
#                        create or update workflow … without `workflows`
#                        permission"): the commit is older than a workflow
#                        change on main. Retrying cannot fix it.
#   other                anything else (auth, a protected-ref rule, a network
#                        failure): not retried, the job stops with the reason.
#
# Prints the class; always exits 0 when the log is readable.
set -euo pipefail

[[ $# -eq 1 && -r "$1" ]] || { echo "usage: $0 PUSH_LOG" >&2; exit 2; }
log="$(cat "$1")"
if grep -Eqi 'without [`'"'"'"]?workflows[`'"'"'"]? permission|refusing to allow a (GitHub App|OAuth App|Personal Access Token) to create or update workflow' <<<"$log"; then
  echo workflow-permission
elif grep -Eqi '\[rejected\][^\n]*\((already exists|non-fast-forward|fetch first|stale info)\)|! \[remote rejected\][^\n]*\((cannot lock ref|reference already exists|failed to update ref)|reference already exists' <<<"$log"; then
  echo contention
else
  echo other
fi
