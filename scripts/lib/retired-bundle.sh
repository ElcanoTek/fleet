# shellcheck shell=bash
# scripts/lib/retired-bundle.sh — refuse a client bundle that still uses a
# removed CHAT_/CUTLASS_ variable (ADR-0106) BEFORE anything is installed.
#
# Sourced by update.sh and bootstrap.sh. The check is the NEW release
# binary's read-only scan (`fleet validate-config --retired-only`: no load,
# no env-file application, no writes), so it is exactly what the binary about
# to be installed will refuse.

# fleet_retired_bundles_check BIN DIR… — scan each bundle dir (duplicates are
# scanned once). Returns
#   0  every bundle is clean — or BIN predates the scan: it rejects the flag
#      itself (usage exit 2 with "flag provided but not defined:
#      -retired-only"), and such a release still reads the old spellings;
#   1  a bundle uses a removed variable (each use and its FLEET_ replacement
#      printed to stderr);
#   2  the scan itself failed (an unreadable or unparsable manifest, a crash
#      — the scanner's output printed to stderr). Callers abort on 1 and 2.
fleet_retired_bundles_check() {
  local bin="$1" dir out rc worst=0 seen=" "
  shift
  for dir in "$@"; do
    [[ -n "$dir" ]] || continue
    [[ "$seen" == *" $dir "* ]] && continue
    seen+="$dir "
    rc=0
    out="$("$bin" validate-config --retired-only --bundle-path "$dir" 2>&1)" || rc=$?
    if [[ "$rc" == 0 ]]; then
      continue
    elif [[ "$rc" == 7 ]]; then
      printf '%s\n' "$out" >&2
      [[ "$worst" == 2 ]] || worst=1
    elif [[ "$rc" == 2 && "$out" == *"flag provided but not defined: -retired-only"* ]]; then
      printf '! %s predates the removed-variable check (no --retired-only); it still reads the old spellings — skipping the check\n' "$bin" >&2
      return 0
    else
      printf 'the removed-variable check of %s failed (exit %s):\n%s\n' "$dir" "$rc" "$out" >&2
      worst=2
    fi
  done
  return "$worst"
}
