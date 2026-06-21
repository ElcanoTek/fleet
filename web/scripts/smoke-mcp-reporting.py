#!/usr/bin/env python3
"""scripts/smoke-mcp-reporting.py — live reporting smoke test for SSP MCPs.

Exercises the high-level `*_run_report_from_prompt_inputs` (or equivalent)
tool on each SSP MCP using the credentials currently in env. Each provider
that's configured runs an end-to-end report pull over a small recent date
window and the script prints elapsed time + downloaded byte count. A
provider with missing creds skips gracefully.

Why this exists: agents have looped on `*_list_report_files` /
`*_fetch_report_data` for 20+ tool calls when the all-in-one tools'
internal poll timed out (ix_run_marketplace_draft_report's 60s default
was the original culprit; bumped to 300s). The polling-path config is
load-bearing — drift in any of these tools is invisible to chat-server
unit tests but the operator pays for it the first time a user asks for
a Magnite or Xandr pull. Run this before deploying changes that touch
any MCP under server/mcp/.

Usage (from a host with creds loaded):

    set -a && source ~/.bashrc && set +a
    python3 scripts/smoke-mcp-reporting.py

Exit 0 on every configured provider succeeding, 1 if any failed (skipped
providers don't count as failures).
"""

import asyncio
import os
import sys
import time
from pathlib import Path
from typing import Any, Awaitable, Callable

# Make the MCP modules importable regardless of where the script is run from.
_REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_REPO_ROOT / "server" / "mcp"))


def _check_creds(*names: str) -> tuple[bool, list[str]]:
    """True if all named env vars are set; returns names of any missing."""
    missing = [n for n in names if not os.environ.get(n)]
    return (not missing, missing)


async def smoke_indexexchange() -> dict[str, Any]:
    ok, missing = _check_creds("INDEXEXCHANGE_USERNAME", "INDEXEXCHANGE_PASSWORD")
    if not ok:
        return {"provider": "indexexchange", "skipped": True, "missing": missing}
    import indexexchange_mcp as ix  # type: ignore

    listed = await ix.ix_list_active_reports()
    reports = listed.get("reports") or listed.get("data") or []
    if not isinstance(reports, list) or not reports:
        return {"provider": "indexexchange", "ok": False, "error": "no active reports to derive account_id"}
    first = reports[0]
    account_id = first.get("accountID") or (first.get("accounts") or [None])[0]
    if not account_id:
        return {"provider": "indexexchange", "ok": False, "error": "could not extract account_id"}

    t0 = time.time()
    r = await ix.ix_run_marketplace_draft_report(
        account_id=account_id,
        report_title="elcano-mcp-smoke",
        date_range={"from": "2026-04-25", "to": "2026-04-29"},
        preset="deal_summary",
        download=True,
    )
    elapsed = time.time() - t0
    if r.get("success") and r.get("download"):
        return {
            "provider": "indexexchange",
            "ok": True,
            "elapsed": elapsed,
            "bytes": r["download"].get("bytes"),
            "path": r["download"].get("path"),
        }
    return {
        "provider": "indexexchange",
        "ok": False,
        "elapsed": elapsed,
        "error": str(r.get("error") or r.get("warning") or r),
    }


async def smoke_medianet() -> dict[str, Any]:
    ok, missing = _check_creds("MEDIANET_SELECT_EMAIL", "MEDIANET_SELECT_PASSWORD")
    if not ok:
        # Token-based auth is also valid.
        if not os.environ.get("MEDIANET_SELECT_TOKEN"):
            return {"provider": "medianet", "skipped": True, "missing": missing}
    import medianet_mcp as mn  # type: ignore

    t0 = time.time()
    r = await mn.mn_run_report_from_prompt_inputs(
        breakdowns=["day", "deal"],
        metrics=["impressions", "spend"],
        start_date_time="2026-04-25T00:00",
        end_date_time="2026-04-29T23:59",
        queue=True,
    )
    elapsed = time.time() - t0
    if r.get("success") and r.get("download"):
        return {
            "provider": "medianet",
            "ok": True,
            "elapsed": elapsed,
            "bytes": r["download"].get("bytes"),
            "path": r["download"].get("path"),
        }
    return {
        "provider": "medianet",
        "ok": False,
        "elapsed": elapsed,
        "error": str(r.get("error") or r),
    }


async def smoke_magnite() -> dict[str, Any]:
    ok, missing = _check_creds("MAGNITE_ACCESS_KEY", "MAGNITE_SECRET_KEY")
    if not ok:
        return {"provider": "magnite", "skipped": True, "missing": missing}
    import magnite_mcp as mg  # type: ignore

    t0 = time.time()
    r = await mg.magnite_run_report_from_prompt_inputs(
        breakdowns=["date", "deal"],
        metrics=["impressions", "spend"],
        date_range="last_3",
    )
    elapsed = time.time() - t0
    if r.get("success") and r.get("download"):
        return {
            "provider": "magnite",
            "ok": True,
            "elapsed": elapsed,
            "bytes": r["download"].get("bytes"),
            "path": r["download"].get("path"),
        }
    return {
        "provider": "magnite",
        "ok": False,
        "elapsed": elapsed,
        "error": str(r.get("error") or r),
    }


async def smoke_pubmatic() -> dict[str, Any]:
    ok, missing = _check_creds("PUBMATIC_USERNAME", "PUBMATIC_PASSWORD")
    if not ok:
        return {"provider": "pubmatic", "skipped": True, "missing": missing}
    import pubmatic_mcp as pm  # type: ignore

    presets = await pm.pm_list_reporting_presets()
    accounts = (presets or {}).get("known_accounts") or {}
    if not accounts:
        return {"provider": "pubmatic", "ok": False, "error": "no known_accounts configured"}
    account_id = next(iter(accounts.values()))

    t0 = time.time()
    r = await pm.pm_run_report_from_prompt_inputs(
        account_id=account_id,
        date_range={"from": "2026-04-25", "to": "2026-04-29"},
        report_type="deal performance",
    )
    elapsed = time.time() - t0
    if r.get("success") and r.get("download"):
        return {
            "provider": "pubmatic",
            "ok": True,
            "elapsed": elapsed,
            "bytes": r["download"].get("bytes"),
            "path": r["download"].get("path"),
        }
    return {
        "provider": "pubmatic",
        "ok": False,
        "elapsed": elapsed,
        "error": str(r.get("error") or r),
    }


async def smoke_xandr() -> dict[str, Any]:
    ok, missing = _check_creds("XANDR_USERNAME", "XANDR_PASSWORD")
    if not ok:
        return {"provider": "xandr", "skipped": True, "missing": missing}
    import xandr_mcp as xa  # type: ignore

    t0 = time.time()
    r = await xa.xandr_run_curator_report_from_prompt_inputs(
        breakdowns=["day", "deal"],
        metrics=["impressions", "spend"],
        last_n_days=5,
    )
    elapsed = time.time() - t0
    if r.get("success") and r.get("download"):
        return {
            "provider": "xandr",
            "ok": True,
            "elapsed": elapsed,
            "bytes": r["download"].get("bytes"),
            "path": r["download"].get("path"),
        }
    return {
        "provider": "xandr",
        "ok": False,
        "elapsed": elapsed,
        "error": str(r.get("error") or r),
    }


SMOKES: list[Callable[[], Awaitable[dict[str, Any]]]] = [
    smoke_indexexchange,
    smoke_medianet,
    smoke_magnite,
    smoke_pubmatic,
    smoke_xandr,
]


def _format(result: dict[str, Any]) -> str:
    name = result["provider"]
    if result.get("skipped"):
        return f"  ⏭  {name:14s} skipped (missing: {', '.join(result['missing'])})"
    if result.get("ok"):
        elapsed = result.get("elapsed", 0.0)
        size_kb = (result.get("bytes") or 0) / 1024.0
        path = result.get("path", "")
        return f"  ✓  {name:14s} {elapsed:5.1f}s  {size_kb:7.1f} KB  {path}"
    elapsed = result.get("elapsed", 0.0)
    err = result.get("error", "?")
    if len(err) > 200:
        err = err[:197] + "..."
    return f"  ✗  {name:14s} {elapsed:5.1f}s  FAILED: {err}"


async def main() -> int:
    print("SSP MCP reporting smoke")
    print("=" * 70)
    results = []
    for fn in SMOKES:
        try:
            r = await fn()
        except Exception as e:  # noqa: BLE001
            r = {"provider": fn.__name__.replace("smoke_", ""), "ok": False, "error": f"crashed: {e!r}"}
        results.append(r)
        print(_format(r), flush=True)
    print("=" * 70)
    failed = [r for r in results if not r.get("ok") and not r.get("skipped")]
    skipped = [r for r in results if r.get("skipped")]
    passed = [r for r in results if r.get("ok")]
    print(f"  {len(passed)} passed, {len(failed)} failed, {len(skipped)} skipped")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
