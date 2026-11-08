# shellcheck shell=bash
# scripts/lib/retired-bundle.sh — refuse a client bundle that still uses a
# removed CHAT_/CUTLASS_ variable (ADR-0106) BEFORE anything is installed.
#
# Sourced by update.sh and bootstrap.sh. The check is the NEW release
# binary's read-only scan (`fleet validate-config --retired-only`: no load,
# no env-file application, no writes), so it is exactly what the binary about
# to be installed will refuse.

# fleet_retired_bundle_check BIN BUNDLE_DIR — 0 when BUNDLE_DIR uses no removed
# variable; 1 (after printing each use and its FLEET_ replacement to stderr)
# when it does. A binary that cannot run the scan (an older release without
# --retired-only, which still reads the old spellings) only warns: 0.
fleet_retired_bundle_check() {
  local bin="$1" bundle="$2" out rc=0
  out="$("$bin" validate-config --retired-only --bundle-path "$bundle" 2>&1)" || rc=$?
  case "$rc" in
    0) return 0 ;;
    7) printf '%s\n' "$out" >&2; return 1 ;;
    *) printf '! could not run the release'"'"'s removed-variable check (exit %s); continuing: %s\n' "$rc" "$out" >&2; return 0 ;;
  esac
}
