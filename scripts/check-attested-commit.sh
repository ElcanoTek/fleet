#!/usr/bin/env bash
# scripts/check-attested-commit.sh VERIFY_JSON COMMIT
#
# Check the output of `gh attestation verify --format json` (which has already
# verified the signature, the signer identity and the subject) for the one
# thing it cannot check for release.yml: that every verified SLSA provenance
# statement names COMMIT — the release commit — as its source
# (the resolvedDependencies entry for the release tag, as scripts/slsa-predicate.sh writes
# it), and none names anything else. Exit 0 when they all do; 1 otherwise.
set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 VERIFY_JSON COMMIT" >&2; exit 2; }
report="$1" commit="$2"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "not a commit SHA: $commit" >&2; exit 2; }
[[ -r "$report" ]] || { echo "no verification report at $report" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

got="$(jq -r '
  [ .[]? | .verificationResult.statement
    | select(.predicateType == "https://slsa.dev/provenance/v1")
    | ([ .predicate.buildDefinition.resolvedDependencies[]?
         | select((.uri // "") | test("@refs/tags/v[0-9]")) | .digest.gitCommit ]
       | first // "none") ]
  | if length == 0 then "none" else unique | join(",") end' "$report")"
if [[ "$got" != "$commit" ]]; then
  echo "the verified provenance names source commit(s) ${got}, not the release commit ${commit}" >&2
  exit 1
fi
echo "provenance names the release commit ${commit}"
