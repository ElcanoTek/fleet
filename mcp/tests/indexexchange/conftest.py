"""
Pytest configuration and fixtures for Index Exchange MCP tests.

Provides fixtures for:
- Setting mock Index Exchange environment variables
- Mocking httpx requests with respx
- Resetting the Index Exchange client singleton between tests
"""

import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

# Add mcp directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


# Index Exchange API endpoints for test assertions
IX_BASE_URL = "https://app.indexexchange.com"
IX_LOGIN_ENDPOINT = f"{IX_BASE_URL}/api/authentication/v1/login"
IX_REFRESH_ENDPOINT = f"{IX_BASE_URL}/api/authentication/v1/refresh"
IX_DSPS_ENDPOINT = f"{IX_BASE_URL}/api/deals/v1/dsps"
IX_ACCOUNTS_ENDPOINT = f"{IX_BASE_URL}/api/accounts/v2/accounts/"
IX_REPORT_SPECS_ENDPOINT = f"{IX_BASE_URL}/api/reporting/agg/v1/report-specs"
IX_REPORT_SPEC_UPDATE_ENDPOINT = f"{IX_BASE_URL}/api/reporting/agg/v1/report-specs/11026"
IX_REPORT_SPECS_INFO_ENDPOINT = f"{IX_BASE_URL}/api/reporting/agg/v1/report-specs/info"
IX_REPORT_RUNS_ENDPOINT = f"{IX_BASE_URL}/api/reporting/agg/v1/report-runs"
IX_REPORT_FILES_LIST_ENDPOINT = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/list"


@pytest.fixture
def ix_user_credentials() -> Generator[dict[str, str], None, None]:
    """Set mock Index Exchange user account credentials for tests."""
    test_creds = {
        "INDEXEXCHANGE_BASE_URL": IX_BASE_URL,
        "INDEXEXCHANGE_USERNAME": "testuser@example.com",
        "INDEXEXCHANGE_PASSWORD": "test-password-secret",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def ix_service_credentials() -> Generator[dict[str, str], None, None]:
    """Set mock Index Exchange service account credentials for tests."""
    test_creds = {
        "INDEXEXCHANGE_BASE_URL": IX_BASE_URL,
        "INDEXEXCHANGE_SERVICE_ID": "test-service-id",
        "INDEXEXCHANGE_SERVICE_SECRET": "test-service-secret",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def reset_ix_client() -> Generator[None, None, None]:
    """Reset the Index Exchange client singleton between tests."""
    import indexexchange_mcp

    original_client = indexexchange_mcp._ix_client
    indexexchange_mcp._ix_client = None

    yield

    indexexchange_mcp._ix_client = original_client


@pytest.fixture
def mock_ix_api(ix_user_credentials: dict[str, str], reset_ix_client: None) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Index Exchange API endpoints (user auth).

    Any unexpected HTTP request will raise an error, ensuring no real API calls.
    """
    with respx.mock(assert_all_called=False) as mock:
        yield mock


@pytest.fixture
def mock_ix_api_service(
    ix_service_credentials: dict[str, str], reset_ix_client: None
) -> Generator[respx.MockRouter, None, None]:
    """Mock all HTTP requests to Index Exchange API endpoints (service auth)."""
    with respx.mock(assert_all_called=False) as mock:
        yield mock
