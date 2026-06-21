"""
Pytest configuration and fixtures for Media.net MCP tests.

Provides fixtures for:
- Setting mock Media.net environment variables
- Mocking httpx requests with respx
- Resetting the Media.net client singleton between tests
"""

import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

# Add mcp directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


# Media.net API endpoints for test assertions
MEDIANET_BASE_URL = "https://select.media.net"
MEDIANET_LOGIN_ENDPOINT = f"{MEDIANET_BASE_URL}/api/login"
MEDIANET_DEALS_ENDPOINT = f"{MEDIANET_BASE_URL}/api/v2/deals"


@pytest.fixture
def medianet_credentials() -> Generator[dict[str, str], None, None]:
    """Set mock Media.net credentials environment variables for tests.

    This ensures tests don't require real credentials.
    """
    test_creds = {
        "MEDIANET_SELECT_BASE_URL": MEDIANET_BASE_URL,
        "MEDIANET_SELECT_EMAIL": "test@example.com",
        "MEDIANET_SELECT_PASSWORD": "test-password-12345",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def medianet_token() -> Generator[str, None, None]:
    """Set a mock Media.net token environment variable for tests.

    Use this when you want to skip the login flow.
    """
    test_token = "test-medianet-token-12345"
    env_vars = {
        "MEDIANET_SELECT_BASE_URL": MEDIANET_BASE_URL,
        "MEDIANET_SELECT_TOKEN": test_token,
    }
    with patch.dict(os.environ, env_vars, clear=False):
        yield test_token


@pytest.fixture
def reset_medianet_client() -> Generator[None, None, None]:
    """Reset the Media.net client singleton between tests.

    This ensures each test starts with a fresh client instance.
    """
    import medianet_mcp

    # Store original client
    original_client = medianet_mcp._medianet_client

    # Reset to None before test
    medianet_mcp._medianet_client = None

    yield

    # Restore after test (or reset to None)
    medianet_mcp._medianet_client = original_client


@pytest.fixture
def mock_medianet_api(
    medianet_credentials: dict[str, str], reset_medianet_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Media.net API endpoints.

    Uses respx.mock(assert_all_called=False) to allow flexible mocking.
    Tests should explicitly configure expected requests.

    Any unexpected HTTP request will raise an error, ensuring no real API calls.
    """
    with respx.mock(assert_all_called=False) as mock:
        # By default, block all requests - tests must explicitly mock endpoints
        yield mock


@pytest.fixture
def mock_medianet_api_with_token(
    medianet_token: str, reset_medianet_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Media.net API endpoints with token auth.

    Use this when you want to skip the login flow and use a pre-set token.
    """
    with respx.mock(assert_all_called=False) as mock:
        yield mock


@pytest.fixture
def mock_medianet_api_strict(
    medianet_credentials: dict[str, str], reset_medianet_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Strict mock that asserts all configured routes were called.

    Use this when you want to verify exact API call patterns.
    """
    with respx.mock(assert_all_called=True) as mock:
        yield mock
