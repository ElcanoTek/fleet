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
# (its first line), or failure when DIR carries no regular-file marker. The
# marker sits in the service user's tree, so a read through it could be
# pointed at something secret: a root-only file (/etc/fleet/fleet.env) for a
# root read, the running service's own /proc/<pid>/environ for a read as the
# service user. It is therefore opened with O_NOFOLLOW (dd iflag=nofollow),
# which refuses a symlink at open time — no check-then-read window — and read
# AS OWNER, bounded to 4 KiB, which a path never exceeds. It is opened
# non-blocking too (iflag=nonblock): a FIFO in its place then reads as empty
# or fails instead of hanging update.sh or doctor.sh, and anything that is not
# a regular file is refused outright. Only an absolute path without control
# bytes is returned (see below).
bundle_marker_source() {
  local m out
  m="$(bundle_staged_marker "$1")"
  [[ -f "$m" && ! -L "$m" ]] || return 1
  out="$(_bundle_as "$2" dd if="$m" iflag=nofollow,nonblock bs=4096 count=1 status=none 2>/dev/null)" || return 1
  out="${out%%$'\n'*}"
  # The value is printed by root-run update and doctor, so only an absolute
  # path free of control bytes is accepted: an escape sequence planted in the
  # marker must not erase or spoof their status lines.
  [[ "$out" == /* && "$out" != *[[:cntrl:]]* ]] || return 1
  printf '%s\n' "$out"
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
# no marker — someone's hand-placed bundle, never deleted here. Exit 3 is a
# refresh that failed part-way AND whose rollback failed too: DST is partly
# updated, the previous copy is kept beside it (the path is printed), and the
# caller must stop rather than restart the service onto it.
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
    prev="$(mktemp -d "${dst}.prev.XXXXXX")"
    # chmod first: the kept copy carries the modes of the tree, and a read-only
    # directory in it would otherwise survive the cleanup.
    newmarker="${marker}.new"
    trap '"'"'chmod -R u+w -- "$tmp" "$prev" 2>/dev/null; rm -rf -- "$tmp" "$prev"; rm -f -- "$newmarker"'"'"' EXIT
    tar -C "$tmp" --no-same-owner -xf -
    # Belt and braces: the archive is complete by construction, but a copy
    # without a manifest must still never reach the --delete sync.
    [[ -f "$tmp/manifest.yaml" ]] || { echo "stage_default_bundle: incomplete copy of the source — not syncing" >&2; exit 1; }
    # A tree this account cannot fully rewrite (a root-owned subtree from a
    # hand edit) would fail part-way, so it is refused before anything moves.
    if [[ -n "$(find "$dst" ! -user "$(id -un)" -print -quit 2>/dev/null)" ]]; then
      echo "stage_default_bundle: $dst holds files not owned by $(id -un) — not syncing (chown -R it, then re-run)" >&2
      exit 1
    fi
    # An in-place rsync that fails part-way (a full disk, an I/O error) has
    # already replaced and deleted some files, so the current copy is kept
    # first and put back, in place, on failure: running sandboxes mount its
    # directories, which a swap would empty under them. --checksum compares
    # content, not size+mtime (the archive carries the source mtimes, so an
    # edit that keeps both would otherwise be skipped); --delete-after keeps
    # removals last.
    cp -a -- "$dst/." "$prev/"
    # The new marker is written before anything in DST changes, so a full
    # disk fails here rather than after the sync, which would leave a copy
    # with no marker that neither bootstrap nor update would recognise.
    rm -f -- "$newmarker"
    printf "%s\n" "$src" > "$newmarker"
    sync_from() { rsync -a --checksum --delete --delete-after --no-owner --no-group --exclude "/$(basename "$marker")" --exclude "/$(basename "$marker").new" "$1/" "$dst/"; }
    if ! sync_from "$tmp"; then
      echo "stage_default_bundle: sync into $dst failed — restoring the previous copy" >&2
      rm -f -- "$newmarker"
      if ! sync_from "$prev"; then
        # The restore failed too (the same full disk, say): DST is part old,
        # part new. The kept copy is the only good one left, so it is NOT
        # cleaned up, and exit 3 tells the caller to stop rather than restart
        # the service onto a half-written bundle.
        trap - EXIT
        chmod -R u+w -- "$tmp" 2>/dev/null; rm -rf -- "$tmp"
        echo "stage_default_bundle: could not restore $dst either — it is partly updated; the previous copy is kept at $prev (restore it with: rsync -a --delete $prev/ $dst/)" >&2
        exit 3
      fi
      exit 1
    fi
    # Swap the prepared marker in by rename: no space needed at this point,
    # and a planted symlink at the marker path is replaced, not followed.
    mv -f -- "$newmarker" "$marker"
  ' _ "$src" "$dst" "$marker" < "$archive"
  local rc=$?
  rm -f -- "$archive"
  case "$rc" in 0 | 3) return "$rc" ;; *) return 1 ;; esac
}
