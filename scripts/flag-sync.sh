#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 ElcanoTek, Inc. (canonical copy: ElcanoTek/flag tools/product/)
#
# flag-sync.sh — vendor files from ElcanoTek/flag (the Flag design system) and
# prove they have not drifted. This script is shared verbatim by every Elcano
# repo; change it in all of them at once (bump FLAG_SYNC_VERSION) or not at all.
#
# The lock file (flag.lock at the repo root) records which flag commit the
# vendored files came from and the sha256 of every file:
#
#   flag <commit-sha>                       flag commit the files were copied from
#   dir  <repo-dir>                         every file under this dir must be locked
#   file <sha256> <flag-path> <repo-path>   one vendored file
#
# <flag-path> is relative to flag's design-system/ directory; <repo-path> is
# relative to the repo root. Blank lines and lines starting with # are ignored.
#
# Commands:
#   check                      offline drift gate (CI): every locked file exists
#                              with its locked hash, and no unlocked file sits in
#                              a locked dir. Never edit vendored files by hand.
#   sync   [FLAG_DIR]          re-copy every locked file from a flag checkout
#                              (default: $FLAG_DIR, else ../flag) and rewrite the
#                              lock with that checkout's commit. A locked `dir`
#                              mirrors its flag directory: new upstream files are
#                              added and files flag removed are deleted.
#   add    [FLAG_DIR] SRC DEST copy flag file or directory SRC to repo path DEST
#                              and lock it (a directory also becomes a `dir`).
#   status [FLAG_DIR]          report locked files that differ from the flag
#                              checkout, and files flag added to a locked `dir`
#                              (exit 3 when the vendor copy is stale).
#
# Safety model (v10): files are read from flag's git objects at the checkout's
# HEAD commit (git ls-tree / cat-file), never through its working tree, so the
# lock's commit line always names exactly the bytes that were copied, and a
# symlink in flag can never make this script read outside it (flag symlinks
# and submodules are refused). On the repo side every path is a plain relative
# path (no "..", ".", absolute, whitespace, backslash or .git segment), every
# existing component must be a real directory (no symlinks) and every target
# a regular file. All checks run before the first write; files are staged,
# then installed with an atomic rename, and the lock is written last.
# (A process racing this script with write access to the repo is out of scope.)
set -euo pipefail
set -f # lock paths may contain glob characters ([ ] * ?); never expand them
unset CDPATH GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE
export LC_ALL=C GIT_LITERAL_PATHSPECS=1

FLAG_SYNC_VERSION=16
ROOT="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
LOCK="$ROOT/flag.lock"

die() { echo "flag-sync: $*" >&2; exit 2; }

# Temp files and dirs are tracked globally and removed on any exit (die
# included); newtmp/newtmpdir assign by name so the list lives in this shell.
TMPS=()
cleanup() { if [ "${#TMPS[@]}" -gt 0 ]; then rm -rf -- "${TMPS[@]}"; fi; }
trap cleanup EXIT
# (Never call them inside $(...): a subshell's list is lost.)
newtmp() { local _nt; _nt="$(mktemp "${2:-${TMPDIR:-/tmp}}/.flag-sync.XXXXXX")" || die "mktemp failed"; TMPS+=("$_nt"); printf -v "$1" '%s' "$_nt"; }
newtmpdir() { local _nt; _nt="$(mktemp -d "${TMPDIR:-/tmp}/flag-sync.XXXXXX")" || die "mktemp failed"; TMPS+=("$_nt"); printf -v "$1" '%s' "$_nt"; }

# sha_in / sha FILE: sha256 of stdin / of FILE read on stdin (so no file name
# quoting in the output); anything but 64 hex digits is a failure, never an
# empty or mangled hash.
sha_in() {
  local h
  if command -v sha256sum >/dev/null 2>&1; then h="$(sha256sum)" || return 1
  else h="$(shasum -a 256)" || return 1; fi
  h="${h%% *}"
  [[ "$h" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "$h"
}
sha() { sha_in < "$1"; }

# safe_path PATH: lock records are whitespace-separated, so a path must be a
# plain relative path with no whitespace, control characters, backslash,
# empty, "." or ".." segment, and nothing inside .git.
safe_path() {
  local lower
  case "$1" in
    ''|/*|*/|*//*|*\\*|*[[:space:][:cntrl:]]*) die "unsupported path '$1' (relative paths without whitespace or backslashes only)" ;;
  esac
  case "/$1/" in */../*|*/./*) die "unsupported path '$1' (no . or .. segments)" ;; esac
  lower="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  case "/$lower/" in */.git/*) die "unsupported path '$1' (inside .git)" ;; esac
}

# repo_unsafe REL file|dir: prints why $ROOT/REL cannot be used and succeeds,
# or fails when it is safe: every existing component is a real directory (not
# a symlink) and the leaf, if it exists, is a regular file / real directory.
repo_unsafe() {
  local rest="$1" p="$ROOT" seg
  while :; do
    case "$rest" in */*) seg="${rest%%/*}"; rest="${rest#*/}" ;; *) seg="$rest"; rest="" ;; esac
    p="$p/$seg"
    if [ -L "$p" ]; then echo "symlink at ${p#"$ROOT"/}"; return 0; fi
    if [ -z "$rest" ]; then
      [ -e "$p" ] || return 1
      case "$2" in
        file) [ -f "$p" ] || { echo "not a regular file"; return 0; } ;;
        dir) [ -d "$p" ] || { echo "not a directory"; return 0; } ;;
      esac
      return 1
    fi
    [ -e "$p" ] || return 1
    [ -d "$p" ] || { echo "${p#"$ROOT"/} is not a directory"; return 0; }
  done
}

need_repo_path() { local r; if r="$(repo_unsafe "$1" "$2")"; then die "refusing to touch $1: $r"; fi; }

# need_plan_path REPO_PATH DELETES: like need_repo_path for a file apply will
# install, except that an obstacle the deletes remove first is fine: a parent
# that is a locked file being deleted (flag turned that file into a directory),
# or a directory whose files are all being deleted (flag turned it into a file).
need_plan_path() {
  local r x f
  r="$(repo_unsafe "$1" file)" || return 0
  case "$r" in
    *" is not a directory")
      x="${r% is not a directory}"
      grep -qxF -- "$x" "$2" && [ -f "$ROOT/$x" ] && [ ! -L "$ROOT/$x" ] && return 0 ;;
    "not a regular file")
      if [ -d "$ROOT/$1" ] && [ ! -L "$ROOT/$1" ]; then
        while IFS= read -r -d '' f; do
          { [ -f "$f" ] && [ ! -L "$f" ] && grep -qxF -- "${f#"$ROOT"/}" "$2"; } || die "refusing to touch $1: it is a directory holding ${f#"$ROOT"/}, which is not being replaced"
        done < <(find "$ROOT/$1" -mindepth 1 ! -type d -print0)
        return 0
      fi ;;
  esac
  die "refusing to touch $1: $r"
}

# ENTRIES: the validated lock, one normalized "kind a b c" record per line.
# Every command reads it with a plain redirect, so die always stops the script.
ENTRIES=""
validate_lock() {
  local raw kind a b c extra rc=0 flags=0 d e
  newtmp ENTRIES
  [ -e "$LOCK" ] || [ -L "$LOCK" ] || return 0
  if [ -L "$LOCK" ] || [ ! -f "$LOCK" ]; then die "flag.lock must be a regular file (not a symlink)"; fi
  [ "$(tr -d '\000' < "$LOCK" | wc -c)" -eq "$(wc -c < "$LOCK")" ] || die "flag.lock contains NUL bytes"
  newtmp raw
  grep -a -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$LOCK" > "$raw" || rc=$?
  [ "$rc" -le 1 ] || die "cannot read flag.lock"
  while read -r kind a b c extra; do
    case "$kind" in
      flag) if ! [[ "$a" =~ ^[0-9a-f]{40}$ ]] || [ -n "$b" ]; then die "flag.lock: bad flag line '$kind $a $b'"; fi
            flags=$((flags + 1)); echo "flag $a" >> "$ENTRIES" ;;
      dir) if [ -z "$a" ] || [ -n "$b" ]; then die "flag.lock: bad dir line '$kind $a $b'"; fi
           safe_path "$a"; echo "dir $a" >> "$ENTRIES" ;;
      file) if ! [[ "$a" =~ ^[0-9a-f]{64}$ ]] || [ -z "$c" ] || [ -n "$extra" ]; then die "flag.lock: bad file line '$kind $a $b $c'"; fi
            safe_path "$b"; safe_path "$c"; echo "file $a $b $c" >> "$ENTRIES" ;;
      *) die "flag.lock: unknown entry '$kind'" ;;
    esac
  done < "$raw"
  [ "$flags" -le 1 ] || die "flag.lock: more than one flag line"
  d="$(awk '$1 == "file" { print $4 }' "$ENTRIES" | sort | uniq -d | head -1)"
  [ -z "$d" ] || die "flag.lock: $d is locked more than once"
  d="$(awk '$1 == "dir" { print $2 }' "$ENTRIES" | sort | uniq -d | head -1)"
  [ -z "$d" ] || die "flag.lock: dir $d is listed more than once"
  while read -r d; do
    while read -r e; do
      case "$e" in "$d"/*) die "flag.lock: dir $e is nested inside dir $d" ;; esac
    done < <(awk '$1 == "dir" { print $2 }' "$ENTRIES")
  done < <(awk '$1 == "dir" { print $2 }' "$ENTRIES")
}

need_lock() { [ -f "$LOCK" ] || die "missing $LOCK"; }

# in_plan PLAN PATH: does PLAN ("mode oid flag-path repo-path" records) write
# PATH? Exact string comparison (ENVIRON, not -v, so backslashes stay literal).
in_plan() { P="$2" awk '$4 == ENVIRON["P"] { found = 1 } END { exit !found }' "$1"; }

# dir_prefix REPO_DIR: sets PREFIX to the flag directory a locked `dir`
# mirrors, derived from the locked files under it (repo path REPO_DIR/rel <->
# flag path PREFIX/rel). Files that disagree make it ambiguous: refuse.
PREFIX=""
dir_prefix() {
  local d="$1" kind a b c rel p
  PREFIX=""
  while read -r kind a b c; do
    [ "$kind" = file ] || continue
    case "$c" in "$d"/*) ;; *) continue ;; esac
    rel="${c#"$d"/}"
    case "$b" in */"$rel") p="${b%/"$rel"}" ;; *) continue ;; esac
    if [ -z "$PREFIX" ]; then PREFIX="$p"
    elif [ "$PREFIX" != "$p" ]; then die "locked dir $d mirrors both design-system/$PREFIX and design-system/$p; fix flag.lock by hand"; fi
  done < "$ENTRIES"
}

# --- the flag side: git objects of one commit -------------------------------

FD=""; COMMIT=""
# open_flag [DIR]: FD = top of a flag git checkout, COMMIT = its HEAD.
open_flag() {
  local d="${1:-${FLAG_DIR:-$ROOT/../flag}}" top
  [ -d "$d" ] || die "no flag checkout at $d (clone ElcanoTek/flag there or pass its path)"
  FD="$(cd -P -- "$d" && pwd -P)" || die "cannot enter $d"
  top="$(git -C "$FD" rev-parse --show-toplevel 2>/dev/null)" || die "$d is not a git checkout of ElcanoTek/flag; the lock must name a real commit"
  top="$(cd -P -- "$top" && pwd -P)" || die "cannot enter $top"
  [ "$top" = "$FD" ] || die "$d is not the top of a git checkout (it is inside $top)"
  COMMIT="$(git -C "$FD" rev-parse --verify -q 'HEAD^{commit}')" || die "$d has no commits"
  [[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "$d: unsupported commit id $COMMIT"
  [ "$(git -C "$FD" cat-file -t "$COMMIT:design-system" 2>/dev/null)" = tree ] || die "no design-system/ directory in $d at ${COMMIT:0:12}"
}

# clean_flag: refuse a checkout whose design-system/ has uncommitted or
# untracked changes; the files come from HEAD, which would surprise the user.
clean_flag() {
  local st
  st="$(git -C "$FD" status --porcelain --untracked-files=all -- design-system)" || die "git status failed in $FD"
  [ -z "$st" ] || die "$FD has uncommitted changes under design-system/; commit or stash them so flag.lock names a commit that reproduces the files"
}

# flag_entry PATH: sets E_MODE and E_OID for design-system/PATH at COMMIT
# (both empty when flag does not have it).
E_MODE=""; E_OID=""
flag_entry() {
  local out meta n
  out="$(git -C "$FD" ls-tree "$COMMIT" -- "design-system/$1")" || die "git ls-tree failed in $FD"
  E_MODE=""; E_OID=""
  [ -n "$out" ] || return 0
  n="$(printf '%s\n' "$out" | wc -l)"
  [ "$n" -eq 1 ] || die "unexpected git ls-tree output for design-system/$1"
  meta="${out%%$'\t'*}"; E_MODE="${meta%% *}"; E_OID="${meta##* }"
}

# flag_list PREFIX OUT: every entry under design-system/PREFIX at COMMIT as
# "mode oid rel" lines (rel relative to PREFIX). Flag paths that cannot be
# locked (whitespace, ...) stop the script; .DS_Store is skipped.
flag_list() {
  local raw rec meta path rel
  newtmp raw
  git -C "$FD" ls-tree -r -z --full-tree "$COMMIT" -- "design-system/$1" > "$raw" || die "git ls-tree failed in $FD"
  : > "$2"
  while IFS= read -r -d '' rec; do
    meta="${rec%%$'\t'*}"; path="${rec#*$'\t'}"
    rel="${path#"design-system/$1/"}"
    [ "$rel" != "$path" ] || die "unexpected git ls-tree path $path"
    case "/$rel" in */.DS_Store) continue ;; esac
    safe_path "$1/$rel"
    echo "${meta%% *} ${meta##* } $rel" >> "$2"
  done < "$raw"
}

need_blob() { case "$1" in 100644|100755) ;; 120000) die "flag has a symlink at design-system/$2; refusing to vendor it" ;; *) die "design-system/$2 in flag is not a regular file (mode $1)" ;; esac; }

blob_sha() { git -C "$FD" cat-file blob "$1" | sha_in; }

# --- writing the repo --------------------------------------------------------

# apply PLAN DELETES: PLAN has "mode oid flag-path repo-path" lines. Every
# target was vetted; stage all blobs first (hashing them), then delete (so a
# file flag turned into a directory, or the reverse, is out of the way), then
# install each with an atomic rename. Appends "sha flag-path repo-path" to $FILES.
apply() {
  local stage i=0 mode oid b c d h dst t
  newtmpdir stage
  while read -r mode oid b c; do
    i=$((i + 1))
    git -C "$FD" cat-file blob "$oid" > "$stage/$i" || die "git cat-file failed for design-system/$b"
    h="$(sha "$stage/$i")" || die "cannot hash design-system/$b"
    echo "$h $b $c" >> "$FILES"
  done < "$1"
  while IFS= read -r c; do
    need_repo_path "$c" file
    rm -f -- "$ROOT/$c"
    # Drop directories this emptied, so a path flag turned from a directory
    # into a file can be installed (mkdir -p restores any that are still needed).
    d="$(dirname -- "$c")"
    while [ "$d" != . ] && rmdir -- "$ROOT/$d" 2>/dev/null; do d="$(dirname -- "$d")"; done
  done < "$2"
  i=0
  while read -r mode oid b c; do
    i=$((i + 1))
    dst="$ROOT/$c"
    # A directory flag turned into a file: its files were deleted above, and
    # need_plan_path vetted that nothing else but empty directories remain.
    if [ -d "$dst" ] && [ ! -L "$dst" ]; then find "$dst" -depth -type d -empty -delete; fi
    need_repo_path "$c" file
    mkdir -p -- "$(dirname -- "$dst")"
    need_repo_path "$c" file
    newtmp t "$(dirname -- "$dst")"
    cat "$stage/$i" > "$t"
    if [ "$mode" = 100755 ]; then chmod 755 "$t"; else chmod 644 "$t"; fi
    mv -f -- "$t" "$dst"
  done < "$1"
}

# write_lock COMMIT DIRS_FILE FILES_FILE
write_lock() {
  local t
  if [ -L "$LOCK" ] || { [ -e "$LOCK" ] && [ ! -f "$LOCK" ]; }; then die "flag.lock must be a regular file (not a symlink)"; fi
  newtmp t "$ROOT"
  {
    echo "# Vendored from ElcanoTek/flag (design-system/). Generated by scripts/flag-sync.sh v$FLAG_SYNC_VERSION."
    echo "# Do not edit vendored files or this lock by hand: run scripts/flag-sync.sh sync, then commit both."
    echo "flag $1"
    sort -u "$2" | sed 's/^/dir /'
    sort -k3 "$3" | sed 's/^/file /'
  } > "$t"
  chmod 644 "$t"
  mv -f -- "$t" "$LOCK"
}

# --- commands ----------------------------------------------------------------

cmd_check() {
  local bad=0 kind a b c locked f r
  need_lock
  # A truncated or emptied lock must not pass: exactly one flag record and at
  # least one file record, or nothing below proves anything.
  [ "$(awk '$1 == "flag"' "$ENTRIES" | wc -l)" -eq 1 ] || { echo "flag-sync: flag.lock must have exactly one 'flag <commit>' line" >&2; exit 1; }
  [ "$(awk '$1 == "file"' "$ENTRIES" | wc -l)" -ge 1 ] || { echo "flag-sync: flag.lock locks no files" >&2; exit 1; }
  newtmp locked
  while read -r kind a b c; do
    [ "$kind" = file ] || continue
    echo "$c" >> "$locked"
    if r="$(repo_unsafe "$c" file)"; then echo "UNEXPECTED $c ($r)"; bad=1
    elif [ ! -f "$ROOT/$c" ]; then echo "MISSING  $c (flag: $b)"; bad=1
    elif [ "$(sha "$ROOT/$c" || true)" != "$a" ]; then echo "DRIFTED  $c (flag: $b) — re-run scripts/flag-sync.sh sync; never hand-edit"; bad=1
    fi
  done < "$ENTRIES"
  while read -r kind a _; do
    [ "$kind" = dir ] || continue
    if r="$(repo_unsafe "$a" dir)"; then echo "UNEXPECTED dir $a ($r)"; bad=1; continue; fi
    [ -d "$ROOT/$a" ] || { echo "MISSING  dir $a"; bad=1; continue; }
    while IFS= read -r -d '' f; do
      r="${f#"$ROOT"/}"
      if [ -L "$f" ] || [ ! -f "$f" ]; then
        echo "UNEXPECTED $r (symlink or special file inside vendored dir $a)"; bad=1
      elif ! grep -qxF -- "$r" "$locked"; then
        echo "UNLOCKED $r (inside vendored dir $a — move product files out, or add it from flag)"; bad=1
      fi
    done < <(find "$ROOT/$a" -mindepth 1 ! -type d -print0)
  done < "$ENTRIES"
  if [ "$bad" -ne 0 ]; then echo "flag-sync: vendored Flag files drifted from flag.lock" >&2; exit 1; fi
  echo "flag-sync: $(awk '$1 == "file"' "$ENTRIES" | wc -l | tr -d ' ') vendored files match flag.lock ($(awk '$1 == "flag" { print substr($2, 1, 12) }' "$ENTRIES"))"
}

# status_scan: prints what differs between the lock and COMMIT; sets STALE.
STALE=0
# shellcheck disable=SC2094 # awk only reads ENTRIES
status_scan() {
  local kind a b c list mode oid rel
  STALE=0
  while read -r kind a b c; do
    [ "$kind" = file ] || continue
    flag_entry "$b"
    if [ -z "$E_MODE" ]; then echo "GONE     $b (removed upstream; locked as $c)"; STALE=1
    elif [ "$E_MODE" != 100644 ] && [ "$E_MODE" != 100755 ]; then echo "UNSUPPORTED design-system/$b (mode $E_MODE: not a regular file; locked as $c)"; STALE=1
    elif [ "$(blob_sha "$E_OID" || echo failed)" != "$a" ]; then echo "STALE    $c (flag changed design-system/$b)"; STALE=1
    fi
  done < "$ENTRIES"
  newtmp list
  while read -r kind a _; do
    [ "$kind" = dir ] || continue
    dir_prefix "$a"
    [ -n "$PREFIX" ] || { echo "UNKNOWN  dir $a (no locked file under it names its flag directory)"; STALE=1; continue; }
    flag_entry "$PREFIX"
    [ "$E_MODE" = 040000 ] || { echo "GONE     design-system/$PREFIX (locked as dir $a)"; STALE=1; continue; }
    flag_list "$PREFIX" "$list"
    while read -r mode oid rel; do
      if [ "$mode" != 100644 ] && [ "$mode" != 100755 ]; then echo "UNSUPPORTED design-system/$PREFIX/$rel (mode $mode: not a regular file)"; STALE=1; continue; fi
      F="$PREFIX/$rel" R="$a/$rel" awk '$1 == "file" && $3 == ENVIRON["F"] && $4 == ENVIRON["R"] { found = 1 } END { exit !found }' "$ENTRIES" \
        || { echo "NEW      design-system/$PREFIX/$rel (flag added it to vendored dir $a)"; STALE=1; }
    done < "$list"
  done < "$ENTRIES"
}

cmd_status() {
  need_lock
  open_flag "${1:-}"
  status_scan
  if [ "$STALE" -ne 0 ]; then echo "flag-sync: vendored copy is behind flag ${COMMIT:0:12}; run scripts/flag-sync.sh sync" >&2; exit 3; fi
  echo "flag-sync: vendored copy matches flag ${COMMIT:0:12}"
}

FILES=""
cmd_sync() {
  local dirs plan dels list kind a b c d mode oid rel
  need_lock
  open_flag "${1:-}"; clean_flag
  newtmp dirs; newtmp plan; newtmp dels; newtmp list; newtmp FILES
  awk '$1 == "dir" { print $2 }' "$ENTRIES" > "$dirs"
  # Plan everything first; nothing is written until every path is vetted.
  while read -r kind a b c; do
    [ "$kind" = file ] || continue
    flag_entry "$b"
    if [ -z "$E_MODE" ] || [ "$E_MODE" = 040000 ]; then
      # A file in a locked dir that flag removed (or replaced with a directory)
      # is dropped here and the mirror below plans what is there now; a single
      # locked file flag removed is an error, not a silent drop.
      while read -r d; do case "$c" in "$d"/*) continue 2 ;; esac; done < "$dirs"
      [ -z "$E_MODE" ] || die "flag replaced design-system/$b (locked as $c) with a directory; add the directory instead"
      die "flag no longer has design-system/$b (locked as $c); remove that line or pick its replacement"
    fi
    need_blob "$E_MODE" "$b"
    echo "$E_MODE $E_OID $b $c" >> "$plan"
  done < "$ENTRIES"
  # Mirror locked dirs: add files flag added, drop files flag removed.
  while read -r d; do
    dir_prefix "$d"
    [ -n "$PREFIX" ] || die "cannot tell which flag directory $d mirrors (no locked file under it)"
    flag_entry "$PREFIX"
    [ "$E_MODE" = 040000 ] || die "flag no longer has design-system/$PREFIX (locked as dir $d)"
    need_repo_path "$d" dir
    flag_list "$PREFIX" "$list"
    while read -r mode oid rel; do
      safe_path "$d/$rel"
      need_blob "$mode" "$PREFIX/$rel"
      in_plan "$plan" "$d/$rel" && continue
      echo "$mode $oid $PREFIX/$rel $d/$rel" >> "$plan"
    done < "$list"
  done < "$dirs"
  # Only files the old lock vendored are deleted; anything else in a locked
  # dir is left for `check` to report, never silently removed.
  while read -r kind a b c; do
    [ "$kind" = file ] || continue
    in_plan "$plan" "$c" || echo "$c" >> "$dels"
  done < "$ENTRIES"
  while IFS= read -r c; do need_repo_path "$c" file; done < "$dels"
  while read -r mode oid b c; do need_plan_path "$c" "$dels"; done < "$plan"
  apply "$plan" "$dels"
  write_lock "$COMMIT" "$dirs" "$FILES"
  echo "flag-sync: synced $(wc -l < "$FILES" | tr -d ' ') files from flag ${COMMIT:0:12}"
}

cmd_add() {
  local src dest dirs plan dels list kind a b c d mode oid rel
  case $# in
    3) open_flag "$1"; shift ;;
    2) open_flag "" ;;
    *) die "usage: flag-sync.sh add [FLAG_DIR] <flag-path> <repo-path>" ;;
  esac
  clean_flag
  src="${1%/}"; dest="${2%/}"
  safe_path "$src"; safe_path "$dest"
  # The lock names one commit for every entry, so everything already locked
  # (files and whole-dir mirrors) must match this checkout first; otherwise the
  # rewritten commit line would be a lie.
  status_scan > /dev/null
  [ "$STALE" -eq 0 ] || die "the vendored copy differs from ${COMMIT:0:12} (see: scripts/flag-sync.sh status $FD); run scripts/flag-sync.sh sync against this checkout first"
  # ...and the repo copy must match the lock: rewriting the lock over a locally
  # edited or missing vendored file would report success and leave check failing.
  if [ -n "$(awk '$1 == "file" { print; exit }' "$ENTRIES")" ] && ! ( cmd_check > /dev/null ); then die "the vendored files do not match flag.lock (see: scripts/flag-sync.sh check); run scripts/flag-sync.sh sync first"; fi
  newtmp dirs; newtmp plan; newtmp dels; newtmp list; newtmp FILES
  # Entries at or under <repo-path> are being replaced: drop them from the
  # lock, and remember their files so the old copies can be deleted (a
  # replacement with fewer files must not leave unlocked leftovers behind).
  while read -r kind a b c; do
    case "$kind" in
      dir) [ "$a" = "$dest" ] || [ "${a#"$dest"/}" != "$a" ] || echo "$a" >> "$dirs" ;;
      file) if [ "$c" = "$dest" ] || [ "${c#"$dest"/}" != "$c" ]; then echo "$c" >> "$dels"; else echo "$a $b $c" >> "$FILES"; fi ;;
    esac
  done < "$ENTRIES"
  flag_entry "$src"
  case "$E_MODE" in
    '') die "flag has no design-system/$src" ;;
    040000)
      while read -r d; do case "$dest" in "$d"/*) die "$dest is inside locked dir $d; a locked dir cannot hold another" ;; esac; done < "$dirs"
      echo "$dest" >> "$dirs"
      flag_list "$src" "$list"
      while read -r mode oid rel; do
        safe_path "$dest/$rel"
        need_blob "$mode" "$src/$rel"
        echo "$mode $oid $src/$rel $dest/$rel" >> "$plan"
      done < "$list"
      [ -s "$plan" ] || die "design-system/$src has no files"
      # The new lock claims every file under <repo-path>: anything already there
      # must be a locked file this add replaces, or the next check would find
      # it UNLOCKED after the files were already written.
      if [ -d "$ROOT/$dest" ]; then
        while IFS= read -r -d '' c; do
          c="${c#"$ROOT"/}"
          { [ -f "$ROOT/$c" ] && [ ! -L "$ROOT/$c" ]; } || die "$c is a symlink or special file inside $dest; remove it first"
          in_plan "$plan" "$c" || grep -qxF -- "$c" "$dels" || die "$c is already in $dest and is not from flag; move product files out of $dest (or delete it) before vendoring flag's $src there"
        done < <(find "$ROOT/$dest" -mindepth 1 ! -type d -print0)
      fi ;;
    *)
      # A locked dir mirrors exactly one Flag dir; a single file dropped inside
      # it would make the mirror ambiguous for every later status/sync.
      while read -r d; do case "$dest" in "$d"/*) die "$dest is inside locked dir $d; vendor it outside the mirrored dir (or add the whole dir)" ;; esac; done < "$dirs"
      need_blob "$E_MODE" "$src"; echo "$E_MODE $E_OID $src $dest" >> "$plan" ;;
  esac
  newtmp d
  while IFS= read -r c; do in_plan "$plan" "$c" || echo "$c" >> "$d"; done < "$dels"
  while IFS= read -r c; do need_repo_path "$c" file; done < "$d"
  while read -r mode oid b c; do need_plan_path "$c" "$d"; done < "$plan"
  apply "$plan" "$d"
  write_lock "$COMMIT" "$dirs" "$FILES"
  echo "flag-sync: locked $src -> $dest"
}

case "${1:-check}" in
  check|sync|add|status) validate_lock ;;
esac
case "${1:-check}" in
  check) cmd_check ;;
  sync) shift; cmd_sync "$@" ;;
  add) shift; cmd_add "$@" ;;
  status) shift; cmd_status "$@" ;;
  version) echo "$FLAG_SYNC_VERSION" ;;
  -h|--help|help) sed -n '2,/^set -euo/{/^#/p}' "$0" ;;
  *) die "unknown command $1 (check | sync | add | status)" ;;
esac
