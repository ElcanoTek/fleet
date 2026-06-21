"""
Tests for the list_demand_partners MCP tool.

Verifies correct GraphQL query structure and response parsing.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_list_demand_partners

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import DEMAND_PARTNERS_RESPONSE, GRAPHQL_ERROR_RESPONSE


class TestListDemandPartners:
    """Tests for list_demand_partners tool."""

    @pytest.mark.asyncio
    async def test_success_returns_partners(self, mock_openx_graphql: respx.MockRouter):
        """Test successful retrieval of demand partners."""
        # Configure mock response
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEMAND_PARTNERS_RESPONSE)
        )

        result = await ox_list_demand_partners()

        assert result["success"] is True
        assert "demand_partners" in result
        assert len(result["demand_partners"]) == 4

        # Verify partner data structure
        partner = result["demand_partners"][0]
        assert "id" in partner
        assert "name" in partner

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify the exact GraphQL query sent to the API."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEMAND_PARTNERS_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        await ox_list_demand_partners()

        # Verify request was made
        assert captured_request is not None

        # Parse and verify the payload
        payload = json.loads(captured_request.content)

        assert "query" in payload
        query = payload["query"]

        # Verify query contains expected GraphQL operation
        assert "ListDemandPartners" in query or "optionsByPath" in query
        assert "deal.deal_participants.demand_partner" in query

        # Verify headers
        assert captured_request.headers.get("x-apikey") == "test-openx-api-key-12345"
        assert captured_request.headers.get("Content-Type") == "application/json"

    @pytest.mark.asyncio
    async def test_correct_parsing_of_options_by_path(self, mock_openx_graphql: respx.MockRouter):
        """Test that optionsByPath response is correctly parsed."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEMAND_PARTNERS_RESPONSE)
        )

        result = await ox_list_demand_partners()

        # Verify each partner has expected fields
        partners = result["demand_partners"]

        assert any(p["id"] == "TTD" and p["name"] == "The Trade Desk" for p in partners)
        assert any(p["id"] == "DV360" and p["name"] == "Google DV360" for p in partners)
        assert any(p["id"] == "CRIMTAN" and p["name"] == "Crimtan" for p in partners)

    @pytest.mark.asyncio
    async def test_graphql_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of GraphQL errors in response."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)
        )

        result = await ox_list_demand_partners()

        assert result["success"] is False
        assert "error" in result
        assert "GraphQL errors" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(500, text="Internal Server Error")
        )

        result = await ox_list_demand_partners()

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_empty_partners_list(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of empty partners list."""
        empty_response = {"data": {"optionsByPath": []}}
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=empty_response))

        result = await ox_list_demand_partners()

        assert result["success"] is True
        assert result["demand_partners"] == []
