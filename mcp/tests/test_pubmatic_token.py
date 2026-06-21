"""Tests for PubMatic developer-token persistence and refresh-token renewal.

The PubMatic access token is valid ~60 days. These tests lock in the behavior
that keeps cutlass from re-minting (and email-spamming) a token on every run:
  - the token is cached on disk and reused across runs for the same env lineage;
  - on expiry the refresh token is used first (quiet), with username/password
    as the fallback only when refresh is unavailable or fails.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import pubmatic_mcp


@pytest.fixture(autouse=True)
def _isolate_cache(tmp_path, monkeypatch):
    # Route the token cache into a temp dir and reset the client singleton.
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
    monkeypatch.delenv("PUBMATIC_TOKEN_CACHE_TTL_SECONDS", raising=False)
    monkeypatch.setattr(pubmatic_mcp, "_pubmatic_client", None)
    yield


class TestTokenCache:
    def test_roundtrip_and_seed_invalidation(self):
        seed_a = pubmatic_mcp._seed_hash("env-token-A")
        pubmatic_mcp._save_token_cache(seed_a, "access-1", "refresh-1")

        assert pubmatic_mcp._load_token_cache(seed_a) == {
            "access_token": "access-1",
            "refresh_token": "refresh-1",
        }
        # A different env token (operator rotated PUBMATIC_ACCESS_TOKEN) must
        # invalidate the cached lineage.
        assert pubmatic_mcp._load_token_cache(pubmatic_mcp._seed_hash("env-token-B")) is None

    def test_cache_file_is_owner_only(self, tmp_path):
        pubmatic_mcp._save_token_cache(pubmatic_mcp._seed_hash("env-token-A"), "access-1", "refresh-1")
        path = tmp_path / "cutlass" / "pubmatic" / "auth_token.json"
        assert path.is_file()
        assert (path.stat().st_mode & 0o777) == 0o600

    def test_disabled_when_ttl_zero(self, monkeypatch):
        monkeypatch.setenv("PUBMATIC_TOKEN_CACHE_TTL_SECONDS", "0")
        seed = pubmatic_mcp._seed_hash("env-token-A")
        pubmatic_mcp._save_token_cache(seed, "access-1", "refresh-1")
        assert pubmatic_mcp._load_token_cache(seed) is None


class TestClientTokenSelection:
    def test_reuses_cached_token_for_same_seed(self, monkeypatch):
        monkeypatch.setenv("PUBMATIC_ACCESS_TOKEN", "env-A")
        monkeypatch.setenv("PUBMATIC_REFRESH_TOKEN", "env-refresh")
        pubmatic_mcp._save_token_cache(pubmatic_mcp._seed_hash("env-A"), "cached-access", "cached-refresh")

        client = pubmatic_mcp.PubMaticClient()
        assert client._access_token == "cached-access"
        assert client._refresh_token == "cached-refresh"

    def test_ignores_cache_when_env_token_rotated(self, monkeypatch):
        # Cache was seeded from "old-env"; env now carries a fresh token.
        pubmatic_mcp._save_token_cache(pubmatic_mcp._seed_hash("old-env"), "cached-access", "cached-refresh")
        monkeypatch.setenv("PUBMATIC_ACCESS_TOKEN", "fresh-env")
        monkeypatch.setenv("PUBMATIC_REFRESH_TOKEN", "fresh-refresh")

        client = pubmatic_mcp.PubMaticClient()
        assert client._access_token == "fresh-env"
        assert client._refresh_token == "fresh-refresh"


class TestExpiryRecovery:
    @pytest.mark.asyncio
    async def test_renews_via_refresh_token_first(self, monkeypatch):
        monkeypatch.setenv("PUBMATIC_ACCESS_TOKEN", "old-access")
        monkeypatch.setenv("PUBMATIC_REFRESH_TOKEN", "good-refresh")
        monkeypatch.setenv("PUBMATIC_USERNAME", "user")
        monkeypatch.setenv("PUBMATIC_PASSWORD", "pass")
        client = pubmatic_mcp.PubMaticClient()

        with respx.mock(assert_all_called=False) as mock:
            data = mock.post(f"{client.base_url}/v1/inventory/targeting")
            data.side_effect = [
                httpx.Response(200, json=[{"errorCode": "access_token_expired"}]),
                httpx.Response(200, json={"ok": True}),
            ]
            # Refresh is a PUT to /refreshToken (per PubMatic docs).
            refresh = mock.put(f"{client.base_url}{pubmatic_mcp.PUBMATIC_REFRESH_PATH}").mock(
                return_value=httpx.Response(200, json={"accessToken": "new-access", "refreshToken": "new-refresh"})
            )
            login = mock.post(f"{client.base_url}/v1/developer-integrations/developer/token")

            result = await client.create_targeting({"x": 1})

        assert result == {"ok": True}
        assert refresh.called
        assert not login.called  # refresh path used, NOT username/password
        # Request carried the documented body fields.
        body = json.loads(refresh.calls.last.request.content)
        assert body["email"] == "user" and body["refreshToken"] == "good-refresh"
        assert refresh.calls.last.request.headers["authorization"] == "Bearer old-access"
        assert client._access_token == "new-access"
        # New token persisted for reuse by the next run.
        assert pubmatic_mcp._load_token_cache(client._token_seed_hash)["access_token"] == "new-access"

    @pytest.mark.asyncio
    async def test_falls_back_to_password_when_refresh_fails(self, monkeypatch):
        monkeypatch.setenv("PUBMATIC_ACCESS_TOKEN", "old-access")
        monkeypatch.setenv("PUBMATIC_REFRESH_TOKEN", "expired-refresh")
        monkeypatch.setenv("PUBMATIC_USERNAME", "user")
        monkeypatch.setenv("PUBMATIC_PASSWORD", "pass")
        client = pubmatic_mcp.PubMaticClient()

        with respx.mock(assert_all_called=False) as mock:
            data = mock.post(f"{client.base_url}/v1/inventory/targeting")
            data.side_effect = [
                httpx.Response(200, json=[{"errorCode": "access_token_expired"}]),
                httpx.Response(200, json={"ok": True}),
            ]
            refresh = mock.put(f"{client.base_url}{pubmatic_mcp.PUBMATIC_REFRESH_PATH}").mock(
                return_value=httpx.Response(401, json={"fault": "invalid"})
            )
            login = mock.post(f"{client.base_url}/v1/developer-integrations/developer/token").mock(
                return_value=httpx.Response(200, json={"accessToken": "pw-access", "refreshToken": "pw-refresh"})
            )

            result = await client.create_targeting({"x": 1})

        assert result == {"ok": True}
        assert refresh.called  # tried refresh first
        assert login.called  # then fell back to username/password
        assert client._access_token == "pw-access"
