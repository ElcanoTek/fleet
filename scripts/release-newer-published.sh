#!/usr/bin/env bash
# scripts/release-newer-published.sh TAG — print the release tags NEWER than
# TAG that are successfully PUBLISHED: a non-draft GitHub release that carries
# SHA256SUMS (what a complete publish uploads, and what a box verifies first).
# Newest last; nothing printed means TAG is the newest published release.
#
# release.yml decides the Latest mark (and the :latest image tags) with this,
# not with "is there a newer TAG": a newer tag whose build failed, or that is
# still building, has no release a box can install, and must not keep an
# older, complete release from being Latest. Exit 2 when the releases cannot
# be listed (the caller then leaves Latest alone rather than guess).
# Needs: gh (GH_TOKEN), scripts/version.sh.
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 TAG" >&2; exit 2; }
tag="$1"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ -n "$(printf '%s\n' "$tag" | "$here/version.sh" filter)" ]] || { echo "not a release tag: $tag" >&2; exit 2; }

if ! listed="$(gh release list --limit 200 --exclude-drafts --json tagName --jq '.[].tagName')"; then
  echo "could not list the releases" >&2
  exit 2
fi
newer="$(printf '%s\n%s\n' "$listed" "$tag" | awk 'NF && !seen[$0]++' | "$here/version.sh" filter \
  | awk -v t="$tag" 'found { print } $0 == t { found = 1 }')"
[[ -n "$newer" ]] || exit 0
while IFS= read -r t; do
  if ! complete="$(gh release view "$t" --json isDraft,assets --jq '(.isDraft | not) and ([.assets[].name] | index("SHA256SUMS") != null)')"; then
    echo "could not read release $t" >&2
    exit 2
  fi
  [[ "$complete" == "true" ]] && printf '%s\n' "$t"
done <<<"$newer"
exit 0
