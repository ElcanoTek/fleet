---
name: data-profiler
description: Profile a tabular data file (CSV/TSV/JSONL) before any analysis — runs a bundled stdlib script that reports row/column counts, inferred types, null rates, cardinality, numeric min/max/mean, top categorical values, and malformed rows. Use it the moment you receive an unfamiliar data file and before writing queries, charts, or conclusions from it.
---

# Data profiler

Never analyze an unfamiliar data file blind. Profile it first, read the profile,
then decide how to proceed. This skill bundles a stdlib-only profiler at
`scripts/profile.py`.

## Step 1 — run the profiler

From your workspace, run the bundled script with `bash` or `run_python`:

```bash
python3 skills/data-profiler/scripts/profile.py path/to/data.csv
```

Options:

- `--max-rows N` — profile only the first N rows of a very large file (fast
  first pass; rerun without it if the sample looks surprising).
- `--top N` — show N top values per column (default 5).
- `--delimiter ';'` — override delimiter detection for nonstandard files.

Format is chosen by extension (`.csv`, `.tsv`, `.jsonl`/`.ndjson`); anything
else is sniffed as delimited text.

## Step 2 — interpret the profile

Read every section of the output and check, in order:

1. **Malformed rows.** Any nonzero count means the file is not clean. Inspect
   the reported line numbers (`sed -n '42p' file.csv`) before trusting totals —
   a wrong delimiter or embedded newlines can silently drop rows.
2. **Row/column counts.** Do they match what the user or source claimed? A
   mismatch is a finding in itself — say so.
3. **Types.** A column reported as `mixed(...)` (e.g. `int` with a few `str`)
   usually means dirty data: sentinel strings, thousands separators, or units
   embedded in values. Look at its top values to see the offenders.
4. **Null rates.** Flag any column above ~5% nulls to the user; decide
   explicitly (and state) whether you will drop, impute, or ignore those rows.
5. **Cardinality.** distinct == row count suggests an ID/key column; distinct
   of 1 is a constant (useless for analysis); unexpectedly low cardinality on a
   "free text" field suggests categories.
6. **Numeric min/max/mean.** Sanity-check ranges: negative ages, zero prices,
   dates as epoch integers, and mean >> median-looking maxima (outliers) all
   warrant a closer look before aggregation.

## Step 3 — follow up before analysis

- State your findings in 3–5 bullets: shape, quality issues, and any column you
  will exclude or clean — before running the actual analysis.
- If the profile revealed problems, fix or filter them in your analysis code
  and say what you did (e.g. "dropped 12 malformed rows; coerced `price` by
  stripping `$`").
- For files too large to profile whole, profile with `--max-rows 50000`, note
  that stats are from a prefix sample, and avoid claims that require full-file
  scans (exact distinct counts, true max).
