#!/usr/bin/env bash
# scripts/check-migrations.sh — migration linter (#256, #2049 M6).
#
# Checks NEW/CHANGED files of the one migration directory, internal/migrations/sql
# (docs/MIGRATIONS.md, ADR-0107). It is a guardrail, not a full SQL parser.
#
# Layout (every file under internal/migrations/sql/):
#   - sql/legacy/** is frozen: a file added or modified there is refused (its
#     bytes are what every installed database ran). A pure rename (100%
#     similar) is not a change and is exempt — the move into this layout was one.
#   - shipped migrations are append-only: a sql/NNNN_* file that existed at
#     the base and is modified or renamed is refused, and any file deleted
#     from (or moved out of) the directory — legacy/ included — is refused
#     (diff mode).
#   - a new migration is a pair, sql/NNNN_name.up.sql + sql/NNNN_name.down.sql
#     (four digits, from 0200, lower-case name): a misnamed or unpaired file is
#     refused.
#   - new migrations run with search_path pg_catalog, so every object they name
#     must carry its schema (chat.x, core.y, sched.z): an unqualified name after
#     CREATE/ALTER/DROP TABLE|VIEW|SEQUENCE|TYPE|FUNCTION|INDEX, CREATE INDEX/
#     TRIGGER ... ON, EXECUTE FUNCTION, INSERT INTO, UPDATE ... SET, DELETE FROM,
#     TRUNCATE or REFERENCES is refused (in .up.sql and .down.sql alike).
#
# Dangerous DDL, flagged in a forward (.up.sql) migration:
#   1. ADD [COLUMN] ... NOT NULL  without a DEFAULT — rewrites the whole table
#      under an ACCESS EXCLUSIVE lock and rejects existing rows. Checked per
#      comma-separated column clause, so a mixed `ADD a NOT NULL DEFAULT 0,
#      ADD b NOT NULL` still flags column b. The COLUMN keyword is optional in
#      Postgres, so the keyword-less form is caught too (#593); ADD CONSTRAINT /
#      PRIMARY KEY / UNIQUE / FOREIGN KEY / CHECK / EXCLUDE are exempt.
#   2. DROP [COLUMN] in an ALTER TABLE              — destructive + breaks an
#      older binary still reading the column mid-deploy. Keyword-less `DROP c`
#      is caught too (#593); DROP CONSTRAINT / DEFAULT / NOT NULL / IDENTITY /
#      EXPRESSION are exempt.
#   3. ALTER TYPE ... RENAME VALUE                 — an in-flight/older binary
#      reading the old enum label breaks.
#
# Before matching, SQL comments (-- and /* */) are removed and single-quoted
# string literals are blanked to '' — respecting quote state — so a `--` or a
# keyword INSIDE a string neither hides following DDL nor causes a false hit.
#
# `.down.sql` files are the rollback path (reversals are expected to drop), so
# they are skipped. A migration that MUST use a flagged pattern (e.g. a genuine
# destructive cleanup) opts out with a dedicated comment line — its own
# directive, so a passing mention in prose or a string can't trigger it:
#
#   -- migration-lint: allow-dangerous  <one-line justification>
#
# Usage:
#   scripts/check-migrations.sh                 # diff vs the merge-base with origin/main
#   scripts/check-migrations.sh FILE [FILE...]  # check the named files directly (used by tests;
#                                               # the layout checks apply to a path containing
#                                               # internal/migrations/sql/)
#
# In CI, set MIGRATION_LINT_BASE to a base ref/SHA to diff against; the script
# falls back to `git merge-base HEAD origin/main`, and if no base can be resolved
# (e.g. a shallow checkout with no origin/main) it prints a note and passes,
# since there is nothing it can reliably diff.
set -euo pipefail

# The one migration directory (ADR-0107).
MIGRATION_DIR='internal/migrations/sql/'

# normalize_sql strips comments and blanks string literals, emitting cleaned SQL
# on stdout. State (block-comment / string) is tracked so a `--`, `;`, or DDL
# keyword inside a '...' literal is neutralized rather than acted on.
normalize_sql() {
  # sq holds a single-quote char, passed in so the awk program (itself
  # single-quoted for the shell) stays free of embedded quotes and portable
  # across awk variants (gawk / mawk) — no \x hex escapes.
  awk -v sq="'" '
    BEGIN { in_block = 0 }
    {
      line = $0; n = length(line); out = ""; i = 1
      while (i <= n) {
        c  = substr(line, i, 1)
        c2 = substr(line, i, 2)
        if (in_block) {
          if (c2 == "*/") { in_block = 0; i += 2; out = out " " } else { i++ }
          continue
        }
        if (c == sq) {                           # single quote: a string literal
          out = out sq sq; i++                   # emit an empty literal in its place
          while (i <= n) {
            d = substr(line, i, 1)
            if (d == sq) {
              if (substr(line, i + 1, 1) == sq) { i += 2; continue }  # escaped ''
              i++; break
            }
            i++
          }
          continue
        }
        if (c2 == "--") { break }                # line comment: drop the rest of the line
        if (c2 == "/*") { in_block = 1; i += 2; out = out " "; continue }
        out = out c; i++
      }
      print out
    }
  ' "$1"
}

# --- Collect the set of files to check -------------------------------------
files=()
# Paths (diff mode) whose file existed at the base and was changed or renamed.
declare -A SHIPPED_CHANGED=()
# Paths (diff mode) under the migration directory deleted (or moved out).
DELETED=()
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  base="${MIGRATION_LINT_BASE:-}"
  if [ -n "$base" ] && ! git rev-parse --verify --quiet "${base}^{commit}" >/dev/null 2>&1; then
    base=""
  fi
  if [ -z "$base" ]; then
    base="$(git merge-base HEAD origin/main 2>/dev/null || true)"
  fi
  if [ -z "$base" ]; then
    echo "check-migrations: no base ref to diff against (set MIGRATION_LINT_BASE or fetch origin/main); skipping."
    exit 0
  fi
  # Added, modified and renamed files under the migration directory. Rename
  # detection runs over the whole diff (a pathspec would hide the old path and
  # turn a move into an add): a pure rename (R100) is not a change and is
  # skipped; a rename with edits is checked as a modified file.
  while IFS=$'\t' read -r status p1 p2; do
    [ -n "$status" ] || continue
    path="$p1"
    case "$status" in
      D*)
        # A migration that ran on a database cannot leave the build: that
        # database would then refuse to start (a version it does not ship).
        [[ "$p1" == "$MIGRATION_DIR"* ]] && DELETED+=("$p1")
        continue ;;
      R*)
        path="$p2"
        if [[ "$p1" == "$MIGRATION_DIR"* && "$p2" != "$MIGRATION_DIR"* ]]; then
          DELETED+=("$p1") # moved out of the directory: gone from the build
          continue
        fi
        # A pure rename INTO the directory (the M6 move) or within legacy/
        # is not a change. A shipped new-layout migration renamed is.
        if [ "$status" = R100 ] && { [[ "$p1" != "$MIGRATION_DIR"* ]] || [[ "$p1" == "$MIGRATION_DIR"legacy/* && "$p2" == "$MIGRATION_DIR"legacy/* ]]; }; then
          continue
        fi ;;
    esac
    case "$path" in
      "$MIGRATION_DIR"*)
        files+=("$path")
        # Shipped migrations are append-only: a file that existed at the
        # base (modified, or renamed away from its shipped name) is refused.
        case "$status" in
          M*) SHIPPED_CHANGED["$path"]=1 ;;
          R*) [[ "$p1" == "$MIGRATION_DIR"* ]] && SHIPPED_CHANGED["$path"]=1 ;;
        esac ;;
    esac
  done < <(git diff --name-status -M --diff-filter=AMRD "${base}...HEAD" 2>/dev/null || true)
fi

if [ "${#files[@]}" -eq 0 ] && [ "${#DELETED[@]}" -eq 0 ]; then
  echo "check-migrations: no changed migration files to check."
  exit 0
fi

# --- Scan each file ---------------------------------------------------------
violations=0
file_hit=0
emit() { # <message>
  if [ "$file_hit" -eq 0 ]; then
    echo "FAIL: $current_file"
    file_hit=1
  fi
  echo "    - $1"
  violations=$((violations + 1))
}

# layout_checks FILE — the one-directory rules for a file under
# internal/migrations/sql/ (see the header). Prints nothing for other paths.
layout_checks() {
  local f="$1" rel base stem other
  case "$f" in
    "$MIGRATION_DIR"*) rel="${f#"$MIGRATION_DIR"}" ;;
    */"$MIGRATION_DIR"*) rel="${f#*/"$MIGRATION_DIR"}" ;;
    *) return 0 ;;
  esac
  case "$rel" in
    legacy/*)
      emit "legacy migrations are frozen: add a new sql/NNNN_name.up.sql + .down.sql instead of adding or editing a file under sql/legacy/"
      return 0 ;;
    */*)
      emit "new migrations live directly in internal/migrations/sql/ (no subdirectories)"
      return 0 ;;
  esac
  base="$rel"
  case "$base" in
    *.sql) ;;
    *) return 0 ;;
  esac
  if [[ -n "${SHIPPED_CHANGED[$f]:-}" ]]; then
    emit "shipped migrations are append-only: this file existed at the base and was edited or renamed — restore it and add a new sql/NNNN_name.up.sql + .down.sql instead"
  fi
  if ! [[ "$base" =~ ^([0-9]{4})_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
    emit "migration file name must be NNNN_name.up.sql / NNNN_name.down.sql (four digits, lower-case name)"
    return 0
  fi
  if (( 10#${BASH_REMATCH[1]} < 200 )); then
    emit "new migrations start at version 0200 (1–199 are the legacy chat and sched files)"
  fi
  stem="${f%.*.sql}"
  case "$base" in
    *.up.sql) other="${stem}.down.sql" ;;
    *) other="${stem}.up.sql" ;;
  esac
  if [ ! -f "$other" ]; then
    emit "every migration ships both halves: ${other##*/} is missing"
  fi
  unqualified_names "$f"
}

# unqualified_names FILE — names a new migration uses without a schema.
unqualified_names() {
  local cleaned stmts names n
  cleaned="$(normalize_sql "$1")"
  stmts="$(printf '%s' "$cleaned" | tr '\n' ' ' | tr ';' '\n')"
  local id='["[:alnum:]_.]+'
  local s='[[:space:]]+'
  names="$(
    {
      printf '%s\n' "$stmts" | grep -oiE "CREATE${s}((GLOBAL|LOCAL)${s})?((TEMP|TEMPORARY|UNLOGGED)${s})?TABLE${s}(IF${s}NOT${s}EXISTS${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "CREATE${s}(OR${s}REPLACE${s})?(FUNCTION|PROCEDURE|VIEW|MATERIALIZED${s}VIEW|SEQUENCE|TYPE)${s}(IF${s}NOT${s}EXISTS${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "ALTER${s}(TABLE|VIEW|MATERIALIZED${s}VIEW|SEQUENCE|TYPE|FUNCTION|PROCEDURE|INDEX)${s}(IF${s}EXISTS${s})?(ONLY${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "DROP${s}(TABLE|VIEW|MATERIALIZED${s}VIEW|SEQUENCE|TYPE|FUNCTION|PROCEDURE|INDEX)${s}(IF${s}EXISTS${s})?${id}"
      printf '%s\n' "$stmts" | grep -iE "CREATE${s}(UNIQUE${s})?INDEX|CREATE${s}(OR${s}REPLACE${s})?(CONSTRAINT${s})?TRIGGER" \
        | grep -oiE "${s}ON${s}(ONLY${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "EXECUTE${s}(FUNCTION|PROCEDURE)${s}${id}"
      printf '%s\n' "$stmts" | grep -oiE "INSERT${s}INTO${s}${id}"
      printf '%s\n' "$stmts" | grep -oiE "UPDATE${s}(ONLY${s})?${id}${s}SET" | awk '{print $(NF-1)}'
      printf '%s\n' "$stmts" | grep -oiE "DELETE${s}FROM${s}(ONLY${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "TRUNCATE${s}(TABLE${s})?(ONLY${s})?${id}"
      printf '%s\n' "$stmts" | grep -oiE "REFERENCES${s}${id}"
    } | awk '{print $NF}' | grep -v '\.' | sort -u
  )" || true
  for n in $names; do
    emit "unqualified name ${n} (new migrations run with search_path pg_catalog: write chat.${n}, core.${n} or sched.${n})"
  done
}

for f in "${DELETED[@]}"; do
  current_file="$f"
  file_hit=0
  emit "a migration may not be deleted (or moved out of internal/migrations/sql): a database that ran it would refuse to start — shipped migrations are append-only"
done

for f in "${files[@]}"; do
  [ -f "$f" ] || [[ "$f" == *"$MIGRATION_DIR"legacy/* ]] || continue
  current_file="$f"
  file_hit=0
  layout_checks "$f"

  # Dangerous DDL: only forward migrations; down-migrations are reversals and
  # expected to drop.
  case "$f" in
    *.down.sql) continue ;;
    *.sql) ;;
    *) continue ;;
  esac
  [ -f "$f" ] || continue

  # The opt-out must be its own comment directive line — an exact `allow-dangerous`
  # token, so a passing mention (a negation in prose, or `allowlist_sync`) does
  # not silently disable the checks.
  if grep -qiE '^[[:space:]]*--[[:space:]]*migration-lint:[[:space:]]*allow-dangerous([[:space:]]|$)' "$f"; then
    echo "  [skip] $f — dangerous DDL explicitly allowed via migration-lint directive"
    continue
  fi

  # One SQL statement per line: comments stripped + string literals blanked (so
  # a `;` inside a string can't split a statement), then split on ';'.
  cleaned="$(normalize_sql "$f")"
  stmts="$(printf '%s' "$cleaned" | tr '\n' ' ' | tr ';' '\n')"
  # Column clauses: split statements further on ',' so a DEFAULT on one column
  # can't mask a NOT-NULL-without-default sibling column.
  clauses="$(printf '%s' "$stmts" | tr ',' '\n')"

  # 1. ADD [COLUMN] ... NOT NULL without DEFAULT (per column clause). Postgres
  #    makes the COLUMN keyword optional (#593), so match ANY ADD clause and
  #    exclude the ADD <table-constraint> forms (CONSTRAINT / PRIMARY KEY /
  #    UNIQUE / FOREIGN KEY / CHECK / EXCLUDE), which are not column adds.
  if printf '%s\n' "$clauses" \
      | grep -iE '(^|[[:space:]])ADD[[:space:]]' \
      | grep -ivE '(^|[[:space:]])ADD[[:space:]]+(CONSTRAINT|PRIMARY|UNIQUE|FOREIGN|CHECK|EXCLUDE)([[:space:]]|\(|$)' \
      | grep -iE 'NOT[[:space:]]+NULL' | grep -viqE 'DEFAULT'; then
    emit "ADD [COLUMN] ... NOT NULL without DEFAULT (rewrites the table under an exclusive lock; add the column nullable, backfill, then set NOT NULL)"
  fi
  # 2. DROP [COLUMN] in an ALTER TABLE. The COLUMN keyword is optional (#593):
  #    extract each `DROP <word>` from ALTER TABLE statements and flag any whose
  #    following word is NOT one of the safe non-column DROP actions
  #    (DROP CONSTRAINT / DROP DEFAULT / DROP NOT NULL / DROP IDENTITY /
  #    DROP EXPRESSION). `DROP COLUMN x`, `DROP x`, and `DROP IF EXISTS x` all
  #    leave a non-safe word after DROP, so all three forms flag.
  if printf '%s\n' "$stmts" | grep -iE 'ALTER[[:space:]]+TABLE' \
      | grep -ioE 'DROP[[:space:]]+["[:alnum:]_]+' \
      | grep -ivqE '^DROP[[:space:]]+(CONSTRAINT|DEFAULT|NOT|IDENTITY|EXPRESSION)$'; then
    emit "DROP [COLUMN] (destructive + breaks an older binary mid-deploy; stop reading the column first, drop it in a later migration)"
  fi
  # 3. ALTER TYPE ... RENAME VALUE.
  if printf '%s\n' "$stmts" | grep -iE 'ALTER[[:space:]]+TYPE' | grep -iqE 'RENAME[[:space:]]+VALUE'; then
    emit "ALTER TYPE ... RENAME VALUE (an older binary reading the old label breaks; add a new value instead of renaming)"
  fi
done

if [ "$violations" -gt 0 ]; then
  echo ""
  echo "check-migrations: $violations problem(s) found."
  echo "See docs/MIGRATIONS.md for the safe zero-downtime alternative, or add a"
  echo "  -- migration-lint: allow-dangerous <justification>"
  echo "comment line to the file if the pattern is intentional and reviewed."
  exit 1
fi

echo "check-migrations: ${#files[@]} migration file(s) checked, no problem found."
