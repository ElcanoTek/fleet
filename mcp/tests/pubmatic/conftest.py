import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


@pytest.fixture(autouse=True)
def _isolate_token_cache() -> Generator[None, None, None]:
    """Disable the on-disk PubMatic token cache for these auth-focused tests.

    Token persistence is covered separately (with tmp-dir isolation) in
    tests/test_pubmatic_token.py. Without this, a test that mints a token would
    persist it to the shared ~/.cache and leak into later tests that build a
    client with the same env access token (their seed hashes match), producing
    order-dependent failures.
    """
    with patch.dict(os.environ, {"PUBMATIC_TOKEN_CACHE_TTL_SECONDS": "0"}, clear=False):
        yield


@pytest.fixture
def pubmatic_env() -> Generator[None, None, None]:
    with patch.dict(
        os.environ,
        {
            "PUBMATIC_BASE_URL": "https://api.pubmatic.com",
            "PUBMATIC_API_PRODUCT": "PUBLISHER",
            "PUBMATIC_USERNAME": "user@example.com",
            "PUBMATIC_PASSWORD": "password-123",
        },
        clear=False,
    ):
        yield


@pytest.fixture
def reset_pubmatic_client() -> Generator[None, None, None]:
    import pubmatic_mcp

    original_client = pubmatic_mcp._pubmatic_client
    pubmatic_mcp._pubmatic_client = None
    yield
    pubmatic_mcp._pubmatic_client = original_client


@pytest.fixture
def mock_pubmatic_api(pubmatic_env: None, reset_pubmatic_client: None) -> Generator[respx.MockRouter, None, None]:
    with respx.mock(assert_all_called=False) as mock:
        yield mock
