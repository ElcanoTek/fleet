"""
Pytest configuration and fixtures for Magnite MCP tests.

The MCP covers two Magnite surfaces behind one set of Basic-auth
credentials: DV+ Performance Analytics reporting (api.rubiconproject.com)
and the ClearLine Curation Demand Management deal API
(dmg.rubiconproject.com, added with API guide v2.0 in June 2026).
"""

import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


# DV+ Performance Analytics API endpoints
MAGNITE_DV_BASE_URL = "https://api.rubiconproject.com"
TEST_ACCOUNT_ID = "102"
MAGNITE_DV_ANALYTICS_ENDPOINT = f"{MAGNITE_DV_BASE_URL}/analytics/v2/default"

# ClearLine Curation Demand Management API endpoints
MAGNITE_DMG_BASE_URL = "https://dmg.rubiconproject.com"
MAGNITE_DMG_DEALS_ENDPOINT = f"{MAGNITE_DMG_BASE_URL}/api/v1/deals"


@pytest.fixture
def reset_magnite_client() -> Generator[None, None, None]:
    """Reset the Magnite client singleton (and prepared-deal artifacts)
    between tests so each test starts fresh, picking up the current env."""
    import magnite_mcp

    original_client = magnite_mcp._magnite_client
    original_prepared = dict(magnite_mcp._prepared_magnite_deals)
    magnite_mcp._magnite_client = None
    magnite_mcp._prepared_magnite_deals.clear()
    yield
    magnite_mcp._magnite_client = original_client
    magnite_mcp._prepared_magnite_deals.clear()
    magnite_mcp._prepared_magnite_deals.update(original_prepared)


@pytest.fixture
def magnite_dv_credentials() -> Generator[dict[str, str], None, None]:
    """Set mock Magnite credentials (shared by reporting and deal tests)."""
    test_creds = {
        "MAGNITE_ACCESS_KEY": "test-access-key-12345",
        "MAGNITE_SECRET_KEY": "test-secret-key-67890",
        "MAGNITE_ACCOUNT_ID": TEST_ACCOUNT_ID,
        "MAGNITE_DV_BASE_URL": MAGNITE_DV_BASE_URL,
        "MAGNITE_DMG_BASE_URL": MAGNITE_DMG_BASE_URL,
        "MAGNITE_DOWNLOAD_DIR": "",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def mock_magnite_dv_api(
    magnite_dv_credentials: dict[str, str],  # noqa: ARG001
    reset_magnite_client: None,  # noqa: ARG001
) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Magnite API endpoints (DV+ and DMG)."""
    with respx.mock(assert_all_called=False) as mock:
        yield mock


# The Demand Management surface shares credentials and the respx router —
# alias the fixture so deal tests read naturally.
@pytest.fixture
def mock_magnite_dmg_api(mock_magnite_dv_api: respx.MockRouter) -> respx.MockRouter:
    return mock_magnite_dv_api
