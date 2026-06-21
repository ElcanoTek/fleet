"""
Tests for the ox_introspect_type MCP tool.

This validates the GraphQL introspection tool that helps discover
exact field names for any named type, preventing schema mismatches.
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_introspect_type

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import (
    INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE,
    INTROSPECT_TARGETING_PARAMS_RESPONSE,
    INTROSPECT_TYPE_NOT_FOUND_RESPONSE,
)


class TestIntrospectTypeSuccess:
    """Tests for successful type introspection."""

    @pytest.mark.asyncio
    async def test_introspect_geographic_item_type(self, mock_openx_graphql: respx.MockRouter):
        """Test introspecting TargetingGeographicItemCreateParams returns field info."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
        )

        result = await ox_introspect_type("TargetingGeographicItemCreateParams")

        assert result["success"] is True
        assert "type_info" in result

        type_info = result["type_info"]
        assert type_info["name"] == "TargetingGeographicItemCreateParams"
        assert type_info["kind"] == "INPUT_OBJECT"
        assert "inputFields" in type_info

        # Should have country and region fields
        field_names = [f["name"] for f in type_info["inputFields"]]
        assert "country" in field_names
        assert "region" in field_names

    @pytest.mark.asyncio
    async def test_introspect_targeting_params_type(self, mock_openx_graphql: respx.MockRouter):
        """Test introspecting TargetingCreateParams returns field info."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=INTROSPECT_TARGETING_PARAMS_RESPONSE)
        )

        result = await ox_introspect_type("TargetingCreateParams")

        assert result["success"] is True
        assert "type_info" in result

        type_info = result["type_info"]
        assert type_info["name"] == "TargetingCreateParams"
        assert type_info["kind"] == "INPUT_OBJECT"

        # Should have geographic and rendering_context fields
        field_names = [f["name"] for f in type_info["inputFields"]]
        assert "geographic" in field_names
        assert "rendering_context" in field_names
        assert "mapping_hints" in result
        assert any(
            "Language targeting belongs under targeting.technographic.language" in hint
            for hint in result["mapping_hints"]
        )

    @pytest.mark.asyncio
    async def test_introspect_content_type_returns_mapping_hints(self, mock_openx_graphql: respx.MockRouter):
        """TargetingContentCreateParams should include guidance for common mapping mistakes."""
        content_response = {
            "data": {
                "__type": {
                    "name": "TargetingContentCreateParams",
                    "kind": "INPUT_OBJECT",
                    "inputFields": [
                        {
                            "name": "keywords",
                            "type": {
                                "name": "TargetingContentKeywordsCreateParams",
                                "kind": "INPUT_OBJECT",
                                "ofType": None,
                            },
                        },
                        {
                            "name": "page_url",
                            "type": {
                                "name": "TargetingContentPageUrlCreateParams",
                                "kind": "INPUT_OBJECT",
                                "ofType": None,
                            },
                        },
                    ],
                }
            }
        }
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(return_value=httpx.Response(200, json=content_response))

        result = await ox_introspect_type("TargetingContentCreateParams")

        assert result["success"] is True
        assert any("does not expose OpenX language targeting" in hint for hint in result["mapping_hints"])
        assert any("TargetingDomainCreateParams" in hint for hint in result["mapping_hints"])


class TestIntrospectTypeNotFound:
    """Tests for type not found scenarios."""

    @pytest.mark.asyncio
    async def test_introspect_nonexistent_type(self, mock_openx_graphql: respx.MockRouter):
        """Test introspecting a nonexistent type returns appropriate error."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=INTROSPECT_TYPE_NOT_FOUND_RESPONSE)
        )

        result = await ox_introspect_type("NonExistentType")

        assert result["success"] is False
        assert "error" in result
        assert "not found" in result["error"].lower()


class TestIntrospectTypeErrorHandling:
    """Tests for error handling in introspection."""

    @pytest.mark.asyncio
    async def test_introspect_network_error(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of network errors during introspection."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=httpx.ConnectError("Connection refused"))

        result = await ox_introspect_type("TargetingCreateParams")

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_introspect_http_error(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of HTTP errors during introspection."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(500, text="Internal Server Error")
        )

        result = await ox_introspect_type("TargetingCreateParams")

        assert result["success"] is False
        assert "error" in result


class TestIntrospectTypeQuery:
    """Tests verifying the introspection query structure."""

    @pytest.mark.asyncio
    async def test_introspection_query_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify the introspection query uses correct GraphQL syntax."""
        import json

        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        await ox_introspect_type("TargetingGeographicItemCreateParams")

        assert captured_request is not None
        payload = json.loads(captured_request.content)

        # Verify query contains introspection elements
        query = payload["query"]
        assert "__type" in query
        assert "IntrospectType" in query
        assert "inputFields" in query
        assert "ofType" in query

        # Verify variables
        assert payload["variables"]["typeName"] == "TargetingGeographicItemCreateParams"
