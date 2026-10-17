# shellcheck shell=bash
# scripts/lib/cockpit.sh — keep the Cockpit web console off. Shared by
# bootstrap.sh and update.sh (which turn it off) and doctor.sh (which checks).
#
# Sourced (not executed).
#
# Why: the Vultr Fedora image ships cockpit, and cockpit.socket serves a
# root-capable web login on :9090 — a second, separately-patched remote-admin
# surface on an internet-facing box whose only admin path is ssh. Bootstrap
# turns it off on a new box; every update turns it off again, so a box that
# got it back (a package update, a hand-run `systemctl enable`) heals on the
# next `fleet update`. Mirrors keel's step_cockpit (deploy/harden-host.sh).
#
# Safe on any host: it never installs or removes a package, never touches ufw
# or iptables, and never fails the caller. With no systemd or no cockpit.socket
# unit (containers, Debian/Ubuntu without cockpit) it is a one-line no-op —
# including the firewall half. Idempotent: an already-masked socket is skipped.

# Masking leaves cockpit's login-banner notice ("Activate the web console
# with: systemctl enable --now cockpit.socket") in the MOTD: /etc/motd.d/cockpit
# links to /run/cockpit/issue, which the package's tmpfiles.d entry recreates at
# every boot. A same-named /etc/tmpfiles.d copy overrides it (and survives
# cockpit-ws upgrades); deploy/cockpit-ws.tmpfiles.conf keeps the notice empty.
# Paths are overridable for the tests only. Pure bash on purpose: sourcing must
# not depend on PATH.
COCKPIT_PKG_TMPFILES="${COCKPIT_PKG_TMPFILES:-/usr/lib/tmpfiles.d/cockpit-ws.conf}"
COCKPIT_ETC_TMPFILES="${COCKPIT_ETC_TMPFILES:-/etc/tmpfiles.d/cockpit-ws.conf}"
_cockpit_lib_dir="${BASH_SOURCE[0]%/*}"
[[ "${BASH_SOURCE[0]}" == */* ]] || _cockpit_lib_dir=.
COCKPIT_TMPFILES_SRC="${COCKPIT_TMPFILES_SRC:-${_cockpit_lib_dir}/../../deploy/cockpit-ws.tmpfiles.conf}"
unset _cockpit_lib_dir

# cockpit_banner_current — succeed when there is nothing to do: no cockpit-ws
# tmpfiles entry, no systemd-tmpfiles, or the override already installed.
# Read-only.
cockpit_banner_current() {
  [[ -f "$COCKPIT_PKG_TMPFILES" ]] || return 0
  command -v systemd-tmpfiles >/dev/null 2>&1 || return 0
  cmp -s "$COCKPIT_TMPFILES_SRC" "$COCKPIT_ETC_TMPFILES"
}

# cockpit_banner_quiet — install the override and apply it now. Uses the
# caller's run/ok/warn; never fails the caller.
cockpit_banner_quiet() {
  if cockpit_banner_current; then
    [[ -f "$COCKPIT_PKG_TMPFILES" ]] && command -v systemd-tmpfiles >/dev/null 2>&1 \
      && ok "cockpit's login-banner notice already blank (${COCKPIT_ETC_TMPFILES})"
    return 0
  fi
  if [[ ! -f "$COCKPIT_TMPFILES_SRC" ]]; then
    warn "missing ${COCKPIT_TMPFILES_SRC} — cockpit's login-banner notice stays"
    return 0
  fi
  if run install -D -m 0644 "$COCKPIT_TMPFILES_SRC" "$COCKPIT_ETC_TMPFILES" \
    && run systemd-tmpfiles --create "$COCKPIT_ETC_TMPFILES"; then
    [[ "${DRY_RUN:-0}" == "1" ]] || ok "blanked cockpit's login-banner notice (${COCKPIT_ETC_TMPFILES})"
  else
    warn "could not blank cockpit's login-banner notice — run: install -m 0644 ${COCKPIT_TMPFILES_SRC} ${COCKPIT_ETC_TMPFILES} && systemd-tmpfiles --create ${COCKPIT_ETC_TMPFILES}"
  fi
  return 0
}

# cockpit_socket_state — print `absent` (no cockpit.socket unit, or no
# systemd), `masked` (already off for good), or `live` (installed and not
# masked: listening now, or one `systemctl enable` away from it). Read-only.
cockpit_socket_state() {
  if ! command -v systemctl >/dev/null 2>&1 \
    || [[ -z "$(systemctl list-unit-files cockpit.socket --no-legend 2>/dev/null)" ]]; then
    printf 'absent'
  elif [[ "$(systemctl is-enabled cockpit.socket 2>/dev/null || true)" == masked* ]]; then
    printf 'masked'
  else
    printf 'live'
  fi
}

# cockpit_firewall_zones — print the firewalld zones whose PERMANENT config
# still allows the cockpit service, one per line. Empty when firewall-cmd is
# missing, `firewall-cmd --state` does not say running, or no zone lists
# cockpit. Read-only.
# Every zone, not just the default one: an interface bound to a non-default
# zone that allows cockpit opens :9090 just the same.
cockpit_firewall_zones() {
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  [[ "$(firewall-cmd --state 2>/dev/null || true)" == "running" ]] || return 0
  local zone services
  for zone in $(firewall-cmd --permanent --get-zones 2>/dev/null || true); do
    # Captured, not piped into grep -q: under the callers' pipefail an early
    # grep exit can SIGPIPE the writer and turn a match into a miss.
    services="$(firewall-cmd --permanent --zone="$zone" --list-services 2>/dev/null || true)"
    if [[ " ${services} " == *" cockpit "* ]]; then
      printf '%s\n' "$zone"
    fi
  done
}

# cockpit_keep_off — disable + stop + mask cockpit.socket, blank its
# login-banner notice, and drop the cockpit service from every firewalld zone
# that allows it. Uses the CALLER's
# run/ok/info/warn helpers (bootstrap.sh and update.sh define all four, and
# their `run` honours --dry-run). Never fails the caller: a box where this
# cannot be done (no root, systemctl error) gets a warning with the exact
# commands, and `fleet doctor` reports it until it is fixed.
cockpit_keep_off() {
  local state zones zone fw_failed=0
  state="$(cockpit_socket_state)"
  case "$state" in
    absent)
      info "cockpit is not installed — no web console to turn off"
      return 0
      ;;
    masked) ok "cockpit.socket already masked — no web console on :9090" ;;
    *)
      if run systemctl disable --now cockpit.socket && run systemctl mask cockpit.socket; then
        [[ "${DRY_RUN:-0}" == "1" ]] || ok "cockpit.socket disabled, stopped and masked — no web console on :9090"
      else
        warn "could not turn cockpit off — run: systemctl disable --now cockpit.socket && systemctl mask cockpit.socket"
      fi
      ;;
  esac
  cockpit_banner_quiet
  zones="$(cockpit_firewall_zones)"
  [[ -n "$zones" ]] || return 0
  for zone in $zones; do
    run firewall-cmd -q --permanent --zone="$zone" --remove-service=cockpit || fw_failed=1
  done
  run firewall-cmd -q --reload || fw_failed=1
  if [[ "$fw_failed" == "1" ]]; then
    warn "could not remove cockpit from the firewall (zones: ${zones//$'\n'/ }) — run: firewall-cmd --permanent --zone=<zone> --remove-service=cockpit && firewall-cmd --reload"
  elif [[ "${DRY_RUN:-0}" != "1" ]]; then
    ok "removed the cockpit service from firewalld (zones: ${zones//$'\n'/ })"
  fi
  return 0
}
