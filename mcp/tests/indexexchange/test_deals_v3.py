# mcp/tests/indexexchange/test_deals_v3.py
import hashlib
import json
import os
import sys

import httpx
import pytest
import respx
from openpyxl import Workbook

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

import indexexchange_mcp
from indexexchange_mcp import (
    MAX_INLINE_DOMAIN_VALUES,
    _build_marketplace_deal_payload,
    _clear_file_domain_expectation_locks_for_tests,
    _default_rolling_end_date,
    _normalize_deal_type,
    ix_create_marketplace_deal,
    ix_execute_deal_from_prompt_inputs,
    ix_get_deal_settings,
    ix_list_deals_v3,
)

from .conftest import IX_ACCOUNTS_ENDPOINT, IX_BASE_URL, IX_LOGIN_ENDPOINT

IX_DEALS_V3_ENDPOINT = f"{IX_BASE_URL}/api/deals/v3/deals"
IX_TARGETING_KEYS_ENDPOINT = f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups/targets"


def _mock_marketplace_resolution_endpoints(mock_router: respx.MockRouter) -> None:
    mock_router.get(IX_ACCOUNTS_ENDPOINT).mock(
        return_value=httpx.Response(
            200,
            json={
                "accounts": [
                    {
                        "accountID": 1491166,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ]
            },
        )
    )
    mock_router.get(IX_TARGETING_KEYS_ENDPOINT).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {"targetingKeyID": 3, "key": "DeviceType"},
                    {"targetingKeyID": 8, "key": "Viewability"},
                    {"targetingKeyID": 9, "key": "Country"},
                    {"targetingKeyID": 10, "key": "creativeTypeSize"},
                    {"targetingKeyID": 11, "key": "contentGenre"},
                    {"targetingKeyID": 77, "key": "Audience Segment"},
                    {"targetingKeyID": 120, "key": "Domain"},
                    {"targetingKeyID": 272, "key": "inventoryChannel"},
                ]
            },
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/9/values").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"targetingValueID": 375, "value": "USA", "name": "United States of America"}]},
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/3/values").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {"targetingValueID": 9, "value": "2", "name": "Personal computer"},
                    {"targetingValueID": 11, "value": "3", "name": "Connected TV"},
                    {"targetingValueID": 12, "value": "4", "name": "Phone"},
                    {"targetingValueID": 13, "value": "5", "name": "Tablet"},
                    {"targetingValueID": 14, "value": "6", "name": "Connected TV"},
                    {"targetingValueID": 15, "value": "7", "name": "Connected TV"},
                ]
            },
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/272/values").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {"targetingValueID": 2720, "value": "App", "name": "In-App"},
                    {"targetingValueID": 2721, "value": "Site", "name": "Web"},
                ]
            },
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/8/values").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"targetingValueID": 65, "value": "65", "name": "70% or higher"}]},
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/11/values").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"targetingValueID": 449, "value": "449", "name": "Entertainment"}]},
        )
    )
    mock_router.get(f"{IX_TARGETING_KEYS_ENDPOINT}/10/values").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {"targetingValueID": 1100, "value": "Video_ANY", "name": "Video (all sizes)"},
                    {"targetingValueID": 1000, "value": "Banner_ANY", "name": "Banner (all sizes)"},
                ]
            },
        )
    )


def _mock_inventory_group_lookup(
    mock_router: respx.MockRouter, external_deal_id: str, inventory_group: dict | None = None
) -> None:
    inventory_group_payload = inventory_group or {
        "inventoryGroupID": 189941,
        "name": f"ExternalDealID_{external_deal_id}_DspID_81",
        "type": "Deal Specific",
        "targeting": [],
    }
    mock_router.get(f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups").mock(
        return_value=httpx.Response(200, json={"totalCount": 1, "inventoryGroups": [inventory_group_payload]})
    )


def _mock_empty_inventory_group_lookup(mock_router: respx.MockRouter) -> None:
    mock_router.get(f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups").mock(
        return_value=httpx.Response(200, json={"totalCount": 0, "inventoryGroups": []})
    )


@pytest.fixture
def mock_v3_api(mock_ix_api: respx.MockRouter) -> respx.MockRouter:
    mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(
        return_value=httpx.Response(
            200,
            json={"loginResponse": {"authResponse": {"access_token": "test-token"}}},
        )
    )
    return mock_ix_api


@pytest.fixture(autouse=True)
def clear_domain_retry_locks() -> None:
    _clear_file_domain_expectation_locks_for_tests()


class TestBuildMarketplaceDealPayload:
    """Tests for the pure Python _build_marketplace_deal_payload function."""

    def test_valid_payload_minimal(self):
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )
        assert payload["classID"] == 4
        assert payload["account"] == {"accountID": 123456}
        assert payload["marketplaceConfigurations"]["dspID"] == 52
        assert "targeting" not in payload
        assert payload["endDate"] == "2026-12-31"
        # openMarket must NOT be sent when the caller doesn't specify it,
        # because some accounts (e.g. 1491166) reject it as an excluded field.
        assert "openMarket" not in payload

    def test_valid_payload_open_market_explicit_true(self):
        """When open_market=True is explicitly passed the field IS included."""
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            open_market=True,
        )
        assert payload["openMarket"] is True

    def test_valid_payload_open_market_explicit_false(self):
        """When open_market=False is explicitly passed the field IS included."""
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            open_market=False,
        )
        assert payload["openMarket"] is False

    def test_valid_payload_full(self):
        targeting = [
            {
                "targetingKeyID": 9,
                "keyName": "Country",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": "100"}]}],
            }
        ]
        labels = {"advertiser": "Acme"}
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            floor=1.50,
            dsp_id=52,
            end_date="2026-12-31",
            auction_type="fixed",
            seat_ids=["s1"],
            margin=5.5,
            margin_calculation_type="P",
            targeting=targeting,
            labels=labels,
        )
        assert payload["auctionType"] == "fixed"
        assert payload["marketplaceConfigurations"]["seatIDs"] == ["s1"]
        assert payload["marketplaceConfigurations"]["margin"] == 5.5
        assert payload["marketplaceConfigurations"]["marginCalculationType"] == "P"
        assert len(payload["targeting"]) == 1
        assert "seatIDs" not in payload
        assert "dspID" not in payload

    def test_valid_payload_includes_direct_internal_deal_ids(self):
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            internal_deal_ids=[463825, 463428],
        )

        internaldealid_targeting = next(
            targeting_obj
            for targeting_obj in payload["targeting"]
            if str(targeting_obj.get("keyName", "")).strip().lower() == "internaldealid"
        )
        assert [value["value"] for value in internaldealid_targeting["sets"][0]["values"]] == ["463825", "463428"]

    def test_floor_too_low(self):
        with pytest.raises(ValueError, match="0.10"):
            _build_marketplace_deal_payload(
                account_id=1,
                name="t",
                external_deal_id="abc",
                start_date="2026-01-01",
                end_date="2026-12-31",
                floor=0.05,
                dsp_id=1,
            )

    def test_external_deal_id_invalid(self):
        with pytest.raises(ValueError, match="'0'"):
            _build_marketplace_deal_payload(
                account_id=1,
                name="t",
                external_deal_id="0abc",
                start_date="2026-01-01",
                end_date="2026-12-31",
                floor=1,
                dsp_id=1,
            )
        with pytest.raises(ValueError, match="spaces"):
            _build_marketplace_deal_payload(
                account_id=1,
                name="t",
                external_deal_id="a b",
                start_date="2026-01-01",
                end_date="2026-12-31",
                floor=1,
                dsp_id=1,
            )

    def test_targeting_value_not_string(self):
        targeting = [
            {
                "targetingKeyID": 9,
                "keyName": "Country",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": 123}]}],
            }
        ]
        with pytest.raises(ValueError, match="string"):
            _build_marketplace_deal_payload(
                account_id=1,
                name="t",
                external_deal_id="abc",
                start_date="2026-01-01",
                end_date="2026-12-31",
                floor=1,
                dsp_id=1,
                targeting=targeting,
            )

    def test_sanitizes_labels_for_ui_compatibility(self):
        payload = _build_marketplace_deal_payload(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            labels={"advertiser": "McDonald's & Co\""},
        )

        assert payload["labels"]["advertiser"] == "McDonalds and Co"


class TestCreateMarketplaceDealV3:
    """Tests for the ix_create_marketplace_deal MCP tool (v3)."""

    @pytest.mark.asyncio
    async def test_create_deal_v3_success(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(
                201,
                json={
                    "internalDealID": 9002,
                    "externalDealID": "ELCANO-001",
                },
            )
        )
        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )
        assert result["success"] is True
        assert result["internal_deal_id"] == 9002
        assert result["deal_url"] == "https://app.indexexchange.com/deals/9002/show?account_id=123456"

    @pytest.mark.asyncio
    async def test_create_deal_v3_autogenerates_external_deal_id_when_missing(
        self,
        mock_v3_api: respx.MockRouter,
        monkeypatch: pytest.MonkeyPatch,
    ):
        generated_deal_id = "IX17429061325300000"
        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9003, "externalDealID": generated_deal_id})

        monkeypatch.setattr(indexexchange_mcp, "generate_external_deal_id", lambda: generated_deal_id)
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)

        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )

        assert result["success"] is True
        assert result["internal_deal_id"] == 9003
        assert captured_request is not None
        assert json.loads(captured_request.content)["externalDealID"] == generated_deal_id

    @pytest.mark.asyncio
    async def test_create_deal_v3_deal_url_uses_base_url_override(
        self,
        mock_ix_api: respx.MockRouter,
        monkeypatch: pytest.MonkeyPatch,
    ):
        monkeypatch.setenv("INDEXEXCHANGE_BASE_URL", "https://ix-staging.example.com")
        deals_endpoint = "https://ix-staging.example.com/api/deals/v3/deals"

        mock_ix_api.post("https://ix-staging.example.com/api/authentication/v1/login").mock(
            return_value=httpx.Response(
                200,
                json={"loginResponse": {"authResponse": {"access_token": "test-token"}}},
            )
        )
        mock_ix_api.post(deals_endpoint).mock(
            return_value=httpx.Response(
                201,
                json={
                    "internalDealID": 777,
                    "externalDealID": "ELCANO-777",
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-777",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )

        assert result["success"] is True
        assert result["internal_deal_id"] == 777
        assert result["deal_url"] == "https://ix-staging.example.com/deals/777/show?account_id=1491166"

    @pytest.mark.asyncio
    async def test_create_deal_v3_deal_url_is_none_when_internal_deal_id_missing(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(
                201,
                json={
                    "externalDealID": "ELCANO-001",
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )

        assert result["success"] is True
        assert result["internal_deal_id"] is None
        assert result["deal_url"] is None

    @pytest.mark.asyncio
    async def test_create_deal_v3_normalizes_key_name_and_translates_standard_value_ids(
        self, mock_v3_api: respx.MockRouter
    ):
        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9002, "externalDealID": "ELCANO-001"})

        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": [
                        {"targetingKeyID": 9, "key": "Country"},
                        {"targetingKeyID": 120, "key": "Domain"},
                    ]
                },
            )
        )
        mock_v3_api.get(f"{IX_TARGETING_KEYS_ENDPOINT}/9/values").mock(
            return_value=httpx.Response(200, json={"data": [{"targetingValueID": 375, "value": "USA"}]})
        )
        _mock_inventory_group_lookup(
            mock_v3_api,
            "IXTEST9123",
            inventory_group={
                "inventoryGroupID": 189941,
                "name": "ExternalDealID_IXTEST9123_DspID_52",
                "type": "Deal Specific",
                "targeting": [
                    {"key": "Country", "values": [{"value": "USA", "include": True}]},
                    {"key": "Domain", "values": [{"value": "example.com", "include": False}]},
                ],
            },
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9002").mock(
            return_value=httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "targetingKeyID": 9,
                            "keyName": "country",
                            "sets": [{"values": [{"value": "USA"}]}],
                        },
                        {
                            "keyName": "domain",
                            "sets": [{"values": [{"value": "example.com"}]}],
                        },
                    ]
                },
            )
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 9,
                    "keyName": "country",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "375"}]}],
                },
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain and app bundle",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "example.com"}]}],
                },
            ],
        )

        if not result.get("success"):
            pytest.fail(json.dumps(result, indent=2))
        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload["targeting"][0]["keyName"] == "Country"
        assert payload["targeting"][0]["sets"][0]["values"][0]["value"] == "USA"
        assert payload["targeting"][1]["keyName"] == "Domain"
        assert payload["targeting"][1]["sets"][0]["values"][0]["value"] == "example.com"
        assert result["domain_diagnostics"]["source_domain_rows"] == 1
        assert result["domain_diagnostics"]["normalized_unique_domains"] == 1
        assert result["domain_diagnostics"]["submitted_domain_values"] == 1
        assert result["domain_diagnostics"]["persisted_domain_values"] == 1
        assert result["verification"]["domain_count_parity"] is True

    @pytest.mark.asyncio
    async def test_create_deal_v3_accepts_numeric_app_bundle_values(self, mock_v3_api: respx.MockRouter):
        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9020, "externalDealID": "ELCANO-001"})

        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )
        _mock_inventory_group_lookup(
            mock_v3_api,
            "ELCANO-001",
            inventory_group={
                "inventoryGroupID": 189941,
                "name": "ExternalDealID_ELCANO-001_DspID_52",
                "type": "Deal Specific",
                "targeting": [
                    {"key": "Domain", "values": [{"value": "3334", "include": True}]},
                ],
            },
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9020").mock(
            return_value=httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "targetingKeyID": 120,
                            "keyName": "Domain",
                            "sets": [{"values": [{"value": "3334"}]}],
                        }
                    ]
                },
            )
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "3334"}]}],
                }
            ],
        )

        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload["targeting"][0]["sets"][0]["values"][0]["value"] == "3334"

    @pytest.mark.asyncio
    async def test_create_deal_v3_accepts_numeric_standard_value_tokens(self, mock_v3_api: respx.MockRouter):
        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9021, "externalDealID": "ELCANO-001"})

        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 3, "key": "DeviceType"}]})
        )
        mock_v3_api.get(f"{IX_TARGETING_KEYS_ENDPOINT}/3/values").mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": [
                        {"targetingValueID": 11, "value": "3"},
                        {"targetingValueID": 14, "value": "6"},
                        {"targetingValueID": 15, "value": "7"},
                    ]
                },
            )
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 3,
                    "keyName": "DeviceType",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "3"}, {"value": "6"}, {"value": "7"}]}],
                }
            ],
        )

        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert [v["value"] for v in payload["targeting"][0]["sets"][0]["values"]] == ["3", "6", "7"]

    @pytest.mark.asyncio
    async def test_create_deal_v3_payload_sent_correctly(self, mock_v3_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture)

        await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
        )

        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload["account"] == {"accountID": 123456}
        assert payload["classID"] == 4
        assert payload["auctionType"] == "first"
        assert "extendedSeatIDs" not in payload["marketplaceConfigurations"]
        assert "targeting" not in payload["marketplaceConfigurations"]
        # openMarket must be absent when not explicitly provided (accounts like 1491166
        # return "OpenMarket is an excluded field" if it's present in the request body).
        assert "openMarket" not in payload

    @pytest.mark.asyncio
    async def test_create_deal_v3_uses_internaldealid_targeting_object(
        self, mock_v3_api: respx.MockRouter, monkeypatch: pytest.MonkeyPatch
    ):
        monkeypatch.setattr(indexexchange_mcp, "DIRECT_TARGET_VERIFICATION_RETRY_DELAYS_SECONDS", ())
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9004, "externalDealID": "ELCANO-001"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture)
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9004").mock(return_value=httpx.Response(200, json={}))

        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            internal_deal_ids=[463825, 463428],
        )

        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        internaldealid_targeting = next(
            targeting_obj
            for targeting_obj in payload.get("targeting", [])
            if str(targeting_obj.get("keyName", "")).strip().lower() == "internaldealid"
        )
        assert [value["value"] for value in internaldealid_targeting["sets"][0]["values"]] == ["463825", "463428"]
        assert result["verification"]["internal_deal_ids_visible"] is False
        assert result["verification"]["internal_deal_ids_parity"] is False
        assert result["verification_success"] is False
        assert any(flag["flag"] == "internal_deal_id_visibility_failed" for flag in result["quality_flags"])

    @pytest.mark.asyncio
    async def test_create_deal_v3_verifies_native_internal_deal_ids_when_exposed(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9005, "externalDealID": "ELCANO-001"})
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9005").mock(
            return_value=httpx.Response(200, json={"internalDealIDs": [463825, 463428]})
        )
        _mock_inventory_group_lookup(mock_v3_api, "ELCANO-001")

        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            internal_deal_ids=[463825, 463428],
        )

        assert result["success"] is True
        assert result["verification"]["internal_deal_ids_visible"] is True
        assert result["verification"]["internal_deal_ids_parity"] is True

    @pytest.mark.asyncio
    async def test_create_deal_v3_retries_direct_internal_deal_verification_until_visible(
        self, mock_v3_api: respx.MockRouter, monkeypatch: pytest.MonkeyPatch
    ):
        monkeypatch.setattr(indexexchange_mcp, "DIRECT_TARGET_VERIFICATION_RETRY_DELAYS_SECONDS", (0,))

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9006, "externalDealID": "ELCANO-001"})
        )
        get_route = mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9006")
        get_route.side_effect = [
            httpx.Response(200, json={}),
            httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "targetingType": "standard",
                            "keyName": "internaldealid",
                            "sets": [
                                {
                                    "operator": "ANY_OF",
                                    "values": [{"value": "463825"}, {"value": "463428"}],
                                }
                            ],
                        }
                    ]
                },
            ),
        ]
        _mock_inventory_group_lookup(mock_v3_api, "ELCANO-001")

        result = await ix_create_marketplace_deal(
            account_id=123456,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            internal_deal_ids=[463825, 463428],
        )

        assert result["success"] is True
        assert result["verification"]["internal_deal_ids_visible"] is True
        assert result["verification"]["internal_deal_ids_parity"] is True

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_invalid_domain_values_in_strict_mode(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [
                        {
                            "operator": "ANY_OF",
                            "values": [{"value": "example.com"}, {"value": "https://bad.example/path"}],
                        }
                    ],
                }
            ],
        )

        assert result["success"] is False
        assert "strict mode is enabled" in result["error"]["message"]

    @pytest.mark.asyncio
    async def test_create_deal_v3_allows_partial_when_explicitly_enabled(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9010, "externalDealID": "ELCANO-001"})
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9010").mock(
            return_value=httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "keyName": "domain",
                            "sets": [{"values": [{"value": "example.com"}]}],
                        }
                    ]
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            allow_partial_targeting=True,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [
                        {
                            "operator": "ANY_OF",
                            "values": [
                                {"value": "EXAMPLE.com"},
                                {"value": "example.com"},
                                {"value": "https://bad.example/path"},
                            ],
                        }
                    ],
                }
            ],
        )

        assert result["success"] is True
        assert result["domain_diagnostics"]["source_domain_rows"] == 3
        assert result["domain_diagnostics"]["normalized_unique_domains"] == 1
        assert result["domain_diagnostics"]["invalid_count"] == 1
        assert result["domain_diagnostics"]["duplicate_count"] == 1
        assert result["domain_diagnostics"]["submitted_domain_values"] == 1
        assert result["domain_diagnostics"]["persisted_domain_values"] == 1
        assert result["verification"]["domain_count_parity"] is True

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_malformed_domain_literals_in_strict_mode(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [
                        {
                            "operator": "ANY_OF",
                            "values": [
                                {"value": "example.com"},
                                {"value": "https://merriam-webster.com/games/quordle"},
                                {"value": "tastyedits.com/blog"},
                            ],
                        }
                    ],
                }
            ],
        )

        assert result["success"] is False
        assert "strict mode is enabled" in result["error"]["message"]
        assert "invalid_count=2" in result["error"]["message"]

    @pytest.mark.asyncio
    async def test_create_deal_v3_fails_strict_mode_on_post_create_domain_parity_mismatch(
        self, mock_v3_api: respx.MockRouter
    ):
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9011, "externalDealID": "ELCANO-001"})
        )
        _mock_inventory_group_lookup(
            mock_v3_api,
            "ELCANO-001",
            inventory_group={
                "inventoryGroupID": 189941,
                "name": "ExternalDealID_ELCANO-001_DspID_52",
                "type": "Deal Specific",
                "targeting": [
                    {"key": "Domain", "values": [{"value": "a.com", "include": True}]},
                ],
            },
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9011").mock(
            return_value=httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "keyName": "domain",
                            "sets": [{"values": [{"value": "a.com"}]}],
                        }
                    ]
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [
                        {
                            "operator": "ANY_OF",
                            "values": [{"value": "a.com"}, {"value": "b.com"}],
                        }
                    ],
                }
            ],
        )

        assert result["success"] is False
        assert "post-create verification failed" in result["error"]["message"]
        assert result["domain_diagnostics"]["submitted_domain_values"] == 2
        assert result["domain_diagnostics"]["persisted_domain_values"] == 1
        assert result["verification"]["domain_count_parity"] is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_expected_domain_count_mismatch(self, mock_v3_api: respx.MockRouter):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=2,
            expected_domains_fingerprint=hashlib.sha256(
                json.dumps(["a.com"], separators=(",", ":")).encode()
            ).hexdigest(),
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert result["success"] is False
        assert "Domain count mismatch" in result["error"]["message"]
        assert result["domain_diagnostics"]["expected_domain_count"] == 2
        assert "expected_domains_fingerprint" in result["domain_diagnostics"]
        assert result["domain_diagnostics"]["submitted_domain_values"] == 1
        assert "submitted_domains_fingerprint" in result["domain_diagnostics"]
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_expected_domain_fingerprint_mismatch(self, mock_v3_api: respx.MockRouter):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=1,
            expected_domains_fingerprint="deadbeef",
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert result["success"] is False
        assert "Domain fingerprint mismatch" in result["error"]["message"]
        assert result["domain_diagnostics"]["expected_domain_count"] == 1
        assert result["domain_diagnostics"]["expected_domains_fingerprint"] == "deadbeef"
        assert "submitted_domains_fingerprint" in result["domain_diagnostics"]
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_expected_count_and_fingerprint_pass_for_full_payload(
        self, mock_v3_api: respx.MockRouter
    ):
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9012, "externalDealID": "ELCANO-001"})
        )
        _mock_inventory_group_lookup(
            mock_v3_api,
            "ELCANO-001",
            inventory_group={
                "inventoryGroupID": 189941,
                "name": "ExternalDealID_ELCANO-001_DspID_52",
                "type": "Deal Specific",
                "targeting": [
                    {
                        "key": "Domain",
                        "values": [{"value": "a.com", "include": True}, {"value": "b.com", "include": True}],
                    },
                ],
            },
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9012").mock(
            return_value=httpx.Response(
                200,
                json={
                    "targeting": [
                        {
                            "keyName": "domain",
                            "sets": [{"values": [{"value": "a.com"}, {"value": "b.com"}]}],
                        }
                    ]
                },
            )
        )

        expected_fingerprint = hashlib.sha256(
            json.dumps(["a.com", "b.com"], separators=(",", ":")).encode()
        ).hexdigest()

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=2,
            expected_domains_fingerprint=expected_fingerprint,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [
                        {
                            "operator": "ANY_OF",
                            "values": [{"value": "a.com"}, {"value": "B.COM"}],
                        }
                    ],
                }
            ],
        )

        assert result["success"] is True
        assert result["domain_diagnostics"]["submitted_domain_values"] == 2
        assert result["domain_diagnostics"]["persisted_domain_values"] == 2
        assert result["verification"]["domain_count_parity"] is True
        assert result["verification"]["domain_fingerprint_parity"] is True

    @pytest.mark.asyncio
    async def test_create_deal_v3_domain_source_file_path_injects_domains_server_side(
        self, mock_v3_api: respx.MockRouter, tmp_path
    ):
        domains = [f"d{i}.com" for i in range(1, 601)]
        csv_path = tmp_path / "domains.csv"
        csv_path.write_text("domain\n" + "\n".join(domains) + "\n", encoding="utf-8")

        expected_fingerprint = hashlib.sha256(
            json.dumps(sorted(set(domains)), separators=(",", ":")).encode()
        ).hexdigest()

        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        captured_request: httpx.Request | None = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"externalDealID": "ELCANO-DOMAIN-SOURCE"})

        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture)

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-DOMAIN-SOURCE",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            domain_source={"file_path": str(csv_path)},
            expected_domain_count=600,
            expected_domains_fingerprint=expected_fingerprint,
        )

        assert result["success"] is True
        assert result["domain_diagnostics"]["submitted_domain_values"] == 600
        assert result["domain_diagnostics"]["domain_source_type"] == "file_path"
        assert result["domain_diagnostics"]["domain_source_rows_loaded"] == 600
        assert result["domain_diagnostics"]["domain_source_header_rows_removed"] == 1
        assert post_route.called is True
        assert captured_request is not None

        payload = json.loads(captured_request.content.decode("utf-8"))
        domain_targeting = next(t for t in payload["targeting"] if t.get("targetingKeyID") == 120)
        assert len(domain_targeting["sets"][0]["values"]) == 600

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_domain_source_and_inline_domain_targeting_are_both_provided(
        self, mock_v3_api: respx.MockRouter, tmp_path
    ):
        csv_path = tmp_path / "domains.csv"
        csv_path.write_text("domain\na.com\n", encoding="utf-8")

        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-MIXED",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            domain_source={"file_path": str(csv_path)},
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert result["success"] is False
        assert "cannot be used together" in result["error"]["message"]
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_inline_domain_targeting_is_too_large(self, mock_v3_api: respx.MockRouter):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))

        oversized_values = [{"value": f"d{i}.com"} for i in range(MAX_INLINE_DOMAIN_VALUES + 1)]

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-OVERSIZED",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": oversized_values}],
                }
            ],
        )

        assert result["success"] is False
        assert "Use domain_source.file_path" in result["error"]["message"]
        assert result["domain_diagnostics"]["inline_domain_values"] == MAX_INLINE_DOMAIN_VALUES + 1
        assert result["domain_diagnostics"]["max_inline_domain_values"] == MAX_INLINE_DOMAIN_VALUES
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_only_one_expected_domain_guardrail_is_provided(
        self, mock_v3_api: respx.MockRouter
    ):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-001",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=1,
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert result["success"] is False
        assert "must be provided together" in result["error"]["message"]
        assert result["domain_diagnostics"]["expected_domain_count"] == 1
        assert result["domain_diagnostics"]["expected_domains_fingerprint"] is None
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_blocks_when_expected_domain_guardrails_change_across_retries(
        self, mock_v3_api: respx.MockRouter
    ):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        first = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-LOCKED",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=2,
            expected_domains_fingerprint=hashlib.sha256(
                json.dumps(["a.com", "b.com"], separators=(",", ":")).encode()
            ).hexdigest(),
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert first["success"] is False
        assert "Domain count mismatch" in first["error"]["message"]

        second = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-LOCKED",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=1,
            expected_domains_fingerprint=hashlib.sha256(
                json.dumps(["a.com"], separators=(",", ":")).encode()
            ).hexdigest(),
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert second["success"] is False
        assert "changed across retries" in second["error"]["message"]
        assert second["domain_diagnostics"]["locked_expected_domain_count"] == 2
        assert second["domain_diagnostics"]["expected_domain_count"] == 1
        assert post_route.called is False

    @pytest.mark.asyncio
    async def test_create_deal_v3_retry_lock_is_scoped_by_external_deal_id(self, mock_v3_api: respx.MockRouter):
        post_route = mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(return_value=httpx.Response(201, json={}))
        mock_v3_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_v3_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(200, json={"data": [{"targetingKeyID": 120, "key": "Domain"}]})
        )

        _ = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-LOCK-A",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=2,
            expected_domains_fingerprint=hashlib.sha256(
                json.dumps(["a.com", "b.com"], separators=(",", ":")).encode()
            ).hexdigest(),
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        result = await ix_create_marketplace_deal(
            account_id=1491166,
            name="Test Deal",
            external_deal_id="ELCANO-LOCK-B",
            start_date="2026-03-01",
            end_date="2026-12-31",
            floor=1.50,
            dsp_id=52,
            expected_domain_count=2,
            expected_domains_fingerprint=hashlib.sha256(
                json.dumps(["a.com", "b.com"], separators=(",", ":")).encode()
            ).hexdigest(),
            targeting=[
                {
                    "targetingKeyID": 120,
                    "keyName": "Domain",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "a.com"}]}],
                }
            ],
        )

        assert result["success"] is False
        assert "Domain count mismatch" in result["error"]["message"]
        assert "changed across retries" not in result["error"]["message"]
        assert post_route.called is False


class TestListDealsV3:
    """Tests for ix_list_deals_v3."""

    @pytest.mark.asyncio
    async def test_list_deals_v3_success(self, mock_v3_api: respx.MockRouter):
        mock_v3_api.get(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "deals": [{"internalDealID": 1, "name": "Deal 1"}],
                    "total": 1,
                },
            )
        )
        result = await ix_list_deals_v3(account_ids=[123])
        assert result["success"] is True
        assert len(result["deals"]) == 1
        assert result["total"] == 1

    @pytest.mark.asyncio
    async def test_list_deals_v3_params(self, mock_v3_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json={"deals": [], "total": 0})

        mock_v3_api.get(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture)

        await ix_list_deals_v3(
            account_ids=[123],
            class_ids=[4],
            dsp_ids=[52],
            search="test",
            status="active",
            page_size=50,
        )

        assert captured_request is not None
        url_str = str(captured_request.url)
        assert "accountIDs=123" in url_str
        assert "classIDs=4" in url_str
        assert "dspIDs=52" in url_str
        assert "search=test" in url_str
        assert "status=active" in url_str
        assert "pageSize=50" in url_str


class TestGetDealSettings:
    """Tests for ix_get_deal_settings."""

    @pytest.mark.asyncio
    async def test_get_deal_settings_success(self, mock_v3_api: respx.MockRouter):
        deal_id = 9001
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/{deal_id}").mock(
            return_value=httpx.Response(
                200,
                json={"internalDealID": deal_id, "name": "Test Deal"},
            )
        )

        result = await ix_get_deal_settings(internal_deal_id=deal_id)
        assert result["success"] is True
        assert result["deal"]["internalDealID"] == deal_id


class TestExecuteDealFromPromptInputs:
    @pytest.mark.asyncio
    async def test_execute_resolves_human_inputs_and_reads_excel_domains(
        self,
        mock_v3_api: respx.MockRouter,
        tmp_path,
    ):
        workbook = Workbook()
        worksheet = workbook.active
        worksheet.title = "Sheet1"
        worksheet.append(["Sites"])
        worksheet.append(["example.com"])
        domain_file = tmp_path / "domains.xlsx"
        workbook.save(domain_file)

        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/segments/v2/segments").mock(
            return_value=httpx.Response(200, json=[{"id": 280, "name": "Auto Intenders"}])
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9123, "externalDealID": "IXTEST9123"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(
            mock_v3_api,
            "IXTEST9123",
            inventory_group={
                "inventoryGroupID": 189941,
                "name": "ExternalDealID_IXTEST9123_DspID_52",
                "type": "Deal Specific",
                "targeting": [
                    {"key": "Country", "values": [{"value": "USA", "include": True}]},
                    {
                        "key": "DeviceType",
                        "values": [
                            {"value": "3", "include": True},
                            {"value": "6", "include": True},
                            {"value": "7", "include": True},
                        ],
                    },
                    {"key": "creativeTypeSize", "values": [{"value": "Video_ANY", "include": True}]},
                    {"key": "Domain", "values": [{"value": "example.com", "include": False}]},
                ],
            },
        )
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9123").mock(
            return_value=httpx.Response(
                200,
                json={
                    "publishers": [{"legacyAccountID": 321}],
                    "targeting": [
                        {
                            "targetingKeyID": 9,
                            "keyName": "country",
                            "sets": [{"values": [{"value": "USA"}]}],
                        },
                        {
                            "targetingKeyID": 3,
                            "keyName": "devicetype",
                            "sets": [{"values": [{"value": "3"}, {"value": "6"}, {"value": "7"}]}],
                        },
                        {
                            "targetingKeyID": 10,
                            "keyName": "creativetypesize",
                            "sets": [{"values": [{"value": "Video_ANY"}]}],
                        },
                        {
                            "targetingKeyID": 120,
                            "keyName": "Domain",
                            "sets": [{"operator": "NONE_OF", "values": [{"value": "example.com"}]}],
                        },
                    ],
                },
            )
        )

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Prompt Deal",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="TRADR",
            end_date="2026-12-31",
            publisher_names=["Publisher One"],
            segment_names=["Auto Intenders"],
            domain_file_path=str(domain_file),
            domain_sheet="Sheet1",
            domain_column="Sites",
            geo_countries=["US"],
            device_types=["CTV"],
            iab_categories=["Entertainment"],
            viewability_threshold=70,
        )

        if not result.get("success"):
            pytest.fail(json.dumps(result, indent=2))
        assert result["success"] is True
        assert result["deal_url"] == "https://app.indexexchange.com/deals/9123/show?account_id=1491166"
        # The 30% Elcano curator-margin default fires when margin_percent is omitted.
        expected_margin_warning = (
            f"Applied default Elcano curator margin: "
            f"{indexexchange_mcp.ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT:g}% "
            "(Percentage of winning bid). Pass margin_percent= to override."
        )
        # The domain-operator echo confirms which wire operator the file-driven
        # list resolved to (NONE_OF here, since domain_match_operator defaults to
        # blocklist), so the caller can verify intent against the persisted deal.
        expected_domain_operator_warning = (
            "Applied domain targeting operator NONE_OF "
            f"(domain_match_operator='blocklist') to 1 values from {domain_file}."
        )
        assert result["warnings"] == [expected_margin_warning, expected_domain_operator_warning]
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "ix_default_curator_margin_applied" in flag_names
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        assert payload["marketplaceConfigurations"]["dspID"] == 52
        assert payload["publisherIDs"] == [321]
        assert payload["targeting"][0]["keyName"] == "Country"
        assert payload["targeting"][0]["sets"][0]["values"] == [{"value": "USA"}]
        # Segment targeting uses keyName "segmentid" (the API's actual
        # segment-targeting key — `im_segments` from `ix_list_targeting_keys`
        # rejects exclusion). targetingKeyID is omitted from segment objects
        # because the live API stores them without one.
        segment_targeting = next((t for t in payload["targeting"] if t.get("keyName") == "segmentid"), None)
        assert segment_targeting is not None, "expected a segmentid targeting object"
        assert "targetingKeyID" not in segment_targeting

    @pytest.mark.asyncio
    async def test_execute_returns_warning_for_unresolved_publisher_name(self, mock_v3_api: respx.MockRouter):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9124, "externalDealID": "IXTEST9124"})
        )
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9124")

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Prompt Deal Warning",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="TRADR",
            end_date="2026-12-31",
            publisher_names=["Missing Publisher"],
            geo_countries=["US"],
        )

        assert result["success"] is True
        # Margin default fires alongside the publisher-unresolved warning.
        expected_margin_warning = (
            f"Applied default Elcano curator margin: "
            f"{indexexchange_mcp.ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT:g}% "
            "(Percentage of winning bid). Pass margin_percent= to override."
        )
        # The publisher-resolution warning now surfaces the candidate count
        # (0 here, since "Missing Publisher" has no substring match) and
        # directs the caller to use publisher_ids instead of guessing.
        assert result["warnings"] == [
            expected_margin_warning,
            "Publisher 'Missing Publisher' resolution returned 0 candidates; pass publisher_ids=[...] to disambiguate.",
        ]
        # And it's surfaced as a structured quality flag the caller can act on.
        flags = result["quality_flags"]
        not_found_flags = [f for f in flags if f.get("flag") == "ix_publisher_resolution_not_found"]
        assert len(not_found_flags) == 1
        assert not_found_flags[0]["requested_name"] == "Missing Publisher"
        assert not_found_flags[0]["candidates"] == []

    @pytest.mark.asyncio
    async def test_execute_resolves_deals_with_publishers_to_internal_deal_ids(self, mock_v3_api: respx.MockRouter):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 81, "name": "Quantcast", "classID": 4}])
        )

        def list_deals_handler(request: httpx.Request) -> httpx.Response:
            search_value = request.url.params.get("search")
            if search_value == "IX_Live Sports_Scripps_NHL_Hockey_PostSeason":
                return httpx.Response(
                    200,
                    json={
                        "deals": [
                            {
                                "classID": 5,
                                "name": "IX_Live Sports_Scripps_NHL_Hockey_PostSeason",
                                "internalDealID": 501536,
                                "externalDealID": "IXLiveNHLpostseason",
                            },
                            {
                                "classID": 5,
                                "name": "IX_Live Sports_Scripps_NHL_Hockey_PostSeason",
                                "internalDealID": 501537,
                                "externalDealID": "IXLiveNHLpostseason",
                            },
                        ],
                        "total": 2,
                    },
                )
            return httpx.Response(
                200,
                json={
                    "deals": [
                        {
                            "classID": 5,
                            "name": search_value,
                            "internalDealID": 465381,
                            "externalDealID": "IXLiveNHLpostseason",
                        }
                    ],
                    "total": 1,
                },
            )

        mock_v3_api.get(IX_DEALS_V3_ENDPOINT).mock(side_effect=list_deals_handler)

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9130, "externalDealID": "IXTEST9130"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9130")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9130").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Prompt DWP Deal",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="Quantcast",
            end_date="2026-12-31",
            deals_with_publishers=[
                "IXLiveNHLpostseason • IX_Live Sports_Scripps_NHL_Hockey_PostSeason",
                "IXLiveNHLpostseason • IX_Live Sports_Scripps_NHL_Hockey_PostSeason",
            ],
            geo_countries=["US"],
        )

        assert result["success"] is True
        assert result["verification_success"] is False
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        internaldealid_targeting = next(
            targeting_obj
            for targeting_obj in payload["targeting"]
            if str(targeting_obj.get("keyName", "")).strip().lower() == "internaldealid"
        )
        assert [value["value"] for value in internaldealid_targeting["sets"][0]["values"]] == ["501536", "501537"]

    @pytest.mark.asyncio
    async def test_execute_returns_flagged_success_when_internal_deal_ids_not_visible(
        self, mock_v3_api: respx.MockRouter
    ):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 81, "name": "Quantcast", "classID": 4}])
        )
        mock_v3_api.get(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "deals": [
                        {
                            "classID": 5,
                            "name": "IX_Live Sports_Charter_NHL_Hockey_Postseason",
                            "internalDealID": 465381,
                            "externalDealID": "IXLiveNHLpostseason",
                        }
                    ],
                    "total": 1,
                },
            )
        )
        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(
            return_value=httpx.Response(201, json={"internalDealID": 9131, "externalDealID": "IXTEST9131"})
        )
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9131")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9131").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Prompt DWP Visibility",
            start_date="2026-04-20",
            floor=1.25,
            dsp_name="Quantcast",
            end_date="2026-12-31",
            deals_with_publishers=["IXLiveNHLpostseason • IX_Live Sports_Charter_NHL_Hockey_Postseason"],
            geo_countries=["US"],
        )

        assert result["success"] is True
        assert result["verification_success"] is False
        assert any(flag["flag"] == "internal_deal_id_visibility_failed" for flag in result["quality_flags"])


class TestRollingEndDateHelper:
    def test_default_rolling_end_date_adds_two_years(self):
        # Default is 24 months — trader-spec default for IX deals when end_date omitted.
        assert _default_rolling_end_date("2026-04-24") == "2028-04-24"

    def test_default_rolling_end_date_year_rollover(self):
        # Spanning a year boundary still resolves to the same MM-DD two years out.
        assert _default_rolling_end_date("2026-11-15") == "2028-11-15"

    def test_default_rolling_end_date_clamps_short_month(self):
        # Leap-day start clamps to Feb 28 when the +24mo target year is not a leap year.
        assert _default_rolling_end_date("2024-02-29") == "2026-02-28"

    def test_default_rolling_end_date_honors_months_arg(self):
        # Explicit months override still works (lets callers shorten the default).
        assert _default_rolling_end_date("2026-04-24", months=3) == "2026-07-24"


class TestNormalizeDealType:
    def test_display(self):
        assert _normalize_deal_type("display") == "display"
        assert _normalize_deal_type("Display") == "display"

    def test_olv_variants(self):
        # OLV is its own canonical bucket now — it must NOT collapse to
        # "display". The previous behaviour gave OLV deals the Banner
        # creative format; the fix routes them to Video instead.
        assert _normalize_deal_type("OLV") == "olv"
        assert _normalize_deal_type(" Display/OLV ") == "olv"
        assert _normalize_deal_type("display_olv") == "olv"

    def test_ctv(self):
        assert _normalize_deal_type("CTV") == "ctv"

    def test_ott(self):
        assert _normalize_deal_type("OTT") == "ott"
        assert _normalize_deal_type("ott") == "ott"

    def test_unknown_or_empty(self):
        assert _normalize_deal_type(None) is None
        assert _normalize_deal_type("") is None
        assert _normalize_deal_type("video") is None


class TestDealTypeTargetingDefaults:
    """Verify _ensure_deal_type_targeting_defaults auto-fills Index UI defaults."""

    @pytest.mark.asyncio
    async def test_display_olv_defaults_fill_device_inventory_banner(self, mock_v3_api: respx.MockRouter):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9201, "externalDealID": "IXTEST9201"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9201")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9201").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Display Defaults",
            start_date="2026-04-24",
            floor=1.25,
            dsp_name="Bidswitch - RTB",
            publisher_names=["Publisher One"],
            deal_type="display",
        )

        assert result["success"] is True, result
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        targeting_by_key = {
            str(t.get("keyName", "")).strip().lower(): t for t in payload["targeting"] if isinstance(t, dict)
        }

        device_values = {v["value"] for v in targeting_by_key["devicetype"]["sets"][0]["values"]}
        assert device_values == {"2", "4", "5"}

        inventory_values = {v["value"] for v in targeting_by_key["inventorychannel"]["sets"][0]["values"]}
        assert inventory_values == {"App", "Site"}

        creative_values = {v["value"] for v in targeting_by_key["creativetypesize"]["sets"][0]["values"]}
        assert creative_values == {"Banner_ANY"}

    @pytest.mark.asyncio
    async def test_ctv_defaults_fill_device_and_video(self, mock_v3_api: respx.MockRouter):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9202, "externalDealID": "IXTEST9202"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9202")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9202").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX CTV Defaults",
            start_date="2026-04-24",
            floor=1.25,
            dsp_name="Bidswitch - RTB",
            publisher_names=["Publisher One"],
            deal_type="ctv",
        )

        assert result["success"] is True, result
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        targeting_by_key = {
            str(t.get("keyName", "")).strip().lower(): t for t in payload["targeting"] if isinstance(t, dict)
        }

        device_values = {v["value"] for v in targeting_by_key["devicetype"]["sets"][0]["values"]}
        assert device_values == {"3", "6", "7"}

        creative_values = {v["value"] for v in targeting_by_key["creativetypesize"]["sets"][0]["values"]}
        assert creative_values == {"Video_ANY"}

        # CTV deals must force App-only inventory even when the brief omits
        # `inventory_type` — the trader spec routes every CTV deal through
        # in-app inventory regardless of what was requested.
        inventory_values = {v["value"] for v in targeting_by_key["inventorychannel"]["sets"][0]["values"]}
        assert inventory_values == {"App"}

    @pytest.mark.asyncio
    async def test_olv_defaults_fill_video_not_banner(self, mock_v3_api: respx.MockRouter):
        """Headline regression: OLV deals must emit Video_ANY creative format,
        not Banner_ANY. Earlier revisions collapsed OLV into the Display
        bucket, which silently gave OLV deals the Banner creative — the
        trader QA review of Reklaim SMT deals caught this in the Index UI."""
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9204, "externalDealID": "IXTEST9204"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9204")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9204").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX OLV Defaults",
            start_date="2026-04-24",
            floor=1.25,
            dsp_name="Bidswitch - RTB",
            publisher_names=["Publisher One"],
            deal_type="olv",
        )

        assert result["success"] is True, result
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        targeting_by_key = {
            str(t.get("keyName", "")).strip().lower(): t for t in payload["targeting"] if isinstance(t, dict)
        }

        # OLV shares Display's device set (PC + Phone + Tablet) but uses
        # the Video creative format instead of Banner.
        device_values = {v["value"] for v in targeting_by_key["devicetype"]["sets"][0]["values"]}
        assert device_values == {"2", "4", "5"}

        creative_values = {v["value"] for v in targeting_by_key["creativetypesize"]["sets"][0]["values"]}
        assert creative_values == {"Video_ANY"}, "OLV must emit Video_ANY — Banner_ANY here would be the original bug"

        # OLV still allows App + Site inventory (unlike CTV which is App-only).
        inventory_values = {v["value"] for v in targeting_by_key["inventorychannel"]["sets"][0]["values"]}
        assert inventory_values == {"App", "Site"}

    @pytest.mark.asyncio
    async def test_default_end_date_applied_when_omitted(self, mock_v3_api: respx.MockRouter):
        _mock_marketplace_resolution_endpoints(mock_v3_api)
        mock_v3_api.get(f"{IX_BASE_URL}/api/deals/v1/dsps").mock(
            return_value=httpx.Response(200, json=[{"dspID": 52, "name": "Bidswitch - RTB", "classID": 4}])
        )
        mock_v3_api.get(f"{IX_BASE_URL}/api/accounts/v1/marketplaces/1491166/publishers").mock(
            return_value=httpx.Response(200, json=[{"legacyAccountID": 321, "name": "Publisher One"}])
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(201, json={"internalDealID": 9203, "externalDealID": "IXTEST9203"})

        mock_v3_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        _mock_inventory_group_lookup(mock_v3_api, "IXTEST9203")
        mock_v3_api.get(f"{IX_DEALS_V3_ENDPOINT}/9203").mock(return_value=httpx.Response(200, json={}))

        result = await ix_execute_deal_from_prompt_inputs(
            account_id=1491166,
            name="IX Default End Date",
            start_date="2026-04-24",
            floor=1.25,
            dsp_name="Bidswitch - RTB",
            publisher_names=["Publisher One"],
            deal_type="ctv",
        )

        assert result["success"] is True, result
        assert captured_request is not None

        payload = json.loads(captured_request.content)
        # Default end_date is +24 months (2-year rolling) per IX_DEFAULT_END_DATE_MONTHS.
        assert payload["endDate"] == "2028-04-24"
