"""
Tests for Xandr authentication (login, token caching, token refresh on 401).

Validates:
- Login success with username/password credentials
- Token caching (no redundant logins)
- 401 triggers re-auth exactly once
- Auth status reports correct state
- Tokens/secrets are never printed in outputs
- Missing credentials fail gracefully
"""

import json
import os
import sys
from unittest.mock import patch

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from xandr_mcp import get_xandr_deal, list_xandr_deals, xandr_auth_status

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_DEAL_ENDPOINT
from .fixtures import (
    DEAL_RESPONSE,
    DEALS_LIST_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    UNAUTHORIZED_RESPONSE,
)


class TestLoginSuccess:
    """Tests for successful login flows."""

    @pytest.mark.asyncio
    async def test_login_sends_correct_payload(self, mock_xandr_api: respx.MockRouter):
        """Test login sends correct username/password in auth body."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(side_effect=capture)
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        # Trigger a login by making an API call
        await list_xandr_deals(member_id=9544)

        assert captured_request is not None
        body = json.loads(captured_request.content)
        assert "auth" in body
        assert body["auth"]["username"] == "test-xandr-user"
        assert body["auth"]["password"] == "test-xandr-password-12345"

    @pytest.mark.asyncio
    async def test_login_extracts_token(self, mock_xandr_api: respx.MockRouter):
        """Test that token is extracted from login response and used in API calls."""
        captured_api_request = None

        def capture_api(request: httpx.Request) -> httpx.Response:
            nonlocal captured_api_request
            captured_api_request = request
            return httpx.Response(200, json=DEAL_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=capture_api)

        await get_xandr_deal(deal_id=5001)

        # Verify the token from login is used in the Authorization header
        assert captured_api_request.headers.get("Authorization") == "xandr-auth-token-abc123"


class TestTokenCaching:
    """Tests to verify token caching behavior."""

    @pytest.mark.asyncio
    async def test_cached_token_reused(self, mock_xandr_api: respx.MockRouter):
        """Test that a valid cached token is reused without re-login."""
        login_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(side_effect=count_login)
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        # First call triggers login
        await list_xandr_deals(member_id=9544)
        assert login_count["n"] == 1

        # Second call should reuse cached token
        await list_xandr_deals(member_id=9544)
        assert login_count["n"] == 1


class TestReauthOn401:
    """Tests for 401 re-auth during API calls."""

    @pytest.mark.asyncio
    async def test_401_triggers_reauth_once(self, mock_xandr_api: respx.MockRouter):
        """Test that 401 on API call triggers re-auth exactly once."""
        login_count = {"n": 0}
        api_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        def api_handler(request: httpx.Request) -> httpx.Response:
            api_count["n"] += 1
            if api_count["n"] == 1:
                return httpx.Response(401, json=UNAUTHORIZED_RESPONSE)
            return httpx.Response(200, json=DEALS_LIST_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(side_effect=count_login)
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(side_effect=api_handler)

        result = await list_xandr_deals(member_id=9544)
        assert result["success"] is True
        # Initial login + re-login after 401
        assert login_count["n"] == 2
        assert api_count["n"] == 2

    @pytest.mark.asyncio
    async def test_401_does_not_loop_forever(self, mock_xandr_api: respx.MockRouter):
        """Test that persistent 401 does not cause infinite retries."""
        login_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(side_effect=count_login)
        # Always return 401
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(401, json=UNAUTHORIZED_RESPONSE)
        )

        result = await list_xandr_deals(member_id=9544)
        assert result["success"] is False
        # Should login twice (initial + one retry) and then stop
        assert login_count["n"] == 2


class TestAuthStatus:
    """Tests for xandr_auth_status tool."""

    @pytest.mark.asyncio
    async def test_status_unconfigured(
        self,
        reset_xandr_client: None,  # noqa: ARG002 - fixture needed for client reset
    ):
        """Test auth status when no credentials are set."""
        with patch.dict(
            os.environ,
            {
                "XANDR_USERNAME": "",
                "XANDR_PASSWORD": "",
                "XANDR_SEAT_ID": "",
            },
            clear=False,
        ):
            result = await xandr_auth_status()
            assert result["configured"] is False
            assert result["authenticated"] is False

    @pytest.mark.asyncio
    async def test_status_configured_no_token(self, mock_xandr_api: respx.MockRouter):  # noqa: ARG002 - fixture needed for env setup
        """Test auth status when credentials are set but no login yet."""
        result = await xandr_auth_status()
        assert result["configured"] is True
        assert result["authenticated"] is False

    @pytest.mark.asyncio
    async def test_status_after_login(self, mock_xandr_api: respx.MockRouter):
        """Test auth status after successful login."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        # Trigger login
        await list_xandr_deals(member_id=9544)

        result = await xandr_auth_status()
        assert result["configured"] is True
        assert result["authenticated"] is True


class TestSecurityNoTokenLeaks:
    """Ensure tokens/secrets never appear in tool outputs."""

    @pytest.mark.asyncio
    async def test_auth_status_no_token_values(self, mock_xandr_api: respx.MockRouter):
        """Verify auth status does not contain raw token strings."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
        )

        # Trigger login
        await list_xandr_deals(member_id=9544)

        status = await xandr_auth_status()
        status_json = json.dumps(status)

        # Token from the fixture should not appear in the status output
        assert LOGIN_SUCCESS_RESPONSE["response"]["token"] not in status_json
        assert "test-xandr-password" not in status_json


class TestMissingCredentials:
    """Test behavior when credentials are missing."""

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
            api_route = mock.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
                return_value=httpx.Response(200, json=DEALS_LIST_RESPONSE)
            )

            result = await list_xandr_deals(member_id=9544)

            # Verify it failed
            assert result["success"] is False
            assert "error" in result
            assert "not configured" in result["error"].lower()

            # Verify NO HTTP calls were made
            assert login_route.call_count == 0
            assert api_route.call_count == 0

    @pytest.mark.asyncio
    async def test_login_failed_invalid_credentials(self, mock_xandr_api: respx.MockRouter):
        """Test graceful handling of login failure (e.g., 401 from /auth)."""
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(401, json=UNAUTHORIZED_RESPONSE))

        result = await list_xandr_deals(member_id=9544)

        assert result["success"] is False
        assert "error" in result
