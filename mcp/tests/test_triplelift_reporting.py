import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import triplelift_mcp


class TestTripleLiftReporting:
    @pytest.mark.asyncio
    async def test_reporting_auth_status_uses_direct_credentials(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            def _is_configured(self):
                return True

            async def _ensure_auth(self):
                return None

        monkeypatch.setattr(triplelift_mcp, "get_triplelift_reporting_client", lambda: FakeClient())

        result = await triplelift_mcp.tl_reporting_auth_status()

        assert result["success"] if "success" in result else result["authenticated"] is True

    @pytest.mark.asyncio
    async def test_advertiser_deals_report_builds_graphql_query(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def graphql(self, query: str, variables: dict):
                assert "advertiserDealsReport" in query
                assert "AdvertiserDealsDimensionField" in query
                assert "AdvertiserDealsMetricField" in query
                assert variables["dealMemberId"] == "9404"
                assert variables["dimensions"] == ["YMD", "DEAL_NAME"]
                assert variables["metrics"] == ["DEAL_SPEND", "IMPRESSIONS"]
                return {"data": {"advertiserDealsReport": {"rows": [], "nextCursor": None, "totalRows": 0}}}

        monkeypatch.setattr(triplelift_mcp, "get_triplelift_reporting_client", lambda: FakeClient())

        result = await triplelift_mcp.tl_advertiser_deals_report(
            deal_member_id="9404",
            start_date="2026-01-01",
            end_date="2026-01-01",
            dimensions=["YMD", "DEAL_NAME"],
            metrics=["DEAL_SPEND", "IMPRESSIONS"],
        )

        assert result["success"] is True

    @pytest.mark.asyncio
    async def test_async_download_advertiser_report_uses_buyer_member_id(self, monkeypatch: pytest.MonkeyPatch):
        captured: dict = {}

        class FakeClient:
            async def graphql(self, query: str, variables: dict):
                captured["query"] = query
                captured["variables"] = variables
                return {"data": {"asyncDownloadAdvertiserReport": "https://example.com/report.csv"}}

        monkeypatch.setattr(triplelift_mcp, "get_triplelift_reporting_client", lambda: FakeClient())

        result = await triplelift_mcp.tl_async_download_advertiser_report(
            buyer_member_id="9404",
            start_date="2026-01-01",
            end_date="2026-01-31",
            metrics=["DEAL_SPEND", "BID_REQUESTS"],
        )

        assert result["success"] is True
        assert "asyncDownloadAdvertiserReport" in captured["query"]
        assert captured["variables"]["buyerMemberId"] == "9404"
        assert captured["variables"]["metrics"] == ["DEAL_SPEND", "BID_REQUESTS"]
        # The async surface uses AdvertiserMetricField (not AdvertiserDealsMetricField)
        assert "AdvertiserMetricField" in captured["query"]
        # sortFields and useThreshold defaults
        assert captured["variables"]["sortFields"] == []
        assert captured["variables"]["useThreshold"] is False

    @pytest.mark.asyncio
    async def test_run_report_from_prompt_inputs_maps_human_terms_to_advertiser_enums(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path
    ):
        captured_variables: list[dict] = []

        async def fake_report(**kwargs):
            captured_variables.append(kwargs)
            return {
                "success": True,
                "data": {
                    "advertiserDealsReport": {
                        "rows": [
                            {
                                "dimensions": [
                                    {"name": "ymd", "value": "2026-01-01"},
                                    {"name": "deal_name", "value": "Elcano_Test"},
                                    {"name": "deal_id", "value": "1001"},
                                ],
                                "metrics": [
                                    {"name": "deal_spend", "decimalValue": 12.34},
                                    {"name": "impressions", "longValue": 1000},
                                    {"name": "bid_requests", "longValue": 99999},
                                ],
                            }
                        ],
                        "nextCursor": None,
                        "totalRows": 1,
                    }
                },
            }

        monkeypatch.setattr(triplelift_mcp, "tl_advertiser_deals_report", fake_report)
        monkeypatch.setenv("TRIPLELIFT_REPORT_DOWNLOAD_DIR", str(tmp_path))

        result = await triplelift_mcp.tl_run_report_from_prompt_inputs(
            deal_member_id="9404",
            start_date="2026-01-01",
            end_date="2026-01-31",
            breakdowns=["day", "deal", "deal id"],
            metrics=["spend", "impressions", "bid requests"],
            filename_hint="tl_prompt_test.csv",
        )

        assert result["success"] is True
        assert result["resolved_breakdowns"] == ["YMD", "DEAL_NAME", "DEAL_ID"]
        assert result["resolved_metrics"] == ["DEAL_SPEND", "IMPRESSIONS", "BID_REQUESTS"]
        assert result["row_count"] == 1
        assert result["total_rows"] == 1
        assert result["download"]["bytes"] > 0
        # First and only page invocation used the resolved enums against the new tool
        assert captured_variables[0]["deal_member_id"] == "9404"
        assert captured_variables[0]["dimensions"] == ["YMD", "DEAL_NAME", "DEAL_ID"]
        assert captured_variables[0]["metrics"] == ["DEAL_SPEND", "IMPRESSIONS", "BID_REQUESTS"]

    @pytest.mark.asyncio
    async def test_run_report_paginates_via_next_cursor(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        call_count = 0

        async def fake_report(**kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                return {
                    "success": True,
                    "data": {
                        "advertiserDealsReport": {
                            "rows": [{"dimensions": [], "metrics": []}],
                            "nextCursor": "page-2",
                            "totalRows": 2,
                        }
                    },
                }
            return {
                "success": True,
                "data": {
                    "advertiserDealsReport": {
                        "rows": [{"dimensions": [], "metrics": []}],
                        "nextCursor": None,
                        "totalRows": 2,
                    }
                },
            }

        monkeypatch.setattr(triplelift_mcp, "tl_advertiser_deals_report", fake_report)
        monkeypatch.setenv("TRIPLELIFT_REPORT_DOWNLOAD_DIR", str(tmp_path))

        result = await triplelift_mcp.tl_run_report_from_prompt_inputs(
            deal_member_id="9404",
            start_date="2026-01-01",
            end_date="2026-01-31",
        )

        assert result["success"] is True
        assert result["pages_fetched"] == 2
        assert result["row_count"] == 2
