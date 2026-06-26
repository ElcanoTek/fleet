#!/usr/bin/env bash
# scripts/sandbox-mode.sh — set the per-turn sandbox mode on an
# existing /opt/chat install.
#
# Two modes:
#   default          — every chat runs in a per-turn rootless
#                      container; users can opt into Lockdown for
#                      vetted-models-only via the lock icon next
#                      to the "+" button.
#   lockdown-only    — every chat is forcibly Lockdown. Sensitive
#                      deploys.
#
# There is no "off" mode. bash and run_python execute arbitrary
# agent-emitted code; running them on the host would let the agent
# touch the chat-server process's filesystem and credentials. If
# rootless-podman cannot be configured on this box, chat-server
# refuses to start with a clear error pointing back at this script.
#
# Subcommands (`chat sandbox <sub>`):
#   status                        print current mode + restart hint
#   default                       enable; lockdown is per-chat opt-in
#   lockdown-only                 enable; force lockdown for every chat
#
# Both enable modes are idempotent. Restarts chat-server unless --no-restart.

set -euo pipefail

APP_DIR="${APP_DIR:-/opt/chat}"
APP_USER="${APP_USER:-chat}"
ENV_FILE="$APP_DIR/.env.local"
DEFAULT_IMAGE="localhost/fleet-sandbox:latest"

# Colors — TTY-gated, mirror of bootstrap.sh.
if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_bold=''
fi
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root: sudo chat sandbox <sub>"
[[ -f "$ENV_FILE" ]] || die "no install at $APP_DIR (run bootstrap first)"

NO_RESTART=0
mode=""
for arg in "$@"; do
  case "$arg" in
    --no-restart) NO_RESTART=1 ;;
    default|lockdown-only|status) mode="$arg" ;;
    -h|--help|help)
      cat <<'EOF'
chat sandbox — control per-turn sandbox containerization

  chat sandbox status            print current mode
  chat sandbox default           every chat sandboxed; lockdown is per-chat opt-in
  chat sandbox lockdown-only     every chat forcibly Lockdown

  --no-restart                   skip the systemctl restart
EOF
      exit 0
      ;;
    *) die "unknown arg: $arg" ;;
  esac
done
[[ -n "$mode" ]] || die "usage: chat sandbox <default|lockdown-only|status>"

# ── env file helpers ──────────────────────────────────────────────────
# read_env_var KEY → echoes the unquoted value or empty.
read_env_var() {
  local key="$1"
  if grep -q "^${key}=" "$ENV_FILE"; then
    grep "^${key}=" "$ENV_FILE" | head -1 | sed -E "s/^${key}=//; s/^\"//; s/\"$//"
  fi
}
# set_env_var KEY VAL → upsert KEY="VAL" in $ENV_FILE.
set_env_var() {
  local key="$1" val="$2"
  if grep -q "^${key}=" "$ENV_FILE"; then
    # Use | as delimiter to survive slashes in image refs.
    sed -i "s|^${key}=.*|${key}=\"${val}\"|" "$ENV_FILE"
  else
    printf '%s="%s"\n' "$key" "$val" >> "$ENV_FILE"
  fi
}
# remove_env_var KEY → drop any line starting with KEY=.
remove_env_var() {
  local key="$1"
  sed -i "/^${key}=/d" "$ENV_FILE"
}

current_mode() {
  local image lockdown
  image="$(read_env_var CHAT_SANDBOX_IMAGE)"
  lockdown="$(read_env_var CHAT_LOCKDOWN_ONLY)"
  if [[ -z "$image" ]]; then
    # Should not happen on a healthy install — bootstrap requires the
    # image. Treat as "needs setup" and surface that to the operator.
    echo "unconfigured"
  elif [[ "$lockdown" == "true" ]]; then
    echo "lockdown-only"
  else
    echo "default"
  fi
}

# ensure_prereqs runs the one-time rootless-podman setup. Idempotent —
# safe to call on every `default`/`lockdown-only` invocation.
ensure_prereqs() {
  command -v podman >/dev/null || dnf install -y --quiet podman
  if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
    usermod --add-subuids 100000-165535 "$APP_USER"
  fi
  if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
    usermod --add-subgids 100000-165535 "$APP_USER"
  fi
  loginctl enable-linger "$APP_USER" >/dev/null 2>&1 || true
  install -d -o "$APP_USER" -g "$APP_USER" \
    "$APP_DIR/.local/share/containers" \
    "$APP_DIR/.config/containers"
  # `install -d` only sets ownership on dirs it creates — if .local or
  # .config existed already (e.g. created by hand as root to work around
  # an old systemd ReadWritePaths= miss), they'd stay root-owned and
  # rootless podman would refuse to write into them ("path exists and
  # is not owned by the current user"). chown to be safe.
  chown -R "$APP_USER:$APP_USER" "$APP_DIR/.local" "$APP_DIR/.config"
}

restart_services() {
  if [[ "$NO_RESTART" == "1" ]]; then
    printf '  %s(skipping restart — start chat-server when ready)%s\n' "$c_dim" "$c_reset"
    return
  fi
  systemctl restart chat-server.service chat-web.service
  ok "chat services restarted"
}

# ── dispatch ──────────────────────────────────────────────────────────
case "$mode" in
  status)
    cur="$(current_mode)"
    image="$(read_env_var CHAT_SANDBOX_IMAGE)"
    printf '%ssandbox: %s%s\n' "$c_bold" "$cur" "$c_reset"
    case "$cur" in
      unconfigured) printf '  %schat-server will refuse to start. Run: sudo chat sandbox default%s\n' "$c_dim" "$c_reset" ;;
      default)      printf '  %severy chat sandboxed; users see a Lockdown opt-in next to "+".%s\n' "$c_dim" "$c_reset"
                    printf '  image: %s\n' "$image" ;;
      lockdown-only) printf '  %severy chat forcibly Lockdown — vetted models, no escape hatch.%s\n' "$c_dim" "$c_reset"
                    printf '  image: %s\n' "$image" ;;
    esac
    ;;
  default|lockdown-only)
    image="${CHAT_SANDBOX_IMAGE:-$DEFAULT_IMAGE}"
    ensure_prereqs
    printf '  %spulling %s as %s …%s\n' "$c_dim" "$image" "$APP_USER" "$c_reset"
    # Stream stderr through so rootless-podman setup errors are visible
    # (subuid mapping, storage driver, /run/user/UID missing, etc.).
    # stdout is silenced because the layer-by-layer progress is noisy.
    #
    # `cd` to $APP_DIR first because sudo preserves CWD by default —
    # if the operator runs `chat sandbox` from /root, podman inherits
    # /root as its working directory and chat can't chdir there
    # ("cannot chdir to /root: Permission denied" during setting up
    # the process).
    if ! ( cd "$APP_DIR" && sudo -u "$APP_USER" -H podman pull "$image" >/dev/null ); then
      die "podman pull failed — see the podman error above; common causes: missing newuidmap (\`sudo dnf install shadow-utils\`), no subuid/subgid for $APP_USER, or rootless storage init in /opt/chat/.local"
    fi
    set_env_var CHAT_SANDBOX_IMAGE "$image"
    set_env_var CHAT_WORKSPACE_ROOT "$APP_DIR/workspace"
    if [[ "$mode" == "lockdown-only" ]]; then
      set_env_var CHAT_LOCKDOWN_ONLY "true"
      ok "sandbox: lockdown-only ($image)"
    else
      remove_env_var CHAT_LOCKDOWN_ONLY
      ok "sandbox: default ($image)"
    fi
    restart_services
    ;;
esac
