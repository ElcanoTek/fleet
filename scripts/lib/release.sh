# shellcheck shell=bash
# scripts/lib/release.sh — the shell half of fleet's signed release artifacts
# (docs/RELEASE-ARTIFACTS.md, ADR-0100). Sourced by bootstrap.sh and update.sh.
#
# Its one job is the FIRST verified binary on a box: bootstrap has no fleet
# yet, and a box upgrading from a from-source install has a fleet that
# predates `fleet release`. Everything after that runs through the installed
# binary's `fleet release` verbs (internal/release), which implement the same
# checks with tests. The two pin the same trust — the identity and issuer
# below, and the cosign digests in internal/release/cosign.lock —
# and scripts/check_release_artifacts_test.go keeps them equal.
#
# Checks, in order, before the binary is used (it is not even chmod +x until
# they pass): the cosign used is the pinned build (sha256), SHA256SUMS carries
# a signature by fleet's release workflow on main, the binary and release.json
# match the signed sums, release.json names the requested tag, and the binary
# itself carries a valid signature. There is no unverified fallback.
#
# Needs: curl, sha256sum, awk, and git (tag resolution). Not Go, not Node.

FLEET_RELEASE_REPO="ElcanoTek/fleet"
FLEET_RELEASE_IDENTITY="https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main"
FLEET_RELEASE_ISSUER="https://token.actions.githubusercontent.com"

# Download ceilings, the same as internal/release (maxMetaBytes,
# maxBinaryBytes, maxCosignBytes): a hostile or broken server cannot fill the
# disk. curl enforces --max-filesize on the advertised length and, for a body
# without one, aborts once the transfer passes it.
FLEET_RELEASE_MAX_META=$((4 << 20))
FLEET_RELEASE_MAX_BINARY=$((512 << 20))

# fleet_release_get URL OUT MAX — download URL to OUT (0600) under a size
# ceiling; a failed or oversized transfer leaves no partial file behind.
fleet_release_get() {
  local url="$1" out="$2" max="$3"
  rm -f "$out"
  if ! curl -fsSL --retry 3 --max-filesize "$max" -o "$out" "$url"; then
    rm -f "$out"
    return 1
  fi
  if [[ "$(stat -c %s "$out" 2>/dev/null || echo 0)" -gt "$max" ]]; then
    rm -f "$out"
    echo "${url##*/} is larger than ${max} bytes — refusing it" >&2
    return 1
  fi
  chmod 0600 "$out"
}

# fleet_release_arch — this box's release architecture (amd64|arm64), or fail.
fleet_release_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) echo "no fleet release is published for $(uname -m) (amd64 and arm64 are)" >&2; return 1 ;;
  esac
}

fleet_release_base_url() {
  printf '%s' "${FLEET_RELEASE_BASE_URL:-https://github.com/${FLEET_RELEASE_REPO}/releases/download}"
}

# fleet_release_latest_tag — the newest PUBLISHED release (its assets are
# uploaded before the release object is published, so this never names a
# release that is still building). Same question `fleet release latest` asks.
fleet_release_latest_tag() {
  local json tag
  json="$(curl -fsSL --retry 3 --max-filesize "$FLEET_RELEASE_MAX_META" -H 'Accept: application/vnd.github+json' \
    "${FLEET_RELEASE_LATEST_URL:-https://api.github.com/repos/${FLEET_RELEASE_REPO}/releases/latest}")" || return 1
  # Pretty-printed or compact JSON alike (no jq dependency for bootstrap).
  tag="$(printf '%s' "$json" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
  [[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] || { echo "unexpected latest release tag: ${tag:-none}" >&2; return 1; }
  printf '%s' "$tag"
}

# fleet_release_cosign REPO_ROOT CACHE_DIR — print the path of the pinned
# cosign, downloading it when the cached copy is absent or not the pinned
# bytes. The digest in cosign.lock is checked on every call.
fleet_release_cosign() {
  local root="$1" cache="$2" lock arch ver want path tmp
  lock="$root/internal/release/cosign.lock"
  [[ -r "$lock" ]] || { echo "missing $lock" >&2; return 1; }
  arch="$(fleet_release_arch)" || return 1
  ver="$(awk '$1=="version"{print $2}' "$lock")"
  want="$(awk -v k="linux/$arch" '$1==k{print $2}' "$lock")"
  [[ -n "$ver" && ${#want} -eq 64 ]] || { echo "cosign.lock has no pin for linux/$arch" >&2; return 1; }
  install -d -m 0700 "$cache" || return 1
  path="$cache/cosign-${ver}-linux-${arch}"
  if [[ -f "$path" ]] && [[ "$(sha256sum "$path" | awk '{print $1}')" == "$want" ]]; then
    printf '%s' "$path"; return 0
  fi
  tmp="${path}.download"
  fleet_release_get "${FLEET_COSIGN_URL:-https://github.com/sigstore/cosign/releases/download/${ver}}/cosign-linux-${arch}" "$tmp" "$FLEET_RELEASE_MAX_BINARY" || return 1
  if [[ "$(sha256sum "$tmp" | awk '{print $1}')" != "$want" ]]; then
    rm -f "$tmp"
    echo "downloaded cosign ${ver} does not match the digest pinned in cosign.lock — refusing to run it" >&2
    return 1
  fi
  chmod 0755 "$tmp" && mv -f "$tmp" "$path" && printf '%s' "$path"
}

# fleet_release_verify_blob COSIGN FILE — FILE's .sigstore.json bundle must be
# a signature by the release identity.
fleet_release_verify_blob() {
  local cosign="$1" file="$2" extra=()
  [[ -f "${file}.sigstore.json" ]] || { echo "no signature for ${file##*/} — refusing an unsigned release" >&2; return 1; }
  [[ -n "${FLEET_SIGSTORE_TRUSTED_ROOT:-}" ]] && extra=(--trusted-root "$FLEET_SIGSTORE_TRUSTED_ROOT")
  "$cosign" verify-blob --bundle "${file}.sigstore.json" \
    --certificate-identity "$FLEET_RELEASE_IDENTITY" \
    --certificate-oidc-issuer "$FLEET_RELEASE_ISSUER" "${extra[@]}" "$file" >/dev/null 2>&1 \
    || { echo "signature verification FAILED for ${file##*/} (signer must be ${FLEET_RELEASE_IDENTITY})" >&2; return 1; }
}

# fleet_release_fetch REPO_ROOT TAG DIR CACHE_DIR — download and verify TAG's
# binary for this box into DIR; print the verified binary's path.
fleet_release_fetch() {
  local root="$1" tag="$2" dir="$3" cache="$4" arch cosign base name want got _max
  [[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] || { echo "not a release tag: $tag" >&2; return 1; }
  arch="$(fleet_release_arch)" || return 1
  cosign="$(fleet_release_cosign "$root" "$cache")" || return 1
  base="$(fleet_release_base_url)/${tag}"
  install -d -m 0700 "$dir" || return 1
  for name in SHA256SUMS SHA256SUMS.sigstore.json "fleet-linux-${arch}" "fleet-linux-${arch}.sigstore.json" release.json; do
    _max="$FLEET_RELEASE_MAX_META"
    [[ "$name" == "fleet-linux-${arch}" ]] && _max="$FLEET_RELEASE_MAX_BINARY"
    if ! fleet_release_get "$base/$name" "$dir/$name" "$_max"; then
      echo "could not download ${name} for ${tag}" >&2
      return 1
    fi
  done
  fleet_release_verify_blob "$cosign" "$dir/SHA256SUMS" || return 1
  for name in "fleet-linux-${arch}" release.json; do
    want="$(awk -v n="$name" '$2==n || $2=="*"n {print $1}' "$dir/SHA256SUMS")"
    got="$(sha256sum "$dir/$name" | awk '{print $1}')"
    if [[ ${#want} -ne 64 || "$want" != "$got" ]]; then
      echo "${name} does not match the signed SHA256SUMS — refusing it" >&2
      return 1
    fi
  done
  grep -Eq "\"tag\"[[:space:]]*:[[:space:]]*\"${tag//./\\.}\"" "$dir/release.json" \
    || { echo "release.json does not name ${tag} — refusing a release served under another tag" >&2; return 1; }
  fleet_release_verify_blob "$cosign" "$dir/fleet-linux-${arch}" || return 1
  chmod 0755 "$dir/fleet-linux-${arch}"
  printf '%s' "$dir/fleet-linux-${arch}"
}

# fleet_release_capable BIN — the binary has the `fleet release` verbs.
fleet_release_capable() {
  [[ -x "$1" ]] && "$1" release --help >/dev/null 2>&1
}

# fleet_release_health_url ADDR — the readiness URL for FLEET_SERVER_ADDR
# (mirrors release.HealthURLFromAddr; used only for messages and dry runs).
fleet_release_health_url() {
  local addr="${1:-127.0.0.1:8080}" mode="${2:-}" scheme=http
  case "${mode,,}" in manual|auto) scheme=https ;; esac
  case "$addr" in
    :*) addr="127.0.0.1${addr}" ;;
    0.0.0.0:*) addr="127.0.0.1:${addr##*:}" ;;
    \[::\]:*) addr="127.0.0.1:${addr##*:}" ;;
  esac
  printf '%s://%s/readyz' "$scheme" "$addr"
}
