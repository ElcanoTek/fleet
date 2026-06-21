"""Regression tests for the Xandr resolution-layer bugs surfaced by deal
SF_XANDR_TRADR_TDM_DISP_CA_ELC07225_B14 (id 2700527).

The live run hit four blocking errors:
  1. /buyer?name=... returned HTTP 404 and the raw httpx error escaped the
     execute tool, forcing the agent to fall back to a hard-coded buyer ID
     (which violates the protocol's no-manual-resolution guarantee).
  2. All five IAB content categories failed with `candidates: []`.
  3. Segment lookup returned `candidates: []`.
  4. Profile creation silently failed when targeting was mostly unresolved,
     leaving the deal live but un-targeted with the response still claiming
     `success: true`.
"""

import os
import sys
from unittest.mock import MagicMock

import httpx
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import xandr_mcp
from xandr_mcp import (
    XandrResolutionError,
    _pick_entity_match,
    _resolve_xandr_buyer,
    _resolve_xandr_iab_category,
)


@pytest.fixture(autouse=True)
def _isolate_xandr_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so we never pick up
    artifacts written by a prior live run."""
    cache_root = tmp_path_factory.mktemp("xandrcache_resolution")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


class TestBuyerLookupSurvivesHttp404:
    """Regression: GET /buyer?name=The+Trade+Desk returned 404 and the raw
    httpx.HTTPStatusError escaped the execute tool. The resolver must catch
    the error and convert it into an XandrResolutionError carrying a
    `buyer_lookup_failed` code so the execute layer surfaces it as an
    `xandr_unresolved_buyer` quality flag instead of a tool-level crash."""

    @pytest.mark.asyncio
    async def test_404_becomes_resolution_error(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_buyers(self, **_kwargs):
                response = MagicMock(status_code=404)
                raise httpx.HTTPStatusError("404 Not Found", request=MagicMock(), response=response)

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        with pytest.raises(XandrResolutionError) as exc_info:
            await _resolve_xandr_buyer("The Trade Desk")
        assert exc_info.value.code == "buyer_lookup_failed"
        assert exc_info.value.details["status_code"] == 404

    @pytest.mark.asyncio
    async def test_resolver_lists_without_name_filter(self, monkeypatch: pytest.MonkeyPatch):
        """Live API rejects /buyer?name=... — resolver must call list_buyers
        without `name_like` and match client-side."""
        captured_kwargs: list[dict] = []

        class FakeClient:
            async def list_buyers(self, **kwargs):
                captured_kwargs.append(kwargs)
                return [
                    {"id": 1088, "name": "The Trade Desk, Inc."},
                    {"id": 2070, "name": "Amazon DSP"},
                ]

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        result = await _resolve_xandr_buyer("The Trade Desk")
        # Substring match promotes the single hit to exact.
        assert result == {"id": 1088, "name": "The Trade Desk, Inc."}
        # name_like was NOT forwarded to the upstream call.
        assert captured_kwargs[0].get("name_like") is None


class TestPickEntityMatchSurfacesCandidates:
    """Regression: every failed unresolved flag carried `candidates: []`,
    leaving the agent with no debugging signal. The matcher now falls back
    to bidirectional substring AND token-overlap matches."""

    def test_token_overlap_surfaces_candidates(self):
        # "Auto Parts" doesn't substring-match "Automotive Parts &
        # Accessories", but they share the token "parts" — that's enough
        # to surface as a candidate.
        catalog = [
            {"id": 1, "name": "Automotive Parts & Accessories"},
            {"id": 2, "name": "Pets"},
            {"id": 3, "name": "Travel"},
        ]
        exact, candidates = _pick_entity_match(catalog, "Auto Parts")
        assert exact is None
        assert any(c["id"] == 1 for c in candidates), "shared token should surface candidate"

    def test_reverse_substring_match(self):
        # Catalog entry "Auto" is contained in needle "Auto Parts" — that
        # used to miss the substring check. Now both directions match.
        catalog = [{"id": 7, "name": "Auto"}]
        exact, candidates = _pick_entity_match(catalog, "Auto Parts")
        # Single substring hit -> promoted to exact.
        assert exact is not None and exact["id"] == 7

    def test_exact_still_wins(self):
        catalog = [
            {"id": 10, "name": "Auto Parts"},
            {"id": 11, "name": "Automotive Parts"},
        ]
        exact, _candidates = _pick_entity_match(catalog, "Auto Parts")
        assert exact is not None and exact["id"] == 10


class TestIabResolverSurvivesAndSurfacesCandidates:
    """Regression: all 5 IAB categories returned `candidates: []`. The fix
    loads the full catalog and uses token-overlap to surface near matches."""

    @pytest.mark.asyncio
    async def test_unresolved_iab_includes_token_overlap_candidates(self, monkeypatch: pytest.MonkeyPatch):
        catalog = [
            {"id": 100, "name": "Automotive Parts & Accessories", "code": "IAB2-1"},
            {"id": 101, "name": "Auto Repair", "code": "IAB2-2"},
            {"id": 102, "name": "Travel", "code": "IAB20"},
        ]

        class FakeClient:
            async def list_content_categories(self, **_kwargs):
                return catalog

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        # "Auto Parts" doesn't exact-match anything but shares tokens with
        # "Automotive Parts & Accessories".
        with pytest.raises(XandrResolutionError) as exc_info:
            await _resolve_xandr_iab_category("Auto Parts")
        candidates = exc_info.value.details["candidates"]
        assert candidates, "candidate list must not be empty when token overlap exists"
        assert any(c["id"] == 100 for c in candidates)

    @pytest.mark.asyncio
    async def test_resolves_by_iab_code(self, monkeypatch: pytest.MonkeyPatch):
        """Trader can pass IAB code (IAB2-2) and resolver matches via the
        `code` name_key."""
        catalog = [{"id": 101, "name": "Auto Repair", "code": "IAB2-2"}]

        class FakeClient:
            async def list_content_categories(self, **_kwargs):
                return catalog

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())
        result = await _resolve_xandr_iab_category("IAB2-2")
        assert result["id"] == 101


class TestNoTargetingAttachedFlag:
    """Regression: when substantive targeting (IAB, segments, geo) was
    requested but none of it resolved, the previous flow built a
    devices-only profile that the API silently rejected. The deal went
    live with no targeting attached but the response said success=true
    with a soft `xandr_profile_create_failed` flag — easy for the trader
    to miss. Now we skip the profile create entirely and emit a clear
    `xandr_no_targeting_attached` flag plus a top-level
    `targeting_attached: bool` field."""

    @pytest.mark.asyncio
    async def test_skips_profile_create_when_substantive_targeting_unresolved(self, monkeypatch: pytest.MonkeyPatch):
        """End-to-end: trader passes IAB + segment + geo, all fail to
        resolve -> profile create is NOT attempted, deal still creates,
        targeting_attached=False, xandr_no_targeting_attached emitted."""

        async def fake_resolve_buyer(value, **_kwargs):
            return {"id": 1088, "name": "The Trade Desk, Inc."}

        async def fake_resolve_io(value):
            return {
                "id": 11443185,
                "name": "Marketplace Pro",
                "advertiser_id": 11447334,
                "state": "active",
            }

        async def fake_resolve_country(value):
            raise XandrResolutionError(
                f"Country not resolved: {value!r}",
                code="country_unresolved",
                details={"input": value, "candidates": []},
            )

        async def fake_resolve_iab(value):
            raise XandrResolutionError(
                f"IAB not resolved: {value!r}",
                code="iab_category_unresolved",
                details={"input": value, "candidates": []},
            )

        async def fake_resolve_segment(value, **_kwargs):
            raise XandrResolutionError(
                f"Segment not resolved: {value!r}",
                code="segment_unresolved",
                details={"input": value, "candidates": []},
            )

        profile_create_called = {"n": 0}

        async def fake_create_xandr_deal(payload):
            return {"success": True, "deal": {"id": 2700527, **payload["deal"]}}

        async def fake_get_xandr_deal(deal_id):
            return {"success": True, "deal": {"id": deal_id, "line_item_ids": [31373667]}}

        class FakeClient:
            async def create_profile(self, payload):
                profile_create_called["n"] += 1
                assert payload is not None
                return {"profile": {"id": 999}}

            async def create_line_item(self, payload, **_kwargs):
                assert payload is not None
                return {"line-item": {"id": 31373667}}

        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_buyer", fake_resolve_buyer)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_insertion_order", fake_resolve_io)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_country", fake_resolve_country)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_iab_category", fake_resolve_iab)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_segment", fake_resolve_segment)
        monkeypatch.setattr(xandr_mcp, "create_xandr_deal", fake_create_xandr_deal)
        monkeypatch.setattr(xandr_mcp, "get_xandr_deal", fake_get_xandr_deal)
        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())

        result = await xandr_mcp.xandr_execute_deal_from_prompt_inputs(
            name="Test Xandr Deal",
            code="test_xandr_deal",
            buyer="The Trade Desk",
            insertion_order_name="Marketplace Pro",
            channel="display",
            geo_countries=["United States"],
            iab_categories=["Auto Parts"],
            segment_names=["Cars & Auto_Chrysler Enthusiasts"],
        )

        assert result["success"] is True
        assert result["targeting_attached"] is False
        assert profile_create_called["n"] == 0, "profile create must be skipped"
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_no_targeting_attached" in flag_names
        # Old soft flag should NOT appear when we deliberately skipped.
        assert "xandr_profile_create_failed" not in flag_names

    @pytest.mark.asyncio
    async def test_targeting_attached_true_on_happy_path(self, monkeypatch: pytest.MonkeyPatch):
        """When all targeting resolves and the profile creates with a
        numeric id, targeting_attached must be True."""

        async def fake_resolve_buyer(value, **_kwargs):
            return {"id": 1088, "name": "The Trade Desk, Inc."}

        async def fake_resolve_io(value):
            return {
                "id": 11443185,
                "name": "Marketplace Pro",
                "advertiser_id": 11447334,
                "state": "active",
            }

        async def fake_resolve_country(value):
            return {"id": 233, "name": "United States", "code": "US"}

        async def fake_create_xandr_deal(payload):
            return {"success": True, "deal": {"id": 2700528, **payload["deal"]}}

        async def fake_get_xandr_deal(deal_id):
            return {"success": True, "deal": {"id": deal_id, "line_item_ids": [31373668]}}

        class FakeClient:
            async def create_profile(self, payload):
                assert payload is not None
                return {"profile": {"id": 555}}

            async def create_line_item(self, payload, **_kwargs):
                assert payload is not None
                return {"line-item": {"id": 31373668}}

        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_buyer", fake_resolve_buyer)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_insertion_order", fake_resolve_io)
        monkeypatch.setattr(xandr_mcp, "_resolve_xandr_country", fake_resolve_country)
        monkeypatch.setattr(xandr_mcp, "create_xandr_deal", fake_create_xandr_deal)
        monkeypatch.setattr(xandr_mcp, "get_xandr_deal", fake_get_xandr_deal)
        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())

        result = await xandr_mcp.xandr_execute_deal_from_prompt_inputs(
            name="Happy Path Deal",
            code="happy_path",
            buyer="The Trade Desk",
            insertion_order_name="Marketplace Pro",
            channel="display",
            geo_countries=["United States"],
        )

        assert result["success"] is True
        assert result["targeting_attached"] is True
        assert result["profile_id"] == 555
