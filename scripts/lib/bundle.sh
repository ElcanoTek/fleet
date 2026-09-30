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

# stage_default_bundle SRC DST OWNER — make DST a copy of SRC owned by OWNER
# (OWNER's login group), with the marker recording SRC. Idempotent.
#
# Refuses (exit 1) a SRC without manifest.yaml, a DST that is not an absolute
# path below /, a DST that is a symlink, and an OWNER that does not exist; and
# (exit 2) a DST that exists, is non-empty and carries no marker — someone's
# hand-placed bundle, never deleted here.
#
# The copy is synced IN PLACE with rsync (files replaced, directories kept) so
# a running service's bind mounts of DST's subdirectories stay valid until its
# containers are recycled; without rsync the copy is built beside DST and
# swapped in by rename, which keeps the old tree's inodes alive for mounts
# already holding them. Neither preserves the source's SELinux context: the
# copy takes the state dir's label (and restorecon, where present, settles
# it), and the sandbox's `:z` relabels the mounted dirs on first use.
stage_default_bundle() {
  local src="$1" dst="$2" owner="$3" marker
  [[ -f "$src/manifest.yaml" ]] || { echo "stage_default_bundle: no manifest.yaml at $src" >&2; return 1; }
  [[ "$dst" == /?* ]] || { echo "stage_default_bundle: refusing destination '$dst' (must be an absolute path below /)" >&2; return 1; }
  [[ -L "$dst" ]] && { echo "stage_default_bundle: $dst is a symlink — not staging onto it" >&2; return 1; }
  id -u "$owner" >/dev/null 2>&1 || { echo "stage_default_bundle: no such user $owner" >&2; return 1; }
  marker="$(bundle_staged_marker "$dst")"
  if [[ -d "$dst" && -n "$(ls -A "$dst" 2>/dev/null)" && ! -f "$marker" ]]; then
    echo "stage_default_bundle: $dst holds a bundle that is not a staged copy (no $(basename "$marker")) — not touching it" >&2
    return 2
  fi
  install -d -m 0755 -o "$owner" "$dst" || return 1
  if command -v rsync >/dev/null 2>&1; then
    rsync -a --delete --no-owner --no-group --exclude "/$(basename "$marker")" "$src/" "$dst/" || return 1
  else
    local tmp old
    tmp="$(mktemp -d "${dst}.new.XXXXXX")" || return 1
    if ! cp -a --no-preserve=context,ownership "$src/." "$tmp/"; then rm -rf "$tmp"; return 1; fi
    old="${dst}.old.$$"
    if ! { mv -T "$dst" "$old" && mv -T "$tmp" "$dst"; }; then rm -rf "$tmp"; return 1; fi
    rm -rf "$old"
  fi
  printf '%s\n' "$src" > "$marker" || return 1
  chown -R "$owner": "$dst" || return 1
  if command -v restorecon >/dev/null 2>&1; then restorecon -R "$dst" >/dev/null 2>&1 || true; fi
  return 0
}
