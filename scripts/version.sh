#!/usr/bin/env bash
# scripts/version.sh — fleet's build identity, derived from git. The ONE place
# that knows how a fleet release number is shaped.
#
# fleet ships from a rolling release train: `dev` integrates, every promotion
# squash-merges into `main`, and every push to `main` that passes the full CI
# gate is a release. There is no VERSION file, no semver bump to argue about,
# and NOBODY EVER TYPES A VERSION NUMBER — the release workflow
# (.github/workflows/release.yml) derives the next date-based tag from the clock
# and the tags already on the repo, and builds derive theirs from `git
# describe`. See docs/VERSIONING.md and ADR-0059.
#
# The release number is CalVer with a per-day ordinal:
#
#     YYYY.MM.DD.N        e.g. 2026.09.04.1, 2026.09.04.2, 2026.12.25.1
#
# tagged as `v<version>`. The date is UTC (a release train that changes shape
# with the releaser's timezone is not reproducible) and N counts that day's
# releases from 1, because several a day is the normal case, not an exception.
#
# Subcommands (all read-only; none of them writes a tag — release.yml does that):
#
#   current   the newest release tag's version, or the `dev` sentinel when the
#             repo carries no release tag yet.
#   describe  what a build stamps into the binary: the version if HEAD is
#             exactly a release, else <version>+<n>.g<sha> ("n commits past that
#             release"), with a `.dirty` suffix for a modified tree. Falls back
#             to dev+g<sha> before the first release.
#   next      the next unused tag for today, e.g. v2026.09.04.3. Used by
#             release.yml; safe to run locally to see what the next push would
#             be tagged.
#   semver    the same identity re-rendered as a strict 3-field SemVer,
#             YYYY.<MM*100+DD>.<N> (2026.09.04.1 -> 2026.904.1), for the two
#             ecosystems that PARSE the field and reject four components or a
#             leading zero: Helm chart versions and npm package versions. It is
#             a rendering, never a second source of truth, and it sorts in the
#             same order as the tags it comes from.
#
# Kept in POSIX-ish bash with no dependency beyond git so bootstrap.sh,
# update.sh, the Makefile and CI can all call it.

set -euo pipefail

# Release tags only. The glob is deliberately narrow — it must not match a
# hand-cut `v1.2.3` or a `sandbox-image-*` tag, because `next` derives an
# ordinal by counting matches and `describe` would otherwise stamp a version
# this scheme cannot parse.
readonly TAG_GLOB='v[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].[0-9]*'

# The sentinel an unreleased/unstamped build reports. Matches
# internal/version's own fallback: an honest "dev" beats a fabricated number.
readonly DEV_SENTINEL='dev'

die() {
	printf 'version.sh: %s\n' "$1" >&2
	exit 1
}

in_git_repo() {
	git rev-parse --git-dir >/dev/null 2>&1
}

short_sha() {
	git rev-parse --short=12 HEAD 2>/dev/null || echo unknown
}

tree_is_dirty() {
	# --quiet exits 1 when there are differences. Untracked files do not count:
	# a stray scratch file is not a modified build.
	! git diff --quiet HEAD 2>/dev/null
}

# newest_tag prints the highest release tag in the repo, or nothing.
# --sort=-v:refname compares dotted digit runs numerically, so .10 beats .2.
newest_tag() {
	git tag --list "$TAG_GLOB" --sort=-v:refname 2>/dev/null | head -n 1
}

cmd_current() {
	local tag
	tag="$(newest_tag)"
	if [ -z "$tag" ]; then
		echo "$DEV_SENTINEL"
		return 0
	fi
	echo "${tag#v}"
}

cmd_describe() {
	if ! in_git_repo; then
		# A source tarball with no .git — honest sentinel, no invented number.
		echo "$DEV_SENTINEL"
		return 0
	fi

	# Everything after the release number is SemVer build metadata: one `+`,
	# then dot-separated parts. Collected rather than concatenated so a build
	# that is both past a release and dirty renders as
	# `2026.09.04.1+3.g1a2b3c4d5e6f.dirty` instead of growing a second `+`.
	local -a meta=()
	local version

	local described
	if described="$(git describe --tags --long --abbrev=12 --match "$TAG_GLOB" 2>/dev/null)"; then
		# described looks like v2026.09.04.1-3-g1a2b3c4d5e6f
		local sha without_sha distance
		sha="${described##*-g}"
		without_sha="${described%-g*}"
		distance="${without_sha##*-}"
		version="${without_sha%-*}"
		version="${version#v}"
		if [ "$distance" != "0" ]; then
			meta+=("$distance" "g${sha}")
		fi
	else
		# No release tag is reachable from HEAD (a fresh clone before the first
		# release, or a shallow clone with no tags fetched).
		version="$DEV_SENTINEL"
		meta+=("g$(short_sha)")
	fi

	if tree_is_dirty; then
		meta+=('dirty')
	fi

	if [ "${#meta[@]}" -eq 0 ]; then
		echo "$version"
		return 0
	fi
	local joined
	joined="$(
		IFS='.'
		echo "${meta[*]}"
	)"
	echo "${version}+${joined}"
}

cmd_next() {
	local date="${1:-}"
	if [ -z "$date" ]; then
		date="$(date -u +%Y.%m.%d)"
	fi
	case "$date" in
	[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9]) ;;
	*) die "next: date must be YYYY.MM.DD (got '$date')" ;;
	esac

	# Highest ordinal already used for that date, 0 when the day is unopened.
	local highest=0 tag ordinal
	while read -r tag; do
		[ -n "$tag" ] || continue
		ordinal="${tag##*.}"
		case "$ordinal" in
		'' | *[!0-9]*) continue ;;
		esac
		if [ "$ordinal" -gt "$highest" ]; then
			highest="$ordinal"
		fi
	done <<<"$(git tag --list "v${date}.[0-9]*" 2>/dev/null)"

	echo "v${date}.$((highest + 1))"
}

cmd_semver() {
	local version
	version="$(cmd_current)"
	if [ "$version" = "$DEV_SENTINEL" ]; then
		# Both ecosystems that consume this REQUIRE a parseable version, so the
		# pre-release placeholder is the all-zero one rather than a sentinel
		# word that would fail their schema validation.
		echo '0.0.0'
		return 0
	fi

	local year month day ordinal
	IFS='.' read -r year month day ordinal <<<"$version"
	# 10# forces base-10 so the zero-padded 08/09 are not read as octal.
	printf '%d.%d.%d\n' "$((10#$year))" "$((10#$month * 100 + 10#$day))" "$((10#$ordinal))"
}

main() {
	local cmd="${1:-describe}"
	shift || true
	case "$cmd" in
	current) cmd_current "$@" ;;
	describe) cmd_describe "$@" ;;
	next) cmd_next "$@" ;;
	semver) cmd_semver "$@" ;;
	-h | --help | help)
		sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'
		;;
	*) die "unknown subcommand '$cmd' (want: current | describe | next | semver)" ;;
	esac
}

main "$@"
