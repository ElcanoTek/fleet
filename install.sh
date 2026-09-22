#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# install.sh — public one-line installer for fleet.
#
#   curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash
#
# Clones main into /opt/fleet/src (or FLEET_SRC_DIR) and hands off to
# scripts/bootstrap.sh, which prompts for the service / web / domain / key
# choices when it has a terminal. Any arguments are passed straight through, so
# an unattended install is:
#
#   curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh \
#     | sudo bash -s -- --postgres=local --enable-web --domain fleet.example.com \
#         --client-config https://github.com/ElcanoTek/example-config.git
#
# Re-running on a box that already has a clean checkout fast-forwards it and
# re-runs bootstrap (which is idempotent); a dirty or non-main checkout is left
# alone. Everything lives inside main() so a truncated download never runs.
set -euo pipefail
main() {
  local src="${FLEET_SRC_DIR:-/opt/fleet/src}"
  local repo="${FLEET_REPO_URL:-https://github.com/ElcanoTek/fleet.git}"
  if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
    cat <<EOF
Usage: curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash [-s -- BOOTSTRAP_FLAGS...]

Clones fleet main into $src and runs scripts/bootstrap.sh with BOOTSTRAP_FLAGS.
With no flags on a terminal, bootstrap asks for everything it needs.
Common flags: --postgres=local|external  --enable-service  --enable-web --domain <host>
              --client-config <git-url[#ref]|path>  --dry-run
After install: fleet status; sudo fleet doctor; fleet update.
EOF
    exit 0
  fi
  [[ $EUID == 0 ]] || { echo 'Run as root: curl -fsSL …/install.sh | sudo bash' >&2; exit 1; }
  command -v dnf >/dev/null || { echo 'fleet installs on Fedora/RHEL with dnf' >&2; exit 1; }
  command -v git >/dev/null || dnf install -y git ca-certificates

  if [[ -d "$src/.git" ]]; then
    local branch dirty
    branch="$(git -C "$src" rev-parse --abbrev-ref HEAD)"
    dirty="$(git -C "$src" status --porcelain --untracked-files=no)"
    if [[ "$branch" == main && -z "$dirty" ]]; then
      echo "Updating existing checkout at $src"
      git -C "$src" pull --ff-only
    else
      echo "Keeping $src as-is (branch $branch${dirty:+, local changes}); use fleet update to move it." >&2
    fi
  elif [[ -e "$src" ]]; then
    echo "$src exists but is not a git checkout; move it aside or set FLEET_SRC_DIR." >&2
    exit 1
  else
    mkdir -p "$(dirname "$src")"
    git clone --branch main --single-branch "$repo" "$src"
  fi

  # curl | bash leaves stdin on the pipe; give bootstrap the terminal so its
  # prompts work. Without one it runs non-interactively on flags/defaults.
  if [[ ! -t 0 ]] && { : </dev/tty; } 2>/dev/null; then
    exec bash "$src/scripts/bootstrap.sh" "$@" </dev/tty
  fi
  exec bash "$src/scripts/bootstrap.sh" "$@"
}
main "$@"
