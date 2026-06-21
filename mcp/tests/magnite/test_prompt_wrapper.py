import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import magnite_mcp


class TestMagnitePromptWrapper:
    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_maps_human_terms(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_create_offline_report(**kwargs):
            assert kwargs["dimensions"] == ["date", "marketplace_deal_name", "partner"]
            assert kwargs["metrics"] == ["paid_impression", "buyer_spend", "curator_rev_share"]
            return {"success": True, "offline_report_id": 456, "status": "queued"}

        async def fake_check_report_status(report_id: int):
            assert report_id == 456
            return {"success": True, "offline_report_id": 456, "status": "success"}

        async def fake_download_report(**kwargs):
            assert kwargs["report_id"] == 456
            assert kwargs["format"] == "csv"
            return {"success": True, "path": "/tmp/magnite_report_456.csv"}

        monkeypatch.setattr(magnite_mcp, "magnite_create_offline_report", fake_create_offline_report)
        monkeypatch.setattr(magnite_mcp, "magnite_check_report_status", fake_check_report_status)
        monkeypatch.setattr(magnite_mcp, "magnite_download_report", fake_download_report)

        result = await magnite_mcp.magnite_run_report_from_prompt_inputs(
            breakdowns=["day", "deal", "DSP"],
            metrics=["impressions", "spend", "margin"],
            date_range="last_3",
            filename_hint="magnite_prompt_test.csv",
        )

        assert result["success"] is True
        assert result["resolved_breakdowns"] == ["date", "marketplace_deal_name", "partner"]
        assert result["resolved_metrics"] == ["paid_impression", "buyer_spend", "curator_rev_share"]
        assert result["download"]["path"] == "/tmp/magnite_report_456.csv"

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_polls_until_success(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_create_offline_report(**kwargs):
            return {"success": True, "offline_report_id": 789, "status": "queued"}

        statuses = iter(
            [
                {"success": True, "offline_report_id": 789, "status": "queued"},
                {"success": True, "offline_report_id": 789, "status": "success"},
            ]
        )

        async def fake_check_report_status(report_id: int):
            assert report_id == 789
            return next(statuses)

        async def fake_download_report(**kwargs):
            assert kwargs["report_id"] == 789
            return {"success": True, "path": "/tmp/magnite_report_789.csv"}

        monkeypatch.setattr(magnite_mcp, "magnite_create_offline_report", fake_create_offline_report)
        monkeypatch.setattr(magnite_mcp, "magnite_check_report_status", fake_check_report_status)
        monkeypatch.setattr(magnite_mcp, "magnite_download_report", fake_download_report)

        result = await magnite_mcp.magnite_run_report_from_prompt_inputs(
            breakdowns=["day"],
            metrics=["impressions"],
            date_range="last_3",
            poll_timeout_seconds=0.1,
            poll_interval_seconds=0.01,
        )

        assert result["success"] is True
        assert result["offline_report_id"] == 789
