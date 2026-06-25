#!/usr/bin/env bash
# scripts/bootstrap.sh — fleet DB + credential-store bootstrap for fleet.
#
# Merges chat's + moc's bootstrap into ONE script with a --postgres=local|external
# branch (default local). It provisions the ONE cluster, TWO databases (chat +
# sched) layout the unified `fleet` process expects, and ensures the 0600
# credential env file exists. It NEVER runs application migrations — each service
# self-migrates on first start (chat's advisory-lock runner; sched's
# golang-migrate).
#
# It is IDEMPOTENT end to end: re-runs converge on the same state (roles/dbs are
# created only when missing via \gexec; the env file is refreshed in place; the
# sandbox image is rebuilt; the client bundle checkout is updated or left as-is).
#
# Usage:
#   scripts/bootstrap.sh --postgres=local            # dnf+initdb+pg_hba+\gexec, sslmode=disable
#   scripts/bootstrap.sh --postgres=external         # validate DSNs with SELECT 1, sslmode=require
#   scripts/bootstrap.sh --postgres=local --dry-run  # print the plan, touch nothing
#   scripts/bootstrap.sh --client-config <git-url[#<sha-or-tag>]|path>   # check out / point at a client bundle
#   scripts/bootstrap.sh --enable-service            # systemctl enable --now fleet at the end
#   scripts/bootstrap.sh --enable-web [--domain fleet.example.com]  # build + serve the web tier (TLS via Caddy with --domain)
#
# End-to-end flow (every run): ensure 0600 env file → ensure the client bundle is
# in place (--client-config) → build the sandbox image from the bundle → provision
# both chat+sched roles/databases (local) or validate DSNs (external) → write the
# resolved DSNs + FLEET_CLIENT_CONFIG_DIR into the env file → optionally enable +
# start the systemd unit.
#
# Branch A (local):  install + init a local cluster, create the two owner roles
#                    and two databases idempotently via psql \gexec, sslmode=disable.
# Branch B (external): skip install; validate the provided DSNs with SELECT 1 and
#                    assume the roles/dbs are pre-provisioned (opt-in superuser
#                    create via FLEET_DB_SUPERUSER_URL), sslmode=require.
#
# Flags:
#   --postgres=local|external  provisioning mode (default local).
#   --client-config <git-url[#<sha-or-tag>]|path>
#                              a git URL (cloned to a stable location) or an
#                              existing path (pointed at directly). Sets
#                              FLEET_CLIENT_CONFIG_DIR in the env file. An
#                              optional #<sha-or-tag> pins the checkout to that
#                              ref (recorded under the state dir) so `update`
#                              advances only to it instead of tracking HEAD.
#   --enable-service           systemctl enable --now the fleet unit at the end.
#   --enable-web               build + deploy the Next.js web tier and enable
#                              fleet-web (implies --enable-service). Email+password
#                              login against the bundle's chat users.
#   --domain <fqdn>            with --enable-web: front it with Caddy + automatic
#                              TLS for <fqdn> (installs Caddy, opens 80/443).
#   --dry-run                  print the plan; touch nothing.
#
# Env knobs (all optional; sensible local defaults):
#   FLEET_ENV_FILE          credential env file to write/refresh (default: /etc/fleet/fleet.env
#                           under --enable-service — matches deploy/fleet.service —
#                           else .env.local for local/dev runs)
#   FLEET_CLIENT_CONFIG_DIR client config bundle dir (default ./config/default —
#                           the generic bundle baked into the repo). Point at a
#                           checked-out client repo (e.g. /opt/fleet/client) for a
#                           branded deploy with its own MCP catalog + prompts.
#                           --client-config is the operator-friendly way to set it.
#   FLEET_CLIENT_CONFIG_CHECKOUT  stable dir a cloned client repo lands in when
#                           --client-config is a git URL (default /opt/fleet/client,
#                           or ./.fleet-client when /opt is not writable).
#   FLEET_SERVICE_NAME      systemd unit to enable/start (default fleet)
#   CHAT_DB_NAME            chat database name (default chat)
#   CHAT_DB_USER            chat owner role  (default chat)
#   CHAT_DB_PASSWORD        chat role password (local: generated if unset)
#   SCHED_DB_NAME           sched database name (default sched)
#   SCHED_DB_USER           sched owner role (default sched)
#   SCHED_DB_PASSWORD       sched role password (local: generated if unset)
#   FLEET_CHAT_DATABASE_URL external chat DSN (external mode)
#   FLEET_SCHED_DATABASE_URL external sched DSN (external mode)
#   FLEET_DB_SUPERUSER_URL  external superuser DSN for opt-in role/db creation
#   FLEET_WEB_APP_NAME      web UI app name (--enable-web; default Fleet)
#   FLEET_ACME_EMAIL        Let's Encrypt account email for Caddy (--domain; optional)
set -euo pipefail

# Resolve this script's repo root so --enable-service can build + install the
# binary and unit files regardless of the caller's cwd (fleet-admin invokes it
# from elsewhere). The DB/env/bundle steps still use repo-relative defaults.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

POSTGRES_MODE="local"
DRY_RUN=0
CLIENT_CONFIG_ARG=""
ENABLE_SERVICE=0
ENABLE_WEB=0
WEB_DOMAIN=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --postgres=local)    POSTGRES_MODE="local" ;;
    --postgres=external) POSTGRES_MODE="external" ;;
    --postgres=*)        echo "error: --postgres must be local|external" >&2; exit 1 ;;
    --client-config)     shift; [[ $# -gt 0 ]] || { echo "error: --client-config needs a git-url|path" >&2; exit 1; }; CLIENT_CONFIG_ARG="$1" ;;
    --client-config=*)   CLIENT_CONFIG_ARG="${1#*=}" ;;
    --enable-service)    ENABLE_SERVICE=1 ;;
    --enable-web)        ENABLE_WEB=1; ENABLE_SERVICE=1 ;;  # web proxies to the backend → enable it too
    --domain)            shift; [[ $# -gt 0 ]] || { echo "error: --domain needs an FQDN" >&2; exit 1; }; WEB_DOMAIN="$1" ;;
    --domain=*)          WEB_DOMAIN="${1#*=}" ;;
    --dry-run)           DRY_RUN=1 ;;
    -h|--help)
      sed -n '2,73p' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m'; c_green=$'\033[0;32m'; c_yellow=$'\033[0;33m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
else
  c_reset=''; c_green=''; c_yellow=''; c_bold=''; c_dim=''
fi
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
die()  { printf '✗ %s\n' "$*" >&2; exit 1; }
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }

# Env file default: an explicit FLEET_ENV_FILE always wins. Otherwise, under
# --enable-service (the systemd path) default to /etc/fleet/fleet.env — the path
# deploy/fleet.service EnvironmentFiles — so the documented one-command deploy
# writes credentials where the unit actually reads them (not a stray ./.env.local
# the service can't see under ProtectHome). Plain local/dev runs keep .env.local.
if [[ -n "${FLEET_ENV_FILE:-}" ]]; then
  ENV_FILE="$FLEET_ENV_FILE"
elif [[ "$ENABLE_SERVICE" == "1" ]]; then
  ENV_FILE="/etc/fleet/fleet.env"
else
  ENV_FILE=".env.local"
fi
CLIENT_CONFIG_DIR="${FLEET_CLIENT_CONFIG_DIR:-config/default}"
SERVICE_NAME="${FLEET_SERVICE_NAME:-fleet}"
CHAT_DB_NAME="${CHAT_DB_NAME:-chat}"
CHAT_DB_USER="${CHAT_DB_USER:-chat}"
SCHED_DB_NAME="${SCHED_DB_NAME:-sched}"
SCHED_DB_USER="${SCHED_DB_USER:-sched}"

gen_pass() { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# upsert_env KEY VALUE — idempotently set KEY=VALUE in $ENV_FILE, replacing an
# existing KEY= line in place and preserving comments/unrelated lines (mirrors
# internal/creds.SetEnvKey). No-ops under --dry-run. Values are written verbatim;
# DSNs may contain '&'/'/' so we avoid sed substitution and rewrite via awk.
upsert_env() {
  local key="$1" value="$2"
  if [[ "$DRY_RUN" == "1" ]]; then
    # Never echo secret values in the plan — show the key only.
    info "[dry-run] would set ${key}=… in ${ENV_FILE}"
    return 0
  fi
  [[ -f "$ENV_FILE" ]] || install -m 0600 /dev/null "$ENV_FILE"
  local tmp
  tmp="$(mktemp "${ENV_FILE}.XXXXXX")"
  KEY="$key" VALUE="$value" awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
    {
      line = $0
      eq = index(line, "=")
      if (!done && eq > 0 && substr(line, 1, eq - 1) == k) {
        print k "=" v
        done = 1
        next
      }
      print line
    }
    END { if (!done) print k "=" v }
  ' "$ENV_FILE" > "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$ENV_FILE"
}

# ── dedicated service-user + rootless-Podman helpers (systemd/--enable-service) ──
# deploy/fleet.service runs as a FIXED system user (User=fleet), NOT DynamicUser:
# the execution sandbox is rootless Podman, which a transient DynamicUser cannot
# drive (no /etc/subuid range, no HOME). These helpers provision that user and its
# rootless-Podman prerequisites so the unit's sandbox actually runs.
SERVICE_USER="fleet"          # MUST match deploy/fleet.service (User=/Group=fleet)
SERVICE_HOME="/var/lib/fleet" # MUST match the unit's StateDirectory / HOME

setup_service_user() {
  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$SERVICE_HOME" --shell /usr/sbin/nologin --no-create-home "$SERVICE_USER"
    ok "created service user ${SERVICE_USER}"
  else
    info "service user ${SERVICE_USER} present"
  fi
  # subuid/subgid ranges — rootless Podman maps container uids/gids into these.
  grep -q "^${SERVICE_USER}:" /etc/subuid || echo "${SERVICE_USER}:100000:65536" >> /etc/subuid
  grep -q "^${SERVICE_USER}:" /etc/subgid || echo "${SERVICE_USER}:100000:65536" >> /etc/subgid
  # HOME (rootless image store lives at ~/.local/share/containers) + runtime dir.
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "$SERVICE_HOME"
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "/run/${SERVICE_USER}"
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "${SERVICE_HOME}/.config/containers"
  # cgroupfs avoids needing a systemd user D-Bus session (absent for a system
  # service / non-login user, which otherwise fails with "sd-bus call: Access
  # denied ... interactive authentication"); file events backend avoids journald perms.
  cat > "${SERVICE_HOME}/.config/containers/containers.conf" <<'CONF'
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
CONF
  # chown the WHOLE .config tree: `install -d` leaves the intermediate ~/.config
  # root-owned, and rootless Podman refuses to start if $HOME/.config is not owned
  # by the calling user ("path .../.config exists and it is not owned by the
  # current user"), which fails the build/run below.
  chown -R "$SERVICE_USER":"$SERVICE_USER" "${SERVICE_HOME}/.config"
  ok "rootless Podman configured for ${SERVICE_USER} (subuid/subgid + HOME + cgroupfs)"
}

# sandbox_manifest_tag MANIFEST — read the bundle's sandbox.tag (the on-box image
# name); mirrors build-sandbox-image.sh's default. Used to build into the service
# user's rootless store.
sandbox_manifest_tag() {
  local f="$1" t
  t="$(awk '
    /^sandbox:[[:space:]]*$/ { b=1; next }
    /^[^[:space:]]/          { b=0 }
    b && /^[[:space:]]+tag:/ { sub("^[[:space:]]+tag:[[:space:]]*",""); sub(/[[:space:]]+#.*$/,""); gsub(/^["'\'']|["'\'']$/,""); print; exit }
  ' "$f" 2>/dev/null)"
  printf '%s' "${t:-localhost/fleet-sandbox:latest}"
}

# ── web-tier helpers (--enable-web) ──────────────────────────────────────────
gen_secret() { head -c "${1:-32}" /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "${1:-32}"; }

# ensure_env_secret KEY LEN — set KEY=<random> in $ENV_FILE only when ABSENT, so
# re-runs never rotate a shared secret (rotating FLEET_SERVER_TOKEN would 403 the
# web↔backend link; rotating APP_SESSION_SECRET would log everyone out).
ensure_env_secret() {
  local key="$1" len="${2:-32}"
  [[ -f "$ENV_FILE" ]] && grep -q "^${key}=" "$ENV_FILE" && return 0
  upsert_env "$key" "$(gen_secret "$len")"
}

# deploy_web_tier — build + run the Next.js web tier, and (with --domain) front it
# with Caddy TLS. Self-contained email+password login against the bundle's chat
# users (fleet-admin chat user add ...); the external Elcano SSO stays disabled
# unless AUTH_SIGNING_PUBKEY is set. The only client-specific input is --domain.
deploy_web_tier() {
  local web_src="$REPO_ROOT/web" web_dst="/opt/fleet/web" web_env="/etc/fleet/fleet-web.env"
  local origin app_name build_id
  [[ -n "$WEB_DOMAIN" ]] && origin="https://$WEB_DOMAIN" || origin="http://localhost:3000"
  app_name="${FLEET_WEB_APP_NAME:-Fleet}"
  build_id="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo prod)"

  [[ -d "$web_src" ]] || { warn "no web/ in the checkout — skipping web tier."; return; }
  command -v npm >/dev/null 2>&1 || { warn "npm not found — skipping web tier (install nodejs)."; return; }

  # Shared secrets the web↔backend link needs; generate-if-absent then load them
  # into the already-started backend.
  step "Web tier: ensuring shared secrets in ${ENV_FILE} + reloading backend"
  ensure_env_secret FLEET_SERVER_TOKEN 32
  ensure_env_secret ADMIN_API_KEY 32
  systemctl try-restart "$SERVICE_NAME" >/dev/null 2>&1 || true

  step "Web tier: building the Next.js app (origin=${origin})"
  if ( cd "$web_src" && NEXT_PUBLIC_PUBLIC_ORIGIN="$origin" NEXT_PUBLIC_APP_NAME="$app_name" \
        NEXT_PUBLIC_BUILD_ID="$build_id" sh -c 'npm ci && npm run build' ); then
    ok "web app built"
  else
    warn "web build failed — skipping the rest of the web tier."; return
  fi

  install -d "$web_dst" && cp -a "$web_src/." "$web_dst/" && ok "deployed web app → ${web_dst}"

  # Write the 0600 web env. Chat/orchestrator tokens mirror the backend env;
  # APP_SESSION_SECRET is generate-if-absent (rotating it logs everyone out).
  local chat_token admin_token app_secret
  chat_token="$(grep '^FLEET_SERVER_TOKEN=' "$ENV_FILE" 2>/dev/null | cut -d= -f2-)"
  admin_token="$(grep '^ADMIN_API_KEY=' "$ENV_FILE" 2>/dev/null | cut -d= -f2-)"
  if [[ -f "$web_env" ]] && grep -q '^APP_SESSION_SECRET=' "$web_env"; then
    app_secret="$(grep '^APP_SESSION_SECRET=' "$web_env" | cut -d= -f2-)"
  else
    app_secret="$(gen_secret 48)"
  fi
  install -D -m 0600 /dev/null "$web_env"
  {
    printf 'CHAT_SERVER_URL=%s\n'         "http://127.0.0.1:8080"
    printf 'CHAT_SERVER_TOKEN=%s\n'       "$chat_token"
    printf 'ORCHESTRATOR_SERVER_URL=%s\n' "http://127.0.0.1:8000"
    printf 'ORCHESTRATOR_SERVER_TOKEN=%s\n' "$admin_token"
    printf 'APP_SESSION_SECRET=%s\n'      "$app_secret"
    printf 'PORT=%s\n'                    "3000"
    printf 'NODE_ENV=%s\n'                "production"
    printf 'NEXT_PUBLIC_PUBLIC_ORIGIN=%s\n' "$origin"
    printf 'NEXT_PUBLIC_APP_NAME=%s\n'    "$app_name"
    printf 'NEXT_PUBLIC_BUILD_ID=%s\n'    "$build_id"
  } > "$web_env"
  chmod 0600 "$web_env"; ok "wrote ${web_env} (0600)"

  systemctl daemon-reload || true
  if systemctl enable --now fleet-web >/dev/null 2>&1; then
    ok "fleet-web enabled + started (Next.js on :3000)"
  else
    warn "could not enable/start fleet-web — check: journalctl -u fleet-web -n 50"
  fi

  if [[ -z "$WEB_DOMAIN" ]]; then
    info "no --domain → web is loopback-only on :3000; front it with your own TLS proxy for a public URL."
    return
  fi
  step "Web tier: Caddy TLS reverse proxy for ${WEB_DOMAIN}"
  if command -v dnf >/dev/null 2>&1 && ! command -v caddy >/dev/null 2>&1; then
    dnf install -y caddy >/dev/null 2>&1 || warn "dnf install caddy failed — install Caddy manually."
  fi
  command -v caddy >/dev/null 2>&1 || { warn "caddy not found — skipping TLS front (web still on :3000)."; return; }
  install -d /etc/caddy
  {
    [[ -n "${FLEET_ACME_EMAIL:-}" ]] && printf '{\n\temail %s\n}\n\n' "$FLEET_ACME_EMAIL"
    printf '%s {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:3000 {\n\t\tflush_interval -1\n\t\ttransport http {\n\t\t\tread_timeout 30m\n\t\t}\n\t}\n}\n' "$WEB_DOMAIN"
  } > /etc/caddy/Caddyfile
  if command -v firewall-cmd >/dev/null 2>&1; then
    firewall-cmd --add-service=http --add-service=https --permanent >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
  fi
  # A fresh `dnf install caddy` creates /var/lib/caddy (the caddy user's cert/account
  # storage). Ensure it exists when caddy was already installed but its storage dir is
  # missing — otherwise caddy fails ACME with "mkdir /var/lib/caddy: permission denied".
  if id caddy >/dev/null 2>&1 && [[ ! -d /var/lib/caddy ]]; then
    install -d -o caddy -g caddy -m 0700 /var/lib/caddy
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || true
  if systemctl is-active caddy >/dev/null 2>&1; then
    ok "caddy serving https://${WEB_DOMAIN} (Let's Encrypt; requires inbound 80/443 reachable)"
  else
    warn "caddy not active — check: journalctl -u caddy -n 50"
  fi
}

step "fleet bootstrap (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"

# ── system dependencies: the build + runtime + sandbox toolchain ──
# So `git clone … && bash scripts/bootstrap.sh …` provisions a BARE box end to
# end (the chat/moc experience): Go (build the binary), Node (build/run the web
# app), podman (the execution sandbox), python3 + pip (host-side Python MCP
# servers), plus git/curl/jq/gcc. Postgres-server is installed per-mode below
# (local only). Non-Fedora hosts: install these yourself, then re-run.
step "Installing system dependencies (build + runtime + sandbox toolchain)"
FLEET_DEPS=(git curl jq golang nodejs python3 python3-pip gcc podman)
if command -v dnf >/dev/null 2>&1; then
  run dnf install -y "${FLEET_DEPS[@]}"
  [[ "$DRY_RUN" == "1" ]] || ok "system dependencies present (${FLEET_DEPS[*]})"
else
  warn "dnf not found — skipping dependency install. Ensure these are present before continuing: ${FLEET_DEPS[*]}"
fi

# ── dedicated service user + rootless-Podman setup (--enable-service path) ──
# Done early (before the sandbox build) because the image is built INTO this
# user's rootless store and the unit runs as it.
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Provisioning the ${SERVICE_USER} service user + rootless Podman (the sandbox runs under it)"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would create user ${SERVICE_USER} (+subuid/subgid), HOME ${SERVICE_HOME}, /run/${SERVICE_USER}, and ~/.config/containers/containers.conf (cgroupfs)"
  elif command -v useradd >/dev/null 2>&1; then
    setup_service_user
  else
    warn "useradd not found — create the '${SERVICE_USER}' user + subuid/subgid + cgroupfs containers.conf manually (see deploy/fleet.service)."
  fi
fi

# ── credential env file (0600) ──
step "Ensuring credential env file ${ENV_FILE} (0600)"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would create ${ENV_FILE} (0600) if missing"
else
  if [[ ! -f "$ENV_FILE" ]]; then
    install -D -m 0600 /dev/null "$ENV_FILE"
    ok "created ${ENV_FILE} (0600)"
  else
    chmod 0600 "$ENV_FILE"
    ok "${ENV_FILE} present (mode set to 0600)"
  fi
fi

# ── client config bundle: resolve --client-config (clone url / point at path) ──
# A git URL is cloned (or pulled if already cloned) into a stable checkout dir; a
# path is pointed at directly. Either way CLIENT_CONFIG_DIR is updated and later
# persisted to the env file. Idempotent: re-running pulls an existing clone.
if [[ -n "$CLIENT_CONFIG_ARG" ]]; then
  step "Resolving client config (--client-config ${CLIENT_CONFIG_ARG})"
  if [[ "$CLIENT_CONFIG_ARG" == *://* || "$CLIENT_CONFIG_ARG" == *@*:* ]]; then
    # Looks like a git URL (scheme:// or scp-style git@host:path). An optional
    # trailing `#<sha-or-tag>` pins the checkout to that exact ref. A URL
    # fragment is invalid in a clone URL, so '#' is an unambiguous pin delimiter
    # here — and we split it ONLY in the URL branch, never for a path (a path
    # could legitimately contain '#').
    CLIENT_CONFIG_REF=""
    if [[ "$CLIENT_CONFIG_ARG" == *#* ]]; then
      CLIENT_CONFIG_REF="${CLIENT_CONFIG_ARG##*#}"
      CLIENT_CONFIG_ARG="${CLIENT_CONFIG_ARG%#*}"
    fi
    CHECKOUT="${FLEET_CLIENT_CONFIG_CHECKOUT:-/opt/fleet/client}"
    if [[ "$DRY_RUN" != "1" && -z "${FLEET_CLIENT_CONFIG_CHECKOUT:-}" ]]; then
      # Fall back to a repo-local checkout when /opt is not writable.
      if ! mkdir -p "$(dirname "$CHECKOUT")" 2>/dev/null || [[ ! -w "$(dirname "$CHECKOUT")" ]]; then
        CHECKOUT="./.fleet-client"
        warn "/opt not writable — cloning client config into ${CHECKOUT} instead"
      fi
    fi
    if ! command -v git >/dev/null 2>&1; then
      die "git is required to clone a --client-config URL (install git or pass a path)"
    fi
    if [[ "$DRY_RUN" == "1" ]]; then
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        info "[dry-run] would clone ${CLIENT_CONFIG_ARG} into ${CHECKOUT} and checkout pinned ref ${CLIENT_CONFIG_REF}"
      else
        info "[dry-run] would clone/pull ${CLIENT_CONFIG_ARG} into ${CHECKOUT}"
      fi
    elif [[ -d "${CHECKOUT}/.git" ]]; then
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        info "client config already cloned at ${CHECKOUT} — fetching + pinning to ${CLIENT_CONFIG_REF}"
        git -C "$CHECKOUT" fetch --quiet --tags origin || warn "git fetch failed in ${CHECKOUT}"
        git -C "$CHECKOUT" checkout --quiet "$CLIENT_CONFIG_REF" || die "git checkout ${CLIENT_CONFIG_REF} failed in ${CHECKOUT}"
      else
        info "client config already cloned at ${CHECKOUT} — pulling latest"
        git -C "$CHECKOUT" pull --ff-only --quiet || warn "git pull failed in ${CHECKOUT} (leaving existing checkout)"
      fi
    else
      run mkdir -p "$(dirname "$CHECKOUT")"
      git clone --quiet "$CLIENT_CONFIG_ARG" "$CHECKOUT" || die "git clone ${CLIENT_CONFIG_ARG} failed"
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        # Full clone then checkout, so a 40-char SHA works as uniformly as a tag
        # (a bare `clone --branch <sha>` cannot resolve a raw commit).
        git -C "$CHECKOUT" checkout --quiet "$CLIENT_CONFIG_REF" || die "git checkout ${CLIENT_CONFIG_REF} failed in ${CHECKOUT}"
      fi
      ok "cloned client config into ${CHECKOUT}"
    fi
    # Persist the pin to the state dir so `update` re-applies it without sourcing
    # the env file (update.sh reads from the inherited env / state file, not the
    # 0600 env file). A no-pin bootstrap clears any stale pin so the checkout
    # returns to branch-tracking on the next update.
    if [[ "$DRY_RUN" != "1" ]]; then
      STATE_DIR="${FLEET_STATE_DIR:-$REPO_ROOT/.fleet-state}"
      if [[ -n "$CLIENT_CONFIG_REF" ]]; then
        mkdir -p "$STATE_DIR" && printf '%s\n' "$CLIENT_CONFIG_REF" > "$STATE_DIR/client-config.pin"
      else
        rm -f "$STATE_DIR/client-config.pin" 2>/dev/null || true
      fi
    fi
    CLIENT_CONFIG_DIR="$CHECKOUT"
  else
    # A path: point at it directly (must exist unless dry-run).
    if [[ "$DRY_RUN" != "1" && ! -d "$CLIENT_CONFIG_ARG" ]]; then
      die "--client-config path ${CLIENT_CONFIG_ARG} does not exist"
    fi
    CLIENT_CONFIG_DIR="$CLIENT_CONFIG_ARG"
    ok "using client config at ${CLIENT_CONFIG_DIR}"
  fi
fi

# ── client config bundle ──
step "Checking client config bundle (FLEET_CLIENT_CONFIG_DIR=${CLIENT_CONFIG_DIR})"
if [[ -f "${CLIENT_CONFIG_DIR}/manifest.yaml" ]]; then
  ok "client bundle manifest found at ${CLIENT_CONFIG_DIR}/manifest.yaml"
  if [[ "${CLIENT_CONFIG_DIR}" == "config/default" ]]; then
    info "using the GENERIC default bundle (neutral branding, no MCP connectors)."
    info "for a branded deploy, check out a client repo and set FLEET_CLIENT_CONFIG_DIR to it."
  fi
else
  warn "no manifest.yaml at ${CLIENT_CONFIG_DIR} — fleet will fail to start until"
  warn "FLEET_CLIENT_CONFIG_DIR points at a valid bundle (a dir with manifest.yaml)."
fi

# ── bundle ownership for the rootless sandbox (--enable-service path) ──
# The sandbox bind-mounts bundle dirs (protocols/ personas/ skills/ system_prompts/)
# into the container with SELinux relabeling (:Z); the rootless service user can
# only relabel files it OWNS. Chown the CHECKOUT to the service user — skip the
# in-repo default bundle (chowning the repo would be wrong, and the service can't
# read a bundle under /root anyway given ProtectHome).
if [[ "$ENABLE_SERVICE" == "1" && "$DRY_RUN" != "1" && -d "$CLIENT_CONFIG_DIR" \
      && "$CLIENT_CONFIG_DIR" != "config/default" && "$CLIENT_CONFIG_DIR" != "$REPO_ROOT"/* ]]; then
  if chown -R "$SERVICE_USER":"$SERVICE_USER" "$CLIENT_CONFIG_DIR"; then
    ok "bundle ${CLIENT_CONFIG_DIR} owned by ${SERVICE_USER} (so rootless :Z relabel is permitted)"
  else
    warn "could not chown ${CLIENT_CONFIG_DIR} to ${SERVICE_USER} — sandbox :Z relabel may fail (EPERM)."
  fi
fi

# resolve_sandbox_image MANIFEST — print the bundle's resolved sandbox.image, the
# SAME way the Go loader (internal/clientconfig) does: extract the scalar under
# the sandbox: block, then interpolate a bare ${VAR:-default} / ${VAR} reference
# against the process env. An empty result => build-on-box (the default-bundle
# value "${FLEET_SANDBOX_IMAGE:-}" resolves to empty when the var is unset).
resolve_sandbox_image() {
  local file="$1" raw
  [[ -f "$file" ]] || return 0
  raw="$(awk '
    /^sandbox:[[:space:]]*$/ { in_block=1; next }
    /^[^[:space:]]/          { in_block=0 }
    in_block && $0 ~ "^[[:space:]]+image:" {
      sub("^[[:space:]]+image:[[:space:]]*", "")
      sub(/[[:space:]]+#.*$/, "")
      gsub(/^["'\'']|["'\'']$/, "")
      print; exit
    }
  ' "$file")"
  # Interpolate a single leading ${VAR} or ${VAR:-default} (the only shapes the
  # default bundle uses). Anything else is treated as a literal image ref.
  if [[ "$raw" =~ ^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$ ]]; then
    local var="${BASH_REMATCH[1]}" def="${BASH_REMATCH[3]}"
    printf '%s' "${!var:-$def}"
  else
    printf '%s' "$raw"
  fi
}

# ── sandbox image (per-client bundle artifact; build-on-box default) ──
# The execution sandbox is a per-client bundle artifact: the Containerfile lives
# in the bundle at <bundle>/sandbox/Containerfile. DEFAULT = build it on this box
# (auditable supply chain). REGISTRY PUBLISH is opt-in: a client sets
# sandbox.image in its manifest to a prebuilt ref and fleet pulls/uses that
# instead — in which case skip the on-box build here.
step "Building the sandbox image from the bundle (build-on-box default)"
SANDBOX_CONTAINERFILE="${CLIENT_CONFIG_DIR}/sandbox/Containerfile"
SANDBOX_IMAGE_REF="$(resolve_sandbox_image "${CLIENT_CONFIG_DIR}/manifest.yaml")"
if [[ -n "$SANDBOX_IMAGE_REF" ]]; then
  info "manifest resolves sandbox.image=${SANDBOX_IMAGE_REF} — using a prebuilt/registry image; skipping on-box build."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: FLEET_CLIENT_CONFIG_DIR=${CLIENT_CONFIG_DIR} scripts/build-sandbox-image.sh"
elif [[ ! -f "$SANDBOX_CONTAINERFILE" ]]; then
  warn "no ${SANDBOX_CONTAINERFILE} — bundle ships no sandbox Containerfile; set sandbox.image or add one."
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — skipping sandbox build (install podman, then run scripts/build-sandbox-image.sh)."
elif [[ "$ENABLE_SERVICE" == "1" ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
  # systemd path: build INTO the service user's rootless image store, so the
  # User=fleet unit finds the image (root's rootful store is a separate namespace).
  SANDBOX_TAG="$(sandbox_manifest_tag "${CLIENT_CONFIG_DIR}/manifest.yaml")"
  info "building as ${SERVICE_USER} (rootless) → ${SANDBOX_TAG}"
  if runuser -u "$SERVICE_USER" -- sh -c "cd '${SERVICE_HOME}' && HOME='${SERVICE_HOME}' XDG_RUNTIME_DIR='/run/${SERVICE_USER}' podman build -t '${SANDBOX_TAG}' -f '${SANDBOX_CONTAINERFILE}' '${CLIENT_CONFIG_DIR}/sandbox'"; then
    ok "sandbox image ${SANDBOX_TAG} built into ${SERVICE_USER}'s rootless store"
  else
    warn "rootless sandbox build (as ${SERVICE_USER}) failed — fleet will have no runnable sandbox image."
  fi
else
  if FLEET_CLIENT_CONFIG_DIR="${CLIENT_CONFIG_DIR}" "$(dirname "$0")/build-sandbox-image.sh"; then
    ok "sandbox image built from ${SANDBOX_CONTAINERFILE}"
  else
    warn "sandbox image build failed — run scripts/build-sandbox-image.sh manually before starting fleet."
  fi
fi

# ── native-agent image (#159) — the containerized agent LOOP for native-acp, ──
# the DEFAULT runtime flavor. It EXTENDS the sandbox base above, so it builds
# after it. cmd/fleet refuses to start when native-acp is the default but this
# image is absent (fail-closed preflight), so build it now. Best-effort + warn: a
# failure leaves the clear startup error pointing at this script. Skipped when an
# operator runs in-process-only (FLEET_ENABLE_INPROCESS_LOOP=1) — then native-acp
# isn't the default and the preflight has nothing to check.
step "Building the native-agent image (containerized loop for native-acp)"
NATIVE_AGENT_CONTAINERFILE="${REPO_ROOT}/config/default/sandbox/Containerfile.native-agent"
NATIVE_AGENT_IMAGE="${FLEET_NATIVE_AGENT_IMAGE:-localhost/fleet-native-agent:latest}"
NATIVE_AGENT_SANDBOX_BASE="${SANDBOX_IMAGE_REF:-$(sandbox_manifest_tag "${CLIENT_CONFIG_DIR}/manifest.yaml")}"
if [[ "${FLEET_ENABLE_INPROCESS_LOOP:-}" == "1" ]]; then
  info "FLEET_ENABLE_INPROCESS_LOOP=1 — native-acp is not the default; skipping native-agent build."
elif [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would run: FLEET_SANDBOX_IMAGE=${NATIVE_AGENT_SANDBOX_BASE} scripts/build-native-agent-image.sh"
elif [[ ! -f "$NATIVE_AGENT_CONTAINERFILE" ]]; then
  warn "no ${NATIVE_AGENT_CONTAINERFILE} — cannot build native-agent; set runtimes.native-acp.image or FLEET_ENABLE_INPROCESS_LOOP=1."
elif ! command -v podman >/dev/null 2>&1; then
  warn "podman not found — skipping native-agent build (install podman, then run scripts/build-native-agent-image.sh)."
elif [[ "$ENABLE_SERVICE" == "1" ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
  # Build into the service user's rootless store (same as the sandbox image) so
  # the User=fleet unit — and the startup preflight running as that user — find it.
  info "building as ${SERVICE_USER} (rootless) → ${NATIVE_AGENT_IMAGE} (base ${NATIVE_AGENT_SANDBOX_BASE})"
  if runuser -u "$SERVICE_USER" -- sh -c "cd '${REPO_ROOT}' && HOME='${SERVICE_HOME}' XDG_RUNTIME_DIR='/run/${SERVICE_USER}' FLEET_NATIVE_AGENT_IMAGE='${NATIVE_AGENT_IMAGE}' FLEET_SANDBOX_IMAGE='${NATIVE_AGENT_SANDBOX_BASE}' '${REPO_ROOT}/scripts/build-native-agent-image.sh'"; then
    ok "native-agent image ${NATIVE_AGENT_IMAGE} built into ${SERVICE_USER}'s rootless store"
  else
    warn "rootless native-agent build (as ${SERVICE_USER}) failed — fleet will refuse to start with the native-acp default until it's built; run scripts/build-native-agent-image.sh."
  fi
else
  if FLEET_NATIVE_AGENT_IMAGE="${NATIVE_AGENT_IMAGE}" FLEET_SANDBOX_IMAGE="${NATIVE_AGENT_SANDBOX_BASE}" "$(dirname "$0")/build-native-agent-image.sh"; then
    ok "native-agent image built from ${NATIVE_AGENT_CONTAINERFILE}"
  else
    warn "native-agent image build failed — run scripts/build-native-agent-image.sh before starting fleet."
  fi
fi

# ── host-side MCP server Python deps (the active bundle's requirements) ──
# fleet runs the bundle's MCP servers host-side as `python3 <bundle>/mcp/*.py`, so
# their Python deps must be importable by the system python3 the service user runs.
# Generic: installs whatever THIS bundle's mcp/requirements.txt lists (no
# bundle-specific package names here). --break-system-packages is for Fedora's
# PEP-668 externally-managed python; harmless to drop on other distros.
if [[ "$ENABLE_SERVICE" == "1" && -f "${CLIENT_CONFIG_DIR}/mcp/requirements.txt" ]]; then
  step "Installing the bundle's host-side MCP Python deps (${CLIENT_CONFIG_DIR}/mcp/requirements.txt)"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: python3 -m pip install --break-system-packages -r ${CLIENT_CONFIG_DIR}/mcp/requirements.txt"
  elif command -v python3 >/dev/null 2>&1; then
    if python3 -m pip install --break-system-packages -r "${CLIENT_CONFIG_DIR}/mcp/requirements.txt"; then
      ok "bundle MCP Python deps installed (host-side servers can start)"
    else
      warn "pip install of ${CLIENT_CONFIG_DIR}/mcp/requirements.txt failed — host-side MCP servers may not start."
    fi
  else
    warn "python3 not found — install ${CLIENT_CONFIG_DIR}/mcp/requirements.txt manually."
  fi
fi

# ── Postgres provisioning ──
if [[ "$POSTGRES_MODE" == "local" ]]; then
  SSLMODE="disable"
  CHAT_DB_PASSWORD="${CHAT_DB_PASSWORD:-$(gen_pass)}"
  SCHED_DB_PASSWORD="${SCHED_DB_PASSWORD:-$(gen_pass)}"

  step "Branch A (local): install + init a local Postgres cluster"
  if command -v dnf >/dev/null 2>&1; then
    run dnf install -y postgresql-server postgresql
  else
    warn "dnf not found — skipping package install (assuming postgresql-server present)"
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would initdb (if needed), set pg_hba scram-sha-256 on loopback, systemctl enable --now postgresql"
  else
    if [[ ! -s /var/lib/pgsql/data/PG_VERSION ]]; then
      info "initializing data directory"
      if command -v postgresql-setup >/dev/null 2>&1; then
        postgresql-setup --initdb >/dev/null 2>&1 || runuser -u postgres -- /usr/bin/initdb -D /var/lib/pgsql/data
      else
        runuser -u postgres -- /usr/bin/initdb -D /var/lib/pgsql/data
      fi
    fi
    if command -v systemctl >/dev/null 2>&1; then
      systemctl enable --now postgresql >/dev/null 2>&1 || warn "could not start postgresql via systemctl (already running?)"
    fi

    # Default Fedora/RHEL initdb authenticates loopback TCP with `ident`, which
    # REJECTS the password DSN fleet connects with (postgres://chat:…@127.0.0.1).
    # Rewrite the loopback host lines to scram-sha-256 so first boot authenticates
    # (chat/moc bootstrap did this; fleet must too). local peer is left intact so
    # the `runuser -u postgres psql` role provisioning below still works.
    PG_HBA="$(runuser -u postgres -- psql -tAc 'SHOW hba_file' 2>/dev/null || true)"
    [[ -n "$PG_HBA" && -f "$PG_HBA" ]] || PG_HBA="/var/lib/pgsql/data/pg_hba.conf"
    if [[ -f "$PG_HBA" ]]; then
      if grep -qE '^[[:space:]]*host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1/32|::1/128)[[:space:]]+(ident|md5|trust|peer)' "$PG_HBA"; then
        sed -i -E 's#^([[:space:]]*host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1/32|::1/128)[[:space:]]+)(ident|md5|trust|peer)#\1scram-sha-256#' "$PG_HBA"
        systemctl reload postgresql >/dev/null 2>&1 || warn "could not reload postgresql after pg_hba rewrite"
        ok "pg_hba: loopback host auth set to scram-sha-256 (${PG_HBA})"
      else
        info "pg_hba loopback host lines already scram-sha-256 (or non-default) — left as-is"
      fi
    else
      warn "could not locate pg_hba.conf — verify loopback host auth allows password (scram-sha-256) manually"
    fi
  fi

  step "Creating roles + databases idempotently (chat + sched)"
  PSQL_SQL=$(cat <<SQL
SELECT 'CREATE ROLE ${CHAT_DB_USER} LOGIN PASSWORD ''${CHAT_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${CHAT_DB_USER}')\gexec
SELECT 'CREATE DATABASE ${CHAT_DB_NAME} OWNER ${CHAT_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${CHAT_DB_NAME}')\gexec
SELECT 'CREATE ROLE ${SCHED_DB_USER} LOGIN PASSWORD ''${SCHED_DB_PASSWORD}'''
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='${SCHED_DB_USER}')\gexec
SELECT 'CREATE DATABASE ${SCHED_DB_NAME} OWNER ${SCHED_DB_USER}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${SCHED_DB_NAME}')\gexec
SQL
)
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run as postgres:"
    printf '%s\n' "$PSQL_SQL" | sed 's/^/    /'
  else
    printf '%s\n' "$PSQL_SQL" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 >/dev/null \
      || die "role/database provisioning failed"
  fi

  CHAT_URL="postgres://${CHAT_DB_USER}:${CHAT_DB_PASSWORD}@127.0.0.1:5432/${CHAT_DB_NAME}?sslmode=${SSLMODE}"
  SCHED_URL="postgres://${SCHED_DB_USER}:${SCHED_DB_PASSWORD}@127.0.0.1:5432/${SCHED_DB_NAME}?sslmode=${SSLMODE}"
  ok "chat DB:  ${CHAT_DB_NAME} (owner ${CHAT_DB_USER}), sslmode=${SSLMODE}"
  ok "sched DB: ${SCHED_DB_NAME} (owner ${SCHED_DB_USER}), sslmode=${SSLMODE}"

else
  SSLMODE="require"
  step "Branch B (external): validate pre-provisioned DSNs (SELECT 1)"
  CHAT_URL="${FLEET_CHAT_DATABASE_URL:-}"
  SCHED_URL="${FLEET_SCHED_DATABASE_URL:-}"
  [[ -n "$CHAT_URL" ]]  || die "external mode needs FLEET_CHAT_DATABASE_URL"
  [[ -n "$SCHED_URL" ]] || die "external mode needs FLEET_SCHED_DATABASE_URL"

  # Opt-in superuser provisioning of roles/dbs.
  if [[ -n "${FLEET_DB_SUPERUSER_URL:-}" ]]; then
    step "Opt-in: provisioning roles/dbs via FLEET_DB_SUPERUSER_URL"
    SU_SQL=$(cat <<SQL
SELECT 'CREATE DATABASE ${CHAT_DB_NAME}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${CHAT_DB_NAME}')\gexec
SELECT 'CREATE DATABASE ${SCHED_DB_NAME}'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='${SCHED_DB_NAME}')\gexec
SQL
)
    if [[ "$DRY_RUN" == "1" ]]; then
      info "[dry-run] would run superuser SQL against FLEET_DB_SUPERUSER_URL"
    else
      printf '%s\n' "$SU_SQL" | psql -v ON_ERROR_STOP=1 "$FLEET_DB_SUPERUSER_URL" >/dev/null \
        || die "superuser provisioning failed"
    fi
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: psql '<chat dsn>' -c 'SELECT 1'"
    info "[dry-run] would run: psql '<sched dsn>' -c 'SELECT 1'"
  else
    psql -v ON_ERROR_STOP=1 "$CHAT_URL"  -c "SELECT 1" >/dev/null || die "chat DSN failed SELECT 1"
    psql -v ON_ERROR_STOP=1 "$SCHED_URL" -c "SELECT 1" >/dev/null || die "sched DSN failed SELECT 1"
    ok "both external DSNs answered SELECT 1 (sslmode=${SSLMODE} expected in the DSN)"
  fi
fi

# ── write/refresh the env file (0600) ──
# Persist the resolved DSNs + the client-bundle dir so the fleet process and
# fleet-admin read them from the SAME 0600 file deploy/fleet.service EnvironmentFiles.
# Idempotent: re-running rewrites these keys in place (passwords rotate only when
# CHAT_DB_PASSWORD/SCHED_DB_PASSWORD are pre-set; generated ones change per run).
step "Writing connection settings into ${ENV_FILE} (0600)"
upsert_env FLEET_CHAT_DATABASE_URL "$CHAT_URL"
upsert_env FLEET_SCHED_DATABASE_URL "$SCHED_URL"
upsert_env FLEET_CLIENT_CONFIG_DIR "$CLIENT_CONFIG_DIR"
# Point config.Load at the same file so process-env and config-loaded values match.
upsert_env FLEET_ENV_FILE "$ENV_FILE"
if [[ "$DRY_RUN" != "1" ]]; then
  ok "wrote FLEET_CHAT_DATABASE_URL / FLEET_SCHED_DATABASE_URL / FLEET_CLIENT_CONFIG_DIR / FLEET_ENV_FILE"
  info "remember to add OPENROUTER_API_KEY and the bundle's MCP connector credentials."
  info "if the bundle's default persona differs from 'assistant', set PERSONA_DEFAULT=<persona> in ${ENV_FILE}."
fi

# ── optionally build + install the binary + unit, then enable + start it ──
# fleet-admin bootstrap/update operate on a SOURCE CHECKOUT (this repo) and
# install the built artifacts to FLEET_INSTALL_DIR (default /opt/fleet) — the
# location deploy/fleet.service's ExecStart points at.
INSTALL_DIR="${FLEET_INSTALL_DIR:-/opt/fleet}"
if [[ "$ENABLE_SERVICE" == "1" ]]; then
  step "Building + installing the fleet binary, then enabling ${SERVICE_NAME}"
  if [[ "$DRY_RUN" == "1" ]]; then
    info "[dry-run] would run: (cd ${REPO_ROOT} && make build)  → fleet + fleet-admin"
    info "[dry-run] would install fleet + fleet-admin → ${INSTALL_DIR}"
    info "[dry-run] would install deploy/fleet.service + deploy/fleet-web.service → /etc/systemd/system"
    info "[dry-run] would run: systemctl daemon-reload && systemctl enable --now ${SERVICE_NAME}"
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-service (no systemd on this box)."
  else
    # 1. Build the deployable artifacts from this checkout (needs Go on the box).
    if command -v go >/dev/null 2>&1 || command -v make >/dev/null 2>&1; then
      # GOTOOLCHAIN=auto: Fedora's `golang` lags go.mod's pinned version, so let
      # the toolchain fetch the required Go rather than failing an opaque build.
      if ( cd "$REPO_ROOT" && GOTOOLCHAIN=auto make build ) && [[ -x "$REPO_ROOT/fleet" && -x "$REPO_ROOT/fleet-admin" ]]; then
        install -D -m 0755 "$REPO_ROOT/fleet"       "$INSTALL_DIR/fleet"
        install -D -m 0755 "$REPO_ROOT/fleet-admin" "$INSTALL_DIR/fleet-admin"
        ok "installed fleet + fleet-admin → ${INSTALL_DIR}"
      else
        die "make build failed or produced no artifacts — install Go and retry"
      fi
    elif [[ -x "$INSTALL_DIR/fleet" ]]; then
      warn "no Go toolchain — using the existing ${INSTALL_DIR}/fleet (build + install manually to update it)."
    else
      die "no Go toolchain and no binary at ${INSTALL_DIR}/fleet — install Go (or pre-build) then re-run"
    fi
    # 2. Install the unit files from this checkout if not already present.
    for unit in fleet.service fleet-web.service; do
      if [[ -f "$REPO_ROOT/deploy/$unit" ]] && ! systemctl cat "$unit" >/dev/null 2>&1; then
        install -D -m 0644 "$REPO_ROOT/deploy/$unit" "/etc/systemd/system/$unit"
        info "installed /etc/systemd/system/$unit"
      fi
    done
    # 3. daemon-reload + enable the backend unit. The web unit (fleet-web)
    #    additionally needs the built Next app at /opt/fleet/web + its 0600
    #    env file, so we install it but leave enabling it to the operator.
    systemctl daemon-reload || warn "systemctl daemon-reload failed"
    if systemctl enable --now "${SERVICE_NAME}" >/dev/null 2>&1; then
      ok "${SERVICE_NAME} enabled + started (services self-migrate on start)"
    else
      warn "could not enable/start ${SERVICE_NAME} — check: journalctl -u ${SERVICE_NAME} -n 50"
    fi
    if [[ "$ENABLE_WEB" != "1" ]]; then
      info "web tier: re-run with --enable-web [--domain <fqdn>] to build + serve it,"
      info "          or by hand (cd web && npm ci && npm run build → /opt/fleet/web, fill fleet-web.env, enable fleet-web)."
    fi
  fi
fi

# ── web tier + Caddy TLS (opt-in via --enable-web / --domain) ──
if [[ "$ENABLE_WEB" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    step "Web tier (--enable-web): plan"
    info "[dry-run] would ensure FLEET_SERVER_TOKEN + ADMIN_API_KEY in ${ENV_FILE} (generate-if-absent) + reload backend."
    if [[ -n "$WEB_DOMAIN" ]]; then
      info "[dry-run] would build web/ for https://${WEB_DOMAIN} → /opt/fleet/web, write fleet-web.env, enable fleet-web, install Caddy + open 80/443."
    else
      info "[dry-run] would build web/ for http://localhost:3000 → /opt/fleet/web, write fleet-web.env, enable fleet-web (loopback only; no --domain → no Caddy)."
    fi
  elif ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping --enable-web (no systemd on this box)."
  else
    deploy_web_tier
  fi
fi

step "Reminders"
info "Migrations are NOT run here — each service self-migrates on first start."
info "Set MCP account secrets post-bootstrap: fleet-admin mcp account set <server> <account> --secret KEY=-"
info "Check health any time:  fleet-admin status"
info "Update in place later:  fleet-admin update   (or scripts/update.sh)"
ok "bootstrap complete (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"
