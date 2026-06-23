#!/usr/bin/env bash
# scripts/bootstrap.sh — Mega Box DB + credential-store bootstrap for fleet.
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
#   scripts/bootstrap.sh --client-config <git-url|path>   # check out / point at a client bundle
#   scripts/bootstrap.sh --enable-service            # systemctl enable --now fleet at the end
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
#   --client-config <url|path> a git URL (cloned to a stable location) or an
#                              existing path (pointed at directly). Sets
#                              FLEET_CLIENT_CONFIG_DIR in the env file.
#   --enable-service           systemctl enable --now the fleet unit at the end.
#   --dry-run                  print the plan; touch nothing.
#
# Env knobs (all optional; sensible local defaults):
#   FLEET_ENV_FILE          credential env file to write/refresh (default .env.local)
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

while [[ $# -gt 0 ]]; do
  case "$1" in
    --postgres=local)    POSTGRES_MODE="local" ;;
    --postgres=external) POSTGRES_MODE="external" ;;
    --postgres=*)        echo "error: --postgres must be local|external" >&2; exit 1 ;;
    --client-config)     shift; [[ $# -gt 0 ]] || { echo "error: --client-config needs a git-url|path" >&2; exit 1; }; CLIENT_CONFIG_ARG="$1" ;;
    --client-config=*)   CLIENT_CONFIG_ARG="${1#*=}" ;;
    --enable-service)    ENABLE_SERVICE=1 ;;
    --dry-run)           DRY_RUN=1 ;;
    -h|--help)
      sed -n '2,57p' "$0"; exit 0 ;;
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

ENV_FILE="${FLEET_ENV_FILE:-.env.local}"
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

# ── credential env file (0600) ──
step "Ensuring credential env file ${ENV_FILE} (0600)"
if [[ "$DRY_RUN" == "1" ]]; then
  info "[dry-run] would create ${ENV_FILE} (0600) if missing"
else
  if [[ ! -f "$ENV_FILE" ]]; then
    install -m 0600 /dev/null "$ENV_FILE"
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
    # Looks like a git URL (scheme:// or scp-style git@host:path).
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
      info "[dry-run] would clone/pull ${CLIENT_CONFIG_ARG} into ${CHECKOUT}"
    elif [[ -d "${CHECKOUT}/.git" ]]; then
      info "client config already cloned at ${CHECKOUT} — pulling latest"
      git -C "$CHECKOUT" pull --ff-only --quiet || warn "git pull failed in ${CHECKOUT} (leaving existing checkout)"
    else
      run mkdir -p "$(dirname "$CHECKOUT")"
      git clone --quiet "$CLIENT_CONFIG_ARG" "$CHECKOUT" || die "git clone ${CLIENT_CONFIG_ARG} failed"
      ok "cloned client config into ${CHECKOUT}"
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
else
  if FLEET_CLIENT_CONFIG_DIR="${CLIENT_CONFIG_DIR}" "$(dirname "$0")/build-sandbox-image.sh"; then
    ok "sandbox image built from ${SANDBOX_CONTAINERFILE}"
  else
    warn "sandbox image build failed — run scripts/build-sandbox-image.sh manually before starting fleet."
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
      if ( cd "$REPO_ROOT" && make build ) && [[ -x "$REPO_ROOT/fleet" && -x "$REPO_ROOT/fleet-admin" ]]; then
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
    info "web tier: build it (cd web && npm ci && npm run build), deploy to /opt/fleet/web,"
    info "          fill /etc/fleet/fleet-web.env, then: systemctl enable --now fleet-web"
  fi
fi

step "Reminders"
info "Migrations are NOT run here — each service self-migrates on first start."
info "Set MCP account secrets post-bootstrap: fleet-admin mcp account set <server> <account> --secret KEY=-"
info "Check health any time:  fleet-admin status"
info "Update in place later:  fleet-admin update   (or scripts/update.sh)"
ok "bootstrap complete (postgres=${POSTGRES_MODE}, dry-run=${DRY_RUN})"
