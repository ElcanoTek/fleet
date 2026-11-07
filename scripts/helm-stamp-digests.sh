#!/usr/bin/env bash
# scripts/helm-stamp-digests.sh CHART_TGZ FLEET_DIGEST SANDBOX_DIGEST
#
# Stamp a packaged fleet chart with the signed image digests of the release it
# ships with (ADR-0100, docs/RELEASE-ARTIFACTS.md): values.yaml's
# image.signedDigest and sandbox.signedImageDigest are set, so the chart
# deploys both official images BY DIGEST — the exact bytes the release signed —
# instead of by a tag that could be moved (an overridden repository never
# inherits them: see fleet.pinnedRef). The tgz is repackaged in place.
#
# release.yml's publish job runs it on the chart the build job packaged, with
# the digests it just pushed and signed; CI's helm job runs it with fixed
# digests and asserts the render. The digests are data: validated as
# sha256:<64 hex> and written by an exact-match replacement of the two empty
# placeholders, never spliced into a program. Needs: helm, tar, awk.
set -euo pipefail

[[ $# -eq 3 ]] || { echo "usage: $0 CHART_TGZ FLEET_DIGEST SANDBOX_DIGEST" >&2; exit 2; }
tgz="$1" fleet_digest="$2" sandbox_digest="$3"
for d in "$fleet_digest" "$sandbox_digest"; do
  [[ "$d" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "not an image digest: $d" >&2; exit 2; }
done
[[ -f "$tgz" ]] || { echo "no chart at $tgz" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tar -xzmf "$tgz" -C "$work"
values="$work/fleet/values.yaml"
[[ -f "$values" ]] || { echo "$tgz holds no fleet/values.yaml" >&2; exit 1; }

# Each placeholder must occur exactly once, at its key's indentation, or the
# chart is not the shape this script stamps — refuse rather than guess.
FLEET_DIGEST="$fleet_digest" SANDBOX_DIGEST="$sandbox_digest" awk '
  $0 == "  signedDigest: \"\""      { print "  signedDigest: \"" ENVIRON["FLEET_DIGEST"] "\""; n1++; next }
  $0 == "  signedImageDigest: \"\"" { print "  signedImageDigest: \"" ENVIRON["SANDBOX_DIGEST"] "\""; n2++; next }
  { print }
  END { if (n1 != 1 || n2 != 1) { print "placeholders found: signedDigest=" n1+0 " signedImageDigest=" n2+0 > "/dev/stderr"; exit 1 } }
' "$values" > "$work/values.yaml"
mv "$work/values.yaml" "$values"

out="$work/out"
mkdir -p "$out"
helm package "$work/fleet" --destination "$out" >/dev/null
pkg="$(find "$out" -maxdepth 1 -name '*.tgz' -print -quit)"
[[ -n "$pkg" ]] || { echo "helm package produced nothing" >&2; exit 1; }
mv -f "$pkg" "$tgz"
echo "stamped $(basename "$tgz"): image.signedDigest=${fleet_digest} sandbox.signedImageDigest=${sandbox_digest}"
