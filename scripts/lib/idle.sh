#!/usr/bin/env bash
# scripts/lib/idle.sh — active-work inspection and drain helpers for fleet update.
#
# Checks the running deployment's Postgres databases (sched tasks, chat turns,
# and detached approval executions) without sourcing the 0600 env file and
# without placing database passwords on psql's command line.

# fleet_split_dsn_password DSN — prints the password on line 1 and the DSN
# with `:password@` stripped to `@` on line 2. When the DSN has no userinfo
# password, line 1 is empty and line 2 is the original DSN.
fleet_split_dsn_password() {
  local dsn="$1" proto rest userinfo hostpart user pass
  if [[ "$dsn" != *"://"* || "$dsn" != *"@"* ]]; then
    printf '\n%s\n' "$dsn"
    return 0
  fi
  proto="${dsn%%://*}"
  rest="${dsn#*://}"
  userinfo="${rest%%@*}"
  hostpart="${rest#*@}"
  if [[ "$userinfo" != *":"* ]]; then
    printf '\n%s\n' "$dsn"
    return 0
  fi
  user="${userinfo%%:*}"
  pass="${userinfo#*:}"
  printf '%s\n%s://%s@%s\n' "$pass" "$proto" "$user" "$hostpart"
}

# fleet_psql_query DSN SQL — runs a read-only psql query with a 3s connect
# timeout and the password passed via PGPASSWORD rather than argv. Returns 0
# with trimmed output on success, non-zero when psql is absent or the query
# fails.
fleet_psql_query() {
  local dsn="$1" sql="$2" pw="" redacted=""
  [[ -n "$dsn" ]] || return 1
  command -v psql >/dev/null 2>&1 || return 1
  {
    IFS= read -r pw
    IFS= read -r redacted
  } < <(fleet_split_dsn_password "$dsn")
  PGCONNECT_TIMEOUT=3 PGPASSWORD="$pw" psql "$redacted" -X -q -t -A -c "$sql" 2>/dev/null
}

# fleet_resolve_dsn PRIMARY_KEY ENV_FILE — resolves PRIMARY_KEY (falling back
# to DATABASE_URL) from the process environment first, then ENV_FILE via
# env_get (when defined) or grep.
fleet_resolve_dsn() {
  local key="$1" env_file="${2:-/etc/fleet/fleet.env}" val=""
  val="${!key:-}"
  if [[ -z "$val" && -n "${DATABASE_URL:-}" ]]; then
    val="$DATABASE_URL"
  fi
  if [[ -z "$val" && -r "$env_file" ]]; then
    if declare -F env_get >/dev/null 2>&1; then
      val="$(env_get "$key" "$env_file")"
      [[ -n "$val" ]] || val="$(env_get DATABASE_URL "$env_file")"
    else
      val="$(grep -E "^${key}=" "$env_file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
      [[ -n "$val" ]] || val="$(grep -E "^DATABASE_URL=" "$env_file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
    fi
  fi
  printf '%s' "$val"
}

# fleet_active_work_summary ENV_FILE — inspects sched.tasks, chat.turns, and
# chat.approvals for in-flight work.
#
# Prints a semicolon-separated description and returns 1 when work is active;
# prints nothing and returns 0 when idle (or when Postgres/tables are not yet
# reachable, so a stopped or unprovisioned box is never wedged).
fleet_active_work_summary() {
  local env_file="${1:-/etc/fleet/fleet.env}"
  local sched_dsn chat_dsn turn_window approval_window out items=()
  turn_window="${FLEET_IDLE_TURN_WINDOW_SECONDS:-900}"
  approval_window="${FLEET_IDLE_APPROVAL_WINDOW_SECONDS:-7200}"
  [[ "$turn_window" =~ ^[0-9]+$ ]] || turn_window=900
  [[ "$approval_window" =~ ^[0-9]+$ ]] || approval_window=7200

  sched_dsn="$(fleet_resolve_dsn FLEET_SCHED_DATABASE_URL "$env_file")"
  chat_dsn="$(fleet_resolve_dsn FLEET_CHAT_DATABASE_URL "$env_file")"

  if [[ -n "$sched_dsn" ]]; then
    out="$(fleet_psql_query "$sched_dsn" "
      SELECT CASE WHEN to_regclass('public.tasks') IS NULL THEN ''
             ELSE COALESCE((
               SELECT string_agg(
                 format('scheduled task %s (%s, %s, %ss)',
                   id::text,
                   COALESCE(NULLIF(regexp_replace(title, '[[:space:]]+', ' ', 'g'), ''), 'untitled'),
                   status,
                   GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, updated_at, created_at)))::bigint)
                 ), '; ')
               FROM (
                 SELECT id, title, status, started_at, updated_at, created_at
                   FROM tasks
                  WHERE status IN ('running', 'leased', 'assigned')
                  ORDER BY COALESCE(started_at, updated_at, created_at) ASC
                  LIMIT 5
               ) t
             ), '')
             END;" || true)"
    out="${out#"${out%%[![:space:]]*}"}"
    out="${out%"${out##*[![:space:]]}"}"
    [[ -n "$out" ]] && items+=("$out")
  fi

  if [[ -n "$chat_dsn" ]]; then
    out="$(fleet_psql_query "$chat_dsn" "
      SELECT CASE WHEN to_regclass('public.turns') IS NULL THEN ''
             ELSE COALESCE((
               SELECT string_agg(
                 format('chat turn %s (conv %s, %ss)',
                   turn_id,
                   conversation_id,
                   GREATEST(0, EXTRACT(EPOCH FROM now())::bigint - started_at)
                 ), '; ')
               FROM (
                 SELECT turn_id, conversation_id, started_at
                   FROM turns
                  WHERE status = 'running'
                    AND finished_at IS NULL
                    AND started_at > EXTRACT(EPOCH FROM now())::bigint - ${turn_window}
                  ORDER BY started_at ASC
                  LIMIT 5
               ) t
             ), '')
             END;" || true)"
    out="${out#"${out%%[![:space:]]*}"}"
    out="${out%"${out##*[![:space:]]}"}"
    [[ -n "$out" ]] && items+=("$out")

    out="$(fleet_psql_query "$chat_dsn" "
      SELECT CASE WHEN to_regclass('public.approvals') IS NULL THEN ''
             ELSE COALESCE((
               SELECT string_agg(
                 format('approval execution %s (%s, %ss)',
                   id,
                   tool_name,
                   GREATEST(0, EXTRACT(EPOCH FROM now())::bigint - COALESCE(resolved_at, created_at))
                 ), '; ')
               FROM (
                 SELECT id, tool_name, resolved_at, created_at
                   FROM approvals
                  WHERE status = 'approved'
                    AND is_err IS NULL
                    AND result_text LIKE 'Approved%executing%'
                    AND COALESCE(resolved_at, created_at) > EXTRACT(EPOCH FROM now())::bigint - ${approval_window}
                  ORDER BY COALESCE(resolved_at, created_at) ASC
                  LIMIT 5
               ) a
             ), '')
             END;" || true)"
    out="${out#"${out%%[![:space:]]*}"}"
    out="${out%"${out##*[![:space:]]}"}"
    [[ -n "$out" ]] && items+=("$out")
  fi

  if (( ${#items[@]} == 0 )); then
    return 0
  fi
  local IFS="; "
  printf '%s\n' "${items[*]}"
  return 1
}

# fleet_wait_for_idle ENV_FILE MAX_WAIT_SECONDS POLL_SECONDS — polls
# fleet_active_work_summary until the deployment is idle (returns 0) or
# MAX_WAIT_SECONDS elapses (prints the remaining active work on stdout and
# returns 1).
fleet_wait_for_idle() {
  local env_file="$1" max_wait="${2:-0}" poll="${3:-5}"
  [[ "$max_wait" =~ ^[0-9]+$ ]] || max_wait=0
  [[ "$poll" =~ ^[0-9]+$ ]] && (( poll > 0 )) || poll=5
  local start now elapsed summary=""
  start="$(date +%s)"
  while true; do
    if summary="$(fleet_active_work_summary "$env_file")"; then
      return 0
    fi
    now="$(date +%s)"
    elapsed=$(( now - start ))
    if (( max_wait <= 0 || elapsed >= max_wait )); then
      printf '%s\n' "$summary"
      return 1
    fi
    if declare -F info >/dev/null 2>&1; then
      info "waiting for active work to finish (${elapsed}s/${max_wait}s): ${summary}" >&2
    fi
    sleep "$poll"
  done
}
