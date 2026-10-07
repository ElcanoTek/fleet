# shellcheck shell=bash
# scripts/lib/git.sh — git helpers shared by update.sh and doctor.sh.
#
# Sourced (not executed).

# git_trust_dir DIR — leave exactly ONE `safe.directory = DIR` line in the
# caller's global git config (root's ~/.gitconfig on a box).
#
# update.sh and doctor.sh run git as root against checkouts root may not own,
# so they need DIR trusted. They used to `git config --global --add` it on every
# run, and --add never checks for an existing line: a box updated a few hundred
# times carried hundreds of identical entries. This adds the line only when it
# is missing and collapses duplicates left behind by those older runs, so the
# next update tidies a box nobody cleaned by hand. --fixed-value matches DIR
# literally (a path is not a regex). Best effort, like the call it replaces: a
# git without --fixed-value (< 2.30) or an unwritable config never fails the run.
git_trust_dir() {
  local dir="$1" n
  n="$(git config --global --get-all safe.directory 2>/dev/null | grep -cxF -- "$dir" || true)"
  if (( n > 1 )); then
    git config --global --fixed-value --unset-all safe.directory "$dir" 2>/dev/null || return 0
    n=0
  fi
  if (( n == 0 )); then
    git config --global --add safe.directory "$dir" 2>/dev/null || true
  fi
}
