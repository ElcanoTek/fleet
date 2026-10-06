#!/usr/bin/env bash
# Regenerate scripts/lib/node-release-keys.{list,asc} from nodejs/release-keys —
# the keyring fleet_node_tarball_install verifies SHASUMS256.txt.asc against
# (ADR-0078). Run it when .github/workflows/node-latest-reminder.yml files its
# key-drift issue, then review the fingerprint diff before committing.
set -euo pipefail

LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/lib" && pwd)"
BASE="https://raw.githubusercontent.com/nodejs/release-keys/main"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 "$BASE/keys.list" -o "$tmp/keys.list"
: >"$tmp/keys.asc"
while read -r fp; do
  [[ -n "$fp" ]] || continue
  [[ "$fp" =~ ^[0-9A-F]{40}$ ]] || { echo "unexpected line in keys.list: ${fp}" >&2; exit 1; }
  curl -fsSL --retry 3 "$BASE/keys/$fp.asc" >>"$tmp/keys.asc"
  echo >>"$tmp/keys.asc" # a key file without a trailing newline would glue its END line to the next BEGIN
done <"$tmp/keys.list"

# Every listed fingerprint must actually be in the keyring we are about to ship.
GNUPGHOME="$tmp" gpg --batch --show-keys --with-colons <"$tmp/keys.asc" 2>/dev/null \
  | awk -F: '$1 == "fpr" {print $10}' >"$tmp/have"
while read -r fp; do
  [[ -n "$fp" ]] || continue
  grep -qx "$fp" "$tmp/have" || { echo "keys/${fp}.asc did not contain key ${fp}" >&2; exit 1; }
done <"$tmp/keys.list"

mv "$tmp/keys.list" "$LIB/node-release-keys.list"
mv "$tmp/keys.asc" "$LIB/node-release-keys.asc"
echo "updated $LIB/node-release-keys.{list,asc} ($(grep -c . "$LIB/node-release-keys.list") keys)"
