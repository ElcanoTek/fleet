#!/usr/bin/env bash
# scripts/lib/idle.sh — active-work inspection and drain helpers for fleet update.
#
# Checks the running deployment's Postgres databases (sched tasks, chat turns,
# and detached approval executions) without sourcing the 0600 env file and
# without placing database passwords on psql's command line.

# fleet_uri_decode STR — decodes RFC 3986 percent-encoded bytes (%XX) in STR.
fleet_uri_decode() {
  local LC_ALL=C s="$1" out="" i=0 len c hex
  len=${#s}
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

# fleet_split_dsn_password DSN — prints the extracted password on line 1 and the
# DSN with its password stripped on line 2 (supporting both postgres:// URIs and
# libpq keyword=value conninfo strings). When the DSN has no password, line 1 is
# empty and line 2 is the original DSN.
fleet_split_dsn_password() {
  local dsn="$1" proto rest userinfo hostpart user pass="" redacted
  if [[ "$dsn" != *"://"* ]]; then
    redacted="$dsn"
    while [[ "$redacted" =~ ^(.*(^|[[:space:]]))password=\'(([^\'\\]|\\.)*)\'[[:space:]]*(.*)$ ]]; do
      pass="${BASH_REMATCH[3]}"
      pass="${pass//\\\'/\'}"
      pass="${pass//\\\\/\\}"
      redacted="${BASH_REMATCH[1]}${BASH_REMATCH[5]}"
    done
    while [[ "$redacted" =~ ^(.*(^|[[:space:]]))password=\"(([^\"\\]|\\.)*)\"[[:space:]]*(.*)$ ]]; do
      pass="${BASH_REMATCH[3]}"
      pass="${pass//\\\"/\"}"
      pass="${pass//\\\\/\\}"
      redacted="${BASH_REMATCH[1]}${BASH_REMATCH[5]}"
    done
    while [[ "$redacted" =~ ^(.*(^|[[:space:]]))password=([^[:space:]]+)[[:space:]]*(.*)$ ]]; do
      pass="${BASH_REMATCH[3]}"
      redacted="${BASH_REMATCH[1]}${BASH_REMATCH[4]}"
    done
    redacted="${redacted#"${redacted%%[![:space:]]*}"}"
    redacted="${redacted%"${redacted##*[![:space:]]}"}"
    printf '%s\n%s\n' "$pass" "$redacted"
    return 0
  fi
  if [[ "$dsn" != *"@"* ]]; then
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

# fleet_env_or_file KEY [ENV_FILE] — resolves KEY from the process environment
# first, falling back to ENV_FILE without sourcing it.
fleet_env_or_file() {
  local key="$1" env_file="${2:-/etc/fleet/fleet.env}" val=""
  val="${!key:-}"
  if [[ -z "$val" && -r "$env_file" ]]; then
    if declare -F env_get >/dev/null 2>&1; then
      val="$(env_get "$key" "$env_file")"
    else
      val="$(grep -E "^${key}=" "$env_file" 2>/dev/null | tail -n1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
    fi
  fi
  printf '%s' "$val"
}

# fleet_uri_encode_userinfo STR — percent-encodes characters that are reserved
# in URL userinfo so DB_USER / DB_PASSWORD values can be embedded safely into a
# postgres:// URI that fleet_split_dsn_password later splits.
fleet_uri_encode_userinfo() {
  local LC_ALL=C s="$1" out="" i=0 len c hex
  len=${#s}
  while (( i < len )); do
    c="${s:i:1}"
    if [[ "$c" =~ ^[A-Za-z0-9._~-]$ ]]; then
      out+="$c"
    else
      printf -v hex '%%%02X' "'$c"
      out+="$hex"
    fi
    i=$(( i + 1 ))
  done
  printf '%s' "$out"
}

# fleet_resolve_dsn PRIMARY_KEY ENV_FILE — resolves PRIMARY_KEY from the
# process environment first and then ENV_FILE, checking SCHED_DATABASE_URL for
# the scheduler store, before falling back to DATABASE_URL and finally the
# DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME/DB_SSLMODE parts.
fleet_resolve_dsn() {
  local key="$1" env_file="${2:-/etc/fleet/fleet.env}" val=""
  val="$(fleet_env_or_file "$key" "$env_file")"
  if [[ -z "$val" && "$key" == "FLEET_SCHED_DATABASE_URL" ]]; then
    val="$(fleet_env_or_file SCHED_DATABASE_URL "$env_file")"
  fi
  if [[ -z "$val" ]]; then
    val="$(fleet_env_or_file DATABASE_URL "$env_file")"
  fi
  if [[ -z "$val" ]]; then
    local host port user pass name ssl def_user="chat" def_name="chat" auth
    if [[ "$key" == "FLEET_SCHED_DATABASE_URL" ]]; then
      def_user="fleet"
      def_name="sched"
    fi
    host="$(fleet_env_or_file DB_HOST "$env_file")"
    port="$(fleet_env_or_file DB_PORT "$env_file")"
    user="$(fleet_env_or_file DB_USER "$env_file")"
    pass="$(fleet_env_or_file DB_PASSWORD "$env_file")"
    name="$(fleet_env_or_file DB_NAME "$env_file")"
    ssl="$(fleet_env_or_file DB_SSLMODE "$env_file")"
    [[ -n "$host" ]] || host="localhost"
    [[ -n "$port" ]] || port="5432"
    [[ -n "$user" ]] || user="$def_user"
    [[ -n "$name" ]] || name="$def_name"
    [[ -n "$ssl" ]] || ssl="disable"
    auth="$(fleet_uri_encode_userinfo "$user")"
    if [[ -n "$pass" ]]; then
      auth="${auth}:$(fleet_uri_encode_userinfo "$pass")"
    fi
    val="postgres://${auth}@${host}:${port}/${name}?sslmode=${ssl}"
  fi
  printf '%s' "$val"
}

FLEET_DRAIN_OWNER_TOKEN="${FLEET_DRAIN_OWNER_TOKEN:-pid-$$-${RANDOM:-0}}"

# fleet_drain_file [ENV_FILE] — returns the drain sentinel file path inspected
# by the running fleet server and scheduler pool.
fleet_drain_file() {
  local env_file="${1:-/etc/fleet/fleet.env}" p=""
  p="$(fleet_env_or_file FLEET_UPDATE_DRAIN_FILE "$env_file")"
  if [[ -n "$p" ]]; then
    printf '%s' "$p"
    return 0
  fi
  p="$(fleet_env_or_file FLEET_DRAIN_FILE "$env_file")"
  if [[ -n "$p" ]]; then
    printf '%s' "$p"
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
  mkdir -p "$(dirname "$drain_file")" 2>/dev/null || return 1
  printf 'draining %s\n' "$FLEET_DRAIN_OWNER_TOKEN" > "$drain_file" 2>/dev/null || return 1
}

# fleet_clear_drain [ENV_FILE] — removes the update drain sentinel file if owned
# by this invocation (even when fleet_set_drain ran inside a $(...) subshell).
fleet_clear_drain() {
  local drain_file cur=""
  drain_file="$(fleet_drain_file "${1:-/etc/fleet/fleet.env}")"
  if [[ -f "$drain_file" ]]; then
    cur="$(cat "$drain_file" 2>/dev/null || true)"
    if [[ "$cur" == "draining ${FLEET_DRAIN_OWNER_TOKEN}" ]]; then
      rm -f "$drain_file" 2>/dev/null || true
    fi
  fi
}

# fleet_service_runtime_state [SERVICE_NAME] — prints "<state>|<start_epoch>"
# where <state> is "active", "inactive" (unit installed and not active),
# "no-unit" (no systemd or unit not installed), or "unknown" (systemctl probe
# failed).
fleet_service_runtime_state() {
  local unit="${1:-${SERVICE_NAME:-${FLEET_SERVICE_NAME:-fleet}}}"
  [[ "$unit" == *.service ]] || unit="${unit}.service"
  if [[ "${FLEET_ASSUME_SERVICE_ACTIVE:-}" == "1" ]]; then
    printf 'active|0\n'
    return 0
  fi
  if ! command -v systemctl >/dev/null 2>&1; then
    printf 'no-unit|0\n'
    return 0
  fi
  local show_out load_state="" active_state="" start_ts="" start_epoch=0 line
  if ! show_out="$(systemctl show --timestamp=unix -p LoadState -p ActiveState -p ExecMainStartTimestamp "$unit" 2>/dev/null)"; then
    printf 'unknown|0\n'
    return 0
  fi
  while IFS= read -r line; do
    case "$line" in
      LoadState=*) load_state="${line#LoadState=}" ;;
      ActiveState=*) active_state="${line#ActiveState=}" ;;
      ExecMainStartTimestamp=*) start_ts="${line#ExecMainStartTimestamp=}" ;;
    esac
  done <<< "$show_out"
  load_state="$(printf '%s' "$load_state" | tr -d '[:space:]')"
  active_state="$(printf '%s' "$active_state" | tr -d '[:space:]')"
  if [[ "$load_state" == "not-found" ]]; then
    printf 'no-unit|0\n'
    return 0
  fi
  if [[ "$active_state" == "active" || "$active_state" == "activating" || "$active_state" == "reloading" ]]; then
    start_ts="${start_ts#"${start_ts%%[![:space:]]*}"}"
    start_ts="${start_ts%"${start_ts##*[![:space:]]}"}"
    if [[ "$start_ts" == @* ]]; then
      start_epoch="${start_ts#@}"
      start_epoch="${start_epoch%%.*}"
    elif [[ -n "$start_ts" && "$start_ts" != "n/a" ]]; then
      start_epoch="$(date -d "$start_ts" +%s 2>/dev/null || echo 0)"
    fi
    [[ "$start_epoch" =~ ^[0-9]+$ ]] || start_epoch=0
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

# fleet_probe_in_memory_counts ENV_FILE SVC_STATE — prints "<turns>|<approvals>"
# from the running server's /healthz headers, or "0|0" when unreachable.
fleet_probe_in_memory_counts() {
  local env_file="${1:-/etc/fleet/fleet.env}" svc_state="${2:-unknown}"
  local addr="" hdrs="" turns=0 approvals=0
  command -v curl >/dev/null 2>&1 || { printf '0|0\n'; return 0; }
  addr="$(fleet_env_or_file FLEET_SERVER_ADDR "$env_file")"
  [[ -n "$addr" ]] || addr="$(fleet_env_or_file CHAT_SERVER_ADDR "$env_file")"
  [[ -n "$addr" ]] || addr="$(fleet_env_or_file CUTLASS_SERVER_ADDR "$env_file")"
  if [[ -z "$addr" && ( "$svc_state" == "active" || "$svc_state" == "unknown" ) && "${FLEET_ASSUME_SERVICE_ACTIVE:-}" != "1" ]]; then
    addr="127.0.0.1:8080"
  fi
  if [[ -z "$addr" ]]; then
    printf '0|0\n'
    return 0
  fi
  [[ "$addr" == :* ]] && addr="127.0.0.1${addr}"
  if [[ "$addr" != http://* && "$addr" != https://* ]]; then
    addr="http://${addr}"
  fi
  addr="${addr%/}/healthz"
  if hdrs="$(curl -sSI --max-time 2 "$addr" 2>/dev/null || true)"; then
    turns="$(printf '%s\n' "$hdrs" | grep -i '^X-Fleet-Active-Turns:' | tail -n1 | cut -d: -f2- | tr -d '[:space:]' || true)"
    approvals="$(printf '%s\n' "$hdrs" | grep -i '^X-Fleet-Active-Approvals:' | tail -n1 | cut -d: -f2- | tr -d '[:space:]' || true)"
  fi
  [[ "$turns" =~ ^[0-9]+$ ]] || turns=0
  [[ "$approvals" =~ ^[0-9]+$ ]] || approvals=0
  printf '%s|%s\n' "$turns" "$approvals"
}

# fleet_active_work_summary ENV_FILE [SERVICE_NAME] — inspects sched.tasks,
# chat.turns, chat.approvals, and the running server's /healthz headers for
# in-flight work.
#
# Prints a semicolon-separated description and returns 1 when work is active;
# prints nothing and returns 0 when idle (or when the service is not running so
# crash-stranded rows can be recovered on start).
fleet_active_work_summary() {
  local env_file="${1:-/etc/fleet/fleet.env}"
  local svc_unit="${2:-${SERVICE_NAME:-${FLEET_SERVICE_NAME:-fleet}}}"
  local sched_dsn chat_dsn approval_window out has_tbl items=()
  local has_chat_turn_item=0 has_approval_item=0
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
    if has_tbl="$(fleet_psql_query "$sched_dsn" "SELECT to_regclass('public.tasks') IS NOT NULL;")"; then
      if [[ "$has_tbl" == "t" ]]; then
        if out="$(fleet_psql_query "$sched_dsn" "
          SELECT COALESCE((
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
          ), '');")"; then
          out="$(fleet_sanitize_summary "$out")"
          out="${out#"${out%%[![:space:]]*}"}"
          out="${out%"${out##*[![:space:]]}"}"
          [[ -n "$out" ]] && items+=("$out")
        elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
          items+=("sched database active-work query failed while service is ${svc_state}")
        fi
      fi
    elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
      items+=("sched database active-work query failed while service is ${svc_state}")
    fi
  fi

  if [[ -n "$chat_dsn" ]]; then
    if has_tbl="$(fleet_psql_query "$chat_dsn" "SELECT to_regclass('public.turns') IS NOT NULL;")"; then
      if [[ "$has_tbl" == "t" ]]; then
        if out="$(fleet_psql_query "$chat_dsn" "
          SELECT COALESCE((
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
          ), '');")"; then
          out="$(fleet_sanitize_summary "$out")"
          out="${out#"${out%%[![:space:]]*}"}"
          out="${out%"${out##*[![:space:]]}"}"
          if [[ -n "$out" ]]; then
            items+=("$out")
            has_chat_turn_item=1
          fi
        elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
          items+=("chat turns active-work query failed while service is ${svc_state}")
        fi
      fi
    elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
      items+=("chat turns active-work query failed while service is ${svc_state}")
    fi

    if has_tbl="$(fleet_psql_query "$chat_dsn" "
      SELECT to_regclass('public.approvals') IS NOT NULL
         AND EXISTS (
           SELECT 1
             FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'approvals'
              AND column_name = 'is_err'
         );")"; then
      if [[ "$has_tbl" == "t" ]]; then
        if out="$(fleet_psql_query "$chat_dsn" "
          SELECT COALESCE((
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
          ), '');")"; then
          out="$(fleet_sanitize_summary "$out")"
          out="${out#"${out%%[![:space:]]*}"}"
          out="${out%"${out##*[![:space:]]}"}"
          if [[ -n "$out" ]]; then
            items+=("$out")
            has_approval_item=1
          fi
        elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
          items+=("chat approvals active-work query failed while service is ${svc_state}")
        fi
      fi
    elif [[ "$svc_state" == "active" || "$svc_state" == "unknown" ]]; then
      items+=("chat approvals active-work query failed while service is ${svc_state}")
    fi
  fi

  local mem_info mem_turns=0 mem_approvals=0
  mem_info="$(fleet_probe_in_memory_counts "$env_file" "$svc_state")"
  mem_turns="${mem_info%%|*}"
  mem_approvals="${mem_info#*|}"
  if (( mem_turns > 0 && has_chat_turn_item == 0 )); then
    items+=("${mem_turns} in-memory chat turn(s) active")
  fi
  if (( mem_approvals > 0 && has_approval_item == 0 )); then
    items+=("${mem_approvals} in-memory approval execution(s) active")
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
    if ! fleet_set_drain "$env_file"; then
      printf 'failed to write update drain sentinel %s\n' "$(fleet_drain_file "$env_file")"
      return 1
    fi
    if summary="$(fleet_active_work_summary "$env_file" "$svc_unit")"; then
      if ! fleet_set_drain "$env_file"; then
        printf 'failed to write update drain sentinel %s\n' "$(fleet_drain_file "$env_file")"
        return 1
      fi
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
