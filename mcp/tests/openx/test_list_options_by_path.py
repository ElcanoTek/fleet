"""Tests for the generic optionsByPath OpenX MCP tool."""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_list_options_by_path

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import GRAPHQL_ERROR_RESPONSE, OPTIONS_BY_PATH_RESPONSE


class TestListOptionsByPath:
    @pytest.mark.asyncio
    async def test_success_returns_options(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=OPTIONS_BY_PATH_RESPONSE)
        )

        result = await ox_list_options_by_path("deal.package.targeting.technographic.language")

        assert result["success"] is True
        assert result["path"] == "deal.package.targeting.technographic.language"
        assert result["options"][0]["id"] == "es"
        assert result["options"][0]["name"] == "Spanish"

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_openx_graphql: respx.MockRouter):
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=OPTIONS_BY_PATH_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=capture_request)

        result = await ox_list_options_by_path(
            "deal.package.targeting.technographic.language",
            filter={"search": "spanish"},
        )

        assert result["success"] is True
        payload = json.loads(captured_request.content)
        assert "optionsByPath" in payload["query"]
        assert payload["variables"]["path"] == "deal.package.targeting.technographic.language"
        assert payload["variables"]["filter"] == {"search": "spanish"}

    @pytest.mark.asyncio
    async def test_graphql_error_handling(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)
        )

        result = await ox_list_options_by_path("deal.package.targeting.technographic.language")

        assert result["success"] is False
        assert "error" in result
