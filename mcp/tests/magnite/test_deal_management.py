"""Tests for the Magnite ClearLine Curation Demand Management MCP tools."""

import base64
import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from magnite_mcp import (
    magnite_activate_deal,
    magnite_create_deal,
    magnite_create_prepared_deal,
    magnite_create_rtd_signal,
    magnite_deactivate_deal,
    magnite_execute_deal_from_prompt_inputs,
    magnite_get_deal,
    magnite_list_audience_segments,
    magnite_list_dsp_buyers,
    magnite_list_dsps,
    magnite_list_geo_values,
    magnite_list_marketplaces,
    magnite_list_publishers,
    magnite_list_rtd_signals,
    magnite_prepare_deal_from_prompt_inputs,
    magnite_update_deal,
)

from .conftest import MAGNITE_DMG_BASE_URL, MAGNITE_DMG_DEALS_ENDPOINT


def _page(content: list[dict]) -> dict:
    return {
        "page": {"number": 1, "size": len(content), "totalElements": len(content), "totalPages": 1},
        "content": content,
    }


MARKETPLACES_RESPONSE = _page(
    [{"id": 31358, "source": "SpringServe", "name": "Elcano CTV Marketplace", "status": "Active"}]
)
DSPS_RESPONSE = _page(
    [
        {"id": 16, "source": "SpringServe", "name": "The Trade Desk"},
        {"id": 416, "source": "SpringServe", "name": "DV360"},
    ]
)
BUYERS_DSP16_RESPONSE = _page([{"id": 12915, "source": "SpringServe", "name": "Elcano Seat", "code": "tok-1"}])
PUBLISHERS_RESPONSE = _page(
    [
        {
            "id": 61648,
            "name": "Test Seat 1",
            "source": "SpringServe",
            "minimumPriceFloor": {"cpm": 3, "currency": "USD"},
        },
        {
            "id": 61611,
            "name": "Supply Test Seat",
            "source": "SpringServe",
            "minimumPriceFloor": {"cpm": 12, "currency": "USD"},
        },
    ]
)
CREATED_DEAL_RESPONSE = {
    "id": "MGNI-CD-2002-100",
    "name": "CTV Curation Deal 2026",
    "source": "SpringServe",
    "type": "Curator",
    "status": "Active",
    "marketplace": {"id": 31358},
}

MINIMAL_CREATE_PAYLOAD = {
    "name": "CTV Curation Deal 2026",
    "marketplace": {"id": 31358},
    "startDate": "2026-06-15",
    "endDate": "2026-12-31",
    "dsps": [{"id": 16, "buyers": [{"id": 12915}]}],
    "curatorPricing": {
        "revShareModel": "CPM",
        "currency": "USD",
        "publisherRevShares": [{"value": 4.50, "publishers": [{"id": 61648}]}],
    },
}


def _assert_basic_auth(request: httpx.Request) -> None:
    expected = base64.b64encode(b"test-access-key-12345:test-secret-key-67890").decode()
    assert request.headers.get("Authorization") == f"Basic {expected}"


class TestCreateDeal:
    """Tests for the low-level magnite_create_deal tool."""

    @pytest.mark.asyncio
    async def test_create_deal_success(self, mock_magnite_dmg_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json=CREATED_DEAL_RESPONSE)

        mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_create_deal(source="SpringServe", payload=MINIMAL_CREATE_PAYLOAD)

        assert result["success"] is True
        assert result["deal_id"] == "MGNI-CD-2002-100"

        assert captured_request is not None
        _assert_basic_auth(captured_request)
        assert captured_request.url.params.get("account") == "mp-vendor/102"
        assert captured_request.url.params.get("source") == "SpringServe"

        sent = json.loads(captured_request.content)
        assert sent["type"] == "Curator"  # defaulted
        assert sent["startDate"] == "2026-06-15T00:00:00Z"  # date-only normalized to start of day
        assert sent["endDate"] == "2026-12-31T23:59:59Z"  # date-only normalized to end of day

    @pytest.mark.asyncio
    async def test_create_deal_rejects_payload_missing_minimums(self, mock_magnite_dmg_api: respx.MockRouter):
        route = mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(201, json=CREATED_DEAL_RESPONSE)
        )

        result = await magnite_create_deal(source="SpringServe", payload={"name": "incomplete"})

        assert result["success"] is False
        details = result["error"]["details"]
        assert any("marketplace.id" in issue for issue in details)
        assert any("dsps" in issue for issue in details)
        assert any("curatorPricing" in issue for issue in details)
        assert not route.called  # never hit the network with an invalid payload

    @pytest.mark.asyncio
    async def test_create_deal_encodes_dvplus_source(self, mock_magnite_dmg_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json=CREATED_DEAL_RESPONSE)

        mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(side_effect=capture)

        # Channel alias "display" routes to DV+; the + must be %2B in the URL.
        result = await magnite_create_deal(source="display", payload=MINIMAL_CREATE_PAYLOAD)

        assert result["success"] is True
        assert captured_request is not None
        assert captured_request.url.params.get("source") == "DV+"
        assert "source=DV%2B" in str(captured_request.url)

    @pytest.mark.asyncio
    async def test_create_deal_rejects_unknown_source(self, mock_magnite_dmg_api: respx.MockRouter):  # noqa: ARG002
        result = await magnite_create_deal(source="telaria", payload=MINIMAL_CREATE_PAYLOAD)
        assert result["success"] is False
        assert "Unsupported source" in result["error"]["message"]


class TestDealLifecycle:
    """Retrieve / update / activate / deactivate."""

    @pytest.mark.asyncio
    async def test_get_deal(self, mock_magnite_dmg_api: respx.MockRouter):
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100").mock(
            return_value=httpx.Response(200, json=CREATED_DEAL_RESPONSE)
        )
        result = await magnite_get_deal(deal_id="MGNI-CD-2002-100", source="SpringServe")
        assert result["success"] is True
        assert result["deal"]["id"] == "MGNI-CD-2002-100"

    @pytest.mark.asyncio
    async def test_update_deal_partial_payload(self, mock_magnite_dmg_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATED_DEAL_RESPONSE)

        mock_magnite_dmg_api.put(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100").mock(side_effect=capture)

        result = await magnite_update_deal(
            deal_id="MGNI-CD-2002-100",
            source="ctv",
            payload={"endDate": "2027-01-31", "contextual": None},
        )

        assert result["success"] is True
        assert captured_request is not None
        sent = json.loads(captured_request.content)
        assert sent["endDate"] == "2027-01-31T23:59:59Z"
        assert sent["contextual"] is None  # explicit nullification passes through

    @pytest.mark.asyncio
    async def test_update_deal_requires_payload(self, mock_magnite_dmg_api: respx.MockRouter):  # noqa: ARG002
        result = await magnite_update_deal(deal_id="MGNI-CD-2002-100", source="SpringServe", payload={})
        assert result["success"] is False
        assert "payload is required" in result["error"]["message"]

    @pytest.mark.asyncio
    async def test_activate_and_deactivate(self, mock_magnite_dmg_api: respx.MockRouter):
        activate_route = mock_magnite_dmg_api.post(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100/activate").mock(
            return_value=httpx.Response(200, json={**CREATED_DEAL_RESPONSE, "status": "Active"})
        )
        deactivate_route = mock_magnite_dmg_api.post(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100/deactivate").mock(
            return_value=httpx.Response(200, json={**CREATED_DEAL_RESPONSE, "status": "Inactive"})
        )

        activated = await magnite_activate_deal(deal_id="MGNI-CD-2002-100", source="SpringServe")
        deactivated = await magnite_deactivate_deal(deal_id="MGNI-CD-2002-100", source="SpringServe")

        assert activated["success"] is True
        assert deactivated["success"] is True
        assert activate_route.called
        assert deactivate_route.called


class TestReferenceData:
    """Marketplaces, DSPs, buyers, publishers, metadata, RTD signals."""

    @pytest.mark.asyncio
    async def test_list_marketplaces_unwraps_content(self, mock_magnite_dmg_api: respx.MockRouter):
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/marketplaces").mock(
            return_value=httpx.Response(200, json=MARKETPLACES_RESPONSE)
        )
        result = await magnite_list_marketplaces(source="SpringServe")
        assert result["success"] is True
        assert result["marketplaces"][0]["id"] == 31358
        assert result["page"]["totalElements"] == 1

    @pytest.mark.asyncio
    async def test_list_dsps_and_buyers(self, mock_magnite_dmg_api: respx.MockRouter):
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps").mock(
            return_value=httpx.Response(200, json=DSPS_RESPONSE)
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps/16/buyers").mock(
            return_value=httpx.Response(200, json=BUYERS_DSP16_RESPONSE)
        )
        dsps = await magnite_list_dsps(source="SpringServe")
        buyers = await magnite_list_dsp_buyers(dsp_id=16, source="SpringServe")
        assert [d["name"] for d in dsps["dsps"]] == ["The Trade Desk", "DV360"]
        assert buyers["buyers"][0]["code"] == "tok-1"

    @pytest.mark.asyncio
    async def test_list_publishers_passes_marketplace_and_size_ids(self, mock_magnite_dmg_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=PUBLISHERS_RESPONSE)

        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/publishers").mock(side_effect=capture)

        result = await magnite_list_publishers(source="display", marketplace_id=31358, size_ids=[2, 9])

        assert result["success"] is True
        assert captured_request is not None
        assert captured_request.url.params.get("marketplaceId") == "31358"
        assert captured_request.url.params.get("sizeIds") == "2,9"
        assert result["publishers"][0]["minimumPriceFloor"]["cpm"] == 3

    @pytest.mark.asyncio
    async def test_audience_segments_blocked_on_dvplus(self, mock_magnite_dmg_api: respx.MockRouter):
        route = mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/metadata/audience-segments").mock(
            return_value=httpx.Response(200, json=_page([]))
        )
        result = await magnite_list_audience_segments(source="DV+")
        assert result["success"] is False
        assert "SpringServe" in result["error"]["message"]
        assert not route.called  # the v2.0 API has no DV+ audience endpoint to hit

    @pytest.mark.asyncio
    async def test_list_geo_values_validates_kind(self, mock_magnite_dmg_api: respx.MockRouter):  # noqa: ARG002
        result = await magnite_list_geo_values(kind="planets", source="SpringServe")
        assert result["success"] is False
        assert "kind must be one of" in result["error"]["message"]

    @pytest.mark.asyncio
    async def test_list_geo_values_passes_search(self, mock_magnite_dmg_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=_page([{"value": "US", "label": "United States (US)"}]))

        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/metadata/countries").mock(side_effect=capture)

        result = await magnite_list_geo_values(kind="countries", source="SpringServe", size=100, search="united")

        assert result["success"] is True
        assert captured_request is not None
        assert captured_request.url.params.get("size") == "100"
        assert captured_request.url.params.get("search") == "united"

    @pytest.mark.asyncio
    async def test_rtd_signal_create_and_list(self, mock_magnite_dmg_api: respx.MockRouter):
        mock_magnite_dmg_api.post(f"{MAGNITE_DMG_BASE_URL}/api/v1/real-time-data-signals").mock(
            return_value=httpx.Response(201, json={"id": 7, "source": "DV+", "value": "rtdsignalvalue"})
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/real-time-data-signals").mock(
            return_value=httpx.Response(200, json=_page([{"id": 7, "source": "DV+", "value": "rtdsignalvalue"}]))
        )
        created = await magnite_create_rtd_signal(source="DV+", value="rtdsignalvalue")
        listed = await magnite_list_rtd_signals(source="DV+")
        assert created["success"] is True
        assert created["signal"]["id"] == 7
        assert listed["signals"][0]["value"] == "rtdsignalvalue"

    @pytest.mark.asyncio
    async def test_rtd_signal_requires_value(self, mock_magnite_dmg_api: respx.MockRouter):  # noqa: ARG002
        result = await magnite_create_rtd_signal(source="DV+", value="  ")
        assert result["success"] is False
        assert "value is required" in result["error"]["message"]


def _mock_reference_catalog(mock: respx.MockRouter) -> None:
    """Wire up the reference-data endpoints the prepare flow resolves against."""
    mock.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/marketplaces").mock(
        return_value=httpx.Response(200, json=MARKETPLACES_RESPONSE)
    )
    mock.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps").mock(return_value=httpx.Response(200, json=DSPS_RESPONSE))
    mock.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps/16/buyers").mock(
        return_value=httpx.Response(200, json=BUYERS_DSP16_RESPONSE)
    )
    mock.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/publishers").mock(
        return_value=httpx.Response(200, json=PUBLISHERS_RESPONSE)
    )


class TestPromptInputsFlow:
    """prepare / create_prepared / execute_deal_from_prompt_inputs."""

    @pytest.mark.asyncio
    async def test_execute_deal_happy_path(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json=CREATED_DEAL_RESPONSE)

        mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(side_effect=capture)
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100").mock(
            return_value=httpx.Response(200, json=CREATED_DEAL_RESPONSE)
        )

        result = await magnite_execute_deal_from_prompt_inputs(
            deal_name="CTV Curation Deal 2026",
            marketplace="Elcano CTV Marketplace",
            dsps=["The Trade Desk"],
            publishers=["Test Seat 1"],
            channel="ctv",
            start_date="2026-06-15",
            end_date="2026-12-31",
            floor=25.0,
        )

        assert result["success"] is True
        assert result["deal_id"] == "MGNI-CD-2002-100"
        assert result["phase"] == "verify"
        assert result["verification"]["success"] is True
        assert result["deal_url"] is None  # no per-deal console URL documented

    @pytest.mark.asyncio
    async def test_execute_deal_payload_assembly_and_defaults(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json=CREATED_DEAL_RESPONSE)

        mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(side_effect=capture)
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100").mock(
            return_value=httpx.Response(200, json=CREATED_DEAL_RESPONSE)
        )

        result = await magnite_execute_deal_from_prompt_inputs(
            deal_name="CTV Curation Deal 2026",
            marketplace="Elcano CTV Marketplace",
            dsps=["The Trade Desk"],
            publishers=["Test Seat 1"],
            channel="ctv",
            start_date="2026-06-15",
            end_date="2026-12-31",
            floor=25.0,
        )

        assert result["success"] is True
        assert captured_request is not None
        assert captured_request.url.params.get("source") == "SpringServe"

        sent = json.loads(captured_request.content)
        assert sent["name"] == "CTV Curation Deal 2026"
        assert sent["type"] == "Curator"
        assert sent["marketplace"] == {"id": 31358}
        assert sent["startDate"] == "2026-06-15T00:00:00Z"
        assert sent["endDate"] == "2026-12-31T23:59:59Z"
        # Single-buyer DSP auto-selected its buyer.
        assert sent["dsps"] == [{"id": 16, "buyers": [{"id": 12915}]}]
        pricing = sent["curatorPricing"]
        assert pricing["priceType"] == "CPM"  # floor implies CPM pricing
        assert pricing["priceBehavior"] == "Auction"  # default: CPM acts as a floor
        group = pricing["publisherRevShares"][0]
        assert group["cpm"] == 25.0
        assert group["value"] == 0.30  # default 30% curator margin (fraction scale)
        assert group["publishers"] == [{"id": 61648}]

        flags = {flag["flag"] for flag in result["quality_flags"]}
        assert "magnite_default_curator_rev_share_applied" in flags
        assert "magnite_default_price_behavior_applied" in flags
        assert "magnite_single_buyer_auto_selected" in flags

    @pytest.mark.asyncio
    async def test_execute_blocks_on_unresolved_publisher(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        create_route = mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(201, json=CREATED_DEAL_RESPONSE)
        )

        result = await magnite_execute_deal_from_prompt_inputs(
            deal_name="CTV Curation Deal 2026",
            marketplace="Elcano CTV Marketplace",
            dsps=["The Trade Desk"],
            publishers=["No Such Publisher"],
            channel="ctv",
            end_date="2026-12-31",
            floor=25.0,
        )

        assert result["success"] is False
        assert result["phase"] == "prepare"
        blockers = {blocker["code"] for blocker in result["preparation"]["blockers"]}
        assert "publisher_unresolved" in blockers
        assert not create_route.called  # blocked deals never reach the API

    @pytest.mark.asyncio
    async def test_execute_blocks_dvplus_audience_segments(self, mock_magnite_dmg_api: respx.MockRouter):
        # DV+ catalog lookups still resolve, but audience targeting must block:
        # the v2.0 API only supports audiences on SpringServe.
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/marketplaces").mock(
            return_value=httpx.Response(200, json=_page([{"id": 100, "source": "DV+", "name": "Elcano DV+ MP"}]))
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps").mock(
            return_value=httpx.Response(200, json=DSPS_RESPONSE)
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps/16/buyers").mock(
            return_value=httpx.Response(200, json=BUYERS_DSP16_RESPONSE)
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/publishers").mock(
            return_value=httpx.Response(200, json=PUBLISHERS_RESPONSE)
        )
        create_route = mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(201, json=CREATED_DEAL_RESPONSE)
        )

        result = await magnite_execute_deal_from_prompt_inputs(
            deal_name="Display Deal",
            marketplace="Elcano DV+ MP",
            dsps=["The Trade Desk"],
            publishers=["Test Seat 1"],
            channel="display",
            end_date="2026-12-31",
            floor=2.0,
            audience_segments=["My Segment1"],
        )

        assert result["success"] is False
        blockers = {blocker["code"] for blocker in result["preparation"]["blockers"]}
        assert "audience_segments_unsupported_on_dvplus" in blockers
        assert not create_route.called

    @pytest.mark.asyncio
    async def test_floor_below_publisher_minimum_warns(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)

        result = await magnite_prepare_deal_from_prompt_inputs(
            deal_name="Cheap Deal",
            marketplace=31358,
            dsps=[{"dsp": "The Trade Desk", "buyers": ["Elcano Seat"]}],
            publishers=["Test Seat 1"],  # minimumPriceFloor cpm = 3
            channel="ctv",
            end_date="2026-12-31",
            floor=2.5,
        )

        assert result["success"] is True
        assert result["ready_to_create"] is True
        assert any("minimum price floor" in warning for warning in result["warnings"])

    @pytest.mark.asyncio
    async def test_create_prepared_deal_consume_once(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        create_route = mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(201, json=CREATED_DEAL_RESPONSE)
        )
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_DEALS_ENDPOINT}/MGNI-CD-2002-100").mock(
            return_value=httpx.Response(200, json=CREATED_DEAL_RESPONSE)
        )

        prepared = await magnite_prepare_deal_from_prompt_inputs(
            deal_name="CTV Curation Deal 2026",
            marketplace="Elcano CTV Marketplace",
            dsps=["The Trade Desk"],
            publishers=["Test Seat 1"],
            channel="ctv",
            end_date="2026-12-31",
            floor=25.0,
        )
        assert prepared["ready_to_create"] is True

        first = await magnite_create_prepared_deal(prepared["prepared_deal_id"])
        second = await magnite_create_prepared_deal(prepared["prepared_deal_id"])

        assert first["success"] is True
        assert first["deal_id"] == "MGNI-CD-2002-100"
        assert second["success"] is True
        assert second.get("replayed") is True  # no duplicate live deal
        assert create_route.call_count == 1

    @pytest.mark.asyncio
    async def test_create_prepared_deal_refuses_blocked_artifact(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        create_route = mock_magnite_dmg_api.post(MAGNITE_DMG_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(201, json=CREATED_DEAL_RESPONSE)
        )

        prepared = await magnite_prepare_deal_from_prompt_inputs(
            deal_name="Blocked Deal",
            marketplace="Elcano CTV Marketplace",
            dsps=["The Trade Desk"],
            publishers=["Test Seat 1"],
            channel="ctv",
            floor=25.0,
            # end_date intentionally missing -> blocker
        )
        assert prepared["ready_to_create"] is False

        result = await magnite_create_prepared_deal(prepared["prepared_deal_id"])
        assert result["success"] is False
        assert "blocked" in result["error"]
        assert not create_route.called

    @pytest.mark.asyncio
    async def test_prepare_blocks_multi_buyer_dsp_without_buyers(self, mock_magnite_dmg_api: respx.MockRouter):
        _mock_reference_catalog(mock_magnite_dmg_api)
        # DV360 (id 416) has two buyers — a bare DSP token must NOT guess.
        mock_magnite_dmg_api.get(f"{MAGNITE_DMG_BASE_URL}/api/v1/dsps/416/buyers").mock(
            return_value=httpx.Response(200, json=_page([{"id": 1, "name": "Seat A"}, {"id": 2, "name": "Seat B"}]))
        )

        prepared = await magnite_prepare_deal_from_prompt_inputs(
            deal_name="Ambiguous Buyer Deal",
            marketplace="Elcano CTV Marketplace",
            dsps=["DV360"],
            publishers=["Test Seat 1"],
            channel="ctv",
            end_date="2026-12-31",
            floor=25.0,
        )

        assert prepared["ready_to_create"] is False
        blockers = {blocker["code"] for blocker in prepared["blockers"]}
        assert "buyers_missing" in blockers
