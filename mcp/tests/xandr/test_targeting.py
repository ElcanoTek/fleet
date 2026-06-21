"""Unit + integration tests for the Xandr deal-targeting flow.

Covers helpers and integration added in `feat/xandr-deal-targeting`:

- `_resolve_xandr_device_type_id` (numeric passthrough + alias lookup)
- `_apply_xandr_channel_device_defaults`
- `_resolve_xandr_country` / `_resolve_xandr_region` / `_resolve_xandr_iab_category`
  / `_resolve_xandr_segment` / `_resolve_xandr_deal_list` (cached + candidate-aware)
- `_build_xandr_profile_payload` (profile JSON shape)
- End-to-end profile-attach: device + geo + IAB + segment + deal_list inputs
  resolve, build a profile, attach `profile_id` to the deal payload.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from xandr_mcp import (
    XANDR_DEVICE_VALUES_CTV,
    XANDR_DEVICE_VALUES_DISPLAY,
    XANDR_DEVICE_VALUES_OTT,
    XandrResolutionError,
    _apply_xandr_channel_device_defaults,
    _build_xandr_profile_payload,
    _resolve_xandr_country,
    _resolve_xandr_deal_list,
    _resolve_xandr_device_type_id,
    _resolve_xandr_iab_category,
    _resolve_xandr_region,
    _resolve_xandr_segment,
    xandr_execute_deal_from_prompt_inputs,
)

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_BASE_URL, XANDR_DEAL_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE


@pytest.fixture(autouse=True)
def _isolate_xandr_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    cache_root = tmp_path_factory.mktemp("xacache_t")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


def _envelope(key: str, items: list[dict]) -> dict:
    return {"response": {"status": "OK", "count": len(items), key: items, "dbg_info": {"instance": "x", "time": 1}}}


def _create_deal_response(deal_id: int) -> dict:
    return {
        "response": {
            "status": "OK",
            "deal": {
                "id": deal_id,
                "name": "Test",
                "code": "test",
                "state": "active",
                "deal_type": {"id": 2, "name": "Private Auction"},
                "buyers": [{"id": 999, "name": None}],
            },
        }
    }


class TestResolveDeviceTypeId:
    @pytest.mark.parametrize(
        "value,expected",
        [
            ("Desktop", 1),
            ("desktop", 1),
            ("DESKTOP", 1),
            ("Phone", 2),
            ("Mobile", 2),
            ("Tablet", 3),
            ("CTV", 4),
            ("Connected TV", 4),
            ("Set-top Box", 5),
            ("Game Console", 6),
            (1, 1),
            ("1", 1),
            (4, 4),
        ],
    )
    def test_resolves(self, value, expected):
        assert _resolve_xandr_device_type_id(value) == expected

    def test_unknown_name_raises(self):
        with pytest.raises(XandrResolutionError) as excinfo:
            _resolve_xandr_device_type_id("Unicycle")
        assert excinfo.value.code == "device_type_unresolved"

    def test_empty_string_raises(self):
        with pytest.raises(XandrResolutionError) as excinfo:
            _resolve_xandr_device_type_id("")
        assert excinfo.value.code == "device_type_invalid_input"


class TestApplyChannelDeviceDefaults:
    def test_explicit_passthrough(self):
        result, applied = _apply_xandr_channel_device_defaults([1, 2], "ctv")
        assert result == [1, 2]
        assert applied is False

    def test_display(self):
        result, applied = _apply_xandr_channel_device_defaults(None, "display")
        assert result == list(XANDR_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_olv_maps_to_display(self):
        result, applied = _apply_xandr_channel_device_defaults(None, "olv")
        assert result == list(XANDR_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_ctv(self):
        result, applied = _apply_xandr_channel_device_defaults(None, "ctv")
        assert result == list(XANDR_DEVICE_VALUES_CTV)
        assert applied is True

    def test_ott_is_phone_and_tablet(self):
        # OTT = app-only mobile/tablet video (phone+tablet), distinct from CTV.
        result, applied = _apply_xandr_channel_device_defaults(None, "ott")
        assert result == list(XANDR_DEVICE_VALUES_OTT) == [2, 3]
        assert applied is True

    def test_no_channel_no_devices(self):
        result, applied = _apply_xandr_channel_device_defaults(None, None)
        assert result is None
        assert applied is False

    def test_unknown_channel(self):
        result, applied = _apply_xandr_channel_device_defaults(None, "audio")
        assert result is None
        assert applied is False


class TestResolveCountry:
    @pytest.mark.asyncio
    async def test_numeric_passthrough(self):
        # No HTTP call when numeric
        result = await _resolve_xandr_country(232)
        assert result["id"] == 232

    @pytest.mark.asyncio
    async def test_resolves_by_name(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/country").mock(
            return_value=httpx.Response(
                200,
                json=_envelope("countries", [{"id": 232, "name": "United States", "code": "US"}]),
            )
        )
        result = await _resolve_xandr_country("United States")
        assert result == {"id": 232, "name": "United States", "code": "US"}

    @pytest.mark.asyncio
    async def test_unresolved_raises_with_candidates(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/country").mock(
            return_value=httpx.Response(200, json=_envelope("countries", []))
        )
        with pytest.raises(XandrResolutionError) as excinfo:
            await _resolve_xandr_country("Atlantis")
        assert excinfo.value.code in {"country_unresolved", "country_ambiguous"}


class TestResolveRegion:
    @pytest.mark.asyncio
    async def test_resolves_by_name(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/region").mock(
            return_value=httpx.Response(
                200,
                json=_envelope("regions", [{"id": 5024, "name": "California", "code": "CA"}]),
            )
        )
        result = await _resolve_xandr_region("California", country_id=232)
        assert result["id"] == 5024


class TestResolveIabCategory:
    @pytest.mark.asyncio
    async def test_resolves_by_name(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/content-category").mock(
            return_value=httpx.Response(
                200,
                json=_envelope("content-categories", [{"id": 51, "name": "Automotive", "code": "IAB2"}]),
            )
        )
        result = await _resolve_xandr_iab_category("Automotive")
        assert result["id"] == 51


class TestResolveSegment:
    @pytest.mark.asyncio
    async def test_resolves_by_name(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/segment").mock(
            return_value=httpx.Response(
                200,
                json=_envelope("segments", [{"id": 9001, "name": "Auto Intenders"}]),
            )
        )
        result = await _resolve_xandr_segment("Auto Intenders", member_id=9544)
        assert result["id"] == 9001


class TestResolveDealList:
    @pytest.mark.asyncio
    async def test_resolves_by_name(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/deal-list").mock(
            return_value=httpx.Response(
                200,
                json=_envelope("deal-lists", [{"id": 314, "name": "Premium Auto Inventory"}]),
            )
        )
        result = await _resolve_xandr_deal_list("Premium Auto Inventory", member_id=9544)
        assert result["id"] == 314


class TestBuildProfilePayload:
    def test_minimal_devices_only(self):
        payload = _build_xandr_profile_payload(
            name="X",
            member_id=9544,
            device_type_ids=[1, 2, 3],
            country_targets=None,
            region_targets=None,
            content_category_targets=None,
            segment_targets=None,
            deal_list_targets=None,
        )
        profile = payload["profile"]
        assert profile["description"] == "X"
        assert profile["member_id"] == 9544
        assert profile["device_type_targets"] == [
            {"device_type": 1},
            {"device_type": 2},
            {"device_type": 3},
        ]
        assert profile["device_type_action"] == "include"

    def test_full_targeting_shape(self):
        payload = _build_xandr_profile_payload(
            name="Full",
            member_id=9544,
            device_type_ids=[4, 5],
            country_targets=[{"id": 232, "name": "United States"}],
            region_targets=[{"id": 5024, "name": "California"}],
            content_category_targets=[{"id": 51, "name": "Automotive"}],
            segment_targets=[{"id": 9001, "name": "Auto Intenders"}],
            deal_list_targets=[{"id": 314, "name": "Premium"}],
        )
        profile = payload["profile"]
        assert profile["country_targets"] == [{"id": 232, "name": "United States"}]
        assert profile["region_targets"] == [{"id": 5024, "name": "California"}]
        # Per Microsoft Curate docs, IAB content categories MUST use the
        # platform_-prefixed field for curated-deal-line-item profiles.
        assert profile["platform_content_category_targets"] == [
            {"id": 51, "name": "Automotive", "action": "include"},
        ]
        assert profile["segment_targets"] == [{"id": 9001, "action": "include"}]
        assert profile["segment_boolean_operator"] == "or"
        assert profile["deal_list_targets"] == [{"id": 314, "action": "include"}]

    def test_no_targeting_returns_minimal(self):
        payload = _build_xandr_profile_payload(
            name="Empty",
            member_id=None,
            device_type_ids=None,
            country_targets=None,
            region_targets=None,
            content_category_targets=None,
            segment_targets=None,
            deal_list_targets=None,
        )
        profile = payload["profile"]
        assert "device_type_targets" not in profile
        assert "country_targets" not in profile


class TestExecuteWithFullTargeting:
    @pytest.mark.asyncio
    async def test_full_targeting_resolves_and_attaches_profile(self, mock_xandr_api: respx.MockRouter):
        captured = {}

        def capture_profile(request: httpx.Request) -> httpx.Response:
            captured["profile"] = json.loads(request.content)
            return httpx.Response(200, json={"response": {"status": "OK", "profile": {"id": 7777}}})

        def capture_deal(request: httpx.Request) -> httpx.Response:
            captured["deal"] = json.loads(request.content)
            return httpx.Response(200, json=_create_deal_response(deal_id=5050))

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/country").mock(
            return_value=httpx.Response(200, json=_envelope("countries", [{"id": 232, "name": "United States"}]))
        )
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/region").mock(
            return_value=httpx.Response(200, json=_envelope("regions", [{"id": 5024, "name": "California"}]))
        )
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/content-category").mock(
            return_value=httpx.Response(200, json=_envelope("content-categories", [{"id": 51, "name": "Automotive"}]))
        )
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/segment").mock(
            return_value=httpx.Response(200, json=_envelope("segments", [{"id": 9001, "name": "Auto Intenders"}]))
        )
        mock_xandr_api.post(f"{XANDR_BASE_URL}/profile").mock(side_effect=capture_profile)
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_deal)
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "response": {
                        "status": "OK",
                        "deal": {
                            "id": 5050,
                            "name": "Test",
                            "code": "test",
                            "deal_type": {"id": 5, "name": "Curated"},
                            "buyers": [{"id": 999, "name": None}],
                        },
                    }
                },
            )
        )
        # Line-item create — actually carries the curator margin and ties
        # everything (deal + IO + profile) together. This is the resource
        # that shows up as a "deal" in the Curate UI.
        captured_line_item: dict = {}

        def capture_line_item(request: httpx.Request) -> httpx.Response:
            captured_line_item.update(json.loads(request.content))
            return httpx.Response(
                200,
                json={
                    "response": {
                        "status": "OK",
                        "line-item": {
                            "id": 8800,
                            "deals": [{"id": 5050}],
                            "line_item_subtype": "standard_curated",
                            "active": True,
                        },
                    }
                },
            )

        mock_xandr_api.post(f"{XANDR_BASE_URL}/line-item").mock(side_effect=capture_line_item)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Full Targeting Test",
            code="full_targeting_test",
            buyer=999,
            insertion_order_name="Marketplace Pro",
            device_types=["Desktop", "Phone", "Tablet"],
            geo_countries=["United States"],
            geo_states=["California"],
            iab_categories=["Automotive"],
            segment_names=["Auto Intenders"],
        )

        assert result["success"] is True
        assert result["profile_id"] == 7777
        assert result["line_item_id"] == 8800
        assert result["advertiser_id"] == 11447334  # Elcano_Marketplaces (parent of Marketplace Pro)
        assert result["insertion_order_id"] == 11443185  # Elcano – Marketplace Pro
        # Profile payload includes all resolved targeting; IAB uses the
        # platform_-prefixed Curate-required field.
        profile = captured["profile"]["profile"]
        assert profile["device_type_targets"] == [
            {"device_type": 1},
            {"device_type": 2},
            {"device_type": 3},
        ]
        assert profile["country_targets"] == [{"id": 232, "name": "United States"}]
        assert profile["region_targets"] == [{"id": 5024, "name": "California"}]
        assert profile["platform_content_category_targets"] == [
            {"id": 51, "name": "Automotive", "action": "include"},
        ]
        assert profile["segment_targets"] == [{"id": 9001, "action": "include"}]
        # Profile_id lives on the LINE ITEM, not the deal (Curate convention)
        assert "profile_id" not in captured["deal"]["deal"]
        line_item = captured_line_item["line-item"]
        assert line_item["profile_id"] == 7777
        assert line_item["deals"] == [{"id": 5050}]
        assert line_item["insertion_orders"] == [{"id": 11443185}]
        assert line_item["line_item_subtype"] == "standard_curated"
        assert line_item["valuation"]["min_margin_pct"] == 30.0  # Elcano default

    @pytest.mark.asyncio
    async def test_unresolved_country_emits_flag_but_does_not_block_create(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=f"{XANDR_BASE_URL}/country").mock(
            return_value=httpx.Response(200, json=_envelope("countries", []))
        )
        # No /profile mock — none should be created since country resolution failed and no other targeting
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_create_deal_response(deal_id=6060))
        )
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_create_deal_response(deal_id=6060))
        )
        # Line-item still gets created (curator margin still applies) even
        # without a profile.
        mock_xandr_api.post(f"{XANDR_BASE_URL}/line-item").mock(
            return_value=httpx.Response(
                200,
                json={
                    "response": {
                        "status": "OK",
                        "line-item": {"id": 8801, "deals": [{"id": 6060}], "line_item_subtype": "standard_curated"},
                    }
                },
            )
        )

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Partial",
            code="partial",
            buyer=999,
            insertion_order_name="Marketplace Pro",
            geo_countries=["Atlantis"],
        )
        assert result["success"] is True
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_unresolved_country" in flag_names
        assert result["profile_id"] is None
        # Line item still created with the default 30% margin even without
        # a profile (curator margin is independent of targeting).
        assert result["line_item_id"] == 8801
