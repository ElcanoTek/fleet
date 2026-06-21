"""
Tests for Xandr cache helpers and name → ID resolvers.

Covers commit 1 of the Xandr deal-creation refactor: disk cache, deal-type
enum resolver, and buyer resolver with escape hatch + candidate matching.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import xandr_mcp


@pytest.fixture(autouse=True)
def _isolate_xandr_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so live-run artifacts can't leak in."""
    cache_root = tmp_path_factory.mktemp("xacache")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


class TestDiskCache:
    def test_round_trip(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("XANDR_CACHE_TTL_SECONDS", "3600")
        assert xandr_mcp._cache_get("unit_test_key") is None
        xandr_mcp._cache_put("unit_test_key", {"value": [1, 2]})
        assert xandr_mcp._cache_get("unit_test_key") == {"value": [1, 2]}
        assert (tmp_path / "cutlass" / "xandr" / "unit_test_key.json").is_file()

    def test_disabled_when_ttl_zero(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("XANDR_CACHE_TTL_SECONDS", "0")
        xandr_mcp._cache_put("k", {"v": 1})
        assert xandr_mcp._cache_get("k") is None

    def test_expired_entry_returns_none(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("XANDR_CACHE_TTL_SECONDS", "1")
        xandr_mcp._cache_put("stale", {"v": 1})
        # Rewrite stored_at far in the past to simulate expiry.
        path = tmp_path / "cutlass" / "xandr" / "stale.json"
        path.write_text('{"stored_at": 0, "value": {"v": 1}}', encoding="utf-8")
        assert xandr_mcp._cache_get("stale") is None

    def test_malformed_cache_file_treated_as_miss(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("XANDR_CACHE_TTL_SECONDS", "3600")
        cache_dir = tmp_path / "cutlass" / "xandr"
        cache_dir.mkdir(parents=True, exist_ok=True)
        (cache_dir / "broken.json").write_text("{not json", encoding="utf-8")
        assert xandr_mcp._cache_get("broken") is None


class TestResolveDealType:
    def test_by_name_exact(self):
        assert xandr_mcp._resolve_xandr_deal_type("Private Auction") == {"id": 2, "name": "Private Auction"}

    def test_by_name_case_insensitive(self):
        assert xandr_mcp._resolve_xandr_deal_type("private auction") == {"id": 2, "name": "Private Auction"}

    def test_by_id_passthrough(self):
        assert xandr_mcp._resolve_xandr_deal_type(2) == {"id": 2, "name": "Private Auction"}

    def test_by_numeric_string_passthrough(self):
        assert xandr_mcp._resolve_xandr_deal_type("3") == {"id": 3, "name": "Programmatic Guaranteed"}

    def test_unknown_id_raises_with_available_list(self):
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            xandr_mcp._resolve_xandr_deal_type(99)
        assert excinfo.value.code == "deal_type_unresolved"
        assert excinfo.value.details["input"] == 99
        assert any(e["name"] == "Private Auction" for e in excinfo.value.details["available"])

    def test_unknown_name_raises_with_available_list(self):
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            xandr_mcp._resolve_xandr_deal_type("Atlantis Auction")
        assert excinfo.value.code == "deal_type_unresolved"
        assert excinfo.value.details["input"] == "Atlantis Auction"

    def test_empty_input_raises(self):
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            xandr_mcp._resolve_xandr_deal_type("")
        assert excinfo.value.code == "deal_type_invalid_input"

    def test_bool_is_not_treated_as_id(self):
        with pytest.raises(xandr_mcp.XandrResolutionError):
            xandr_mcp._resolve_xandr_deal_type(True)


class TestResolveBuyer:
    @pytest.mark.asyncio
    async def test_by_id_escape_hatch_makes_no_api_call(self, monkeypatch: pytest.MonkeyPatch):
        async def must_not_call(*_args, **_kwargs):
            raise AssertionError("API must not be called when buyer id is passed directly")

        class FakeClient:
            list_buyers = staticmethod(must_not_call)

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        assert await xandr_mcp._resolve_xandr_buyer(123) == {"id": 123, "name": None}
        assert await xandr_mcp._resolve_xandr_buyer("456") == {"id": 456, "name": None}

    @pytest.mark.asyncio
    async def test_by_name_exact_match(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                return [
                    {"id": 123, "name": "Test Buyer Alpha"},
                    {"id": 456, "name": "Test Buyer Beta"},
                ]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        result = await xandr_mcp._resolve_xandr_buyer("Test Buyer Alpha")
        assert result == {"id": 123, "name": "Test Buyer Alpha"}

    @pytest.mark.asyncio
    async def test_by_name_case_insensitive(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                return [{"id": 999, "name": "The Trade Desk Inc."}]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        result = await xandr_mcp._resolve_xandr_buyer("the trade desk inc.")
        assert result == {"id": 999, "name": "The Trade Desk Inc."}

    @pytest.mark.asyncio
    async def test_single_substring_match_promoted_to_exact(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                return [{"id": 999, "name": "The Trade Desk Inc."}]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        result = await xandr_mcp._resolve_xandr_buyer("Trade Desk")
        assert result == {"id": 999, "name": "The Trade Desk Inc."}

    @pytest.mark.asyncio
    async def test_ambiguous_returns_candidates(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                return [
                    {"id": 1, "name": "Trade Desk Europe"},
                    {"id": 2, "name": "Trade Desk Americas"},
                ]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            await xandr_mcp._resolve_xandr_buyer("Trade Desk")
        assert excinfo.value.code == "buyer_ambiguous"
        candidates = excinfo.value.details["candidates"]
        assert {"id": 1, "name": "Trade Desk Europe"} in candidates
        assert {"id": 2, "name": "Trade Desk Americas"} in candidates

    @pytest.mark.asyncio
    async def test_unresolved_raises_with_empty_candidates(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                return [{"id": 1, "name": "Atlantis Buyer"}]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            await xandr_mcp._resolve_xandr_buyer("Bigfoot")
        assert excinfo.value.code == "buyer_unresolved"
        assert excinfo.value.details["candidates"] == []

    @pytest.mark.asyncio
    async def test_empty_string_raises(self):
        with pytest.raises(xandr_mcp.XandrResolutionError) as excinfo:
            await xandr_mcp._resolve_xandr_buyer("")
        assert excinfo.value.code == "buyer_invalid_input"

    @pytest.mark.asyncio
    async def test_disk_cache_avoids_second_api_call(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("XANDR_CACHE_TTL_SECONDS", "3600")
        call_count = {"n": 0}

        class FakeClient:
            async def list_buyers(self, **_kwargs):
                call_count["n"] += 1
                return [{"id": 7, "name": "Cached Buyer"}]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        await xandr_mcp._resolve_xandr_buyer("Cached Buyer")
        await xandr_mcp._resolve_xandr_buyer("Cached Buyer")
        assert call_count["n"] == 1
