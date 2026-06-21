import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import medianet_mcp


class TestMediaNetReporting:
    @pytest.mark.asyncio
    async def test_reporting_healthcheck_lists_views(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_views(self):
                return [{"viewId": 123, "viewName": "Deals", "viewDesc": "Deal reporting"}]

        monkeypatch.setattr(medianet_mcp, "get_medianet_reporting_client", lambda: FakeClient())

        result = await medianet_mcp.mn_reporting_healthcheck()

        assert result["success"] is True
        assert result["view_count"] == 1

    @pytest.mark.asyncio
    async def test_fetch_report_data_uses_reporting_payload(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def fetch_data(self, payload):
                assert payload["viewId"] == 1045
                assert payload["dimensions"] == ["deal_name"]
                assert payload["metrics"] == ["avails"]
                return {
                    "rows": [{"deal_name": "Sample Deal", "avails": 100}],
                    "headers": [{"apiName": "deal_name", "type": "dimension"}],
                    "totalData": {"avails": 100},
                }

        monkeypatch.setattr(medianet_mcp, "get_medianet_reporting_client", lambda: FakeClient())

        result = await medianet_mcp.mn_fetch_report_data(
            view_id=1045,
            start_date_time="2025-09-01T00:00",
            end_date_time="2025-09-02T02:59",
            threshold=10,
            dimensions=["deal_name"],
            metrics=["avails"],
            sort_by_metric={"by": "avails", "order": "DESC"},
        )

        assert result["success"] is True
        assert result["rows"][0]["avails"] == 100

    @pytest.mark.asyncio
    async def test_queue_report_data_polls_then_downloads(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        class FakeClient:
            async def submit_queue_data(self, payload):
                assert payload["viewId"] == 123
                return {
                    "queueId": "SAMP_REQ_123",
                    "headers": [{"apiName": "deal_name", "type": "dimension"}],
                }

            async def get_queue_progress(self, queue_id: str):
                assert queue_id == "SAMP_REQ_123"
                return {"completionPercentage": 100.0, "queueStatus": "SUCCEEDED"}

            async def download_queue_data(self, queue_id: str):
                assert queue_id == "SAMP_REQ_123"
                return b"deal_name,avails\nSample Deal,100\n", "text/csv"

        monkeypatch.setattr(medianet_mcp, "get_medianet_reporting_client", lambda: FakeClient())

        result = await medianet_mcp.mn_queue_report_data(
            view_id=123,
            start_date_time="2025-09-01T00:00",
            end_date_time="2025-09-02T18:59",
            dimensions=["deal_name"],
            metrics=["avails"],
            sort_by_metric={"by": "avails", "order": "DESC"},
            filename_hint="medianet_report.csv",
            output_dir=str(tmp_path),
        )

        assert result["success"] is True
        assert result["queueId"] == "SAMP_REQ_123"
        assert result["download"]["path"].endswith("medianet_report.csv")
        assert (tmp_path / "medianet_report.csv").exists()

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_resolves_human_terms(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_queue_report_data(**kwargs):
            assert kwargs["view_id"] == 1045
            assert kwargs["dimensions"] == ["day", "deal_name", "dsp"]
            assert kwargs["metrics"] == ["ad_impressions", "advertiser_spend", "deal_margin"]
            assert kwargs["sort_by_metric"] == {"by": "ad_impressions", "order": "DESC"}
            return {"success": True, "queueId": "SAMP_REQ_456"}

        monkeypatch.setattr(medianet_mcp, "mn_queue_report_data", fake_queue_report_data)

        result = await medianet_mcp.mn_run_report_from_prompt_inputs(
            breakdowns=["day", "deal", "DSP"],
            metrics=["impressions", "spend", "margin"],
            queue=True,
        )

        assert result["success"] is True
        assert result["resolved_breakdowns"] == ["day", "deal_name", "dsp"]
        assert result["resolved_metrics"] == ["ad_impressions", "advertiser_spend", "deal_margin"]

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_default_window_is_relative(self, monkeypatch: pytest.MonkeyPatch):
        """Omitting dates must yield a trailing window anchored to *now*, not a
        hardcoded literal that silently goes stale (the old default was a fixed
        September 2025 window)."""
        from datetime import UTC, datetime, timedelta

        seen: dict = {}

        async def fake_queue_report_data(**kwargs):
            seen.update(kwargs)
            return {"success": True, "queueId": "SAMP_REQ_REL"}

        monkeypatch.setattr(medianet_mcp, "mn_queue_report_data", fake_queue_report_data)

        result = await medianet_mcp.mn_run_report_from_prompt_inputs(queue=True)

        assert result["success"] is True
        start = datetime.strptime(seen["start_date_time"], "%Y-%m-%dT%H:%M").replace(tzinfo=UTC)
        end = datetime.strptime(seen["end_date_time"], "%Y-%m-%dT%H:%M").replace(tzinfo=UTC)
        now = datetime.now(UTC)
        assert abs((now - end).total_seconds()) < 600, "default end must be ~now"
        assert timedelta(days=29) < (end - start) < timedelta(days=31), "default window must be ~30 days"

    @pytest.mark.parametrize(
        ("value", "expected"),
        [
            ("2026-05-26", "2026-05-26T00:00"),
            ("2026-05-26 00:00:00", "2026-05-26T00:00"),
            ("2026-05-26T00:00", "2026-05-26T00:00"),
            ("2026-06-01T23:59:59", "2026-06-01T23:59"),
            ("2026-06-01T23:59:59Z", "2026-06-01T23:59"),
        ],
    )
    def test_normalize_datetime_coerces_common_formats(self, value: str, expected: str):
        assert medianet_mcp._normalize_medianet_datetime(value, "start_date_time") == expected

    def test_normalize_datetime_rejects_garbage(self):
        with pytest.raises(ValueError, match="start_date_time"):
            medianet_mcp._normalize_medianet_datetime("not-a-date", "start_date_time")

    @pytest.mark.asyncio
    async def test_fetch_report_data_normalizes_loose_dates(self, monkeypatch: pytest.MonkeyPatch):
        seen: dict = {}

        class FakeClient:
            async def fetch_data(self, payload):
                seen.update(payload)
                return {"rows": [], "headers": [], "totalData": {}}

        monkeypatch.setattr(medianet_mcp, "get_medianet_reporting_client", lambda: FakeClient())

        result = await medianet_mcp.mn_fetch_report_data(
            view_id=1045,
            start_date_time="2026-05-26 00:00:00",
            end_date_time="2026-06-01",
            threshold=10,
            dimensions=["dsp"],
            metrics=["advertiser_spend"],
            sort_by_metric={"by": "advertiser_spend", "order": "DESC"},
        )

        assert result["success"] is True
        assert seen["startDateTime"] == "2026-05-26T00:00"
        assert seen["endDateTime"] == "2026-06-01T00:00"
