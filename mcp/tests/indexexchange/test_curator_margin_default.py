"""Tests for the Index Exchange 30% Elcano curator-margin default.

Mirrors the OpenX/PubMatic/Media.net pattern: when margin_percent is
omitted from ix_execute_deal_from_prompt_inputs, the MCP applies a flat
30% Marketplace Owner fee (Percentage of winning bid) and emits an
`ix_default_curator_margin_applied` quality flag.
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT, ix_execute_deal_from_prompt_inputs

from .conftest import IX_BASE_URL
from .test_deals_v3 import (
    IX_DEALS_V3_ENDPOINT,
    IX_LOGIN_ENDPOINT,
    _mock_inventory_group_lookup,
    _mock_marketplace_resolution_endpoints,
)


@pytest.fixture
def mock_v3_api(mock_ix_api: respx.MockRouter) -> respx.MockRouter:
    """Local copy of the mock_v3_api fixture from test_deals_v3.py.

    test_deals_v3.py defines this fixture privately rather than in
    conftest.py, so we re-declare it here. Auth POST returns a fixed token
    so per-request lookups don't have to mock /login individually.
    """
    mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(
        return_value=httpx.Response(
            200,
            json={"loginResponse": {"authResponse": {"access_token": "test-token"}}},
        )
    )
    return mock_ix_api


class TestCuratorMarginConstant:
    def test_default_is_30_percent(self):
        assert ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT == 30.0


class TestExecuteAppliesMarginDefault:
    @pytest.mark.asyncio
    async def test_omitted_margin_applies_30_percent_default(self, mock_v3_api: respx.MockRouter):
        captured_request: httpx.Request | None = None

        def capture_post(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9200, "externalDealID": "IXTEST9200"})

        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_post)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9200")
        # Post-create verification re-fetch
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9200").mock(
            return_value=httpx.Response(200, json={"publishers": [{"legacyAccountID": 321}], "targeting": []})
        )

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="Margin Default Smoke",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="TRADR",
            end_date="2026-12-31",
            publisher_names=["Publisher One"],
        )

        assert result["success"] is True

        # Default warning + quality flag both present.
        expected_warning = (
            f"Applied default Elcano curator margin: {ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT:g}% "
            "(Percentage of winning bid). Pass margin_percent= to override."
        )
        assert expected_warning in result["warnings"]

        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "ix_default_curator_margin_applied" in flag_names

        flag = next(f for f in result["quality_flags"] if f["flag"] == "ix_default_curator_margin_applied")
        assert flag["margin_percent"] == ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT
        assert flag["margin_calculation_type"] == "P"

        # Submitted payload carries the resolved 30% margin.
        import json

        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload["marketplaceConfigurations"]["margin"] == ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT
        assert payload["marketplaceConfigurations"]["marginCalculationType"] == "P"

    @pytest.mark.asyncio
    async def test_explicit_margin_percent_overrides_default(self, mock_v3_api: respx.MockRouter):
        captured_request: httpx.Request | None = None

        def capture_post(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9201, "externalDealID": "IXTEST9201"})

        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_post)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9201")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9201").mock(
            return_value=httpx.Response(200, json={"publishers": [{"legacyAccountID": 321}], "targeting": []})
        )

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="Explicit Margin Smoke",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="TRADR",
            end_date="2026-12-31",
            publisher_names=["Publisher One"],
            margin_percent=15.0,
        )

        assert result["success"] is True

        # Default warning + quality flag should NOT be emitted when caller is explicit.
        unwanted_warning_prefix = "Applied default Elcano curator margin"
        assert not any(unwanted_warning_prefix in w for w in result["warnings"])

        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "ix_default_curator_margin_applied" not in flag_names

        # Submitted payload uses the explicit value.
        import json

        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload["marketplaceConfigurations"]["margin"] == 15.0
        assert payload["marketplaceConfigurations"]["marginCalculationType"] == "P"
