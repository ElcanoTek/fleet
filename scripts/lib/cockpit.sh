# shellcheck shell=bash
# scripts/lib/cockpit.sh — uninstall Cockpit at bootstrap.
#
# Sourced (not executed) by bootstrap.sh only.

# The Vultr Fedora image ships Cockpit's root-capable web login on :9090; our boxes are administered over ssh only.
# cockpit_uninstall — remove every cockpit package when cockpit-ws is
# installed. FLEET_KEEP_COCKPIT=1 skips it. Silent where rpm or cockpit is
# absent; never touches firewalld; never fails the caller. Uses the caller's
# run/info/ok/warn (`run` honours --dry-run).
cockpit_uninstall() {
  if [[ "${FLEET_KEEP_COCKPIT:-0}" == "1" ]]; then
    info "FLEET_KEEP_COCKPIT=1 — leaving Cockpit installed"
    return 0
  fi
  command -v rpm >/dev/null 2>&1 && rpm -q cockpit-ws >/dev/null 2>&1 || return 0
  if run dnf -y remove 'cockpit*'; then
    [[ "${DRY_RUN:-0}" == "1" ]] || ok "removed Cockpit — no web console on :9090"
  else
    warn "could not remove Cockpit — run: dnf -y remove 'cockpit*'"
  fi
  return 0
}
