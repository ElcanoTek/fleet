"""Tests for PubMatic curated-deal update support.

PubMatic's PUT /curateddeals/{id} is FULL-REPLACEMENT (create-shaped body),
so pm_update_curated_deal does read-modify-write: GET, map the response bean
back to a request bean, overlay only the caller's changes, PUT with
requestTypeEnum=UPDATE, verify with a re-GET. Pause/resume goes through the
dedicated updateStatus endpoint.
"""

import json
import os
import sys

import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from pubmatic_mcp import (
    PUBMATIC_ALLOWED_PLATFORM_IDS,
    PUBMATIC_CTV_PLATFORM_ID,
    _apply_pm_channel_platform_defaults,
    _curated_response_to_update_payload,
    pm_update_curated_deal,
    pm_update_curated_deal_status,
)

TOKEN_URL = "https://api.pubmatic.com/v1/developer-integrations/developer/token"
DEAL_URL = "https://api.pubmatic.com/curateddeals/111"
STATUS_URL = "https://api.pubmatic.com/curateddeals/updateStatus/111"

TOKEN_RESPONSE = {
    "userEmail": "user@example.com",
    "tokenType": "Bearer",
    "accessToken": "access-123",
    "refreshToken": "refresh-123",
}

# A GET response bean using the documented object shapes.
CURRENT_DEAL = {
    "id": 111,
    "dealId": "PM-ELC-CUR-001",
    "name": "Elcano_PM_TTD_Test_Deal",
    "ownedById": 60067,
    "startDate": "2026-06-01T00:00:00.000Z",
    "endDate": "2028-06-01T00:00:00.000Z",
    "auctionType": 3,
    "flooreCPM": 2.0,
    "priority": 10,
    "dealSource": 1,
    "hasMaxReach": 0,
    "targeting": 999,
    "pubIds": [1001, 1002],
    "publisherBlockList": [2001],
    "adFormats": [{"id": 12, "name": "Video"}],
    "platforms": [{"id": 7, "name": "CTV"}],
    "labels": [{"id": 5, "name": "Q3", "active": True}],
    "status": {"id": 1, "name": "Active"},
    "dealDspBuyerMappings": [{"buyerId": 2, "dspId": 3, "id": 9, "ruleMetaId": 4}],
    "ttdMainBuyer": {"buyerId": 2, "buyerName": "Buyer", "dspId": 3, "seatId": "seat-1"},
    "marketplace": {"id": 0, "name": "PubMatic"},
    # Response-only fields that must NOT leak into the PUT body.
    "creationTime": "2026-06-01T00:00:00.000Z",
    "modificationTime": "2026-06-02T00:00:00.000Z",
    "targetedDealPubs": [{"pubId": 1001, "dealEcpm": 2.0}],
    "samplingStatus": "DONE",
    "dealType": "PMP_TARGETED",
    "dailyTxnFeeEditCount": 0,
}


def _mock_token(mock: respx.MockRouter) -> None:
    mock.post(TOKEN_URL).respond(200, json=TOKEN_RESPONSE)


class TestCuratedResponseToUpdatePayload:
    def test_object_shapes_mapped_to_request_shapes(self):
        payload = _curated_response_to_update_payload(CURRENT_DEAL)
        assert payload["adFormats"] == [12]
        assert payload["platforms"] == [7]
        assert payload["labelIds"] == [5]
        assert payload["status"] == 1
        assert payload["ttdMainBuyer"] == [CURRENT_DEAL["ttdMainBuyer"]]
        assert payload["dealDspBuyerMappings"] == CURRENT_DEAL["dealDspBuyerMappings"]
        assert payload["marketplace"] == {"id": 0, "name": "PubMatic"}

    def test_scalars_round_trip(self):
        payload = _curated_response_to_update_payload(CURRENT_DEAL)
        for field, expected in (
            ("name", "Elcano_PM_TTD_Test_Deal"),
            ("flooreCPM", 2.0),
            ("auctionType", 3),
            ("targeting", 999),
            ("pubIds", [1001, 1002]),
            ("publisherBlockList", [2001]),
            ("hasMaxReach", 0),
        ):
            assert payload[field] == expected

    def test_response_only_fields_never_leak(self):
        payload = _curated_response_to_update_payload(CURRENT_DEAL)
        for forbidden in (
            "creationTime",
            "modificationTime",
            "targetedDealPubs",
            "samplingStatus",
            "dealType",
            "dailyTxnFeeEditCount",
            "ownedById",
            "labels",
        ):
            assert forbidden not in payload

    def test_bundled_pmp_maps_to_pmp_ids(self):
        deal = {**CURRENT_DEAL, "bundled": True, "bundledPMP": [{"id": 1, "pmpId": 77}, {"id": 2, "pmpId": 78}]}
        payload = _curated_response_to_update_payload(deal)
        assert payload["bundledPMP"] == [77, 78]
        assert payload["bundled"] is True


class TestUpdateCuratedDeal:
    async def test_read_modify_write_overlays_only_changes(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=CURRENT_DEAL)
        put_route = mock_pubmatic_api.put(DEAL_URL).respond(200, json={**CURRENT_DEAL, "flooreCPM": 5.0})

        result = await pm_update_curated_deal(
            curated_id=111,
            logged_in_owner_id=60067,
            logged_in_owner_type_id=5,
            floor_ecpm=5.0,
            name="Renamed Deal",
        )

        assert result["success"] is True
        assert result["updated_fields"] == ["flooreCPM", "name"]
        # Verification re-GET happened (GET route called twice).
        assert result["verification"] is not None

        sent = json.loads(put_route.calls[0].request.content)
        assert sent["requestTypeEnum"] == "UPDATE"
        assert sent["id"] == 111
        assert sent["loggedInOwnerId"] == 60067
        assert sent["flooreCPM"] == 5.0
        assert sent["name"] == "Renamed Deal"
        # Unchanged fields round-tripped from the GET (full-replacement body).
        assert sent["targeting"] == 999
        assert sent["pubIds"] == [1001, 1002]
        assert sent["adFormats"] == [12]
        assert sent["platforms"] == [7]
        assert sent["status"] == 1
        # Response-only fields never reach the PUT.
        assert "targetedDealPubs" not in sent
        assert "creationTime" not in sent

    async def test_ownership_mismatch_blocks_update(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        mock_pubmatic_api.get(DEAL_URL).respond(200, json={**CURRENT_DEAL, "ownedById": 99999})
        put_route = mock_pubmatic_api.put(DEAL_URL).respond(200, json={})

        result = await pm_update_curated_deal(
            curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5, name="x"
        )

        assert result["success"] is False
        assert "SECURITY FAILURE" in result["error"]
        assert not put_route.called

    async def test_wrong_owner_argument_rejected_without_network(self):
        result = await pm_update_curated_deal(curated_id=111, logged_in_owner_id=1, logged_in_owner_type_id=5, name="x")
        assert result["success"] is False
        assert "SECURITY" in result["error"]

    async def test_no_changes_rejected_before_put(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=CURRENT_DEAL)
        put_route = mock_pubmatic_api.put(DEAL_URL).respond(200, json={})

        result = await pm_update_curated_deal(curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5)

        assert result["success"] is False
        assert "No update fields" in result["error"]
        assert not put_route.called

    async def test_system_status_codes_rejected(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=CURRENT_DEAL)

        result = await pm_update_curated_deal(
            curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5, status=5
        )
        assert result["success"] is False
        assert "system states" in result["error"]

    async def test_floor_on_first_price_deal_warns(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        first_price_deal = {**CURRENT_DEAL, "auctionType": 1}
        first_price_deal.pop("flooreCPM")
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=first_price_deal)
        mock_pubmatic_api.put(DEAL_URL).respond(200, json=first_price_deal)

        result = await pm_update_curated_deal(
            curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5, floor_ecpm=3.0
        )
        assert result["success"] is True
        assert any("First Price" in w for w in result["warnings"])

    async def test_fixed_price_without_roundtrippable_floor_warns(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        floorless = {**CURRENT_DEAL}
        floorless.pop("flooreCPM")
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=floorless)
        mock_pubmatic_api.put(DEAL_URL).respond(200, json=floorless)

        result = await pm_update_curated_deal(
            curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5, name="x"
        )
        assert result["success"] is True
        assert any("without one on a Fixed Price deal" in w for w in result["warnings"])

    async def test_platform_7_accepted_on_update(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        mock_pubmatic_api.get(DEAL_URL).respond(200, json=CURRENT_DEAL)
        put_route = mock_pubmatic_api.put(DEAL_URL).respond(200, json=CURRENT_DEAL)

        result = await pm_update_curated_deal(
            curated_id=111, logged_in_owner_id=60067, logged_in_owner_type_id=5, platforms=[7]
        )
        assert result["success"] is True
        assert json.loads(put_route.calls[0].request.content)["platforms"] == [7]


class TestUpdateCuratedDealStatus:
    async def test_pause_via_alias(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        route = mock_pubmatic_api.put(STATUS_URL).respond(200, json={"id": 111, "status": 2})

        result = await pm_update_curated_deal_status(
            curated_deal_id="111", status="paused", logged_in_owner_id=60067, logged_in_owner_type_id=5
        )

        assert result["success"] is True
        assert result["status"] == 2
        assert result["status_name"] == "Inactive"
        sent = json.loads(route.calls[0].request.content)
        assert sent == {"status": 2, "loggedInOwnerId": 60067, "loggedInOwnerTypeId": 5}

    async def test_resume_via_int(self, mock_pubmatic_api: respx.MockRouter):
        _mock_token(mock_pubmatic_api)
        route = mock_pubmatic_api.put(STATUS_URL).respond(200, json={"id": 111, "status": 1})

        result = await pm_update_curated_deal_status(
            curated_deal_id="111", status=1, logged_in_owner_id=60067, logged_in_owner_type_id=5
        )
        assert result["success"] is True
        assert result["status_name"] == "Active"
        assert json.loads(route.calls[0].request.content)["status"] == 1

    async def test_system_status_rejected(self):
        result = await pm_update_curated_deal_status(
            curated_deal_id="111", status=11, logged_in_owner_id=60067, logged_in_owner_type_id=5
        )
        assert result["success"] is False
        assert "system states" in result["error"]

    async def test_owner_guard(self):
        result = await pm_update_curated_deal_status(
            curated_deal_id="111", status="paused", logged_in_owner_id=1, logged_in_owner_type_id=5
        )
        assert result["success"] is False
        assert "SECURITY" in result["error"]


class TestCtvPlatformFix:
    """CTV is platform 7 per the Curated Deals docs — 5 is Mobile App Android.

    The old allowed set {1,2,4,5} rejected real CTV entirely while the
    mislabeled enum sent CTV deals out as Android in-app.
    """

    def test_platform_7_is_allowed(self):
        assert PUBMATIC_CTV_PLATFORM_ID == 7
        assert 7 in PUBMATIC_ALLOWED_PLATFORM_IDS

    def test_ctv_channel_defaults_to_platform_7(self):
        platforms, applied = _apply_pm_channel_platform_defaults(None, "ctv")
        assert platforms == [7]
        assert applied is True

    def test_ott_channel_defaults_to_mobile_apps(self):
        platforms, applied = _apply_pm_channel_platform_defaults(None, "ott")
        assert platforms == [4, 5]
        assert applied is True

    def test_display_keeps_legacy_web_default(self):
        platforms, applied = _apply_pm_channel_platform_defaults(None, "display")
        assert platforms == [1]
        assert applied is True
        # No channel hint at all → legacy [1] without the default flag.
        platforms, applied = _apply_pm_channel_platform_defaults(None, None)
        assert platforms == [1]
        assert applied is False

    def test_explicit_platforms_never_overridden(self):
        platforms, applied = _apply_pm_channel_platform_defaults([5], "ctv")
        assert platforms == [5]
        assert applied is False
