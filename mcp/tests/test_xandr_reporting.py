import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import xandr_mcp


class TestXandrReporting:
    def test_normalize_xandr_download_content_type_prefers_csv_for_csv_bytes(self):
        content_type = xandr_mcp._normalize_xandr_download_content_type(
            b"day,curated_deal,imps\n2026-04-01,Deal A,10\n", "text/html; charset=UTF-8"
        )

        assert content_type == "text/csv"

    @pytest.mark.asyncio
    async def test_reporting_healthcheck_uses_metadata_endpoint(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def get_report_metadata(self, report_type: str | None = None):
                assert report_type == "curator_analytics"
                return {
                    "response": {
                        "meta": {
                            "time_granularity": "daily",
                            "columns": [{"column": "day"}, {"column": "curated_deal"}],
                            "filters": [{"column": "member_id"}],
                            "time_intervals": ["today", "last_month"],
                        }
                    }
                }

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())

        result = await xandr_mcp.xandr_reporting_healthcheck()

        assert result["success"] is True
        assert result["has_metadata"] is True
        assert result["column_count"] == 2

    @pytest.mark.asyncio
    async def test_run_report_polls_then_downloads(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        class FakeClient:
            async def request_report(self, payload):
                assert payload["report"]["report_type"] == "curator_analytics"
                return {"response": {"report_id": "report-123"}}

            async def get_report_status(self, report_id: str):
                assert report_id == "report-123"
                return {
                    "response": {
                        "execution_status": "ready",
                        "report": {"row_count": "2", "url": "report-download?id=report-123"},
                    }
                }

            async def download_report(self, report_id: str):
                assert report_id == "report-123"
                return b"day,curated_deal,imps\n2026-04-01,Deal A,10\n", "text/csv"

        monkeypatch.setattr(xandr_mcp, "get_xandr_client", lambda: FakeClient())

        result = await xandr_mcp.xandr_run_report(
            report_type="curator_analytics",
            columns=["day", "curated_deal", "imps"],
            report_interval="today",
            filename_hint="xandr_curator_report.csv",
            output_dir=str(tmp_path),
        )

        assert result["success"] is True
        assert result["report_id"] == "report-123"
        assert result["download"]["path"].endswith("xandr_curator_report.csv")
        assert (tmp_path / "xandr_curator_report.csv").exists()

    @pytest.mark.asyncio
    async def test_run_curator_report_uses_preset_defaults(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_run_report(**kwargs):
            assert kwargs["report_type"] == "curator_analytics"
            assert kwargs["columns"] == [
                "day",
                "curated_deal",
                "buyer_member_name",
                "imps",
                "curator_revenue",
                "curator_margin",
            ]
            return {"success": True, "report_id": "abc"}

        monkeypatch.setattr(xandr_mcp, "xandr_run_report", fake_run_report)

        result = await xandr_mcp.xandr_run_curator_report()

        assert result["success"] is True
        assert result["preset"] == "curator_revenue_summary"

    @pytest.mark.asyncio
    async def test_run_curator_report_from_prompt_inputs_maps_human_terms(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_run_report(**kwargs):
            assert kwargs["report_type"] == "curator_analytics"
            assert kwargs["columns"] == [
                "day",
                "curated_deal",
                "buyer_member_name",
                "imps",
                "curator_revenue",
                "curator_tech_fees",
            ]
            return {"success": True, "report_id": "abc123"}

        monkeypatch.setattr(xandr_mcp, "xandr_run_report", fake_run_report)

        result = await xandr_mcp.xandr_run_curator_report_from_prompt_inputs(
            breakdowns=["day", "deal", "buyer"],
            metrics=["impressions", "spend", "total marketplace fees"],
            last_n_days=30,
        )

        assert result["success"] is True
        assert result["resolved_breakdowns"] == ["day", "curated_deal", "buyer_member_name"]
        assert result["resolved_metrics"] == ["imps", "curator_revenue", "curator_tech_fees"]
