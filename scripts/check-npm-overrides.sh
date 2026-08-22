#!/usr/bin/env bash
# check-npm-overrides.sh — fail when a security override has become droppable.
#
# scripts/rampart-service/package.json carries two `overrides` that force
# patched versions of transitive dependencies whose parents have not released a
# fix yet:
#
#   sharp   ^0.35.3  — @huggingface/transformers pins sharp ^0.34.5, which
#                      carries the libvips CVEs (CVE-2026-33327/-33328/-35590/
#                      -35591, GHSA-f88m-g3jw-g9cj).
#   adm-zip ^0.6.0   — onnxruntime-node pins adm-zip ^0.5.16, which carries
#                      GHSA-xcpc-8h2w-3j85 (crafted-ZIP 4 GB allocation).
#
# An override is a fork of upstream's intent: correct while upstream is broken,
# and pure drift the day upstream fixes itself — at which point Dependabot's
# normal updates are silently pinned down by us instead. Nothing else notices
# that day. This check does: it asks the registry what floor each PARENT now
# declares, and FAILS with removal instructions once the parent's own range
# reaches the patched line. So the reminder to drop the override is a red build
# with a two-line fix, not a stale-pin archaeology session years later.
#
# Registry unreachable / output unparsable is a SKIP with a notice, never a
# failure: this check's job is "tell me when the override is droppable", and a
# network flake is not evidence of that. The vulnerability gate itself is
# `npm audit` in the same CI job, which does fail closed on its own findings.
set -uo pipefail

# floor RANGE -> x.y.z: the minimum version of a caret/tilde/plain range.
# The parents' published ranges are simple ("^0.34.5"); anything fancier
# parses to "unknown" and is treated as a skip, not a verdict.
floor() {
  local r="${1#\^}"; r="${r#~}"; r="${r#>=}"
  if [[ "$r" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
    printf '%s.%s.%s' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
  else
    printf 'unknown'
  fi
}

# ge A B — true when version A >= B (numeric, three components).
ge() {
  local IFS=. a b
  read -ra a <<<"$1"; read -ra b <<<"$2"
  for i in 0 1 2; do
    if (( ${a[$i]:-0} > ${b[$i]:-0} )); then return 0; fi
    if (( ${a[$i]:-0} < ${b[$i]:-0} )); then return 1; fi
  done
  return 0
}

# check PARENT DEP PATCHED_FLOOR — fail if PARENT's declared range for DEP now
# starts at or above PATCHED_FLOOR (the override is then droppable).
stale=0
check() {
  local parent="$1" dep="$2" patched="$3" range f
  range="$(npm view "$parent@latest" "dependencies.$dep" 2>/dev/null || true)"
  if [ -z "$range" ]; then
    echo "notice: could not read $parent's $dep range from the registry — skipping (not a verdict)."
    return 0
  fi
  f="$(floor "$range")"
  if [ "$f" = "unknown" ]; then
    echo "notice: $parent declares $dep '$range' — cannot parse a floor, skipping."
    return 0
  fi
  if ge "$f" "$patched"; then
    echo "::error::$parent@latest now declares $dep '$range' (floor $f >= $patched):"
    echo "  the '$dep' override in scripts/rampart-service/package.json is DROPPABLE."
    echo "  Remove it, regenerate package-lock.json (npm install --package-lock-only),"
    echo "  and re-run npm audit — upstream now ships the patched line itself."
    stale=1
  else
    echo "override for $dep still required: $parent@latest pins '$range' (floor $f < $patched)."
  fi
}

check "@huggingface/transformers" "sharp"   "0.35.0"
check "onnxruntime-node"          "adm-zip" "0.6.0"

exit "$stale"
