#!/usr/bin/env bash
# scripts/release-ghcr-gate.sh REPO TAG AMD64_ARCHIVE ARM64_ARCHIVE — the
# publish job's immutability gate for one image repository, before it pushes.
#
# Releases are immutable: different bytes must never go under a tag a box may
# already have pulled. But a publish job that failed AFTER pushing (a Rekor
# outage while signing, say) must be resumable by "Re-run failed jobs", which
# re-runs it on the SAME build artifacts. So, per tag:
#
#   none of TAG, TAG-amd64, TAG-arm64 exists        → state=push: a first
#                                                     publish;
#   every one that exists holds exactly this run's   → state=resume: what
#   images — TAG-<arch> has the config digest of       exists is reused AS IS
#   dist/…-<arch>.oci.tar, and TAG (if present) is     (its digest printed),
#   an index of exactly those two manifests            only what is missing is
#                                                      pushed — nothing is
#                                                      ever pushed over a tag;
#   anything else (another build's bytes)            → exits 1: refuse.
#
# Output (stdout): state=push|resume, then amd64=, arm64=, index= with the
# digest already under that tag, or empty when it is to be pushed.
#
# The comparison is by CONFIG digest (the image's identity: its layers'
# uncompressed content, its configuration, its creation time), which a
# registry copy never changes even when it converts the manifest format; two
# builds of the same commit differ in it.
# Needs skopeo, jq.
set -euo pipefail

[[ $# -eq 4 ]] || { echo "usage: $0 REPO TAG AMD64_ARCHIVE ARM64_ARCHIVE" >&2; exit 2; }
repo="$1" tag="$2"
declare -A archive=([amd64]="$3" [arm64]="$4")

config_of() { skopeo inspect --raw "$1" | jq -r '.config.digest // empty'; }
# The manifest digest of a registry ref, or "" when it does not exist.
digest_of() {
  local raw
  raw="$(mktemp)"
  if skopeo inspect --raw "$1" >"$raw" 2>/dev/null; then
    printf 'sha256:%s' "$(sha256sum <"$raw" | awk '{ print $1 }')"
  fi
  rm -f "$raw"
}

existing=0
declare -A arch_digest=()
for arch in amd64 arm64; do
  ref="docker://${repo}:${tag}-${arch}"
  d="$(digest_of "$ref")"
  [[ -n "$d" ]] || continue
  existing=1
  want="$(config_of "oci-archive:${archive[$arch]}")"
  got="$(config_of "$ref")"
  if [[ -z "$want" || "$got" != "$want" ]]; then
    echo "${repo}:${tag}-${arch} already holds another build (config ${got:-?}, this run's is ${want:-?}) — releases are immutable; refusing" >&2
    exit 1
  fi
  arch_digest[$arch]="$d"
done

index="$(digest_of "docker://${repo}:${tag}")"
if [[ -n "$index" ]]; then
  existing=1
  listed="$(skopeo inspect --raw "docker://${repo}:${tag}" | jq -r '[.manifests[]?.digest] | sort | join(" ")')"
  expect="$(printf '%s\n' "${arch_digest[amd64]:-}" "${arch_digest[arm64]:-}" | sort | awk 'NF' | paste -sd' ' -)"
  if [[ ${#arch_digest[@]} -ne 2 || "$listed" != "$expect" ]]; then
    echo "${repo}:${tag} is an index of [${listed}], not of this run's images [${expect}] — releases are immutable; refusing" >&2
    exit 1
  fi
fi

state=resume
[[ "$existing" == 0 ]] && state=push
# What the caller reuses as is (empty = push it).
printf 'state=%s\namd64=%s\narm64=%s\nindex=%s\n' "$state" "${arch_digest[amd64]:-}" "${arch_digest[arm64]:-}" "$index"
