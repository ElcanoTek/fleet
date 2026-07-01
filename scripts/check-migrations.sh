#!/usr/bin/env bash
# scripts/check-migrations.sh — zero-downtime migration linter (#256).
#
# Rejects a small set of DANGEROUS DDL patterns in NEW/CHANGED migration files so
# a lock-heavy or backward-incompatible change can't merge unnoticed. It is a
# guardrail, not a full SQL parser: it flags the patterns the migration-safety
# guide (docs/MIGRATIONS.md) calls out and points at the safe alternative.
#
# Flagged in a forward (.up.sql or chat NNN_name.sql) migration:
#   1. ADD COLUMN ... NOT NULL  without a DEFAULT  — rewrites the whole table
#      under an ACCESS EXCLUSIVE lock and rejects existing rows. Checked per
#      comma-separated column clause, so a mixed `ADD a NOT NULL DEFAULT 0,
#      ADD b NOT NULL` still flags column b.
#   2. DROP COLUMN                                 — destructive + breaks an
#      older binary still reading the column mid-deploy.
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
#   scripts/check-migrations.sh FILE [FILE...]  # check the named files directly (used by tests)
#
# In CI, set MIGRATION_LINT_BASE to a base ref/SHA to diff against; the script
# falls back to `git merge-base HEAD origin/main`, and if no base can be resolved
# (e.g. a shallow checkout with no origin/main) it prints a note and passes,
# since there is nothing it can reliably diff.
set -euo pipefail

MIGRATION_GLOBS=(
  'internal/store/migrations/*.sql'
  'internal/sched/db/migrations/*.sql'
)

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
  # --diff-filter=AM: only added/modified files (a deletion can't introduce DDL).
  while IFS= read -r f; do
    [ -n "$f" ] && files+=("$f")
  done < <(git diff --name-only --diff-filter=AM "${base}...HEAD" -- "${MIGRATION_GLOBS[@]}" 2>/dev/null || true)
fi

if [ "${#files[@]}" -eq 0 ]; then
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

for f in "${files[@]}"; do
  # Only forward migrations; down-migrations are reversals and expected to drop.
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

  current_file="$f"
  file_hit=0

  # One SQL statement per line: comments stripped + string literals blanked (so
  # a `;` inside a string can't split a statement), then split on ';'.
  cleaned="$(normalize_sql "$f")"
  stmts="$(printf '%s' "$cleaned" | tr '\n' ' ' | tr ';' '\n')"
  # Column clauses: split statements further on ',' so a DEFAULT on one column
  # can't mask a NOT-NULL-without-default sibling column.
  clauses="$(printf '%s' "$stmts" | tr ',' '\n')"

  # 1. ADD COLUMN ... NOT NULL without DEFAULT (per column clause).
  if printf '%s\n' "$clauses" | grep -iE 'ADD[[:space:]]+COLUMN' | grep -iE 'NOT[[:space:]]+NULL' | grep -viqE 'DEFAULT'; then
    emit "ADD COLUMN ... NOT NULL without DEFAULT (rewrites the table under an exclusive lock; add the column nullable, backfill, then set NOT NULL)"
  fi
  # 2. DROP COLUMN.
  if printf '%s\n' "$stmts" | grep -iqE 'DROP[[:space:]]+COLUMN'; then
    emit "DROP COLUMN (destructive + breaks an older binary mid-deploy; stop reading the column first, drop it in a later migration)"
  fi
  # 3. ALTER TYPE ... RENAME VALUE.
  if printf '%s\n' "$stmts" | grep -iE 'ALTER[[:space:]]+TYPE' | grep -iqE 'RENAME[[:space:]]+VALUE'; then
    emit "ALTER TYPE ... RENAME VALUE (an older binary reading the old label breaks; add a new value instead of renaming)"
  fi
done

if [ "$violations" -gt 0 ]; then
  echo ""
  echo "check-migrations: $violations dangerous DDL pattern(s) found."
  echo "See docs/MIGRATIONS.md for the safe zero-downtime alternative, or add a"
  echo "  -- migration-lint: allow-dangerous <justification>"
  echo "comment line to the file if the pattern is intentional and reviewed."
  exit 1
fi

echo "check-migrations: ${#files[@]} migration file(s) checked, no dangerous DDL found."
