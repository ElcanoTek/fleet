#!/usr/bin/env bash
# scripts/check-migrations.sh — zero-downtime migration linter (#256).
#
# Rejects a small set of DANGEROUS DDL patterns in NEW/CHANGED migration files so
# a lock-heavy or backward-incompatible change can't merge unnoticed. It is a
# guardrail, not a schema validator: it flags the patterns the migration-safety
# guide (docs/MIGRATIONS.md) calls out and points at the safe alternative.
#
# Flagged in a forward (.up.sql or chat NNN_name.sql) migration statement:
#   1. ADD COLUMN ... NOT NULL  without a DEFAULT  — rewrites the whole table
#      under an ACCESS EXCLUSIVE lock and rejects existing rows.
#   2. DROP COLUMN                                 — destructive + breaks an
#      older binary still reading the column mid-deploy.
#   3. ALTER TYPE ... RENAME VALUE                 — an in-flight/older binary
#      reading the old enum label breaks.
#
# `.down.sql` files are the rollback path (reversals are expected to drop), so
# they are skipped. A migration that MUST use a flagged pattern (e.g. a genuine
# destructive cleanup) opts out by including a marker comment anywhere in the
# file, which keeps the intent explicit and reviewable:
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
for f in "${files[@]}"; do
  # Only forward migrations; down-migrations are reversals and expected to drop.
  case "$f" in
    *.down.sql) continue ;;
    *.sql) ;;
    *) continue ;;
  esac
  [ -f "$f" ] || continue

  if grep -qiE 'migration-lint:[[:space:]]*allow' "$f"; then
    echo "  [skip] $f — dangerous DDL explicitly allowed via migration-lint marker"
    continue
  fi

  # Normalize to one SQL statement per line: strip -- comments, join lines,
  # then split on ';'. This lets a single regex see a whole statement even when
  # the source wraps it across lines.
  stmts="$(sed -E 's/--.*$//' "$f" | tr '\n' ' ' | tr ';' '\n')"

  file_hit=0
  emit() { # <message>
    if [ "$file_hit" -eq 0 ]; then
      echo "FAIL: $f"
      file_hit=1
    fi
    echo "    - $1"
    violations=$((violations + 1))
  }

  # 1. ADD COLUMN ... NOT NULL without DEFAULT.
  if echo "$stmts" | grep -iE 'ADD[[:space:]]+COLUMN' | grep -iE 'NOT[[:space:]]+NULL' | grep -viqE 'DEFAULT'; then
    emit "ADD COLUMN ... NOT NULL without DEFAULT (rewrites the table under an exclusive lock; add the column nullable, backfill, then set NOT NULL)"
  fi
  # 2. DROP COLUMN.
  if echo "$stmts" | grep -iqE 'DROP[[:space:]]+COLUMN'; then
    emit "DROP COLUMN (destructive + breaks an older binary mid-deploy; stop reading the column first, drop it in a later migration)"
  fi
  # 3. ALTER TYPE ... RENAME VALUE.
  if echo "$stmts" | grep -iE 'ALTER[[:space:]]+TYPE' | grep -iqE 'RENAME[[:space:]]+VALUE'; then
    emit "ALTER TYPE ... RENAME VALUE (an older binary reading the old label breaks; add a new value instead of renaming)"
  fi
done

if [ "$violations" -gt 0 ]; then
  echo ""
  echo "check-migrations: $violations dangerous DDL pattern(s) found."
  echo "See docs/MIGRATIONS.md for the safe zero-downtime alternative, or add a"
  echo "  -- migration-lint: allow-dangerous <justification>"
  echo "marker to the file if the pattern is intentional and reviewed."
  exit 1
fi

echo "check-migrations: ${#files[@]} migration file(s) checked, no dangerous DDL found."
