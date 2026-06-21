import json
import os
import sys
from unittest.mock import patch

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from triplelift_mcp import tl_auth_status, tl_list_buyers

from .conftest import TRIPLELIFT_BUYERS_ENDPOINT, TRIPLELIFT_TOKEN_URL
from .fixtures import AUTH_SUCCESS_RESPONSE, BUYERS_RESPONSE


class TestAuthFlow:
    @pytest.mark.asyncio
    async def test_login_sends_client_credentials(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(side_effect=capture)
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(return_value=httpx.Response(200, json=BUYERS_RESPONSE))

        await tl_list_buyers(member_id=12345)

        assert captured_request is not None
        assert captured_request.headers.get("Content-Type", "").startswith("application/json")
        body = json.loads(captured_request.content.decode("utf-8"))
        assert body["grant_type"] == "client_credentials"
        assert body["client_id"] == "test-client-id"
        assert body["client_secret"] == "test-client-secret"

    @pytest.mark.asyncio
    async def test_login_sends_optional_audience_and_organization(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)

        with patch.dict(
            os.environ,
            {
                "TRIPLELIFT_AUDIENCE": "https://federated-api.prod.triplelift.net",
                "TRIPLELIFT_ORGANIZATION": "org_test123",
            },
            clear=False,
        ):
            mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(side_effect=capture)
            mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(
                return_value=httpx.Response(200, json=BUYERS_RESPONSE)
            )

            await tl_list_buyers(member_id=12345)

        assert captured_request is not None
        body = json.loads(captured_request.content.decode("utf-8"))
        assert body["audience"] == "https://federated-api.prod.triplelift.net"
        assert body["organization"] == "org_test123"

    @pytest.mark.asyncio
    async def test_token_used_as_bearer_header(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=BUYERS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(side_effect=capture)

        await tl_list_buyers(member_id=12345)

        assert captured_request is not None
        assert captured_request.headers.get("Authorization") == f"Bearer {AUTH_SUCCESS_RESPONSE['access_token']}"


class TestTokenCaching:
    @pytest.mark.asyncio
    async def test_cached_token_reused(self, mock_triplelift_api: respx.MockRouter):
        token_count = {"n": 0}

        def token_handler(request: httpx.Request) -> httpx.Response:
            token_count["n"] += 1
            return httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(side_effect=token_handler)
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(return_value=httpx.Response(200, json=BUYERS_RESPONSE))

        await tl_list_buyers(member_id=12345)
        await tl_list_buyers(member_id=12345)

        assert token_count["n"] == 1


class TestAuthStatus:
    @pytest.mark.asyncio
    async def test_auth_status_unconfigured(
        self,
        reset_triplelift_client: None,  # noqa: ARG002 - fixture needed to reset singleton
    ):
        with patch.dict(
            os.environ,
            {
                "TRIPLELIFT_CLIENT_ID": "",
                "TRIPLELIFT_CLIENT_SECRET": "",
                "TRIPLELIFT_MEMBER_ID": "",
            },
            clear=False,
        ):
            result = await tl_auth_status()
            assert result["configured"] is False
            assert result["authenticated"] is False

    @pytest.mark.asyncio
    async def test_auth_status_after_login(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(return_value=httpx.Response(200, json=BUYERS_RESPONSE))

        await tl_list_buyers(member_id=12345)
        status = await tl_auth_status()

        assert status["configured"] is True
        assert status["authenticated"] is True

    @pytest.mark.asyncio
    async def test_auth_status_does_not_leak_token(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(return_value=httpx.Response(200, json=BUYERS_RESPONSE))

        await tl_list_buyers(member_id=12345)
        status = await tl_auth_status()
        status_json = json.dumps(status)

        assert AUTH_SUCCESS_RESPONSE["access_token"] not in status_json
