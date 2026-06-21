"""
Tests for the create_deal MCP tool for Media.net Select.

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

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import mn_create_deal

from .conftest import MEDIANET_DEALS_ENDPOINT, MEDIANET_LOGIN_ENDPOINT
from .fixtures import (
    CREATE_DEAL_SUCCESS_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    SAMPLE_DEAL_PAYLOAD,
    SAMPLE_DEAL_PAYLOAD_FULL,
    SERVER_ERROR_RESPONSE,
    UNAUTHORIZED_RESPONSE,
    VALIDATION_ERROR_RESPONSE,
)


class TestCreateDealSuccess:
    """Tests for successful deal creation scenarios."""

    @pytest.mark.asyncio
    async def test_create_deal_success(self, mock_medianet_api: respx.MockRouter):
        """Test successful deal creation returns expected structure."""
        # Mock login
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        # Mock create deal
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is True
        assert "deal" in result
        assert result["deal"]["deal_id"] == "ELC-MN-2024-NEW"
        assert result["deal"]["display_name"] == "Elcano_MediaNet_Test_Deal"

    @pytest.mark.asyncio
    async def test_create_deal_with_token(self, mock_medianet_api_with_token: respx.MockRouter):
        """Test deal creation using pre-existing token (no login needed)."""
        mock_medianet_api_with_token.post(MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is True
        assert "deal" in result

    @pytest.mark.asyncio
    async def test_returned_mcp_output_structure(self, mock_medianet_api: respx.MockRouter):
        """Verify the MCP tool output has correct structure."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Verify top-level structure
        assert "success" in result
        assert result["success"] is True
        assert "deal" in result

        # Verify deal fields from response
        deal = result["deal"]
        assert "id" in deal
        assert "deal_id" in deal
        assert "display_name" in deal
        assert "status" in deal
        assert "ad_format" in deal
        assert "margin" in deal


class TestCreateDealPayloadStructure:
    """Tests verifying the exact HTTP request payload."""

    @pytest.mark.asyncio
    async def test_request_payload_structure(self, mock_medianet_api: respx.MockRouter):
        """Verify exact request URL, method, and payload structure."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD_FULL)

        assert captured_request is not None
        payload = json.loads(captured_request.content)

        # Core required fields
        assert payload["deal_id"] == "ELC-MN-2024-FULL"
        assert payload["display_name"] == "Elcano_MediaNet_Complete_Deal"
        assert payload["start_date"] == "2024-03-01T00:00:00Z"
        assert payload["ad_format"] == 0
        assert payload["margin"] == 30.0
        assert payload["margin_type"] == 1  # Transformed from "percentage" to 1
        assert payload["status"] == 1

        # is_always_on should be removed by transform
        assert "is_always_on" not in payload

        # Demand partners (transformed from bidders objects to list of ID strings)
        assert "demand_partners" in payload
        assert len(payload["demand_partners"]) == 2
        assert "bidders" not in payload

        # Environments (capitalized by transform)
        assert payload["environments"] == ["Web", "App"]

        # Optional fields (floor_price renamed to bid_floor)
        assert payload["end_date"] == "2024-12-31T23:59:59Z"
        assert payload["bid_floor"] == 2.50
        assert "floor_price" not in payload
        assert "domains" in payload
        assert "geos" in payload
        assert "devices" in payload

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_medianet_api: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Verify headers
        assert captured_request.headers.get("token") == "medianet-auth-token-12345"
        assert captured_request.headers.get("Content-Type") == "application/json"
        assert "victoria-terminal" in captured_request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_request_url(self, mock_medianet_api: respx.MockRouter):
        """Verify request is sent to correct URL."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert str(captured_request.url) == MEDIANET_DEALS_ENDPOINT


class TestCreateDealTokenRefresh:
    """Tests for token refresh behavior on 401 responses."""

    @pytest.mark.asyncio
    async def test_token_refresh_on_401(self, mock_medianet_api: respx.MockRouter):
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

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(side_effect=login_side_effect)
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=create_side_effect)

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        # Should succeed after retry
        assert result["success"] is True
        # Should have logged in twice (initial + refresh)
        assert call_count["login"] == 2
        # Should have made two create attempts
        assert call_count["create"] == 2


class TestCreateDealValidation:
    """Tests for input validation."""

    @pytest.mark.asyncio
    async def test_missing_required_fields(
        self,
        mock_medianet_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that missing required fields return validation error."""
        incomplete_payload = {
            "deal_id": "test-deal",
            "display_name": "Test Deal",
            # Missing: start_date, ad_format, margin, margin_type, bidders, environments, status, is_always_on
        }

        result = await mn_create_deal(payload=incomplete_payload)

        assert result["success"] is False
        assert "error" in result
        assert "Missing required fields" in result["error"]

    @pytest.mark.asyncio
    async def test_empty_payload(
        self,
        mock_medianet_api: respx.MockRouter,  # noqa: ARG002 - fixture needed for env setup
    ):
        """Test that empty payload fails validation."""
        result = await mn_create_deal(payload={})

        assert result["success"] is False
        assert "error" in result
        assert "Missing required fields" in result["error"]


class TestCreateDealErrorHandling:
    """Tests for error handling in deal creation."""

    @pytest.mark.asyncio
    async def test_validation_error_response(self, mock_medianet_api: respx.MockRouter):
        """Test handling of 422 validation errors from API."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(422, json=VALIDATION_ERROR_RESPONSE)
        )

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result
        assert "422" in result["error"]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of HTTP 500 errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(
            return_value=httpx.Response(500, json=SERVER_ERROR_RESPONSE)
        )

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_medianet_api: respx.MockRouter):
        """Test handling of network errors."""
        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=httpx.ConnectError("Connection refused"))

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

        assert result["success"] is False
        assert "error" in result


class TestCreateDealCredentialValidation:
    """Tests for credential validation - should fail before HTTP call."""

    @pytest.mark.asyncio
    async def test_missing_credentials_fails_before_http(self):
        """Test that missing credentials fails before making an HTTP call."""
        import medianet_mcp

        medianet_mcp._medianet_client = None

        # Remove credentials from environment
        with (
            patch.dict(
                os.environ,
                {
                    "MEDIANET_SELECT_EMAIL": "",
                    "MEDIANET_SELECT_PASSWORD": "",
                    "MEDIANET_SELECT_TOKEN": "",
                },
                clear=False,
            ),
            respx.mock(assert_all_called=False) as mock,
        ):
            # Configure routes that should NOT be called
            login_route = mock.post(MEDIANET_LOGIN_ENDPOINT).mock(
                return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
            )
            create_route = mock.post(MEDIANET_DEALS_ENDPOINT).mock(
                return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
            )

            result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD)

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
    async def test_complete_deal_creation_with_all_fields(self, mock_medianet_api: respx.MockRouter):
        """Test creating a complete deal with all optional fields."""
        captured_request = None

        def capture_request(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_medianet_api.post(MEDIANET_LOGIN_ENDPOINT).mock(
            return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)
        )
        mock_medianet_api.post(MEDIANET_DEALS_ENDPOINT).mock(side_effect=capture_request)

        result = await mn_create_deal(payload=SAMPLE_DEAL_PAYLOAD_FULL)

        # Verify success
        assert result["success"] is True
        assert "deal" in result

        # Verify payload structure
        payload = json.loads(captured_request.content)

        # All fields should be present (transformed to API v9 format)
        assert payload["deal_id"] == "ELC-MN-2024-FULL"
        assert payload["display_name"] == "Elcano_MediaNet_Complete_Deal"
        assert payload["start_date"] == "2024-03-01T00:00:00Z"
        assert payload["end_date"] == "2024-12-31T23:59:59Z"
        assert payload["ad_format"] == 0
        assert payload["margin"] == 30.0
        assert payload["margin_type"] == 1  # Transformed from "percentage" to 1
        assert payload["bid_floor"] == 2.50  # Renamed from floor_price
        assert len(payload["demand_partners"]) == 2  # Transformed from bidders
        assert payload["environments"] == ["Web", "App"]  # Capitalized
        assert payload["status"] == 1
        assert "is_always_on" not in payload  # Removed by transform
        assert payload["domains"] == ["premium-site.com", "quality-publisher.org"]
        assert payload["geos"] == ["US", "CA", "UK"]
        assert payload["devices"] == ["desktop", "mobile", "tablet"]
