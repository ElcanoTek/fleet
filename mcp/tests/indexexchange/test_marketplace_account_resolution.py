"""Tests for Index Exchange Marketplace account-ID resolution.

Covers the one-login / many-accounts UX:
- The IX_MARKETPLACE_ACCOUNT_IDS dict resolves all 7 active accounts by name.
- The DEFAULT_MARKETPLACE_ACCOUNT_ID env-var fallback (defaults to Elcano).
- The shared resolver accepts numeric IDs, name strings, and rejects unknown
  names with an error message listing every supported account.
"""

import importlib
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import indexexchange_mcp
from indexexchange_mcp import (
    DEFAULT_MARKETPLACE_ACCOUNT_ID,
    IX_MARKETPLACE_ACCOUNT_IDS,
    _resolve_marketplace_account_id,
)


class TestIxMarketplaceAccountIdsDict:
    def test_all_seven_active_accounts_resolve(self) -> None:
        # Sourced from the Index Exchange UI account picker.
        expected = {
            "reklaim": 1485234,
            "permutive": 1490424,
            "elcano": 1491166,
            "the weather company, llc": 1499155,
            "raptive": 1502939,
            "stirista": 1503605,
            "zeta global": 1507580,
        }
        for name, account_id in expected.items():
            assert IX_MARKETPLACE_ACCOUNT_IDS[name] == account_id

    def test_short_aliases_resolve(self) -> None:
        # Convenience aliases for the longer canonical names.
        assert IX_MARKETPLACE_ACCOUNT_IDS["twc"] == 1499155
        assert IX_MARKETPLACE_ACCOUNT_IDS["the weather company"] == 1499155
        assert IX_MARKETPLACE_ACCOUNT_IDS["zeta"] == 1507580


class TestResolveMarketplaceAccountId:
    def test_numeric_passthrough(self) -> None:
        assert _resolve_marketplace_account_id(1485234) == 1485234

    def test_numeric_string_passthrough(self) -> None:
        assert _resolve_marketplace_account_id("1485234") == 1485234

    def test_canonical_name(self) -> None:
        assert _resolve_marketplace_account_id("Reklaim") == 1485234
        assert _resolve_marketplace_account_id("Permutive") == 1490424
        assert _resolve_marketplace_account_id("Zeta Global") == 1507580

    def test_case_insensitive(self) -> None:
        assert _resolve_marketplace_account_id("PERMUTIVE") == 1490424
        assert _resolve_marketplace_account_id("zeta global") == 1507580

    def test_unknown_name_raises_with_full_list(self) -> None:
        with pytest.raises(ValueError) as excinfo:
            _resolve_marketplace_account_id("Acme Corp")
        message = str(excinfo.value)
        # The error message must enumerate every supported account so the
        # caller can pick the right one without reading source.
        for required in [
            "Reklaim",
            "Permutive",
            "Elcano",
            "The Weather Company, LLC",
            "Raptive",
            "Stirista",
            "Zeta Global",
        ]:
            assert required in message

    def test_empty_string_rejected(self) -> None:
        with pytest.raises(ValueError):
            _resolve_marketplace_account_id("   ")


class TestDefaultMarketplaceAccountId:
    def test_default_is_elcano_when_env_unset(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", raising=False)
        reloaded = importlib.reload(indexexchange_mcp)
        try:
            assert reloaded.DEFAULT_MARKETPLACE_ACCOUNT_ID == 1491166
        finally:
            importlib.reload(indexexchange_mcp)

    def test_env_override_picked_up(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", "1485234")
        reloaded = importlib.reload(indexexchange_mcp)
        try:
            assert reloaded.DEFAULT_MARKETPLACE_ACCOUNT_ID == 1485234
        finally:
            monkeypatch.delenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", raising=False)
            importlib.reload(indexexchange_mcp)

    def test_non_integer_env_falls_back_to_elcano(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", "not-a-number")
        reloaded = importlib.reload(indexexchange_mcp)
        try:
            assert reloaded.DEFAULT_MARKETPLACE_ACCOUNT_ID == 1491166
        finally:
            monkeypatch.delenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", raising=False)
            importlib.reload(indexexchange_mcp)

    def test_module_level_default_matches_elcano(self) -> None:
        # Sanity check on the import-time value (env var should be unset in
        # the test runner).
        assert DEFAULT_MARKETPLACE_ACCOUNT_ID == 1491166
