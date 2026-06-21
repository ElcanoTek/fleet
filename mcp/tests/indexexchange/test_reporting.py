"""
Tests for Index Exchange Reporting MCP tools.

Validates:
- ix_create_report_spec: payload correctness, account validation
- ix_list_active_reports: query params
- ix_run_report_download: returns reportRunID
- ix_list_report_files: query params formatting
- ix_download_report_file: saves file to disk, returns metadata
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    _normalize_ix_report_date_range,
    ix_create_marketplace_report_spec,
    ix_create_report_spec,
    ix_download_report_file,
    ix_list_active_reports,
    ix_list_marketplace_report_presets,
    ix_list_report_files,
    ix_reporting_healthcheck,
    ix_run_marketplace_draft_report,
    ix_run_report_download,
    ix_run_updated_marketplace_draft_report,
    ix_update_marketplace_draft_report_spec,
    ix_update_report_spec,
)

from .conftest import (
    IX_BASE_URL,
    IX_LOGIN_ENDPOINT,
    IX_REPORT_FILES_LIST_ENDPOINT,
    IX_REPORT_RUNS_ENDPOINT,
    IX_REPORT_SPEC_UPDATE_ENDPOINT,
    IX_REPORT_SPECS_ENDPOINT,
    IX_REPORT_SPECS_INFO_ENDPOINT,
)
from .fixtures import (
    ACTIVE_REPORTS_RESPONSE,
    CREATE_REPORT_SPEC_RESPONSE,
    LOGIN_SUCCESS_RESPONSE,
    REPORT_FILES_RESPONSE,
    REPORT_RUN_RESPONSE,
)


class TestCreateReportSpec:
    """Tests for ix_create_report_spec tool."""

    @pytest.mark.asyncio
    async def test_create_report_spec_success(self, mock_ix_api: respx.MockRouter):
        """Test successful report spec creation."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_report_spec(
            report_title="Daily Revenue",
            accounts=[100],
            fields=["date", "impressions", "revenue"],
            date_range={"from": "2024-06-01", "to": "2024-06-30"},
        )

        assert result["success"] is True
        assert result["report_spec"]["reportSpecID"] == 11026

        # Verify payload structure
        payload = json.loads(captured_request.content)
        assert payload["reportTitle"] == "Daily Revenue"
        assert payload["accounts"] == [100]
        assert payload["querySpec"]["fields"] == ["date", "impressions", "revenue"]
        assert payload["dateRange"]["from"] == "2024-06-01"
        assert payload["dateRange"]["to"] == "2024-06-30"

    @pytest.mark.asyncio
    async def test_create_report_spec_single_day_range_uses_exclusive_end(self, mock_ix_api: respx.MockRouter):
        """A same-day {from, to} must be widened to an exclusive next-day end.

        IX treats dateRange.to as exclusive, so {"from": D, "to": D} is empty
        and the API returns RSE-4009. The wrapper should send to = D+1.
        """
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_report_spec(
            report_title="Yesterday",
            accounts=[100],
            fields=["day", "impressions"],
            date_range={"from": "2026-06-01", "to": "2026-06-01"},
        )

        assert result["success"] is True
        payload = json.loads(captured_request.content)
        assert payload["dateRange"]["from"] == "2026-06-01"
        assert payload["dateRange"]["to"] == "2026-06-02"

    def test_normalize_date_range(self):
        """Single-day/reversed ranges get an exclusive end; everything else passes through."""
        # Same-day -> next-day exclusive end.
        assert _normalize_ix_report_date_range({"from": "2026-06-01", "to": "2026-06-01"}) == {
            "from": "2026-06-01",
            "to": "2026-06-02",
        }
        # Reversed end is also corrected.
        assert _normalize_ix_report_date_range({"from": "2026-06-01", "to": "2026-05-30"})["to"] == "2026-06-02"
        # Valid multi-day range is untouched.
        assert _normalize_ix_report_date_range({"from": "2026-06-01", "to": "2026-06-08"}) == {
            "from": "2026-06-01",
            "to": "2026-06-08",
        }
        # Relative ranges pass through unchanged.
        assert _normalize_ix_report_date_range({"current": "month"}) == {"current": "month"}
        assert _normalize_ix_report_date_range({"previous": {"days": 1}}) == {"previous": {"days": 1}}
        # Non-ISO / timestamped values are left alone.
        assert _normalize_ix_report_date_range({"from": "2026-06-01T00:00:00Z", "to": "2026-06-01T00:00:00Z"}) == {
            "from": "2026-06-01T00:00:00Z",
            "to": "2026-06-01T00:00:00Z",
        }

    @pytest.mark.asyncio
    async def test_create_report_spec_with_current_date_range(self, mock_ix_api: respx.MockRouter):
        """Test report spec with 'current' date range type."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_report_spec(
            report_title="Current Month",
            accounts=[100],
            fields=["date", "impressions"],
            date_range={"current": "month"},
        )

        assert result["success"] is True
        payload = json.loads(captured_request.content)
        assert payload["dateRange"]["current"] == "month"

    @pytest.mark.asyncio
    async def test_create_report_spec_with_previous_date_range(self, mock_ix_api: respx.MockRouter):
        """Test report spec with 'previous' date range type."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_report_spec(
            report_title="Last Week",
            accounts=[100],
            fields=["date", "impressions"],
            date_range={"previous": {"weeks": 1}},
        )

        assert result["success"] is True
        payload = json.loads(captured_request.content)
        assert payload["dateRange"]["previous"] == {"weeks": 1}

    @pytest.mark.asyncio
    async def test_create_report_spec_with_optional_fields(self, mock_ix_api: respx.MockRouter):
        """Test report spec with delivery, schedule, and other optional fields."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_report_spec(
            report_title="Scheduled Report",
            accounts=[100],
            fields=["date", "revenue"],
            date_range={"from": "2024-06-01", "to": "2024-06-30"},
            delivery={"email": "test@example.com"},
            schedule={"frequency": "daily"},
            file_type="csv",
            region_setting="US",
            report_status="saved",
            query_spec_extras={"filters": [{"field": "site", "values": ["example.com"]}]},
        )

        assert result["success"] is True
        payload = json.loads(captured_request.content)
        assert payload["delivery"] == {"email": "test@example.com"}
        assert payload["schedule"] == {"frequency": "daily"}
        assert payload["fileType"] == "csv"
        assert payload["regionSetting"] == "US"
        assert payload["reportStatus"] == "saved"
        assert "filters" in payload["querySpec"]


class TestCreateReportSpecValidation:
    """Tests for report spec input validation."""

    @pytest.mark.asyncio
    async def test_too_many_accounts(
        self,
        mock_ix_api: respx.MockRouter,  # noqa: ARG002
    ):
        """Test that >1 account fails fast."""
        result = await ix_create_report_spec(
            report_title="Test",
            accounts=[100, 200],
            fields=["date"],
            date_range={"from": "2024-01-01", "to": "2024-01-31"},
        )
        assert result["success"] is False
        assert "1 account" in result["error"]["message"].lower()

    @pytest.mark.asyncio
    async def test_zero_accounts(
        self,
        mock_ix_api: respx.MockRouter,  # noqa: ARG002
    ):
        """Test that 0 accounts fails fast."""
        result = await ix_create_report_spec(
            report_title="Test",
            accounts=[],
            fields=["date"],
            date_range={"from": "2024-01-01", "to": "2024-01-31"},
        )
        assert result["success"] is False
        assert "1 account" in result["error"]["message"].lower()

    @pytest.mark.asyncio
    async def test_invalid_date_range(
        self,
        mock_ix_api: respx.MockRouter,  # noqa: ARG002
    ):
        """Test that date_range without recognized keys fails."""
        result = await ix_create_report_spec(
            report_title="Test",
            accounts=[100],
            fields=["date"],
            date_range={"invalid_key": "value"},
        )
        assert result["success"] is False
        assert "date_range" in result["error"]["message"]


class TestUpdateReportSpec:
    """Tests for ix_update_report_spec tool."""

    @pytest.mark.asyncio
    async def test_update_report_spec_success(self, mock_ix_api: respx.MockRouter):
        """Test successful report spec update via PATCH."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.patch(IX_REPORT_SPEC_UPDATE_ENDPOINT).mock(side_effect=capture)

        result = await ix_update_report_spec(
            report_id=11026,
            report_title="Draft report for testing",
            accounts=[12345],
            fields=["ad_spend", "month", "brand_name"],
            date_range={"current": "month"},
            file_type="csv.zip",
            report_status="draft",
        )

        assert result["success"] is True
        assert result["report_spec"]["reportStatus"] == "draft"
        assert captured_request.method == "PATCH"

        payload = json.loads(captured_request.content)
        assert payload["reportTitle"] == "Draft report for testing"
        assert payload["accounts"] == [12345]
        assert payload["querySpec"]["fields"] == ["ad_spend", "month", "brand_name"]
        assert payload["dateRange"] == {"current": "month"}
        assert payload["fileType"] == "csv.zip"
        assert payload["reportStatus"] == "draft"

    @pytest.mark.asyncio
    async def test_update_report_spec_validation(self):
        """Test update validation mirrors create validation."""
        result = await ix_update_report_spec(
            report_id=11026,
            report_title="Bad Update",
            accounts=[],
            fields=["ad_spend"],
            date_range={"current": "month"},
        )

        assert result["success"] is False
        assert result["error"]["operation"] == "update_report_spec"

    @pytest.mark.asyncio
    async def test_update_marketplace_draft_report_spec_accepts_account_name(self, mock_ix_api: respx.MockRouter):
        """Test Marketplace draft update resolves account names and preset fields."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.patch(IX_REPORT_SPEC_UPDATE_ENDPOINT).mock(side_effect=capture)

        result = await ix_update_marketplace_draft_report_spec(
            report_id=11026,
            account_id="Raptive",
            report_title="Raptive Draft Labels",
            preset="deal_labels",
            date_range={"previous": {"days": 1}},
            extra_fields=["brand_name"],
            file_type="csv.zip",
        )

        assert result["success"] is True
        assert result["account_id"] == 1502939
        assert result["preset"] == "deal_labels"
        assert "brand_name" in result["selected_fields"]

        payload = json.loads(captured_request.content)
        assert payload["accounts"] == [1502939]
        assert payload["reportStatus"] == "draft"
        assert payload["dateRange"] == {"previous": {"days": 1}}
        assert payload["querySpec"]["fields"] == result["selected_fields"]


class TestMarketplaceReportPresets:
    """Tests for Marketplace reporting preset helpers."""

    @pytest.mark.asyncio
    async def test_list_marketplace_presets(self):
        """Test preset discovery response shape."""
        result = await ix_list_marketplace_report_presets()

        assert result["success"] is True
        assert result["account_type"] == "marketplace_partner"
        assert "deal_summary" in result["presets"]
        assert "deal_labels" in result["presets"]
        assert "supply_breakdown" in result["presets"]
        assert "segment_performance" in result["presets"]
        assert result["known_accounts"]["Elcano"] == 1491166
        assert result["known_accounts"]["The Weather Company, LLC"] == 1499155

    @pytest.mark.asyncio
    async def test_create_marketplace_report_spec_from_preset(self, mock_ix_api: respx.MockRouter):
        """Test preset wrapper builds the expected field list."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_marketplace_report_spec(
            account_id=100,
            report_title="Marketplace Deal Summary",
            preset="deal_summary",
            date_range={"previous": {"days": 1}},
            extra_fields=["external_reference_id", "marketplace_media_spend"],
        )

        assert result["success"] is True
        assert result["preset"] == "deal_summary"
        assert "external_reference_id" in result["selected_fields"]

        payload = json.loads(captured_request.content)
        assert payload["accounts"] == [100]
        assert payload["reportTitle"] == "Marketplace Deal Summary"
        assert payload["dateRange"] == {"previous": {"days": 1}}
        assert payload["querySpec"]["fields"] == result["selected_fields"]
        assert payload["querySpec"]["fields"].count("marketplace_media_spend") == 1
        assert "publisher_payment" not in payload["querySpec"]["fields"]
        assert "device_type" in payload["querySpec"]["fields"]

    @pytest.mark.asyncio
    async def test_create_marketplace_report_spec_accepts_account_name(self, mock_ix_api: respx.MockRouter):
        """Test known Marketplace account names resolve to account IDs."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_marketplace_report_spec(
            account_id="Elcano",
            report_title="Marketplace Deal Summary",
            preset="deal_summary",
            date_range={"previous": {"days": 1}},
        )

        assert result["success"] is True
        assert result["account_id"] == 1491166

        payload = json.loads(captured_request.content)
        assert payload["accounts"] == [1491166]

    @pytest.mark.asyncio
    async def test_create_marketplace_report_spec_invalid_preset(self):
        """Test invalid preset names fail fast."""
        result = await ix_create_marketplace_report_spec(
            account_id=100,
            report_title="Bad Preset",
            preset="unknown",
            date_range={"previous": {"days": 1}},
        )

        assert result["success"] is False
        assert result["error"]["operation"] == "create_marketplace_report_spec"
        assert result["error"]["details"]["preset"] == "unknown"

    @pytest.mark.asyncio
    async def test_create_marketplace_report_spec_invalid_account_name(self):
        """Test unknown account names fail fast with a clear message."""
        result = await ix_create_marketplace_report_spec(
            account_id="Unknown Account",
            report_title="Bad Account",
            preset="deal_summary",
            date_range={"previous": {"days": 1}},
        )

        assert result["success"] is False
        assert result["error"]["operation"] == "create_marketplace_report_spec"
        assert "Unknown Marketplace account name" in result["error"]["message"]


class TestRunMarketplaceDraftReport:
    """Tests for the end-to-end Marketplace draft report workflow."""

    @pytest.mark.asyncio
    async def test_run_marketplace_draft_report_with_download(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test create -> run -> list files -> download workflow."""
        import indexexchange_mcp

        create_request = None
        run_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal create_request
            create_request = request
            return httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})

        def capture_run(request: httpx.Request) -> httpx.Response:
            nonlocal run_request
            run_request = request
            return httpx.Response(200, json=REPORT_RUN_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(side_effect=capture_create)
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(side_effect=capture_run)
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(200, json=REPORT_FILES_RESPONSE)
        )

        csv_content = b"deal_id,deal_name,marketplace_media_spend\nabc,Test Deal,12.34\n"
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-abc-123"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(200, content=csv_content, headers={"content-type": "text/csv"})
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_run_marketplace_draft_report(
            account_id="The Weather Company, LLC",
            report_title="Marketplace Draft",
            date_range={"current": "month"},
            preset="deal_summary",
            extra_fields=["external_reference_id"],
            file_type="csv",
        )

        assert result["success"] is True
        assert result["account_id"] == 1499155
        assert result["report_spec"]["reportStatus"] == "draft"
        assert result["report_run"]["reportRunID"] == 55001
        assert result["report_file"]["fileID"] == "file-abc-123"
        assert result["download"]["success"] is True

        create_payload = json.loads(create_request.content)
        assert create_payload["accounts"] == [1499155]
        assert create_payload["reportStatus"] == "draft"
        assert create_payload["fileType"] == "csv"
        assert create_payload["dateRange"] == {"current": "month"}

        run_payload = json.loads(run_request.content)
        assert run_payload["reportID"] == 11026
        assert run_payload["reportStatus"] == "draft"

    @pytest.mark.asyncio
    async def test_run_marketplace_draft_report_timeout_returns_pending(self, mock_ix_api: respx.MockRouter):
        """Test pending file state when polling times out."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_REPORT_SPEC_RESPONSE)
        )
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(return_value=httpx.Response(200, json=REPORT_RUN_RESPONSE))
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json=[{"fileID": "file-pending-1", "reportID": 11026, "status": "queued"}],
            )
        )

        result = await ix_run_marketplace_draft_report(
            account_id=100,
            report_title="Marketplace Draft Timeout",
            date_range={"previous": {"days": 1}},
            poll_timeout_seconds=0,
        )

        assert result["success"] is True
        assert result["download_pending"] is True
        assert "warning" in result

    @pytest.mark.asyncio
    async def test_run_marketplace_draft_report_downloads_when_download_status_is_new(
        self, mock_ix_api: respx.MockRouter, tmp_path
    ):
        """Test Index file responses that use downloadStatus instead of status."""
        import indexexchange_mcp

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_SPECS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})
        )
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(return_value=httpx.Response(200, json=REPORT_RUN_RESPONSE))
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json=[
                    {
                        "fileID": "file-new-123",
                        "reportID": 11026,
                        "downloadStatus": "new",
                        "fileName": "marketplace_draft.csv.zip",
                    }
                ],
            )
        )

        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-new-123"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(200, content=b"zip-bytes", headers={"content-type": "application/zip"})
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_run_marketplace_draft_report(
            account_id="Elcano",
            report_title="Marketplace Draft New Download Status",
            date_range={"previous": {"days": 1}},
            preset="deal_labels",
            file_type="csv.zip",
        )

        assert result["success"] is True
        assert result["report_file"]["fileID"] == "file-new-123"
        assert result["download"]["success"] is True


class TestRunUpdatedMarketplaceDraftReport:
    """Tests for update -> run -> download Marketplace draft workflow."""

    @pytest.mark.asyncio
    async def test_run_updated_marketplace_draft_report_with_download(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test update -> run -> list files -> download workflow."""
        import indexexchange_mcp

        update_request = None
        run_request = None

        def capture_update(request: httpx.Request) -> httpx.Response:
            nonlocal update_request
            update_request = request
            return httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})

        def capture_run(request: httpx.Request) -> httpx.Response:
            nonlocal run_request
            run_request = request
            return httpx.Response(200, json=REPORT_RUN_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.patch(IX_REPORT_SPEC_UPDATE_ENDPOINT).mock(side_effect=capture_update)
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(side_effect=capture_run)
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(200, json=REPORT_FILES_RESPONSE)
        )

        csv_content = b"deal_id,deal_name,marketplace_media_spend\nabc,Updated Deal,99.99\n"
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-abc-123"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(200, content=csv_content, headers={"content-type": "text/csv"})
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_run_updated_marketplace_draft_report(
            report_id=11026,
            account_id="Stirista",
            report_title="Stirista Updated Draft",
            date_range={"previous": {"days": 1}},
            preset="deal_summary",
            extra_fields=["external_reference_id"],
            file_type="csv",
        )

        assert result["success"] is True
        assert result["account_id"] == 1503605
        assert result["report_spec"]["reportStatus"] == "draft"
        assert result["report_run"]["reportRunID"] == 55001
        assert result["report_file"]["fileID"] == "file-abc-123"
        assert result["download"]["success"] is True

        update_payload = json.loads(update_request.content)
        assert update_payload["accounts"] == [1503605]
        assert update_payload["reportStatus"] == "draft"
        assert update_payload["fileType"] == "csv"
        assert update_payload["dateRange"] == {"previous": {"days": 1}}

        run_payload = json.loads(run_request.content)
        assert run_payload["reportID"] == 11026
        assert run_payload["reportStatus"] == "draft"

    @pytest.mark.asyncio
    async def test_run_updated_marketplace_draft_report_timeout_returns_pending(self, mock_ix_api: respx.MockRouter):
        """Test pending file state when update-run polling times out."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.patch(IX_REPORT_SPEC_UPDATE_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"reportSpecID": 11026, "reportStatus": "draft"})
        )
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(return_value=httpx.Response(200, json=REPORT_RUN_RESPONSE))
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json=[{"fileID": "file-pending-1", "reportID": 11026, "status": "queued"}],
            )
        )

        result = await ix_run_updated_marketplace_draft_report(
            report_id=11026,
            account_id="Reklaim",
            report_title="Reklaim Updated Draft",
            date_range={"current": "month"},
            poll_timeout_seconds=0,
        )

        assert result["success"] is True
        assert result["account_id"] == 1485234
        assert result["download_pending"] is True
        assert "warning" in result


class TestReportingHealthcheck:
    """Tests for ix_reporting_healthcheck tool."""

    @pytest.mark.asyncio
    async def test_healthcheck_success(self, mock_ix_api: respx.MockRouter):
        """Test reporting healthcheck authenticates and hits reporting endpoint."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_SPECS_INFO_ENDPOINT).mock(
            return_value=httpx.Response(200, json=ACTIVE_REPORTS_RESPONSE)
        )

        result = await ix_reporting_healthcheck()

        assert result["success"] is True
        assert result["configured"] is True
        assert result["auth_mode"] == "user_account"
        assert result["token_cached"] is True
        assert result["token_valid"] is True
        assert result["report_count"] == len(ACTIVE_REPORTS_RESPONSE)
        assert result["reports"] == ACTIVE_REPORTS_RESPONSE

    @pytest.mark.asyncio
    async def test_healthcheck_passes_filters(self, mock_ix_api: respx.MockRouter):
        """Test reporting healthcheck forwards optional reporting filters."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=ACTIVE_REPORTS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_SPECS_INFO_ENDPOINT).mock(side_effect=capture)

        result = await ix_reporting_healthcheck(
            account_ids=[100, 200],
            account_group_ids=[10],
            report_status="draft",
        )

        assert result["success"] is True
        url_str = str(captured_request.url)
        assert "accountIDs=" in url_str
        assert "accountGroupIDs=10" in url_str
        assert "reportStatus=draft" in url_str

    @pytest.mark.asyncio
    async def test_healthcheck_returns_error(self, mock_ix_api: respx.MockRouter):
        """Test reporting healthcheck surfaces reporting auth/access failures."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_SPECS_INFO_ENDPOINT).mock(
            return_value=httpx.Response(403, json={"message": "forbidden"})
        )

        result = await ix_reporting_healthcheck()

        assert result["success"] is False
        assert result["error"]["operation"] == "reporting_healthcheck"
        assert "HTTP 403" in result["error"]["message"]


class TestListActiveReports:
    """Tests for ix_list_active_reports tool."""

    @pytest.mark.asyncio
    async def test_list_reports_success(self, mock_ix_api: respx.MockRouter):
        """Test listing active reports."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_SPECS_INFO_ENDPOINT).mock(
            return_value=httpx.Response(200, json=ACTIVE_REPORTS_RESPONSE)
        )

        result = await ix_list_active_reports()
        assert result["success"] is True
        assert len(result["reports"]) == 2

    @pytest.mark.asyncio
    async def test_list_reports_with_filters(self, mock_ix_api: respx.MockRouter):
        """Test listing reports with query filters."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=ACTIVE_REPORTS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_SPECS_INFO_ENDPOINT).mock(side_effect=capture)

        result = await ix_list_active_reports(
            account_ids=[100, 200],
            account_group_ids=[10],
            report_status="saved",
        )
        assert result["success"] is True

        url_str = str(captured_request.url)
        assert "accountIDs=" in url_str
        assert "accountGroupIDs=10" in url_str
        assert "reportStatus=saved" in url_str


class TestRunReportDownload:
    """Tests for ix_run_report_download tool."""

    @pytest.mark.asyncio
    async def test_run_report_success(self, mock_ix_api: respx.MockRouter):
        """Test running a report returns reportRunID."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=REPORT_RUN_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(side_effect=capture)

        result = await ix_run_report_download(report_id=11026)
        assert result["success"] is True
        assert result["report_run"]["reportRunID"] == 55001

        payload = json.loads(captured_request.content)
        assert payload["reportID"] == 11026

    @pytest.mark.asyncio
    async def test_run_report_with_status(self, mock_ix_api: respx.MockRouter):
        """Test running a report with explicit status."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=REPORT_RUN_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.post(IX_REPORT_RUNS_ENDPOINT).mock(side_effect=capture)

        result = await ix_run_report_download(report_id=11026, report_status="saved")
        assert result["success"] is True

        payload = json.loads(captured_request.content)
        assert payload["reportStatus"] == "saved"


class TestListReportFiles:
    """Tests for ix_list_report_files tool."""

    @pytest.mark.asyncio
    async def test_list_files_success(self, mock_ix_api: respx.MockRouter):
        """Test listing report files."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(
            return_value=httpx.Response(200, json=REPORT_FILES_RESPONSE)
        )

        result = await ix_list_report_files()
        assert result["success"] is True
        assert len(result["files"]) == 2

    @pytest.mark.asyncio
    async def test_list_files_with_filters(self, mock_ix_api: respx.MockRouter):
        """Test listing files with comma-separated query params."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=REPORT_FILES_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_REPORT_FILES_LIST_ENDPOINT).mock(side_effect=capture)

        result = await ix_list_report_files(
            account_ids=[100, 200],
            status="completed",
            file_ids=["file-abc-123", "file-def-456"],
            report_ids=["11026", "11027"],
        )
        assert result["success"] is True

        url_str = str(captured_request.url)
        assert "accountIDs=" in url_str
        assert "status=completed" in url_str
        assert "fileIDs=" in url_str
        assert "reportIDs=" in url_str


class TestDownloadReportFile:
    """Tests for ix_download_report_file tool."""

    @pytest.mark.asyncio
    async def test_download_csv_file(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test downloading a CSV report file saves to disk."""
        import indexexchange_mcp

        # Use monkeypatch-equivalent: set download dir on client
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        csv_content = b"date,impressions,revenue\n2024-06-01,1000,50.00\n"
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-abc-123"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(
                200,
                content=csv_content,
                headers={"content-type": "text/csv"},
            )
        )

        # Override download dir
        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_download_report_file(file_id="file-abc-123")
        assert result["success"] is True
        assert result["bytes"] == len(csv_content)
        assert result["content_type"] == "text/csv"
        assert result["sha256"]  # Non-empty hash
        assert ".csv" in result["path"]

        # Verify file exists on disk
        from pathlib import Path

        assert Path(result["path"]).exists()
        assert Path(result["path"]).read_bytes() == csv_content

    @pytest.mark.asyncio
    async def test_download_with_filename_hint(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test download uses filename_hint for extension."""
        import indexexchange_mcp

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        content = b"binary data"
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-xyz"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(
                200,
                content=content,
                headers={"content-type": "application/octet-stream"},
            )
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_download_report_file(file_id="file-xyz", filename_hint="report.csv.gz")
        assert result["success"] is True
        assert ".csv.gz" in result["path"]

    @pytest.mark.asyncio
    async def test_download_default_extension(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test download defaults to .bin for unknown content type."""
        import indexexchange_mcp

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        content = b"\x00\x01\x02"
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-bin"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(
                200,
                content=content,
                headers={"content-type": "application/octet-stream"},
            )
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_download_report_file(file_id="file-bin")
        assert result["success"] is True
        assert result["path"].endswith(".bin")

    @pytest.mark.asyncio
    async def test_download_sha256_hash(self, mock_ix_api: respx.MockRouter, tmp_path):
        """Test that SHA256 hash is correctly computed."""
        import hashlib

        import indexexchange_mcp

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        content = b"test content for hashing"
        expected_hash = hashlib.sha256(content).hexdigest()
        download_url = f"{IX_BASE_URL}/api/reporting/agg/v1/report-files/download/file-hash"
        mock_ix_api.get(download_url).mock(
            return_value=httpx.Response(
                200,
                content=content,
                headers={"content-type": "text/csv"},
            )
        )

        client = indexexchange_mcp.get_ix_client()
        client._download_dir = str(tmp_path)

        result = await ix_download_report_file(file_id="file-hash")
        assert result["success"] is True
        assert result["sha256"] == expected_hash
