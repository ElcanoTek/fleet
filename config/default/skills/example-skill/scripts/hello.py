#!/usr/bin/env python3
"""Minimal bundled-script demo for the example skill.

A real skill script would do something deterministic and reusable — a data
transform, a validator, a report generator — that is more reliable run as code
than re-derived by the model each turn. This one just greets its argument so the
SKILL.md walkthrough has something to run.
"""

import sys


def main() -> int:
    who = sys.argv[1] if len(sys.argv) > 1 else "world"
    print(f"Hello, {who} — this greeting came from a bundled skill script.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
