"""Tests for Media.net Select deal-update support.

Media.net's PUT /api/v2/deals/{deal_id} is FULL-REPLACEMENT per API Guide v9
("any parameter not passed will be set to null"), so mn_update_deal does
read-modify-write: list-fetch the deal, map the response back to a request
body (deal_id removed — it must not appear in an update), overlay only the
caller's changes, PUT, verify with a re-fetch.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import _deal_response_to_update_payload, mn_update_deal

from .conftest import MEDIANET_DEALS_ENDPOINT, MEDIANET_LOGIN_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE

DEAL_ID = "ELC-MN-2024-001"

# A list-deals response object per the v9 guide's response snippet.
CURRENT_DEAL = {
    "deal_id": DEAL_ID,
    "display_name": "Elcano_MN_Premium_Banner_US",
    "start_date": "2026-06-01",
    "end_date": None,
    "ad_format": 0,
    "bid_floor": 2.5,
    "margin": 30,
    "margin_type": 1,
    "deal_auction_type": 1,
    "demand_partners": [
        {"id": "DV 360", "name": "DV 360", "sync": {"sync_id": 3792, "sync_status": 6}},
        {"id": "TTD", "name": "The Trade Desk", "sync": None},
    ],
    "environments": ["A"],
    "whitelisted_seats": None,
    "publisher_domains": None,
    "devices": [1, 2],
    "geo": [{"geo_type": "country", "id": "US", "is_excluded": False}],
    "video": {"min": None, "max": None},
    "vcr": {"min": None, "max": None},
    "viewability": {"min": 10, "max": 20},
    "content_categories": None,
    "status": 1,
    "is_interstitial": 1,
    "is_rewarded": None,
    "updated_at": "2026-06-10 11:35:26",
}

LIST_RESPONSE = {"status": "OK", "data": [CURRENT_DEAL]}


def _mock_login(mock: respx.MockRouter) -> None:
    mock.post(MEDIANET_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))


class TestDealResponseToUpdatePayload:
    def test_deal_id_and_response_only_fields_dropped(self):
        payload, _ = _deal_response_to_update_payload(CURRENT_DEAL)
        assert "deal_id" not in payload
        assert "updated_at" not in payload

    def test_demand_partner_objects_become_id_strings(self):
        payload, _ = _deal_response_to_update_payload(CURRENT_DEAL)
        assert payload["demand_partners"] == ["DV 360", "TTD"]

    def test_nulls_and_all_null_ranges_dropped(self):
        payload, _ = _deal_response_to_update_payload(CURRENT_DEAL)
        for absent in ("end_date", "whitelisted_seats", "publisher_domains", "content_categories", "is_rewarded"):
            assert absent not in payload
        assert "video" not in payload  # all-null range dropped
        assert "vcr" not in payload
        assert payload["viewability"] == {"min": 10, "max": 20}  # populated range kept

    def test_environment_letter_codes_mapped(self):
        payload, warnings = _deal_response_to_update_payload(CURRENT_DEAL)
        assert payload["environments"] == ["App"]
        assert any("environment codes" in w for w in warnings)

    def test_system_status_omitted_with_warning(self):
        expired = {**CURRENT_DEAL, "status": 0}
        payload, warnings = _deal_response_to_update_payload(expired)
        assert "status" not in payload
        assert any("system state" in w for w in warnings)
        # Settable statuses round-trip.
        payload, _ = _deal_response_to_update_payload(CURRENT_DEAL)
        assert payload["status"] == 1


class TestUpdateDeal:
    @pytest.mark.asyncio
    async def test_read_modify_write_overlays_only_changes(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        put_route = mock_medianet_api.put(f"{MEDIANET_DEALS_ENDPOINT}/{DEAL_ID}").mock(
            return_value=httpx.Response(200, json={"status": "OK", "data": {"message": "updated"}})
        )

        result = await mn_update_deal(deal_id=DEAL_ID, bid_floor=5.0, display_name="Renamed_MN_Deal")

        assert result["success"] is True
        assert result["updated_fields"] == ["bid_floor", "display_name"]
        assert result["verification"] is not None

        sent = json.loads(put_route.calls[0].request.content)
        assert sent["bid_floor"] == 5.0
        assert sent["display_name"] == "Renamed_MN_Deal"
        # Unchanged fields round-tripped (full-replacement body).
        assert sent["margin"] == 30
        assert sent["demand_partners"] == ["DV 360", "TTD"]
        assert sent["geo"] == [{"geo_type": "country", "id": "US", "is_excluded": False}]
        assert sent["status"] == 1
        # deal_id must never be in an update body.
        assert "deal_id" not in sent

    @pytest.mark.asyncio
    async def test_pause_via_status_alias(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        put_route = mock_medianet_api.put(f"{MEDIANET_DEALS_ENDPOINT}/{DEAL_ID}").mock(
            return_value=httpx.Response(200, json={"status": "OK", "data": {}})
        )

        result = await mn_update_deal(deal_id=DEAL_ID, status="paused")

        assert result["success"] is True
        assert json.loads(put_route.calls[0].request.content)["status"] == -1

    @pytest.mark.asyncio
    async def test_system_status_rejected(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        result = await mn_update_deal(deal_id=DEAL_ID, status=3)
        assert result["success"] is False
        assert "system states" in result["error"]

    @pytest.mark.asyncio
    async def test_deal_not_found(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"status": "OK", "data": []})
        )
        result = await mn_update_deal(deal_id="missing", display_name="x")
        assert result["success"] is False
        assert "not found" in result["error"]

    @pytest.mark.asyncio
    async def test_no_changes_rejected_before_put(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        put_route = mock_medianet_api.put(f"{MEDIANET_DEALS_ENDPOINT}/{DEAL_ID}").mock(
            return_value=httpx.Response(200, json={})
        )
        result = await mn_update_deal(deal_id=DEAL_ID)
        assert result["success"] is False
        assert "No update fields" in result["error"]
        assert not put_route.called

    @pytest.mark.asyncio
    async def test_margin_validated_against_margin_type(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        # Deal is margin_type=1 (Percentage, 0-50): 45 ok, 60 not.
        bad = await mn_update_deal(deal_id=DEAL_ID, margin=60)
        assert bad["success"] is False
        assert "0-50" in bad["error"]
        # Fixed (0-25) bound applies when margin_type=0 is part of the same update.
        bad_fixed = await mn_update_deal(deal_id=DEAL_ID, margin=30, margin_type=0)
        assert bad_fixed["success"] is False
        assert "0-25" in bad_fixed["error"]

    @pytest.mark.asyncio
    async def test_payload_overrides_replace_targeting_and_drop_deal_id(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        put_route = mock_medianet_api.put(f"{MEDIANET_DEALS_ENDPOINT}/{DEAL_ID}").mock(
            return_value=httpx.Response(200, json={"status": "OK", "data": {}})
        )

        new_geo = [{"geo_type": "country", "id": "CA", "is_excluded": False}]
        result = await mn_update_deal(
            deal_id=DEAL_ID,
            payload_overrides={"geo": new_geo, "deal_id": "evil-override"},
        )

        assert result["success"] is True
        assert any("deal_id is never sent" in w for w in result["warnings"])
        sent = json.loads(put_route.calls[0].request.content)
        assert sent["geo"] == new_geo
        assert "deal_id" not in sent

    @pytest.mark.asyncio
    async def test_display_name_length_validated(self, mock_medianet_api: respx.MockRouter):
        _mock_login(mock_medianet_api)
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LIST_RESPONSE)
        )
        result = await mn_update_deal(deal_id=DEAL_ID, display_name="x" * 31)
        assert result["success"] is False
        assert "1-30" in result["error"]
