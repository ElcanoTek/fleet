"""HTTP response fixtures for Magnite DV+ reporting tests."""

# =============================================================================
# Performance Analytics Reporting Fixtures
# =============================================================================

CREATE_REPORT_RESPONSE = {
    "offline_report_id": 456,
    "status": "queued",
    "created": "2026-03-07T23:14:47Z",
    "updated": "2026-03-07T23:14:47Z",
}

REPORT_STATUS_QUEUED = {
    "offline_report_id": 456,
    "status": "queued",
    "created": "2026-03-07T23:14:47Z",
    "updated": "2026-03-07T23:14:47Z",
}

REPORT_STATUS_SUCCESS = {
    "offline_report_id": 456,
    "status": "success",
    "created": "2026-03-07T23:14:47Z",
    "updated": "2026-03-07T23:15:02Z",
}

REPORT_DATA_JSON_RESPONSE = {
    "content": [
        {
            "date": "2026-03-06",
            "site": "example-site-1",
            "bid_requests": 150000,
            "paid_impression": 12000,
            "buyer_spend": 96.50,
            "curator_net_revenue": 14.48,
        },
        {
            "date": "2026-03-06",
            "site": "example-site-2",
            "bid_requests": 85000,
            "paid_impression": 7200,
            "buyer_spend": 57.60,
            "curator_net_revenue": 8.64,
        },
    ],
    "summary": {
        "paid_impression": 19200,
        "buyer_spend": 154.10,
        "publisher_gross_revenue": 100.25,
    },
    "pageInfo": None,
}

LIST_REPORTS_RESPONSE = {
    "content": [
        {
            "date": "2026-03-07",
            "marketplace": "Elcano",
            "marketplace__filterValue": "131",
            "paid_impression": 12000,
            "bid_requests": 150000,
            "bid_responses": 5000,
            "publisher_gross_revenue": 96.5,
            "buyer_spend": 154.1,
        },
        {
            "date": "2026-03-08",
            "marketplace": "Elcano - OMG approved publishers",
            "marketplace__filterValue": "459",
            "paid_impression": 0,
            "bid_requests": 1000,
            "bid_responses": 0,
            "publisher_gross_revenue": 0,
            "buyer_spend": 0,
        },
    ],
    "summary": {
        "paid_impression": 12000,
        "bid_requests": 151000,
        "bid_responses": 5000,
        "publisher_gross_revenue": 96.5,
        "buyer_spend": 154.1,
    },
    "pageInfo": None,
}
