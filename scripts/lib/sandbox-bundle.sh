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
# with a single bare ${VAR} or ${VAR:-default} interpolated (the only shapes
# the bundles use; anything else is returned literally). LOOKUP is the name
# of a function that prints VAR's value — the caller decides the source:
# bootstrap and doctor read their shell (fleet_shell_env_value), update reads
# the service's env file. A spelling ADR-0106 removed is never looked up in
# any source: it reads as unset (so the default applies), exactly as fleet
# itself reads it (envreg.RetiredAlias; scripts/lib/retired-env.sh).
fleet_sandbox_resolve_scalar() {
  local key="$1" file="$2" lookup="$3" raw var def val=""
  raw="$(fleet_sandbox_manifest_value "$key" "$file")"
  if [[ "$raw" =~ ^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$ ]]; then
    var="${BASH_REMATCH[1]}" def="${BASH_REMATCH[3]}"
    if ! fleet_env_name_retired "$var"; then
      val="$("$lookup" "$var")"
    fi
    printf '%s' "${val:-$def}"
  else
    printf '%s' "$raw"
  fi
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
# ships no Containerfile (and names none), or its Containerfile and every file
# of the generic build context are byte-identical to the generic bundle's (the
# image is built from both, so an edited requirements.txt is a custom image).
fleet_sandbox_uses_release_image() {
  local cf generic ctx gctx f
  cf="$(fleet_sandbox_containerfile "$1")"
  if [[ ! -f "$cf" ]]; then
    fleet_sandbox_containerfile_named "$1" && return 1
    return 0
  fi
  generic="$(fleet_sandbox_containerfile "$2")"
  [[ -f "$generic" ]] && cmp -s "$cf" "$generic" || return 1
  ctx="$(dirname "$cf")"
  gctx="$(dirname "$generic")"
  for f in "$gctx"/*; do
    [[ -f "$f" ]] || continue
    cmp -s "$f" "$ctx/$(basename "$f")" || return 1
  done
  return 0
}
