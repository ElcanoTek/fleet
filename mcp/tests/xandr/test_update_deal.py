"""Tests for Xandr deal-update support.

The Deal Service PUT /deal?id={id} is a genuine PARTIAL update — only sent
fields change — with one caveat: the docs mark ask_price Required On PUT, so
update_xandr_deal round-trips the current price when the caller doesn't
change it. Buyer fields, code, and is_archived are locked by policy.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import xandr_mcp
from xandr_mcp import _build_xandr_deal_update, update_xandr_deal

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_DEAL_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE

DEAL_ID = 884422

CURRENT_DEAL = {
    "id": DEAL_ID,
    "code": "ELC-XN-001",
    "name": "Elcano_Xandr_TTD_Curated_Deal",
    "active": True,
    "start_date": "2026-06-01 00:00:00",
    "end_date": None,
    "ask_price": 12.5,
    "currency": "USD",
    "use_deal_floor": True,
    "type": {"id": 5, "name": "Curated"},
    "auction_type": {"id": 2, "name": "standard_price"},
    "buyer": {"id": 1234, "bidder_id": 2, "name": "Buyer"},
    "profile_id": 777,
}


def _mock_auth(mock: respx.MockRouter) -> None:
    mock.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))


def _deal_response(deal: dict) -> httpx.Response:
    return httpx.Response(200, json={"response": {"status": "OK", "deal": deal}})


class TestBuildXandrDealUpdate:
    def test_partial_body_with_ask_price_roundtrip(self):
        payload = _build_xandr_deal_update(CURRENT_DEAL, name="Renamed Deal")
        # Only the change + the required ask_price round-trip + id.
        assert payload == {"deal": {"name": "Renamed Deal", "ask_price": 12.5, "id": DEAL_ID}}

    def test_explicit_ask_price_wins(self):
        payload = _build_xandr_deal_update(CURRENT_DEAL, ask_price=20.0)
        assert payload["deal"]["ask_price"] == 20.0

    def test_empty_update_rejected(self):
        with pytest.raises(ValueError, match="No update fields"):
            _build_xandr_deal_update(CURRENT_DEAL)
        # ask_price alone counts as a change... but only via the explicit arg.
        payload = _build_xandr_deal_update(CURRENT_DEAL, active=False)
        assert payload["deal"]["active"] is False

    def test_date_normalization_and_order(self):
        payload = _build_xandr_deal_update(CURRENT_DEAL, start_date="2026-07-01", end_date="2026-12-31")
        assert payload["deal"]["start_date"] == "2026-07-01 00:00:00"
        assert payload["deal"]["end_date"] == "2026-12-31 23:59:59"
        # New end date before the deal's CURRENT (unchanged) start date fails.
        with pytest.raises(ValueError, match="cannot be before"):
            _build_xandr_deal_update(CURRENT_DEAL, end_date="2026-05-01")
        with pytest.raises(ValueError, match="YYYY-MM-DD"):
            _build_xandr_deal_update(CURRENT_DEAL, start_date="07/01/2026")

    def test_auction_type_aliases_and_priority_bounds(self):
        payload = _build_xandr_deal_update(CURRENT_DEAL, auction_type="fixed", priority=20)
        assert payload["deal"]["auction_type"] == {"id": 3}
        assert payload["deal"]["priority"] == 20
        with pytest.raises(ValueError, match="auction_type"):
            _build_xandr_deal_update(CURRENT_DEAL, auction_type="dutch")
        with pytest.raises(ValueError, match="1-20"):
            _build_xandr_deal_update(CURRENT_DEAL, priority=21)

    @pytest.mark.parametrize("forbidden", ["buyer", "buyer_seats", "code", "is_archived"])
    def test_forbidden_override_fields_rejected(self, forbidden):
        with pytest.raises(ValueError, match="payload_overrides may not change"):
            _build_xandr_deal_update(CURRENT_DEAL, payload_overrides={forbidden: "x"})

    def test_allowed_overrides_pass_through(self):
        brands = [{"id": 1}, {"id": 5}]
        payload = _build_xandr_deal_update(CURRENT_DEAL, payload_overrides={"brands": brands})
        assert payload["deal"]["brands"] == brands


class TestUpdateXandrDeal:
    @pytest.mark.asyncio
    async def test_partial_put_with_verification(self, mock_xandr_api: respx.MockRouter):
        _mock_auth(mock_xandr_api)
        get_route = mock_xandr_api.get(XANDR_DEAL_ENDPOINT).mock(return_value=_deal_response(CURRENT_DEAL))
        put_route = mock_xandr_api.put(XANDR_DEAL_ENDPOINT).mock(
            return_value=_deal_response({**CURRENT_DEAL, "ask_price": 15.0})
        )

        result = await update_xandr_deal(deal_id=DEAL_ID, ask_price=15.0)

        assert result["success"] is True
        assert result["updated_fields"] == ["ask_price"]
        assert result["deal"]["ask_price"] == 15.0
        assert result["verification"]["id"] == DEAL_ID  # re-GET happened
        assert get_route.call_count == 2

        request = put_route.calls[0].request
        assert request.url.params.get("id") == str(DEAL_ID)
        sent = json.loads(request.content)
        assert sent == {"deal": {"ask_price": 15.0, "id": DEAL_ID}}

    @pytest.mark.asyncio
    async def test_pause_via_active_false(self, mock_xandr_api: respx.MockRouter):
        _mock_auth(mock_xandr_api)
        mock_xandr_api.get(XANDR_DEAL_ENDPOINT).mock(return_value=_deal_response(CURRENT_DEAL))
        put_route = mock_xandr_api.put(XANDR_DEAL_ENDPOINT).mock(
            return_value=_deal_response({**CURRENT_DEAL, "active": False})
        )

        result = await update_xandr_deal(deal_id=DEAL_ID, active=False)

        assert result["success"] is True
        sent = json.loads(put_route.calls[0].request.content)
        assert sent["deal"]["active"] is False
        # The required ask_price round-trips even on a pause-only update.
        assert sent["deal"]["ask_price"] == 12.5

    @pytest.mark.asyncio
    async def test_deal_not_found_blocks_put(self, mock_xandr_api: respx.MockRouter):
        _mock_auth(mock_xandr_api)
        mock_xandr_api.get(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"response": {"status": "OK", "deal": None}})
        )
        put_route = mock_xandr_api.put(XANDR_DEAL_ENDPOINT).mock(return_value=_deal_response(CURRENT_DEAL))

        result = await update_xandr_deal(deal_id=DEAL_ID, name="x")

        assert result["success"] is False
        assert "not found" in result["error"]
        assert not put_route.called

    @pytest.mark.asyncio
    async def test_validation_error_blocks_put(self, mock_xandr_api: respx.MockRouter):
        _mock_auth(mock_xandr_api)
        mock_xandr_api.get(XANDR_DEAL_ENDPOINT).mock(return_value=_deal_response(CURRENT_DEAL))
        put_route = mock_xandr_api.put(XANDR_DEAL_ENDPOINT).mock(return_value=_deal_response(CURRENT_DEAL))

        result = await update_xandr_deal(deal_id=DEAL_ID, payload_overrides={"buyer": {"id": 9}})

        assert result["success"] is False
        assert "payload_overrides may not change" in result["error"]
        assert not put_route.called


class TestDeleteNotExposed:
    """Deal deletion must NOT be reachable by the agent.

    The Deal Service DELETE is documented as permanent ("Deletions are
    permanent and cannot be reverted") — same security policy as OpenX
    dealArchive and TripleLift deal deletion (2026-06-11). Pausing via
    update_xandr_deal(active=False) is the supported way to stop delivery.
    """

    def test_no_delete_tool_or_client_method(self):
        assert not hasattr(xandr_mcp, "delete_xandr_deal")
        assert not hasattr(xandr_mcp.XandrClient, "delete_deal")
        with open(xandr_mcp.__file__) as f:
            assert '"DELETE"' not in f.read()
