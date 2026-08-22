#!/usr/bin/env bash
# Fail for actionable CRITICAL and HIGH vulnerabilities in Fedora RPMs.
#
# High was added to the gate after measuring, not before: the published
# sandbox image at the time of the change carried zero fixable Critical or
# High RPM findings (its only fixable findings were two Medium openssh
# advisories), so the tightened gate started clean rather than arming over a
# backlog. Medium and below stay report-only — the image tracks
# fedora-minimal:latest, so routine rebuilds pick those up without a gate.
#
# Grype also catalogs Python dist-info shipped *by* Fedora RPMs as independent
# PyPI packages. Those records use upstream versions/advisories and can claim a
# fix exists even when Fedora has already backported it or has not published an
# RPM update yet. Treating those language records as a merge gate caused Fleet
# to layer hand-maintained pip replacements over a coherent distro package set.
# We still publish every finding to SARIF; this gate is narrower by design:
# block when the deployed package manager can actually move an RPM forward.
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
  | select((.artifact.type // "") == "rpm")
  | select(((.vulnerability.fix.versions // []) | length) > 0)
] | unique_by([.vulnerability.id, .artifact.name, .artifact.version])'

findings="$(jq -r "$filter"'[] | [.vulnerability.id, .artifact.name, .artifact.version, (.vulnerability.fix.versions | join(","))] | @tsv' "$report")"
if [[ -z "$findings" ]]; then
  echo "Grype policy: no fixable CRITICAL or HIGH Fedora RPM findings."
  exit 0
fi

count="$(awk 'NF { count++ } END { print count + 0 }' <<<"$findings")"
echo "Grype policy: $count fixable CRITICAL/HIGH Fedora RPM finding(s):" >&2
awk -F $'\t' '{ printf "  %s  %s %s  fix: %s\n", $1, $2, $3, $4 }' <<<"$findings" >&2
echo "Rebuild against Fedora latest or update the affected RPM; do not shadow it with an ad-hoc language-package pin." >&2
exit 1
