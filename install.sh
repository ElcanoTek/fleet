#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# install.sh — public one-line installer for fleet. Design note: docs/INSTALLER.md.
#
#   curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash
#
# Clones main into /opt/fleet/src (or FLEET_SRC_DIR) and hands off to
# scripts/bootstrap.sh. With no arguments on a terminal, bootstrap prompts for
# the service / web / domain / key choices. Any arguments are passed straight
# through and bootstrap runs non-interactively on them:
#
#   curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh \
#     | sudo bash -s -- --postgres=local --enable-web --domain fleet.example.com \
#         --client-config https://github.com/ElcanoTek/example-config.git
#
# --dry-run changes nothing on the host: it prints the installer's plan and runs
# bootstrap's own dry run from a throwaway clone (or the existing checkout).
# Re-running on a box with a clean main checkout fast-forwards it and re-runs
# bootstrap (which is idempotent); a dirty or non-main checkout is left alone.
# Everything lives inside functions, and the call on the last line is wrapped in
# { …; } so a truncated download is a syntax error rather than a partial run.
set -euo pipefail

usage() {
  cat <<EOF
Usage: curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash [-s -- BOOTSTRAP_FLAGS...]

Clones fleet main into $1 and runs scripts/bootstrap.sh with BOOTSTRAP_FLAGS.
With no flags on a terminal, bootstrap asks for everything it needs; with flags
it runs unattended. For automation, download first so a failed fetch is an error:
  curl -fsSLo /tmp/fleet-install.sh https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh \\
    && sudo bash /tmp/fleet-install.sh FLAGS... </dev/null
Common flags: --postgres=local|external  --enable-service  --enable-web --domain <host>
              --client-config <git-url[#ref]|path>  --dry-run (changes nothing)
After install: fleet status; sudo fleet doctor; sudo fleet update.
EOF
}

# absolutize FLAG VALUE CALLER_PWD — bootstrap runs from the checkout, so a
# local path the caller gave relative to their own directory must be made
# absolute first. URLs, absolute paths and non-path values pass through.
absolutize() {
  local flag="$1" value="$2" base="$3"
  case "$flag" in
    --client-config)
      # Test the whole value first: bootstrap allows "#" in a filesystem path
      # and treats it as a ref separator only for URLs, so bundles/acme#prod
      # may be a directory. Fall back to the part before "#" (path#ref form).
      if [[ "$value" != /* && "$value" != *://* && "$value" != git@* ]] \
         && [[ -e "$base/$value" || -e "$base/${value%%#*}" ]]; then
        value="$base/$value"
      fi ;;
    --auth-pubkey)
      if [[ "$value" == @* && "$value" != @/* ]]; then value="@$base/${value#@}"; fi ;;
  esac
  printf '%s' "$value"
}

main() {
  local src="${FLEET_SRC_DIR:-/opt/fleet/src}"
  src="${src%/}"   # a trailing slash would put the .partial clone inside the target
  local repo="${FLEET_REPO_URL:-https://github.com/ElcanoTek/fleet.git}"
  local arg want="" dry_run=0 caller_pwd="$PWD"
  local -a args=()
  # Parse the way bootstrap does, so a value-taking flag consumes the next word:
  # "--client-config --dry-run" is a (bad) client-config value, not a dry run.
  for arg in "$@"; do
    if [[ -n "$want" ]]; then
      args+=("$(absolutize "$want" "$arg" "$caller_pwd")"); want=""; continue
    fi
    case "$arg" in
      --help|-h) usage "$src"; exit 0 ;;
      --dry-run) dry_run=1; args+=("$arg") ;;
      --client-config|--auth-pubkey|--domain|--admin|--chat-db-name|--chat-db-user|--sched-db-name|--sched-db-user)
        want="$arg"; args+=("$arg") ;;
      --client-config=*|--auth-pubkey=*)
        args+=("${arg%%=*}=$(absolutize "${arg%%=*}" "${arg#*=}" "$caller_pwd")") ;;
      *) args+=("$arg") ;;
    esac
  done
  set -- ${args[@]+"${args[@]}"}

  [[ $EUID == 0 ]] || { echo 'Run as root: curl -fsSL …/install.sh | sudo bash' >&2; exit 1; }
  command -v dnf >/dev/null || { echo 'fleet installs on Fedora/RHEL with dnf' >&2; exit 1; }

  if [[ "$dry_run" == 1 ]]; then
    local plan_src="$src" tmp=""
    command -v git >/dev/null || { echo "[dry-run] would: dnf install -y git ca-certificates (git is needed for the rest of the plan)"; exit 0; }
    if [[ -d "$src/.git" && ( "$(git -C "$src" rev-parse --abbrev-ref HEAD)" != main \
          || -n "$(git -C "$src" status --porcelain)" ) ]]; then
      # A real run keeps a dirty / non-main checkout as-is, so preview that tree.
      echo "[dry-run] would: keep $src as-is (not a clean main checkout), then run its bootstrap"
    else
      # A real run clones, or fast-forwards a clean main checkout, before running
      # bootstrap, so preview the current remote main rather than the local tree.
      if [[ -d "$src/.git" ]]; then
        echo "[dry-run] would: git -C $src pull --ff-only, then run bootstrap"
      else
        echo "[dry-run] would: git clone --branch main $repo $src, then run bootstrap"
      fi
      tmp="$(mktemp -d)"
      # Expand now: $tmp is local to main() and gone by the time EXIT fires.
      # shellcheck disable=SC2064
      trap "rm -rf '$tmp'" EXIT
      git clone -q --depth 1 --branch main --single-branch "$repo" "$tmp/src"
      plan_src="$tmp/src"
    fi
    cd "$plan_src"
    bash scripts/bootstrap.sh "$@" </dev/null
    return
  fi

  command -v git >/dev/null || dnf install -y git ca-certificates
  if [[ -d "$src/.git" ]]; then
    local branch dirty
    branch="$(git -C "$src" rev-parse --abbrev-ref HEAD)"
    # Untracked non-ignored files count too: bootstrap would build them in.
    dirty="$(git -C "$src" status --porcelain)"
    if [[ "$branch" == main && -z "$dirty" ]]; then
      echo "Updating existing checkout at $src"
      git -C "$src" pull --ff-only
    else
      echo "Keeping $src as-is (branch $branch${dirty:+, local changes}); use sudo fleet update to move it." >&2
    fi
  elif [[ -e "$src" ]]; then
    echo "$src exists but is not a git checkout; move it aside or set FLEET_SRC_DIR." >&2
    exit 1
  else
    mkdir -p "$(dirname "$src")"
    # Clone next to the target and rename, so an interrupted clone never leaves
    # a half-populated $src that blocks the next run.
    local partial="$src.partial.$$"
    # shellcheck disable=SC2064
    trap "rm -rf '$partial'" EXIT
    git clone --branch main --single-branch "$repo" "$partial"
    mv "$partial" "$src"
    trap - EXIT
  fi

  # Run from the checkout so relative paths (a dev-mode .env.local) land in one
  # stable place across reruns, whatever directory curl was started from.
  cd "$src"
  # curl | bash leaves stdin on the pipe. Only the no-argument form is the
  # interactive install, so only then reattach the terminal for bootstrap's
  # prompts; with flags, stdin stays non-interactive and bootstrap never asks.
  if [[ $# == 0 && ! -t 0 ]] && { : </dev/tty; } 2>/dev/null; then
    exec bash scripts/bootstrap.sh </dev/tty
  fi
  exec bash scripts/bootstrap.sh "$@"
}
# The call sits inside a group whose closing brace is the last byte that matters:
# a download cut off anywhere before it is a syntax error, never a bare "main".
{ main "$@"; }
