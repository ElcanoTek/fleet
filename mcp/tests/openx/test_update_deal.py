"""
Tests for the ox_update_deal / ox_archive_deal MCP tools and the
full-targeting dealById selection used for read-modify-write updates.
"""

import json
import os
import sys
from typing import Any

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import openx_mcp
from openx_mcp import ox_get_deal, ox_update_deal

from .conftest import OPENX_GRAPHQL_ENDPOINT

UPDATED_DEAL = {
    "id": "deal-001",
    "deal_id": "OX-bef-4c7eFf",
    "name": "My Newer Deal",
    "status": "Active",
    "currency": "USD",
    "deal_price": "2.50",
    "pmp_deal_type": "3",
    "start_date": "2026-06-01 00:00:00",
    "end_date": "2028-06-01 00:00:00",
    "modified_date": "2026-06-11 12:00:00",
    "package": {"uid": "pkg-1", "modified_date": "2026-06-11 12:00:00"},
}


def _graphql_capture(mock: respx.MockRouter, data: dict[str, Any]) -> list[httpx.Request]:
    """Mock the GraphQL endpoint, capturing each request for assertions."""
    captured: list[httpx.Request] = []

    def responder(request: httpx.Request) -> httpx.Response:
        captured.append(request)
        return httpx.Response(200, json={"data": data})

    mock.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=responder)
    return captured


def _request_body(request: httpx.Request) -> dict[str, Any]:
    return json.loads(request.content)


class TestUpdateDeal:
    """Tests for the ox_update_deal tool."""

    @pytest.mark.asyncio
    async def test_partial_update_sends_only_provided_fields(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})

        result = await ox_update_deal(deal_id="deal-001", name="My Newer Deal", deal_price=2.5)

        assert result["success"] is True
        assert result["updated_fields"] == ["deal_price", "name"]
        assert result["deal"]["deal_id"] == "OX-bef-4c7eFf"
        assert result["deal_url"] == "https://select.openx.com/deals/deal-001/details"

        body = _request_body(captured[0])
        assert "dealUpdate" in body["query"]
        assert body["variables"]["id"] == "deal-001"
        # deal_price is serialized as a 2-decimal string, matching create.
        assert body["variables"]["input"] == {"name": "My Newer Deal", "deal_price": "2.50"}

    @pytest.mark.asyncio
    async def test_status_pause_and_validation(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": {**UPDATED_DEAL, "status": "Paused"}})

        result = await ox_update_deal(deal_id="deal-001", status="Paused")
        assert result["success"] is True
        assert _request_body(captured[0])["variables"]["input"] == {"status": "Paused"}
        # Status-only updates apply near-realtime — no 1-2h propagation warning.
        assert not any("1-2 hours" in w for w in result["warnings"])

        bad = await ox_update_deal(deal_id="deal-001", status="Stopped")
        assert bad["success"] is False
        assert "Active" in bad["error"]

    @pytest.mark.asyncio
    async def test_non_status_update_carries_propagation_warning(self, mock_openx_graphql: respx.MockRouter):
        _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})
        result = await ox_update_deal(deal_id="deal-001", deal_price=3.0)
        assert result["success"] is True
        assert any("1-2 hours" in w for w in result["warnings"])

    @pytest.mark.asyncio
    async def test_empty_update_rejected_without_network(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})
        result = await ox_update_deal(deal_id="deal-001")
        assert result["success"] is False
        assert "No update fields" in result["error"]
        assert captured == []

    @pytest.mark.asyncio
    async def test_pmp_deal_type_and_participants_normalized(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})

        result = await ox_update_deal(
            deal_id="deal-001",
            pmp_deal_type="PREFERRED_DEAL",
            deal_participants=[{"demand_partner_id": "537240397", "buyer_ids": ["b1"]}],
        )

        assert result["success"] is True
        sent = _request_body(captured[0])["variables"]["input"]
        assert sent["pmp_deal_type"] == "3"  # mapped like create
        assert sent["deal_participants"] == [{"demand_partner": "537240397", "buyer_ids": ["b1"]}]
        assert any("full participant list" in w for w in result["warnings"])

    @pytest.mark.asyncio
    async def test_url_targeting_allowlist_conversion(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})

        result = await ox_update_deal(deal_id="deal-001", url_targeting={"allowlist": ["example.com"]})

        assert result["success"] is True
        sent = _request_body(captured[0])["variables"]["input"]
        assert sent["package"]["url_targeting"] == {"type": "whitelist", "urls": ["example.com"]}

    @pytest.mark.asyncio
    async def test_url_targeting_conflict_with_package_rejected(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})
        result = await ox_update_deal(
            deal_id="deal-001",
            url_targeting={"blocklist": ["bad.com"]},
            package={"url_targeting": {"type": "whitelist", "urls": ["good.com"]}},
        )
        assert result["success"] is False
        assert "not both" in result["error"]
        assert captured == []

    @pytest.mark.asyncio
    async def test_package_targeting_must_be_complete_object(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealUpdate": UPDATED_DEAL})

        # An empty targeting object would null every branch — refused.
        result = await ox_update_deal(deal_id="deal-001", package={"targeting": {}})
        assert result["success"] is False
        assert "COMPLETE" in result["error"]
        assert captured == []

        # A populated targeting object passes through wholesale, with a warning.
        full_targeting = {
            "inter_dimension_operator": "AND",
            "geographic": {"includes": {"country": "us"}},
            "rendering_context": {"op": "AND", "device_type": {"op": "INTERSECTS", "tv_devices": "5"}},
        }
        result = await ox_update_deal(deal_id="deal-001", package={"targeting": full_targeting})
        assert result["success"] is True
        sent = _request_body(captured[0])["variables"]["input"]
        assert sent["package"]["targeting"] == full_targeting
        assert any("replaced wholesale" in w for w in result["warnings"])

    @pytest.mark.asyncio
    async def test_graphql_error_surfaces_as_failure(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"errors": [{"message": "deal not found"}]})
        )
        result = await ox_update_deal(deal_id="deal-404", name="x")
        assert result["success"] is False
        assert "deal not found" in result["error"]


class TestArchiveNotExposed:
    """dealArchive must NOT be reachable by the agent.

    Archiving is effectively irreversible (the OpenXSelect guide documents no
    un-archive), so exposing it to an LLM agent was vetoed as a security
    decision (2026-06-11, Elyse). Pausing via ox_update_deal(status="Paused")
    is the supported way to stop delivery. This test pins the decision so a
    future change re-introducing an archive surface fails loudly.
    """

    def test_no_archive_tool_or_client_method(self):
        assert not hasattr(openx_mcp, "ox_archive_deal")
        assert not hasattr(openx_mcp.OpenXClient, "archive_deal")
        # No dealArchive mutation may exist anywhere in the module source
        # (the policy comment references the name in prose, not a call).
        with open(openx_mcp.__file__) as f:
            assert "dealArchive(id:" not in f.read()


class TestGetDealFullTargeting:
    """Tests for the full-targeting dealById selection and its fallback."""

    @pytest.mark.asyncio
    async def test_full_targeting_query_marks_selection(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealById": {"id": "deal-001", "name": "d"}})

        result = await ox_get_deal(deal_id="deal-001", full_targeting=True)

        assert result["success"] is True
        assert result["deal"]["_targeting_selection"] == "full"
        query = _request_body(captured[0])["query"]
        # The full selection covers every documented targeting branch.
        for branch in ("technographic", "viewability", "vtr", "custom", "page_url", "app_bundle_id", "postal_code"):
            assert branch in query, f"full dealById selection is missing {branch}"

    @pytest.mark.asyncio
    # Apollo returns GraphQL validation failures as HTTP 400 (observed live
    # 2026-06-11: 'Cannot query field "app_bundle_id" on "TargetingContent"'),
    # while resolver-level errors come back as HTTP 200 with an errors body.
    # The fallback must fire for BOTH shapes.
    @pytest.mark.parametrize(
        "schema_rejection",
        [
            httpx.Response(200, json={"errors": [{"message": 'Cannot query field "vtr"'}]}),
            httpx.Response(400, json={"errors": [{"message": 'Cannot query field "app_bundle_id"'}]}),
        ],
        ids=["graphql-errors-body", "http-400-validation"],
    )
    async def test_full_targeting_falls_back_to_legacy_on_schema_error(
        self, mock_openx_graphql: respx.MockRouter, schema_rejection: httpx.Response
    ):
        responses = iter(
            [
                schema_rejection,
                httpx.Response(200, json={"data": {"dealById": {"id": "deal-001", "name": "d"}}}),
            ]
        )
        captured: list[httpx.Request] = []

        def responder(request: httpx.Request) -> httpx.Response:
            captured.append(request)
            return next(responses)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=responder)

        result = await ox_get_deal(deal_id="deal-001", full_targeting=True)

        assert result["success"] is True
        # The legacy fallback is marked so update flows refuse a blind
        # targeting resubmit (unfetched branches would be nulled).
        assert result["deal"]["_targeting_selection"] == "legacy"
        assert len(captured) == 2
        assert "GetDealFull" in _request_body(captured[0])["query"]
        assert "GetDealFull" not in _request_body(captured[1])["query"]

    @pytest.mark.asyncio
    async def test_default_get_deal_unchanged(self, mock_openx_graphql: respx.MockRouter):
        captured = _graphql_capture(mock_openx_graphql, {"dealById": {"id": "deal-001", "name": "d"}})
        result = await ox_get_deal(deal_id="deal-001")
        assert result["success"] is True
        assert "_targeting_selection" not in result["deal"]
        assert "GetDealFull" not in _request_body(captured[0])["query"]
