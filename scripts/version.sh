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
#   current   the newest release REACHABLE FROM HEAD, or the `dev` sentinel when
#             none is. Reachability, not "newest tag anywhere": an old checkout
#             in a clone that has fetched newer tags must not claim them.
#   describe  what a build stamps into the binary: the version if HEAD is
#             exactly a release, else <version>+<n>.g<sha> ("n commits past that
#             release"), with a `.dirty` suffix for a modified tree. Falls back
#             to dev+g<sha> before the first release.
#   next      the next unused tag for today, e.g. v2026.09.04.3. Used by
#             release.yml; safe to run locally to see what the next push would
#             be tagged.
#   released-at [rev]
#             the release tag ON that commit (default HEAD), or nothing. The
#             idempotence check release.yml uses.
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

# Release tags only. Git's glob is the PRE-filter, not the check: in git's
# fnmatch syntax `[0-9]*` is "one digit then anything at all", so this alone
# also matches `v2026.09.04.1oops`. It narrows what git walks; TAG_RE below is
# what actually decides whether a name is a release tag. Both are needed —
# `--match` takes a glob, not a regex.
readonly TAG_GLOB='v[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].[0-9]*'

# The real format. Anchored and fully numeric, so a tag the glob lets through
# but this scheme cannot parse is rejected here rather than surfacing as an
# arithmetic error in `semver` (`10#1oops`) or, worse, as a release name the
# tagging workflow mistakes for "already released" and silently skips.
readonly TAG_RE='^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$'

# The sentinel an unreleased/unstamped build reports. Matches
# internal/version's own fallback: an honest "dev" beats a fabricated number.
readonly DEV_SENTINEL='dev'

die() {
	printf 'version.sh: %s\n' "$1" >&2
	exit 1
}

# is_release_tag is the single yes/no for "is this name one of ours?".
is_release_tag() {
	printf '%s' "${1:-}" | grep -Eq "$TAG_RE"
}

# in_fleet_git_repo is true only when git discovery lands on THIS checkout.
#
# `git rev-parse --git-dir` alone answers "is there a repo anywhere above the
# cwd?", which is a different question: an unpacked fleet source archive sitting
# inside some unrelated git checkout would answer yes, and every command below
# would then read that repo's tags and HEAD — stamping a stranger's release and
# revision into a fleet binary instead of the documented `dev` fallback. So the
# discovered worktree root must be the root of the tree this script lives in.
in_fleet_git_repo() {
	local toplevel script_root
	toplevel="$(git rev-parse --show-toplevel 2>/dev/null)" || return 1
	# The script lives at <root>/scripts/version.sh; resolve symlinks so a
	# PATH-installed link does not change the answer.
	script_root="$(CDPATH='' cd -- "$(dirname -- "$(script_path)")/.." && pwd -P)" || return 1
	[ "$(CDPATH='' cd -- "$toplevel" && pwd -P)" = "$script_root" ]
}

script_path() {
	local src="${BASH_SOURCE[0]}" dir
	while [ -L "$src" ]; do
		dir="$(CDPATH='' cd -- "$(dirname -- "$src")" && pwd -P)"
		src="$(readlink -- "$src")"
		case "$src" in
		/*) ;;
		*) src="$dir/$src" ;;
		esac
	done
	printf '%s' "$src"
}

short_sha() {
	git rev-parse --short=12 HEAD 2>/dev/null || echo unknown
}

tree_is_dirty() {
	# `git status --porcelain` and NOT `git diff --quiet HEAD`: the latter sees
	# only tracked-file changes, so dropping an untracked .go file or an
	# embedded asset into the tree changes what `make build` produces while the
	# stamp still claims the pristine release. Ignored files stay excluded (they
	# are not build inputs), so this does not fire on ./fleet or coverage.out.
	[ -n "$(git status --porcelain --untracked-files=normal 2>/dev/null)" ]
}

# newest_tag prints the highest release tag REACHABLE FROM HEAD, or nothing.
#
# Reachability matters: `git tag --list` would answer with the newest tag
# anywhere in the repo, so checking out an older release on a box whose clone
# has since fetched newer tags would make `make helm-package` stamp the old
# chart with a newer release — an artifact whose declared version is not its
# source. `describe --abbrev=0` answers "the newest release this commit is
# descended from", which is the honest answer for anything built from HEAD.
newest_tag() {
	local tag
	tag="$(git describe --tags --abbrev=0 --match "$TAG_GLOB" 2>/dev/null)" || return 0
	is_release_tag "$tag" || return 0
	printf '%s' "$tag"
}

cmd_current() {
	local tag=''
	if in_fleet_git_repo; then
		tag="$(newest_tag)"
	fi
	if [ -z "$tag" ]; then
		echo "$DEV_SENTINEL"
		return 0
	fi
	echo "${tag#v}"
}

# cmd_released_at prints the release tag ON the given commit (default HEAD), or
# nothing. release.yml's idempotence check calls it rather than matching the
# glob itself: a `v2026.09.04.1oops` tag on the commit would otherwise read as
# "already released" and the real tag would never be cut.
cmd_released_at() {
	local rev="${1:-HEAD}" tag
	in_fleet_git_repo || return 0
	tag="$(git describe --tags --exact-match --match "$TAG_GLOB" "$rev" 2>/dev/null)" || return 0
	is_release_tag "$tag" || return 0
	printf '%s\n' "$tag"
}

cmd_describe() {
	if ! in_fleet_git_repo; then
		# A source tarball with no .git, or one unpacked inside somebody else's
		# checkout — honest sentinel either way, no invented number and none
		# borrowed from a repo that is not fleet.
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
		if is_release_tag "$version"; then
			version="${version#v}"
			if [ "$distance" != "0" ]; then
				meta+=("$distance" "g${sha}")
			fi
		else
			# The glob matched something this scheme cannot parse (see TAG_RE).
			# Stamping it would put an unparseable string in the binary and in
			# /api-info; the sentinel is the honest answer.
			version="$DEV_SENTINEL"
			meta=("g$(short_sha)")
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
	released-at) cmd_released_at "$@" ;;
	-h | --help | help)
		sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'
		;;
	*) die "unknown subcommand '$cmd' (want: current | describe | next | semver | released-at)" ;;
	esac
}

main "$@"
