# shellcheck shell=bash
# scripts/lib/sandbox-bundle.sh — where a client bundle's sandbox Containerfile
# lives, and whether the bundle runs the release's published sandbox image.
#
# Sourced by build-sandbox-image.sh, bootstrap.sh and update.sh, so all three
# resolve the Containerfile the SAME way: the manifest's sandbox.containerfile
# (relative to the bundle) when set, else sandbox/Containerfile. A bundle that
# names its own path must never be mistaken for one that ships no Containerfile
# and get the generic release image tagged over its own.
#
# It is also the ONE place a script resolves a manifest sandbox scalar that
# names an env variable (fleet_sandbox_resolve_scalar): bootstrap.sh, update.sh
# and doctor.sh each used to carry a copy, and the copies drifted (#2081).

fleet__sandbox_bundle_scripts="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=retired-env.sh
. "$fleet__sandbox_bundle_scripts/lib/retired-env.sh"

# fleet_sandbox_manifest_value KEY MANIFEST — a scalar under the top-level
# `sandbox:` block. Tiny purpose-built parser (no yq dependency): finds the
# `sandbox:` line, then the first `  <key>:` under it, strips quotes and an
# inline comment. Good enough for the flat scalars this block carries; the Go
# loader is the authoritative parser the process consumes.
fleet_sandbox_manifest_value() {
  local key="$1" file="$2"
  [[ -f "$file" ]] || return 0
  awk -v key="$key" '
    /^sandbox:[[:space:]]*$/ { in_block=1; next }
    /^[^[:space:]]/ { in_block=0 }
    in_block && $0 ~ "^[[:space:]]+" key ":" {
      sub("^[[:space:]]+" key ":[[:space:]]*", "")
      sub(/[[:space:]]+#.*$/, "")            # strip inline comment
      gsub(/^["'\'']|["'\'']$/, "")          # strip surrounding quotes
      print
      exit
    }
  ' "$file"
}

# fleet_sandbox_resolve_scalar KEY MANIFEST LOOKUP — the sandbox.KEY scalar
# with a whole-value ${...} reference interpolated the way the Go loader does
# (internal/clientconfig expandExpr / retiredExpr; TestShellResolverMatchesGo
# runs both over the same table):
#   ${X}       X's value; X unset or empty → an error (the loader refuses an
#              unresolved reference in this field)
#   ${X:-d}    X's value, else d
#   ${X:?m}    X's value, else an error carrying m
#   ${X:<other>…} the whole text is the name (as in Go): unset → an error
# Values are trimmed; empty counts as unset. A spelling ADR-0106 removed is
# never looked up — it is unset, exactly as fleet reads it: ${X:-d} gives d,
# and ${X} / ${X:?m} fail with the rename spelled out (envreg.RetiredAlias;
# retired-env.sh).
# Anything that is not a single whole-value ${...} is returned literally.
# LOOKUP is the name of a function printing a variable's value — the caller
# picks the source: bootstrap and doctor read their shell
# (fleet_shell_env_value), update reads the service's env file. On an error
# it prints the reason to stderr and returns 1.
fleet_sandbox_resolve_scalar() {
  local key="$1" file="$2" lookup="$3" raw expr name op="" body="" val=""
  raw="$(fleet_sandbox_manifest_value "$key" "$file")"
  if [[ ! "$raw" =~ ^\$\{([^{}]*)\}$ ]]; then
    printf '%s' "$raw"
    return 0
  fi
  expr="${BASH_REMATCH[1]}"
  name="$expr"
  if [[ "$expr" == *:* ]]; then
    local rest="${expr#*:}"
    if [[ -n "$rest" ]]; then
      name="${expr%%:*}" op="${rest:0:1}" body="${rest:1}"
    fi
  fi
  local retired=""
  if fleet_env_name_retired "$(fleet__trim "$name")"; then
    retired="$(fleet__trim "$name")"
    [[ "$op" == "-" ]] && { printf '%s' "$(fleet__trim "$body")"; return 0; }
    printf 'sandbox.%s: manifest references removed variable %s; rename it to %s (ADR-0106)\n' "$key" "$retired" "$(fleet_env_canonical_name "$retired")" >&2
    return 1
  fi
  case "$op" in
    -|\?) ;;
    *) op="" name="$expr" ;;
  esac
  name="$(fleet__trim "$name")"
  [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] && val="$(fleet__trim "$("$lookup" "$name")")"
  if [[ -n "$val" ]]; then
    printf '%s' "$val"
    return 0
  fi
  case "$op" in
    -) printf '%s' "$(fleet__trim "$body")" ;;
    \?)
      body="$(fleet__trim "$body")"
      printf 'sandbox.%s: ${%s:?...}: %s\n' "$key" "$name" "${body:-required value is unset or empty}" >&2
      return 1 ;;
    *)
      printf 'sandbox.%s: ${%s} is unset or empty (set it, or use ${%s:-default})\n' "$key" "$name" "$name" >&2
      return 1 ;;
  esac
}

# fleet_env_canonical_name NAME — the FLEET_ name a removed spelling maps to
# (CHAT_X / CUTLASS_X → FLEET_X; the bare DATA_DIR → FLEET_DATA_DIR).
fleet_env_canonical_name() {
  case "$1" in
    CHAT_*) printf 'FLEET_%s' "${1#CHAT_}" ;;
    CUTLASS_*) printf 'FLEET_%s' "${1#CUTLASS_}" ;;
    *) printf 'FLEET_%s' "$1" ;;
  esac
}

# fleet__trim TEXT — TEXT without leading/trailing whitespace.
fleet__trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  printf '%s' "${s%"${s##*[![:space:]]}"}"
}

# fleet_shell_env_value NAME — NAME's value in this shell's environment.
fleet_shell_env_value() { printf '%s' "${!1:-}"; }

# fleet_sandbox_containerfile BUNDLE_DIR — the bundle's Containerfile path.
fleet_sandbox_containerfile() {
  local rel
  rel="$(fleet_sandbox_manifest_value containerfile "$1/manifest.yaml")"
  printf '%s/%s' "$1" "${rel:-sandbox/Containerfile}"
}

# fleet_sandbox_containerfile_named BUNDLE_DIR — true when the manifest names
# sandbox.containerfile explicitly (so a missing file is an error, not "the
# bundle ships no Containerfile").
fleet_sandbox_containerfile_named() {
  [[ -n "$(fleet_sandbox_manifest_value containerfile "$1/manifest.yaml")" ]]
}

# fleet_sandbox_build_containerfile BUNDLE_DIR GENERIC_BUNDLE_DIR — the
# Containerfile an on-box build of BUNDLE_DIR's sandbox uses (its build
# context is the file's directory). The bundle's own when it ships one; for a
# bundle that ships none AND names none, the generic bundle's — such a bundle
# customizes nothing, so its sandbox IS the generic image
# (fleet_sandbox_uses_release_image agrees). A bundle that NAMES a missing
# Containerfile gets that missing path back: the build must fail on it, never
# quietly build the generic image instead. build-sandbox-image.sh and
# bootstrap.sh both resolve the build through this.
fleet_sandbox_build_containerfile() {
  local cf
  cf="$(fleet_sandbox_containerfile "$1")"
  if [[ ! -f "$cf" ]] && ! fleet_sandbox_containerfile_named "$1"; then
    cf="$(fleet_sandbox_containerfile "$2")"
  fi
  printf '%s' "$cf"
}

# fleet_sandbox_uses_release_image BUNDLE_DIR GENERIC_BUNDLE_DIR — true when
# the release's published sandbox image IS this bundle's image: the bundle
# ships no Containerfile (and names none), or its whole build context — the
# Containerfile and every other file the build would send — hashes the same as
# the generic bundle's (fleet_sandbox_context_hash). The image is built from
# all of it, so an edited requirements.txt, or a file the generic context does
# not have at all, makes a custom image.
fleet_sandbox_uses_release_image() {
  local cf generic
  cf="$(fleet_sandbox_containerfile "$1")"
  if [[ ! -f "$cf" ]]; then
    fleet_sandbox_containerfile_named "$1" && return 1
    return 0
  fi
  generic="$(fleet_sandbox_containerfile "$2")"
  [[ -f "$generic" ]] || return 1
  [[ "$(fleet_sandbox_context_hash "$cf")" == "$(fleet_sandbox_context_hash "$generic")" ]]
}

# fleet_sandbox_ignore_file CONTEXT_DIR — the ignore file `podman build`
# honours in CONTEXT_DIR (.containerignore, else .dockerignore), or nothing.
fleet_sandbox_ignore_file() {
  local f
  for f in .containerignore .dockerignore; do
    if [[ -f "$1/$f" ]]; then
      printf '%s' "$1/$f"
      return 0
    fi
  done
  return 0
}

# fleet__ignore_match PATTERN REL — true when one ignore-file PATTERN (already
# stripped of `!` and of leading and trailing `/`) matches the context-relative
# path REL or one of its parent directories (a pattern naming a directory
# excludes everything under it). As in the build: `*`, `?` and `[...]` match
# within one path segment, and `**` spans any number of segments.
fleet__ignore_match() {
  local -a ig_pat ig_rel
  IFS=/ read -r -a ig_pat <<<"$1"
  IFS=/ read -r -a ig_rel <<<"$2"
  fleet__ignore_match_at 0 0
}

# fleet__ignore_match_at I J — ig_pat[I:] matches ig_rel[J:] or a prefix of it.
# Reads fleet__ignore_match's ig_pat/ig_rel arrays (bash dynamic scope).
fleet__ignore_match_at() {
  local i="$1" j="$2" k
  # `a//b` and `./a` leave empty and `.` segments: they match nothing.
  while (( i < ${#ig_pat[@]} )) && [[ -z "${ig_pat[i]}" || "${ig_pat[i]}" == "." ]]; do i=$((i + 1)); done
  (( i >= ${#ig_pat[@]} )) && return 0
  if [[ "${ig_pat[i]}" == "**" ]]; then
    for (( k = j; k <= ${#ig_rel[@]}; k++ )); do
      fleet__ignore_match_at $((i + 1)) "$k" && return 0
    done
    return 1
  fi
  (( j < ${#ig_rel[@]} )) || return 1
  # shellcheck disable=SC2053 # the pattern segment is a glob on purpose
  [[ "${ig_rel[j]}" == ${ig_pat[i]} ]] || return 1
  fleet__ignore_match_at $((i + 1)) $((j + 1))
}

# fleet__context_ignored REL IGNORE_FILE — true when IGNORE_FILE (may be
# empty: nothing is ignored) excludes the context-relative path REL from the
# build. The last matching line wins, so a later `!pattern` re-includes what an
# earlier line excluded.
fleet__context_ignored() {
  local rel="$1" line neg ignored=1
  [[ -n "$2" ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(fleet__trim "$line")"
    [[ -z "$line" || "$line" == "#"* ]] && continue
    neg=0
    if [[ "$line" == "!"* ]]; then
      neg=1
      line="$(fleet__trim "${line#!}")"
    fi
    while [[ "$line" == /* ]]; do line="${line#/}"; done
    while [[ "$line" == */ ]]; do line="${line%/}"; done
    [[ -n "$line" ]] || continue
    if fleet__ignore_match "$line" "$rel"; then
      ignored=$neg
    fi
  done < "$2"
  return "$ignored"
}

# fleet__mode PATH — PATH's permission bits that the build preserves and that
# do not depend on the umask of whoever checked the bundle out: mode & 0755
# (owner rwx, group/other read + execute). A group/other WRITE bit is what a
# 002 vs 022 umask flips, so hashing it would make two identical checkouts —
# or one checkout rewritten by a pull under another umask — read as different
# images. GNU stat: update.sh, bootstrap.sh and build-sandbox-image.sh run
# only on Linux (BSD/macOS stat would need `stat -f %Lp`).
fleet__mode() {
  local m
  m="$(stat -c %a "$1")" || return 1
  printf '%o' $(( 8#$m & 8#755 ))
}

# fleet_sandbox_context_hash CONTAINERFILE — one SHA-256 over the build
# context `podman build -f CONTAINERFILE "$(dirname CONTAINERFILE)"` uses
# (build-sandbox-image.sh), or `absent` when CONTAINERFILE does not exist.
# Deterministic: every directory, regular file and symlink under the context,
# sorted bytewise by relative path, each hashed as its kind, its path, its
# permission bits (fleet__mode — COPY preserves them, so a chmod +x makes a
# new image) and, for a file, its contents (a symlink: its target).
# Directories count on their own, so an added empty directory or a directory
# mode change is a change. VCS metadata (.git, .hg, .svn) never counts, nor
# does a path the context's .containerignore / .dockerignore excludes (the
# build never sends it). The Containerfile and the ignore file itself always
# count — the Containerfile under a fixed name, so the same context at another
# path or under another file name hashes the same
# (fleet_sandbox_uses_release_image compares a bundle's context with the
# generic one this way).
# update.sh stores this as the sandbox build stamp, so an edit to ANY file the
# build can COPY — not only to the Containerfile — triggers a rebuild. The
# prefix names the format: a `ctx2-` hash (modes and directories) never
# equals a `ctx1-` stamp (contents only) or the bare Containerfile sha256 of
# earlier releases, so a format change rebuilds once.
fleet_sandbox_context_hash() {
  local cf="$1" ctx ignore path rel cf_rel ignore_rel=""
  if [[ ! -f "$cf" ]]; then
    printf 'absent'
    return 0
  fi
  ctx="$(cd "$(dirname "$cf")" && pwd -P)"
  cf_rel="$(basename "$cf")"
  ignore="$(fleet_sandbox_ignore_file "$ctx")"
  [[ -n "$ignore" ]] && ignore_rel="$(basename "$ignore")"
  {
    printf 'containerfile\0%s\0%s\0' "$(fleet__mode "$cf")" "$(sha256sum < "$cf" | awk '{print $1}')"
    while IFS= read -r -d '' path; do
      rel="${path#"$ctx"/}"
      [[ "$rel" == "$cf_rel" ]] && continue
      if [[ "$rel" != "$ignore_rel" ]] && fleet__context_ignored "$rel" "$ignore"; then
        continue
      fi
      if [[ -L "$path" ]]; then
        printf 'link\0%s\0%s\0' "$rel" "$(readlink "$path")"
      elif [[ -d "$path" ]]; then
        printf 'dir\0%s\0%s\0' "$rel" "$(fleet__mode "$path")"
      else
        printf 'file\0%s\0%s\0%s\0' "$rel" "$(fleet__mode "$path")" "$(sha256sum < "$path" | awk '{print $1}')"
      fi
    done < <(find "$ctx" -mindepth 1 \( -name .git -o -name .hg -o -name .svn \) -prune -o \( -type f -o -type l -o -type d \) -print0 | LC_ALL=C sort -z)
  } | sha256sum | awk '{printf "ctx2-%s", $1}'
}

# fleet_sandbox_reclaim_release_images LIST_FILE NEW_REF PROTECTED_REF… —
# remove superseded digest-pulled release sandbox images. `podman pull
# repo@sha256:…` keeps that digest reference among the image's names, so an
# old release image never goes dangling and `image prune` never reclaims it
# (~1.5 GB per release). LIST_FILE holds the refs still waiting, one per line;
# NEW_REF (may be empty) is the one this update just superseded. Each is tried
# with a plain `sandbox_podman rmi` (the caller's: the service user's store).
# A ref that is removed, already gone, or not a digest reference leaves the
# list; one whose removal is refused (a container still uses it — work that
# started after the idle gate, or a --force update) stays for the next try, so
# overwriting the digest file never strands it.
# It never removes what is in use:
#   - never a PROTECTED_REF (the current release ref, the bundle's tag, a
#     custom build's base) — such a ref leaves the list, it is current again —
#     nor an image whose ID is one of theirs (that ref stays listed: once it is
#     no longer current, a later run removes it);
#   - never --force: podman refuses an image any container, running or
#     stopped, or any child image still uses;
#   - a store that cannot be read keeps the ref (only `image exists` exit 1
#     means gone).
# Best-effort: always returns 0.
fleet_sandbox_reclaim_release_images() {
  local list="$1" new="$2" ref id rc seen=" " ids=" "
  shift 2
  local -a protect=("$@") pending=() keep=()
  if [[ -f "$list" ]]; then
    while IFS= read -r ref || [[ -n "$ref" ]]; do
      ref="$(fleet__trim "$ref")"
      [[ -n "$ref" ]] && pending+=("$ref")
    done < "$list"
  fi
  [[ -n "$new" ]] && pending+=("$new")
  (( ${#pending[@]} > 0 )) || return 0
  for ref in "${protect[@]}"; do
    [[ -n "$ref" ]] || continue
    id="$(sandbox_podman image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" || continue
    [[ -n "$id" ]] && ids+="$id "
  done
  for ref in "${pending[@]}"; do
    [[ "$seen" == *" $ref "* ]] && continue
    seen+="$ref "
    [[ "$ref" == *@sha256:* ]] || continue
    [[ " ${protect[*]} " == *" $ref "* ]] && continue
    rc=0
    sandbox_podman image exists "$ref" >/dev/null 2>&1 || rc=$?
    (( rc == 1 )) && continue
    if (( rc != 0 )); then
      keep+=("$ref")
      continue
    fi
    id="$(sandbox_podman image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" || id=""
    if [[ -z "$id" || "$ids" == *" $id "* ]]; then
      keep+=("$ref")
      continue
    fi
    sandbox_podman rmi "$ref" >/dev/null 2>&1 || keep+=("$ref")
  done
  if (( ${#keep[@]} == 0 )); then
    rm -f "$list" 2>/dev/null || true
  else
    mkdir -p "$(dirname "$list")" 2>/dev/null || true
    { printf '%s\n' "${keep[@]}" > "$list.tmp" && mv -f "$list.tmp" "$list"; } 2>/dev/null || true
  fi
  return 0
}
