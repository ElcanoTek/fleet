#!/usr/bin/env bash
# scripts/lib/idle.sh — active-work inspection and drain helpers for fleet update.
#
# Checks the running deployment's Postgres databases (sched tasks, chat turns,
# and detached approval executions) without sourcing the 0600 env file and
# without placing database passwords on psql's command line.

# fleet_uri_decode STR — decodes RFC 3986 percent-encoded bytes (%XX) in STR.
fleet_uri_decode() {
  local s="$1" out="" i=0 len=${#1} c hex
  while (( i < len )); do
    c="${s:i:1}"
    if [[ "$c" == "%" && $(( i + 2 )) -lt $len ]]; then
      hex="${s:i+1:2}"
      if [[ "$hex" =~ ^[0-9A-Fa-f]{2}$ ]]; then
        printf -v c "\\x$hex"
        out+="$c"
        i=$(( i + 3 ))
        continue
      fi
    fi
    out+="$c"
    i=$(( i + 1 ))
  done
  printf '%s' "$out"
}

# fleet_split_dsn_password DSN — prints the percent-decoded password on line 1
# and the DSN with `:password@` stripped to `@` on line 2. When the DSN has no
# userinfo password, line 1 is empty and line 2 is the original DSN.
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
  pass="$(fleet_uri_decode "${userinfo#*:}")"
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

# fleet_resolve_dsn PRIMARY_KEY ENV_FILE — resolves PRIMARY_KEY from the
# process environment first and then ENV_FILE before falling back to
# DATABASE_URL from the process environment or ENV_FILE.
fleet_resolve_dsn() {
  local key="$1" env_file="${2:-/etc/fleet/fleet.env}" val=""
  val="${!key:-}"
  if [[ -z "$val" && -r "$env_file" ]]; then
    if declare -F env_get >/dev/null 2>&1; then
      val="$(env_get "$key" "$env_file")"
    else
      val="$(grep -E "^${key}=" "$env_file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
    fi
  fi
  if [[ -z "$val" && -n "${DATABASE_URL:-}" ]]; then
    val="$DATABASE_URL"
  fi
  if [[ -z "$val" && -r "$env_file" ]]; then
    if declare -F env_get >/dev/null 2>&1; then
      val="$(env_get DATABASE_URL "$env_file")"
    else
      val="$(grep -E "^DATABASE_URL=" "$env_file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
    fi
  fi
  printf '%s' "$val"
}

# fleet_drain_file [ENV_FILE] — returns the drain sentinel file path inspected
# by the running fleet server and scheduler pool.
fleet_drain_file() {
  local env_file="${1:-/etc/fleet/fleet.env}"
  if [[ -n "${FLEET_UPDATE_DRAIN_FILE:-}" ]]; then
    printf '%s' "$FLEET_UPDATE_DRAIN_FILE"
    return 0
  fi
  if [[ -n "${FLEET_DRAIN_FILE:-}" ]]; then
    printf '%s' "$FLEET_DRAIN_FILE"
    return 0
  fi
  printf '%s.update.drain' "$env_file"
}

# fleet_set_drain [ENV_FILE] — creates or refreshes the update drain sentinel
# file so the running server refuses new chat turns/approvals and pauses
# scheduler task claims.
fleet_set_drain() {
  local drain_file
  drain_file="$(fleet_drain_file "${1:-/etc/fleet/fleet.env}")"
  mkdir -p "$(dirname "$drain_file")" 2>/dev/null || true
  printf 'draining\n' > "$drain_file" 2>/dev/null || true
}

# fleet_clear_drain [ENV_FILE] — removes the update drain sentinel file.
fleet_clear_drain() {
  local drain_file
  drain_file="$(fleet_drain_file "${1:-/etc/fleet/fleet.env}")"
  rm -f "$drain_file" 2>/dev/null || true
}

# fleet_service_runtime_state [SERVICE_NAME] — prints "<state>|<start_epoch>"
# where <state> is "active", "inactive" (unit installed and not active), or
# "unknown" (no systemd or unit not installed).
fleet_service_runtime_state() {
  local unit="${1:-${SERVICE_NAME:-${FLEET_SERVICE_NAME:-fleet}}}"
  [[ "$unit" == *.service ]] || unit="${unit}.service"
  if [[ "${FLEET_ASSUME_SERVICE_ACTIVE:-}" == "1" ]]; then
    printf 'active|0\n'
    return 0
  fi
  if ! command -v systemctl >/dev/null 2>&1; then
    printf 'unknown|0\n'
    return 0
  fi
  if ! systemctl cat "$unit" >/dev/null 2>&1; then
    printf 'unknown|0\n'
    return 0
  fi
  local active_state start_ts start_epoch=0
  active_state="$(systemctl show -p ActiveState --value "$unit" 2>/dev/null | tr -d '[:space:]' || true)"
  if [[ "$active_state" == "active" ]]; then
    start_ts="$(systemctl show -p ExecMainStartTimestamp --value "$unit" 2>/dev/null || true)"
    if [[ -n "$start_ts" && "$start_ts" != "n/a" ]]; then
      start_epoch="$(date -d "$start_ts" +%s 2>/dev/null || echo 0)"
      [[ "$start_epoch" =~ ^[0-9]+$ ]] || start_epoch=0
    fi
    printf 'active|%s\n' "$start_epoch"
    return 0
  fi
  if [[ -n "$active_state" ]]; then
    printf 'inactive|0\n'
    return 0
  fi
  printf 'unknown|0\n'
}

# fleet_sanitize_summary STR — strips ASCII control characters so untrusted
# task titles or identifiers cannot inject terminal escape sequences.
fleet_sanitize_summary() {
  printf '%s' "$1" | LC_ALL=C tr -d '[:cntrl:]'
}

# fleet_active_work_summary ENV_FILE [SERVICE_NAME] — inspects sched.tasks,
# chat.turns, and chat.approvals for in-flight work.
#
# Prints a semicolon-separated description and returns 1 when work is active;
# prints nothing and returns 0 when idle (or when the service is not running so
# crash-stranded rows can be recovered on start).
fleet_active_work_summary() {
  local env_file="${1:-/etc/fleet/fleet.env}"
  local svc_unit="${2:-${SERVICE_NAME:-${FLEET_SERVICE_NAME:-fleet}}}"
  local sched_dsn chat_dsn approval_window out items=()
  local svc_info svc_state="unknown" svc_start_epoch=0
  approval_window="${FLEET_IDLE_APPROVAL_WINDOW_SECONDS:-7200}"
  [[ "$approval_window" =~ ^[0-9]+$ ]] || approval_window=7200

  svc_info="$(fleet_service_runtime_state "$svc_unit")"
  svc_state="${svc_info%%|*}"
  svc_start_epoch="${svc_info#*|}"
  [[ "$svc_start_epoch" =~ ^[0-9]+$ ]] || svc_start_epoch=0

  # When systemd reports that fleet.service is installed and not active, no
  # in-process task, turn, or approval goroutine is running; any DB rows still
  # marked running/leased are crash-stranded and will be recovered on boot.
  if [[ "$svc_state" == "inactive" ]]; then
    return 0
  fi

  sched_dsn="$(fleet_resolve_dsn FLEET_SCHED_DATABASE_URL "$env_file")"
  chat_dsn="$(fleet_resolve_dsn FLEET_CHAT_DATABASE_URL "$env_file")"

  if [[ -n "$sched_dsn" ]]; then
    if out="$(fleet_psql_query "$sched_dsn" "
      SELECT CASE WHEN to_regclass('public.tasks') IS NULL THEN ''
             ELSE COALESCE((
               SELECT string_agg(
                 format('scheduled task %s (%s, %s, %ss)',
                   id::text,
                   COALESCE(NULLIF(regexp_replace(regexp_replace(title, '[[:cntrl:]]+', ' ', 'g'), '[[:space:]]+', ' ', 'g'), ''), 'untitled'),
                   status,
                   GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at)))::bigint)
                 ), '; ')
               FROM (
                 SELECT id, title, status, started_at, created_at
                   FROM tasks
                  WHERE status IN ('running', 'leased', 'assigned')
                    AND (
                      (lease_expires_at IS NOT NULL AND lease_expires_at >= now())
                      OR (${svc_start_epoch} > 0 AND COALESCE(started_at, created_at) >= to_timestamp(${svc_start_epoch}))
                    )
                  ORDER BY COALESCE(started_at, created_at) ASC
                  LIMIT 5
               ) t
             ), '')
             END;")"; then
      out="$(fleet_sanitize_summary "$out")"
      out="${out#"${out%%[![:space:]]*}"}"
      out="${out%"${out##*[![:space:]]}"}"
      [[ -n "$out" ]] && items+=("$out")
    elif [[ "$svc_state" == "active" ]]; then
      items+=("sched database active-work query failed while service is active")
    fi
  fi

  if [[ -n "$chat_dsn" ]]; then
    if out="$(fleet_psql_query "$chat_dsn" "
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
                    AND started_at >= ${svc_start_epoch}
                  ORDER BY started_at ASC
                  LIMIT 5
               ) t
             ), '')
             END;")"; then
      out="$(fleet_sanitize_summary "$out")"
      out="${out#"${out%%[![:space:]]*}"}"
      out="${out%"${out##*[![:space:]]}"}"
      [[ -n "$out" ]] && items+=("$out")
    elif [[ "$svc_state" == "active" ]]; then
      items+=("chat turns active-work query failed while service is active")
    fi

    if out="$(fleet_psql_query "$chat_dsn" "
      SELECT CASE WHEN to_regclass('public.approvals') IS NULL
                    OR NOT EXISTS (
                      SELECT 1
                        FROM information_schema.columns
                       WHERE table_schema = 'public'
                         AND table_name = 'approvals'
                         AND column_name = 'is_err'
                    ) THEN ''
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
                    AND COALESCE(resolved_at, created_at) >= GREATEST(${svc_start_epoch}, EXTRACT(EPOCH FROM now())::bigint - ${approval_window})
                  ORDER BY COALESCE(resolved_at, created_at) ASC
                  LIMIT 5
               ) a
             ), '')
             END;")"; then
      out="$(fleet_sanitize_summary "$out")"
      out="${out#"${out%%[![:space:]]*}"}"
      out="${out%"${out##*[![:space:]]}"}"
      [[ -n "$out" ]] && items+=("$out")
    elif [[ "$svc_state" == "active" ]]; then
      items+=("chat approvals active-work query failed while service is active")
    fi
  fi

  if (( ${#items[@]} == 0 )); then
    return 0
  fi
  local IFS="; "
  printf '%s\n' "${items[*]}"
  return 1
}

# fleet_wait_for_idle ENV_FILE MAX_WAIT_SECONDS POLL_SECONDS [SERVICE_NAME] —
# sets and refreshes the update drain sentinel file so new work admission stops,
# then polls fleet_active_work_summary until the deployment is idle (returns 0
# with the drain file still set) or MAX_WAIT_SECONDS elapses (clears the drain
# file, prints the remaining active work on stdout, and returns 1).
fleet_wait_for_idle() {
  local env_file="${1:-/etc/fleet/fleet.env}" max_wait="${2:-0}" poll="${3:-5}"
  local svc_unit="${4:-${SERVICE_NAME:-${FLEET_SERVICE_NAME:-fleet}}}"
  [[ "$max_wait" =~ ^[0-9]+$ ]] || max_wait=0
  [[ "$poll" =~ ^[0-9]+$ ]] && (( poll > 0 )) || poll=5
  local start now elapsed summary=""
  start="$(date +%s)"
  while true; do
    fleet_set_drain "$env_file"
    if summary="$(fleet_active_work_summary "$env_file" "$svc_unit")"; then
      fleet_set_drain "$env_file"
      return 0
    fi
    now="$(date +%s)"
    elapsed=$(( now - start ))
    if (( max_wait <= 0 || elapsed >= max_wait )); then
      fleet_clear_drain "$env_file"
      printf '%s\n' "$summary"
      return 1
    fi
    if declare -F info >/dev/null 2>&1; then
      info "waiting for active work to finish (${elapsed}s/${max_wait}s): ${summary}" >&2
    fi
    sleep "$poll"
  done
}
