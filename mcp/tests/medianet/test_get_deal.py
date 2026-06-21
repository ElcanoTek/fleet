"""
Tests for the get_deal MCP tool for Media.net Select.

Verifies deal retrieval by ID and handling of missing deals.
Uses the list_deals endpoint with filter[deal_id][] parameter.
"""

import os
import sys
from urllib.parse import parse_qs, urlparse

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import mn_get_deal

from .conftest import MEDIANET_DEALS_ENDPOINT, MEDIANET_LOGIN_ENDPOINT
from .fixtures import (
    DEAL_NOT_FOUND_RESPONSE,
    DEAL_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    SERVER_ERROR_RESPONSE,
)


class TestGetDeal:
    """Tests for get_deal tool."""

    @pytest.mark.asyncio
    async def test_valid_deal_id(self, mock_medianet_api: respx.MockRouter):
        """Test successful retrieval of a deal by ID."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await mn_get_deal(deal_id="ELC-MN-2024-001")

        assert result["success"] is True
        assert "deal" in result

        deal = result["deal"]
        assert deal["deal_id"] == "ELC-MN-2024-001"
        assert deal["display_name"] == "Elcano_MediaNet_Premium_Banner_US"
        assert deal["status"] == 1

    @pytest.mark.asyncio
    async def test_deal_includes_all_fields(self, mock_medianet_api: respx.MockRouter):
        """Verify all expected fields are returned in deal response."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await mn_get_deal(deal_id="ELC-MN-2024-001")

        deal = result["deal"]

        # Core fields
        assert "id" in deal
        assert "deal_id" in deal
        assert "display_name" in deal
        assert "status" in deal
        assert "ad_format" in deal
        assert "margin" in deal
        assert "margin_type" in deal

        # Extended fields
        assert "demand_partners" in deal
        assert "environments" in deal
        assert "domains" in deal
        assert "geos" in deal
        assert "devices" in deal

    @pytest.mark.asyncio
    async def test_uses_list_endpoint_with_filter(self, mock_medianet_api: respx.MockRouter):
        """Verify get_deal uses list endpoint with filter[deal_id][] parameter."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_get_deal(deal_id="my-test-deal-id")

        # Verify URL uses the list endpoint
        assert MEDIANET_DEALS_ENDPOINT in str(captured_request.url)

        # Verify filter parameter
        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        assert "filter[deal_id][]" in query_params
        assert query_params["filter[deal_id][]"] == ["my-test-deal-id"]

    @pytest.mark.asyncio
    async def test_missing_deal_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling when deal is not found."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_NOT_FOUND_RESPONSE)
        )

        result = await mn_get_deal(deal_id="non-existent-deal")

        assert result["success"] is False
        assert "error" in result
        assert "Deal not found" in result["error"]
        assert "non-existent-deal" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await mn_get_deal(deal_id="any-deal")

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            side_effect=httpx.ConnectError("Connection refused")
        )

        result = await mn_get_deal(deal_id="any-deal")

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_medianet_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_get_deal(deal_id="ELC-MN-2024-001")

        # Verify headers
        assert captured_request.headers.get("token") == "medianet-auth-token-12345"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_demand_partners_structure(self, mock_medianet_api: respx.MockRouter):
        """Verify demand partners are correctly parsed."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await mn_get_deal(deal_id="ELC-MN-2024-001")

        demand_partners = result["deal"]["demand_partners"]
        assert len(demand_partners) == 1
        assert demand_partners[0] == "1"

    @pytest.mark.asyncio
    async def test_targeting_fields(self, mock_medianet_api: respx.MockRouter):
        """Verify targeting fields are correctly parsed."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await mn_get_deal(deal_id="ELC-MN-2024-001")

        deal = result["deal"]

        # Domain targeting
        assert deal["domains"] == ["premium-site.com", "quality-publisher.org"]

        # Geographic targeting
        assert deal["geos"] == ["US", "CA"]

        # Device targeting
        assert deal["devices"] == ["desktop", "mobile"]
