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
# Everything lives inside main() so a truncated download never runs.
set -euo pipefail
main() {
  local src="${FLEET_SRC_DIR:-/opt/fleet/src}"
  local repo="${FLEET_REPO_URL:-https://github.com/ElcanoTek/fleet.git}"
  local arg dry_run=0
  for arg in "$@"; do
    case "$arg" in
      --help|-h)
        cat <<EOF
Usage: curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash [-s -- BOOTSTRAP_FLAGS...]

Clones fleet main into $src and runs scripts/bootstrap.sh with BOOTSTRAP_FLAGS.
With no flags on a terminal, bootstrap asks for everything it needs; with flags
it runs unattended. For automation, download first so a failed fetch is an error:
  curl -fsSLo /tmp/fleet-install.sh https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh && sudo bash /tmp/fleet-install.sh FLAGS...
Common flags: --postgres=local|external  --enable-service  --enable-web --domain <host>
              --client-config <git-url[#ref]|path>  --dry-run (changes nothing)
After install: fleet status; sudo fleet doctor; sudo fleet update.
EOF
        exit 0 ;;
      --dry-run) dry_run=1 ;;
    esac
  done
  [[ $EUID == 0 ]] || { echo 'Run as root: curl -fsSL …/install.sh | sudo bash' >&2; exit 1; }
  command -v dnf >/dev/null || { echo 'fleet installs on Fedora/RHEL with dnf' >&2; exit 1; }

  if [[ "$dry_run" == 1 ]]; then
    local plan_src="$src" tmp=""
    command -v git >/dev/null || { echo "[dry-run] would: dnf install -y git ca-certificates (git is needed for the rest of the plan)"; exit 0; }
    if [[ -d "$src/.git" ]]; then
      echo "[dry-run] would: update $src if it is a clean main checkout, then run bootstrap"
    else
      echo "[dry-run] would: git clone --branch main $repo $src, then run bootstrap"
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
main "$@"
