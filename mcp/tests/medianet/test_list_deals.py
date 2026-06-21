"""
Tests for the list_deals MCP tool for Media.net Select.

Verifies pagination handling, filter parameters, and response parsing.
"""

import os
import sys
from urllib.parse import parse_qs, urlparse

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import mn_list_deals

from .conftest import MEDIANET_DEALS_ENDPOINT, MEDIANET_LOGIN_ENDPOINT
from .fixtures import (
    DEALS_LIST_EMPTY_RESPONSE,
    DEALS_LIST_RESPONSE_PAGE1,
    DEALS_LIST_RESPONSE_PAGE2,
    LOGIN_SUCCESS_RESPONSE,
    SERVER_ERROR_RESPONSE,
)


class TestListDeals:
    """Tests for list_deals tool."""

    @pytest.mark.asyncio
    async def test_single_page_success(self, mock_medianet_api: respx.MockRouter):
        """Test successful retrieval of a single page of deals."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)
        )

        result = await mn_list_deals(page_no=1, page_size=50)

        assert result["success"] is True
        assert "deals" in result
        assert len(result["deals"]) == 2

        # Verify deal structure
        deal = result["deals"][0]
        assert "id" in deal
        assert "deal_id" in deal
        assert "display_name" in deal
        assert "status" in deal
        assert "ad_format" in deal
        assert "margin" in deal

    @pytest.mark.asyncio
    async def test_pagination_parameters(self, mock_medianet_api: respx.MockRouter):
        """Verify page_no and page_size are correctly passed as query params."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE2)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        # Request second page with smaller page size
        await mn_list_deals(page_no=2, page_size=10)

        assert captured_request is not None
        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        assert query_params["page_no"] == ["2"]
        assert query_params["page_size"] == ["10"]

    @pytest.mark.asyncio
    async def test_status_filter(self, mock_medianet_api: respx.MockRouter):
        """Verify filter[status][] parameters are correctly encoded."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        # Filter by active (1) and inactive (0) status
        await mn_list_deals(status=[1, 0])

        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        # Should have filter[status][] repeated for each value
        assert "filter[status][]" in query_params
        assert "1" in query_params["filter[status][]"]
        assert "0" in query_params["filter[status][]"]

    @pytest.mark.asyncio
    async def test_deal_id_filter(self, mock_medianet_api: respx.MockRouter):
        """Verify filter[deal_id][] parameters are correctly encoded."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        # Filter by specific deal IDs
        await mn_list_deals(deal_ids=["ELC-MN-2024-001", "ELC-MN-2024-002"])

        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        assert "filter[deal_id][]" in query_params
        assert "ELC-MN-2024-001" in query_params["filter[deal_id][]"]
        assert "ELC-MN-2024-002" in query_params["filter[deal_id][]"]

    @pytest.mark.asyncio
    async def test_combined_filters(self, mock_medianet_api: respx.MockRouter):
        """Verify multiple filters can be combined."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_list_deals(page_no=1, page_size=25, status=[1], deal_ids=["ELC-MN-2024-001"])

        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        assert query_params["page_no"] == ["1"]
        assert query_params["page_size"] == ["25"]
        assert "filter[status][]" in query_params
        assert "filter[deal_id][]" in query_params

    @pytest.mark.asyncio
    async def test_empty_results(self, mock_medianet_api: respx.MockRouter):
        """Test handling of empty deals list."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_EMPTY_RESPONSE)
        )

        result = await mn_list_deals()

        assert result["success"] is True
        assert result["deals"] == []

    @pytest.mark.asyncio
    async def test_default_pagination_values(self, mock_medianet_api: respx.MockRouter):
        """Verify default page_no=1 and page_size=50 are used."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        # Call without explicit pagination params
        await mn_list_deals()

        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)

        assert query_params["page_no"] == ["1"]
        assert query_params["page_size"] == ["50"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await mn_list_deals()

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

        result = await mn_list_deals()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_deal_fields_are_present(self, mock_medianet_api: respx.MockRouter):
        """Verify all expected deal fields are returned."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)
        )

        result = await mn_list_deals()

        deal = result["deals"][0]

        # All fields from the response should be present
        expected_fields = [
            "id",
            "deal_id",
            "display_name",
            "status",
            "ad_format",
            "margin",
            "margin_type",
            "start_date",
            "demand_partners",
            "environments",
        ]

        for field in expected_fields:
            assert field in deal, f"Missing field: {field}"

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_medianet_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.get(url__startswith=MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_list_deals()

        # Verify headers
        assert captured_request.headers.get("token") == "medianet-auth-token-12345"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")
