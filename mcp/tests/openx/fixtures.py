"""
GraphQL response fixtures for OpenX MCP tests.

These fixtures provide realistic mock responses for all GraphQL operations.
"""

# =============================================================================
# Demand Partners Fixtures
# =============================================================================

DEMAND_PARTNERS_RESPONSE = {
    "data": {
        "optionsByPath": [
            {
                "id": "TTD",
                "name": "The Trade Desk",
                "path": "deal.deal_participants.demand_partner",
                "extra": None,
            },
            {
                "id": "DV360",
                "name": "Google DV360",
                "path": "deal.deal_participants.demand_partner",
                "extra": None,
            },
            {
                "id": "CRIMTAN",
                "name": "Crimtan",
                "path": "deal.deal_participants.demand_partner",
                "extra": None,
            },
            {
                "id": "XANDR",
                "name": "Xandr (Microsoft)",
                "path": "deal.deal_participants.demand_partner",
                "extra": None,
            },
        ]
    }
}

OPTIONS_BY_PATH_RESPONSE = {
    "data": {
        "optionsByPath": [
            {
                "id": "es",
                "name": "Spanish",
                "path": "deal.package.targeting.technographic.language",
                "extra": None,
            },
            {
                "id": "en",
                "name": "English",
                "path": "deal.package.targeting.technographic.language",
                "extra": None,
            },
        ]
    }
}

# =============================================================================
# Deals List Fixtures
# =============================================================================

DEALS_LIST_RESPONSE_PAGE1 = {
    "data": {
        "deals": [
            {
                "id": "deal-001",
                "deal_id": "ELC-2024-001",
                "name": "Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0",
                "status": "ACTIVE",
                "currency": "USD",
                "deal_price": 5.50,
                "pmp_deal_type": "3",
                "start_date": "2024-01-15T00:00:00Z",
                "end_date": None,
                "created_date": "2024-01-10T12:00:00Z",
                "modified_date": "2024-01-12T15:30:00Z",
            },
            {
                "id": "deal-002",
                "deal_id": "ELC-2024-002",
                "name": "Elcano_OpenX_TTD_CTV_Premium_ELC00002_A0",
                "status": "ACTIVE",
                "currency": "USD",
                "deal_price": 12.00,
                "pmp_deal_type": "2",
                "start_date": "2024-01-20T00:00:00Z",
                "end_date": "2024-06-30T23:59:59Z",
                "created_date": "2024-01-18T09:00:00Z",
                "modified_date": "2024-01-18T09:00:00Z",
            },
        ]
    }
}

DEALS_LIST_RESPONSE_PAGE2 = {
    "data": {
        "deals": [
            {
                "id": "deal-003",
                "deal_id": "ELC-2024-003",
                "name": "Elcano_OpenX_DV360_UK_StandardDomains_ELC00003_B1",
                "status": "PAUSED",
                "currency": "GBP",
                "deal_price": 4.25,
                "pmp_deal_type": "3",
                "start_date": "2024-02-01T00:00:00Z",
                "end_date": None,
                "created_date": "2024-01-28T10:00:00Z",
                "modified_date": "2024-02-15T14:00:00Z",
            },
        ]
    }
}

DEALS_LIST_EMPTY_RESPONSE = {"data": {"deals": []}}

# =============================================================================
# Get Deal Fixtures
# =============================================================================

DEAL_RESPONSE = {
    "data": {
        "dealById": {
            "id": "deal-001",
            "deal_id": "ELC-2024-001",
            "name": "Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0",
            "status": "ACTIVE",
            "currency": "USD",
            "deal_price": 5.50,
            "pmp_deal_type": "3",
            "start_date": "2024-01-15T00:00:00Z",
            "end_date": None,
            "created_date": "2024-01-10T12:00:00Z",
            "modified_date": "2024-01-12T15:30:00Z",
            "deal_participants": [
                {
                    "demand_partner": "CRIMTAN",
                    "buyer_ids": ["buyer-123", "buyer-456"],
                    "brand_ids": ["brand-abc"],
                }
            ],
            "package": {
                "uid": "21b2e0df-c0a9-fff1-8123-69534a",
                "name": "US_CuratedDomains_Package",
                "targeting": {
                    "inter_dimension_operator": "AND",
                    "rendering_context": {
                        "op": "AND",
                        "ad_placement": {"op": "==", "val": "BANNER"},
                        "distribution_channel": {"op": "INTERSECTS", "val": "WEB,APP"},
                        "device_type": {
                            "op": "INTERSECTS",
                            "desktop_devices": "desktop",
                            "mobile_devices": "phone,tablet",
                            "tv_devices": None,
                        },
                    },
                    "domain": {
                        "categories_iab_v2": {"op": "INTERSECTS", "val": "384"},
                    },
                    "audience": {
                        "openaudience_custom": {
                            "op": "INTERSECTS",
                            "val": "openaudience-123e4567-e89b-12d3-a456-426614174000",
                        },
                    },
                    "content": {
                        "account": {"op": "NOT INTERSECTS", "val": "193155,209125"},
                    },
                    "geographic": {
                        "includes": {"country": "us", "state": None, "region": None},
                        "excludes": None,
                    },
                },
                "url_targeting": {
                    "type": "blacklist",
                    "urls": ["example.com", "duplicate.com"],
                    "domain_targeting_option": "ROOT",
                },
            },
            "third_party_fees_config": [
                {
                    "partner_id": "partner-001",
                    "revenue_method": "PoM",
                    "gross_share": "30",
                    "gross_cpm_cap": None,
                    "platform_partner_id": None,
                    "platform_share": None,
                }
            ],
        }
    }
}

DEAL_NOT_FOUND_RESPONSE = {"data": {"dealById": None}}

# =============================================================================
# Create Deal Fixtures
# =============================================================================

CREATE_DEAL_SUCCESS_RESPONSE = {
    "data": {
        "dealCreate": {
            "id": "deal-new-001",
            "deal_id": "ELC-2024-NEW-001",
            "name": "Elcano_OpenX_Test_Deal",
            "status": "ACTIVE",
            "currency": "USD",
            "deal_price": 7.50,
            "pmp_deal_type": "3",
            "start_date": "2024-03-01T00:00:00Z",
            "end_date": None,
            "created_date": "2024-02-28T10:00:00Z",
            "modified_date": "2024-02-28T10:00:00Z",
            "package": {
                "uid": "21b2e0df-c0a9-fff1-8123-69534a",
            },
        }
    }
}

# =============================================================================
# Error Fixtures
# =============================================================================

GRAPHQL_ERROR_RESPONSE = {
    "errors": [
        {
            "message": "Invalid input: deal_price must be greater than 0",
            "locations": [{"line": 2, "column": 3}],
            "path": ["dealCreate"],
            "extensions": {
                "code": "VALIDATION_ERROR",
            },
        }
    ],
    "data": None,
}

GRAPHQL_AUTHENTICATION_ERROR = {
    "errors": [
        {
            "message": "Invalid or expired API key",
            "extensions": {
                "code": "AUTHENTICATION_ERROR",
            },
        }
    ],
    "data": None,
}

GRAPHQL_MULTIPLE_ERRORS = {
    "errors": [
        {
            "message": "Field 'name' is required",
            "path": ["dealCreate", "input", "name"],
        },
        {
            "message": "Field 'currency' must be a valid ISO currency code",
            "path": ["dealCreate", "input", "currency"],
        },
    ],
    "data": None,
}

# =============================================================================
# Introspection Fixtures
# =============================================================================

INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE = {
    "data": {
        "__type": {
            "name": "TargetingGeographicItemCreateParams",
            "kind": "INPUT_OBJECT",
            "inputFields": [
                {
                    "name": "country",
                    "type": {
                        "name": "String",
                        "kind": "SCALAR",
                        "ofType": None,
                    },
                },
                {
                    "name": "region",
                    "type": {
                        "name": "String",
                        "kind": "SCALAR",
                        "ofType": None,
                    },
                },
            ],
        }
    }
}

INTROSPECT_TARGETING_PARAMS_RESPONSE = {
    "data": {
        "__type": {
            "name": "TargetingCreateParams",
            "kind": "INPUT_OBJECT",
            "inputFields": [
                {
                    "name": "geographic",
                    "type": {
                        "name": "TargetingGeographicCreateParams",
                        "kind": "INPUT_OBJECT",
                        "ofType": None,
                    },
                },
                {
                    "name": "rendering_context",
                    "type": {
                        "name": "TargetingRenderingContextCreateParams",
                        "kind": "INPUT_OBJECT",
                        "ofType": None,
                    },
                },
                {
                    "name": "technographic",
                    "type": {
                        "name": "TargetingTechnographicCreateParams",
                        "kind": "INPUT_OBJECT",
                        "ofType": None,
                    },
                },
                {
                    "name": "domain",
                    "type": {
                        "name": "TargetingDomainCreateParams",
                        "kind": "INPUT_OBJECT",
                        "ofType": None,
                    },
                },
            ],
        }
    }
}

INTROSPECT_TYPE_NOT_FOUND_RESPONSE = {"data": {"__type": None}}
