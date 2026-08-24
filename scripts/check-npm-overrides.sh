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
# An override is a fork of upstream's intent: correct while the dependency tree
# is broken, and pure drift once every locked parent accepts the patched line.
# This check reads the exact parent versions in package-lock.json and FAILS with
# removal instructions only when all locked instances have reached that line.
# Looking at PARENT@latest is unsafe: a transitive consumer may still pin an
# older parent (as transformers 4.2.0 does with onnxruntime-node 1.24.3), so the
# latest release can be fixed while removing the override would reintroduce the
# vulnerable dependency. npm audit remains the vulnerability gate itself.
set -uo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
lockfile="$script_dir/rampart-service/package-lock.json"

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

# locked_ranges PARENT DEP prints VERSION<TAB>RANGE for every distinct locked
# instance of PARENT. package-lock v3 records the parent's original dependency
# range even when an override changes the resolved child, which is exactly the
# evidence this canary needs.
locked_ranges() {
  local parent="$1" dep="$2"
  node - "$lockfile" "$parent" "$dep" <<'NODE'
const fs = require("node:fs");
const [lockPath, parent, dep] = process.argv.slice(2);
const lock = JSON.parse(fs.readFileSync(lockPath, "utf8"));
const suffix = `node_modules/${parent}`;
const seen = new Set();
for (const [path, pkg] of Object.entries(lock.packages ?? {})) {
  if (path !== suffix && !path.endsWith(`/${suffix}`)) continue;
  const version = pkg.version ?? "";
  const range = pkg.dependencies?.[dep] ?? "";
  const row = `${version}\t${range}`;
  if (!seen.has(row)) {
    seen.add(row);
    process.stdout.write(`${row}\n`);
  }
}
NODE
}

# check PARENT DEP PATCHED_FLOOR — fail only if every locked PARENT instance's
# declared range for DEP starts at or above PATCHED_FLOOR.
stale=0
check() {
  local parent="$1" dep="$2" patched="$3" version range f
  local -a entries=()
  mapfile -t entries < <(locked_ranges "$parent" "$dep" 2>/dev/null || true)
  if (( ${#entries[@]} == 0 )); then
    echo "notice: could not find locked $parent instances — skipping (not a verdict)."
    return 0
  fi

  local all_fixed=1
  for entry in "${entries[@]}"; do
    IFS=$'\t' read -r version range <<<"$entry"
    if [ -z "$version" ] || [ -z "$range" ]; then
      echo "notice: locked $parent has no readable version/$dep range — skipping that instance."
      all_fixed=0
      continue
    fi
    f="$(floor "$range")"
    if [ "$f" = "unknown" ]; then
      echo "notice: locked $parent@$version declares $dep '$range' — cannot parse a floor."
      all_fixed=0
      continue
    fi
    if ge "$f" "$patched"; then
      echo "locked $parent@$version accepts $dep '$range' (floor $f >= $patched)."
    else
      echo "override for $dep still required: locked $parent@$version pins '$range' (floor $f < $patched)."
      all_fixed=0
    fi
  done

  if (( all_fixed )); then
    echo "::error::Every locked $parent instance now accepts $dep >= $patched:"
    echo "  the '$dep' override in scripts/rampart-service/package.json is DROPPABLE."
    echo "  Remove it, regenerate package-lock.json (npm install --package-lock-only),"
    echo "  and re-run npm audit — upstream now ships the patched line itself."
    stale=1
  fi
}

check "@huggingface/transformers" "sharp"   "0.35.0"
check "onnxruntime-node"          "adm-zip" "0.6.0"

exit "$stale"
