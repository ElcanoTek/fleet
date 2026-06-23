#!/usr/bin/env bash
# e2e-boot-server.sh — boot the FULL fleet stack for the live Playwright suite.
#
# Everything REAL except the LLM: Postgres, both Go listeners (chat + orchestrator),
# SSE streaming, the scheduler + worker pool, and the Podman sandbox all run for
# real. Only the LLM is stubbed — by the wire-compatible fake (cmd/fake-llm),
# which fleet is pointed at via OPENROUTER_BASE_URL. This keeps the live suite
# deterministic and free while exercising the genuine provider/SSE/tool-loop and
# the real container sandbox.
#
# Stack booted here (in order, each health-gated — no fixed sleeps):
#   1. Postgres        — local instance or $E2E_DATABASE_DSN; fresh chat+sched DBs
#   2. sandbox image   — built locally (or pulled; see CI) → $FLEET_SANDBOX_IMAGE
#   3. fake-llm        — cmd/fake-llm on $FAKE_LLM_ADDR (OpenRouter wire fake)
#   4. fleet           — cmd/fleet: chat :$CHAT_PORT, orchestrator :$ORCH_PORT
#   5. web             — next build + next start on :$NEXT_PORT
# Then it seeds the test users (chat + sched), prints the env Playwright needs,
# and (in --serve mode) blocks until signalled, tearing everything down on exit.
#
# ── Modes ──
#   scripts/e2e-boot-server.sh                 # boot, print env, block (Ctrl-C to stop)
#   scripts/e2e-boot-server.sh --serve         # same as default (explicit)
#   scripts/e2e-boot-server.sh --print-env >env # boot, write env file, KEEP RUNNING in bg? No —
#                                               # --serve blocks; use the env it prints.
#
# Playwright (playwright.config.ts, the "live" project) launches this script as
# its webServer and waits on the Next URL. The script writes the resolved env to
# $E2E_ENV_FILE (default web/.e2e-live.env) so the Playwright config / specs can
# source the ports + secrets.
#
# ── Required tools ──
#   go, node, npm, podman, and a Postgres server (local `pg_ctl`/`initdb`, OR a
#   reachable instance via $E2E_DATABASE_DSN).
#
# ── Key env (all have defaults; override to suit local/CI) ──
#   CHAT_PORT            chat listener port            (default 18080)
#   ORCH_PORT            orchestrator listener port    (default 18000)
#   NEXT_PORT            web app port                  (default 3100)
#   FAKE_LLM_ADDR        fake LLM listen addr          (default 127.0.0.1:18090)
#   FLEET_SANDBOX_IMAGE  sandbox image ref             (default localhost/fleet-sandbox:latest)
#   E2E_DATABASE_DSN     base Postgres DSN to a server with CREATEDB rights;
#                        the script creates chat + sched DBs on it.
#                        (default: local instance at 127.0.0.1:5432 as the
#                        current user, or a throwaway cluster if none is up)
#   E2E_TEST_EMAIL       chat test user  (default e2e@example.com)
#   E2E_TEST_PASSWORD    test password   (default e2e-test-password)
#   E2E_SCHED_USERNAME   orchestrator user (default e2e)
#   FLEET_SERVER_TOKEN   chat shared secret (default e2e-shared-secret)
#   ADMIN_API_KEY        orchestrator admin key (default e2e-admin-key)
#   APP_SESSION_SECRET   web HMAC session secret
#   E2E_ENV_FILE         where to write resolved env (default web/.e2e-live.env)
#   E2E_SKIP_WEB=1       boot only the backend (no next build/start)
#   E2E_REUSE_DB=1       do not drop/recreate the chat+sched DBs
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# ── config ──
CHAT_PORT="${CHAT_PORT:-18080}"
ORCH_PORT="${ORCH_PORT:-18000}"
NEXT_PORT="${NEXT_PORT:-3100}"
FAKE_LLM_ADDR="${FAKE_LLM_ADDR:-127.0.0.1:18090}"
FLEET_SANDBOX_IMAGE="${FLEET_SANDBOX_IMAGE:-localhost/fleet-sandbox:latest}"
E2E_TEST_EMAIL="${E2E_TEST_EMAIL:-e2e@example.com}"
E2E_TEST_PASSWORD="${E2E_TEST_PASSWORD:-e2e-test-password}"
E2E_SCHED_USERNAME="${E2E_SCHED_USERNAME:-e2e}"
FLEET_SERVER_TOKEN="${FLEET_SERVER_TOKEN:-e2e-shared-secret}"
ADMIN_API_KEY="${ADMIN_API_KEY:-e2e-admin-key}"
APP_SESSION_SECRET="${APP_SESSION_SECRET:-e2e-session-secret-0123456789abcdef}"
# TEST ONLY — throwaway Ed25519 pubkey so the "Use Elcano email" login path
# renders; the live suite never mints/verifies an elcano_auth token, so any
# shape-valid pubkey works. Not used in any deployment.
AUTH_SIGNING_PUBKEY="${AUTH_SIGNING_PUBKEY:-P8Lyn/xy8bS2SWcnPkg2kKiGBctyJZdEMMzNEixh0As=}"
E2E_ENV_FILE="${E2E_ENV_FILE:-$REPO_ROOT/web/.e2e-live.env}"
# CANARY MODE (E2E_CANARY=1): use the REAL OpenRouter + a real cheap model
# instead of the fake LLM. This is the drift canary — it spends a few credits to
# prove fleet still works against a genuine provider. In canary mode we keep the
# operator's OPENROUTER_API_KEY and do NOT set OPENROUTER_BASE_URL (so the real
# upstream is used) and do NOT start the fake. Everything else is identical.
E2E_CANARY="${E2E_CANARY:-0}"
CANARY_MODEL="${CANARY_MODEL:-openai/gpt-5.2}"
# Fake-LLM mode key: must be non-empty + sk-or-* for fleet's scheduled-mode
# validation; the FAKE llm ignores it. NEVER a real key — the live suite must
# not spend credits.
FAKE_OPENROUTER_KEY="sk-or-fake-e2e-do-not-bill"

BIN_DIR="$REPO_ROOT/.e2e-bin"
RUN_DIR="$REPO_ROOT/.e2e-run"
LOG_DIR="${E2E_LOG_DIR:-$RUN_DIR/logs}"
# The sandbox bind-mounts the workspace at the SAME host path inside the
# container and runs as uid 1000 (--userns=keep-id). If that path traverses a
# non-world-executable parent (e.g. /root is mode 0550 when running as root),
# the container user cannot enter it and run_python's chdir fails. So the
# workspace + bridge live under a world-traversable base, NOT under the repo
# when the repo sits inside a locked-down home. Override with E2E_WORKSPACE_BASE.
WORKSPACE_BASE="${E2E_WORKSPACE_BASE:-${TMPDIR:-/tmp}/fleet-e2e-$(id -u)}"
WORKSPACE_DIR="$WORKSPACE_BASE/workspace"
DATA_DIR="$WORKSPACE_BASE/data"
mkdir -p "$BIN_DIR" "$RUN_DIR" "$LOG_DIR" "$WORKSPACE_DIR" "$DATA_DIR"
# Make the base traversable by the container uid (parents must be enterable).
chmod 0755 "$WORKSPACE_BASE" "$WORKSPACE_DIR" "$DATA_DIR" 2>/dev/null || true

log() { printf '[e2e-boot] %s\n' "$*" >&2; }
die() { printf '[e2e-boot] FATAL: %s\n' "$*" >&2; exit 1; }

# ── cleanup trap: kill procs, drop sandbox containers by name prefix ──
PIDS=()
PG_TEMP_DIR=""
PG_TEMP_STARTED=0
cleanup() {
  local ec=$?
  log "cleanup: tearing down (exit=$ec)"
  for pid in "${PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  # Reap any leftover sandbox containers fleet launched. They are named
  # chat-sandbox-<hex> (internal/sandbox/container.go); there are no labels.
  if command -v podman >/dev/null 2>&1; then
    podman ps -aq --filter "name=chat-sandbox-" 2>/dev/null | xargs -r podman rm -f >/dev/null 2>&1 || true
  fi
  # Stop the throwaway Postgres cluster if WE started one.
  if [[ "$PG_TEMP_STARTED" == "1" && -n "$PG_TEMP_DIR" ]]; then
    pg_ctl -D "$PG_TEMP_DIR/data" stop -m immediate >/dev/null 2>&1 || true
  fi
  log "cleanup: done"
}
trap cleanup EXIT INT TERM

wait_http() { # url timeout_secs label
  local url="$1" timeout="${2:-60}" label="${3:-$1}" start now
  start=$(date +%s)
  while true; do
    if curl -fsS -o /dev/null --max-time 3 "$url" 2>/dev/null; then
      log "ready: $label ($url)"
      return 0
    fi
    now=$(date +%s)
    if (( now - start >= timeout )); then
      die "timeout waiting for $label at $url after ${timeout}s"
    fi
    sleep 0.5
  done
}

wait_pg() { # dsn timeout
  local dsn="$1" timeout="${2:-60}" start now
  start=$(date +%s)
  while true; do
    if psql "$dsn" -c 'SELECT 1' >/dev/null 2>&1; then return 0; fi
    now=$(date +%s)
    (( now - start >= timeout )) && die "timeout waiting for Postgres at $dsn"
    sleep 0.5
  done
}

# ── 1. Postgres: reuse a reachable instance, else stand up a throwaway one ──
ensure_postgres() {
  local admin_dsn
  if [[ -n "${E2E_DATABASE_DSN:-}" ]]; then
    admin_dsn="$E2E_DATABASE_DSN"
    log "postgres: using provided E2E_DATABASE_DSN"
  elif PGCONNECT_TIMEOUT=2 psql "postgres://127.0.0.1:5432/postgres" -c 'SELECT 1' >/dev/null 2>&1; then
    admin_dsn="postgres://127.0.0.1:5432/postgres"
    log "postgres: using local instance at 127.0.0.1:5432"
  elif PGCONNECT_TIMEOUT=2 psql "postgres://postgres@127.0.0.1:5432/postgres" -c 'SELECT 1' >/dev/null 2>&1; then
    admin_dsn="postgres://postgres@127.0.0.1:5432/postgres"
    log "postgres: using local instance (postgres role) at 127.0.0.1:5432"
  else
    log "postgres: no reachable instance; starting a throwaway cluster"
    command -v initdb >/dev/null 2>&1 || die "no Postgres reachable and initdb not found"
    PG_TEMP_DIR="$RUN_DIR/pg"
    rm -rf "$PG_TEMP_DIR"; mkdir -p "$PG_TEMP_DIR/data"
    # Drop to the postgres user if we are root (initdb refuses to run as root).
    if [[ "$(id -u)" == "0" ]] && id postgres >/dev/null 2>&1; then
      chown -R postgres "$PG_TEMP_DIR"
      su postgres -c "initdb -D '$PG_TEMP_DIR/data' -A trust -U postgres" >"$LOG_DIR/pg-init.log" 2>&1
      su postgres -c "pg_ctl -D '$PG_TEMP_DIR/data' -o '-p 55433 -k \"$PG_TEMP_DIR\" -h 127.0.0.1' -l '$LOG_DIR/pg.log' start" >/dev/null 2>&1
    else
      initdb -D "$PG_TEMP_DIR/data" -A trust -U postgres >"$LOG_DIR/pg-init.log" 2>&1
      pg_ctl -D "$PG_TEMP_DIR/data" -o "-p 55433 -k '$PG_TEMP_DIR' -h 127.0.0.1" -l "$LOG_DIR/pg.log" start >/dev/null 2>&1
    fi
    PG_TEMP_STARTED=1
    admin_dsn="postgres://postgres@127.0.0.1:55433/postgres"
    wait_pg "$admin_dsn" 60
  fi

  # Parse host/port/user from the admin DSN to derive role-scoped DSNs.
  local host port user
  host="$(printf '%s' "$admin_dsn" | sed -E 's#^postgres(ql)?://([^@/]*@)?([^:/]+).*#\3#')"
  port="$(printf '%s' "$admin_dsn" | sed -E 's#^postgres(ql)?://([^@/]*@)?[^:/]+:?([0-9]*)/.*#\3#')"
  [[ -z "$port" ]] && port=5432
  user="$(printf '%s' "$admin_dsn" | sed -E 's#^postgres(ql)?://([^:@/]+).*#\2#')"
  [[ "$user" == "postgres://"* || -z "$user" || "$user" == "$host" ]] && user="$(whoami)"

  log "postgres: admin=$admin_dsn host=$host port=$port"

  if [[ "${E2E_REUSE_DB:-0}" != "1" ]]; then
    log "postgres: (re)creating fresh chat + sched databases"
    psql "$admin_dsn" -v ON_ERROR_STOP=1 >/dev/null 2>&1 <<'SQL' || true
DROP DATABASE IF EXISTS fleet_e2e_chat;
DROP DATABASE IF EXISTS fleet_e2e_sched;
SQL
    psql "$admin_dsn" -v ON_ERROR_STOP=1 <<'SQL' >>"$LOG_DIR/pg.log" 2>&1
CREATE DATABASE fleet_e2e_chat;
CREATE DATABASE fleet_e2e_sched;
SQL
  fi

  CHAT_DSN="postgres://${host}:${port}/fleet_e2e_chat?sslmode=disable"
  SCHED_DSN="postgres://${host}:${port}/fleet_e2e_sched?sslmode=disable"
  # Preserve the admin DSN's user if it had one (e.g. postgres@).
  if printf '%s' "$admin_dsn" | grep -q '://[^/@]*@'; then
    local cred
    cred="$(printf '%s' "$admin_dsn" | sed -E 's#^postgres(ql)?://([^@]+)@.*#\2#')"
    CHAT_DSN="postgres://${cred}@${host}:${port}/fleet_e2e_chat?sslmode=disable"
    SCHED_DSN="postgres://${cred}@${host}:${port}/fleet_e2e_sched?sslmode=disable"
  fi
  log "postgres: chat DSN  = $CHAT_DSN"
  log "postgres: sched DSN = $SCHED_DSN"
}

# ── 2. build Go binaries ──
build_binaries() {
  log "build: go build fleet + fake-llm + fleet-admin"
  GOTOOLCHAIN=auto go build -o "$BIN_DIR/fleet" ./cmd/fleet        >>"$LOG_DIR/build.log" 2>&1 || die "go build fleet failed (see $LOG_DIR/build.log)"
  GOTOOLCHAIN=auto go build -o "$BIN_DIR/fake-llm" ./cmd/fake-llm  >>"$LOG_DIR/build.log" 2>&1 || die "go build fake-llm failed"
  GOTOOLCHAIN=auto go build -o "$BIN_DIR/fleet-admin" ./cmd/fleet-admin >>"$LOG_DIR/build.log" 2>&1 || die "go build fleet-admin failed"
}

# ── 3. ensure the sandbox image, then probe it ──
ensure_sandbox() {
  command -v podman >/dev/null 2>&1 || die "podman is required for the live sandbox"
  if ! podman image exists "$FLEET_SANDBOX_IMAGE" 2>/dev/null; then
    log "sandbox: image $FLEET_SANDBOX_IMAGE not present; building locally"
    IMAGE_NAME="${FLEET_SANDBOX_IMAGE%%:*}" "$REPO_ROOT/scripts/build-sandbox-image.sh" "${FLEET_SANDBOX_IMAGE##*:}" \
      >>"$LOG_DIR/sandbox-build.log" 2>&1 || die "sandbox image build failed (see $LOG_DIR/sandbox-build.log)"
  fi
  log "sandbox: probing $FLEET_SANDBOX_IMAGE (bash + run_python, normal + lockdown)"
  GOTOOLCHAIN=auto go build -o "$BIN_DIR/sandbox-probe" ./cmd/sandbox-probe >>"$LOG_DIR/build.log" 2>&1 \
    || die "go build sandbox-probe failed"
  mkdir -p "$WORKSPACE_BASE/probe-workspace" "$WORKSPACE_BASE/probe-bridge"
  chmod 0755 "$WORKSPACE_BASE/probe-workspace" "$WORKSPACE_BASE/probe-bridge" 2>/dev/null || true
  FLEET_SANDBOX_IMAGE="$FLEET_SANDBOX_IMAGE" \
  SANDBOX_WORKSPACE="$WORKSPACE_BASE/probe-workspace" \
  SANDBOX_BRIDGE_DIR="$WORKSPACE_BASE/probe-bridge" \
  SANDBOX_SUPPORTING="" \
    "$BIN_DIR/sandbox-probe" >>"$LOG_DIR/sandbox-probe.log" 2>&1 \
    || die "sandbox probe FAILED — the container sandbox is not working (see $LOG_DIR/sandbox-probe.log)"
  log "sandbox: probe OK"
}

# build_acp_image_if_stale IMAGE CONTAINERFILE SRCDIR LOGBASE — (re)build IMAGE
# when it is absent, when FLEET_ACP_E2E_REBUILD=1, or when any build input
# (SRCDIR, the Containerfile, or go.mod) is newer than the cached image. A stale
# local image would silently test OLD agent code — a real footgun: it fails
# confusingly, or false-passes on an additive change. On any inspect/find error
# we keep the existing image, so this can never make the boot stricter than
# before. (Deep transitive deps aren't tracked; FLEET_ACP_E2E_REBUILD=1 forces a
# rebuild.)
build_acp_image_if_stale() {
  local image="$1" containerfile="$2" srcdir="$3" logbase="$4"
  shift 4
  # Any remaining args are extra `podman build` flags (e.g. --build-arg).
  command -v podman >/dev/null 2>&1 || die "podman is required for the ACP e2e"
  local need_build=0 reason=""
  if ! podman image exists "$image" 2>/dev/null; then
    need_build=1 reason="not present"
  elif [[ "${FLEET_ACP_E2E_REBUILD:-}" == "1" ]]; then
    need_build=1 reason="FLEET_ACP_E2E_REBUILD=1"
  else
    local created
    created="$(podman image inspect "$image" --format '{{.Created}}' 2>/dev/null || true)"
    if [[ -n "$created" ]] && [[ -n "$(find "$srcdir" "$containerfile" \
        "$REPO_ROOT/go.mod" -newermt "$created" -print -quit 2>/dev/null)" ]]; then
      need_build=1 reason="source newer than image"
    fi
  fi
  if [[ "$need_build" == "1" ]]; then
    log "$logbase: building image $image ($reason)"
    podman build "$@" -f "$containerfile" -t "$image" "$REPO_ROOT" \
      >>"$LOG_DIR/$logbase-build.log" 2>&1 \
      || die "$logbase image build failed (see $LOG_DIR/$logbase-build.log)"
  fi
}

# ── 3b. ensure the external ACP example-agent image + drive it end-to-end ──
# The external proof: fleet's ExternalRuntime drives the coder-SDK-shaped,
# credential-free ACP example agent (cmd/acp-example-agent) over real
# podman-stdio — a turn streams + a permission request is handled (allow +
# default-deny). This is the GENERIC external path Claude Code / Goose ride. We
# build the image and run the Go live test (TestExternalPodmanE2E) as part of the
# live suite so it can never silently rot.
FLEET_ACP_EXTERNAL_E2E_IMAGE="${FLEET_ACP_EXTERNAL_E2E_IMAGE:-localhost/fleet-acp-example-agent:latest}"
ensure_acp_example_agent() {
  build_acp_image_if_stale "$FLEET_ACP_EXTERNAL_E2E_IMAGE" \
    "$REPO_ROOT/config/default/sandbox/Containerfile.acp-example-agent" \
    "$REPO_ROOT/cmd/acp-example-agent" "acp-example-agent"
  log "acp-example-agent: driving the external ACP path end-to-end (fleet ↔ example agent over podman-stdio)"
  FLEET_ACP_EXTERNAL_E2E_IMAGE="$FLEET_ACP_EXTERNAL_E2E_IMAGE" \
    go test ./internal/acpruntime/ -count=1 -run 'TestExternalPodmanE2E' \
    >>"$LOG_DIR/acp-external-e2e.log" 2>&1 \
    || die "external ACP e2e FAILED — fleet could not drive the example agent (see $LOG_DIR/acp-external-e2e.log)"
  log "acp-example-agent: external ACP e2e OK"
}

# ── 3c. ensure the native-agent image + run the native ACP podman e2e ──
# The native client proof: fleet's ClientRuntime spawns the native-agent image
# (cmd/fleet-native-agent) over podman-stdio; the in-container agent runs a real
# agentcore.Run loop, issues a governed bash tool call delegated back over
# `_fleet/tool`, and — critically — proves the #2 non-negotiable invariant: an
# MCP tool call succeeds via the HOST broker while NO MCP credential ever enters
# the container (TestPodmanE2E_*NoCredsInContainer). The in-container agent
# reaches the host-side fake LLM at host.containers.internal, which podman maps
# to the host in BOTH rootful (bridge gateway) and rootless (pasta/slirp4netns)
# networking — verified on rootless pasta — so this gate holds on the CI runner's
# rootless podman as well as locally. Set FLEET_ACP_E2E_HOST_IP to override.
FLEET_ACP_NATIVE_E2E_IMAGE="${FLEET_ACP_E2E_IMAGE:-localhost/fleet-native-agent:latest}"
ensure_native_agent() {
  # The native-agent image extends the sandbox base (Containerfile.native-agent's
  # SANDBOX_BASE arg, default localhost/fleet-sandbox:latest). Pass the RESOLVED
  # sandbox image so the build works when the live stack uses a non-local tag
  # (e.g. CI pulls ghcr.io/elcanotek/sandbox:latest) — otherwise the hardcoded
  # default base is absent locally and podman tries to pull "localhost/...".
  build_acp_image_if_stale "$FLEET_ACP_NATIVE_E2E_IMAGE" \
    "$REPO_ROOT/config/default/sandbox/Containerfile.native-agent" \
    "$REPO_ROOT/cmd/fleet-native-agent" "native-agent" \
    --build-arg "SANDBOX_BASE=$FLEET_SANDBOX_IMAGE"
  log "native-agent: driving the native ACP path end-to-end (credentials stay host-side)"
  FLEET_ACP_E2E_IMAGE="$FLEET_ACP_NATIVE_E2E_IMAGE" \
  FLEET_ACP_E2E_HOST_IP="${FLEET_ACP_E2E_HOST_IP:-host.containers.internal}" \
    go test ./internal/acpruntime/ -count=1 -run 'TestPodmanE2E' \
    >>"$LOG_DIR/acp-native-e2e.log" 2>&1 \
    || die "native ACP e2e FAILED — fleet could not drive the native agent or a credential leaked into the container (see $LOG_DIR/acp-native-e2e.log)"
  log "native-agent: native ACP e2e OK (incl. no-creds-in-container proof)"
}

# ── 4. start fake-llm (skipped in canary mode) ──
start_fake_llm() {
  if [[ "$E2E_CANARY" == "1" ]]; then
    log "fake-llm: SKIPPED (canary mode → real OpenRouter)"
    return
  fi
  log "fake-llm: starting on $FAKE_LLM_ADDR"
  "$BIN_DIR/fake-llm" -addr "$FAKE_LLM_ADDR" >"$LOG_DIR/fake-llm.log" 2>&1 &
  PIDS+=("$!")
  wait_http "http://$FAKE_LLM_ADDR/healthz" 30 "fake-llm"
}

# ── 5. boot fleet (both listeners) ──
start_fleet() {
  mkdir -p "$DATA_DIR" "$WORKSPACE_DIR"
  # LLM wiring: fake by default (deterministic, free); REAL OpenRouter in canary.
  local llm_key llm_base default_model
  if [[ "$E2E_CANARY" == "1" ]]; then
    [[ -n "${OPENROUTER_API_KEY:-}" ]] || die "canary mode requires a real OPENROUTER_API_KEY in env"
    llm_key="$OPENROUTER_API_KEY"
    llm_base=""  # empty → fleet uses the hardcoded upstream OpenRouter URL
    default_model="$CANARY_MODEL"
    log "fleet: booting chat :$CHAT_PORT + orchestrator :$ORCH_PORT (LLM → REAL OpenRouter, model=$CANARY_MODEL)"
  else
    llm_key="$FAKE_OPENROUTER_KEY"
    llm_base="http://$FAKE_LLM_ADDR"
    default_model="anthropic/claude-opus-4.8"
    log "fleet: booting chat :$CHAT_PORT + orchestrator :$ORCH_PORT (LLM → fake)"
  fi
  FLEET_CLIENT_CONFIG_DIR="$REPO_ROOT/config/default" \
  FLEET_SERVER_ADDR="127.0.0.1:$CHAT_PORT" \
  FLEET_ORCHESTRATOR_ADDR="127.0.0.1:$ORCH_PORT" \
  FLEET_CHAT_DATABASE_URL="$CHAT_DSN" \
  FLEET_SCHED_DATABASE_URL="$SCHED_DSN" \
  DATABASE_URL="$SCHED_DSN" \
  OPENROUTER_API_KEY="$llm_key" \
  OPENROUTER_BASE_URL="$llm_base" \
  FLEET_SANDBOX_IMAGE="$FLEET_SANDBOX_IMAGE" \
  FLEET_WORKSPACE_ROOT="$WORKSPACE_DIR" \
  FLEET_DATA_DIR="$DATA_DIR" \
  FLEET_SERVER_TOKEN="$FLEET_SERVER_TOKEN" \
  ADMIN_API_KEY="$ADMIN_API_KEY" \
  AUTH_SIGNING_PUBKEY="$AUTH_SIGNING_PUBKEY" \
  FLEET_TITLE_MODEL="$default_model" \
  CUTLASS_TASK_MODEL="$default_model" \
  FLEET_TIMEZONE="UTC" \
    "$BIN_DIR/fleet" >"$LOG_DIR/fleet.log" 2>&1 &
  PIDS+=("$!")
  wait_http "http://127.0.0.1:$CHAT_PORT/healthz" 90 "fleet chat"
  wait_http "http://127.0.0.1:$ORCH_PORT/health" 90 "fleet orchestrator"
}

# ── seed test users (chat + sched) ──
seed_users() {
  log "seed: chat user $E2E_TEST_EMAIL + sched admin $E2E_SCHED_USERNAME"
  FLEET_CHAT_DATABASE_URL="$CHAT_DSN" \
    "$BIN_DIR/fleet-admin" chat user add "$E2E_TEST_EMAIL" --password - <<<"$E2E_TEST_PASSWORD" \
    >>"$LOG_DIR/seed.log" 2>&1 || log "seed: chat user may already exist (continuing)"
  FLEET_SCHED_DATABASE_URL="$SCHED_DSN" \
    "$BIN_DIR/fleet-admin" sched user add "$E2E_SCHED_USERNAME" --role admin --password - <<<"$E2E_TEST_PASSWORD" \
    >>"$LOG_DIR/seed.log" 2>&1 || log "seed: sched user may already exist (continuing)"
}

# ── 6. web app: build + next start ──
start_web() {
  [[ "${E2E_SKIP_WEB:-0}" == "1" ]] && { log "web: skipped (E2E_SKIP_WEB=1)"; return; }
  log "web: npm ci + next build (this is the slow step)"
  ( cd "$REPO_ROOT/web" && [[ -d node_modules ]] || npm ci >>"$LOG_DIR/web-build.log" 2>&1 ) || die "npm ci failed"
  ( cd "$REPO_ROOT/web" && npm run build >>"$LOG_DIR/web-build.log" 2>&1 ) || die "next build failed (see $LOG_DIR/web-build.log)"
  log "web: next start on :$NEXT_PORT"
  ( cd "$REPO_ROOT/web" && \
    CHAT_MOCK_MODE="" \
    APP_SESSION_SECRET="$APP_SESSION_SECRET" \
    AUTH_SIGNING_PUBKEY="$AUTH_SIGNING_PUBKEY" \
    CHAT_SERVER_URL="http://127.0.0.1:$CHAT_PORT" \
    CHAT_SERVER_TOKEN="$FLEET_SERVER_TOKEN" \
    ORCHESTRATOR_SERVER_URL="http://127.0.0.1:$ORCH_PORT" \
      npx next start -p "$NEXT_PORT" -H 127.0.0.1 >"$LOG_DIR/web.log" 2>&1 ) &
  PIDS+=("$!")
  wait_http "http://127.0.0.1:$NEXT_PORT/login" 90 "web (next start)"
}

# ── write the env Playwright + specs need ──
write_env() {
  cat >"$E2E_ENV_FILE" <<EOF
# Generated by scripts/e2e-boot-server.sh — resolved live-stack endpoints.
E2E_LIVE=1
NEXT_PORT=$NEXT_PORT
E2E_BASE_URL=http://127.0.0.1:$NEXT_PORT
CHAT_PORT=$CHAT_PORT
ORCH_PORT=$ORCH_PORT
FAKE_LLM_ADDR=$FAKE_LLM_ADDR
CHAT_SERVER_URL=http://127.0.0.1:$CHAT_PORT
ORCHESTRATOR_SERVER_URL=http://127.0.0.1:$ORCH_PORT
CHAT_SERVER_TOKEN=$FLEET_SERVER_TOKEN
ADMIN_API_KEY=$ADMIN_API_KEY
APP_SESSION_SECRET=$APP_SESSION_SECRET
AUTH_SIGNING_PUBKEY=$AUTH_SIGNING_PUBKEY
E2E_TEST_EMAIL=$E2E_TEST_EMAIL
E2E_TEST_PASSWORD=$E2E_TEST_PASSWORD
E2E_SCHED_USERNAME=$E2E_SCHED_USERNAME
FLEET_SANDBOX_IMAGE=$FLEET_SANDBOX_IMAGE
EOF
  log "env: wrote $E2E_ENV_FILE"
  log "==== LIVE STACK READY ===="
  cat "$E2E_ENV_FILE" >&2
}

main() {
  ensure_postgres
  build_binaries
  ensure_sandbox
  ensure_acp_example_agent
  ensure_native_agent
  start_fake_llm
  start_fleet
  seed_users
  start_web
  write_env

  case "${1:-}" in
    --print-env-only)
      # Used by callers that manage the lifecycle externally; this script still
      # holds the procs, so this mode just blocks like --serve.
      ;;
  esac

  log "stack up. Blocking until signalled (Ctrl-C to tear down)."
  # Block forever; the EXIT trap tears everything down on signal.
  while true; do sleep 3600 & wait $!; done
}

main "$@"
