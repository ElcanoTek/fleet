#!/usr/bin/env bash
# scripts/release-promote-latest.sh — point ghcr.io/elcanotek/fleet:latest and
# fleet-sandbox:latest at the newest COMPLETE published release (release.yml's
# promote-latest job, the only writer of those tags).
#
#  1. The newest release that is published (non-draft) and complete (carries
#     SHA256SUMS), from the GitHub API, ordered by scripts/version.sh — never
#     "the release this run built", which an overlapping run may have
#     overtaken.
#  2. Its SHA256SUMS signature is verified with the pinned cosign against the
#     release signer, and its release.json against SHA256SUMS: the digests
#     promoted are the ones the release signed.
#  3. For each repository, fleet first and then the sandbox, :latest is
#     copied from repo@digest (--all --preserve-digests: the signed index as
#     is) — unless it already resolves to that digest. Re-running is a no-op.
#
# Env: FLEET_IMAGE, SANDBOX_IMAGE, SIGNER_IDENTITY, OIDC_ISSUER (release.yml
# sets them); GH_TOKEN for gh. Needs gh, cosign, skopeo, jq, sha256sum.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for v in FLEET_IMAGE SANDBOX_IMAGE SIGNER_IDENTITY OIDC_ISSUER; do
  [[ -n "${!v:-}" ]] || { echo "$v is not set" >&2; exit 2; }
done

listed="$(gh release list --limit 200 --exclude-drafts --json tagName --jq '.[].tagName')"
ordered="$(printf '%s\n' "$listed" | awk 'NF && !seen[$0]++' | "$here/version.sh" filter)"
newest=""
while IFS= read -r t; do
  [[ -n "$t" ]] || continue
  complete="$(gh release view "$t" --json isDraft,assets --jq '(.isDraft | not) and ([.assets[].name] | index("SHA256SUMS") != null)')"
  if [[ "$complete" == "true" ]]; then
    newest="$t"
    break
  fi
done < <(printf '%s\n' "$ordered" | tac)
if [[ -z "$newest" ]]; then
  echo "no complete published release; :latest is left as it is."
  exit 0
fi
echo "newest complete published release: ${newest}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
gh release download "$newest" --pattern SHA256SUMS --pattern SHA256SUMS.sigstore.json --pattern release.json --dir "$work"
cosign verify-blob --bundle "$work/SHA256SUMS.sigstore.json" \
  --certificate-identity "$SIGNER_IDENTITY" --certificate-oidc-issuer "$OIDC_ISSUER" "$work/SHA256SUMS" >/dev/null
want="$(awk '$2 == "release.json" { print $1 }' "$work/SHA256SUMS")"
[[ "$want" =~ ^[0-9a-f]{64}$ ]] || { echo "SHA256SUMS of ${newest} has no release.json entry" >&2; exit 1; }
got="$(sha256sum "$work/release.json" | awk '{ print $1 }')"
[[ "$got" == "$want" ]] || { echo "release.json of ${newest} does not match its signed SHA256SUMS" >&2; exit 1; }
[[ "$(jq -r '.tag' "$work/release.json")" == "$newest" ]] || { echo "release.json of ${newest} names another tag" >&2; exit 1; }

for repo in "$FLEET_IMAGE" "$SANDBOX_IMAGE"; do
  ref="$(jq -r --arg r "$repo" '.images[$r] // empty' "$work/release.json")"
  digest="${ref#"${repo}"@}"
  if [[ "$ref" != "${repo}@${digest}" || ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "release.json of ${newest} records no ${repo}@sha256:… image (got '${ref}')" >&2
    exit 1
  fi
  current=""
  # The digest of the manifest :latest names, hashed from the exact bytes
  # (piped, so no trailing newline is lost to a command substitution).
  if hex="$(skopeo inspect --raw "docker://${repo}:latest" 2>/dev/null | sha256sum | awk '{ print $1 }')"; then
    current="sha256:${hex}"
  fi
  if [[ "$current" == "$digest" ]]; then
    echo "${repo}:latest already is ${newest} (${digest})"
    continue
  fi
  echo "${repo}:latest → ${newest} (${digest})"
  skopeo copy --all --preserve-digests "docker://${repo}@${digest}" "docker://${repo}:latest"
done
