#!/usr/bin/env bash
# scripts/mcp-catalog-lint.sh — dead-link lint for the built-in remote MCP
# catalog (#986, Phase 2).
#
# The directory in internal/clientconfig/builtin_remote_catalog.yaml is a
# static snapshot of ~290 vendor endpoints, and its per-entry docs_url is the
# source of truth users are sent to before they connect. Vendors move pages
# without telling anyone, so this script fetches every documentation link and
# fails on the ones that are OBVIOUSLY gone. It is deliberately not a
# reachability monitor for the MCP endpoints themselves and it never touches a
# credential.
#
# Verdicts, per link:
#   OK    — final status 2xx/3xx after following up to 5 redirects.
#   DEAD  — 404 or 410, or the hostname still does not resolve after the
#           retries. These FAIL the run.
#   WARN  — anything else: 401/403/405/406/429 (documentation sites often sit
#           behind a bot wall that rejects a non-browser client), 5xx, TLS or
#           timeout errors. Reported, not failing, because a curl-shaped
#           request being refused is not evidence the page is gone. Pass
#           --strict to make WARN fail too.
#
# Rate-limit aware: links are grouped by hostname; hosts run in parallel
# (--concurrency) but each host's links run one at a time with --delay seconds
# between them. A 429/503 is retried up to --retries times honouring
# Retry-After (capped at 60 s) or an exponential backoff on --delay, and the
# wait happens BEFORE any further request to that host. A resolver failure is
# retried the same way before the link is called dead. Every link is tried
# with HEAD first and falls back to GET when the server does not like HEAD, so
# a page is never downloaded unless it has to be.
#
# It needs only bash 4+, awk and curl — no YAML parser. The catalog's shape is
# regular (one `- name:` per entry, one `<field>: "<url>"` line per link) and
# the Go loader's strict decode plus scripts/check_mcp_catalog_lint_test.go
# keep it that way; --list prints what was extracted so drift is visible.
#
# Usage:
#   scripts/mcp-catalog-lint.sh                         # docs_url of every entry
#   scripts/mcp-catalog-lint.sh --fields docs_url,setup_url,repo_url
#   scripts/mcp-catalog-lint.sh --only stripe,linear    # a few entries
#   scripts/mcp-catalog-lint.sh --list                  # extraction only, no network
#   scripts/mcp-catalog-lint.sh --report links.tsv      # machine-readable results
#
# Exit status: 0 no DEAD link (and, with --strict, no WARN); 1 otherwise;
# 2 usage or setup error, including a link that produced no result. When
# GITHUB_STEP_SUMMARY is set, a Markdown table of the non-OK links is
# appended to it.
set -euo pipefail

CATALOG="internal/clientconfig/builtin_remote_catalog.yaml"
FIELDS="docs_url"
CONCURRENCY=4
DELAY=1
TIMEOUT=20
RETRIES=2
REPORT=""
STRICT=0
LIST=0
ONLY=""
UA="fleet-mcp-catalog-lint/1 (+https://github.com/ElcanoTek/fleet; documentation link check)"
# Internal records use the ASCII unit separator, not TAB: `read` treats TAB as
# whitespace and collapses an EMPTY field (no Retry-After header, no note), which
# would shift every column after it. The --report TSV is produced at the end.
US=$'\x1f'

usage() {
  sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --catalog) CATALOG="$2"; shift 2 ;;
    --fields) FIELDS="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    --delay) DELAY="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --retries) RETRIES="$2"; shift 2 ;;
    --report) REPORT="$2"; shift 2 ;;
    --only) ONLY="$2"; shift 2 ;;
    --strict) STRICT=1; shift ;;
    --list) LIST=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; echo "run with --help for usage" >&2; exit 2 ;;
  esac
done

if [[ ! -r "$CATALOG" ]]; then
  echo "catalog is missing or unreadable: $CATALOG" >&2
  exit 2
fi
if [[ ! "$FIELDS" =~ ^[a-z_]+(,[a-z_]+)*$ ]]; then
  echo "--fields must be a comma-separated list of field names (got $FIELDS)" >&2
  exit 2
fi
for n in "$TIMEOUT" "$RETRIES"; do
  if [[ ! "$n" =~ ^[0-9]+$ ]]; then
    echo "--timeout and --retries take whole numbers (got $n)" >&2
    exit 2
  fi
done
if [[ ! "$CONCURRENCY" =~ ^[1-9][0-9]*$ ]]; then
  # 0 would reach xargs as "as many as possible" — the opposite of politeness.
  echo "--concurrency takes a whole number of at least 1 (got $CONCURRENCY)" >&2
  exit 2
fi
if [[ ! "$DELAY" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  echo "--delay takes seconds (got $DELAY)" >&2
  exit 2
fi
command -v awk >/dev/null 2>&1 || { echo "awk is required" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# extract prints one "name<TAB>field<TAB>url" line per requested link field.
# An entry starts at its `- name:` line; a field line inside it is
# `<indent><field>: <value>` with the value optionally quoted and optionally
# followed by a `# comment`. The program is passed as a variable so it can use
# both quote characters without shell escaping.
read -r -d '' AWK_EXTRACT <<'AWK' || true
BEGIN {
  re = "^[[:space:]]+(" fields "):[[:space:]]*"
  if (only != "") {
    n = split(only, parts, ",")
    for (i = 1; i <= n; i++) {
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", parts[i])
      if (parts[i] != "") want[parts[i]] = 1
    }
  }
}
/^[[:space:]]*-[[:space:]]+name:[[:space:]]*/ {
  cur = $0
  sub(/^[[:space:]]*-[[:space:]]+name:[[:space:]]*/, "", cur)
  gsub(/["'[:space:]]/, "", cur)
  next
}
cur != "" && match($0, re) {
  if (only != "" && !(cur in want)) next
  key = substr($0, RSTART, RLENGTH)
  gsub(/[[:space:]:]/, "", key)
  val = substr($0, RLENGTH + 1)
  sub(/^[[:space:]]+/, "", val)
  if (val ~ /^"/) {
    val = substr(val, 2); sub(/".*$/, "", val)
  } else if (val ~ /^'/) {
    val = substr(val, 2); sub(/'.*$/, "", val)
  } else {
    sub(/[[:space:]]+#.*$/, "", val); sub(/[[:space:]]+$/, "", val)
  }
  if (val != "") print cur "\t" key "\t" val
}
AWK

LINKS="$WORK/links.tsv"
awk -v fields="${FIELDS//,/|}" -v only="$ONLY" "$AWK_EXTRACT" "$CATALOG" > "$LINKS"

# Every --only name must have matched, or a typo silently shrinks the run.
if [[ -n "$ONLY" ]]; then
  missing=()
  IFS=',' read -r -a wanted <<< "$ONLY"
  for w in "${wanted[@]}"; do
    w="${w#"${w%%[![:space:]]*}"}"; w="${w%"${w##*[![:space:]]}"}"
    [[ -z "$w" ]] && continue
    if ! cut -f1 "$LINKS" | grep -qx -- "$w"; then missing+=("$w"); fi
  done
  if (( ${#missing[@]} > 0 )); then
    echo "--only names not found in $CATALOG (or without a $FIELDS link): ${missing[*]}" >&2
    exit 2
  fi
fi

if (( LIST )); then
  cat "$LINKS"
  entries="$(grep -cE '^[[:space:]]*-[[:space:]]+name:' "$CATALOG" || true)"
  extracted="$(cut -f1 "$LINKS" | sort -u | wc -l | tr -d ' ')"
  if [[ -z "$ONLY" && "$entries" != "$extracted" ]]; then
    echo "note: $entries entries in the catalog, $extracted with a $FIELDS link" >&2
  fi
  exit 0
fi

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
RESULTS="$WORK/results.tsv"
: > "$RESULTS"
mkdir -p "$WORK/hosts"

# fetch URL METHOD → "http_code<US>curl_exit<US>retry_after<US>final_url"
fetch() {
  local url="$1" method="$2" hdrs out rc code eff ra
  hdrs="$(mktemp -p "$WORK")"
  local -a args=(-sS -o /dev/null -L --max-redirs 5 --max-time "$TIMEOUT" -A "$UA" -D "$hdrs" -w '%{http_code}\t%{url_effective}')
  if [[ "$method" == "HEAD" ]]; then args+=(-I); fi
  set +e
  out="$(curl "${args[@]}" "$url" 2>/dev/null)"
  rc=$?
  set -e
  code="${out%%$'\t'*}"
  eff="${out#*$'\t'}"
  ra="$(awk 'tolower($1) == "retry-after:" { gsub(/\r/, "", $2); print $2 }' "$hdrs" | tail -n 1)"
  rm -f "$hdrs"
  printf '%s%s%s%s%s%s%s\n' "${code:-000}" "$US" "$rc" "$US" "${ra:-}" "$US" "${eff:-$url}"
}

# classify HTTP_CODE CURL_EXIT → "OK|WARN|DEAD<US>note"
classify() {
  local code="$1" rc="$2"
  if (( rc != 0 )); then
    case "$rc" in
      6) printf 'DEAD%scould not resolve host (after %s retries)\n' "$US" "$RETRIES" ;;
      7) printf 'WARN%sconnection failed (curl exit 7)\n' "$US" ;;
      28) printf 'WARN%stimed out after %ss\n' "$US" "$TIMEOUT" ;;
      35|51|58|59|60) printf 'WARN%sTLS error (curl exit %s)\n' "$US" "$rc" ;;
      *) printf 'WARN%scurl exit %s\n' "$US" "$rc" ;;
    esac
    return
  fi
  case "$code" in
    2*|3*) printf 'OK%s\n' "$US" ;;
    404|410) printf 'DEAD%sHTTP %s\n' "$US" "$code" ;;
    401|403|405|406|429) printf 'WARN%sHTTP %s (bot wall or rate limit; re-check in a browser)\n' "$US" "$code" ;;
    5*) printf 'WARN%sHTTP %s (server error)\n' "$US" "$code" ;;
    *) printf 'WARN%sHTTP %s\n' "$US" "$code" ;;
  esac
}

# backoff ATTEMPT RETRY_AFTER → seconds to wait: Retry-After when sane, else
# --delay doubled per attempt.
backoff() {
  local attempt="$1" ra="$2"
  if [[ "$ra" =~ ^[0-9]+$ ]] && (( ra <= 60 )); then
    echo "$ra"
  else
    awk -v d="$DELAY" -v a="$attempt" 'BEGIN { printf "%.1f", d * (2 ^ a) }'
  fi
}

# check_url NAME FIELD URL — appends one result line.
check_url() {
  local name="$1" field="$2" url="$3"
  local attempt=0 method=HEAD code rc ra eff verdict note line
  while :; do
    IFS="$US" read -r code rc ra eff < <(fetch "$url" "$method")
    # Throttled: wait before ANY further request to this host, including the
    # GET fallback — an immediate second hit is what extends a ban.
    if [[ "$code" == "429" || "$code" == "503" ]]; then
      if (( attempt < RETRIES )); then
        attempt=$((attempt + 1))
        sleep "$(backoff "$attempt" "$ra")"
        continue
      fi
      break
    fi
    # A resolver failure is often the runner's, not the vendor's: retry
    # before calling the link dead. GET cannot help, so no fallback for it.
    if (( rc == 6 )); then
      if (( attempt < RETRIES )); then
        attempt=$((attempt + 1))
        sleep "$(backoff "$attempt" "")"
        continue
      fi
      break
    fi
    # HEAD refused, errored or answered anything but 2xx/3xx: ask again with
    # GET. That includes a HEAD 404 — some servers only route GET — so a page
    # is never called dead on the strength of HEAD alone.
    if [[ "$method" == "HEAD" ]] && { (( rc != 0 )) || [[ ! "$code" =~ ^[23] ]]; }; then
      method=GET
      continue
    fi
    break
  done
  IFS="$US" read -r verdict note < <(classify "$code" "$rc")
  line="$(printf '%s%s%s%s%s%s%s%s%s%s%s%s%s' "$verdict" "$US" "$code" "$US" "$name" "$US" "$field" "$US" "$url" "$US" "$eff" "$US" "$note")"
  echo "$line" >> "$RESULTS"
}

# process_host FILE — one host's links, sequentially, --delay apart.
process_host() {
  local first=1 name field url
  while IFS=$'\t' read -r name field url; do
    if (( first )); then first=0; else sleep "$DELAY"; fi
    check_url "$name" "$field" "$url"
  done < "$1"
}

export -f fetch classify backoff check_url process_host
export WORK RESULTS DELAY TIMEOUT RETRIES UA US

total=0
while IFS=$'\t' read -r name field url; do
  host="${url#*://}"; host="${host%%/*}"; host="${host%%:*}"
  host="${host//[^A-Za-z0-9.-]/_}"
  printf '%s\t%s\t%s\n' "$name" "$field" "$url" >> "$WORK/hosts/$host"
  total=$((total + 1))
done < "$LINKS"

if (( total == 0 )); then
  echo "no $FIELDS links found in $CATALOG${ONLY:+ for --only $ONLY}" >&2
  exit 2
fi
echo "checking $total link(s) across $(find "$WORK/hosts" -type f | wc -l | tr -d ' ') host(s), $CONCURRENCY host(s) at a time, ${DELAY}s between links on a host"

xargs_rc=0
find "$WORK/hosts" -type f -print0 | xargs -0 -P "$CONCURRENCY" -I{} bash -c 'process_host "$1"' _ {} || xargs_rc=$?

sort -t "$US" -k3,3 -k4,4 "$RESULTS" > "$RESULTS.sorted"
ok=0; warn=0; dead=0
while IFS="$US" read -r verdict code name field url eff note; do
  case "$verdict" in
    OK) ok=$((ok + 1)) ;;
    WARN) warn=$((warn + 1)) ;;
    DEAD) dead=$((dead + 1)) ;;
  esac
  if [[ "$verdict" == "OK" ]]; then
    printf '%-4s %s  %s  %s  %s\n' "$verdict" "$code" "$name" "$field" "$url"
  else
    printf '%-4s %s  %s  %s  %s  — %s\n' "$verdict" "$code" "$name" "$field" "$url" "$note"
  fi
done < "$RESULTS.sorted"
echo "links: $total  ok: $ok  warn: $warn  dead: $dead"

if [[ -n "$REPORT" ]]; then
  { printf 'verdict\thttp_code\tentry\tfield\turl\tfinal_url\tnote\n'; tr "$US" '\t' < "$RESULTS.sorted"; } > "$REPORT"
  echo "report written to $REPORT"
fi

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### MCP catalog links — $total checked, $ok ok, $warn warn, $dead dead"
    if (( warn + dead > 0 )); then
      echo
      echo "| verdict | code | entry | field | url | note |"
      echo "|---|---|---|---|---|---|"
      awk -F "$US" '$1 != "OK" { printf "| %s | %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5, $7 }' "$RESULTS.sorted"
    fi
  } >> "$GITHUB_STEP_SUMMARY"
fi

# A link with no result line is neither ok nor dead — it was never checked
# (a worker died, xargs could not start one). Say so instead of exiting 0.
checked=$((ok + warn + dead))
if (( checked != total )) || (( xargs_rc != 0 )); then
  echo "$((total - checked)) link(s) produced no result (xargs exit $xargs_rc); the run is incomplete" >&2
  exit 2
fi
if (( dead > 0 )); then
  echo "$dead documentation link(s) look dead: fix the docs_url, or hide the entry with remote_mcp_catalog_hidden (docs/MCP-CATALOG.md); --help explains the verdicts" >&2
  exit 1
fi
if (( STRICT )) && (( warn > 0 )); then
  echo "--strict: $warn link(s) did not answer 2xx" >&2
  exit 1
fi
exit 0
