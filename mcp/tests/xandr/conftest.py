"""
Pytest configuration and fixtures for Xandr MCP tests.

Provides fixtures for:
- Setting mock Xandr environment variables
- Mocking httpx requests with respx
- Resetting the Xandr client singleton between tests
"""

import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

# Add mcp directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


# Xandr API endpoints for test assertions
XANDR_BASE_URL = "https://api.appnexus.com"
XANDR_AUTH_ENDPOINT = f"{XANDR_BASE_URL}/auth"
XANDR_DEAL_ENDPOINT = f"{XANDR_BASE_URL}/deal"


@pytest.fixture
def xandr_credentials() -> Generator[dict[str, str], None, None]:
    """Set mock Xandr credentials environment variables for tests.

    This ensures tests don't require real credentials.
    """
    test_creds = {
        "XANDR_BASE_URL": XANDR_BASE_URL,
        "XANDR_USERNAME": "test-xandr-user",
        "XANDR_PASSWORD": "test-xandr-password-12345",
        "XANDR_SEAT_ID": "99001",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def reset_xandr_client() -> Generator[None, None, None]:
    """Reset the Xandr client singleton between tests.

    This ensures each test starts with a fresh client instance.
    """
    import xandr_mcp

    # Store original client
    original_client = xandr_mcp._xandr_client

    # Reset to None before test
    xandr_mcp._xandr_client = None

    yield

    # Restore after test (or reset to None)
    xandr_mcp._xandr_client = original_client


@pytest.fixture
def mock_xandr_api(
    xandr_credentials: dict[str, str], reset_xandr_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Xandr API endpoints.

    Uses respx.mock(assert_all_called=False) to allow flexible mocking.
    Tests should explicitly configure expected requests.

    Any unexpected HTTP request will raise an error, ensuring no real API calls.
    """
    with respx.mock(assert_all_called=False) as mock:
        yield mock


@pytest.fixture
def mock_xandr_api_strict(
    xandr_credentials: dict[str, str], reset_xandr_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Strict mock that asserts all configured routes were called.

    Use this when you want to verify exact API call patterns.
    """
    with respx.mock(assert_all_called=True) as mock:
        yield mock
