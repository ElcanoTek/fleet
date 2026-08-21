# scripts/lib/node-version.sh — the ONE implementation of "which node?".
#
# Sourced by scripts/bootstrap.sh, scripts/doctor.sh and scripts/update.sh. It
# exists because those three each grew their own copy of this logic, which is
# the same disease as the six CI jobs that hardcoded node 22: four call
# sites that must be edited in lockstep, and a bug fixed in one of them stays
# alive in the others. (The first version of this change shipped exactly that:
# byte-identical copies in bootstrap and doctor plus an inlined variant in
# update, all three carrying the same docstring bug.)
#
# bash, not sh: the callers are bash and use [[ ]] throughout.

# fleet_node_major_want REPO_ROOT
#   Echo the node MAJOR this repo targets, read from web/.nvmrc. Returns 1 and
#   echoes nothing if the file is missing or unparseable.
#
#   Deliberately NO fallback default. An earlier version did `|| NODE_MAJOR=24`,
#   which is worse than failing: .nvmrc legitimately holds forms like `lts/*`,
#   `v24` or `20.11.0` — all of which actions/setup-node's node-version-file
#   understands — so a silent default meant CI could build on 20.11.0 while the
#   box quietly targeted 24. That is precisely the CI-vs-box divergence this
#   file exists to prevent, so an unreadable .nvmrc is an error, not a guess.
#   A stale hardcoded fallback also rots in the worst possible way: it keeps
#   pointing at the OLD major exactly when nothing else can be read.
fleet_node_major_want() {
  local root="$1" raw
  [[ -r "$root/web/.nvmrc" ]] || return 1
  raw="$(tr -d '[:space:]' < "$root/web/.nvmrc")" || return 1
  raw="${raw#v}"        # v24 -> 24
  raw="${raw%%.*}"      # 24.19.0 -> 24
  [[ "$raw" =~ ^[0-9]+$ ]] || return 1
  printf '%s' "$raw"
}

# fleet_resolve_node_bin WANT
#   Echo the path to the NEWEST node interpreter whose major is >= WANT, or
#   return 1 if there is none.
#
#   Versioned paths are searched first, and by descending major rather than by
#   exact match. Two reasons:
#
#   1. On Fedora the node streams are parallel-installable: /usr/bin/node-24 is
#      unambiguous while /usr/bin/node is a symlink owned by whichever stream
#      the release designated default. Preferring the unversioned name is how a
#      box ends up serving an old major with the new one installed.
#   2. Matching only node-$WANT means a box with node-26 and no node-24 reports
#      "no suitable node" and refuses to deploy, despite having a NEWER
#      qualifying interpreter. It also couples a .nvmrc bump to the matching
#      nodejs<major> package existing in the distro repos.
fleet_resolve_node_bin() {
  local want="$1" cand base major best="" best_major=0
  for cand in /usr/bin/node-* /usr/local/bin/node-*; do
    [[ -x "$cand" ]] || continue           # also skips the literal glob when it does not match
    base="${cand##*/}"; major="${base#node-}"
    [[ "$major" =~ ^[0-9]+$ ]] || continue # node-gyp, node-waf, … are not interpreters
    (( major >= want )) || continue
    (( major > best_major )) && { best="$cand"; best_major="$major"; }
  done
  [[ -n "$best" ]] && { printf '%s' "$best"; return 0; }

  # Fall back to the unversioned interpreter — the portable case (Debian/Ubuntu,
  # nodesource, nvm) and any distro shipping exactly one node.
  cand="$(command -v node 2>/dev/null)" || return 1
  [[ -n "$cand" ]] || return 1
  major="$("$cand" -v 2>/dev/null | sed 's/^v//' | cut -d. -f1)"
  [[ "$major" =~ ^[0-9]+$ ]] || return 1
  (( major >= want )) || return 1
  printf '%s' "$cand"
}

# fleet_node_build_path NODE_BIN
#   Echo a PATH under which the bare name `node` resolves to NODE_BIN, so
#   `npm`/`npx` in a build subshell run under THAT interpreter.
#
#   This is not cosmetic. npm's shebang is `#!/usr/bin/env node`, so a build
#   invoked as plain `npm ci && npm run build` runs under whatever `node` PATH
#   resolves to — the distro DEFAULT stream. Without this, bootstrap/update
#   would print "web tier will run node 24" and then build the app on node 22,
#   which is the exact "reported a thing that did not happen" failure the whole
#   change set is about.
#
#   Prefixing NODE_BIN's DIRECTORY is not enough, and on Fedora it is exactly
#   wrong: the streams are parallel-installable INTO THE SAME DIRECTORY, so
#   /usr/bin holds both `node-24` and the default stream's `node`. Putting
#   /usr/bin in front still resolves `node` to the old major — measured, not
#   assumed — so this function's promise was false on precisely the layout it
#   was written for. When the directory does not already resolve `node` to
#   NODE_BIN, a private shim directory holding a single `node` symlink goes in
#   front instead.
#
#   The shim is mktemp'd rather than given a predictable name. A fixed path
#   under /tmp would be a world-writable directory placed at the FRONT of root's
#   PATH for a build — an attacker who pre-creates it owns the build. mktemp -d
#   is 0700 and unpredictable. The caller is responsible for removing it; both
#   in-repo callers do, in the subshell that uses it.
fleet_node_build_path() {
  local node_bin="$1" dir shim
  dir="$(cd "$(dirname "$node_bin")" && pwd)" || { printf '%s' "$PATH"; return 1; }
  if [[ "${dir}/node" -ef "$node_bin" ]]; then
    printf '%s:%s' "$dir" "$PATH"
    return 0
  fi
  shim="$(mktemp -d 2>/dev/null)" || { printf '%s:%s' "$dir" "$PATH"; return 1; }
  if ! ln -s "$node_bin" "$shim/node" 2>/dev/null; then
    rm -rf "$shim"
    printf '%s:%s' "$dir" "$PATH"
    return 1
  fi
  printf '%s:%s:%s' "$shim" "$dir" "$PATH"
}

# fleet_node_build_path_cleanup PATH_STRING NODE_BIN
#   Remove the shim directory fleet_node_build_path may have created, given the
#   PATH it returned. A no-op when no shim was needed. Never removes a directory
#   that is not a shim: it checks the entry holds exactly the expected symlink.
fleet_node_build_path_cleanup() {
  local path_str="$1" node_bin="$2" first
  first="${path_str%%:*}"
  [[ -n "$first" && -d "$first" && -L "$first/node" ]] || return 0
  [[ "$first/node" -ef "$node_bin" ]] || return 0
  [[ "$(cd "$first" && ls -A)" == "node" ]] || return 0
  rm -rf "$first"
}
