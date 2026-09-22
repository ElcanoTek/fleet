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
# --dry-run changes nothing on the host: it runs the same checkout preparation
# as a real install (against a throwaway copy where that would write), then
# bootstrap's own dry run. Re-running on a box with a clean main checkout
# fast-forwards it and re-runs bootstrap (which is idempotent); a dirty or
# non-main checkout is left alone. Everything lives inside functions, and the
# call on the last line is wrapped in { …; } so a truncated download is a
# syntax error rather than a partial run.
set -euo pipefail
# Paths to remove on exit (a .partial clone, a dry-run copy). One global list and
# one trap, so no step can drop another step's cleanup by resetting the trap.
CLEANUP=()
trap 'rm -rf "${CLEANUP[@]}"' EXIT

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

# absolutize_env CALLER_PWD — the same for bootstrap's path-valued environment
# settings. FLEET_CLIENT_CONFIG_DIR keeps bootstrap's own rule (the caller's
# path when it exists there, else relative to the checkout), so it is only
# rewritten when it exists relative to the caller.
absolutize_env() {
  local base="$1" name value
  for name in FLEET_ENV_FILE FLEET_BACKUP_DIR FLEET_INSTALL_DIR FLEET_STATE_DIR FLEET_CLIENT_CONFIG_DIR; do
    value="${!name:-}"
    [[ -n "$value" && "$value" != /* ]] || continue
    if [[ "$name" == FLEET_CLIENT_CONFIG_DIR && ! -e "$base/$value" ]]; then continue; fi
    export "$name=$base/$value"
  done
}

# checkout_state DIR — absent | clean-main | keep | occupied: the one decision
# both the real run and --dry-run act on.
checkout_state() {
  local dir="$1"
  if [[ -d "$dir/.git" ]]; then
    # Untracked non-ignored files count as local changes: bootstrap would build them in.
    if [[ "$(git -C "$dir" rev-parse --abbrev-ref HEAD)" == main && -z "$(git -C "$dir" status --porcelain)" ]]; then
      echo clean-main
    else
      echo keep
    fi
  elif [[ -e "$dir" ]]; then
    echo occupied
  else
    echo absent
  fi
}

# prepare_checkout DIR REPO — act on checkout_state: clone into an absent DIR
# (via a .partial dir renamed on success), fast-forward a clean main checkout
# (aborting if it has diverged), keep anything else as-is, refuse a path that
# exists but is not a checkout.
prepare_checkout() {
  local dir="$1" repo="$2"
  case "$(checkout_state "$dir")" in
    clean-main)
      echo "Updating existing checkout at $dir"
      git -C "$dir" pull --ff-only ;;
    keep)
      echo "Keeping $dir as-is (not a clean main checkout); use sudo fleet update to move it." >&2 ;;
    occupied)
      echo "$dir exists but is not a git checkout; move it aside or set FLEET_SRC_DIR." >&2
      return 1 ;;
    absent)
      mkdir -p "$(dirname "$dir")"
      # Clone next to the target and rename, so an interrupted clone never leaves
      # a half-populated dir that blocks the next run.
      local partial="$dir.partial.$$"
      CLEANUP+=("$partial")
      git clone --branch main --single-branch "$repo" "$partial"
      mv "$partial" "$dir" ;;
  esac
}

main() {
  local src="${FLEET_SRC_DIR:-/opt/fleet/src}"
  # Every trailing slash, or the .partial clone would land inside the target.
  while [[ "$src" == */ && "$src" != / ]]; do src="${src%/}"; done
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
  absolutize_env "$caller_pwd"

  [[ $EUID == 0 ]] || { echo 'Run as root: curl -fsSL …/install.sh | sudo bash' >&2; exit 1; }
  command -v dnf >/dev/null || { echo 'fleet installs on Fedora/RHEL with dnf' >&2; exit 1; }

  if [[ "$dry_run" == 1 ]]; then
    command -v git >/dev/null || { echo "[dry-run] would: dnf install -y git ca-certificates (git is needed for the rest of the plan)"; exit 0; }
    local plan="$src" tmp
    case "$(checkout_state "$src")" in
      occupied)
        prepare_checkout "$src" "$repo" || { echo "[dry-run] a real run would stop here." >&2; exit 1; } ;;
      keep)
        # A real run builds this tree unchanged, so preview it in place (read-only).
        echo "[dry-run] would keep $src as-is (not a clean main checkout) and run its bootstrap" ;;
      clean-main|absent)
        # Both would write (pull or clone), so rehearse on a throwaway copy that
        # has the same commits and the same upstream: an ahead checkout previews
        # its own commits, a diverged one fails the same --ff-only pull.
        tmp="$(mktemp -d)"
        CLEANUP+=("$tmp")
        plan="$tmp/src"
        if [[ -d "$src/.git" ]]; then
          git clone -q --no-hardlinks --branch main "$src" "$plan"
          git -C "$plan" remote set-url origin "$(git -C "$src" remote get-url origin)"
          echo "[dry-run] would: git -C $src pull --ff-only (rehearsed on a copy)"
        else
          echo "[dry-run] would: git clone --branch main $repo $src (rehearsed in a temp dir)"
        fi
        prepare_checkout "$plan" "$repo" || { echo "[dry-run] a real run would stop at the checkout step." >&2; exit 1; } ;;
    esac
    cd "$plan"
    bash scripts/bootstrap.sh "$@" </dev/null
    return
  fi

  command -v git >/dev/null || dnf install -y git ca-certificates
  prepare_checkout "$src" "$repo"

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
