"""Tests for the Microsoft Curate (Xandr) line-item rewrite.

Covers helpers and integration added in `feat/xandr-curate-line-item-rewrite`:

- `_resolve_xandr_insertion_order` (name -> id with seed-catalog lookup)
- `_resolve_xandr_curator_margin` (default + percent + cpm + mutual-exclusion)
- `_xandr_ad_types_from_channel` (channel -> ad_types mapping)
- `_build_xandr_line_item_payload` (curated-deal-line-item shape)
- End-to-end: IO required, deal_type=Curated (id=5, version=2), profile attaches
  via line item, line item created with the right margin/IO/advertiser.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from xandr_mcp import (
    ELCANO_DEFAULT_XANDR_MARGIN_PERCENT,
    ELCANO_XANDR_DEFAULT_ADVERTISER_ID,
    ELCANO_XANDR_MEMBER_ID,
    XANDR_DEAL_TYPE_ID_CURATED,
    XANDR_DEAL_VERSION_CURATED,
    XANDR_INSERTION_ORDER_SEED_CATALOG,
    XANDR_LINE_ITEM_SUBTYPE_CURATED,
    XANDR_MARGIN_TYPE_CPM,
    XANDR_MARGIN_TYPE_PERCENTAGE,
    XANDR_REVENUE_TYPE_DYNAMIC_CPM,
    XandrResolutionError,
    _build_xandr_line_item_payload,
    _resolve_xandr_curator_margin,
    _resolve_xandr_insertion_order,
    _xandr_ad_types_from_channel,
    xandr_execute_deal_from_prompt_inputs,
)

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_BASE_URL, XANDR_DEAL_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE


@pytest.fixture(autouse=True)
def _isolate_xandr_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    cache_root = tmp_path_factory.mktemp("xacache_curate")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


XANDR_LINE_ITEM_ENDPOINT = f"{XANDR_BASE_URL}/line-item"


def _deal_response(deal_id: int) -> dict:
    return {
        "response": {
            "status": "OK",
            "deal": {
                "id": deal_id,
                "name": "Curate Smoke",
                "code": "curate_smoke",
                "deal_type": {"id": 5, "name": "Curated"},
                "buyers": [{"id": 999, "name": None}],
            },
        }
    }


def _line_item_response(line_item_id: int, deal_id: int) -> dict:
    return {
        "response": {
            "status": "OK",
            "line-item": {
                "id": line_item_id,
                "deals": [{"id": deal_id}],
                "line_item_subtype": "standard_curated",
                "active": True,
            },
        }
    }


class TestConstantsAndCatalog:
    def test_member_and_advertiser(self):
        assert ELCANO_XANDR_MEMBER_ID == 17094
        assert ELCANO_XANDR_DEFAULT_ADVERTISER_ID == 11447334

    def test_curated_deal_type_constants(self):
        assert XANDR_DEAL_TYPE_ID_CURATED == 5
        assert XANDR_DEAL_VERSION_CURATED == 2
        assert XANDR_LINE_ITEM_SUBTYPE_CURATED == "standard_curated"

    def test_seed_catalog_has_marketplace_pro(self):
        names = {entry["name"] for entry in XANDR_INSERTION_ORDER_SEED_CATALOG}
        assert "Elcano – Marketplace Pro" in names
        assert "Elcano - Premium Marketplaces" in names
        assert "Elcano - Yahoo Marketplace" in names


class TestResolveInsertionOrder:
    @pytest.mark.asyncio
    async def test_numeric_passthrough(self):
        result = await _resolve_xandr_insertion_order(11443185)
        assert result["id"] == 11443185
        assert result["advertiser_id"] == 11447334
        assert result["name"] == "Elcano – Marketplace Pro"

    @pytest.mark.asyncio
    async def test_name_resolves_with_endash_tolerance(self):
        # The catalog entry uses an en-dash (–), but the user might type
        # a plain hyphen (-).
        result = await _resolve_xandr_insertion_order("Elcano - Marketplace Pro")
        assert result["id"] == 11443185

    @pytest.mark.asyncio
    async def test_name_case_insensitive(self):
        result = await _resolve_xandr_insertion_order("ELCANO - PREMIUM MARKETPLACES")
        assert result["id"] == 11316257

    @pytest.mark.asyncio
    async def test_substring_match_promoted_to_exact(self):
        # "Marketplace Pro" doesn't exactly match anything, but only one
        # entry contains it as a substring → promoted to exact.
        result = await _resolve_xandr_insertion_order("Marketplace Pro")
        assert result["id"] == 11443185

    @pytest.mark.asyncio
    async def test_inactive_io_raises(self):
        # "Elcano - Soundwave" is marked inactive in the catalog.
        with pytest.raises(XandrResolutionError) as excinfo:
            await _resolve_xandr_insertion_order("Elcano - Soundwave")
        assert excinfo.value.code == "insertion_order_inactive"

    @pytest.mark.asyncio
    async def test_unknown_name_raises_with_candidates(self):
        with pytest.raises(XandrResolutionError) as excinfo:
            await _resolve_xandr_insertion_order("Nonexistent IO XYZ")
        assert excinfo.value.code in {"insertion_order_unresolved", "insertion_order_ambiguous"}

    @pytest.mark.asyncio
    async def test_empty_string_raises(self):
        with pytest.raises(XandrResolutionError) as excinfo:
            await _resolve_xandr_insertion_order("")
        assert excinfo.value.code == "insertion_order_invalid_input"

    @pytest.mark.asyncio
    async def test_unknown_numeric_id_passes_through(self):
        # Unknown id is allowed (escape hatch for newly-created IOs not in
        # the seed catalog) — caller will fail at the line-item create step
        # if the id is bad.
        result = await _resolve_xandr_insertion_order(99999999)
        assert result["id"] == 99999999
        assert result["advertiser_id"] is None


class TestResolveCuratorMargin:
    def test_default_when_both_none(self):
        margin_type, margin_value, applied_default = _resolve_xandr_curator_margin(None, None)
        assert margin_type == XANDR_MARGIN_TYPE_PERCENTAGE
        assert margin_value == ELCANO_DEFAULT_XANDR_MARGIN_PERCENT
        assert applied_default is True

    def test_explicit_percent(self):
        margin_type, margin_value, applied_default = _resolve_xandr_curator_margin(15.0, None)
        assert margin_type == XANDR_MARGIN_TYPE_PERCENTAGE
        assert margin_value == 15.0
        assert applied_default is False

    def test_explicit_cpm(self):
        margin_type, margin_value, applied_default = _resolve_xandr_curator_margin(None, 1.50)
        assert margin_type == XANDR_MARGIN_TYPE_CPM
        assert margin_value == 1.5
        assert applied_default is False

    def test_both_supplied_raises(self):
        with pytest.raises(ValueError, match="mutually exclusive"):
            _resolve_xandr_curator_margin(15.0, 1.5)


class TestAdTypesFromChannel:
    @pytest.mark.parametrize(
        "channel,expected",
        [
            ("display", ["banner"]),
            ("DISPLAY", ["banner"]),
            ("olv", ["video"]),
            ("OLV", ["video"]),
            ("ctv", ["video"]),
            ("CTV", ["video"]),
            ("ott", ["video"]),
            ("OTT", ["video"]),
            (None, ["banner"]),
        ],
    )
    def test_maps(self, channel, expected):
        assert _xandr_ad_types_from_channel(channel) == expected


class TestBuildLineItemPayload:
    def test_minimal_percentage_margin(self):
        payload = _build_xandr_line_item_payload(
            name="X (Elcano line item)",
            deal_id=5050,
            insertion_order_id=11443185,
            profile_id=7777,
            margin_type="percentage",
            margin_value=30.0,
            revenue_type="vcpm",
            revenue_value=None,
            floor_price=2.50,
            ad_types=["banner"],
            start_date="2026-05-01 00:00:00",
            end_date=None,
        )
        line_item = payload["line-item"]
        assert line_item["line_item_subtype"] == "standard_curated"
        assert line_item["deals"] == [{"id": 5050}]
        assert line_item["insertion_orders"] == [{"id": 11443185}]
        assert line_item["profile_id"] == 7777
        assert line_item["valuation"]["min_margin_pct"] == 30.0
        assert line_item["valuation"]["min_margin_cpm"] is None
        assert line_item["valuation"]["min_revenue_value"] == 2.50  # floor for vcpm
        assert line_item["revenue_type"] == "vcpm"
        assert line_item["revenue_value"] is None
        assert line_item["ad_types"] == ["banner"]
        assert line_item["auction_event"] == {
            "kpi_auction_type_id": 1,
            "payment_auction_type_id": 1,
            "revenue_auction_type_id": 1,
        }
        assert line_item["supply_strategies"] == {"managed": False, "rtb": False, "deals": True}
        assert line_item["state"] == "active"

    def test_cpm_fixed_revenue_type(self):
        payload = _build_xandr_line_item_payload(
            name="X",
            deal_id=5050,
            insertion_order_id=11443185,
            profile_id=None,
            margin_type="cpm",
            margin_value=1.5,
            revenue_type="cpm",
            revenue_value=4.50,
            floor_price=None,
            ad_types=["video"],
            start_date="2026-05-01 00:00:00",
            end_date="2026-05-31 23:59:59",
        )
        line_item = payload["line-item"]
        assert line_item["valuation"]["min_margin_cpm"] == 1.5
        assert line_item["valuation"]["min_margin_pct"] is None
        assert line_item["valuation"]["min_revenue_value"] is None
        assert line_item["revenue_value"] == 4.50  # fixed-price rate
        assert line_item["revenue_type"] == "cpm"
        assert line_item["budget_intervals"][0]["end_date"] == "2026-05-31 23:59:59"
        assert "profile_id" not in line_item


class TestExecuteCurateFullFlow:
    @pytest.mark.asyncio
    async def test_full_curate_flow_creates_deal_and_line_item(self, mock_xandr_api: respx.MockRouter):
        captured: dict = {}

        def capture_deal(request: httpx.Request) -> httpx.Response:
            captured["deal"] = json.loads(request.content)
            return httpx.Response(200, json=_deal_response(deal_id=5050))

        def capture_line_item(request: httpx.Request) -> httpx.Response:
            captured["line_item"] = json.loads(request.content)
            captured["line_item_url"] = str(request.url)
            return httpx.Response(200, json=_line_item_response(line_item_id=8800, deal_id=5050))

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_deal)
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_deal_response(deal_id=5050))
        )
        mock_xandr_api.post(XANDR_LINE_ITEM_ENDPOINT).mock(side_effect=capture_line_item)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Curate Smoke Test",
            code="curate_smoke",
            buyer=999,
            insertion_order_name="Marketplace Pro",
        )

        assert result["success"] is True
        assert result["line_item_id"] == 8800
        assert result["advertiser_id"] == 11447334
        assert result["insertion_order_id"] == 11443185

        # Deal payload uses Curated type + version=2
        deal_payload = captured["deal"]["deal"]
        assert deal_payload["type"]["id"] == XANDR_DEAL_TYPE_ID_CURATED
        assert deal_payload["type"]["name"] == "Curated"
        assert deal_payload["version"] == XANDR_DEAL_VERSION_CURATED
        assert deal_payload["member_id"] == ELCANO_XANDR_MEMBER_ID
        assert "profile_id" not in deal_payload  # profile attaches via line item, not deal

        # Line item carries the margin + references everything
        line_item_payload = captured["line_item"]["line-item"]
        assert line_item_payload["line_item_subtype"] == XANDR_LINE_ITEM_SUBTYPE_CURATED
        assert line_item_payload["deals"] == [{"id": 5050}]
        assert line_item_payload["insertion_orders"] == [{"id": 11443185}]
        assert line_item_payload["valuation"]["min_margin_pct"] == 30.0  # default
        assert line_item_payload["revenue_type"] == XANDR_REVENUE_TYPE_DYNAMIC_CPM

        # Line-item POST includes advertiser_id query param (required by Xandr)
        assert "advertiser_id=11447334" in captured["line_item_url"]

        # Default 30% margin flag emitted
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_default_curator_margin_applied" in flag_names

    @pytest.mark.asyncio
    async def test_missing_io_emits_validation_error(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=999,
            # No insertion_order_name or insertion_order_id
        )
        assert result["success"] is False
        assert result["phase"] == "validate"
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_missing_insertion_order" in flag_names

    @pytest.mark.asyncio
    async def test_inactive_io_emits_resolve_error(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=999,
            insertion_order_name="Elcano - Soundwave",  # marked inactive in seed catalog
        )
        assert result["success"] is False
        assert result["phase"] == "resolve"
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_unresolved_insertion_order" in flag_names

    @pytest.mark.asyncio
    async def test_explicit_cpm_margin_overrides_default(self, mock_xandr_api: respx.MockRouter):
        captured: dict = {}

        def capture_line_item(request: httpx.Request) -> httpx.Response:
            captured["line_item"] = json.loads(request.content)
            return httpx.Response(200, json=_line_item_response(line_item_id=8801, deal_id=5051))

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(return_value=httpx.Response(200, json=_deal_response(5051)))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_deal_response(5051))
        )
        mock_xandr_api.post(XANDR_LINE_ITEM_ENDPOINT).mock(side_effect=capture_line_item)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test", code="test", buyer=999, insertion_order_name="Marketplace Pro", margin_cpm=1.50
        )

        assert result["success"] is True
        # No default-margin flag (caller was explicit)
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_default_curator_margin_applied" not in flag_names
        # CPM margin propagated through
        valuation = captured["line_item"]["line-item"]["valuation"]
        assert valuation["min_margin_cpm"] == 1.5
        assert valuation["min_margin_pct"] is None
