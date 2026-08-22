#!/usr/bin/env python3
"""Profile a tabular data file (CSV, TSV, or JSONL) before analysis.

Prints row/column counts, per-column type inference, null rates, cardinality,
min/max/mean for numeric columns, top values for categorical columns, and a
malformed-row report. Standard library only.

Usage:
    python3 profile.py DATA_FILE [--max-rows N] [--top N] [--delimiter CHAR]
"""

import argparse
import csv
import json
import statistics
import sys
from collections import Counter

NULL_TOKENS = {"", "null", "none", "na", "n/a", "nan", "\\n"}
# Cap distinct-value tracking so huge ID columns don't eat memory.
MAX_DISTINCT_TRACKED = 100_000


class ColumnStats:
    """Accumulates per-column statistics over one pass of the data."""

    def __init__(self, name):
        self.name = name
        self.count = 0
        self.nulls = 0
        self.type_counts = Counter()  # int / float / bool / str
        self.values = Counter()  # distinct value -> occurrences (capped)
        self.distinct_overflow = False
        self.numbers = []

    def add(self, raw):
        self.count += 1
        if raw is None or (isinstance(raw, str) and raw.strip().lower() in NULL_TOKENS):
            self.nulls += 1
            return
        kind, num = infer_type(raw)
        self.type_counts[kind] += 1
        if num is not None:
            self.numbers.append(num)
        key = str(raw)
        if len(key) > 80:
            key = key[:77] + "..."
        if len(self.values) < MAX_DISTINCT_TRACKED or key in self.values:
            self.values[key] += 1
        else:
            self.distinct_overflow = True

    @property
    def inferred_type(self):
        if not self.type_counts:
            return "empty"
        ranked = self.type_counts.most_common()
        # int + float mix is just float.
        kinds = set(self.type_counts)
        if kinds <= {"int", "float"} and len(kinds) == 2:
            return "float"
        if len(ranked) == 1:
            return ranked[0][0]
        return "mixed(%s)" % ",".join(f"{k}:{v}" for k, v in ranked)


def infer_type(value):
    """Return (kind, numeric_value_or_None) for one non-null cell."""
    if isinstance(value, bool):
        return "bool", None
    if isinstance(value, int):
        return "int", float(value)
    if isinstance(value, float):
        return "float", value
    if not isinstance(value, str):
        return type(value).__name__, None
    s = value.strip()
    if s.lower() in ("true", "false"):
        return "bool", None
    try:
        return "int", float(int(s, 10))
    except ValueError:
        pass
    try:
        return "float", float(s)
    except ValueError:
        return "str", None


def iter_jsonl(path, max_rows):
    """Yield (row_number, dict_or_None) for each line; None marks malformed."""
    with open(path, encoding="utf-8", errors="replace") as fh:
        for i, line in enumerate(fh, start=1):
            if max_rows and i > max_rows:
                return
            if not line.strip():
                continue
            try:
                obj = json.loads(line)
                yield i, obj if isinstance(obj, dict) else None
            except json.JSONDecodeError:
                yield i, None


def profile_jsonl(path, max_rows, top):
    cols = {}
    total = 0
    malformed = []
    for lineno, obj in iter_jsonl(path, max_rows):
        total += 1
        if obj is None:
            malformed.append(lineno)
            continue
        for key, val in obj.items():
            if key not in cols:
                cols[key] = ColumnStats(key)
            if isinstance(val, (dict, list)):
                val = json.dumps(val, sort_keys=True)
            cols[key].add(val)
        # Keys absent from this record count as nulls for known columns.
        for key, stats in cols.items():
            if key not in obj:
                stats.count += 1
                stats.nulls += 1
    report(path, "jsonl", total, list(cols.values()), malformed, top)


def profile_delimited(path, delimiter, max_rows, top):
    with open(path, newline="", encoding="utf-8", errors="replace") as fh:
        reader = csv.reader(fh, delimiter=delimiter)
        try:
            header = next(reader)
        except StopIteration:
            print(f"{path}: file is empty")
            return
        cols = [ColumnStats(name or f"col_{i}") for i, name in enumerate(header)]
        total = 0
        malformed = []
        for row in reader:
            total += 1
            if max_rows and total > max_rows:
                total -= 1
                break
            if len(row) != len(cols):
                malformed.append(reader.line_num)
                continue
            for stats, cell in zip(cols, row, strict=True):
                stats.add(cell)
    kind = "tsv" if delimiter == "\t" else "csv"
    report(path, kind, total, cols, malformed, top)


def report(path, kind, total, cols, malformed, top):
    good = total - len(malformed)
    print(f"== Profile: {path} ({kind}) ==")
    print(f"rows: {total} total, {good} parsed, {len(malformed)} malformed")
    print(f"columns: {len(cols)}")
    if malformed:
        shown = ", ".join(str(n) for n in malformed[:10])
        more = f" (+{len(malformed) - 10} more)" if len(malformed) > 10 else ""
        print(f"malformed at lines: {shown}{more}")
    for stats in cols:
        print(f"\n-- {stats.name} --")
        null_pct = 100.0 * stats.nulls / stats.count if stats.count else 0.0
        print(f"  type: {stats.inferred_type}")
        print(f"  nulls: {stats.nulls}/{stats.count} ({null_pct:.1f}%)")
        card = len(stats.values)
        suffix = "+ (tracking capped)" if stats.distinct_overflow else ""
        print(f"  distinct: {card}{suffix}")
        if stats.numbers:
            print(
                "  min/max/mean: {:g} / {:g} / {:g}".format(
                    min(stats.numbers),
                    max(stats.numbers),
                    statistics.fmean(stats.numbers),
                )
            )
            if len(stats.numbers) > 1:
                print(f"  stdev: {statistics.stdev(stats.numbers):g}")
        non_null = stats.count - stats.nulls
        looks_categorical = non_null > 0 and card <= max(top, non_null * 0.5)
        if stats.values and (looks_categorical or not stats.numbers):
            print("  top values:")
            for value, n in stats.values.most_common(top):
                print(f"    {value!r}: {n}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("path", help="CSV, TSV, or JSONL file to profile")
    parser.add_argument(
        "--max-rows",
        type=int,
        default=0,
        help="profile at most N data rows (0 = all rows)",
    )
    parser.add_argument(
        "--top", type=int, default=5, help="show N top values per column (default 5)"
    )
    parser.add_argument(
        "--delimiter",
        default=None,
        help="field delimiter override (default: by extension, then sniffed)",
    )
    args = parser.parse_args(argv)

    lower = args.path.lower()
    if lower.endswith((".jsonl", ".ndjson")):
        profile_jsonl(args.path, args.max_rows, args.top)
        return 0
    delimiter = args.delimiter
    if delimiter is None:
        if lower.endswith(".tsv"):
            delimiter = "\t"
        elif lower.endswith(".csv"):
            delimiter = ","
        else:
            with open(args.path, encoding="utf-8", errors="replace") as fh:
                sample = fh.read(64 * 1024)
            try:
                delimiter = csv.Sniffer().sniff(sample, delimiters=",\t;|").delimiter
            except csv.Error:
                delimiter = ","
    profile_delimited(args.path, delimiter, args.max_rows, args.top)
    return 0


if __name__ == "__main__":
    sys.exit(main())
