"""
Tests for the deal MCP tools for Xandr (create_xandr_deal, list_xandr_deals, get_xandr_deal).

This is the HIGHEST PRIORITY test file. It validates:
- Correct API endpoint and payload structure
- Token-based authentication flow
- Token refresh on 401 responses
- Returned MCP output structure
- Error handling for validation, HTTP, and network errors
"""

import json
import os
import sys
from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from xandr_mcp import create_xandr_deal, get_xandr_deal, list_xandr_deals

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_DEAL_ENDPOINT
from .fixtures import (
    CREATE_DEAL_SUCCESS_RESPONSE,
    DEAL_NOT_FOUND_RESPONSE,
    DEAL_RESPONSE,
    DEALS_LIST_EMPTY_RESPONSE,
    DEALS_LIST_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    SAMPLE_DEAL_PAYLOAD,
    SAMPLE_DEAL_PAYLOAD_FULL,
    SERVER_ERROR_RESPONSE,
    UNAUTHORIZED_RESPONSE,
    VALIDATION_ERROR_RESPONSE,
)

# =============================================================================
# create_xandr_deal tests
# =============================================================================


class TestCreateDealSuccess:
    """Tests for successful deal creation scenarios."""

    @pytest.mark.asyncio
    async def test_create_deal_success(self, mock_xandr_api: respx.MockRouter):
        """Test successful deal creation returns expected structure."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is True
        assert "data" in result
        assert result["data"]["deal"]["name"] == "Elcano_Xandr_New_Deal"
        assert result["data"]["deal"]["id"] == 5010

    @pytest.mark.asyncio
    async def test_returned_mcp_output_structure(self, mock_xandr_api: respx.MockRouter):
        """Verify the MCP tool output has correct structure."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Verify top-level structure
        assert "success" in result
        assert result["success"] is True
        assert "data" in result

        # Verify deal fields from response
        deal = result["data"]["deal"]
        assert "id" in deal
        assert "name" in deal
        assert "state" in deal
        assert "deal_type" in deal
        assert "buyers" in deal


class TestCreateDealPayloadStructure:
    """Tests verifying the exact HTTP request payload."""

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_xandr_api: respx.MockRouter):
        """Verify exact request URL, method, and payload structure."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD_FULL)

        assert captured_request is not None
        payload = json.loads(captured_request.content)

        # Top-level must have "deal" key
        assert "deal" in payload
        deal = payload["deal"]

        # Core required fields (transformed from legacy names)
        assert deal["name"] == "Elcano_Xandr_Complete_Deal"
        assert deal["code"] == "elcano_xandr_complete_deal"
        assert deal["type"]["id"] == 2
        assert deal["type"]["name"] == "Private Auction"
        assert deal["buyer"]["id"] == 123

        # Optional fields (transformed from legacy formats)
        assert deal["description"] == "A complete deal with all optional fields"
        assert deal["active"] is True
        assert deal["start_date"] == "2024-03-01 00:00:00"
        assert deal["end_date"] == "2024-12-31 23:59:59"
        assert deal["payment_type"] == "cpm"
        assert deal["currency"] == "USD"
        assert deal["use_deal_floor"] is True
        assert deal["ask_price"] == 5.00
        assert deal["member_id"] == 9544

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_xandr_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Verify headers
        assert captured_request.headers.get("Authorization") == "xandr-auth-token-abc123"
        assert captured_request.headers.get("Content-Type") == "application/json"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_request_url(self, mock_xandr_api: respx.MockRouter):
        """Verify request is sent to correct URL."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert str(captured_request.url) == XANDR_DEAL_ENDPOINT


class TestCreateDealTokenRefresh:
    """Tests for token refresh behavior on 401 responses."""

    @pytest.mark.asyncio
    async def test_token_refresh_on_401(self, mock_xandr_api: respx.MockRouter):
        """Test that 401 response triggers re-login and retry."""
        call_count = {"login": 0, "create": 0}

        def login_side_effect(request: httpx.Request) -> httpx.Response:
            call_count["login"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        def create_side_effect(request: httpx.Request) -> httpx.Response:
            call_count["create"] += 1
            if call_count["create"] == 1:
                return httpx.Response(401, json=UNAUTHORIZED_RESPONSE)
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(side_effect=login_side_effect)
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=create_side_effect)

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Should succeed after retry
        assert result["success"] is True
        # Should have logged in twice (initial + refresh)
        assert call_count["login"] == 2
        # Should have made two create attempts
        assert call_count["create"] == 2


class TestCreateDealValidation:
    """Tests for input validation."""

    @pytest.mark.asyncio
    async def test_missing_deal_key(
        self,
        mock_xandr_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that payload without 'deal' key returns validation error."""
        result = await create_xandr_deal(payload={"name": "Bad Payload"})

        assert result["success"] is False
        assert "error" in result
        assert "deal" in result["error"].lower()

    @pytest.mark.asyncio
    async def test_missing_required_fields(
        self,
        mock_xandr_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that missing required fields return validation error."""
        incomplete_payload = {
            "deal": {
                "name": "Test Deal",
                # Missing: deal_type, buyers
            }
        }

        result = await create_xandr_deal(payload=incomplete_payload)

        assert result["success"] is False
        assert "error" in result
        assert "Missing required fields" in result["error"]

    @pytest.mark.asyncio
    async def test_empty_deal_object(
        self,
        mock_xandr_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that empty deal object fails validation."""
        result = await create_xandr_deal(payload={"deal": {}})

        assert result["success"] is False
        assert "error" in result
        assert "Missing required fields" in result["error"]

    @pytest.mark.asyncio
    async def test_empty_payload(
        self,
        mock_xandr_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that empty payload fails validation."""
        result = await create_xandr_deal(payload={})

        assert result["success"] is False
        assert "error" in result


class TestCreateDealErrorHandling:
    """Tests for error handling in deal creation."""

    @pytest.mark.asyncio
    async def test_validation_error_response(self, mock_xandr_api: respx.MockRouter):
        """Test handling of 422 validation errors from API."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(return_value=httpx.Response(422, json=VALIDATION_ERROR_RESPONSE))

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result
        assert "422" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of HTTP 500 errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE))

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=httpx.ConnectError("Connection refused"))

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result


class TestCreateDealCredentialValidation:
    """Tests for credential validation - should fail before HTTP call."""

    @pytest.mark.asyncio
    async def test_missing_credentials_fails_before_http(self):
        """Test that missing credentials fails before making an HTTP call."""
        import xandr_mcp

        xandr_mcp._xandr_client = None

        with (
            patch.dict(
                os.environ,
                {
                    "XANDR_USERNAME": "",
                    "XANDR_PASSWORD": "",
                },
                clear=False,
            ),
            respx.mock(assert_all_called=False) as mock,
        ):
            # Configure routes that should NOT be called
            login_route = mock.post(XANDR_AUTH_ENDPOINT).mock(
                return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
            )
            create_route = mock.post(XANDR_DEAL_ENDPOINT).mock(
                return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
            )

            result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD)

            # Verify it failed
            assert result["success"] is False
            assert "error" in result
            assert "not configured" in result["error"].lower()

            # Verify NO HTTP calls were made
            assert login_route.call_count == 0
            assert create_route.call_count == 0


class TestCreateDealFullWorkflow:
    """Integration-style tests for complete deal creation workflows."""

    @pytest.mark.asyncio
    async def test_complete_deal_creation_with_all_fields(self, mock_xandr_api: respx.MockRouter):
        """Test creating a complete deal with all optional fields."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        result = await create_xandr_deal(payload=SAMPLE_DEAL_PAYLOAD_FULL)

        # Verify success
        assert result["success"] is True
        assert "data" in result

        # Verify payload structure (fields transformed to API format)
        payload = json.loads(captured_request.content)
        deal = payload["deal"]

        assert deal["name"] == "Elcano_Xandr_Complete_Deal"
        assert deal["code"] == "elcano_xandr_complete_deal"
        assert deal["description"] == "A complete deal with all optional fields"
        assert deal["active"] is True
        assert deal["start_date"] == "2024-03-01 00:00:00"
        assert deal["end_date"] == "2024-12-31 23:59:59"
        assert deal["type"]["id"] == 2
        assert deal["payment_type"] == "cpm"
        assert deal["currency"] == "USD"
        assert deal["use_deal_floor"] is True
        assert deal["ask_price"] == 5.00
        assert deal["buyer"]["id"] == 123
        assert deal["member_id"] == 9544


# =============================================================================
# list_xandr_deals tests
# =============================================================================


class TestListDeals:
    """Tests for list_xandr_deals tool."""

    @pytest.mark.asyncio
    async def test_list_deals_success(self, mock_xandr_api: respx.MockRouter):
        """Test successful retrieval of deals."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        result = await list_xandr_deals(member_id=9544)

        assert result["success"] is True
        assert "deals" in result
        assert len(result["deals"]) == 2

    @pytest.mark.asyncio
    async def test_member_id_parameter(self, mock_xandr_api: respx.MockRouter):
        """Verify member_id is correctly passed as query parameter."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await list_xandr_deals(member_id=9544)

        assert captured_request is not None
        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)
        assert query_params["member_id"] == ["9544"]

    @pytest.mark.asyncio
    async def test_empty_results(self, mock_xandr_api: respx.MockRouter):
        """Test handling of empty deals list."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_EMPTY_RESPONSE)
        )

        result = await list_xandr_deals(member_id=9544)

        assert result["success"] is True
        assert result["deals"] == []

    @pytest.mark.asyncio
    async def test_deal_fields_are_present(self, mock_xandr_api: respx.MockRouter):
        """Verify all expected deal fields are returned."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        result = await list_xandr_deals(member_id=9544)

        deal = result["deals"][0]

        expected_fields = [
            "id",
            "name",
            "state",
            "deal_type",
            "buyers",
            "member_id",
        ]

        for field in expected_fields:
            assert field in deal, f"Missing field: {field}"

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_xandr_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEALS_LIST_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await list_xandr_deals(member_id=9544)

        assert captured_request.headers.get("Authorization") == "xandr-auth-token-abc123"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await list_xandr_deals(member_id=9544)

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            side_effect=httpx.ConnectError("Connection refused")
        )

        result = await list_xandr_deals(member_id=9544)

        assert result["success"] is False
        assert "error" in result


# =============================================================================
# get_xandr_deal tests
# =============================================================================


class TestGetDeal:
    """Tests for get_xandr_deal tool."""

    @pytest.mark.asyncio
    async def test_valid_deal_id(self, mock_xandr_api: respx.MockRouter):
        """Test successful retrieval of a deal by ID."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=5001)

        assert result["success"] is True
        assert "deal" in result

        deal = result["deal"]
        assert deal["id"] == 5001
        assert deal["name"] == "Elcano_Xandr_Premium_Banner"
        assert deal["state"] == "active"

    @pytest.mark.asyncio
    async def test_deal_includes_all_fields(self, mock_xandr_api: respx.MockRouter):
        """Verify all expected fields are returned in deal response."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=5001)

        deal = result["deal"]

        # Core fields
        assert "id" in deal
        assert "name" in deal
        assert "state" in deal
        assert "deal_type" in deal
        assert "buyers" in deal
        assert "member_id" in deal

        # Extended fields
        assert "description" in deal
        assert "payment_type" in deal
        assert "currency" in deal
        assert "floor_price" in deal

    @pytest.mark.asyncio
    async def test_uses_id_query_parameter(self, mock_xandr_api: respx.MockRouter):
        """Verify get_deal uses id query parameter."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await get_xandr_deal(deal_id=5001)

        # Verify URL uses the deal endpoint with id param
        assert XANDR_DEAL_ENDPOINT in str(captured_request.url)

        parsed_url = urlparse(str(captured_request.url))
        query_params = parse_qs(parsed_url.query)
        assert query_params["id"] == ["5001"]

    @pytest.mark.asyncio
    async def test_missing_deal_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling when deal is not found."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_NOT_FOUND_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=99999)

        assert result["success"] is False
        assert "error" in result
        assert "Deal not found" in result["error"]
        assert "99999" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of HTTP errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=5001)

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_xandr_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            side_effect=httpx.ConnectError("Connection refused")
        )

        result = await get_xandr_deal(deal_id=5001)

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_xandr_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=capture_request)

        await get_xandr_deal(deal_id=5001)

        assert captured_request.headers.get("Authorization") == "xandr-auth-token-abc123"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_buyers_structure(self, mock_xandr_api: respx.MockRouter):
        """Verify buyers are correctly parsed."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=5001)

        buyers = result["deal"]["buyers"]
        assert len(buyers) == 1

        buyer = buyers[0]
        assert buyer["id"] == 123
        assert buyer["name"] == "Test Buyer Alpha"

    @pytest.mark.asyncio
    async def test_deal_type_structure(self, mock_xandr_api: respx.MockRouter):
        """Verify deal_type is correctly parsed."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEAL_RESPONSE)
        )

        result = await get_xandr_deal(deal_id=5001)

        deal_type = result["deal"]["deal_type"]
        assert deal_type["id"] == 2
        assert deal_type["name"] == "Private Auction"
