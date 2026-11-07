#!/usr/bin/env bash
# scripts/lib/envfile.sh — read and write KEY=VALUE env files (/etc/fleet/*.env).
#
# Sourced, not executed. One file, three readers, and a value must mean the
# same bytes to all of them:
#   1. systemd EnvironmentFile= (deploy/fleet*.service) — what the service sees.
#      Single quotes are literal; double quotes take \" \\ \` \$ escapes; an
#      unquoted value takes \x escapes and loses trailing whitespace. No
#      variable expansion, no inline comments.
#   2. fleet's own parser (internal/config.loadEnvFile, internal/creds) — trims,
#      cuts an UNQUOTED value at ` #`/tab-#, then strips one layer of matching
#      quotes. No escapes at all.
#   3. the scripts' env_get (bootstrap/update/doctor) — strips one leading and
#      one trailing quote character.
# fleet_env_encode picks the one form all three decode to the original bytes
# (internal/creds.encodeEnvValue applies the same rules in Go); a value no form
# can carry is refused rather than written corrupted.

# fleet_env_encode VALUE — print VALUE's on-disk form; exit 1 if unrepresentable.
fleet_env_encode() {
  local v="$1" LC_ALL=C
  case "$v" in *$'\n'* | *$'\r'*) return 1 ;; esac
  # Verbatim: printable, non-space, non-special edges; no quote, backslash,
  # `$`, backtick or comment marker anywhere a reader treats it specially.
  if [[ -z "$v" ]] || {
       [[ "$v" =~ ^[!-~]$|^[!-~].*[!-~]$ ]] &&
       [[ "${v:0:1}" != [\'\"\#] ]] &&
       [[ "$v" != *[\'\"\\\$\`]* && "$v" != *[[:space:]]\#* ]]
     }; then
    printf '%s' "$v"
  elif [[ "$v" != *\'* ]]; then
    printf "'%s'" "$v"                # literal to every reader
  elif [[ "$v" != *[\"\\]* ]]; then
    printf '"%s"' "$v"                # nothing for systemd to unescape
  else
    return 1                          # both quote kinds plus " or \: no shared form
  fi
}

# fleet_env_decode RAW — the value systemd's EnvironmentFile= parser yields for
# the text after `KEY=` (a port of env-file.c's value states). Used to read
# files a systemd unit consumed, e.g. the retired tier's fleet-web.env.
fleet_env_decode() {
  local raw="$1" LC_ALL=C out="" state=pre c i ws=-1
  for ((i = 0; i < ${#raw}; i++)); do
    c="${raw:i:1}"
    case "$state" in
      pre)
        case "$c" in
          "'") state=sq ;;
          '"') state=dq ;;
          \\) state=vesc ;;
          ' ' | $'\t') ;;
          *) state="val"; out+="$c"; ws=-1 ;;
        esac ;;
      val)
        case "$c" in
          \\) state=vesc ;;
          ' ' | $'\t') ((ws < 0)) && ws=${#out}; out+="$c" ;;
          *) out+="$c"; ws=-1 ;;
        esac ;;
      vesc) out+="$c"; ws=-1; state="val" ;;
      sq) if [[ "$c" == "'" ]]; then state=pre; else out+="$c"; fi ;;
      dq)
        case "$c" in
          '"') state=pre ;;
          \\) state=dqesc ;;
          *) out+="$c" ;;
        esac ;;
      dqesc)
        case "$c" in
          '"' | \\ | '`' | '$') out+="$c" ;;
          *) out+="\\$c" ;;
        esac
        state=dq ;;
    esac
  done
  [[ "$state" == val ]] && ((ws >= 0)) && out="${out:0:ws}"
  printf '%s' "$out"
}

# fleet_env_get_systemd FILE KEY — KEY's value as systemd reads FILE (the last
# assignment wins); empty when absent.
fleet_env_get_systemd() {
  local line
  line="$(grep -E "^${2}=" "$1" 2>/dev/null | tail -n1)" || true
  [[ -n "$line" ]] || return 0
  fleet_env_decode "${line#*=}"
}

# fleet_env_value FILE KEY — KEY's value as fleet's own parser reads FILE
# (internal/creds.ParseEnvLine, which config.Load and the admin CLI use, and
# so what config.ResolveDSNs sees): blank and # lines skipped, an `export `
# prefix tolerated, key and value trimmed, an UNQUOTED value cut at ` #` or
# tab-#, then one layer of matching quotes stripped; the last assignment
# wins; whitespace-trimmed again, as ResolveDSNs trims. Empty when absent or
# empty — so `KEY=""` counts as unset, exactly as in Go.
fleet_env_value() {
  local file="$1" want="$2" line t k v val=""
  [[ -r "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    t="${line%$'\r'}"
    t="${t#"${t%%[![:space:]]*}"}"
    t="${t%"${t##*[![:space:]]}"}"
    [[ -z "$t" || "$t" == \#* ]] && continue
    [[ "$t" == "export "* ]] && t="${t#export }"
    [[ "$t" == *=* ]] || continue
    k="${t%%=*}"
    k="${k#"${k%%[![:space:]]*}"}"
    k="${k%"${k##*[![:space:]]}"}"
    [[ -n "$k" && "$k" == "$want" ]] || continue
    v="${t#*=}"
    v="${v#"${v%%[![:space:]]*}"}"
    v="${v%"${v##*[![:space:]]}"}"
    if [[ "${v:0:1}" != \" && "${v:0:1}" != \' ]]; then
      if [[ "$v" == *" #"* ]]; then
        v="${v%% #*}"
      elif [[ "$v" == *$'\t#'* ]]; then
        v="${v%%$'\t#'*}"
      fi
      v="${v%"${v##*[![:space:]]}"}"
    fi
    if (( ${#v} >= 2 )) && [[ ( "${v:0:1}" == '"' && "${v: -1}" == '"' ) || ( "${v:0:1}" == "'" && "${v: -1}" == "'" ) ]]; then
      v="${v:1:${#v}-2}"
    fi
    val="$v"
  done < "$file"
  val="${val#"${val%%[![:space:]]*}"}"
  val="${val%"${val##*[![:space:]]}"}"
  printf '%s' "$val"
}

# fleet_upsert_env_file FILE KEY VALUE — set KEY in FILE (0600, created if
# missing), encoded by fleet_env_encode: the first KEY= line is replaced in
# place, later duplicates are dropped (every reader is last-assignment-wins, so
# a stale later line would override the new value), and every other line is
# kept. Mirrors internal/creds.SetEnvKey. Exit 1, file untouched, when the value
# cannot be represented.
fleet_upsert_env_file() {
  local file="$1" key="$2" enc tmp
  enc="$(fleet_env_encode "$3")" || return 1
  [[ -f "$file" ]] || install -D -m 0600 /dev/null "$file"
  tmp="$(mktemp "${file}.XXXXXX")"
  KEY="$key" VALUE="$enc" awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
    {
      eq = index($0, "=")
      if (eq > 0 && substr($0, 1, eq - 1) == k) {
        if (!done) print k "=" v
        done = 1
        next
      }
      print
    }
    END { if (!done) print k "=" v }
  ' "$file" > "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$file"
}
