# shellcheck shell=bash
# scripts/lib/bundle.sh — where the fleet service's client bundle lives, and the
# staging of the in-repo default bundle for a bare install.
#
# Sourced (not executed) by bootstrap.sh, update.sh and doctor.sh.
#
# Why this exists (#1655). The sandbox bind-mounts bundle dirs (protocols/,
# personas/, skills/, system_prompts/) into every container with an SELinux
# relabel (`:z`), which asks podman — running rootless as the service user —
# to write the security.selinux xattr on the SOURCE. That needs two things the
# in-repo default bundle under the fleet checkout does not have: ownership by
# the service user (rootless relabel is refused on files it does not own) and a
# path the unit can write (deploy/fleet.service runs with ProtectSystem=strict
# and only /var/lib/fleet and /opt/fleet/client writable — under that mount
# namespace the checkout is read-only, so the relabel fails with EROFS and
# every Pool.fill exits 126). A `--client-config` checkout is chowned to the
# service user and lives at /opt/fleet/client, so it works; a bare install on
# config/default did not, on every SELinux-enforcing host.
#
# The fix is a service-owned COPY of the default bundle under the state dir,
# ${SERVICE_HOME}/bundle, staged by bootstrap and refreshed by update (the
# checkout stays pristine and root-owned). Only the generic config/default is
# ever staged: a client bundle someone placed inside the checkout is left where
# it is, with a warning, rather than copied and later overwritten by the
# generic one. The copy carries a marker naming its source, and nothing here
# ever deletes a non-empty directory that lacks the marker — a bundle an
# operator placed at the staging path by hand is theirs.

# default_bundle_stage SERVICE_HOME — the staged copy's path.
default_bundle_stage() { printf '%s/bundle' "${1:-/var/lib/fleet}"; }

# bundle_staged_marker DIR — the marker file a staged copy carries.
bundle_staged_marker() { printf '%s/.fleet-staged-from' "$1"; }

_bundle_norm() { readlink -f -- "$1" 2>/dev/null || printf '%s' "$1"; }

# bundle_is_default_in_checkout DIR REPO_ROOT — true when DIR is exactly the
# in-repo generic bundle, REPO_ROOT/config/default (paths normalised).
bundle_is_default_in_checkout() {
  [[ -n "${1:-}" && -n "${2:-}" ]] || return 1
  [[ "$(_bundle_norm "$1")" == "$(_bundle_norm "$2")/config/default" ]]
}

# bundle_is_in_checkout DIR REPO_ROOT — true when DIR is anywhere inside the
# fleet checkout (the generic bundle, or a client bundle someone put there).
bundle_is_in_checkout() {
  [[ -n "${1:-}" && -n "${2:-}" ]] || return 1
  case "$(_bundle_norm "$1")" in "$(_bundle_norm "$2")"/*) return 0 ;; esac
  return 1
}

# bundle_looks_like_fleet_default DIR — DIR is a config/default inside SOME
# fleet checkout (the go.mod two levels up names the fleet module). For doctor,
# which cannot know which checkout the service was installed from.
bundle_looks_like_fleet_default() {
  [[ -n "${1:-}" ]] || return 1
  [[ "$(basename -- "$1")" == "default" && "$(basename -- "$(dirname -- "$1")")" == "config" ]] || return 1
  grep -qs '^module github.com/ElcanoTek/fleet$' "$1/../../go.mod"
}

# _bundle_as OWNER CMD... — run CMD as OWNER: directly when this shell already
# is OWNER (the tests), through runuser when it is root (bootstrap, update).
_bundle_as() {
  local owner="$1"; shift
  if [[ "$(id -u)" == "$(id -u "$owner" 2>/dev/null)" ]]; then
    "$@"
  elif [[ "$(id -u)" == "0" ]] && command -v runuser >/dev/null 2>&1; then
    runuser -u "$owner" -- "$@"
  else
    echo "stage_default_bundle: cannot act as $owner (need root and runuser)" >&2
    return 1
  fi
}

# bundle_marker_source DIR OWNER — the source a staged copy's marker names
# (its first line), or failure when DIR carries no regular-file marker. Read AS
# OWNER and never through a symlink: the marker sits in the service user's
# tree, so a root read could be pointed (marker → /etc/fleet/fleet.env) at a
# root-only file and print it. Bounded to 4 KiB, which a path never exceeds.
bundle_marker_source() {
  local m
  m="$(bundle_staged_marker "$1")"
  [[ -f "$m" && ! -L "$m" ]] || return 1
  _bundle_as "$2" head -c 4096 -- "$m" 2>/dev/null | head -n1
}

# bundle_writable_in_unit DIR UNIT STATE_DIR — true when DIR lies inside a path
# the unit can write: its ReadWritePaths (as systemd reports them, else the
# shipped unit's /var/lib/fleet and /opt/fleet/client) or its state dir. Under
# ProtectSystem=strict everything else is read-only to the service, so the
# sandbox's :z relabel of a bundle there fails with EROFS on an SELinux host,
# however the files are owned.
bundle_writable_in_unit() {
  local dir="$1" unit="$2" state="$3" paths p d
  paths="$(systemctl show -p ReadWritePaths --value "${unit}.service" 2>/dev/null || true)"
  [[ -n "$paths" ]] || paths="/var/lib/fleet -/opt/fleet/client"
  d="$(_bundle_norm "$dir")"
  for p in $paths $state; do
    p="${p#-}"
    [[ -n "$p" ]] || continue
    p="$(_bundle_norm "$p")"
    [[ "$d" == "$p" || "$d" == "$p"/* ]] && return 0
  done
  return 1
}

# stage_default_bundle SRC DST OWNER — make DST a copy of SRC owned by OWNER,
# with the marker recording SRC. Idempotent.
#
# Refuses (exit 1) a SRC without manifest.yaml, a DST that is not an absolute
# path below /, a DST that is a symlink, an OWNER that does not exist, and a
# box without rsync; and (exit 2) a DST that exists, is non-empty and carries
# no marker — someone's hand-placed bundle, never deleted here.
#
# Every write happens AS OWNER. DST sits in the service user's own state dir,
# so that account controls every path component below it: a root process that
# wrote there could be redirected by a planted symlink (the marker pointed at
# /etc/fleet/fleet.env, say) into truncating a root-only file. Root only READS
# the source here — archived whole into a root-private temp file, because the
# checkout may sit somewhere OWNER cannot read — and OWNER unpacks it into a scratch dir beside DST and
# syncs it IN PLACE with rsync --delete (files replaced, directories kept), so
# a running service's bind mounts of DST's subdirectories keep valid content
# until its containers are recycled. There is no rename-and-delete fallback
# without rsync: deleting the old tree would empty those mounts under running
# sandboxes, so a box without rsync is refused instead (bootstrap installs it).
#
# Labels are left alone: the copy takes the state dir's default context, and
# the sandbox's `:z` relabels the mounted dirs on first use. A restorecon here
# would reset an already-relabelled copy under running containers, which then
# lose read access until they are recycled.
stage_default_bundle() {
  local src="$1" dst="$2" owner="$3" marker
  [[ -f "$src/manifest.yaml" ]] || { echo "stage_default_bundle: no manifest.yaml at $src" >&2; return 1; }
  [[ "$dst" == /?* ]] || { echo "stage_default_bundle: refusing destination '$dst' (must be an absolute path below /)" >&2; return 1; }
  [[ -L "$dst" ]] && { echo "stage_default_bundle: $dst is a symlink — not staging onto it" >&2; return 1; }
  id -u "$owner" >/dev/null 2>&1 || { echo "stage_default_bundle: no such user $owner" >&2; return 1; }
  command -v rsync >/dev/null 2>&1 || { echo "stage_default_bundle: rsync is required to refresh the copy in place (dnf/apt install rsync)" >&2; return 1; }
  marker="$(bundle_staged_marker "$dst")"
  if [[ -d "$dst" && -n "$(ls -A "$dst" 2>/dev/null)" && ! -f "$marker" ]]; then
    echo "stage_default_bundle: $dst holds a bundle that is not a staged copy (no $(basename "$marker")) — not touching it" >&2
    return 2
  fi
  # The archive is written in full, by root, into a root-private temp file
  # before OWNER sees a byte: a reading tar that fails part-way (a source file
  # vanishing mid-read) can still emit a well-formed partial archive, and a
  # stream would hand that to the --delete sync below before the failure was
  # known. Only an archive whose producer exited 0 is unpacked.
  local archive
  archive="$(mktemp)" || return 1
  if ! tar -C "$src" --exclude="./$(basename "$marker")" -cf "$archive" .; then
    rm -f -- "$archive"
    echo "stage_default_bundle: could not read $src — not staging" >&2
    return 1
  fi
  # shellcheck disable=SC2016 # the script is single-quoted on purpose: it runs as OWNER with its own $1..$3
  _bundle_as "$owner" bash -c '
    set -euo pipefail
    src="$1" dst="$2" marker="$3"
    [[ -L "$dst" ]] && { echo "stage_default_bundle: $dst is a symlink — not staging onto it" >&2; exit 1; }
    mkdir -p -m 0755 -- "$dst"
    tmp="$(mktemp -d "${dst}.new.XXXXXX")"
    trap '"'"'rm -rf -- "$tmp"'"'"' EXIT
    tar -C "$tmp" --no-same-owner -xf -
    # Belt and braces: the archive is complete by construction, but a copy
    # without a manifest must still never reach the --delete sync.
    [[ -f "$tmp/manifest.yaml" ]] || { echo "stage_default_bundle: incomplete copy of the source — not syncing" >&2; exit 1; }
    rsync -a --delete --no-owner --no-group --exclude "/$(basename "$marker")" "$tmp/" "$dst/"
    rm -f -- "$marker"
    printf "%s\n" "$src" > "$marker"
  ' _ "$src" "$dst" "$marker" < "$archive"
  local rc=$?
  rm -f -- "$archive"
  return "$(( rc == 0 ? 0 : 1 ))"
}
