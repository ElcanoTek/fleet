"""
HTTP response fixtures for Media.net MCP tests.

These fixtures provide realistic mock responses for all API operations.
"""

# =============================================================================
# Login Fixtures
# =============================================================================

LOGIN_SUCCESS_RESPONSE = {
    "status": "success",
    "data": {
        "token": "medianet-auth-token-12345",
        "user": {
            "id": "user-001",
            "email": "test@example.com",
            "name": "Test User",
        },
    },
}

LOGIN_INVALID_CREDENTIALS_RESPONSE = {
    "status": "error",
    "message": "Invalid email or password",
}

# =============================================================================
# Demand Partners Fixtures
# =============================================================================

BIDDERS_RESPONSE = {
    "status": "success",
    "data": [
        {
            "id": 1,
            "name": "AppNexus",
        },
        {
            "id": 2,
            "name": "Rubicon",
        },
        {
            "id": 3,
            "name": "OpenX",
        },
        {
            "id": 4,
            "name": "Index Exchange",
        },
    ],
}

BIDDERS_EMPTY_RESPONSE = {
    "status": "success",
    "data": [],
}

# =============================================================================
# Deals List Fixtures
# =============================================================================

DEALS_LIST_RESPONSE_PAGE1 = {
    "status": "success",
    "data": [
        {
            "id": 1001,
            "deal_id": "ELC-MN-2024-001",
            "display_name": "Elcano_MediaNet_Premium_Banner_US",
            "status": 1,
            "ad_format": 0,
            "margin": 25.0,
            "margin_type": "percentage",
            "start_date": "2024-01-15T00:00:00Z",
            "end_date": None,
            "bid_floor": 2.50,
            "demand_partners": ["1"],
            "environments": ["Web"],
            "created_at": "2024-01-10T12:00:00Z",
            "updated_at": "2024-01-12T15:30:00Z",
        },
        {
            "id": 1002,
            "deal_id": "ELC-MN-2024-002",
            "display_name": "Elcano_MediaNet_Video_Premium",
            "status": 1,
            "ad_format": 1,
            "margin": 30.0,
            "margin_type": "percentage",
            "start_date": "2024-01-20T00:00:00Z",
            "end_date": "2024-06-30T23:59:59Z",
            "bid_floor": 5.00,
            "demand_partners": ["2", "3"],
            "environments": ["Web", "App"],
            "created_at": "2024-01-18T09:00:00Z",
            "updated_at": "2024-01-18T09:00:00Z",
        },
    ],
}

DEALS_LIST_RESPONSE_PAGE2 = {
    "status": "success",
    "data": [
        {
            "id": 1003,
            "deal_id": "ELC-MN-2024-003",
            "display_name": "Elcano_MediaNet_Native_UK",
            "status": -1,
            "ad_format": 2,
            "margin": 20.0,
            "margin_type": "percentage",
            "start_date": "2024-02-01T00:00:00Z",
            "end_date": None,
            "bid_floor": 1.50,
            "demand_partners": ["4"],
            "environments": ["Web"],
            "created_at": "2024-01-28T10:00:00Z",
            "updated_at": "2024-02-15T14:00:00Z",
        },
    ],
}

DEALS_LIST_EMPTY_RESPONSE = {
    "status": "success",
    "data": [],
}

# =============================================================================
# Get Deal Fixtures (uses list endpoint with filter)
# =============================================================================

DEAL_RESPONSE = {
    "status": "success",
    "data": [
        {
            "id": 1001,
            "deal_id": "ELC-MN-2024-001",
            "display_name": "Elcano_MediaNet_Premium_Banner_US",
            "status": 1,
            "ad_format": 0,
            "margin": 25.0,
            "margin_type": "percentage",
            "start_date": "2024-01-15T00:00:00Z",
            "end_date": None,
            "bid_floor": 2.50,
            "demand_partners": ["1"],
            "environments": ["Web"],
            "domains": ["premium-site.com", "quality-publisher.org"],
            "geos": ["US", "CA"],
            "devices": ["desktop", "mobile"],
            "created_at": "2024-01-10T12:00:00Z",
            "updated_at": "2024-01-12T15:30:00Z",
        }
    ],
}

DEAL_NOT_FOUND_RESPONSE = {
    "status": "success",
    "data": [],
}

# =============================================================================
# Create Deal Fixtures
# =============================================================================

CREATE_DEAL_SUCCESS_RESPONSE = {
    "status": "success",
    "data": {
        "id": 1004,
        "deal_id": "ELC-MN-2024-NEW",
        "display_name": "Elcano_MediaNet_Test_Deal",
        "status": 1,
        "ad_format": 0,
        "margin": 25.0,
        "margin_type": 1,
        "start_date": "2024-03-01T00:00:00Z",
        "end_date": None,
        "bid_floor": 3.00,
        "demand_partners": ["1"],
        "environments": ["Web"],
        "created_at": "2024-02-28T10:00:00Z",
        "updated_at": "2024-02-28T10:00:00Z",
    },
}

# =============================================================================
# Error Fixtures
# =============================================================================

VALIDATION_ERROR_RESPONSE = {
    "status": "error",
    "code": 422,
    "message": "Validation failed",
    "errors": [
        {"field": "deal_id", "message": "Deal ID is required"},
        {"field": "margin", "message": "Margin must be a positive number"},
    ],
}

UNAUTHORIZED_RESPONSE = {
    "status": "error",
    "code": 401,
    "message": "Invalid Token",
}

SERVER_ERROR_RESPONSE = {
    "status": "error",
    "code": 500,
    "message": "Internal Server Error",
}

# =============================================================================
# Sample Deal Payloads for Tests
# =============================================================================

SAMPLE_DEAL_PAYLOAD = {
    "deal_id": "ELC-MN-2024-NEW",
    "display_name": "Elcano_MediaNet_Test_Deal",
    "start_date": "2024-03-01T00:00:00Z",
    "ad_format": 0,
    "margin": 25.0,
    "margin_type": "percentage",
    "bidders": [{"id": 1}],
    "environments": ["web"],
    "status": 1,
    "is_always_on": True,
}

SAMPLE_DEAL_PAYLOAD_FULL = {
    "deal_id": "ELC-MN-2024-FULL",
    "display_name": "Elcano_MediaNet_Complete_Deal",
    "start_date": "2024-03-01T00:00:00Z",
    "end_date": "2024-12-31T23:59:59Z",
    "ad_format": 0,
    "margin": 30.0,
    "margin_type": "percentage",
    "floor_price": 2.50,
    "bidders": [{"id": 1}, {"id": 2}],
    "environments": ["web", "app"],
    "status": 1,
    "is_always_on": False,
    "domains": ["premium-site.com", "quality-publisher.org"],
    "geos": ["US", "CA", "UK"],
    "devices": ["desktop", "mobile", "tablet"],
}
