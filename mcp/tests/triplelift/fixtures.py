AUTH_SUCCESS_RESPONSE = {
    "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test-token",
    "token_type": "Bearer",
    "expires_in": 3600,
}

# TripleLift wraps successful curation API responses as {"status": "success", "data": ...}.
# Fixtures below mirror that real shape so tests exercise the same unwrap path as production.

# Mirrors the real GET deal envelope: `targeting` / `adQualityProfile` / `curationFee`
# / `dealTypeId` are SIBLINGS of `deal`, not nested under it.
GET_DEAL_RESPONSE = {
    "status": "success",
    "data": {
        "deal": {
            "id": 1001,
            "name": "Elcano_TL_Premium_Display",
            "memberId": 12345,
            "memberType": "curator",
            "active": True,
            "dealPriceType": "FLOOR",
            "dealPriceValue": 5.0,
            "startDate": "2025-03-01",
            "endDate": "2025-12-31",
            "channel": "WEB",
            "commercializedFormats": ["DISPLAY", "IMAGE"],
        },
        "targeting": {
            "type": "AND",
            "children": [
                {
                    "type": "ANY",
                    "excluded": False,
                    "binding": "EB_SUPPLY_GEO_COUNTRY_ID",
                    "integralTargets": [233],
                },
                {
                    "type": "AND",
                    "children": [
                        {
                            "type": "ANY",
                            "excluded": False,
                            "binding": "UI_EXPR_REGULATORY_POLICY_CONTROLLED",
                            "integralTargets": [23891],
                        }
                    ],
                },
            ],
        },
        "adQualityProfile": {
            "dsp": {"id": 1, "seat": {"id": 100, "name": "Test Seat", "seatString": "seat-100"}},
        },
        "curationFee": None,
        "dealTypeId": 2,
    },
}

LIST_DEALS_RESPONSE = {
    "status": "success",
    "data": {
        "deals": [
            {"id": 1001, "name": "Elcano_TL_Premium_Display", "active": True},
            {"id": 1002, "name": "Elcano_TL_CTV_Sports", "active": False},
        ]
    },
}

CREATE_DEAL_SUCCESS_RESPONSE = {
    "status": "success",
    "data": {
        "deal": {
            "id": 1010,
            "name": "Elcano_TL_New_Deal",
            "memberId": 12345,
            "active": True,
        }
    },
}

# Real /buyers response is {"status":"success","data":[{id, name, active}, ...]}
# (the OpenAPI's nested {member: {...}} shape doesn't match what the server returns).
BUYERS_RESPONSE = {
    "status": "success",
    "data": [
        {"id": 1, "name": "The Trade Desk", "active": True},
        {"id": 2, "name": "DV360", "active": True},
    ],
}

COUNTRIES_RESPONSE = {
    "status": "success",
    "data": {
        "countries": [
            {"id": 233, "name": "United States", "code": "US"},
            {"id": 38, "name": "Canada", "code": "CA"},
            {"id": 77, "name": "United Kingdom", "code": "GB"},
        ]
    },
}

# Real /segments response is {"status":"success","data":[{...}, ...]}.
SEGMENTS_RESPONSE = {
    "status": "success",
    "data": [
        {"id": 991, "name": "Sports Enthusiasts"},
        {"id": 992, "name": "Luxury Shoppers"},
    ],
}

AVAILS_RESPONSE = {
    "status": "success",
    "data": {
        "availsCount": 123456,
        "unsupportedBindings": [],
    },
}

SAMPLE_CREATE_DEAL_PAYLOAD = {
    "memberId": 12345,
    "name": "Elcano_TL_New_Deal",
    "active": True,
    "primaryGoalId": 1,
    "secondaryGoal": {"id": None, "value": None},
    "budget": None,
    "dealPriceType": "FLOOR",
    "dealPriceValue": 5.0,
    "startDate": "2025-03-01",
    "endDate": "2025-12-31",
    "commercializedFormats": ["DISPLAY", "IMAGE"],
    "dsp": {"id": 1},
    "channel": "WEB",
    "isPublisher": False,
    "creativeTags": None,
    "dspFormatWorkflow": "NATIVE",
    "dealTypeId": 2,
    "targetingExpression": {
        "type": "AND",
        "children": [
            {
                "type": "ANY",
                "binding": "EB_SUPPLY_GEO_COUNTRY_ID",
                "integralTargets": [233],
            }
        ],
    },
}
