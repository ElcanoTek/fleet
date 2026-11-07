#!/usr/bin/env bash
# scripts/update-pins.sh — move the pins Dependabot cannot see to upstream's
# newest release, verified before anything is rewritten.
#
# The table is scripts/lib/pins.tsv. For each pin:
#   * find upstream's newest stable release that is at least
#     PINS_COOLDOWN_DAYS old (default 7 — Dependabot's cooldown here);
#   * github: download the release's checksums file, check its keyless
#     signature when upstream signs it (with the cosign pinned in
#     internal/release/cosign.lock), download the asset and require its SHA-256
#     to match, then rewrite <NAME>_VERSION / <NAME>_SHA256 in every workflow;
#   * pypi: rewrite <NAME>_VERSION (these are `pip install x==v` pins);
#   * cosign: verify cosign_checksums.txt's keyless signature with the
#     CURRENTLY pinned cosign (the trust chain carries forward, never resets),
#     download both linux binaries, require their SHA-256 to match, rewrite
#     internal/release/cosign.lock.
# Nothing is rewritten for a pin whose verification fails; the script then
# exits non-zero after trying the rest.
#
# Usage: scripts/update-pins.sh            rewrite the pins in the working tree
#        scripts/update-pins.sh --check    report what would move; change nothing
# Env:   GH_TOKEN (optional) raises the GitHub API rate limit.
#        PINS_COOLDOWN_DAYS (default 7).
#        PINS_GITHUB_API / PINS_GITHUB_DOWNLOAD / PINS_PYPI — upstream base URLs
#        (defaults: api.github.com, github.com, pypi.org); the tests point them
#        at fixtures. What is trusted is the checksum match and the signature,
#        not the URL.
# Needs: bash, curl, jq, sha256sum, tar-free (assets are hashed, not unpacked).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/release.sh
. "$ROOT/scripts/lib/release.sh"

CHECK=0
[[ "${1:-}" == "--check" ]] && CHECK=1
COOLDOWN_DAYS="${PINS_COOLDOWN_DAYS:-7}"
[[ "$COOLDOWN_DAYS" =~ ^[0-9]+$ ]] || { echo "PINS_COOLDOWN_DAYS must be a day count" >&2; exit 2; }
CUTOFF="$(( $(date -u +%s) - COOLDOWN_DAYS * 86400 ))"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0
CHANGES=()
GH_API="${PINS_GITHUB_API:-https://api.github.com}"
GH_DL="${PINS_GITHUB_DOWNLOAD:-https://github.com}"
PYPI="${PINS_PYPI:-https://pypi.org}"
MAX_API=$((64 << 20)) # PyPI's JSON for a busy project runs to megabytes

note() { printf '%s\n' "$*"; [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] && printf -- '- %s\n' "$*" >> "$GITHUB_STEP_SUMMARY"; return 0; }
fail() { note "FAILED: $*"; FAILED=1; }

api() {
  local auth=()
  [[ -n "${GH_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${GH_TOKEN}")
  curl -fsSL --retry 3 --max-filesize "$MAX_API" -H 'Accept: application/vnd.github+json' "${auth[@]}" "$1"
}

# newest_github REPO — the newest non-draft, non-prerelease tag published
# before the cutoff.
newest_github() {
  api "$GH_API/repos/$1/releases?per_page=30" \
    | jq -r --argjson cutoff "$CUTOFF" '
        [.[] | select(.draft | not) | select(.prerelease | not)
             | select((.published_at | fromdateiso8601) <= $cutoff)][0].tag_name // empty'
}

# newest_pypi PROJECT — the newest final release first uploaded before the cutoff.
newest_pypi() {
  api "$PYPI/pypi/$1/json" \
    | jq -r --argjson cutoff "$CUTOFF" '
        .releases | to_entries
        | map(select(.key | test("^[0-9]+(\\.[0-9]+)*$")))
        | map(select((.value | length) > 0))
        | map(select(([.value[].upload_time_iso_8601 | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601] | min) <= $cutoff))
        | map(.key) | .[]' \
    | sort -V | tail -n 1
}

mapfile -t WORKFLOWS < <(find "$ROOT/.github/workflows" -name '*.yml' -print | sort)

current_value() { # NAME SUFFIX → the pinned value (first occurrence)
  grep -h -o -E "^[[:space:]]+$1_$2:[[:space:]]*'[^']+'" "${WORKFLOWS[@]}" | head -n1 | sed -E "s/.*'([^']+)'/\\1/"
}

rewrite() { # NAME SUFFIX OLD NEW
  local f
  for f in "${WORKFLOWS[@]}"; do
    sed -i -E "s|^([[:space:]]+$1_$2:[[:space:]]*)'$3'|\\1'$4'|" "$f"
  done
}

COSIGN_BIN=""
cosign_bin() {
  [[ -n "$COSIGN_BIN" ]] || COSIGN_BIN="$(fleet_release_cosign "$ROOT" "${FLEET_RELEASE_CACHE_DIR:-$WORK/cosign-cache}")"
  printf '%s' "$COSIGN_BIN"
}

# verify_signed FILE BASEURL IDENTITY ISSUER — FILE's keyless signature, as
# upstream publishes it: a .sigstore.json bundle, else .sig + .pem.
verify_signed() {
  local file="$1" base="$2" id="$3" iss="$4" name cos
  name="$(basename "$file")"
  cos="$(cosign_bin)" || return 1
  if fleet_release_get "$base/$name.sigstore.json" "$file.sigstore.json" "$FLEET_RELEASE_MAX_META" 2>/dev/null; then
    "$cos" verify-blob --bundle "$file.sigstore.json" --certificate-identity "$id" --certificate-oidc-issuer "$iss" "$file" >/dev/null 2>&1
    return
  fi
  fleet_release_get "$base/$name.sig" "$file.sig" "$FLEET_RELEASE_MAX_META" || return 1
  fleet_release_get "$base/$name.pem" "$file.pem" "$FLEET_RELEASE_MAX_META" || return 1
  "$cos" verify-blob --signature "$file.sig" --certificate "$file.pem" \
    --certificate-identity "$id" --certificate-oidc-issuer "$iss" "$file" >/dev/null 2>&1
}

# checked_sha CHECKSUMS ASSET BASEURL — the asset's SHA-256 from the checksums
# file, confirmed by downloading the asset and hashing it.
checked_sha() {
  local sums="$1" asset="$2" base="$3" want got
  want="$(awk -v a="$asset" '$2==a || $2=="*"a {print $1}' "$sums" | head -n1)"
  [[ ${#want} -eq 64 ]] || { echo "no checksum for $asset" >&2; return 1; }
  fleet_release_get "$base/$asset" "$WORK/asset" "$FLEET_RELEASE_MAX_BINARY" || return 1
  got="$(sha256sum "$WORK/asset" | awk '{print $1}')"
  rm -f "$WORK/asset"
  [[ "$got" == "$want" ]] || { echo "$asset hashes to $got, its checksums file says $want" >&2; return 1; }
  printf '%s' "$want"
}

update_github() { # NAME REPO ASSET SUMS ID ISS
  local name="$1" repo="$2" asset_t="$3" sums_t="$4" id="$5" iss="$6" cur tag v base asset sums sha cursha
  cur="$(current_value "$name" VERSION)"
  [[ -n "$cur" ]] || { fail "$name: no ${name}_VERSION pin found in the workflows"; return; }
  tag="$(newest_github "$repo")" || { fail "$name: could not list $repo releases"; return; }
  [[ -n "$tag" ]] || { note "$name: no release of $repo older than ${COOLDOWN_DAYS} days"; return; }
  v="${tag#v}"
  [[ "$v" == "$cur" ]] && { note "$name: $cur is current"; return; }
  if [[ "$(printf '%s\n%s\n' "$cur" "$v" | sort -V | tail -n1)" != "$v" ]]; then
    note "$name: pinned $cur is newer than upstream's $v (cooldown) — leaving it"; return
  fi
  if [[ "$CHECK" == "1" ]]; then note "$name: $cur → $v (would update)"; return; fi
  base="$GH_DL/$repo/releases/download/$tag"
  asset="${asset_t//\{v\}/$v}"; sums="${sums_t//\{v\}/$v}"
  fleet_release_get "$base/$sums" "$WORK/$sums" "$FLEET_RELEASE_MAX_META" || { fail "$name: no $sums in $tag"; return; }
  if [[ "$id" != "-" ]]; then
    verify_signed "$WORK/$sums" "$base" "$id" "$iss" || { fail "$name: the signature on $sums does not verify as $id — not updating"; return; }
  fi
  sha="$(checked_sha "$WORK/$sums" "$asset" "$base")" || { fail "$name: $asset did not verify against $sums — not updating"; return; }
  cursha="$(current_value "$name" SHA256)"
  rewrite "$name" VERSION "$cur" "$v"
  [[ -n "$cursha" ]] && rewrite "$name" SHA256 "$cursha" "$sha"
  CHANGES+=("$name $cur → $v")
  note "$name: $cur → $v (sha256 ${sha:0:12}…, checksums$([[ "$id" != "-" ]] && echo ' signature') verified)"
}

update_pypi() { # NAME PROJECT
  local name="$1" proj="$2" cur v
  cur="$(current_value "$name" VERSION)"
  [[ -n "$cur" ]] || { fail "$name: no ${name}_VERSION pin found in the workflows"; return; }
  v="$(newest_pypi "$proj")" || { fail "$name: could not read PyPI for $proj"; return; }
  [[ -n "$v" && "$v" != "$cur" ]] || { note "$name: $cur is current"; return; }
  if [[ "$(printf '%s\n%s\n' "$cur" "$v" | sort -V | tail -n1)" != "$v" ]]; then
    note "$name: pinned $cur is newer than $v (cooldown) — leaving it"; return
  fi
  if [[ "$CHECK" == "1" ]]; then note "$name: $cur → $v (would update)"; return; fi
  rewrite "$name" VERSION "$cur" "$v"
  CHANGES+=("$name $cur → $v")
  note "$name: $cur → $v"
}

update_cosign() { # REPO ASSET SUMS ID ISS
  local repo="$1" asset_t="$2" sums_t="$3" id="$4" iss="$5" lock cur tag base arch sha out
  lock="$ROOT/internal/release/cosign.lock"
  cur="$(awk '$1=="version"{print $2}' "$lock")"
  tag="$(newest_github "$repo")" || { fail "COSIGN: could not list $repo releases"; return; }
  [[ -n "$tag" && "$tag" != "$cur" ]] || { note "COSIGN: $cur is current"; return; }
  if [[ "$(printf '%s\n%s\n' "${cur#v}" "${tag#v}" | sort -V | tail -n1)" != "${tag#v}" ]]; then
    note "COSIGN: pinned $cur is newer than $tag (cooldown) — leaving it"; return
  fi
  if [[ "$CHECK" == "1" ]]; then note "COSIGN: $cur → $tag (would update)"; return; fi
  base="$GH_DL/$repo/releases/download/$tag"
  fleet_release_get "$base/$sums_t" "$WORK/$sums_t" "$FLEET_RELEASE_MAX_META" || { fail "COSIGN: no $sums_t in $tag"; return; }
  # Verified by the cosign we already trust — the pin moves forward only
  # along a chain of signatures.
  verify_signed "$WORK/$sums_t" "$base" "$id" "$iss" || { fail "COSIGN: the signature on $sums_t does not verify as $id — not updating"; return; }
  out="$WORK/cosign.lock"
  awk '/^#/' "$lock" > "$out"
  printf 'version %s\n' "$tag" >> "$out"
  for arch in amd64 arm64; do
    sha="$(checked_sha "$WORK/$sums_t" "${asset_t//\{arch\}/$arch}" "$base")" || { fail "COSIGN: cosign-linux-$arch did not verify — not updating"; return; }
    printf 'linux/%s %s\n' "$arch" "$sha" >> "$out"
  done
  cp "$out" "$lock"
  CHANGES+=("COSIGN $cur → $tag")
  note "COSIGN: $cur → $tag (checksums signature verified by cosign $cur; both binaries hash-checked)"
}

while IFS=$'\t' read -r name kind source asset sums id iss; do
  [[ -z "$name" || "$name" == \#* ]] && continue
  case "$kind" in
    github) update_github "$name" "$source" "$asset" "$sums" "$id" "$iss" ;;
    pypi)   update_pypi "$name" "$source" ;;
    cosign) update_cosign "$source" "$asset" "$sums" "$id" "$iss" ;;
    *)      fail "$name: unknown kind $kind" ;;
  esac
done < "$ROOT/scripts/lib/pins.tsv"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "changes<<PINS_EOF"
    printf '%s\n' "${CHANGES[@]+"${CHANGES[@]}"}"
    echo "PINS_EOF"
  } >> "$GITHUB_OUTPUT"
fi
exit "$FAILED"
