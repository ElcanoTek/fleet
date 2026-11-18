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
#
# The global line alone is not enough: git finds the global config through
# $HOME, and a run without one (a `systemd-run` unit has no HOME) neither reads
# nor writes it, so every git call on a checkout root does not own failed with
# "detected dubious ownership" — `fleet update` launched that way left the
# client bundle unpulled on every box from 2026-11-18 and still exited 0. So
# the helper also trusts DIR for this process and its children through git's
# environment config (GIT_CONFIG_COUNT / _KEY_n / _VALUE_n, git >= 2.31), which
# is command scope, the protected scope git honours safe.directory from, read
# after the global file whatever HOME is.
git_trust_dir() {
  local dir="$1" e n=0 effective=0
  local -a entries=()
  _git_trust_dir_env "$dir"
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

# _git_trust_dir_env DIR — append `safe.directory = DIR` to this process's git
# environment config unless it already takes effect there (its last occurrence
# after the last empty reset, the same rule as the global list). Called once
# per checkout per run, and a self re-exec inherits the entry, so the list
# stays short. A malformed GIT_CONFIG_COUNT is left alone: git refuses to run
# with one whatever this adds.
_git_trust_dir_env() {
  local dir="$1" count="${GIT_CONFIG_COUNT:-0}" i key val effective=0
  [[ "$count" =~ ^[0-9]+$ ]] || return 0
  for ((i = 0; i < count; i++)); do
    key="GIT_CONFIG_KEY_${i}" val="GIT_CONFIG_VALUE_${i}"
    [[ "${!key:-}" == "safe.directory" ]] || continue
    if [[ -z "${!val:-}" ]]; then
      effective=0
    elif [[ "${!val}" == "$dir" ]]; then
      effective=1
    fi
  done
  (( effective )) && return 0
  export "GIT_CONFIG_KEY_${count}=safe.directory" "GIT_CONFIG_VALUE_${count}=${dir}"
  export GIT_CONFIG_COUNT=$((count + 1))
}
