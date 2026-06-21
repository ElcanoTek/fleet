#!/usr/bin/env python3
"""Regression tests for gamma.py's user-delivery result shaping.

Bug report (conv 79af2721, "i cant download this"): the Gamma exportUrl
authenticates with the server's X-API-KEY header and 502s on anonymous
browser fetches, but it sat prominently in completed-generation tool
results, so models pasted it into chat replies as "Download from Gamma
CDN" and users got dead links. `_present_completion_for_chat` is the
poka-yoke: after a successful local save the exportUrl is dropped and
replaced with ready-to-paste workspace-link markdown; when the save
failed the URL stays (the agent needs it to retry) but carries an
explicit never-share warning.

Run directly (`python3 server/mcp/test_gamma_delivery.py`) or via
`make test-py` in server/ (unittest discovery).
"""

from __future__ import annotations

import importlib.util
import sys
import types
import unittest
from pathlib import Path

_MCP_DIR = Path(__file__).resolve().parent


def _stub_module(name: str, **attrs) -> types.ModuleType:
    """Install a fake top-level module so gamma.py can import its heavy
    deps (fastmcp, httpx) without those wheels being present in CI.

    Only stubs when the real wheel is unavailable: in the consolidated
    fleet suite the real ``mcp``/``httpx`` wheels ARE installed and are
    needed by sibling tests (e.g. ``aioboto3``-backed ses_s3_email),
    so clobbering ``sys.modules`` with an empty fake would poison the
    shared session. Importing under the fake only happens in the
    wheel-less CI path this test was originally written for."""
    try:
        return importlib.import_module(name)
    except Exception:
        pass
    mod = types.ModuleType(name)
    for key, value in attrs.items():
        setattr(mod, key, value)
    sys.modules[name] = mod
    return mod


class _FakeMCP:
    def __init__(self, *_a, **_kw):
        pass

    def tool(self, *_a, **_kw):
        def decorator(fn):
            return fn

        return decorator

    def run(self, *_a, **_kw):
        pass


_stub_module("mcp")
_stub_module("mcp.server")
_stub_module("mcp.server.fastmcp", FastMCP=_FakeMCP)
_stub_module("httpx")


def _load(name: str):
    spec = importlib.util.spec_from_file_location(name, _MCP_DIR / f"{name}.py")
    assert spec and spec.loader, f"could not locate {name}.py next to this test"
    module = importlib.util.module_from_spec(spec)
    # Register under `name` only while executing so self-references resolve,
    # then restore the prior sys.modules entry so this stubbed instance does
    # not leak into sibling pytest tests that import the same module normally.
    previous = sys.modules.get(name)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    finally:
        if previous is not None:
            sys.modules[name] = previous
        else:
            sys.modules.pop(name, None)
    return module


gamma = _load("gamma")

_EXPORT_URL = "https://assets.api.gamma.app/export/pptx/abc/def/Deck.pptx"


class PresentCompletionForChat(unittest.TestCase):
    def test_successful_save_drops_export_url_and_adds_delivery(self):
        result = gamma._present_completion_for_chat(
            {
                "status": "completed",
                "gammaUrl": "https://gamma.app/docs/xyz",
                "exportUrl": _EXPORT_URL,
                "download": {
                    "path": "/opt/chat/workspace/79af2721-0000-0000-0000-000000000000/OM_Deck_g_1.pptx",
                    "bytes": 82000,
                },
            }
        )
        self.assertNotIn("exportUrl", result, "exportUrl must not be handed to the model after a successful save")
        delivery = result.get("user_delivery")
        self.assertIsInstance(delivery, dict)
        self.assertEqual(delivery["markdown_link"], "[OM_Deck_g_1.pptx](OM_Deck_g_1.pptx)")
        # The link target must be the bare relative filename — no scheme,
        # no absolute workspace path (the UI rewrite contract).
        self.assertNotIn("/opt/chat", delivery["markdown_link"])
        self.assertIn("assets.api.gamma.app", delivery["instructions"])
        # gammaUrl stays — it's a legitimate (seat-gated) edit link.
        self.assertIn("gammaUrl", result)

    def test_failed_save_keeps_export_url_with_warning(self):
        result = gamma._present_completion_for_chat(
            {
                "status": "completed",
                "exportUrl": _EXPORT_URL,
                "download": {"error": "could not create output dir"},
            }
        )
        self.assertEqual(result.get("exportUrl"), _EXPORT_URL, "agent needs the URL to retry the save")
        self.assertEqual(result.get("exportUrlWarning"), gamma.EXPORT_URL_WARNING)
        self.assertNotIn("user_delivery", result)

    def test_no_export_url_no_download_is_untouched(self):
        result = gamma._present_completion_for_chat({"status": "completed"})
        self.assertNotIn("exportUrlWarning", result)
        self.assertNotIn("user_delivery", result)

    def test_successful_save_clears_stale_status_warning(self):
        # check_presentation_status annotates raw results; once the wait
        # tool saves the file the warning must not linger next to the
        # user_delivery block.
        result = gamma._present_completion_for_chat(
            {
                "status": "completed",
                "exportUrl": _EXPORT_URL,
                "exportUrlWarning": gamma.EXPORT_URL_WARNING,
                "download": {"path": "/tmp/deck_g_2.pptx", "bytes": 10},
            }
        )
        self.assertNotIn("exportUrl", result)
        self.assertNotIn("exportUrlWarning", result)
        self.assertEqual(result["user_delivery"]["markdown_link"], "[deck_g_2.pptx](deck_g_2.pptx)")


if __name__ == "__main__":
    unittest.main()
