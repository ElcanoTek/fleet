#!/usr/bin/env bash
# Fail for actionable CRITICAL and HIGH vulnerabilities in the packages an
# image's build installs and can move forward:
#   - Fedora RPMs (dnf can update them), and
#   - the PyPI packages the image's own build pip-installs under /usr/local
#     (deploy/python/requirements.txt — Playwright and its dependencies — and
#     the sandbox's hash-locked requirements): a pin bump moves them, and
#     Dependabot's pip entry proposes it.
# "Actionable" is the same rule for both: Grype names a fixed version.
#
# High was added to the gate after measuring, not before: the published
# sandbox image at the time of the change carried zero fixable Critical or
# High RPM findings (its only fixable findings were two Medium openssh
# advisories), so the tightened gate started clean rather than arming over a
# backlog. Medium and below stay report-only — the image tracks
# fedora-minimal:latest, so routine rebuilds pick those up without a gate.
#
# Grype also catalogs Python dist-info shipped *by* Fedora RPMs (under
# /usr/lib*/python3*/site-packages) as independent PyPI packages. Those records
# use upstream versions/advisories and can claim a fix exists even when Fedora
# has already backported it or has not published an RPM update yet. Treating
# them as a gate caused Fleet to layer hand-maintained pip replacements over a
# coherent distro package set, so they stay report-only: only a Python record
# found under /usr/local — where pip, never dnf, puts packages — is gated.
# Every finding is still published to SARIF.
#
# Usage: check-grype-policy.sh [GRYPE_JSON]   (default grype-results.json)
set -euo pipefail

report="${1:-grype-results.json}"
if [[ ! -r "$report" ]]; then
  echo "Grype JSON report is missing or unreadable: $report" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

filter='[
  .matches[]
  | select((.vulnerability.severity // "" | ascii_downcase) as $s | $s == "critical" or $s == "high")
  | select(((.vulnerability.fix.versions // []) | length) > 0)
  | select(
      ((.artifact.type // "") == "rpm")
      or (((.artifact.type // "") == "python")
          and any((.artifact.locations // [])[]; ((.path // "") | startswith("/usr/local/"))))
    )
] | unique_by([.vulnerability.id, .artifact.type, .artifact.name, .artifact.version])'

findings="$(jq -r "$filter"'[] | [.vulnerability.id, .artifact.type, .artifact.name, .artifact.version, (.vulnerability.fix.versions | join(","))] | @tsv' "$report")"
if [[ -z "$findings" ]]; then
  echo "Grype policy: no fixable CRITICAL or HIGH Fedora RPM or /usr/local PyPI findings."
  exit 0
fi

count="$(awk 'NF { count++ } END { print count + 0 }' <<<"$findings")"
echo "Grype policy: $count fixable CRITICAL/HIGH finding(s):" >&2
awk -F $'\t' '{ printf "  %s  %s %s %s  fix: %s\n", $1, $2, $3, $4, $5 }' <<<"$findings" >&2
echo "An rpm: rebuild against Fedora latest or update the affected RPM; do not shadow it with an ad-hoc language-package pin." >&2
echo "A python package under /usr/local: bump its hash-locked pin (pip-compile --generate-hashes) to the fixed version." >&2
exit 1
