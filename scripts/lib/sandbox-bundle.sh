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
# ships no Containerfile (and names none), or its whole build-context
# directory — the Containerfile and every other entry beside it, VCS metadata
# and ignore files included — hashes the same as the generic bundle's
# (fleet_sandbox_context_hash). The image is built from that directory, so an
# edited requirements.txt, or anything the generic context does not have at
# all, makes a custom image (when in doubt, build: never a stale image). A
# context that cannot be hashed completely is "not the release image" too.
fleet_sandbox_uses_release_image() {
  local cf generic h g
  cf="$(fleet_sandbox_containerfile "$1")"
  if [[ ! -f "$cf" ]]; then
    fleet_sandbox_containerfile_named "$1" && return 1
    return 0
  fi
  generic="$(fleet_sandbox_containerfile "$2")"
  [[ -f "$generic" ]] || return 1
  h="$(fleet_sandbox_context_hash "$cf")" || return 1
  g="$(fleet_sandbox_context_hash "$generic")" || return 1
  [[ "$h" == "$g" ]]
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
  m="$(stat -c %a -- "$1")" || return 1
  [[ "$m" =~ ^[0-7]+$ ]] || return 1
  printf '%o' $(( 8#$m & 8#755 ))
}

# fleet_sandbox_context_hash CONTAINERFILE — one SHA-256 over the build
# context `podman build -f CONTAINERFILE "$(dirname CONTAINERFILE)"` uses
# (build-sandbox-image.sh), or `absent` when CONTAINERFILE does not exist.
# Deterministic: EVERY entry under the context directory, sorted bytewise by
# relative path, each hashed as its kind, its path, its permission bits
# (fleet__mode — COPY preserves them, so a chmod +x makes a new image) and,
# for a file, its contents (a symlink: its target). Directories count on their
# own, so an added empty directory or a directory mode change is a change.
# The Containerfile is an entry like any other, at its real relative path
# (a `COPY .` puts its name in the image), and the records also name which
# entry the build uses as its Containerfile. Only relative paths count, so the
# same context at another absolute path hashes the same, while a Containerfile
# renamed or moved within its context does not
# (fleet_sandbox_uses_release_image compares a bundle's context with the
# generic one this way).
# Nothing is excluded — not VCS metadata (.git), not what a .containerignore /
# .dockerignore keeps out of the build, and not those files themselves. The
# hash over-approximates the context podman sends rather than re-implementing
# its exclusion rules: erring that way costs at most an extra rebuild, while
# the other way — a missed change — leaves a stale image. The trade-off: a
# context holding often-changing files the build ignores (a Containerfile at
# a bundle root beside the bundle's .git, say) rebuilds whenever they change.
# The default <bundle>/sandbox/ layout holds only the build's own files.
# update.sh stores this as the sandbox build stamp, so an edit to ANY file the
# build can COPY — not only to the Containerfile — triggers a rebuild. The
# prefix names the format: a `ctx4-` hash never equals a stamp an earlier
# format wrote (`ctx3-`, `ctx2-`, `ctx1-`, or the bare Containerfile sha256),
# so a format change rebuilds once.
# It fails closed: when any entry cannot be listed, stat'ed, read or hashed
# (an unreadable file, a root-squashed NFS bundle, a full /tmp) it prints the
# path and reason to stderr and returns 1 — never a hash of a partial tree.
# Every step's exit status is checked (no pipeline or process substitution
# that would swallow one); callers treat a failure as "unknown".
fleet_sandbox_context_hash() {
  local cf="$1" ctx cf_rel tmp sum="" rc=0
  if [[ ! -f "$cf" ]]; then
    printf 'absent'
    return 0
  fi
  if ! ctx="$(cd "$(dirname "$cf")" && pwd -P)"; then
    fleet__hash_error "$(dirname "$cf")" "cannot enter the directory"
    return 1
  fi
  cf_rel="$(basename "$cf")"
  if ! tmp="$(mktemp -d)"; then
    fleet__hash_error "$ctx" "cannot create a temporary directory"
    return 1
  fi
  fleet__context_records "$cf" "$ctx" "$cf_rel" "$tmp" || rc=1
  if (( rc == 0 )) && ! sum="$(fleet__sha "$tmp/records")"; then
    fleet__hash_error "$tmp/records" "cannot hash the record list"
    rc=1
  fi
  rm -rf -- "$tmp"
  (( rc == 0 )) || return 1
  printf 'ctx4-%s' "$sum"
}

# fleet__hash_error PATH WHY — fleet_sandbox_context_hash's failure message.
fleet__hash_error() {
  printf 'sandbox build context: cannot hash %s: %s\n' "$1" "$2" >&2
}

# fleet__sha FILE — FILE's SHA-256 (hex), or status 1 when it cannot be read.
fleet__sha() {
  local s
  s="$(sha256sum < "$1")" || return 1
  s="${s%% *}"
  [[ "$s" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$s"
}

# fleet__context_records CONTAINERFILE CONTEXT CF_REL TMP — write the
# NUL-separated records fleet_sandbox_context_hash hashes to TMP/records.
# Status 1 (with the path on stderr) on the first entry it cannot account for.
fleet__context_records() {
  local cf="$1" ctx="$2" cf_rel="$3" tmp="$4" path rel mode sum target
  # Which entry is the Containerfile (by relative path); its mode and contents
  # are hashed below with every other entry.
  if ! printf 'containerfile\0%s\0' "$cf_rel" > "$tmp/records"; then
    fleet__hash_error "$tmp/records" "cannot write"
    return 1
  fi
  if ! find "$ctx" -mindepth 1 -print0 > "$tmp/found"; then
    fleet__hash_error "$ctx" "find could not list every entry"
    return 1
  fi
  if ! LC_ALL=C sort -z "$tmp/found" > "$tmp/sorted"; then
    fleet__hash_error "$ctx" "cannot sort the entry list"
    return 1
  fi
  while IFS= read -r -d '' path; do
    rel="${path#"$ctx"/}"
    if [[ -L "$path" ]]; then
      if ! target="$(readlink -- "$path")"; then fleet__hash_error "$path" "cannot read the link"; return 1; fi
      printf 'link\0%s\0%s\0' "$rel" "$target"
      continue
    fi
    if ! mode="$(fleet__mode "$path")"; then fleet__hash_error "$path" "cannot stat"; return 1; fi
    if [[ -d "$path" ]]; then
      printf 'dir\0%s\0%s\0' "$rel" "$mode"
    elif [[ -f "$path" ]]; then
      if ! sum="$(fleet__sha "$path")"; then fleet__hash_error "$path" "cannot read"; return 1; fi
      printf 'file\0%s\0%s\0%s\0' "$rel" "$mode" "$sum"
    else
      printf 'other\0%s\0%s\0' "$rel" "$mode"
    fi
  done < "$tmp/sorted" >> "$tmp/records" || return 1
  return 0
}

# fleet_sandbox_reclaim_release_images LIST_FILE NEW_REFS PROTECTED_REF… —
# remove superseded sandbox images. `podman pull repo@sha256:…` keeps that
# digest reference among the image's names, so an old release image never
# goes dangling and `image prune` never reclaims it (~1.5 GB per release); an
# image left under a sandbox.tag the bundle renamed away from is never
# dangling either. LIST_FILE holds the entries still waiting, one per line;
# NEW_REFS (space-separated, may be empty) are the ones this update just
# superseded. An entry is a digest reference (`…@sha256:…`), or `tag:NAME`
# for a superseded tag — the only way a non-digest name gets here, so a stray
# line naming some other image is never acted on. Each is tried with a plain
# `sandbox_podman rmi` (the caller's: the service user's store; on an image
# with other names left, that just drops this name). An entry that is
# removed, already gone, or malformed leaves the list; one whose removal is
# refused (a container still uses it — work that
# started after the idle gate, or a --force update) stays for the next try, so
# overwriting the digest file never strands it.
# It never removes what is in use:
#   - a ref equal to a PROTECTED_REF (the current release ref, the bundle's
#     tag, a custom build's base, the manifest's prebuilt sandbox.image), or
#     naming the same image ID as one, is skipped — no rmi — but stays
#     listed, so a later run reclaims it once it is no longer current;
#   - never --force: podman refuses an image any container, running or
#     stopped, or any child image still uses;
#   - a store that cannot be read keeps the ref (only `image exists` exit 1
#     means gone).
# Best-effort: always returns 0.
fleet_sandbox_reclaim_release_images() {
  local list="$1" new="$2" entry ref id rc seen=" " ids=" "
  shift 2
  local -a protect=("$@") pending=() keep=()
  if [[ -f "$list" ]]; then
    while IFS= read -r ref || [[ -n "$ref" ]]; do
      ref="$(fleet__trim "$ref")"
      [[ -n "$ref" ]] && pending+=("$ref")
    done < "$list"
  fi
  local -a new_refs=()
  read -r -a new_refs <<<"$new" || true
  pending+=("${new_refs[@]}")
  (( ${#pending[@]} > 0 )) || return 0
  for ref in "${protect[@]}"; do
    [[ -n "$ref" ]] || continue
    id="$(sandbox_podman image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" || continue
    [[ -n "$id" ]] && ids+="$id "
  done
  for entry in "${pending[@]}"; do
    [[ "$seen" == *" $entry "* ]] && continue
    seen+="$entry "
    if [[ "$entry" == tag:* ]]; then
      ref="${entry#tag:}"
      [[ -n "$ref" && "$ref" != *@* ]] || continue
    else
      ref="$entry"
      [[ "$ref" == *@sha256:* ]] || continue
    fi
    if [[ " ${protect[*]} " == *" $ref "* ]]; then
      keep+=("$entry")    # current now: no rmi, reclaimed once it no longer is
      continue
    fi
    rc=0
    sandbox_podman image exists "$ref" >/dev/null 2>&1 || rc=$?
    (( rc == 1 )) && continue
    if (( rc != 0 )); then
      keep+=("$entry")
      continue
    fi
    id="$(sandbox_podman image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" || id=""
    if [[ -z "$id" ]]; then
      keep+=("$entry")    # present but unreadable: retry later, remove nothing
      continue
    fi
    if [[ "$ids" == *" $id "* ]]; then
      keep+=("$entry")    # the current image under this name: same as above
      continue
    fi
    sandbox_podman rmi "$ref" >/dev/null 2>&1 || keep+=("$entry")
  done
  if (( ${#keep[@]} == 0 )); then
    rm -f "$list" 2>/dev/null || true
  else
    mkdir -p "$(dirname "$list")" 2>/dev/null || true
    { printf '%s\n' "${keep[@]}" > "$list.tmp" && mv -f "$list.tmp" "$list"; } 2>/dev/null || true
  fi
  return 0
}
