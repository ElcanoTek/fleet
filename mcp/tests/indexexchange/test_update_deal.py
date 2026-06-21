# mcp/tests/indexexchange/test_update_deal.py
"""Tests for ix_update_deal (PATCH /api/deals/v3/deals/{id}) and the ETag flow."""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    _build_deal_update_payload,
    ix_get_deal_settings,
    ix_list_deals_v3,
    ix_update_deal,
)

from .conftest import IX_BASE_URL, IX_LOGIN_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE

IX_DEALS_V3_ENDPOINT = f"{IX_BASE_URL}/api/deals/v3/deals"
DEAL_ID = 424242
DEAL_ENDPOINT = f"{IX_DEALS_V3_ENDPOINT}/{DEAL_ID}"

CURRENT_DEAL = {
    "internalDealID": DEAL_ID,
    "externalDealID": "IX17000000000000001",
    "classID": 4,
    "name": "Elcano_IX_TTD_Test_Deal",
    "startDate": "2026-06-01",
    "endDate": "2028-06-01",
    "floor": 0.10,
    "auctionType": "first",
    "status": "active",
    "targeting": [
        {
            "keyName": "Country",
            "targetingType": "standard",
            "sets": [{"operator": "ANY_OF", "values": [{"value": "USA", "label": "United States"}]}],
        }
    ],
}


def _mock_login(mock: respx.MockRouter) -> None:
    mock.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))


# =============================================================================
# _build_deal_update_payload — pure validation
# =============================================================================


class TestBuildDealUpdatePayload:
    def test_partial_body_only_includes_provided_fields(self):
        payload = _build_deal_update_payload(current_deal=CURRENT_DEAL, floor=1.25, status="paused")
        assert payload == {"floor": 1.25, "status": "paused"}

    def test_empty_update_rejected(self):
        with pytest.raises(ValueError, match="No update fields"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL)

    def test_name_length_validated(self):
        with pytest.raises(ValueError, match="1–255"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, name="x" * 256)

    def test_floor_minimum_is_class_aware(self):
        # Marketplace Package (classID=4) floor minimum is 0.10...
        with pytest.raises(ValueError, match=">= 0.1"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, floor=0.05)
        # ...while a Direct Deal (classID=1) accepts down to 0.01.
        direct_deal = {**CURRENT_DEAL, "classID": 1}
        payload = _build_deal_update_payload(current_deal=direct_deal, floor=0.05)
        assert payload["floor"] == 0.05

    def test_floor_maximum_enforced(self):
        with pytest.raises(ValueError, match="<= 99999.99"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, floor=100000.0)

    def test_status_and_auction_enums(self):
        with pytest.raises(ValueError, match="status"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, status="enabled")
        with pytest.raises(ValueError, match="auction_type"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, auction_type="second")
        payload = _build_deal_update_payload(current_deal=CURRENT_DEAL, status="A", auction_type="fixed")
        assert payload == {"status": "A", "auctionType": "fixed"}

    def test_end_date_cannot_precede_unchanged_start_date(self):
        # New end date before the deal's CURRENT start date must fail even
        # though start_date isn't part of this PATCH.
        with pytest.raises(ValueError, match="cannot be before"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, end_date="2026-05-01")
        # Moving both dates together is fine.
        payload = _build_deal_update_payload(current_deal=CURRENT_DEAL, start_date="2026-01-01", end_date="2026-05-01")
        assert payload == {"startDate": "2026-01-01", "endDate": "2026-05-01"}

    def test_date_format_validated(self):
        with pytest.raises(ValueError, match="YYYY-MM-DD"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, start_date="06/01/2026")

    def test_targeting_shape_validated(self):
        bad = [{"keyName": "Country", "targetingType": "standard", "sets": [{"operator": "ALL_OF", "values": []}]}]
        with pytest.raises(ValueError, match="ANY_OF"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, targeting=bad)
        payload = _build_deal_update_payload(current_deal=CURRENT_DEAL, targeting=CURRENT_DEAL["targeting"])
        assert payload["targeting"] == CURRENT_DEAL["targeting"]

    def test_class_config_must_match_deal_class(self):
        # directConfigurations on a classID=4 deal is a caller bug.
        with pytest.raises(ValueError, match="classID=1"):
            _build_deal_update_payload(current_deal=CURRENT_DEAL, direct_configurations={"priority": 5})
        payload = _build_deal_update_payload(current_deal=CURRENT_DEAL, marketplace_configurations={"margin": 5.5})
        assert payload["marketplaceConfigurations"] == {"margin": 5.5}


# =============================================================================
# ix_get_deal_settings — etag surfacing
# =============================================================================


@pytest.mark.asyncio
async def test_get_deal_settings_returns_etag(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v7"'})
    )
    result = await ix_get_deal_settings(internal_deal_id=DEAL_ID)
    assert result["success"] is True
    assert result["deal"]["internalDealID"] == DEAL_ID
    assert result["etag"] == 'W/"deal-v7"'


# =============================================================================
# ix_update_deal — PATCH flow
# =============================================================================


@pytest.mark.asyncio
async def test_update_deal_sends_partial_body_with_if_match(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v7"'})
    )
    patch_route = mock_ix_api.patch(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json={**CURRENT_DEAL, "floor": 2.50, "status": "paused"})
    )

    result = await ix_update_deal(internal_deal_id=DEAL_ID, floor=2.50, status="paused")

    assert result["success"] is True
    assert result["updated_fields"] == ["floor", "status"]
    assert result["etag_used"] == 'W/"deal-v7"'
    assert result["deal"]["floor"] == 2.50

    assert patch_route.call_count == 1
    request = patch_route.calls[0].request
    assert request.headers["If-Match"] == 'W/"deal-v7"'
    assert json.loads(request.content) == {"floor": 2.50, "status": "paused"}


@pytest.mark.asyncio
async def test_update_deal_validation_failure_never_hits_patch(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v7"'})
    )
    patch_route = mock_ix_api.patch(DEAL_ENDPOINT).mock(return_value=httpx.Response(200, json=CURRENT_DEAL))

    # 0.05 is below the classID=4 floor minimum read from the fetched deal.
    result = await ix_update_deal(internal_deal_id=DEAL_ID, floor=0.05)

    assert result["success"] is False
    assert "floor" in result["error"]["message"]
    assert not patch_route.called


@pytest.mark.asyncio
async def test_update_deal_retries_once_on_etag_conflict(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        side_effect=[
            httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v7"'}),
            httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v8"'}),
        ]
    )
    patch_route = mock_ix_api.patch(DEAL_ENDPOINT).mock(
        side_effect=[
            httpx.Response(412, json={"error": "precondition failed"}),
            httpx.Response(200, json={**CURRENT_DEAL, "floor": 3.00}),
        ]
    )

    result = await ix_update_deal(internal_deal_id=DEAL_ID, floor=3.00)

    assert result["success"] is True
    assert result["etag_used"] == 'W/"deal-v8"'
    assert patch_route.call_count == 2
    assert patch_route.calls[0].request.headers["If-Match"] == 'W/"deal-v7"'
    assert patch_route.calls[1].request.headers["If-Match"] == 'W/"deal-v8"'


@pytest.mark.asyncio
async def test_update_deal_pinned_etag_conflict_is_not_retried(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v9"'})
    )
    patch_route = mock_ix_api.patch(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(412, json={"error": "precondition failed"})
    )

    result = await ix_update_deal(internal_deal_id=DEAL_ID, floor=3.00, etag='W/"deal-v7"')

    assert result["success"] is False
    assert "etag_conflict" in result["error"]["message"]
    # The pinned (stale) etag was used verbatim and never silently replaced.
    assert patch_route.call_count == 1
    assert patch_route.calls[0].request.headers["If-Match"] == 'W/"deal-v7"'


@pytest.mark.asyncio
async def test_update_deal_full_targeting_replacement_passthrough(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(DEAL_ENDPOINT).mock(
        return_value=httpx.Response(200, json=CURRENT_DEAL, headers={"etag": 'W/"deal-v7"'})
    )
    patch_route = mock_ix_api.patch(DEAL_ENDPOINT).mock(return_value=httpx.Response(200, json=CURRENT_DEAL))

    # The complete array (current country targeting + a new device set), as
    # the docs require — targeting is replaced wholesale on update.
    full_targeting = [
        *CURRENT_DEAL["targeting"],
        {
            "keyName": "DeviceType",
            "targetingType": "standard",
            "sets": [{"operator": "ANY_OF", "values": [{"value": "Connected TV"}]}],
        },
    ]
    result = await ix_update_deal(internal_deal_id=DEAL_ID, targeting=full_targeting)

    assert result["success"] is True
    sent = json.loads(patch_route.calls[0].request.content)
    assert sent == {"targeting": full_targeting}


# =============================================================================
# ix_list_deals_v3 — documented totalCount field
# =============================================================================


@pytest.mark.asyncio
async def test_list_deals_v3_prefers_documented_total_count(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    mock_ix_api.get(IX_DEALS_V3_ENDPOINT).mock(
        return_value=httpx.Response(200, json={"deals": [CURRENT_DEAL], "totalCount": 57})
    )
    result = await ix_list_deals_v3(search="Elcano")
    assert result["success"] is True
    assert result["total"] == 57


@pytest.mark.asyncio
async def test_list_deals_v3_passes_auction_type_and_bidding_strategy(mock_ix_api: respx.MockRouter):
    _mock_login(mock_ix_api)
    route = mock_ix_api.get(IX_DEALS_V3_ENDPOINT).mock(
        return_value=httpx.Response(200, json={"deals": [], "totalCount": 0})
    )
    result = await ix_list_deals_v3(auction_type="fixed", bidding_strategy=["standard", "preferredPrice"])
    assert result["success"] is True
    params = route.calls[0].request.url.params
    assert params.get("auctionType") == "fixed"
    assert params.get("biddingStrategy") == "standard,preferredPrice"

    bad = await ix_list_deals_v3(auction_type="second")
    assert bad["success"] is False
