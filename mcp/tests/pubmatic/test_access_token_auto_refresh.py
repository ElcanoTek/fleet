"""Regression test for the PubMatic access-token auto-refresh path.

Before this fix the PubMatic client read PUBMATIC_ACCESS_TOKEN from env
at startup and never re-authenticated — once the env-provided token
expired on PubMatic's side every request failed with
``Access Token expired`` until an operator hand-rotated the env var.
The trader's first 9-deal run on 2026-05-20 lost all three PubMatic
deals to exactly this.

The fix wires `_request` to detect PubMatic's two expired-token
signals (HTTP 401 OR a JSON ``errorCode=access_token_expired`` body),
discard the stale token, exchange the configured
``PUBMATIC_USERNAME`` / ``PUBMATIC_PASSWORD`` for a fresh one via the
``/v1/developer-integrations/developer/token`` endpoint, and retry the
original request once. The refresh path requires the username/password
to be configured; without them the original 401 is surfaced verbatim
so the operator still gets a clear signal.
"""

import os
from collections.abc import Generator
from unittest.mock import patch

import httpx
import pubmatic_mcp
import pytest
import respx


@pytest.fixture
def pubmatic_env_with_stale_token() -> Generator[None, None, None]:
    """Seed env with a stale access token + valid username/password.

    The token is pre-set so the client starts with `_access_token`
    populated (this is the production failure mode — the env-loaded
    token sits there expired). Username + password are also set so the
    auto-refresh path is reachable.
    """
    with patch.dict(
        os.environ,
        {
            "PUBMATIC_BASE_URL": "https://api.pubmatic.com",
            "PUBMATIC_API_PRODUCT": "PUBLISHER",
            "PUBMATIC_USERNAME": "user@example.com",
            "PUBMATIC_PASSWORD": "password-123",
            "PUBMATIC_ACCESS_TOKEN": "stale-token-from-env",
        },
        clear=False,
    ):
        yield


@pytest.fixture
def reset_client() -> Generator[None, None, None]:
    original_client = pubmatic_mcp._pubmatic_client
    pubmatic_mcp._pubmatic_client = None
    yield
    pubmatic_mcp._pubmatic_client = original_client


@pytest.fixture
def mock_api(
    pubmatic_env_with_stale_token: None,
    reset_client: None,
) -> Generator[respx.MockRouter, None, None]:
    with respx.mock(assert_all_called=False) as mock:
        yield mock


# ---------------------------------------------------------------------------
# _is_token_expired_response detection
# ---------------------------------------------------------------------------


class TestIsTokenExpiredResponse:
    """The detector must catch both expired-token shapes PubMatic emits."""

    def _client(self) -> pubmatic_mcp.PubMaticClient:
        return pubmatic_mcp.PubMaticClient()

    def test_http_401_is_expired(self):
        response = httpx.Response(401, json={})
        assert self._client()._is_token_expired_response(response) is True

    def test_top_level_error_code(self):
        response = httpx.Response(
            200, json={"errorCode": "access_token_expired", "errorMessage": "Access Token expired"}
        )
        assert self._client()._is_token_expired_response(response) is True

    def test_nested_under_errors_array(self):
        """The curated-deal API surfaces the error nested under `errors[]`."""
        response = httpx.Response(
            200,
            json={
                "success": False,
                "errors": [{"errorCode": "access_token_expired", "errorMessage": "Access Token expired"}],
            },
        )
        assert self._client()._is_token_expired_response(response) is True

    def test_error_message_only_matches_case_insensitive(self):
        response = httpx.Response(400, json={"message": "Access Token Expired."})
        assert self._client()._is_token_expired_response(response) is True

    def test_unrelated_400_is_not_expired(self):
        response = httpx.Response(400, json={"errorCode": "VALIDATION", "errorMessage": "bad input"})
        assert self._client()._is_token_expired_response(response) is False

    def test_200_with_no_error_is_not_expired(self):
        response = httpx.Response(200, json={"data": "ok"})
        assert self._client()._is_token_expired_response(response) is False

    def test_non_json_body_is_not_expired(self):
        response = httpx.Response(500, content=b"<html>server error</html>")
        assert self._client()._is_token_expired_response(response) is False


# ---------------------------------------------------------------------------
# Auto-refresh end-to-end
# ---------------------------------------------------------------------------


class TestAccessTokenAutoRefresh:
    @pytest.mark.asyncio
    async def test_expired_token_triggers_username_password_refresh_and_retry(self, mock_api: respx.MockRouter):
        """Headline regression: a stale env token + 401 response must
        prompt a username/password exchange and a successful retry, all
        transparent to the caller."""
        # First request → 401 (stale token rejected).
        # Token endpoint → 200 with a fresh token.
        # Retried request → 200 with the actual data.
        token_route = mock_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "fresh-token-456",
                "refreshToken": "refresh-456",
            },
        )

        # respx serves routes in order — first call gets the 401, second
        # call gets the 200.
        first_call_made = {"count": 0}

        def _data_endpoint(request: httpx.Request) -> httpx.Response:
            first_call_made["count"] += 1
            if first_call_made["count"] == 1:
                # Stale token rejection.
                return httpx.Response(401, json={"errorCode": "access_token_expired"})
            return httpx.Response(200, json={"deals": [{"id": 12345}]})

        mock_api.get("https://api.pubmatic.com/curateddeals/12345").mock(side_effect=_data_endpoint)

        client = pubmatic_mcp.PubMaticClient()
        # Sanity: the client started with the stale env token.
        assert client._access_token == "stale-token-from-env"

        result = await client._request("GET", "/curateddeals/12345")

        # The retry used the fresh token from username/password exchange.
        assert client._access_token == "fresh-token-456"
        assert client._refresh_token == "refresh-456"
        assert result == {"deals": [{"id": 12345}]}
        # Token endpoint was hit exactly once during the auto-refresh.
        assert token_route.call_count == 1
        # The data endpoint was hit twice (initial 401 + retry).
        assert first_call_made["count"] == 2

    @pytest.mark.asyncio
    async def test_inline_error_body_triggers_refresh(self, mock_api: respx.MockRouter):
        """When PubMatic returns HTTP 200 with the expired-token error
        nested in the JSON body (curated-deal / reporting style), the
        client must still detect and refresh."""
        mock_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "fresh-token-789",
                "refreshToken": "refresh-789",
            },
        )

        responses = [
            httpx.Response(
                200,
                json={
                    "success": False,
                    "errors": [{"errorCode": "access_token_expired", "errorMessage": "Access Token expired"}],
                },
            ),
            httpx.Response(200, json={"success": True, "deal_id": 999}),
        ]
        call_count = {"n": 0}

        def _serve(request: httpx.Request) -> httpx.Response:
            response = responses[call_count["n"]]
            call_count["n"] += 1
            return response

        mock_api.post("https://api.pubmatic.com/curateddeals/create").mock(side_effect=_serve)

        client = pubmatic_mcp.PubMaticClient()
        result = await client._request("POST", "/curateddeals/create", json_data={"name": "X"})

        assert client._access_token == "fresh-token-789"
        assert result == {"success": True, "deal_id": 999}
        assert call_count["n"] == 2

    @pytest.mark.asyncio
    async def test_no_username_password_falls_through_to_raise(
        self,
        reset_client: None,  # noqa: ARG002
    ):
        """When the operator only configured PUBMATIC_ACCESS_TOKEN with
        no username/password, an expired token cannot be auto-refreshed
        and the 401 must surface so the operator regenerates manually."""
        with (
            patch.dict(
                os.environ,
                {
                    "PUBMATIC_BASE_URL": "https://api.pubmatic.com",
                    "PUBMATIC_API_PRODUCT": "PUBLISHER",
                    "PUBMATIC_ACCESS_TOKEN": "stale-only",
                    "PUBMATIC_USERNAME": "",
                    "PUBMATIC_PASSWORD": "",
                },
                clear=False,
            ),
            respx.mock(assert_all_called=False) as mock_api,
        ):
            mock_api.get("https://api.pubmatic.com/curateddeals/1").respond(
                401, json={"errorCode": "access_token_expired"}
            )

            client = pubmatic_mcp.PubMaticClient()
            with pytest.raises(httpx.HTTPStatusError) as excinfo:
                await client._request("GET", "/curateddeals/1")
            assert excinfo.value.response.status_code == 401


# ---------------------------------------------------------------------------
# authenticate(force=True) behaviour
# ---------------------------------------------------------------------------


class TestAuthenticateForce:
    @pytest.mark.asyncio
    async def test_force_discards_existing_token_and_re_exchanges(self, mock_api: respx.MockRouter):
        mock_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "fresh-via-force",
                "refreshToken": "refresh-force",
            },
        )

        client = pubmatic_mcp.PubMaticClient()
        assert client._access_token == "stale-token-from-env"

        await client.authenticate(force=True)

        assert client._access_token == "fresh-via-force"
        assert client._refresh_token == "refresh-force"

    @pytest.mark.asyncio
    async def test_no_force_with_existing_token_skips_exchange(self, mock_api: respx.MockRouter):
        # Token endpoint registered but should NOT be hit.
        token_route = mock_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200, json={"accessToken": "should-not-be-used"}
        )

        client = pubmatic_mcp.PubMaticClient()
        await client.authenticate()  # default force=False

        # Env token preserved; token endpoint not called.
        assert client._access_token == "stale-token-from-env"
        assert token_route.call_count == 0
