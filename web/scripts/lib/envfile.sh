#!/usr/bin/env bash
# scripts/lib/envfile.sh — shared helpers for reading/writing KEY=VALUE
# env files. Sourced by deploy/chat-cli so the quoting rules live in
# one place. Ported from sister project gig.
#
# Values are always written as:  KEY="value"
# with \ and " inside the value escaped. Reads accept either unquoted,
# double-quoted, or single-quoted values so hand-edited files still work.
#
# None of these helpers call sudo or restart services — callers are
# responsible for privilege + side effects.

# is_secret_key KEY → returns 0 if the name looks like a secret we should
# mask in output or prompts.
is_secret_key() {
  local key="$1"
  case "$key" in
    *TOKEN*|*KEY*|*SECRET*|*PASSWORD*|*PASSWD*) return 0 ;;
    *) return 1 ;;
  esac
}

# env_get KEY FILE → prints the (unquoted) value for KEY, or exits
# nonzero if KEY is not present. Last match wins, matching shell's own
# `set -a; . file` behavior.
env_get() {
  local key="$1" file="$2"
  [[ -f "$file" ]] || return 1
  local line
  line="$(grep -E "^[[:space:]]*${key}=" "$file" 2>/dev/null | tail -n1)"
  [[ -n "$line" ]] || return 1
  line="${line#*=}"
  if [[ ${#line} -ge 2 && "${line:0:1}" == '"' && "${line: -1}" == '"' ]]; then
    line="${line:1:${#line}-2}"
    line="${line//\\\"/\"}"
    line="${line//\\\\/\\}"
  elif [[ ${#line} -ge 2 && "${line:0:1}" == "'" && "${line: -1}" == "'" ]]; then
    line="${line:1:${#line}-2}"
  fi
  printf '%s' "$line"
}

# env_set KEY VALUE FILE → writes KEY="VALUE" to FILE. If KEY already
# has an uncommented line, that line is rewritten in place; otherwise a
# new line is appended. Commented placeholder lines (# KEY=...) are left
# alone. Preserves FILE's mode; creates FILE with 0600 if missing.
env_set() {
  local key="$1" value="$2" file="$3"
  local tmp; tmp="$(mktemp)"
  local found=0

  local escaped="${value//\\/\\\\}"
  escaped="${escaped//\"/\\\"}"

  if [[ -f "$file" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" =~ ^[[:space:]]*${key}= ]]; then
        if (( found == 0 )); then
          printf '%s="%s"\n' "$key" "$escaped" >> "$tmp"
          found=1
        fi
      else
        printf '%s\n' "$line" >> "$tmp"
      fi
    done < "$file"
  fi

  if (( found == 0 )); then
    printf '%s="%s"\n' "$key" "$escaped" >> "$tmp"
  fi

  if [[ -f "$file" ]]; then
    chmod --reference="$file" "$tmp" 2>/dev/null || chmod 0600 "$tmp"
  else
    chmod 0600 "$tmp"
  fi
  mv -f "$tmp" "$file"
}

# env_unset KEY FILE → removes any uncommented line assigning KEY.
# No-op if FILE doesn't exist or KEY isn't present.
env_unset() {
  local key="$1" file="$2"
  [[ -f "$file" ]] || return 0
  local tmp; tmp="$(mktemp)"
  grep -vE "^[[:space:]]*${key}=" "$file" > "$tmp" || true
  chmod --reference="$file" "$tmp" 2>/dev/null || chmod 0600 "$tmp"
  mv -f "$tmp" "$file"
}

# env_show_redacted FILE → prints FILE, masking values for any key that
# looks like a secret (see is_secret_key). Skips blank lines + comments.
env_show_redacted() {
  local file="$1"
  [[ -f "$file" ]] || return 1
  awk '
    /^[[:space:]]*#/    { next }
    /^[[:space:]]*$/    { next }
    {
      eq = index($0, "=")
      if (eq == 0) { print; next }
      key = substr($0, 1, eq - 1)
      sub(/^[[:space:]]+/, "", key)
      if (key ~ /TOKEN|KEY|SECRET|PASSWORD|PASSWD/) {
        printf "%s=[REDACTED]\n", key
      } else {
        print
      }
    }
  ' "$file"
}
