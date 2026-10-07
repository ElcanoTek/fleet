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
# literally (a path is not a regex).
#
# Present is not the same as effective: git reads safe.directory as a list in
# which an EMPTY value resets everything before it. The old bare --add always
# appended after any such reset, so it always took effect; this helper must
# keep that property. DIR counts as trusted only when its last occurrence comes
# after the last empty value; otherwise it is collapsed and re-added at the
# end. Best effort, like the call it replaces: an unwritable config never fails
# the run, and a git without --fixed-value (< 2.30) skips the dedupe but still
# gets an effective entry.
git_trust_dir() {
  local dir="$1" e n=0 effective=0
  local -a entries=()
  mapfile -t entries < <(git config --global --get-all safe.directory 2>/dev/null || true)
  for e in "${entries[@]}"; do
    if [[ -z "$e" ]]; then
      effective=0
    elif [[ "$e" == "$dir" ]]; then
      n=$((n + 1))
      effective=1
    fi
  done
  (( n == 1 && effective )) && return 0
  if (( n > 0 )) && ! git config --global --fixed-value --unset-all safe.directory "$dir" 2>/dev/null; then
    # Could not dedupe; an entry that already takes effect is good enough.
    (( effective )) && return 0
  fi
  git config --global --add safe.directory "$dir" 2>/dev/null || true
}
