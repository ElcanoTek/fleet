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

# fleet_sandbox_context_hash CONTAINERFILE — one SHA-256 over the build
# context `podman build -f CONTAINERFILE "$(dirname CONTAINERFILE)"` uses
# (build-sandbox-image.sh), or `absent` when CONTAINERFILE does not exist.
# Deterministic: every regular file and symlink under the context, sorted
# bytewise by relative path, each hashed as its path plus its contents (a
# symlink as its target). VCS metadata (.git, .hg, .svn) never counts, nor
# does a path the context's .containerignore / .dockerignore excludes (the
# build never sends it). The Containerfile and the ignore file itself always
# count — the Containerfile under a fixed name, so the same context at another
# path or under another file name hashes the same
# (fleet_sandbox_uses_release_image compares a bundle's context with the
# generic one this way).
# update.sh stores this as the sandbox build stamp, so an edit to ANY file the
# build can COPY — not only to the Containerfile — triggers a rebuild. The
# `ctx1-` prefix keeps it distinct from the bare Containerfile sha256 earlier
# releases stored there.
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
    printf 'containerfile\0%s\0' "$(sha256sum < "$cf" | awk '{print $1}')"
    while IFS= read -r -d '' path; do
      rel="${path#"$ctx"/}"
      [[ "$rel" == "$cf_rel" ]] && continue
      if [[ "$rel" != "$ignore_rel" ]] && fleet__context_ignored "$rel" "$ignore"; then
        continue
      fi
      if [[ -L "$path" ]]; then
        printf 'link\0%s\0%s\0' "$rel" "$(readlink "$path")"
      else
        printf 'file\0%s\0%s\0' "$rel" "$(sha256sum < "$path" | awk '{print $1}')"
      fi
    done < <(find "$ctx" -mindepth 1 \( -name .git -o -name .hg -o -name .svn \) -prune -o \( -type f -o -type l \) -print0 | LC_ALL=C sort -z)
  } | sha256sum | awk '{printf "ctx1-%s", $1}'
}

# fleet_sandbox_reclaim_release_image PREV_REF CURRENT_REF TAG — remove the
# superseded digest-pulled release image PREV_REF once CURRENT_REF has been
# pulled and tagged TAG. `podman pull repo@sha256:…` keeps that digest
# reference among the image's names, so the old image never goes dangling and
# `image prune` never reclaims it (~1.5 GB per release). Runs the caller's
# sandbox_podman (the service user's store). It never removes the image in use:
#   - only a digest reference (`…@sha256:…`) that is neither CURRENT_REF nor
#     TAG is considered;
#   - nothing happens when PREV_REF is not in the store, or is the same image
#     (by ID) as TAG or CURRENT_REF under another name;
#   - a plain `rmi`, never --force: podman refuses to remove an image any
#     container, running or stopped, still uses, and that refusal is ignored.
# Best-effort: always returns 0.
fleet_sandbox_reclaim_release_image() {
  local prev="$1" cur="$2" tag="$3" prev_id ref
  [[ -n "$prev" && "$prev" != "$cur" && "$prev" != "$tag" ]] || return 0
  [[ "$prev" == *@sha256:* ]] || return 0
  prev_id="$(sandbox_podman image inspect --format '{{.Id}}' "$prev" 2>/dev/null)" || return 0
  [[ -n "$prev_id" ]] || return 0
  for ref in "$tag" "$cur"; do
    [[ -n "$ref" ]] || continue
    [[ "$(sandbox_podman image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" == "$prev_id" ]] && return 0
  done
  sandbox_podman rmi "$prev" >/dev/null 2>&1 || true
  return 0
}
