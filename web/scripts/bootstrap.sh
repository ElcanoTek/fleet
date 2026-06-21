#!/usr/bin/env bash
# scripts/bootstrap.sh — interactive one-shot installer for Elcano Chat.
#
# Drops the deploy complexity to roughly three answers:
#   1. Your OpenRouter key
#   2. Who logs in (emails)
#   3. What DNS + whether we auto-provision TLS
#
# Everything else (secrets, hashes, systemd, firewalld) is generated or
# handled for you. Safe to re-run — it picks up where it left off.
#
# Usage:
#   # from a checkout
#   sudo bash scripts/bootstrap.sh
#
#   # or, in the future, "curl | bash"
#   curl -fsSL https://elcano.example.com/install | sudo bash
#
# Targets Fedora 39+ / RHEL 9+ / AlmaLinux 9+.
# Lifted UX patterns from opencode (colored log), rustup (preview menu,
# /dev/tty reopen), bun (TTY-gated colors). See docs/DEPLOY.md for the
# under-the-hood story.

set -euo pipefail

# ── preamble: re-open /dev/tty so `curl | sudo bash` still prompts ──────
# If stdin isn't a terminal (piped), point it at /dev/tty so read works.
# In DRY_RUN mode we consume an answer script from stdin — keep it as is.
if [[ "${CHAT_BOOTSTRAP_DRY_RUN:-0}" != "1" && ! -t 0 ]]; then
  if [[ -t 1 ]]; then
    exec </dev/tty
  else
    echo "bootstrap.sh needs an interactive terminal. Re-run locally:" >&2
    echo "  sudo bash scripts/bootstrap.sh" >&2
    exit 1
  fi
fi

# ── config ───────────────────────────────────────────────────────────────
APP_DIR="${APP_DIR:-/opt/chat}"
APP_USER="${APP_USER:-chat}"
CLI_PATH="/usr/local/bin/chat"

# CHAT_BOOTSTRAP_DRY_RUN=1 exercises the dnf + build + user-add path
# inside a container without touching systemd or firewalld. Used by
# scripts/dry-run-bootstrap.sh (podman-backed smoke test) so we can
# catch real-world install bugs before a production box sees the script.
DRY_RUN="${CHAT_BOOTSTRAP_DRY_RUN:-0}"

# ── colors (TTY-gated, a la bun) ────────────────────────────────────────
if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m'
  c_dim=$'\033[2m'
  c_red=$'\033[0;31m'
  c_green=$'\033[0;32m'
  c_yellow=$'\033[0;33m'
  c_blue=$'\033[0;34m'
  c_cyan=$'\033[0;36m'
  c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_blue='' c_cyan='' c_bold=''
fi

say()     { printf '%s\n' "$*"; }
info()    { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
step()    { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()      { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn()    { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()     { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }
ask()     { printf '%s?%s %s ' "$c_cyan" "$c_reset" "$*" >&2; }

# ── helpers ──────────────────────────────────────────────────────────────
#
# The prompt/prompt_secret/confirm helpers each accept an optional env
# var name as the first argument. If that env var is set AND non-empty
# in the environment, we use it and skip the interactive prompt. This
# lets CI/agents/Ansible/kickstart pre-answer every question via env and
# run the installer non-interactively, while a human at a terminal still
# gets the nice colored prompts.
#
# NON_INTERACTIVE=1 turns any unanswered prompt into a hard error — so
# scripted callers discover missing env vars fast instead of hanging.

NON_INTERACTIVE="${CHAT_BOOTSTRAP_NON_INTERACTIVE:-0}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found (the dnf step should have installed it)"
}

# prompt ENV_VAR "label" "default" → echoes answer
prompt() {
  local envvar="$1" label="$2" default="${3:-}" answer=""
  if [[ -n "${!envvar:-}" ]]; then
    printf '%s' "${!envvar}"
    return
  fi
  if [[ "$NON_INTERACTIVE" == "1" ]]; then
    if [[ -n "$default" ]]; then
      printf '%s' "$default"
      return
    fi
    die "non-interactive mode + missing answer: set ${envvar}"
  fi
  if [[ -n "$default" ]]; then
    ask "${label} ${c_dim}[${default}]${c_reset}:"
  else
    ask "${label}:"
  fi
  read -r answer
  [[ -z "$answer" ]] && answer="$default"
  printf '%s' "$answer"
}

# prompt_secret ENV_VAR "label" → echoes answer
prompt_secret() {
  local envvar="$1" label="$2" answer=""
  if [[ -n "${!envvar:-}" ]]; then
    printf '%s' "${!envvar}"
    return
  fi
  if [[ "$NON_INTERACTIVE" == "1" ]]; then
    die "non-interactive mode + missing secret: set ${envvar}"
  fi
  ask "${label}:"
  read -r answer
  echo >&2
  printf '%s' "$answer"
}

# confirm ENV_VAR "question" [default y|n] → returns 0 for yes, 1 for no
confirm() {
  local envvar="$1" q="$2" default="${3:-y}" answer=""
  if [[ -n "${!envvar:-}" ]]; then
    answer="${!envvar}"
  elif [[ "$NON_INTERACTIVE" == "1" ]]; then
    answer="$default"
  else
    local hint="y/N"; [[ "$default" == "y" ]] && hint="Y/n"
    ask "${q} ${c_dim}(${hint})${c_reset}"
    read -r answer
    answer="${answer:-$default}"
  fi
  [[ "${answer,,}" == "y" || "${answer,,}" == "yes" || "${answer,,}" == "1" || "${answer,,}" == "true" ]]
}

genbase64() { openssl rand -base64 "$1" | tr -d '=\n' | tr '/+' '_-'; }
genhex()    { openssl rand -hex "$1"; }

# ── banner ──────────────────────────────────────────────────────────────
clear || true
cat <<EOF
${c_bold}Elcano Chat — interactive install${c_reset}
${c_dim}Fedora / RHEL 9+  •  systemd  •  PostgreSQL  •  optional Caddy${c_reset}

This will:
  • install system deps (git, go, python3, nodejs, ripgrep, postgresql, caddy?)
  • initialize a local Postgres cluster and create the chat role + database
  • create a '${APP_USER}' system user + ${APP_DIR}
  • build chat-server and the Next.js frontend
  • generate session + shared-token secrets
  • seed your .env.local
  • provision the initial user(s)
  • install systemd units and (optionally) Caddy with automatic TLS

Safe to re-run: existing .env.local and data/ are preserved.

EOF

# ── preflight ────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || die "run as root: sudo bash scripts/bootstrap.sh"

if [[ ! -f /etc/fedora-release && ! -f /etc/redhat-release ]]; then
  warn "this installer targets Fedora/RHEL. Your OS isn't one — the dnf step will fail."
  confirm CHAT_BOOTSTRAP_CONTINUE_ON_UNSUPPORTED_OS "Continue anyway?" n || exit 1
fi

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
[[ -d "$SRC_DIR/server" ]] || die "not running from a repo checkout (no server/). Clone first and re-run."

# ── step 1: system packages ──────────────────────────────────────────────
step "1/8  Installing system dependencies via dnf"

# python3-pip is intentionally omitted. We install Python deps via uv
# (Astral's fast resolver — same tool cutlass uses). uv is now in the
# Fedora repos (≥ F40), so we just dnf it in. That sidesteps the
# "resolution-too-deep" wall pip 24+ hits with the transitive graph of
# aioboto3 + fastmcp on Python 3.14.
PKGS=(git curl jq golang python3 python3-devel gcc ripgrep postgresql postgresql-server postgresql-contrib openssl uv bind-utils podman)
dnf install -y "${PKGS[@]}" >/dev/null
need_cmd uv
ok "installed: ${PKGS[*]} (uv $(uv --version 2>&1 | awk '{print $2}'))"

# Node 20+ — use the version in Fedora if new enough, else NodeSource.
if ! command -v node >/dev/null 2>&1 || [[ "$(node -v | cut -dv -f2 | cut -d. -f1)" -lt 20 ]]; then
  info "installing Node 20 via NodeSource"
  curl -fsSL https://rpm.nodesource.com/setup_20.x | bash - >/dev/null
  dnf install -y nodejs >/dev/null
fi
need_cmd node
need_cmd npm
need_cmd go
need_cmd python3
ok "node $(node -v), go $(go version | awk '{print $3}'), python3 $(python3 -V | awk '{print $2}')"

# ── step 2: Postgres cluster + chat database ────────────────────────────
step "2/8  Preparing PostgreSQL"

# Initialize the data directory on first install. `postgresql-setup`
# is the Fedora-blessed wrapper, but it relies on systemd-tmpfiles and
# other runtime bits that aren't present in the DRY_RUN container. In
# that case we go direct via `initdb` as the postgres user, which is
# what postgresql-setup does under the hood minus the tmpfiles plumbing.
# Idempotent in both paths — PG_VERSION short-circuits re-runs.
if [[ ! -f /var/lib/pgsql/data/PG_VERSION ]]; then
  install -d -o postgres -g postgres -m 0700 /var/lib/pgsql/data
  if [[ "$DRY_RUN" == "1" ]]; then
    # `peer` for the Unix socket lets `runuser -u postgres -- psql`
    # authenticate without a password (it matches the calling Unix
    # user against pg_user). `scram-sha-256` for host connections is
    # what Fedora's postgresql-setup picks too — keeps the chat role
    # password-protected.
    runuser -u postgres -- /usr/bin/initdb -D /var/lib/pgsql/data \
      --auth-local=peer --auth-host=scram-sha-256 \
      || die "initdb failed (see output above)"
  else
    /usr/bin/postgresql-setup --initdb >/dev/null 2>&1 \
      || die "postgresql-setup --initdb failed — check /var/lib/pgsql/initdb_postgresql.log"
  fi
  ok "initialized Postgres cluster at /var/lib/pgsql/data"
fi

# pg_hba.conf on Fedora defaults to `ident` for host connections on
# 127.0.0.1, which rejects our TCP-based chat role. The file is
# evaluated top-down first-match-wins, so appending a scram-sha-256
# rule at the bottom does nothing — the default `host all all` ident
# line matches first and fails. Rewrite the loopback rules to
# scram-sha-256 in place (matches what `initdb --auth-host=scram-sha-256`
# would have produced; postgresql-setup --initdb doesn't expose that
# flag). Idempotent: sed only matches lines still set to `ident`.
PG_HBA=/var/lib/pgsql/data/pg_hba.conf
sed -i -E \
  's/^(host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1\/32|::1\/128)[[:space:]]+)ident([[:space:]]*)$/\1scram-sha-256\3/' \
  "$PG_HBA"
ok "pg_hba.conf: loopback host rules set to scram-sha-256"

if [[ "$DRY_RUN" == "1" ]]; then
  # In a container without systemd, start Postgres via pg_ctl so the
  # rest of the bootstrap (role creation, chat-admin user add) still
  # exercises real DB calls. runuser avoids a sudo dependency that
  # minimal Fedora container images don't ship by default.
  #
  # logging_collector=off keeps startup output on stderr where pg_ctl
  # -w can see it. PG 18 on Fedora 43 enables logging_collector by
  # default, redirecting into a log/ subdir that pg_ctl -w can't
  # observe — so the wait silently times out.
  info "DRY_RUN: starting Postgres via pg_ctl (no systemd in container)"
  # /var/run/postgresql is normally created by systemd-tmpfiles for
  # the unix socket lock. Containers without systemd skip that step;
  # create it manually so pg can claim the socket.
  install -d -o postgres -g postgres -m 0755 /var/run/postgresql
  # PG 18's default config has logging_collector = on uncommented.
  # Appending unconditionally exploits postgres's last-write-wins
  # config semantics — even an earlier-set 'on' is shadowed by our
  # 'off' here. The bootstrap-managed marker keeps re-runs idempotent
  # without growing the file forever.
  if ! grep -q '^# managed by chat bootstrap dry-run' /var/lib/pgsql/data/postgresql.conf; then
    cat >> /var/lib/pgsql/data/postgresql.conf <<'EOF'

# managed by chat bootstrap dry-run
logging_collector = off
EOF
  fi
  if ! runuser -u postgres -- /usr/bin/pg_ctl -D /var/lib/pgsql/data \
       -l /tmp/pg-dryrun.log -w start; then
    warn "pg_ctl start log:"
    [[ -f /tmp/pg-dryrun.log ]] && tail -n 50 /tmp/pg-dryrun.log >&2 || true
    die "pg_ctl start failed"
  fi
else
  systemctl enable --now postgresql >/dev/null 2>&1 \
    || die "failed to start postgresql.service — check: journalctl -u postgresql -n 50"
  # Reload config in case pg_hba.conf changed on an existing cluster.
  systemctl reload postgresql >/dev/null 2>&1 || true
fi

# Generate or reuse the chat role password. We stash it in
# $ENV_FILE below as part of DATABASE_URL; if that file already exists
# from a prior install we source the value so re-runs don't rotate the
# password (which would break the existing cluster until we also
# ALTER ROLE'd it).
ENV_FILE="$APP_DIR/.env.local"
DB_PASS=""
if [[ -f "$ENV_FILE" ]]; then
  existing_url="$(awk -F= '/^DATABASE_URL=/{gsub(/^"|"$/,"",$2); print $2}' "$ENV_FILE" 2>/dev/null || true)"
  # Extract password from postgres://chat:PASSWORD@... — greedy match
  # to the last @ so passwords containing '@' would still work (we
  # don't generate any, but belt-and-suspenders).
  if [[ "$existing_url" =~ postgres://chat:([^@]+)@ ]]; then
    DB_PASS="${BASH_REMATCH[1]}"
  fi
fi
[[ -z "$DB_PASS" ]] && DB_PASS="$(genhex 16)"

# Create the role + database idempotently. \gexec runs the SELECT's
# resulting string as SQL — a common pattern for "CREATE ... IF NOT
# EXISTS" on objects where Postgres doesn't support IF NOT EXISTS.
# runuser over sudo: minimal Fedora container images (dry-run) don't
# ship sudo; runuser is provided by util-linux which is always present.
runuser -u postgres -- psql -v ON_ERROR_STOP=1 <<EOF >/dev/null
SELECT 'CREATE ROLE chat LOGIN PASSWORD ''${DB_PASS}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='chat')\gexec
ALTER ROLE chat WITH PASSWORD '${DB_PASS}';
SELECT 'CREATE DATABASE chat OWNER chat'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='chat')\gexec
GRANT ALL PRIVILEGES ON DATABASE chat TO chat;
EOF
ok "Postgres role 'chat' + database 'chat' are ready"

DATABASE_URL="postgres://chat:${DB_PASS}@127.0.0.1:5432/chat?sslmode=disable"

# ── step 3: user + directory ─────────────────────────────────────────────
step "3/8  Preparing ${APP_DIR} + '${APP_USER}' system user"
if ! id -u "$APP_USER" >/dev/null 2>&1; then
  useradd --system --shell /usr/sbin/nologin --home-dir "$APP_DIR" --create-home "$APP_USER"
fi
mkdir -p "$APP_DIR/data" "$APP_DIR/data/audit" "$APP_DIR/workspace"
# Rootless podman storage lives under HOME=/opt/chat at
# /opt/chat/.local/share/containers — pre-create it so the systemd unit's
# ReadWritePaths includes a directory that already exists.
mkdir -p "$APP_DIR/.local/share/containers" "$APP_DIR/.config/containers"
# NOTE: earlier we symlinked server/{personas,protocols,system_prompts}
# up to $APP_DIR so the LLM's bare-path reads resolved from cwd.
# That's no longer necessary — each per-conversation workspace
# (workspace/<convID>/) now gets its own scoped symlinks at creation
# time. Worse, the top-level symlinks tricked resolveBaseDir into
# returning /opt/chat instead of /opt/chat/server, which broke MCP
# subprocess spawning. Clean them up if a prior install created them.
for d in personas protocols system_prompts; do
  if [[ -L "$APP_DIR/$d" ]]; then
    rm -f "$APP_DIR/$d"
  fi
done
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

# Rootless podman prerequisites:
#   - subuid/subgid entries (useradd --system doesn't allocate them)
#   - lingering enabled so the per-user systemd manager + cgroup
#     delegation is set up at boot, not on (non-existent) login.
# Both are idempotent.
if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
  usermod --add-subuids 100000-165535 "$APP_USER"
fi
if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
  usermod --add-subgids 100000-165535 "$APP_USER"
fi
if [[ "$DRY_RUN" != "1" ]]; then
  loginctl enable-linger "$APP_USER" >/dev/null 2>&1 || true
fi

ok "user '${APP_USER}' ready, ${APP_DIR} owned"

# ── step 3: prompt for secrets + users + hostname ───────────────────────
step "4/8  Configuring the instance"

# Persist answers so re-runs are friction-free. ENV_FILE is set above
# in the Postgres step so DATABASE_URL can be sourced idempotently.
if [[ -f "$ENV_FILE" ]]; then
  info "found existing ${ENV_FILE} — re-using values, only asking for what's missing"
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
fi

# 3a — OpenRouter key
if [[ -z "${OPENROUTER_API_KEY:-}" || "${OPENROUTER_API_KEY:-}" == "sk-or-v1-..." ]]; then
  say
  say "  Your OpenRouter API key (grab one at ${c_cyan}https://openrouter.ai/keys${c_reset})."
  say "  It's NEVER committed — stored only in ${c_dim}${ENV_FILE}${c_reset}."
  OPENROUTER_API_KEY="$(prompt_secret CHAT_BOOTSTRAP_OPENROUTER_KEY "OpenRouter API key")"
  [[ -n "$OPENROUTER_API_KEY" ]] || die "OPENROUTER_API_KEY is required"
  # Lightweight validation: is the key format plausible + does the host
  # respond? We don't burn a real completion.
  if [[ "$OPENROUTER_API_KEY" != sk-or-* ]]; then
    warn "that doesn't look like an OpenRouter key (expected 'sk-or-...'). Continuing anyway."
  fi
  # /v1/auth/key returns 200 with a valid key, 401 otherwise — the real
  # auth-gated endpoint we can ping cheaply. (Historically we used
  # /v1/models which is public, so "verified" was misleading.)
  key_status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -H "Authorization: Bearer $OPENROUTER_API_KEY" \
        https://openrouter.ai/api/v1/auth/key 2>/dev/null || echo 000)"
  case "$key_status" in
    200)       ok "OpenRouter key verified" ;;
    401|403)   warn "OpenRouter rejected that key (status $key_status). Continuing anyway — logins will fail until fixed." ;;
    *)         warn "could not reach openrouter.ai (status $key_status). Continuing anyway — logins will fail until it works." ;;
  esac
fi

# 3b — session + shared-token secrets (always auto-generated)
APP_SESSION_SECRET="${APP_SESSION_SECRET:-$(genbase64 48)}"
CHAT_SERVER_TOKEN="${CHAT_SERVER_TOKEN:-$(genhex 32)}"

# 3c — outgoing email From address. Both email MCPs (SendGrid + Mailbux)
# send as this address unless a tool call overrides it. No interactive
# prompt: most installs want the Elcano default, and operators can edit
# .env.local (or pre-set CHAT_BOOTSTRAP_FROM_EMAIL) to change it.
DEFAULT_FROM_EMAIL="${CHAT_BOOTSTRAP_FROM_EMAIL:-victoria@elcanotek.com}"
SENDGRID_FROM_EMAIL="${SENDGRID_FROM_EMAIL:-$DEFAULT_FROM_EMAIL}"
MAILBUX_FROM_EMAIL="${MAILBUX_FROM_EMAIL:-$DEFAULT_FROM_EMAIL}"

# 3d — hostname + TLS plan
say
say "  How will people reach this box in the browser?"
say "    • ${c_dim}localhost${c_reset}  — dev laptop, no TLS"
say "    • ${c_dim}chat.example.com${c_reset} — real DNS, we'll offer auto-TLS via Caddy"
HOSTNAME_ANSWER="$(prompt CHAT_BOOTSTRAP_HOSTNAME "Hostname or URL" "localhost")"

SETUP_CADDY="n"
USE_LETSENCRYPT="n"
LE_EMAIL=""
if [[ "$HOSTNAME_ANSWER" != "localhost" && "$HOSTNAME_ANSWER" != 127.* ]]; then
  # DNS pre-check: verify the hostname resolves to this box's public IP
  # BEFORE we ask ACME for a cert. A 30s ACME timeout with a vague
  # "timeout during HTTP-01 challenge" error is awful UX; catching it
  # here lets us point the operator at the DNS record they need to fix.
  if command -v dig >/dev/null 2>&1; then
    resolved_ip="$(dig +short "$HOSTNAME_ANSWER" A | tail -n1 2>/dev/null || true)"
  else
    resolved_ip="$(getent hosts "$HOSTNAME_ANSWER" 2>/dev/null | awk '{print $1}' | head -n1 || true)"
  fi
  # If ipify is unreachable (common on fresh boxes behind strict firewalls),
  # fall back to the IP of the default route interface so we can still
  # compare DNS resolution against a plausible local address.
  public_ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  if [[ -z "$public_ip" ]] && command -v ip >/dev/null 2>&1; then
    public_ip="$(ip -4 route show default 2>/dev/null | awk '{print $9}' | head -n1 || true)"
    # Some kernels output the interface name; resolve it.
    if [[ -n "$public_ip" && "$public_ip" != *.* ]]; then
      public_ip="$(ip -4 addr show "$public_ip" 2>/dev/null | awk '/inet /{print $2}' | cut -d/ -f1 | head -n1 || true)"
    fi
  fi
  if [[ -z "$public_ip" ]] && command -v hostname >/dev/null 2>&1; then
    # Last resort: resolve this host's own hostname.
    public_ip="$(getent hosts "$(hostname)" 2>/dev/null | awk '{print $1}' | head -n1 || true)"
  fi
  if [[ -n "$resolved_ip" && -n "$public_ip" && "$resolved_ip" != "$public_ip" ]]; then
    warn "DNS mismatch:"
    warn "  ${HOSTNAME_ANSWER} resolves to ${resolved_ip}"
    warn "  this box's public IP is       ${public_ip}"
    warn "  Let's Encrypt will fail the HTTP-01 challenge until the A"
    warn "  record is updated. You can still continue and pick the"
    warn "  'tls internal' (self-signed) option below."
  elif [[ -z "$resolved_ip" ]]; then
    warn "${HOSTNAME_ANSWER} doesn't resolve to anything yet."
    if [[ -n "$public_ip" ]]; then
      warn "  Add an A record pointing at ${public_ip} before Caddy"
      warn "  can fetch a cert, or use 'tls internal' for a self-signed cert."
    else
      warn "  Could not determine this box's IP either. Add an A record"
      warn "  pointing at this box, or use 'tls internal' for a self-signed cert."
    fi
  elif [[ -n "$resolved_ip" && -n "$public_ip" && "$resolved_ip" == "$public_ip" ]]; then
    ok "DNS resolves correctly (${resolved_ip})"
  fi

  if confirm CHAT_BOOTSTRAP_SETUP_CADDY "Set up Caddy + auto-TLS for ${HOSTNAME_ANSWER}?" y; then
    SETUP_CADDY="y"
    say
    say "  Caddy will obtain a cert from Let's Encrypt by default. If this"
    say "  host ${c_bold}can't${c_reset} be reached from the public internet on ports 80/443,"
    say "  pick the internal option (self-signed cert, browser warning)."
    if confirm CHAT_BOOTSTRAP_USE_LETSENCRYPT "Use Let's Encrypt (requires public reachability)?" y; then
      USE_LETSENCRYPT="y"
      # Optional — LE uses this to warn you if cert renewal ever breaks.
      # Empty is fine; Caddy just doesn't register a contact.
      LE_EMAIL="$(prompt CHAT_BOOTSTRAP_LE_EMAIL "LE contact email for renewal warnings (blank to skip)" "")"
    fi
  fi
fi

# 3e — users
step "5/8  Provisioning users"
say
say "  Every person who logs in gets their own password — no shared secret."
say "  Enter their emails; we'll generate a password for each and print it."
say "  (You can add more later: ${c_bold}chat user add alice@example.com${c_reset})"
say
USER_EMAILS=()
if [[ -n "${CHAT_BOOTSTRAP_USERS:-}" ]]; then
  # Comma-, space-, or newline-separated list. All three play nicely with
  # kickstart/Ansible vars.
  IFS=$',\n '
  for e in ${CHAT_BOOTSTRAP_USERS}; do
    e="${e// /}"
    [[ -n "$e" ]] && USER_EMAILS+=("$e")
  done
  unset IFS
  info "users from CHAT_BOOTSTRAP_USERS: ${USER_EMAILS[*]}"
elif [[ "$NON_INTERACTIVE" == "1" ]]; then
  warn "NON_INTERACTIVE: no users provided. Add some later with 'chat user add'."
else
  while :; do
    if [[ ${#USER_EMAILS[@]} -eq 0 ]]; then
      say "  ${c_dim}e.g. you@example.com${c_reset}"
      ask "User email (blank to stop)"
    else
      ask "User email (blank to stop)"
    fi
    read -r e
    [[ -z "$e" ]] && break
    USER_EMAILS+=("$e")
  done
fi

# ── step 5: write .env.local ────────────────────────────────────────────
step "6/8  Writing ${ENV_FILE}"

umask 077
cat > "$ENV_FILE" <<EOF
# Auto-generated by bootstrap.sh on $(date -Iseconds)
# Override anything via the process env (systemd EnvironmentFile=).

# ── Session / auth (Next.js) ─────────────────────────────────────
APP_SESSION_SECRET="$APP_SESSION_SECRET"

# ── Next.js ↔ chat-server link ──────────────────────────────────
CHAT_SERVER_URL="http://127.0.0.1:8080"
CHAT_SERVER_TOKEN="$CHAT_SERVER_TOKEN"

# ── Postgres ────────────────────────────────────────────────────
# chat-server + chat-admin both read this. The cluster was
# initialized by bootstrap.sh; the chat role has a random password
# scoped to localhost.
DATABASE_URL="$DATABASE_URL"

# ── LLM provider ────────────────────────────────────────────────
OPENROUTER_API_KEY="$OPENROUTER_API_KEY"

# ── Outgoing email ──────────────────────────────────────────────
# Default From address for the SendGrid and Mailbux MCP servers.
# Sending stays disabled until SENDGRID_API_KEY or MAILBUX_USERNAME/
# MAILBUX_PASSWORD are configured (e.g. via provision.sh).
SENDGRID_FROM_EMAIL="$SENDGRID_FROM_EMAIL"
MAILBUX_FROM_EMAIL="$MAILBUX_FROM_EMAIL"

# ── Deploy info ─────────────────────────────────────────────────
CHAT_PUBLIC_HOSTNAME="$HOSTNAME_ANSWER"
EOF
chown "$APP_USER:$APP_USER" "$ENV_FILE"
chmod 0640 "$ENV_FILE"
ok "env seeded"

# ── step 6b: optional Elcano-controlled shared-secrets bundle ───────────
# Run BEFORE the build/start step so services see the merged values on
# their first boot. provision.sh is also runnable standalone post-install
# via `sudo chat provision` — this prompt is just the kickoff convenience.
PROVISION_DIR="$SRC_DIR/provision/clients"
if compgen -G "$PROVISION_DIR/*.env.enc" >/dev/null 2>&1; then
  step "Elcano client secrets (optional)"
  if confirm CHAT_BOOTSTRAP_PROVISION "Provision shared client secrets now?" y; then
    # Auto-discover available clients. If only one, skip the picker.
    mapfile -t _clients < <(find "$PROVISION_DIR" -maxdepth 1 -name '*.env.enc' -printf '%f\n' \
                              | sed 's/\.env\.enc$//' | sort)
    if [[ ${#_clients[@]} -eq 1 ]]; then
      _client="${_clients[0]}"
    else
      say "  Available clients:"
      for _c in "${_clients[@]}"; do say "    • $_c"; done
      _default="elcano"
      [[ " ${_clients[*]} " == *" elcano "* ]] || _default="${_clients[0]}"
      _client="$(prompt CHAT_BOOTSTRAP_PROVISION_CLIENT "Which client?" "$_default")"
    fi
    # --no-restart: services aren't running yet; the build/start step
    # right after this picks up the merged values on first boot.
    if bash "$SRC_DIR/scripts/provision.sh" --client="$_client" --no-restart; then
      ok "client secrets provisioned ($_client)"
    else
      warn "provision failed — continuing install. Retry later with: sudo chat provision"
    fi
  else
    info "skipping provision — run 'sudo chat provision' later if you change your mind"
  fi
fi

# ── step 6: sync source, build, install units ───────────────────────────
step "7/8  Building chat-server + Next.js bundle (this is the slow part)"

# Pull sources into /opt/chat, preserving user-editable + stateful bits.
# /workspace is a runtime scratch dir declared in chat-server.service's
# ReadWritePaths=; the source tree doesn't have one, so without the
# exclude, --delete wipes the dir we just mkdir'd above and the next
# `systemctl start chat-server` fails at the NAMESPACE step.
rsync -a --delete \
  --exclude='/.git' \
  --exclude='/node_modules' \
  --exclude='/.next' \
  --exclude='/bin' \
  --exclude='/data' \
  --exclude='/workspace' \
  --exclude='/.env.local' \
  "$SRC_DIR/" "$APP_DIR/"
# Belt-and-suspenders: re-assert the runtime dirs in case an older
# rsync invocation (or a future refactor) drops the exclude. Cheap,
# idempotent, and keeps bootstrap + update symmetric.
install -d -o "$APP_USER" -g "$APP_USER" "$APP_DIR/workspace" "$APP_DIR/data/audit"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

# uv pip install --reinstall: writes into system site-packages, with
# proper transitive resolution. We previously tried `uv pip sync` for
# its automatic-uninstall-of-dropped-deps property, but sync expects a
# fully-resolved lockfile (a la `pip-compile` output) — fed a hand-
# written constraints file like requirements.txt, sync skips transitive
# resolution and fails at import time on anything pinned by a transitive
# (e.g. jmespath, which botocore needs). install --reinstall keeps the
# resolution and is the right shape for our deploy story; the cleanup
# of dropped deps happens via the explicit uv pip uninstall block below.
#
# --break-system-packages: PEP 668 ships an "EXTERNALLY-MANAGED" marker
# on Fedora/Debian that blocks system-level pip installs by default.
# We bypass it because the service user needs these libs.
#
# --reinstall: uv's default `pip install -r requirements.txt` short-
# circuits when every listed package looks satisfied, which misses
# half-installed transitive trees on new Pythons.
uv pip install --system --no-cache --break-system-packages --reinstall \
  -r "$APP_DIR/server/requirements.txt" \
  || die "python deps install (uv) failed — chat-server MCP subprocesses will not boot"
# Drop packages that used to be host-side run_python kernel deps. With
# run_python now exclusively inside the per-turn container image (which
# has its own Python + Jupyter kernel), these are dead weight on the
# host. Idempotent — uninstall is a no-op if the package isn't there,
# so this stays safe to leave in place after every box has converged.
# Once the fleet is past the transition we can drop the block.
uv pip uninstall --system --break-system-packages \
  ipykernel ipython jupyter-client pyzmq pandas numpy >/dev/null 2>&1 || true
ok "python deps installed (uv)"
# `uv pip install --break-system-packages` on Fedora's PEP 668 python
# creates /usr/local/lib{,64}/pythonX.Y/ from scratch under sudo's 0077
# umask — which makes the tree unreadable to non-root users. Root's
# import check passes (below) but chat-server's MCP subprocesses, which
# run as the 'chat' user, hit ModuleNotFoundError at startup. Fix BOTH
# /lib and /lib64 (compiled C extensions like aiohttp's _frozenlist
# land under lib64 on Fedora; pure-Python under lib). Cheap + idempotent.
for d in /usr/local/lib/python3.* /usr/local/lib64/python3.*; do
  [[ -d "$d" ]] && chmod -R o+rX "$d"
done
# Probe one symbol from each MCP-subprocess dep so a half-installed
# transitive tree fails here loudly instead of at first chat-server
# turn. Removed packages (Jupyter kernel stack, openpyxl) are no
# longer host-side concerns — run_python lives in the container image.
python3 -c "import aioboto3, mcp.server.fastmcp, sendgrid, httpx, html5lib" \
  || die "post-install import check failed — aioboto3 / mcp / sendgrid / httpx / html5lib must be importable under $(python3 -c 'import sys;print(sys.executable)')"
sudo -u "$APP_USER" /usr/bin/python3 -c "import aioboto3, mcp.server.fastmcp, sendgrid, httpx, html5lib" \
  || die "post-install import check failed for user '$APP_USER' — check /usr/local/lib/python3.*/site-packages perms"

sudo -u "$APP_USER" -H bash -c "
  cd '$APP_DIR/server'
  GOTOOLCHAIN=auto go build -o '$APP_DIR/bin/chat-server' ./cmd/chat-server
  GOTOOLCHAIN=auto go build -o '$APP_DIR/bin/chat-admin'  ./cmd/chat-admin
"

sudo -u "$APP_USER" -H bash -c "
  export NEXT_TELEMETRY_DISABLED=1
  cd '$APP_DIR'
  npm install --no-audit --no-fund --loglevel=warn
  npm run build
"

install -m 0755 "$APP_DIR/deploy/chat-cli" "$CLI_PATH"

# ── per-turn sandbox mode (rootless podman) ─────────────────────────────
# Two modes: default / lockdown-only.
#   - default: every chat sandboxed in a per-turn rootless container;
#     users opt into Lockdown via the lock icon next to "+".
#   - lockdown-only: every chat is forcibly Lockdown.
#
# Containers are mandatory — bash and run_python execute arbitrary
# agent-emitted code, and host execution would let the agent reach
# into the chat-server process's filesystem and credentials. If
# rootless podman setup or the image pull fails here, bootstrap aborts
# rather than installing a chat-server that won't start.
#
# Switchable post-install via `chat sandbox <default|lockdown-only>`.
#
# Skipped in DRY_RUN — rootless podman doesn't nest cleanly inside the
# dry-run's own podman container, and the goal of dry-run is to exercise
# the install logic itself, not the runtime container path.
SANDBOX_IMAGE="${CHAT_SANDBOX_IMAGE:-ghcr.io/elcanotek/sandbox:latest}"
SANDBOX_MODE="default"
if [[ "$DRY_RUN" == "1" ]]; then
  info "DRY_RUN: skipping sandbox image pull (would pull $SANDBOX_IMAGE)"
else
  step "Per-turn sandbox containerization"
  say
  say "  Every chat runs inside an isolated rootless container. Users"
  say "  can opt into Lockdown (vetted-models-only) via the lock icon"
  say "  next to the '+' button. For sensitive deployments you can"
  say "  require every chat to be Lockdown."
  say
  if confirm CHAT_BOOTSTRAP_SANDBOX_LOCKDOWN_ONLY \
      "Force Lockdown mode for every chat? (sensitive deploys)" n; then
    SANDBOX_MODE="lockdown-only"
  fi
  info "pulling $SANDBOX_IMAGE as $APP_USER (≈900 MB) …"
  # cd to $APP_DIR first: sudo -u inherits the invoker's cwd, and if that's
  # a directory $APP_USER can't read (e.g. /root when bootstrap is run as
  # root), podman's child processes fail with "cannot chdir to …: Permission
  # denied / Error: setting up the process" before the pull even starts.
  pull_log="$(mktemp)"
  if ! sudo -u "$APP_USER" -H bash -c "cd '$APP_DIR' && podman pull '$SANDBOX_IMAGE'" \
       >"$pull_log" 2>&1; then
    pull_err="$(tail -n 20 "$pull_log")"
    rm -f "$pull_log"
    die "sandbox image pull failed. chat-server will not start without a working sandbox.

podman output:
$pull_err

Common causes:
  - newuidmap missing: sudo dnf install shadow-utils
  - no subuid/subgid for $APP_USER: re-run bootstrap or check /etc/subuid /etc/subgid
  - rootless storage init under $APP_DIR/.local/share/containers
After fixing, re-run bootstrap or run: sudo chat sandbox $SANDBOX_MODE"
  fi
  rm -f "$pull_log"
  if ! grep -q '^CHAT_SANDBOX_IMAGE=' "$ENV_FILE"; then
    printf '\n# ── Per-turn sandbox container ───────────────────────\nCHAT_SANDBOX_IMAGE="%s"\nCHAT_WORKSPACE_ROOT="%s/workspace"\n' \
      "$SANDBOX_IMAGE" "$APP_DIR" >> "$ENV_FILE"
  fi
  if [[ "$SANDBOX_MODE" == "lockdown-only" ]] && ! grep -q '^CHAT_LOCKDOWN_ONLY=' "$ENV_FILE"; then
    printf 'CHAT_LOCKDOWN_ONLY="true"\n' >> "$ENV_FILE"
  fi
  ok "sandbox: $SANDBOX_MODE ($SANDBOX_IMAGE)"
fi

if [[ "$DRY_RUN" == "1" ]]; then
  info "DRY_RUN: skipping systemd install + service start"
  # Spawn chat-server directly so chat-admin user add can find the DB.
  sudo -u "$APP_USER" --preserve-env=HOME env \
    CHAT_DATA_DIR="$APP_DIR/data" \
    CHAT_MOCK_MODE=1 \
    OPENROUTER_API_KEY="$OPENROUTER_API_KEY" \
    CHAT_SERVER_TOKEN="$CHAT_SERVER_TOKEN" \
    "$APP_DIR/bin/chat-server" >/tmp/chat-server-dry.log 2>&1 &
  DRY_SERVER_PID=$!
  trap 'kill $DRY_SERVER_PID 2>/dev/null || true' EXIT
else
  install -m 0644 "$APP_DIR/deploy/chat-server.service" /etc/systemd/system/
  install -m 0644 "$APP_DIR/deploy/chat-web.service"    /etc/systemd/system/
  install -m 0644 "$APP_DIR/deploy/chat.target"         /etc/systemd/system/
  systemctl daemon-reload
  ok "systemd units + ${CLI_PATH} installed"

  systemctl enable chat.target >/dev/null 2>&1 || true
  systemctl restart chat-server.service chat-web.service
  ok "chat-server + chat-web are running"
fi

# Wait for chat-server's HTTP port instead of a DB file — Postgres is
# already running, so readiness is about the service binary having
# booted + applied migrations.
for _ in $(seq 1 20); do
  curl -fsS --max-time 1 http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 0.5
done

# ── step 7b: provision users via chat-admin ─────────────────────────────
if [[ ${#USER_EMAILS[@]} -gt 0 ]]; then
  say
  step "   Provisioning ${#USER_EMAILS[@]} user(s)"
  SUMMARY=()
  for e in "${USER_EMAILS[@]}"; do
    # chat-admin prints "password: X" — capture it. DATABASE_URL is
    # read from the env we pass through, matching chat-server.
    out=$(sudo -u "$APP_USER" DATABASE_URL="$DATABASE_URL" \
          "$APP_DIR/bin/chat-admin" user add "$e" 2>&1 || true)
    pw=$(echo "$out" | awk '/^  password:/ {print $2}')
    if [[ -n "$pw" ]]; then
      SUMMARY+=("$e|$pw")
      ok "provisioned $e"
    else
      warn "failed to provision $e:"
      echo "$out" | sed 's/^/    /'
    fi
  done
fi

# ── step 7: Caddy (optional) ────────────────────────────────────────────
step "8/8  Reverse proxy"
if [[ "$DRY_RUN" == "1" ]]; then
  info "DRY_RUN: skipping Caddy / firewalld"
elif [[ "$SETUP_CADDY" == "y" ]]; then
  info "installing Caddy"
  dnf install -y caddy >/dev/null

  # Compose the Caddyfile:
  #   1. If LE_EMAIL is set, prepend a global `{ email ... }` block so
  #      Caddy registers an ACME contact (LE uses it to warn you if
  #      renewals fail for any reason).
  #   2. Swap chat.example.com → actual hostname.
  #   3. If Let's Encrypt is off, inject `tls internal` so Caddy issues
  #      a self-signed cert from its local CA instead of talking to LE.
  tmp=$(mktemp)
  if [[ -n "$LE_EMAIL" ]]; then
    printf '{\n\temail %s\n}\n\n' "$LE_EMAIL" > "$tmp"
  fi
  sed "s/chat\.example\.com/$HOSTNAME_ANSWER/" "$APP_DIR/deploy/Caddyfile" >> "$tmp"
  if [[ "$USE_LETSENCRYPT" != "y" ]]; then
    sed -i '/^'"${HOSTNAME_ANSWER//./\\.}"' {/a\\ttls internal' "$tmp"
  fi
  install -m 0644 "$tmp" /etc/caddy/Caddyfile
  rm -f "$tmp"

  # Open 80/443 if firewalld is running.
  if systemctl is-active --quiet firewalld; then
    firewall-cmd --add-service=http --permanent >/dev/null
    firewall-cmd --add-service=https --permanent >/dev/null
    firewall-cmd --reload >/dev/null
    ok "firewalld: http + https opened"
  fi

  systemctl enable --now caddy
  ok "Caddy running — auto-renews ~30 days before expiry, no cron needed"

  # Post-start TLS verification: poll https://hostname until the cert
  # is served (up to 45s; LE issuance is usually under 15s but can be
  # slower if CAA records need checking). We don't fail the install if
  # it doesn't come up — the operator may have reasons (e.g. firewall
  # from a CDN in front).
  if [[ "$USE_LETSENCRYPT" == "y" ]]; then
    info "waiting for TLS to come up at https://${HOSTNAME_ANSWER}"
    tls_ok=0
    for _ in $(seq 1 45); do
      if curl -fsS --max-time 5 "https://${HOSTNAME_ANSWER}/login" -o /dev/null 2>/dev/null; then
        tls_ok=1
        break
      fi
      sleep 1
    done
    if [[ "$tls_ok" == "1" ]]; then
      # Show cert subject + "valid until" from openssl so the operator
      # can sanity-check what got issued.
      expiry=$(echo | openssl s_client -servername "$HOSTNAME_ANSWER" \
        -connect "${HOSTNAME_ANSWER}:443" 2>/dev/null \
        | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
      ok "TLS live — cert valid until ${expiry:-unknown}"
    else
      warn "https://${HOSTNAME_ANSWER} didn't come up in 45s."
      warn "  Check: journalctl -u caddy -n 50 --no-pager"
      warn "  Common causes: DNS not yet propagated, firewall upstream"
      warn "  of this box blocking 80/443, or LE rate limit."
    fi
  fi
else
  info "skipping Caddy — reach the app at http://${HOSTNAME_ANSWER}:3000"
fi

# ── motd ─────────────────────────────────────────────────────────────────
tee /etc/motd > /dev/null <<'MOTD'
     ___________
    /           \
   /   ~~~~~~   \
  |   ~~    ~~   |
  |   CHAT   !!  |
   \   ~~~~~~   /
    \___________/
       |     |
      _|     |_
     |         |
     |_________|

Welcome to the Chat Server.
To manage Chat → `chat --help`
MOTD

# ── step 8: the final card ──────────────────────────────────────────────
say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Elcano Chat installed%s\n' "$c_bold" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
if [[ "$SETUP_CADDY" == "y" ]]; then
  say "  URL         ${c_bold}https://${HOSTNAME_ANSWER}${c_reset}"
else
  say "  URL         ${c_bold}http://${HOSTNAME_ANSWER}:3000${c_reset}"
fi
say "  Data dir    ${APP_DIR}/data"
say "  Logs        ${c_dim}journalctl -fu chat-server -u chat-web${c_reset}"
say "  Sandbox     ${c_dim}${SANDBOX_MODE} (toggle via 'chat sandbox <mode>')${c_reset}"
say "  CLI         ${c_dim}chat user add …  •  chat restart  •  chat backup${c_reset}"
say
if [[ ${#SUMMARY[@]} -gt 0 ]]; then
  printf '%s─── share these with your users ───%s\n' "$c_yellow" "$c_reset"
  printf '  %-40s  %s\n' "EMAIL" "PASSWORD"
  for row in "${SUMMARY[@]}"; do
    IFS='|' read -r e pw <<<"$row"
    printf '  %-40s  %s\n' "$e" "$pw"
  done
  say
  say "  ${c_dim}These are shown ONLY once. Lose them = rotate via 'chat user passwd'.${c_reset}"
  say
fi
