# shellcheck shell=bash
# scripts/lib/hostname.sh — name a new box after its service FQDN.
#
# Sourced (not executed). Sourced by bootstrap.sh only: update never
# renames a box.
#
# Vultr boxes come up as "vultr.guest" (or a short label), so every prompt
# read root@vultr. fleet_set_hostname renames the box to the public hostname
# bootstrap configures, but only when the current static hostname is exactly
# one of the unset cloud-image defaults: vultr.guest, localhost or
# localhost.localdomain. Any other name, short ones included, is a deliberate
# name (a laptop called "yoga") and is left alone with a one-line hint.
# FLEET_SET_HOSTNAME=1 forces the rename whatever the current name;
# FLEET_SET_HOSTNAME=0 disables the step. Skipped when hostnamectl is missing
# or the target is not a dotted FQDN.
#
# When it renames: hostnamectl set-hostname; when /etc/cloud exists,
# cloud.cfg.d/99-preserve-hostname.cfg so cloud-init does not reset the name
# from Vultr metadata; and /etc/hosts, where on the 127.0.0.1 and ::1 lines
# the old hostname token is replaced by "<fqdn> <short>", keeping every other
# alias (localhost names are never replaced) and, when no line names the FQDN,
# the two loopback lines are appended. Idempotent, honours DRY_RUN=1, never fails
# the caller. Uses the caller's info/ok/warn. The paths are overridable for
# the tests only.
FLEET_HOSTS_FILE="${FLEET_HOSTS_FILE:-/etc/hosts}"
FLEET_CLOUD_DIR="${FLEET_CLOUD_DIR:-/etc/cloud}"
FLEET_PRESERVE_HOSTNAME_CFG='# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.
preserve_hostname: true
manage_etc_hosts: false'

_fleet_hn_do() {
  if [[ "${DRY_RUN:-0}" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi
}

# fleet_hostname_is_default NAME — true only for an unset cloud-image default.
fleet_hostname_is_default() {
  case "$1" in
    vultr.guest | localhost | localhost.localdomain) return 0 ;;
    *) return 1 ;;
  esac
}

# fleet_hosts_rewrite FILE OLD FQDN — print FILE with, on each 127.0.0.1/::1
# line, the OLD hostname token (and OLD's short form) replaced in place by
# "FQDN <short>"; every other alias on the line is kept. Then both loopback
# lines are appended when no line names FQDN. An empty OLD replaces nothing
# (the caller passes it for localhost[.localdomain], whose tokens must stay).
# Read-only.
fleet_hosts_rewrite() {
  awk -v old="$2" -v fqdn="$3" -v short="${3%%.*}" '
    BEGIN { oldshort = (index(old, ".") ? substr(old, 1, index(old, ".") - 1) : "") }
    function isold(t) { return t == old || (oldshort != "" && t == oldshort) }
    function names(n,   i) {
      for (i = 2; i <= NF; i++) { if ($i ~ /^#/) break; if ($i == n) return 1 }
      return 0
    }
    function hasold(   i) {
      for (i = 2; i <= NF; i++) { if ($i ~ /^#/) break; if (isold($i)) return 1 }
      return 0
    }
    ($1 == "127.0.0.1" || $1 == "::1") && old != "" && hasold() {
      line = $1; placed = 0; cmt = 0
      for (i = 2; i <= NF; i++) {
        if ($i ~ /^#/) cmt = 1
        if (!cmt && isold($i)) {
          if (!placed) { line = line " " fqdn; if (short != fqdn) line = line " " short; placed = 1 }
          continue
        }
        if (!cmt && ($i == fqdn || $i == short)) continue
        line = line " " $i
      }
      print line; seen = 1; next
    }
    { if ($1 !~ /^#/ && names(fqdn)) seen = 1; print }
    END { if (!seen) { print "127.0.0.1 " fqdn " " short; print "::1 " fqdn " " short } }
  ' "$1"
}

# fleet_set_hostname FQDN
fleet_set_hostname() {
  local fqdn="${1,,}" current old="" cfg hosts
  if [[ "${FLEET_SET_HOSTNAME:-1}" == "0" ]]; then
    info "FLEET_SET_HOSTNAME=0 — leaving the hostname alone"
    return 0
  fi
  command -v hostnamectl >/dev/null 2>&1 || return 0
  if [[ ! "$fqdn" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ \
    || "$fqdn" =~ ^[0-9.]+$ ]]; then
    info "hostname: '${1}' is not a dotted FQDN — leaving the hostname alone"
    return 0
  fi
  current="$(hostnamectl --static 2>/dev/null || true)"
  if [[ "$current" == "$fqdn" ]]; then
    ok "hostname already ${fqdn}"
  elif [[ "${FLEET_SET_HOSTNAME:-}" != "1" ]] && ! fleet_hostname_is_default "$current"; then
    info "hostname is ${current:-<unset>}, not ${fqdn} — left as is; to rename: hostnamectl set-hostname ${fqdn} (or FLEET_SET_HOSTNAME=1)"
    return 0
  elif _fleet_hn_do hostnamectl set-hostname "$fqdn"; then
    case "$current" in localhost | localhost.localdomain) ;; *) old="$current" ;; esac
    [[ "${DRY_RUN:-0}" == "1" ]] || ok "hostname ${current:-<unset>} → ${fqdn}"
  else
    warn "could not set the hostname — run: hostnamectl set-hostname ${fqdn}"
    return 0
  fi

  if [[ -d "$FLEET_CLOUD_DIR" ]]; then
    cfg="$FLEET_CLOUD_DIR/cloud.cfg.d/99-preserve-hostname.cfg"
    if [[ "$(cat "$cfg" 2>/dev/null || true)" != "$FLEET_PRESERVE_HOSTNAME_CFG" ]]; then
      if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "[dry-run] would write ${cfg} (preserve_hostname: true, manage_etc_hosts: false)"
      elif mkdir -p "${cfg%/*}" && printf '%s\n' "$FLEET_PRESERVE_HOSTNAME_CFG" > "$cfg"; then
        ok "cloud-init keeps the hostname (${cfg})"
      else
        warn "could not write ${cfg} — cloud-init may reset the hostname on reboot"
      fi
    fi
  fi

  [[ -f "$FLEET_HOSTS_FILE" ]] || return 0
  if ! hosts="$(fleet_hosts_rewrite "$FLEET_HOSTS_FILE" "$old" "$fqdn")"; then
    warn "could not read ${FLEET_HOSTS_FILE} — add: 127.0.0.1 ${fqdn} ${fqdn%%.*}"
    return 0
  fi
  [[ "$hosts" != "$(cat "$FLEET_HOSTS_FILE")" ]] || return 0
  if [[ "${DRY_RUN:-0}" == "1" ]]; then
    info "[dry-run] would point ${FLEET_HOSTS_FILE}'s loopback lines at ${fqdn}"
  elif printf '%s\n' "$hosts" > "$FLEET_HOSTS_FILE"; then
    ok "${FLEET_HOSTS_FILE} names ${fqdn} on 127.0.0.1 and ::1"
  else
    warn "could not update ${FLEET_HOSTS_FILE} — add: 127.0.0.1 ${fqdn} ${fqdn%%.*}"
  fi
  return 0
}
