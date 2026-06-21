"""
Tests for the list_deals MCP tool.

Verifies pagination handling and response parsing.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_list_deals

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import (
    DEALS_LIST_EMPTY_RESPONSE,
    DEALS_LIST_RESPONSE_PAGE1,
    DEALS_LIST_RESPONSE_PAGE2,
    GRAPHQL_ERROR_RESPONSE,
)


class TestListDeals:
    """Tests for list_deals tool."""

    @pytest.mark.asyncio
    async def test_single_page_success(self, mock_openx_graphql: respx.MockRouter):
        """Test successful retrieval of a single page of deals."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)
        )

        result = await ox_list_deals(limit=10, offset=0)

        assert result["success"] is True
        assert "deals" in result
        assert len(result["deals"]) == 2

        # Verify deal structure
        deal = result["deals"][0]
        assert "id" in deal
        assert "deal_id" in deal
        assert "name" in deal
        assert "status" in deal
        assert "currency" in deal
        assert "deal_price" in deal

    @pytest.mark.asyncio
    async def test_pagination_variables(self, mock_openx_graphql: respx.MockRouter):
        """Verify limit and offset are correctly passed to GraphQL."""
        captured_requests = []

        def capture_request(request: httpx.Request) -> httpx.Response:
            captured_requests.append(request)
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE2)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        # Request second page
        await ox_list_deals(limit=5, offset=10)

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)

        assert "variables" in payload
        assert payload["variables"]["limit"] == 5
        assert payload["variables"]["offset"] == 10

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify the exact GraphQL query structure."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        await ox_list_deals()

        payload = json.loads(captured_request.content)
        query = payload["query"]

        # Verify query structure
        assert "ListDeals" in query or "deals" in query
        assert "$limit: Int!" in query
        assert "$offset: Int!" in query

        # Verify requested fields
        assert "deal_id" in query
        assert "name" in query
        assert "status" in query
        assert "currency" in query
        assert "deal_price" in query

    @pytest.mark.asyncio
    async def test_empty_results(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of empty deals list."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_EMPTY_RESPONSE)
        )

        result = await ox_list_deals()

        assert result["success"] is True
        assert result["deals"] == []

    @pytest.mark.asyncio
    async def test_default_pagination_values(self, mock_openx_graphql: respx.MockRouter):
        """Verify default limit=10 and offset=0 are used."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        # Call without explicit pagination params
        await ox_list_deals()

        payload = json.loads(captured_request.content)
        assert payload["variables"]["limit"] == 10
        assert payload["variables"]["offset"] == 0

    @pytest.mark.asyncio
    async def test_graphql_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of GraphQL errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)
        )

        result = await ox_list_deals()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(503, text="Service Unavailable")
        )

        result = await ox_list_deals()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_deal_fields_are_present(self, mock_openx_graphql: respx.MockRouter):
        """Verify all expected deal fields are returned."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE_PAGE1)
        )

        result = await ox_list_deals()

        deal = result["deals"][0]

        # All fields from the query should be present
        expected_fields = [
            "id",
            "deal_id",
            "name",
            "status",
            "currency",
            "deal_price",
            "pmp_deal_type",
            "start_date",
            "end_date",
            "created_date",
            "modified_date",
        ]

        for field in expected_fields:
            assert field in deal, f"Missing field: {field}"
