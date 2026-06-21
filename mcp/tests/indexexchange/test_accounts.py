"""
Tests for Index Exchange Accounts MCP tool.

Validates:
- ix_list_account_information: correct endpoint, query params
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import ix_list_account_information

from .conftest import IX_ACCOUNTS_ENDPOINT, IX_LOGIN_ENDPOINT
from .fixtures import ACCOUNTS_RESPONSE, LOGIN_SUCCESS_RESPONSE


class TestListAccountInformation:
    """Tests for ix_list_account_information tool."""

    @pytest.mark.asyncio
    async def test_list_accounts_no_filters(self, mock_ix_api: respx.MockRouter):
        """Test listing accounts without any filters."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_ACCOUNTS_ENDPOINT).mock(return_value=httpx.Response(200, json=ACCOUNTS_RESPONSE))

        result = await ix_list_account_information()
        assert result["success"] is True
        assert len(result["accounts"]) == 2
        assert result["accounts"][0]["accountName"] == "Test Publisher"

    @pytest.mark.asyncio
    async def test_list_accounts_with_filters(self, mock_ix_api: respx.MockRouter):
        """Test listing accounts with all filter params."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=ACCOUNTS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture)

        result = await ix_list_account_information(
            account_ids=[100, 200],
            account_type_ids=[1],
            legacy_marketplace_ids=[500],
        )
        assert result["success"] is True

        url_str = str(captured_request.url)
        assert "accountIDs=" in url_str
        assert "accountTypeIDs=1" in url_str
        assert "legacyMarketplaceIDs=500" in url_str

    @pytest.mark.asyncio
    async def test_correct_endpoint_and_method(self, mock_ix_api: respx.MockRouter):
        """Test correct API endpoint and HTTP method."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=ACCOUNTS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture)

        await ix_list_account_information()
        assert captured_request.method == "GET"
        assert "/api/accounts/v2/accounts/" in str(captured_request.url)
