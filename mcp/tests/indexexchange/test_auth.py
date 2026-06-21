"""
Tests for Index Exchange authentication (login, refresh, token caching).

Validates:
- Login success with user and service account credentials
- Refresh flow
- Token caching (no redundant logins)
- 401 triggers re-auth exactly once
- Tokens/secrets are never printed
"""

import json
import os
import sys
import time
from unittest.mock import patch

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    _decode_jwt_exp,
    ix_auth_login,
    ix_auth_status,
    ix_list_dsps,
)

from .conftest import IX_DSPS_ENDPOINT, IX_LOGIN_ENDPOINT, IX_REFRESH_ENDPOINT
from .fixtures import (
    DSPS_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    REFRESH_SUCCESS_RESPONSE,
    UNAUTHORIZED_RESPONSE,
    make_access_token,
    make_expired_token,
)


class TestJWTDecoding:
    """Tests for JWT exp claim parsing."""

    def test_decode_valid_jwt(self):
        token = make_access_token(600)
        exp = _decode_jwt_exp(token)
        assert exp is not None
        assert exp > time.time()

    def test_decode_expired_jwt(self):
        token = make_expired_token()
        exp = _decode_jwt_exp(token)
        assert exp is not None
        assert exp < time.time()

    def test_decode_invalid_jwt_returns_none(self):
        assert _decode_jwt_exp("not-a-jwt") is None
        assert _decode_jwt_exp("only.two") is None
        assert _decode_jwt_exp("") is None

    def test_decode_jwt_without_exp(self):
        import base64

        header = base64.urlsafe_b64encode(b'{"alg":"HS256"}').rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(b'{"sub":"user"}').rstrip(b"=").decode()
        sig = base64.urlsafe_b64encode(b"sig").rstrip(b"=").decode()
        token = f"{header}.{payload}.{sig}"
        assert _decode_jwt_exp(token) is None


class TestLoginSuccess:
    """Tests for successful login flows."""

    @pytest.mark.asyncio
    async def test_user_login_success(self, mock_ix_api: respx.MockRouter):
        """Test login with user credentials."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=capture)

        result = await ix_auth_login()
        assert result["success"] is True
        assert result["auth_mode"] == "user_account"
        assert result["token_expiry"] > 0

        # Verify correct body sent
        body = json.loads(captured_request.content)
        assert body["username"] == "testuser@example.com"
        assert body["password"] == "test-password-secret"

    @pytest.mark.asyncio
    async def test_service_login_success(self, mock_ix_api_service: respx.MockRouter):
        """Test login with service account credentials."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_ix_api_service.post(IX_LOGIN_ENDPOINT).mock(side_effect=capture)

        result = await ix_auth_login()
        assert result["success"] is True
        assert result["auth_mode"] == "service_account"

        body = json.loads(captured_request.content)
        assert body["username"] == "test-service-id"
        assert body["password"] == "test-service-secret"


class TestTokenCaching:
    """Tests to verify token caching behavior."""

    @pytest.mark.asyncio
    async def test_cached_token_reused(self, mock_ix_api: respx.MockRouter):
        """Test that a valid cached token is reused without re-login."""
        login_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=count_login)
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(return_value=httpx.Response(200, json=DSPS_RESPONSE))

        # First call triggers login
        await ix_list_dsps()
        assert login_count["n"] == 1

        # Second call should reuse cached token
        await ix_list_dsps()
        assert login_count["n"] == 1


class TestTokenRefresh:
    """Tests for token refresh behavior."""

    @pytest.mark.asyncio
    async def test_refresh_on_expiry(self, mock_ix_api: respx.MockRouter):
        """Test that expired cached token triggers refresh on next call."""
        login_count = {"n": 0}
        refresh_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            # Return a token that's already expired (within buffer)
            resp = {
                "access_token": make_expired_token(),
                "refresh_token": "test-refresh-token",
            }
            return httpx.Response(200, json=resp)

        def count_refresh(request: httpx.Request) -> httpx.Response:
            refresh_count["n"] += 1
            return httpx.Response(200, json=REFRESH_SUCCESS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=count_login)
        mock_ix_api.post(IX_REFRESH_ENDPOINT).mock(side_effect=count_refresh)
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(return_value=httpx.Response(200, json=DSPS_RESPONSE))

        # First call: login (gets expired token), API call succeeds
        await ix_list_dsps()
        assert login_count["n"] == 1
        assert refresh_count["n"] == 0

        # Second call: token is expired in cache -> tries refresh (has refresh_token)
        await ix_list_dsps()
        assert login_count["n"] == 1  # No new login
        assert refresh_count["n"] == 1  # Refresh was called

    @pytest.mark.asyncio
    async def test_refresh_failure_triggers_relogin(self, mock_ix_api: respx.MockRouter):
        """Test that refresh failure falls back to full login."""
        login_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            if login_count["n"] == 1:
                # First login returns expired token
                resp = {
                    "access_token": make_expired_token(),
                    "refresh_token": "test-refresh-token",
                }
            else:
                # Second login returns valid token
                resp = LOGIN_SUCCESS_RESPONSE
            return httpx.Response(200, json=resp)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=count_login)
        # Refresh returns 401
        mock_ix_api.post(IX_REFRESH_ENDPOINT).mock(return_value=httpx.Response(401, json=UNAUTHORIZED_RESPONSE))
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(return_value=httpx.Response(200, json=DSPS_RESPONSE))

        # First call: login -> gets expired token + refresh_token -> API call succeeds
        await ix_list_dsps()
        assert login_count["n"] == 1

        # Second call: token expired -> refresh fails (401) -> falls back to login
        await ix_list_dsps()
        assert login_count["n"] == 2


class TestReauthOn401:
    """Tests for 401 re-auth during API calls."""

    @pytest.mark.asyncio
    async def test_401_triggers_reauth_once(self, mock_ix_api: respx.MockRouter):
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
            return httpx.Response(200, json=DSPS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=count_login)
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(side_effect=api_handler)

        result = await ix_list_dsps()
        assert result["success"] is True
        # Initial login + re-login after 401
        assert login_count["n"] == 2
        assert api_count["n"] == 2


class TestAuthStatus:
    """Tests for ix_auth_status tool."""

    @pytest.mark.asyncio
    async def test_status_unconfigured(
        self,
        reset_ix_client: None,  # noqa: ARG002 - fixture needed for client reset
    ):
        """Test auth status when no credentials are set."""
        with patch.dict(
            os.environ,
            {
                "INDEXEXCHANGE_USERNAME": "",
                "INDEXEXCHANGE_PASSWORD": "",
                "INDEXEXCHANGE_SERVICE_ID": "",
                "INDEXEXCHANGE_SERVICE_SECRET": "",
            },
            clear=False,
        ):
            result = await ix_auth_status()
            assert result["configured"] is False
            assert result["auth_mode"] is None
            assert result["token_cached"] is False

    @pytest.mark.asyncio
    async def test_status_after_login(self, mock_ix_api: respx.MockRouter):
        """Test auth status after successful login."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        await ix_auth_login()
        result = await ix_auth_status()

        assert result["configured"] is True
        assert result["auth_mode"] == "user_account"
        assert result["token_cached"] is True
        assert result["token_expiry"] is not None
        # Verify no raw tokens in response
        status_str = json.dumps(result)
        assert "access_token" not in status_str.lower()
        assert "refresh_token" not in status_str.lower()


class TestForceLogin:
    """Tests for force login."""

    @pytest.mark.asyncio
    async def test_force_login_clears_cache(self, mock_ix_api: respx.MockRouter):
        """Test that force=True clears cached token and re-authenticates."""
        login_count = {"n": 0}

        def count_login(request: httpx.Request) -> httpx.Response:
            login_count["n"] += 1
            return httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(side_effect=count_login)

        await ix_auth_login()
        assert login_count["n"] == 1

        await ix_auth_login(force=True)
        assert login_count["n"] == 2


class TestMissingCredentials:
    """Test behavior when credentials are missing."""

    @pytest.mark.asyncio
    async def test_login_fails_without_credentials(
        self,
        reset_ix_client: None,  # noqa: ARG002 - fixture needed for client reset
    ):
        """Test that login fails gracefully with no credentials."""
        with patch.dict(
            os.environ,
            {
                "INDEXEXCHANGE_USERNAME": "",
                "INDEXEXCHANGE_PASSWORD": "",
                "INDEXEXCHANGE_SERVICE_ID": "",
                "INDEXEXCHANGE_SERVICE_SECRET": "",
            },
            clear=False,
        ):
            result = await ix_auth_login()
            assert result["success"] is False
            assert "error" in result


class TestSecurityNoTokenLeaks:
    """Ensure tokens/secrets never appear in tool outputs."""

    @pytest.mark.asyncio
    async def test_auth_status_no_token_values(self, mock_ix_api: respx.MockRouter):
        """Verify auth status does not contain raw token strings."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        await ix_auth_login()
        status = await ix_auth_status()
        status_json = json.dumps(status)
        # Token from the fixture should not appear
        auth_resp = LOGIN_SUCCESS_RESPONSE["loginResponse"]["authResponse"]
        assert auth_resp["access_token"] not in status_json
        assert auth_resp["refresh_token"] not in status_json
