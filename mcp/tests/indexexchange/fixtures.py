"""
HTTP response fixtures for Index Exchange MCP tests.

These fixtures provide realistic mock responses for all API operations.
"""

import base64
import json
import time

# =============================================================================
# JWT Helpers for Tests
# =============================================================================


def _make_jwt(exp: float | None = None, extra: dict | None = None) -> str:
    """Create a fake JWT with a given exp claim for testing."""
    header = base64.urlsafe_b64encode(json.dumps({"alg": "HS256"}).encode()).rstrip(b"=").decode()
    payload_data: dict = {}
    if exp is not None:
        payload_data["exp"] = exp
    if extra:
        payload_data.update(extra)
    payload = base64.urlsafe_b64encode(json.dumps(payload_data).encode()).rstrip(b"=").decode()
    sig = base64.urlsafe_b64encode(b"fakesig").rstrip(b"=").decode()
    return f"{header}.{payload}.{sig}"


def make_access_token(expires_in: float = 600) -> str:
    """Create a fake access token expiring in `expires_in` seconds from now."""
    return _make_jwt(exp=time.time() + expires_in)


def make_expired_token() -> str:
    """Create a fake access token that already expired."""
    return _make_jwt(exp=time.time() - 100)


REFRESH_TOKEN = "test-refresh-token-abc123"

# =============================================================================
# Login Fixtures
# =============================================================================

LOGIN_SUCCESS_RESPONSE = {
    "loginResponse": {
        "authResponse": {
            "access_token": make_access_token(600),
            "refresh_token": REFRESH_TOKEN,
            "expires_in": 600,
            "id_token": "",
            "token_type": "string",
        },
        "redir": "Bearer",
    },
}

LOGIN_INVALID_CREDENTIALS = {
    "error": "invalid_credentials",
    "message": "Invalid username or password",
}

# =============================================================================
# Refresh Fixtures
# =============================================================================

REFRESH_SUCCESS_RESPONSE = {
    "access_token": make_access_token(600),
    "refresh_token": "new-refresh-token-xyz789",
}

# =============================================================================
# DSP Fixtures
# =============================================================================

DSPS_RESPONSE = [
    {"dspID": 1, "name": "The Trade Desk", "classID": 1},
    {"dspID": 2, "name": "DV360", "classID": 1},
    {"dspID": 3, "name": "Amazon DSP", "classID": 2},
]

DSP_SEATS_RESPONSE = [
    {"seatID": 101, "seatName": "Seat Alpha", "dspID": 1},
    {"seatID": 102, "seatName": "Seat Beta", "dspID": 1},
]

# =============================================================================
# Create Deal Fixtures
# =============================================================================

CREATE_DEAL_SUCCESS_RESPONSE = {
    "dealID": 9001,
    "name": "Test Marketplace Deal",
    "classID": 4,
    "externalDealID": "IXTEST003",
    "floor": 5.00,
    "auctionType": "first",
    "openMarket": False,
    "startDate": "2024-06-01",
    "endDate": "2024-12-31",
    "account": {"id": 12345},
    "marketplaceConfigurations": {
        "dspID": 1,
        "seatIDs": [],
    },
    "targeting": [],
    "status": "active",
}

# =============================================================================
# Inventory Groups Fixtures
# =============================================================================

INVENTORY_GROUPS_RESPONSE = {
    "inventoryGroups": [
        {"inventoryGroupID": 1, "name": "Group A", "publisherAccountID": 100},
        {"inventoryGroupID": 2, "name": "Group B", "publisherAccountID": 100},
    ],
    "totalCount": 2,
}

# =============================================================================
# Targeting Keys Fixtures
# =============================================================================

TARGETING_KEYS_RESPONSE = [
    {"keyID": 10, "keyName": "geo", "status": "active"},
    {"keyID": 11, "keyName": "device", "status": "active"},
    {"keyID": 12, "keyName": "os", "status": "inactive"},
]

# =============================================================================
# Targeting Values Fixtures
# =============================================================================

TARGETING_VALUES_RESPONSE = [
    {"valueID": 100, "value": "US", "status": "active"},
    {"valueID": 101, "value": "CA", "status": "active"},
    {"valueID": 102, "value": "UK", "status": "inactive"},
]

# =============================================================================
# Accounts Fixtures
# =============================================================================

ACCOUNTS_RESPONSE = [
    {
        "accountID": 100,
        "accountName": "Test Publisher",
        "accountTypeID": 1,
        "legacyMarketplaceID": 500,
    },
    {
        "accountID": 200,
        "accountName": "Test Publisher 2",
        "accountTypeID": 1,
        "legacyMarketplaceID": 501,
    },
]

# =============================================================================
# Reporting Fixtures
# =============================================================================

CREATE_REPORT_SPEC_RESPONSE = {
    "reportSpecID": 11026,
    "reportStatus": "saved",
}

ACTIVE_REPORTS_RESPONSE = [
    {
        "reportSpecID": 11026,
        "reportTitle": "Daily Revenue",
        "reportStatus": "saved",
        "accounts": [100],
    },
    {
        "reportSpecID": 11027,
        "reportTitle": "Weekly Impressions",
        "reportStatus": "saved",
        "accounts": [100],
    },
]

REPORT_RUN_RESPONSE = {
    "reportRunID": 55001,
    "reportID": 11026,
    "status": "queued",
}

REPORT_FILES_RESPONSE = [
    {
        "fileID": "file-abc-123",
        "reportID": 11026,
        "status": "completed",
        "fileName": "daily_revenue_2024-06-01.csv",
    },
    {
        "fileID": "file-def-456",
        "reportID": 11027,
        "status": "completed",
        "fileName": "weekly_impressions_2024-06-01.csv.gz",
    },
]

# =============================================================================
# Error Fixtures
# =============================================================================

UNAUTHORIZED_RESPONSE = {
    "error": "unauthorized",
    "message": "Invalid or expired token",
}

SERVER_ERROR_RESPONSE = {
    "error": "internal_server_error",
    "message": "An unexpected error occurred",
}

VALIDATION_ERROR_RESPONSE = {
    "error": "validation_error",
    "message": "Request validation failed",
    "details": [
        {"field": "name", "message": "Name is required"},
        {"field": "rate", "message": "Rate must be a valid decimal"},
    ],
}

# =============================================================================
# Sample Payloads for Tests
# =============================================================================

SAMPLE_DEAL_PAYLOAD = {
    "name": "Test Marketplace Deal",
    "external_deal_id": "IXTEST003",
    "dsp_id": 1,
    "legacy_account_id": 12345,
    "floor": 5.00,
    "start_date": "2024-06-01",
    "end_date": "2024-12-31",
}

SAMPLE_DEAL_PAYLOAD_FULL = {
    **SAMPLE_DEAL_PAYLOAD,
    "seat_ids": ["101", "102"],
    "auction_type": "fixed",
    "margin": 5.5,
    "margin_calculation_type": "P",
    "targeting": {
        "includedValueIDs": [250, 550],
        "publisherIDs": [123, 456],
    },
}

SAMPLE_REPORT_SPEC = {
    "report_title": "Daily Revenue Report",
    "accounts": [100],
    "fields": ["date", "impressions", "revenue"],
    "date_range": {"from": "2024-06-01", "to": "2024-06-30"},
}
