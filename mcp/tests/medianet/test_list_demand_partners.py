"""
Tests for the list_demand_partners MCP tool for Media.net Select.

Verifies correct API endpoint usage and response normalization.
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import mn_list_demand_partners

from .conftest import MEDIANET_BASE_URL, MEDIANET_LOGIN_ENDPOINT
from .fixtures import (
    BIDDERS_EMPTY_RESPONSE,
    BIDDERS_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    SERVER_ERROR_RESPONSE,
)

# Bidders endpoint pattern
MEDIANET_BIDDERS_ENDPOINT = f"{MEDIANET_BASE_URL}/api/v2/deals/ad-formats"


class TestListDemandPartners:
    """Tests for list_demand_partners tool."""

    @pytest.mark.asyncio
    async def test_success_returns_partners(self, mock_medianet_api: respx.MockRouter):
        """Test successful retrieval of demand partners (bidders)."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=BIDDERS_RESPONSE)
        )

        result = await mn_list_demand_partners()

        assert result["success"] is True
        assert "demand_partners" in result
        assert len(result["demand_partners"]) == 4

        # Verify partner data structure is normalized
        partner = result["demand_partners"][0]
        assert "id" in partner
        assert "name" in partner

    @pytest.mark.asyncio
    async def test_request_url_structure(self, mock_medianet_api: respx.MockRouter):
        """Verify the request hits the correct bidders endpoint."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=BIDDERS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(side_effect=capture_request)

        await mn_list_demand_partners(ad_format_id=0)

        assert captured_request is not None

        # Should hit /api/v2/deals/ad-formats/0/demand-partners
        expected_url = f"{MEDIANET_BASE_URL}/api/v2/deals/ad-formats/0/demand-partners"
        assert str(captured_request.url) == expected_url

    @pytest.mark.asyncio
    async def test_different_ad_format_ids(self, mock_medianet_api: respx.MockRouter):
        """Verify different ad_format_id values are passed correctly."""
        captured_requests = []

        def capture_request(request: httpx.Request) -> httpx.Response:
            captured_requests.append(request)
            return httpx.Response(200, json=BIDDERS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(side_effect=capture_request)

        # Test Banner (0)
        await mn_list_demand_partners(ad_format_id=0)
        # Reset client for fresh token
        import medianet_mcp

        medianet_mcp._medianet_client = None

        # Test Video (1)
        await mn_list_demand_partners(ad_format_id=1)
        medianet_mcp._medianet_client = None

        # Test Native (2)
        await mn_list_demand_partners(ad_format_id=2)

        # Verify URLs
        assert "/ad-formats/0/demand-partners" in str(captured_requests[0].url)
        assert "/ad-formats/1/demand-partners" in str(captured_requests[1].url)
        assert "/ad-formats/2/demand-partners" in str(captured_requests[2].url)

    @pytest.mark.asyncio
    async def test_normalized_response_format(self, mock_medianet_api: respx.MockRouter):
        """Test that response is normalized to list of {id, name} objects."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=BIDDERS_RESPONSE)
        )

        result = await mn_list_demand_partners()

        partners = result["demand_partners"]

        # Verify each partner has id and name
        assert any(p["id"] == 1 and p["name"] == "AppNexus" for p in partners)
        assert any(p["id"] == 2 and p["name"] == "Rubicon" for p in partners)
        assert any(p["id"] == 3 and p["name"] == "OpenX" for p in partners)
        assert any(p["id"] == 4 and p["name"] == "Index Exchange" for p in partners)

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_medianet_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=BIDDERS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(side_effect=capture_request)

        await mn_list_demand_partners()

        # Verify headers
        assert captured_request.headers.get("token") == "medianet-auth-token-12345"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await mn_list_demand_partners()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(
            side_effect=httpx.ConnectError("Connection refused")
        )

        result = await mn_list_demand_partners()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_empty_partners_list(self, mock_medianet_api: respx.MockRouter):
        """Test handling of empty bidders list."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=BIDDERS_EMPTY_RESPONSE)
        )

        result = await mn_list_demand_partners()

        assert result["success"] is True
        assert result["demand_partners"] == []

    @pytest.mark.asyncio
    async def test_default_ad_format_id(self, mock_medianet_api: respx.MockRouter):
        """Verify default ad_format_id=0 (Banner) is used."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=BIDDERS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_BIDDERS_ENDPOINT).mock(side_effect=capture_request)

        # Call without explicit ad_format_id
        await mn_list_demand_partners()

        # Should use default ad_format_id=0
        assert "/ad-formats/0/demand-partners" in str(captured_request.url)
