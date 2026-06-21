import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import pubmatic_mcp


class TestPubMaticReportingHelpers:
    def test_resolve_pubmatic_report_account_id_accepts_known_account_name(self):
        assert pubmatic_mcp._resolve_pubmatic_report_account_id("Elcano") == 60067

    def test_flatten_pubmatic_analytics_rows_adds_display_columns(self):
        columns, rows = pubmatic_mcp._flatten_pubmatic_analytics_rows(
            {
                "columns": ["date", "dealMetaId", "dspId", "revenue"],
                "rows": [["2026-04-01", "123", "9", 42.5]],
                "displayValue": {
                    "dealMetaId": {"123": "Deal Alpha"},
                    "dspId": {"9": "The Trade Desk"},
                },
            }
        )

        assert columns == ["date", "dealMetaId", "dspId", "revenue", "dealMetaId_display", "dspId_display"]
        assert rows == [
            {
                "date": "2026-04-01",
                "dealMetaId": "123",
                "dspId": "9",
                "revenue": 42.5,
                "dealMetaId_display": "Deal Alpha",
                "dspId_display": "The Trade Desk",
            }
        ]


class TestPubMaticReportingTools:
    @pytest.mark.asyncio
    async def test_reporting_healthcheck_uses_standard_analytics_query(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def query_standard_analytics(self, account_id: int, **kwargs):
                assert account_id == 60067
                assert kwargs["metrics"] == ["spend", "paidImpressions"]
                assert kwargs["dimensions"] == ["date"]
                return {
                    "columns": ["date", "spend", "paidImpressions"],
                    "rows": [["2026-04-21", 12.3, 45]],
                    "alert": None,
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        result = await pubmatic_mcp.pm_reporting_healthcheck(account_id="Elcano")

        assert result["success"] is True
        assert result["account_id"] == 60067
        assert result["row_count"] == 1
        assert result["sample_rows"][0]["spend"] == 12.3

    @pytest.mark.asyncio
    async def test_run_preset_report_downloads_csv(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        class FakeClient:
            async def query_standard_analytics(self, account_id: int, **kwargs):
                assert account_id == 60067
                assert kwargs["dimensions"] == ["date", "dealMetaId", "dspId"]
                assert kwargs["metrics"] == ["paidImpressions", "spend", "ecpm"]
                return {
                    "columns": ["date", "dealMetaId", "dspId", "paidImpressions", "spend", "ecpm"],
                    "rows": [["2026-04-01", "123", "9", 1000, 25.5, 1.2]],
                    "displayValue": {
                        "dealMetaId": {"123": "Deal Alpha"},
                        "dspId": {"9": "The Trade Desk"},
                    },
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        result = await pubmatic_mcp.pm_run_preset_report(
            account_id="Elcano",
            date_range={"previous": {"days": 7}},
            preset="deal_summary",
            download=True,
            filename_hint="pubmatic_deal_summary_test.csv",
            output_dir=str(tmp_path),
        )

        assert result["success"] is True
        assert result["preset"] == "deal_summary"
        assert result["row_count"] == 1
        assert result["sample_rows"][0]["dealMetaId_display"] == "Deal Alpha"
        assert result["rows_truncated"] is False
        assert result["download"]["success"] is True
        assert result["download"]["path"].endswith("pubmatic_deal_summary_test.csv")
        assert (tmp_path / "pubmatic_deal_summary_test.csv").exists()

    @pytest.mark.asyncio
    async def test_run_preset_report_rejects_unknown_preset(self):
        result = await pubmatic_mcp.pm_run_preset_report(
            account_id="Elcano",
            date_range={"previous": {"days": 7}},
            preset="unknown",
        )

        assert result["success"] is False
        assert "Unknown preset" in result["error"]

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_resolves_human_terms(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        class FakeClient:
            async def query_standard_analytics(self, account_id: int, **kwargs):
                assert account_id == 60067
                assert kwargs["dimensions"] == ["date", "dealMetaId", "dspId"]
                assert kwargs["metrics"] == ["paidImpressions", "spend", "ecpm"]
                return {
                    "columns": ["date", "dealMetaId", "dspId", "paidImpressions", "spend", "ecpm"],
                    "rows": [["2026-04-01", "123", "9", 1000, 25.5, 1.2]],
                    "displayValue": {
                        "dealMetaId": {"123": "Deal Alpha"},
                        "dspId": {"9": "The Trade Desk"},
                    },
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        result = await pubmatic_mcp.pm_run_report_from_prompt_inputs(
            account_id="Elcano",
            date_range={"previous": {"days": 30}},
            breakdowns=["day", "deal", "DSP"],
            metrics=["impressions", "spend", "eCPM"],
            filename_hint="pubmatic_prompt_test.csv",
            output_dir=str(tmp_path),
        )

        assert result["success"] is True
        assert result["resolved_breakdowns"] == ["date", "dealMetaId", "dspId"]
        assert result["resolved_metrics"] == ["paidImpressions", "spend", "ecpm"]
        assert result["warnings"] == []
        assert result["download"]["path"].endswith("pubmatic_prompt_test.csv")

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_maps_marketplace_fees(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def query_standard_analytics(self, account_id: int, **kwargs):
                assert account_id == 60067
                assert kwargs["metrics"] == ["paidImpressions", "spend", "transactionRevenue"]
                return {
                    "columns": ["date", "paidImpressions", "spend", "transactionRevenue"],
                    "rows": [["2026-04-01", 1000, 25.5, 3.1]],
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        result = await pubmatic_mcp.pm_run_report_from_prompt_inputs(
            account_id="Elcano",
            date_range={"previous": {"days": 30}},
            breakdowns=["day"],
            metrics=["impressions", "spend", "total marketplace fees"],
            download=False,
        )

        assert result["success"] is True
        assert result["resolved_metrics"] == ["paidImpressions", "spend", "transactionRevenue"]

    @pytest.mark.asyncio
    async def test_run_standard_report_truncates_large_rows_by_default(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def query_standard_analytics(self, account_id: int, **kwargs):  # noqa: ARG002
                return {
                    "columns": ["date", "paidImpressions"],
                    "rows": [[f"2026-04-{idx:02d}", idx] for idx in range(1, 31)],
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        result = await pubmatic_mcp.pm_run_standard_report(
            account_id="Elcano",
            date_range={"previous": {"days": 30}},
            dimensions=["date"],
            metrics=["paidImpressions"],
            download=False,
        )

        assert result["success"] is True
        assert result["row_count"] == 30
        assert len(result["sample_rows"]) == 25
        assert result["rows_truncated"] is True
        assert "rows" not in result

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_uses_report_type_preset(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_run_preset_report(**kwargs):
            assert kwargs["preset"] == "deal_summary"
            assert kwargs["account_id"] == "Elcano"
            return {"success": True, "preset": "deal_summary", "row_count": 0}

        monkeypatch.setattr(pubmatic_mcp, "pm_run_preset_report", fake_run_preset_report)

        result = await pubmatic_mcp.pm_run_report_from_prompt_inputs(
            account_id="Elcano",
            date_range={"previous": {"days": 30}},
            report_type="deal performance",
        )

        assert result["success"] is True
        assert result["preset"] == "deal_summary"
