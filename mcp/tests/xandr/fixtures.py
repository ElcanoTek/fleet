"""
HTTP response fixtures for Xandr MCP tests.

These fixtures provide realistic mock responses for all API operations
against the Xandr (AppNexus) Deal Service API.
"""

# =============================================================================
# Login Fixtures
# =============================================================================

LOGIN_SUCCESS_RESPONSE = {
    "response": {
        "status": "OK",
        "token": "xandr-auth-token-abc123",
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 50,
        },
    }
}

LOGIN_INVALID_CREDENTIALS_RESPONSE = {
    "response": {
        "error_id": "NOAUTH",
        "error": "No match found for user/pass",
        "error_description": "Authentication failed - invalid credentials",
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 30,
        },
    }
}

# =============================================================================
# Deal List Fixtures
# =============================================================================

DEALS_LIST_RESPONSE = {
    "response": {
        "status": "OK",
        "count": 2,
        "start_element": 0,
        "num_elements": 100,
        "deals": [
            {
                "id": 5001,
                "name": "Elcano_Xandr_Premium_Banner",
                "description": "Premium banner deal for US traffic",
                "state": "active",
                "start_date": "2024-01-15 00:00:00",
                "end_date": None,
                "deal_type": {"id": 2, "name": "Private Auction"},
                "payment_type": {"id": 1, "name": "CPM"},
                "currency": "USD",
                "use_deal_floor": True,
                "floor_price": 5.00,
                "buyers": [{"id": 123, "name": "Test Buyer Alpha"}],
                "member_id": 9544,
                "last_modified": "2024-01-12T15:30:00Z",
            },
            {
                "id": 5002,
                "name": "Elcano_Xandr_Video_PMP",
                "description": "Video PMP deal for premium inventory",
                "state": "active",
                "start_date": "2024-02-01 00:00:00",
                "end_date": "2024-12-31 23:59:59",
                "deal_type": {"id": 2, "name": "Private Auction"},
                "payment_type": {"id": 1, "name": "CPM"},
                "currency": "USD",
                "use_deal_floor": True,
                "floor_price": 8.50,
                "buyers": [
                    {"id": 123, "name": "Test Buyer Alpha"},
                    {"id": 456, "name": "Test Buyer Beta"},
                ],
                "member_id": 9544,
                "last_modified": "2024-02-05T09:00:00Z",
            },
        ],
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 120,
        },
    }
}

DEALS_LIST_EMPTY_RESPONSE = {
    "response": {
        "status": "OK",
        "count": 0,
        "start_element": 0,
        "num_elements": 100,
        "deals": [],
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 45,
        },
    }
}

# =============================================================================
# Get Deal Fixtures
# =============================================================================

DEAL_RESPONSE = {
    "response": {
        "status": "OK",
        "count": 1,
        "deal": {
            "id": 5001,
            "name": "Elcano_Xandr_Premium_Banner",
            "description": "Premium banner deal for US traffic",
            "state": "active",
            "start_date": "2024-01-15 00:00:00",
            "end_date": None,
            "deal_type": {"id": 2, "name": "Private Auction"},
            "payment_type": {"id": 1, "name": "CPM"},
            "currency": "USD",
            "use_deal_floor": True,
            "floor_price": 5.00,
            "buyers": [{"id": 123, "name": "Test Buyer Alpha"}],
            "member_id": 9544,
            "last_modified": "2024-01-12T15:30:00Z",
        },
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 80,
        },
    }
}

DEAL_NOT_FOUND_RESPONSE = {
    "response": {
        "status": "OK",
        "count": 0,
        "deal": None,
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 40,
        },
    }
}

# =============================================================================
# Create Deal Fixtures
# =============================================================================

CREATE_DEAL_SUCCESS_RESPONSE = {
    "response": {
        "status": "OK",
        "count": 1,
        "id": 5010,
        "deal": {
            "id": 5010,
            "name": "Elcano_Xandr_New_Deal",
            "description": "A newly created test deal",
            "state": "active",
            "start_date": "2024-03-01 00:00:00",
            "end_date": None,
            "deal_type": {"id": 2, "name": "Private Auction"},
            "payment_type": {"id": 1, "name": "CPM"},
            "currency": "USD",
            "use_deal_floor": True,
            "floor_price": 5.00,
            "buyers": [{"id": 123, "name": "Test Buyer Alpha"}],
            "member_id": 9544,
            "last_modified": "2024-02-28T10:00:00Z",
        },
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 150,
        },
    }
}

# =============================================================================
# Error Fixtures
# =============================================================================

VALIDATION_ERROR_RESPONSE = {
    "response": {
        "error_id": "SYNTAX",
        "error": "Invalid deal payload",
        "error_description": "Field 'name' is required",
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 30,
        },
    }
}

UNAUTHORIZED_RESPONSE = {
    "response": {
        "error_id": "NOAUTH",
        "error": "Authentication failed",
        "error_description": "Token is invalid or expired",
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 10,
        },
    }
}

SERVER_ERROR_RESPONSE = {
    "response": {
        "error_id": "SYSTEM",
        "error": "Service unavailable",
        "error_description": "An internal error has occurred",
        "dbg_info": {
            "instance": "api-prod-01",
            "time": 5,
        },
    }
}

# =============================================================================
# Sample Deal Payloads for Tests
# =============================================================================

SAMPLE_DEAL_PAYLOAD = {
    "deal": {
        "name": "Elcano_Xandr_New_Deal",
        "code": "elcano_xandr_new_deal",
        "deal_type": {"id": 2, "name": "Private Auction"},
        "buyers": [{"id": 123, "name": "Test Buyer Alpha"}],
    }
}

SAMPLE_DEAL_PAYLOAD_FULL = {
    "deal": {
        "name": "Elcano_Xandr_Complete_Deal",
        "code": "elcano_xandr_complete_deal",
        "description": "A complete deal with all optional fields",
        "state": "active",
        "start_date": "2024-03-01 00:00:00",
        "end_date": "2024-12-31 23:59:59",
        "deal_type": {"id": 2, "name": "Private Auction"},
        "payment_type": {"id": 1, "name": "CPM"},
        "currency": "USD",
        "use_deal_floor": True,
        "floor_price": 5.00,
        "buyers": [
            {"id": 123, "name": "Test Buyer Alpha"},
            {"id": 456, "name": "Test Buyer Beta"},
        ],
        "member_id": 9544,
    }
}
