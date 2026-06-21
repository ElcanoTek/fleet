"""
Tests for the get_deal MCP tool.

Verifies deal retrieval by ID and handling of missing deals.
"""

import json
import os
import sys
from typing import Any

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_get_deal

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import DEAL_NOT_FOUND_RESPONSE, DEAL_RESPONSE, GRAPHQL_ERROR_RESPONSE


class TestGetDeal:
    """Tests for get_deal tool."""

    @pytest.mark.asyncio
    async def test_valid_deal_id(self, mock_openx_graphql: respx.MockRouter):
        """Test successful retrieval of a deal by ID."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=DEAL_RESPONSE))

        result = await ox_get_deal(deal_id="deal-001")

        assert result["success"] is True
        assert "deal" in result
        assert result["deal_url"] == "https://select.openx.com/deals/deal-001/details"

        deal = result["deal"]
        assert deal["id"] == "deal-001"
        assert deal["deal_id"] == "ELC-2024-001"
        assert deal["name"] == "Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0"
        assert deal["status"] == "ACTIVE"

    @pytest.mark.asyncio
    async def test_deal_includes_all_fields(self, mock_openx_graphql: respx.MockRouter):
        """Verify all expected fields are returned in deal response."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=DEAL_RESPONSE))

        result = await ox_get_deal(deal_id="deal-001")

        deal = result["deal"]

        # Core fields
        assert "id" in deal
        assert "deal_id" in deal
        assert "name" in deal
        assert "status" in deal
        assert "currency" in deal
        assert "deal_price" in deal

        # Extended fields from full query
        assert "deal_participants" in deal
        assert "package" in deal
        assert "third_party_fees_config" in deal
        assert "targeting" in deal["package"]

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify the exact GraphQL query and variables."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        await ox_get_deal(deal_id="my-test-deal-id")

        payload = json.loads(captured_request.content)

        # Verify query structure
        query = payload["query"]
        assert "GetDeal" in query or "dealById" in query
        assert "$id" in query
        assert "rendering_context" in query
        assert "desktop_devices" in query
        assert "mobile_devices" in query
        assert "tv_devices" in query
        assert "device_type" in query
        device_type_selection = query.split("device_type", 1)[1]
        assert "desktop_devices" in device_type_selection
        assert "mobile_devices" in device_type_selection
        assert "tv_devices" in device_type_selection
        assert "device_type {\n                                op\n                                val" not in query
        assert "platform_partner_id" not in query
        assert "platform_share" not in query
        assert "uid" in query

        # Verify the deal ID is passed correctly
        assert payload["variables"]["id"] == "my-test-deal-id"

    @pytest.mark.asyncio
    async def test_missing_deal_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling when deal is not found."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_NOT_FOUND_RESPONSE)
        )

        result = await ox_get_deal(deal_id="non-existent-deal")

        assert result["success"] is False
        assert "error" in result
        assert "not found" in result["error"].lower()

    @pytest.mark.asyncio
    async def test_graphql_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of GraphQL errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)
        )

        result = await ox_get_deal(deal_id="any-deal")

        assert result["success"] is False
        assert "error" in result
        assert "GraphQL errors" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(401, text="Unauthorized"))

        result = await ox_get_deal(deal_id="any-deal")

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_deal_participants_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify deal_participants are correctly parsed."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=DEAL_RESPONSE))

        result = await ox_get_deal(deal_id="deal-001")

        participants = result["deal"]["deal_participants"]
        assert len(participants) == 1

        participant = participants[0]
        assert participant["demand_partner"] == "CRIMTAN"
        assert "buyer_ids" in participant
        assert "brand_ids" in participant

    @pytest.mark.asyncio
    async def test_third_party_fees_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify third_party_fees_config is correctly parsed with real API fields."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=DEAL_RESPONSE))

        result = await ox_get_deal(deal_id="deal-001")

        fees = result["deal"]["third_party_fees_config"]
        assert isinstance(fees, list)
        assert len(fees) == 1
        fee = fees[0]
        assert fee["partner_id"] == "partner-001"
        assert fee["revenue_method"] == "PoM"
        assert fee["gross_share"] == "30"

    @pytest.mark.asyncio
    async def test_rendering_context_v2_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify package.targeting reads the V2 rendering_context fields."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=DEAL_RESPONSE))

        result = await ox_get_deal(deal_id="deal-001")

        rendering_context = result["deal"]["package"]["targeting"]["rendering_context"]
        assert rendering_context["op"] == "AND"
        assert rendering_context["ad_placement"]["val"] == "BANNER"
        assert rendering_context["distribution_channel"]["val"] == "WEB,APP"
        assert rendering_context["device_type"]["desktop_devices"] == "desktop"
        assert rendering_context["device_type"]["mobile_devices"] == "phone,tablet"

    @pytest.mark.asyncio
    async def test_returns_full_targeting_for_verification(self, mock_openx_graphql: respx.MockRouter):
        """Post-create verification needs targeting beyond rendering_context — without IAB,
        url_targeting, audience, content.account, and geographic in the response, the trader
        has no way to confirm the wire payload matches the brief without going to the OpenX
        UI. This test pins the extended selection set."""

        captured_request: dict[str, Any] = {}

        def capture(request: httpx.Request) -> httpx.Response:
            captured_request["payload"] = json.loads(request.content)
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture)

        result = await ox_get_deal(deal_id="deal-001")

        # The query MUST request every targeting branch trader workflows verify.
        query = captured_request["payload"]["query"]
        for required_field in (
            "categories_iab_v2",
            "openaudience_custom",
            "content",
            "geographic",
            "url_targeting",
        ):
            assert required_field in query, f"get_deal query missing required field: {required_field}"

        targeting = result["deal"]["package"]["targeting"]
        assert targeting["domain"]["categories_iab_v2"] == {"op": "INTERSECTS", "val": "384"}
        assert targeting["audience"]["openaudience_custom"]["val"].startswith("openaudience-")
        assert targeting["content"]["account"] == {"op": "NOT INTERSECTS", "val": "193155,209125"}
        assert targeting["geographic"]["includes"]["country"] == "us"

        url_targeting = result["deal"]["package"]["url_targeting"]
        assert url_targeting["type"] == "blacklist"
        assert url_targeting["urls"] == ["example.com", "duplicate.com"]
        assert url_targeting["domain_targeting_option"] == "ROOT"
