"""Regression test for the variant-aware default-curator-fee warning text.

`pm_prepare_deal_from_prompt_inputs` emits a human-readable warning and a
structured quality flag when it auto-applies the default 30% PoM curator
fee. Both pieces of text MUST reflect the MCP variant client's recipient
name (Elcano default, Reklaim variant, etc.) — earlier revisions
hard-coded "Elcano" in the warning string and the quality-flag impact
text, so the Reklaim variant emitted "Applied default Elcano curator
fee" even though the underlying `recipient` / `recipient_id` were
correctly set to 50751/Reklaim. Fixed by templating both strings with
`ELCANO_FEE_RECIPIENT_NAME` (aliased to the variant-aware
`DEFAULT_FEE_RECIPIENT_NAME`).
"""

import importlib
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import pubmatic_mcp


def _reload_pubmatic() -> object:
    """Re-evaluate the module so env-driven constants pick up changes."""
    return importlib.reload(pubmatic_mcp)


class TestDefaultCuratorFeeWarningText:
    def test_default_variant_text_is_elcano(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("MCP_VARIANT_CLIENT", raising=False)
        reloaded = _reload_pubmatic()
        try:
            assert reloaded.ELCANO_FEE_RECIPIENT_NAME == "Elcano"
            assert reloaded.DEFAULT_FEE_RECIPIENT_NAME == "Elcano"
        finally:
            monkeypatch.delenv("MCP_VARIANT_CLIENT", raising=False)
            _reload_pubmatic()

    def test_reklaim_variant_text_is_reklaim(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("MCP_VARIANT_CLIENT", "reklaim")
        reloaded = _reload_pubmatic()
        try:
            # MCP_VARIANT_CLIENT comes in lowercase from the loader; the
            # PubMatic module title-cases it so the warning reads
            # "Reklaim", not "reklaim".
            assert reloaded.ELCANO_FEE_RECIPIENT_NAME == "Reklaim"
            assert reloaded.DEFAULT_FEE_RECIPIENT_NAME == "Reklaim"
        finally:
            monkeypatch.delenv("MCP_VARIANT_CLIENT", raising=False)
            _reload_pubmatic()

    def test_warning_string_uses_recipient_name(self) -> None:
        """Source-level assertion: the warning string MUST interpolate
        ELCANO_FEE_RECIPIENT_NAME so it tracks the variant. Belt-and-
        suspenders against someone re-hardcoding "Elcano" in a future
        edit.
        """
        source_path = os.path.join(os.path.dirname(__file__), "..", "..", "pubmatic_mcp.py")
        with open(source_path) as f:
            source = f.read()
        # Negative: the legacy hardcoded warning must NOT come back.
        assert "Applied default Elcano curator fee" not in source, (
            "Hardcoded 'Applied default Elcano curator fee' regressed in "
            "pubmatic_mcp.py — use ELCANO_FEE_RECIPIENT_NAME instead."
        )
        assert "curator fee for Elcano." not in source, (
            "Hardcoded 'curator fee for Elcano.' regressed in pubmatic_mcp.py — use ELCANO_FEE_RECIPIENT_NAME instead."
        )
        # Positive: at least one variant-aware string must reference the
        # constant.
        assert "ELCANO_FEE_RECIPIENT_NAME} curator fee" in source, (
            "Expected variant-aware curator fee warning template not found"
        )
