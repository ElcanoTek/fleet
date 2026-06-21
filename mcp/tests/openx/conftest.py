"""
Pytest configuration and fixtures for OpenX MCP tests.

Provides fixtures for:
- Setting mock OPENX_API_KEY environment variable
- Mocking httpx requests with respx
- Resetting the OpenX client singleton between tests
"""

import os
import sys
from collections.abc import AsyncGenerator, Generator
from unittest.mock import patch

import pytest
import respx

# Add mcp directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


@pytest.fixture
def openx_api_key() -> Generator[str, None, None]:
    """Set a mock OPENX_API_KEY environment variable for tests.

    This ensures tests don't require a real API key.
    """
    test_key = "test-openx-api-key-12345"
    with patch.dict(os.environ, {"OPENX_API_KEY": test_key}):
        yield test_key


@pytest.fixture
async def reset_openx_client() -> AsyncGenerator[None, None]:
    """Reset the OpenX client singleton between tests.

    This ensures each test starts with a fresh client instance.
    """
    import openx_mcp

    # Store original client
    original_client = openx_mcp._openx_client

    # Reset to None before test
    openx_mcp._openx_client = None
    openx_mcp._prepared_openx_deals = {}

    yield

    # Close the temporary client if it was created
    if openx_mcp._openx_client:
        await openx_mcp._openx_client.close()

    # Restore after test (or reset to None)
    openx_mcp._openx_client = original_client
    openx_mcp._prepared_openx_deals = {}


@pytest.fixture
def mock_openx_graphql(openx_api_key: str, reset_openx_client: None) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to the OpenX GraphQL endpoint.

    Uses respx.mock(assert_all_called=False) to allow flexible mocking.
    Tests should explicitly configure expected requests.

    Any unexpected HTTP request will raise an error, ensuring no real API calls.
    """
    with respx.mock(assert_all_called=False) as mock:
        # By default, block all requests - tests must explicitly mock endpoints
        yield mock


@pytest.fixture
def mock_openx_graphql_strict(openx_api_key: str, reset_openx_client: None) -> Generator[respx.MockRouter, None, None]:
    """Strict mock that asserts all configured routes were called.

    Use this when you want to verify exact API call patterns.
    """
    with respx.mock(assert_all_called=True) as mock:
        yield mock


# OpenX GraphQL endpoint constant for test assertions
OPENX_GRAPHQL_ENDPOINT = "https://api.openx.com/oa/graphql"
