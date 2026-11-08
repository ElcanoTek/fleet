#!/usr/bin/env bash
# scripts/upgrade-from-release.sh — rehearse a box's upgrade from a published
# release to this checkout's fleet (#2049 M6; the CI job upgrade-from-release).
#
#   scripts/upgrade-from-release.sh OLD_FLEET NEW_FLEET ADMIN_DSN
#
# OLD_FLEET is the previous release's verified binary, NEW_FLEET this build,
# ADMIN_DSN a postgres:// URL to any database of a server whose role may
# CREATE DATABASE. Two scratch databases are created and dropped:
#
#   1. fleet_upgrade: OLD_FLEET boots on it (so its runners migrate it, as a
#      box on that release is), and is seeded — accounts through its CLI, a
#      task through its API, a chat schedule and its conversation by SQL (no
#      CLI writes those) — then stopped;
#   2. NEW_FLEET boots on it, and the script asserts: `fleet migrate status
#      --json` reports the one ledger adopted, current, clean and unified;
#      the schema (chat, core, sched, minus the migration ledgers) equals a
#      fresh install by NEW_FLEET (fleet_upgrade_fresh); the seeded rows are
#      intact; `fleet doctor` (its in-process fallback, `fleet status`) passes
#      its migrations line; and GET /tasks answers with the seeded task.
#
# psql and pg_dump must match the server's major version. Set
# FLEET_UPGRADE_PG_CONTAINER to run them inside that Postgres container
# (docker exec) instead of from PATH.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 OLD_FLEET NEW_FLEET ADMIN_DSN" >&2
  exit 2
fi
OLD_FLEET="$(readlink -f "$1")"
NEW_FLEET="$(readlink -f "$2")"
ADMIN_DSN="$3"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CHAT_PORT="${CHAT_PORT:-18480}"
ORCH_PORT="${ORCH_PORT:-18400}"
ADMIN_API_KEY="upgrade-rehearsal-admin-key"
WORK="$(mktemp -d)"
LOGS="${FLEET_UPGRADE_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS" "$WORK/workspace" "$WORK/data"
SERVER_PID=""

log() { printf '[upgrade] %s\n' "$*" >&2; }
die() { printf '[upgrade] FAIL: %s\n' "$*" >&2; exit 1; }

# dsn_for DB — ADMIN_DSN pointed at database DB (query string kept).
dsn_for() {
  local base="${ADMIN_DSN%%\?*}" query=""
  [[ "$ADMIN_DSN" == *\?* ]] && query="?${ADMIN_DSN#*\?}"
  printf '%s/%s%s' "${base%/*}" "$1" "$query"
}

# pg TOOL DB ARGS... — psql or pg_dump against DB, locally or in the container.
pg() {
  local tool="$1" db="$2"
  shift 2
  if [[ -n "${FLEET_UPGRADE_PG_CONTAINER:-}" ]]; then
    local user
    user="$(sed -E 's#^postgres(ql)?://([^:@/]+).*#\2#' <<<"$ADMIN_DSN")"
    docker exec -i "$FLEET_UPGRADE_PG_CONTAINER" "$tool" -U "$user" -d "$db" "$@"
  else
    "$tool" "$(dsn_for "$db")" "$@"
  fi
}
sql() { pg psql "$1" -X -v ON_ERROR_STOP=1 -qAt -c "$2"; }

UP_DB=fleet_upgrade
FRESH_DB=fleet_upgrade_fresh

stop_server() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 60); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.5; done
    kill -9 "$SERVER_PID" 2>/dev/null || true
  fi
  SERVER_PID=""
}
cleanup() {
  local ec=$?
  stop_server
  if (( ec != 0 )); then
    for f in "$LOGS"/*.log; do
      [[ -f "$f" ]] || continue
      echo "──── $f (last 60 lines)" >&2
      tail -n 60 "$f" >&2 || true
    done
  fi
  sql postgres "DROP DATABASE IF EXISTS $UP_DB WITH (FORCE)" >/dev/null 2>&1 || true
  sql postgres "DROP DATABASE IF EXISTS $FRESH_DB WITH (FORCE)" >/dev/null 2>&1 || true
  [[ -n "${FLEET_UPGRADE_LOG_DIR:-}" ]] || rm -rf "$WORK"
  exit "$ec"
}
trap cleanup EXIT INT TERM

# fleet_env DB — the environment a fleet process runs with on database DB.
fleet_env() {
  printf '%s\n' \
    "DATABASE_URL=$(dsn_for "$1")" \
    "FLEET_CLIENT_CONFIG_DIR=$REPO_ROOT/config/default" \
    "FLEET_SERVER_ADDR=127.0.0.1:$CHAT_PORT" \
    "FLEET_ORCHESTRATOR_ADDR=127.0.0.1:$ORCH_PORT" \
    "FLEET_WEB_ADDR=off" \
    "APP_SESSION_SECRET=upgrade-rehearsal-session-secret-0123456789" \
    "ADMIN_API_KEY=$ADMIN_API_KEY" \
    "AUTH_SIGNING_PUBKEY=P8Lyn/xy8bS2SWcnPkg2kKiGBctyJZdEMMzNEixh0As=" \
    "OPENROUTER_API_KEY=sk-or-fake-upgrade-rehearsal" \
    "OPENROUTER_BASE_URL=http://127.0.0.1:9" \
    "FLEET_DEFAULT_MODEL=anthropic/claude-opus-4.8" \
    "FLEET_TITLE_MODEL=anthropic/claude-opus-4.8" \
    "FLEET_TASK_MODEL=anthropic/claude-opus-4.8" \
    "FLEET_SANDBOX_IMAGE=localhost/fleet-sandbox:upgrade-rehearsal" \
    "FLEET_WORKSPACE_ROOT=$WORK/workspace" \
    "FLEET_DATA_DIR=$WORK/data" \
    "FLEET_TIMEZONE=UTC"
}

# run_fleet BIN DB ARGS... — one CLI verb against DB.
run_fleet() {
  local bin="$1" db="$2"
  shift 2
  local -a envs
  mapfile -t envs < <(fleet_env "$db")
  (cd "$WORK" && env "${envs[@]}" FLEET_ROOT="$WORK/no-checkout" "$bin" "$@")
}

wait_http() { # URL SECONDS
  local deadline=$((SECONDS + $2))
  while (( SECONDS < deadline )); do
    curl -fsS -o /dev/null --max-time 3 "$1" 2>/dev/null && return 0
    kill -0 "$SERVER_PID" 2>/dev/null || die "the server exited before $1 answered"
    sleep 1
  done
  die "$1 did not answer within $2s"
}

# boot BIN DB LABEL — start BIN's server on DB and wait for both planes.
boot() {
  local -a envs
  mapfile -t envs < <(fleet_env "$2")
  (cd "$WORK" && exec env "${envs[@]}" "$1") >"$LOGS/$3.log" 2>&1 &
  SERVER_PID=$!
  wait_http "http://127.0.0.1:$CHAT_PORT/healthz" 180
  wait_http "http://127.0.0.1:$ORCH_PORT/health" 180
  log "$3 is up (pid $SERVER_PID)"
}

# schema DB — chat, core and sched as pg_dump writes them, minus the migration
# ledgers (the bookkeeping that differs by history).
schema() {
  pg pg_dump "$1" --schema-only --no-owner --no-acl -n chat -n core -n sched \
    -T chat.schema_migrations -T sched.schema_migrations -T core.schema_versions \
    | grep -vE '^(--|SET |SELECT pg_catalog.set_config|\\(un)?restrict )' | sed '/^$/d'
}

log "old: $("$OLD_FLEET" version 2>/dev/null | head -1 || echo unknown)"
log "new: $("$NEW_FLEET" version 2>/dev/null | head -1 || echo unknown)"
for db in "$UP_DB" "$FRESH_DB"; do
  sql postgres "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null
  sql postgres "CREATE DATABASE $db" >/dev/null
done

# ── 1. the previous release migrates and serves; seed it ───────────────────
boot "$OLD_FLEET" "$UP_DB" old-serve
run_fleet "$OLD_FLEET" "$UP_DB" chat user add alice@example.com --password - <<<"alice-password-1" >>"$LOGS/seed.log" 2>&1
run_fleet "$OLD_FLEET" "$UP_DB" chat user role alice@example.com --role admin >>"$LOGS/seed.log" 2>&1
run_fleet "$OLD_FLEET" "$UP_DB" sched user add ops@example.com --role client --password - <<<"ops-password-1" >>"$LOGS/seed.log" 2>&1 || true
# Scheduled a month out, so neither server tries to run it.
due="$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)"
task_json="$(curl -fsS --retry 3 --retry-all-errors --connect-timeout 5 -X POST \
  -H "X-API-Key: $ADMIN_API_KEY" -H 'Content-Type: application/json' \
  -d "{\"title\":\"upgrade canary\",\"prompt\":\"Report the pacing.\",\"scheduled_for\":\"$due\"}" \
  "http://127.0.0.1:$ORCH_PORT/tasks")"
task_id="$(jq -er '.id' <<<"$task_json")" || die "POST /tasks returned no id: $task_json"
log "seeded task $task_id"
stop_server
# A chat schedule and its conversation, wherever this release keeps them.
sched_schema="$(sql "$UP_DB" "SELECT CASE WHEN to_regclass('sched.chat_schedules') IS NOT NULL THEN 'sched' ELSE 'chat' END")"
users_table="$(sql "$UP_DB" "SELECT CASE WHEN to_regclass('core.users') IS NOT NULL THEN 'core.users' ELSE 'chat.users' END")"
sql "$UP_DB" "INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('conv-upgrade', 'alice@example.com', 'Deals', 1700000000, 1700000000)"
sql "$UP_DB" "INSERT INTO ${sched_schema}.chat_schedules (id, conversation_id, user_email, title, prompt, cron_expr, timezone, status, next_run_at, created_at, updated_at)
  VALUES ('sched-upgrade', 'conv-upgrade', 'alice@example.com', 'Sweep', 'Check pacing.', '0 9 * * *', 'UTC', 'active', 4070908800, 1700000000, 1700000000)"
before="$(sql "$UP_DB" "SELECT (SELECT count(*) FROM $users_table WHERE email = 'alice@example.com'),
  (SELECT count(*) FROM sched.tasks WHERE id = '$task_id'), (SELECT count(*) FROM chat.conversations WHERE id = 'conv-upgrade'),
  (SELECT count(*) FROM ${sched_schema}.chat_schedules WHERE id = 'sched-upgrade')")"
[[ "$before" == "1|1|1|1" ]] || die "seeding failed: $before"
log "seeded (users in $users_table, chat schedules in $sched_schema)"

# ── 2. this build boots on it ──────────────────────────────────────────────
boot "$NEW_FLEET" "$UP_DB" new-serve
status="$(run_fleet "$NEW_FLEET" "$UP_DB" migrate status --json)"
printf '%s\n' "$status" >"$LOGS/migrate-status.json"
jq -e '.adopted == true and .dirty == false and .layout == "unified"
  and (.pending | length) == 0 and .current_version != null
  and .migration_table == "core.schema_versions"
  and (.sched.current_version // 0) >= 77' <<<"$status" >/dev/null \
  || die "fleet migrate status --json after the upgrade: $status"
log "migrate status: adopted, at $(jq -r .current_version <<<"$status"), nothing pending"

tasks="$(curl -fsS --retry 3 --retry-all-errors --connect-timeout 5 -H "X-API-Key: $ADMIN_API_KEY" "http://127.0.0.1:$ORCH_PORT/tasks")"
grep -q "$task_id" <<<"$tasks" || die "GET /tasks does not list the seeded task: $tasks"
log "GET /tasks lists the seeded task"

after="$(sql "$UP_DB" "SELECT (SELECT count(*) FROM core.users WHERE email IN ('alice@example.com')),
  (SELECT count(*) FROM sched.tasks WHERE id = '$task_id'), (SELECT count(*) FROM chat.conversations WHERE id = 'conv-upgrade'),
  (SELECT count(*) FROM sched.chat_schedules WHERE id = 'sched-upgrade')")"
[[ "$after" == "1|1|1|1" ]] || die "seeded rows after the upgrade: $after (users|task|conversation|chat schedule)"
log "seeded rows intact"

doctor="$(run_fleet "$NEW_FLEET" "$UP_DB" doctor --check 2>&1 || true)"
printf '%s\n' "$doctor" >"$LOGS/doctor.log"
grep -qE '✓ migrations +migrations: version [0-9]+, [0-9]+ applied, 0 pending, clean; adopted' <<<"$doctor" \
  || die "fleet doctor does not pass its migrations line (see $LOGS/doctor.log)"
log "fleet doctor: migrations adopted and current"
stop_server

# ── 3. the schema equals a fresh install by this build ─────────────────────
boot "$NEW_FLEET" "$FRESH_DB" fresh-serve
stop_server
if ! diff <(schema "$FRESH_DB") <(schema "$UP_DB") >"$LOGS/schema.diff"; then
  head -n 80 "$LOGS/schema.diff" >&2
  die "the upgraded schema differs from a fresh install (< fresh, > upgraded)"
fi
log "schema equals a fresh install ($(schema "$FRESH_DB" | wc -l) lines)"
log "OK"
