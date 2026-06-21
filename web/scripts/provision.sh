#!/usr/bin/env bash
# scripts/provision.sh — install Elcano-controlled shared secrets onto a chat box.
#
# Decrypts provision/clients/<client>.env.enc and upserts each KEY=VALUE into
# /opt/chat/.env.local. Existing bootstrap-managed keys (DATABASE_URL,
# OPENROUTER_API_KEY, APP_SESSION_SECRET, etc.) are left alone.
#
# Usage:
#   sudo bash scripts/provision.sh                       # interactive (asks for client + passphrase)
#   sudo chat provision                                  # via the operator CLI
#   sudo chat provision --client=elcano --no-restart
#
# Env overrides (skip prompts):
#   CHAT_PROVISION_CLIENT=elcano
#   CHAT_PROVISION_PASSPHRASE=...           # or use a pass file at /etc/elcano/passphrase
#
# Re-runnable. Decrypt → merge → restart is the whole loop. Use this after
# rotating a shared key (re-encrypt the bundle, commit, `git pull && chat
# provision` on each box).

set -euo pipefail

# ── re-open /dev/tty when piped ──────────────────────────────────────────
if [[ ! -t 0 && -t 1 ]]; then exec </dev/tty; fi

APP_DIR="${APP_DIR:-/opt/chat}"
APP_USER="${APP_USER:-chat}"
ENV_FILE="$APP_DIR/.env.local"
PASSPHRASE_FILE="${ELCANO_PASSPHRASE_FILE:-/etc/elcano/passphrase}"

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CLIENTS_DIR="$SRC_DIR/provision/clients"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }
ask()  { printf '%s?%s %s ' "$c_cyan" "$c_reset" "$*" >&2; }

# ── arg parsing ──────────────────────────────────────────────────────────
CLIENT="${CHAT_PROVISION_CLIENT:-}"
RESTART=1
NON_INTERACTIVE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --client=*)         CLIENT="${1#*=}" ;;
    --client)           shift; CLIENT="$1" ;;
    --no-restart)       RESTART=0 ;;
    --restart)          RESTART=1 ;;
    --non-interactive)  NON_INTERACTIVE=1 ;;
    -h|--help)
      sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) die "unknown flag: $1" ;;
  esac
  shift
done

[[ $EUID -eq 0 ]] || die "run as root: sudo bash scripts/provision.sh"
[[ -d "$CLIENTS_DIR" ]] || die "no $CLIENTS_DIR — is this a checkout? ($SRC_DIR)"

# Pre-create target dirs in case provision is run before bootstrap.
install -d -m 0755 "$APP_DIR" 2>/dev/null || true

# ── pick a client ────────────────────────────────────────────────────────
mapfile -t available < <(find "$CLIENTS_DIR" -maxdepth 1 -name '*.env.enc' -printf '%f\n' 2>/dev/null \
                          | sed 's/\.env\.enc$//' | sort)
[[ ${#available[@]} -gt 0 ]] || die "no encrypted client files in $CLIENTS_DIR (looked for *.env.enc)"

if [[ -z "$CLIENT" ]]; then
  if [[ "$NON_INTERACTIVE" == "1" ]]; then
    [[ ${#available[@]} -eq 1 ]] || die "non-interactive + multiple clients available: pass --client=NAME"
    CLIENT="${available[0]}"
  else
    say
    say "${c_bold}Available clients:${c_reset}"
    for c in "${available[@]}"; do say "  • $c"; done
    default="elcano"
    [[ " ${available[*]} " == *" elcano "* ]] || default="${available[0]}"
    ask "Which client? ${c_dim}[${default}]${c_reset}:"
    read -r CLIENT
    CLIENT="${CLIENT:-$default}"
  fi
fi

ENC_FILE="$CLIENTS_DIR/${CLIENT}.env.enc"
[[ -f "$ENC_FILE" ]] || die "no encrypted bundle for client '$CLIENT' at $ENC_FILE"

# ── locate the passphrase ────────────────────────────────────────────────
# Priority: $CHAT_PROVISION_PASSPHRASE → $PASSPHRASE_FILE → interactive prompt.
PASSPHRASE=""
PASSPHRASE_SOURCE=""
if [[ -n "${CHAT_PROVISION_PASSPHRASE:-}" ]]; then
  PASSPHRASE="$CHAT_PROVISION_PASSPHRASE"
  PASSPHRASE_SOURCE="env"
elif [[ -f "$PASSPHRASE_FILE" ]]; then
  PASSPHRASE="$(<"$PASSPHRASE_FILE")"
  PASSPHRASE_SOURCE="$PASSPHRASE_FILE"
elif [[ "$NON_INTERACTIVE" == "1" ]]; then
  die "non-interactive + no passphrase: set CHAT_PROVISION_PASSPHRASE or write one to $PASSPHRASE_FILE"
else
  ask "Passphrase for ${CLIENT}.env.enc:"
  read -rs PASSPHRASE
  echo >&2
  PASSPHRASE_SOURCE="prompt"
  if [[ -z "$PASSPHRASE" ]]; then die "passphrase is required"; fi

  # Offer to save for next time. /etc/elcano/passphrase is root-only (0600).
  ask "Save passphrase to ${PASSPHRASE_FILE} for future runs? ${c_dim}(y/N)${c_reset}"
  read -r save_ans
  if [[ "${save_ans,,}" == "y" || "${save_ans,,}" == "yes" ]]; then
    install -d -m 0700 "$(dirname "$PASSPHRASE_FILE")"
    umask 077
    printf '%s' "$PASSPHRASE" > "$PASSPHRASE_FILE"
    chmod 0600 "$PASSPHRASE_FILE"
    ok "saved → $PASSPHRASE_FILE (mode 0600)"
  fi
fi

# ── decrypt ──────────────────────────────────────────────────────────────
step "Decrypting ${CLIENT}.env.enc (passphrase from ${PASSPHRASE_SOURCE})"
TMP_PLAIN="$(mktemp)"
trap 'rm -f "$TMP_PLAIN"' EXIT

if ! openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 \
      -in "$ENC_FILE" -out "$TMP_PLAIN" -pass stdin <<<"$PASSPHRASE" 2>/dev/null; then
  die "decryption failed — wrong passphrase?"
fi
ok "decrypted $(wc -l <"$TMP_PLAIN") line(s) into a temp file"

# ── merge into $ENV_FILE ─────────────────────────────────────────────────
# Strategy: keep all bundle-managed keys inside a single marked block.
# On each run we strip the old block and append a fresh one. Lines
# outside the markers are never touched, so any operator overrides in
# .env.local (above or below the block) are preserved.
#
# Practical caveat: if an operator manually adds a key that's also in
# the bundle, they'll have two lines for that key. dotenv loaders
# typically use last-write-wins, and the managed block lives at the
# bottom — so the bundle's value usually applies. Operators who want to
# override a bundle value should set the override via systemd's
# EnvironmentFile= (process env beats both files).
step "Merging into $ENV_FILE"
if [[ ! -f "$ENV_FILE" ]]; then
  install -d -m 0755 "$APP_DIR"
  : > "$ENV_FILE"
  if id -u "$APP_USER" >/dev/null 2>&1; then
    chown "$APP_USER:$APP_USER" "$ENV_FILE"
  fi
  chmod 0640 "$ENV_FILE"
  info "created empty $ENV_FILE — bootstrap.sh will fill in the rest"
fi

MARKER_BEGIN="# ── managed by chat provision (${CLIENT}) — do not edit, run \`chat provision\` to refresh ──"
MARKER_END="# ── end chat provision (${CLIENT}) ──"

# Drop any previous managed block for this client (and the trailing
# blank line we'd have inserted before it, to keep re-runs from
# accumulating whitespace).
if grep -qF "$MARKER_BEGIN" "$ENV_FILE"; then
  awk -v b="$MARKER_BEGIN" -v e="$MARKER_END" '
    BEGIN { blank=0 }
    $0 == b { skip=1; blank=0; next }
    skip && $0 == e { skip=0; next }
    !skip {
      if ($0 == "") { blank++; next }
      while (blank > 0) { print ""; blank-- }
      print
    }
  ' "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
  # Trim trailing blanks.
  sed -i -e :a -e '/^$/{$d;N;ba' -e '}' "$ENV_FILE"
fi

# Build the new managed block. Count what's new vs. duplicating an
# already-present key (for the operator-facing summary at the end).
new_count=0
collide_count=0
declare -a managed_block collisions
managed_block=("$MARKER_BEGIN")

while IFS= read -r line || [[ -n "$line" ]]; do
  [[ "$line" =~ ^[[:space:]]*# || -z "${line//[[:space:]]/}" ]] && continue
  [[ "$line" == *=* ]] || continue
  key="${line%%=*}"
  key="${key//[[:space:]]/}"
  [[ -n "$key" ]] || continue
  managed_block+=("$line")
  if grep -qE "^[[:space:]]*${key}=" "$ENV_FILE"; then
    collide_count=$((collide_count + 1))
    collisions+=("$key")
  else
    new_count=$((new_count + 1))
  fi
done <"$TMP_PLAIN"
managed_block+=("$MARKER_END")

{
  printf '\n'
  for l in "${managed_block[@]}"; do printf '%s\n' "$l"; done
} >> "$ENV_FILE"

chmod 0640 "$ENV_FILE"
if id -u "$APP_USER" >/dev/null 2>&1; then
  chown "$APP_USER:$APP_USER" "$ENV_FILE" 2>/dev/null || true
fi

ok "merged: ${new_count} key(s) provisioned"
if [[ $collide_count -gt 0 ]]; then
  warn "${collide_count} bundle key(s) also set outside the managed block — bundle value will win at runtime if dotenv uses last-write-wins:"
  for k in "${collisions[@]}"; do warn "    • $k"; done
  warn "  to override a bundle value cleanly, set it via systemd's EnvironmentFile= (process env beats both files)"
fi

# ── restart ──────────────────────────────────────────────────────────────
if [[ "$RESTART" == "1" ]]; then
  if systemctl list-unit-files chat-server.service >/dev/null 2>&1; then
    step "Restarting chat services"
    systemctl restart chat-server.service chat-web.service \
      && ok "chat-server + chat-web restarted" \
      || warn "restart failed — check: journalctl -u chat-server -u chat-web -n 50"
  else
    info "chat services not installed yet — skipping restart"
  fi
else
  info "skipping restart (--no-restart). Apply with: sudo systemctl restart chat-server chat-web"
fi

say
ok "provision complete (client=${CLIENT})"
