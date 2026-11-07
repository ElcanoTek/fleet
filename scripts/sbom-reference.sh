#!/usr/bin/env bash
# scripts/sbom-reference.sh TAG SBOM_FILE — write the by-reference SBOM
# predicate release.yml attests each image with, and print its path.
#
# The predicate (type https://github.com/ElcanoTek/fleet/sbom-reference/v1)
# names the SBOM release asset — its file name, format, SHA-256 and download
# URL — instead of embedding it: a full SPDX SBOM of the sandbox image is
# ~28 MB, and Rekor refuses a DSSE envelope that large. The asset itself is
# covered by the release's signed SHA256SUMS, so a verifier checks the
# attestation's signature, downloads the named file and compares its hash.
set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 TAG SBOM_FILE" >&2; exit 2; }
tag="$1" sbom="$2"
[[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$ ]] || { echo "not a release tag: $tag" >&2; exit 2; }
[[ -f "$sbom" ]] || { echo "no SBOM at $sbom" >&2; exit 2; }
name="$(basename "$sbom")"
sum="$(sha256sum "$sbom" | awk '{ print $1 }')"
out="$(mktemp)"
jq -n --arg name "$name" --arg sha "$sum" --arg tag "$tag" \
  '{sbom: {name: $name, format: "spdx-json", digest: {sha256: $sha},
           uri: ("https://github.com/ElcanoTek/fleet/releases/download/" + $tag + "/" + $name)},
    covered_by: ("https://github.com/ElcanoTek/fleet/releases/download/" + $tag + "/SHA256SUMS")}' >"$out"
printf '%s\n' "$out"
