import os
import sys
from collections.abc import Generator
from unittest.mock import patch

import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

TRIPLELIFT_BASE_URL = "https://api.triplelift.net"
TRIPLELIFT_TOKEN_URL = "https://auth.triplelift.net/oauth/token"

TRIPLELIFT_DEAL_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/deal"
TRIPLELIFT_DEALS_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/deals"
TRIPLELIFT_BUYERS_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/buyers"
TRIPLELIFT_COUNTRIES_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/targets/EB_SUPPLY_GEO_COUNTRY_ID"
TRIPLELIFT_SEGMENTS_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/segments"
TRIPLELIFT_AVAILS_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/targeted-avails"
TRIPLELIFT_STATUS_ENDPOINT = f"{TRIPLELIFT_BASE_URL}/curation/v1/12345/deal/status"


@pytest.fixture
def triplelift_credentials() -> Generator[dict[str, str], None, None]:
    test_creds = {
        "TRIPLELIFT_BASE_URL": TRIPLELIFT_BASE_URL,
        "TRIPLELIFT_TOKEN_URL": TRIPLELIFT_TOKEN_URL,
        "TRIPLELIFT_CLIENT_ID": "test-client-id",
        "TRIPLELIFT_CLIENT_SECRET": "test-client-secret",
        "TRIPLELIFT_MEMBER_ID": "12345",
    }
    with patch.dict(os.environ, test_creds, clear=False):
        yield test_creds


@pytest.fixture
def reset_triplelift_client() -> Generator[None, None, None]:
    import triplelift_mcp

    original_client = triplelift_mcp._triplelift_client
    triplelift_mcp._triplelift_client = None
    yield
    triplelift_mcp._triplelift_client = original_client


@pytest.fixture
def mock_triplelift_api(
    triplelift_credentials: dict[str, str], reset_triplelift_client: None
) -> Generator[respx.MockRouter, None, None]:
    with respx.mock(assert_all_called=False) as mock:
        yield mock
