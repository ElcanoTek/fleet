# shellcheck shell=bash
# ^ no shebang: this file is only ever sourced, never executed. The directive
#   tells shellcheck which dialect to check it as (see the bash note below).
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

# fleet_node_version_major VERSION
#   Echo the MAJOR from a `node -v`-style string (v24.11.0 -> 24, 24.11.0 -> 24),
#   or return 1 on anything that is not a version. Exists so a caller comparing
#   "what did npm actually run under?" against web/.nvmrc does not grow its own
#   sed/cut pipeline — the same reason fleet_node_major_want exists.
fleet_node_version_major() {
  local raw="${1#v}"
  raw="${raw%%.*}"
  [[ "$raw" =~ ^[0-9]+$ ]] || return 1
  printf '%s' "$raw"
}

# fleet__shquote WORD — internal. Single-quote WORD for safe embedding in the
# generated shim wrappers below.
fleet__shquote() { printf "'%s'" "${1//\'/\'\\\'\'}"; }

# fleet__npm_cli_at CANDIDATE — internal. Echo CANDIDATE fully resolved when it
# is a readable JavaScript file, else return 1. `readlink -f` because every
# packaging of npm puts a symlink in bindir and the shebang — the thing that
# decides npm's interpreter — lives in the target, not the link.
fleet__npm_cli_at() {
  local cand="$1" real
  [[ -n "$cand" ]] || return 1
  real="$(readlink -f -- "$cand" 2>/dev/null)" || return 1
  [[ -n "$real" && -f "$real" && -r "$real" && "$real" == *.js ]] || return 1
  printf '%s' "$real"
}

# fleet_resolve_npm_cli NODE_BIN
#   Echo the path to the npm CLI entrypoint (npm-cli.js) belonging to NODE_BIN,
#   or return 1 when none can be found.
#
#   PATH cannot answer "which node will npm run under?" on the distro this whole
#   file targets. Fedora's nodejs spec renames the interpreter to node-<major>
#   and then rewrites npm's shebang to that interpreter's ABSOLUTE path:
#
#       SHEBANG_ERE='^#!/usr/bin/(env\s+)?node\b'
#       SHEBANG_FIX='#!%{_bindir}/node-%{node_version_major}'
#
#   It has to: the streams are parallel-installable, so a relative `env node`
#   shebang would make npm-22 run under whichever stream is the default. The
#   consequence is that /usr/bin/npm -> npm-22 -> .../npm-cli.js begins
#   `#!/usr/bin/node-22` and runs under node 22 NO MATTER WHAT PATH SAYS. A
#   `node` symlink at the head of PATH (what fleet_node_build_path used to be,
#   on its own) moves `next` and every other `env node` shebang onto the
#   resolved interpreter but cannot move npm itself — so `fleet update` printed
#   "web tier will build+run on /usr/bin/node-24 (v24.x)" and npm then printed
#   `EBADENGINE ... required: { node: '>=24' }, current: { node: 'v22.23.1' }`.
#   Same class of bug as the directory-prefix version, one link further down.
#
#   The only way to pin npm's interpreter is to stop letting the shebang decide
#   it: run `<node_bin> <npm-cli.js>`. This resolves the .js;
#   fleet_node_build_path writes the wrapper that does the invoking.
fleet_resolve_npm_cli() {
  local node_bin="$1" dir base major cand
  [[ -n "$node_bin" ]] || return 1
  dir="$(cd "$(dirname "$node_bin")" 2>/dev/null && pwd)" || return 1
  base="${node_bin##*/}"

  if [[ "$base" =~ ^node-([0-9]+)$ ]]; then
    # A versioned interpreter means parallel streams, and then the stream's OWN
    # npm-<major> is the only npm that matches it. Deliberately no fallback to
    # the unversioned `npm` here: that is the default stream's npm, i.e. exactly
    # the wrong answer this function exists to stop returning. A box with
    # nodejs24 and no nodejs24-npm is a real (and repairable) state, so it gets
    # a refusal with the one-package fix, not a silent downgrade.
    major="${BASH_REMATCH[1]}"
    for cand in "$dir/npm-$major" "/usr/bin/npm-$major" "/usr/local/bin/npm-$major"; do
      fleet__npm_cli_at "$cand" && return 0
    done
    return 1
  fi

  # One-node layouts (Debian/Ubuntu, nodesource, nvm): npm beside node, else on
  # PATH. Both ship it as a symlink to npm-cli.js, so pinning works there too —
  # and pinning is still worth doing, because it is the only form of the answer
  # that does not depend on PATH resolution order.
  for cand in "$dir/npm" "$(command -v npm 2>/dev/null || true)"; do
    fleet__npm_cli_at "$cand" && return 0
  done
  return 1
}

# The marker file every shim directory carries, so cleanup can tell a directory
# THIS file created from any other first PATH entry. Plain assignment, not
# readonly: this library is sourced by scripts that may source it twice.
FLEET_NODE_SHIM_MARKER='.fleet-node-shim'

# fleet__write_shim_wrapper DEST NODE_BIN CLI_JS — internal. Write an executable
# wrapper at DEST that execs NODE_BIN against CLI_JS. `exec`, so nothing lingers
# between the caller and node.
fleet__write_shim_wrapper() {
  local dest="$1" node_bin="$2" cli="$3"
  {
    printf '#!/bin/sh\n'
    printf '# Generated by scripts/lib/node-version.sh — pins this command to one interpreter.\n'
    printf 'exec %s %s "$@"\n' "$(fleet__shquote "$node_bin")" "$(fleet__shquote "$cli")"
  } >"$dest" 2>/dev/null || return 1
  chmod 0755 "$dest" 2>/dev/null || return 1
}
# fleet_node_build_path NODE_BIN
#   Echo a PATH under which `node`, `npm` and `npx` all run under NODE_BIN, so
#   a build invoked as plain `npm ci && npm run build` uses THAT interpreter.
#   The caller removes the directory it may create (see the cleanup below).
#
#   This is not cosmetic, and it took two goes to get right. Both failures had
#   the same shape — a PATH edit that looked like it pinned the interpreter and
#   did not — so both are recorded here rather than deleted:
#
#   1. Prefixing NODE_BIN's DIRECTORY is not enough, and on Fedora it is exactly
#      wrong: the streams are parallel-installable INTO THE SAME DIRECTORY, so
#      /usr/bin holds both `node-24` and the default stream's `node`. Putting
#      /usr/bin in front still resolves `node` to the old major. Hence a private
#      shim directory holding a `node` symlink, in front.
#   2. A `node` symlink alone still does not move npm. Fedora rewrites npm's
#      shebang to an ABSOLUTE `#!/usr/bin/node-<major>` (it must — see
#      fleet_resolve_npm_cli), so `npm ci` kept running under the default
#      stream while the update reported the resolved one, and the build tripped
#      npm's own EBADENGINE against web/package.json's `"node": ">=24"`. So the
#      shim also carries `npm` and `npx` wrappers that exec NODE_BIN against
#      the matching npm-cli.js / npx-cli.js — the one form of the answer no
#      shebang can override.
#
#   Everything npm then spawns inherits this PATH ahead of /usr/bin — npm's
#   run-script only prepends the package's node_modules/.bin dirs and its
#   node-gyp shim, it does not inject node's own directory — so `next` and every
#   other `#!/usr/bin/env node` bin in the build resolves to the shim too.
#
#   The shim is mktemp'd rather than given a predictable name. A fixed path
#   under /tmp would be a world-writable directory placed at the FRONT of root's
#   PATH for a build — an attacker who pre-creates it owns the build. mktemp -d
#   is 0700 and unpredictable. The caller is responsible for removing it; both
#   in-repo callers do, in the subshell that uses it.
#
#   A shim is now built unconditionally, where the old version returned early
#   when NODE_BIN's directory already resolved `node` to it. That early return
#   answered the node question and silently left the npm one to PATH, which on a
#   box with two streams in one directory is failure 2 above. One code path, one
#   guarantee.
fleet_node_build_path() {
  local node_bin="$1" dir npm_cli npx_cli shim
  dir="$(cd "$(dirname "$node_bin")" 2>/dev/null && pwd)" || { printf '%s' "$PATH"; return 1; }
  shim="$(mktemp -d 2>/dev/null)" || { printf '%s:%s' "$dir" "$PATH"; return 1; }
  if ! : >"$shim/$FLEET_NODE_SHIM_MARKER" || ! ln -s "$node_bin" "$shim/node" 2>/dev/null; then
    rm -rf "$shim"
    printf '%s:%s' "$dir" "$PATH"
    return 1
  fi
  # No npm-cli.js resolvable (a distro shipping npm as a shell wrapper, or a
  # versioned stream installed without its -npm package) is NOT patched over
  # with the default stream's npm: the PATH comes back with `node` pinned, the
  # bare `npm` keeps whatever interpreter its shebang names, and the caller's
  # read-back (fleet_npm_node_version) is what reports it. Guessing here would
  # re-hide precisely the divergence this function exists to surface.
  npm_cli="$(fleet_resolve_npm_cli "$node_bin" || true)"
  if [[ -n "$npm_cli" ]]; then
    fleet__write_shim_wrapper "$shim/npm" "$node_bin" "$npm_cli" || rm -f "$shim/npm"
    npx_cli="${npm_cli%/*}/npx-cli.js"
    if [[ -r "$npx_cli" ]]; then
      fleet__write_shim_wrapper "$shim/npx" "$node_bin" "$npx_cli" || rm -f "$shim/npx"
    fi
  fi
  printf '%s:%s:%s' "$shim" "$dir" "$PATH"
}

# fleet_node_build_path_cleanup PATH_STRING NODE_BIN
#   Remove the shim directory fleet_node_build_path created, given the PATH it
#   returned. A no-op when no shim was created (mktemp failed). Never removes a
#   directory that is not a shim: it requires the marker file, a `node` symlink
#   pointing at NODE_BIN, and nothing in there beyond the wrappers this library
#   writes.
fleet_node_build_path_cleanup() {
  local path_str="$1" node_bin="$2" first entry
  first="${path_str%%:*}"
  [[ -n "$first" && -d "$first" ]] || return 0
  [[ -f "$first/$FLEET_NODE_SHIM_MARKER" ]] || return 0
  [[ -L "$first/node" && "$first/node" -ef "$node_bin" ]] || return 0
  while IFS= read -r entry; do
    case "$entry" in
      "$FLEET_NODE_SHIM_MARKER" | node | npm | npx) ;;
      *) return 0 ;;
    esac
  done < <(cd "$first" && ls -A)
  rm -rf "$first"
}

# fleet_npm_node_version BUILD_PATH [CWD]
#   Echo the node version string npm ACTUALLY runs under (e.g. v24.11.0), or
#   return 1 when npm cannot be asked. This is the read-back for the pin above:
#   npm's interpreter is decided by its shebang, so the only trustworthy source
#   for "which node is building the web tier?" is npm's own process. `npm
#   version` with no version argument is read-only — it prints the versions
#   object and bumps nothing — and --json makes the node field greppable.
fleet_npm_node_version() {
  local build_path="$1" cwd="${2:-.}" out ver
  out="$(cd "$cwd" 2>/dev/null && PATH="$build_path" npm version --json 2>/dev/null)" || return 1
  ver="$(printf '%s\n' "$out" | sed -n 's/.*"node": *"\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$ver" ]] || return 1
  printf 'v%s' "${ver#v}"
}
