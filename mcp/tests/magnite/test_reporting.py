"""Tests for Magnite DV+ reporting MCP tools."""

import base64
import hashlib
import json
import os
import sys
from pathlib import Path

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import magnite_mcp
from magnite_mcp import (
    magnite_check_report_status,
    magnite_create_offline_report,
    magnite_download_report,
    magnite_list_reports,
)

from .conftest import MAGNITE_DV_ANALYTICS_ENDPOINT
from .fixtures import (
    CREATE_REPORT_RESPONSE,
    LIST_REPORTS_RESPONSE,
    REPORT_DATA_JSON_RESPONSE,
    REPORT_STATUS_QUEUED,
    REPORT_STATUS_SUCCESS,
)


def _assert_dv_auth(request: httpx.Request) -> None:
    expected = base64.b64encode(b"test-access-key-12345:test-secret-key-67890").decode()
    assert request.headers.get("Authorization") == f"Basic {expected}"
    assert request.headers.get("Cookie") is None


class TestCreateOfflineReport:
    """Tests for magnite_create_offline_report."""

    @pytest.mark.asyncio
    async def test_create_offline_report_success_with_date_range(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_RESPONSE)

        mock_magnite_dv_api.post(MAGNITE_DV_ANALYTICS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_create_offline_report(
            dimensions=["date", "site", "marketplace_deal_name"],
            metrics=["bid_requests", "paid_impression", "buyer_spend"],
            date_range="yesterday",
            filters="dimension:site_id==32464,32426",
            timezone="America/New_York",
        )

        assert result == {
            "success": True,
            "offline_report_id": 456,
            "status": "queued",
            "created": "2026-03-07T23:14:47Z",
            "updated": "2026-03-07T23:14:47Z",
        }

        assert captured_request is not None
        _assert_dv_auth(captured_request)
        assert captured_request.url.params.get("account") == "mp-vendor/102"

        payload = json.loads(captured_request.content)
        criteria = payload["criteria"]
        assert criteria["dimension"] == "date,site,marketplace_deal_name"
        assert criteria["metric"] == "bid_requests,paid_impression,buyer_spend"
        assert criteria["date_range"] == "yesterday"
        assert criteria["start"] is None
        assert criteria["end"] is None
        assert criteria["timezone"] == "America/New_York"
        assert criteria["filters"] == "dimension:site_id==32464,32426"

    @pytest.mark.asyncio
    async def test_create_offline_report_success_with_start_end(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_RESPONSE)

        mock_magnite_dv_api.post(MAGNITE_DV_ANALYTICS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_create_offline_report(
            dimensions=["date", "site"],
            metrics=["bid_requests", "buyer_spend"],
            start_date="2026-03-01T00:00:00Z",
            end_date="2026-03-02T00:00:00Z",
        )

        assert result["success"] is True
        assert captured_request is not None
        _assert_dv_auth(captured_request)

        payload = json.loads(captured_request.content)
        criteria = payload["criteria"]
        assert criteria["date_range"] is None
        assert criteria["start"] == "2026-03-01T00:00:00Z"
        assert criteria["end"] == "2026-03-02T00:00:00Z"

    @pytest.mark.asyncio
    async def test_create_offline_report_normalizes_bare_dates(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_RESPONSE)

        mock_magnite_dv_api.post(MAGNITE_DV_ANALYTICS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_create_offline_report(
            dimensions=["date", "site"],
            metrics=["bid_requests", "buyer_spend"],
            start_date="2026-03-01",
            end_date="2026-03-02",
        )

        assert result["success"] is True
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        criteria = payload["criteria"]
        assert criteria["start"] == "2026-03-01T00:00:00Z"
        assert criteria["end"] == "2026-03-02T00:00:00Z"

    @pytest.mark.asyncio
    async def test_create_offline_report_rejects_naive_datetimes(
        self,
        mock_magnite_dv_api: respx.MockRouter,  # noqa: ARG002
    ):
        result = await magnite_create_offline_report(
            dimensions=["date", "site"],
            metrics=["bid_requests", "buyer_spend"],
            start_date="2026-03-01T00:00:00",
            end_date="2026-03-02T00:00:00Z",
        )

        assert result["success"] is False
        assert "start_date must include a timezone offset or Z suffix" in result["error"]["message"]

    @pytest.mark.asyncio
    async def test_create_offline_report_validation_failure(
        self,
        mock_magnite_dv_api: respx.MockRouter,  # noqa: ARG002
    ):
        result = await magnite_create_offline_report(dimensions=["date"], metrics=["bid_requests"])

        assert result["success"] is False
        assert "Either date_range or both start_date and end_date must be provided." in result["error"]["message"]


class TestCheckReportStatus:
    """Tests for magnite_check_report_status."""

    @pytest.mark.asyncio
    async def test_check_report_status_success(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=REPORT_STATUS_SUCCESS)

        mock_magnite_dv_api.get(f"{MAGNITE_DV_ANALYTICS_ENDPOINT}/456").mock(side_effect=capture)

        result = await magnite_check_report_status(456)

        assert result["success"] is True
        assert result["status"] == "success"
        assert captured_request is not None
        _assert_dv_auth(captured_request)
        assert captured_request.url.params.get("account") == "mp-vendor/102"

    @pytest.mark.asyncio
    async def test_check_report_status_queued(self, mock_magnite_dv_api: respx.MockRouter):
        mock_magnite_dv_api.get(f"{MAGNITE_DV_ANALYTICS_ENDPOINT}/456").mock(
            return_value=httpx.Response(200, json=REPORT_STATUS_QUEUED)
        )

        result = await magnite_check_report_status(456)

        assert result["success"] is True
        assert result["status"] == "queued"


class TestListReports:
    """Tests for magnite_list_reports."""

    @pytest.mark.asyncio
    async def test_list_reports_default_date_range(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LIST_REPORTS_RESPONSE)

        mock_magnite_dv_api.get(MAGNITE_DV_ANALYTICS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_list_reports()

        assert result["success"] is True
        assert len(result["reports"]) == 2
        assert captured_request is not None
        _assert_dv_auth(captured_request)
        assert captured_request.url.params.get("date_range") == "today"
        assert captured_request.url.params.get("account") == "mp-vendor/102"

    @pytest.mark.asyncio
    async def test_list_reports_explicit_date_range(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LIST_REPORTS_RESPONSE)

        mock_magnite_dv_api.get(MAGNITE_DV_ANALYTICS_ENDPOINT).mock(side_effect=capture)

        result = await magnite_list_reports("last_3")

        assert result["success"] is True
        assert captured_request is not None
        _assert_dv_auth(captured_request)
        assert captured_request.url.params.get("date_range") == "last_3"


class TestDownloadReport:
    """Tests for magnite_download_report."""

    @pytest.mark.asyncio
    async def test_download_report_json(self, mock_magnite_dv_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=REPORT_DATA_JSON_RESPONSE)

        mock_magnite_dv_api.get(f"{MAGNITE_DV_ANALYTICS_ENDPOINT}/456/data").mock(side_effect=capture)

        result = await magnite_download_report(report_id=456, format="json", page=2, size=1000)

        assert result["success"] is True
        assert result["content"] == REPORT_DATA_JSON_RESPONSE["content"]
        assert result["page"] == REPORT_DATA_JSON_RESPONSE["pageInfo"]
        assert captured_request is not None
        _assert_dv_auth(captured_request)
        assert captured_request.url.params.get("format") == "json"
        assert captured_request.url.params.get("page") == "2"
        assert captured_request.url.params.get("size") == "1000"
        assert captured_request.url.params.get("account") == "mp-vendor/102"

    @pytest.mark.asyncio
    async def test_download_report_csv_saves_file(self, mock_magnite_dv_api: respx.MockRouter, tmp_path):
        csv_content = b"date,site,bid_requests\n2026-03-06,example-site-1,150000\n"
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, content=csv_content, headers={"content-type": "text/csv"})

        mock_magnite_dv_api.get(f"{MAGNITE_DV_ANALYTICS_ENDPOINT}/456/data").mock(side_effect=capture)

        client = magnite_mcp.get_magnite_client()
        client._download_dir = str(tmp_path)

        result = await magnite_download_report(report_id=456, format="csv", page=1)

        assert result["success"] is True
        assert result["bytes"] == len(csv_content)
        assert result["sha256"] == hashlib.sha256(csv_content).hexdigest()
        assert result["content_type"] == "text/csv"
        assert captured_request is not None
        _assert_dv_auth(captured_request)

        saved_path = Path(result["path"])
        assert saved_path.exists()
        assert saved_path.read_bytes() == csv_content

    @pytest.mark.asyncio
    async def test_download_report_not_ready_returns_helpful_error(self, mock_magnite_dv_api: respx.MockRouter):
        mock_magnite_dv_api.get(f"{MAGNITE_DV_ANALYTICS_ENDPOINT}/456/data").mock(
            return_value=httpx.Response(409, json={"message": "Report not ready"})
        )

        result = await magnite_download_report(report_id=456)

        assert result["success"] is False
        assert result["error"]["status_code"] == 409
        assert "Poll magnite_check_report_status until status is 'success'." in result["error"]["message"]
