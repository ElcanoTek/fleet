"""Tests for domain operator fix and segment targeting documentation.

These tests verify:
1. _build_domain_targeting_object defaults to NONE_OF (blocklist)
2. _build_domain_targeting_object accepts explicit operator parameter
3. _build_domain_targeting_object rejects invalid operators
4. ix_create_marketplace_deal accepts domain_operator parameter
5. Segment targeting objects pass validation in _build_marketplace_deal_payload
"""

import hashlib
import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    _build_domain_targeting_object,
    _build_marketplace_deal_payload,
    _clear_file_domain_expectation_locks_for_tests,
    _extract_domains_from_csv,
    _extract_domains_from_xlsx,
    _normalize_domain_operator,
    ix_create_marketplace_deal,
)

from .conftest import IX_ACCOUNTS_ENDPOINT, IX_BASE_URL, IX_LOGIN_ENDPOINT

IX_DEALS_V3_ENDPOINT = f"{IX_BASE_URL}/api/deals/v3/deals"
IX_TARGETING_KEYS_ENDPOINT = f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups/targets"


@pytest.fixture
def mock_deal_api(mock_ix_api: respx.MockRouter) -> respx.MockRouter:
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


class TestNormalizeDomainOperator:
    """Tests for the _normalize_domain_operator wrapper-level helper."""

    @pytest.mark.parametrize(
        ("value", "expected"),
        [
            ("allowlist", "ANY_OF"),
            ("ALLOWLIST", "ANY_OF"),
            ("allow", "ANY_OF"),
            ("include", "ANY_OF"),
            ("inclusion", "ANY_OF"),
            ("ANY_OF", "ANY_OF"),
            ("any_of", "ANY_OF"),
            ("blocklist", "NONE_OF"),
            ("block", "NONE_OF"),
            ("exclude", "NONE_OF"),
            ("exclusion", "NONE_OF"),
            ("NONE_OF", "NONE_OF"),
            ("  AllowList  ", "ANY_OF"),
        ],
    )
    def test_normalizes_known_aliases(self, value, expected):
        assert _normalize_domain_operator(value) == expected

    def test_rejects_unknown_operator(self):
        with pytest.raises(ValueError, match="domain_match_operator"):
            _normalize_domain_operator("whitelist-ish")

    def test_rejects_none(self):
        with pytest.raises(ValueError, match="domain_match_operator"):
            _normalize_domain_operator(None)


class TestAppBundleExtraction:
    """App-bundle IDs are only accepted when allow_app_bundle_ids=True.

    Regression guard: CTV/OTT lists carry bare numeric store IDs (Roku/Apple/
    Amazon) and dotted bundles with numeric final labels, which the web-domain
    validator silently dropped (observed: 739/912 bundles lost).
    """

    @staticmethod
    def _write_csv(tmp_path, *values):
        path = tmp_path / "bundles.csv"
        path.write_text("Bundle ID\n" + "\n".join(values) + "\n", encoding="utf-8")
        return str(path)

    def test_numeric_ids_dropped_without_app_bundle_mode(self, tmp_path):
        path = self._write_csv(tmp_path, "com.foo.bar", "523428113", "711586")
        result = _extract_domains_from_csv(path, column_name="Bundle ID")
        assert "com.foo.bar" in result["domains"]
        assert "523428113" not in result["domains"]
        assert set(result["invalid_values"]) == {"523428113", "711586"}

    def test_numeric_ids_accepted_in_app_bundle_mode(self, tmp_path):
        path = self._write_csv(tmp_path, "com.foo.bar", "523428113", "711586")
        result = _extract_domains_from_csv(path, column_name="Bundle ID", allow_app_bundle_ids=True)
        assert {"com.foo.bar", "523428113", "711586"} <= set(result["domains"])
        assert result["invalid_values"] == []

    def test_dotted_numeric_label_accepted_only_in_app_bundle_mode(self, tmp_path):
        # com.example.app2 ends in a numeric-bearing label -> DOMAIN_PATTERN rejects it.
        path = self._write_csv(tmp_path, "com.example.app2")
        assert _extract_domains_from_csv(path, column_name="Bundle ID")["domains"] == []
        bundle = _extract_domains_from_csv(path, column_name="Bundle ID", allow_app_bundle_ids=True)
        assert "com.example.app2" in bundle["domains"]

    def test_bare_tokens_still_rejected_in_app_bundle_mode(self, tmp_path):
        # Bare app-name tokens (no dot, not numeric) stay rejected to guard wrong-column picks.
        path = self._write_csv(tmp_path, "kbzk", "bloomberg")
        result = _extract_domains_from_csv(path, column_name="Bundle ID", allow_app_bundle_ids=True)
        assert result["domains"] == []
        assert set(result["invalid_values"]) == {"kbzk", "bloomberg"}

    def test_xlsx_numeric_ids_coerced_and_accepted(self, tmp_path):
        # openpyxl yields ints/floats for numeric cells; integral floats must not
        # persist a trailing ".0" (e.g. 711586.0 -> "711586").
        from openpyxl import Workbook

        workbook = Workbook()
        worksheet = workbook.active
        worksheet.append(["Bundle ID"])
        worksheet.append(["com.foo.bar"])
        worksheet.append([523428113])  # stored as int
        worksheet.append([711586.0])  # stored as float
        path = tmp_path / "bundles.xlsx"
        workbook.save(str(path))

        result = _extract_domains_from_xlsx(str(path), column_name="Bundle ID", allow_app_bundle_ids=True)
        assert {"com.foo.bar", "523428113", "711586"} <= set(result["domains"])
        assert "711586.0" not in result["domains"]


class TestBuildDomainTargetingObject:
    """Tests for the _build_domain_targeting_object helper."""

    def test_defaults_to_none_of_operator(self):
        """Domain targeting should default to NONE_OF (exclusion/blocklist)."""
        result = _build_domain_targeting_object(["example.com", "test.org"])
        assert result["targetingKeyID"] == 120
        assert result["keyName"] == "Domain"
        assert result["targetingType"] == "standard"
        assert len(result["sets"]) == 1
        assert result["sets"][0]["operator"] == "NONE_OF"
        assert len(result["sets"][0]["values"]) == 2
        assert result["sets"][0]["values"][0] == {"value": "example.com"}
        assert result["sets"][0]["values"][1] == {"value": "test.org"}

    def test_explicit_none_of_operator(self):
        """Explicit NONE_OF should work for blocklists."""
        result = _build_domain_targeting_object(["blocked.com"], operator="NONE_OF")
        assert result["sets"][0]["operator"] == "NONE_OF"

    def test_explicit_any_of_operator(self):
        """Explicit ANY_OF should work for allowlists."""
        result = _build_domain_targeting_object(["allowed.com"], operator="ANY_OF")
        assert result["sets"][0]["operator"] == "ANY_OF"

    def test_rejects_invalid_operator(self):
        """Invalid operator values should raise ValueError."""
        with pytest.raises(ValueError, match="operator must be"):
            _build_domain_targeting_object(["example.com"], operator="INCLUDE")

    def test_rejects_lowercase_operator(self):
        """Operators must be uppercase."""
        with pytest.raises(ValueError, match="operator must be"):
            _build_domain_targeting_object(["example.com"], operator="none_of")

    def test_empty_domain_list(self):
        """Empty domain list should produce empty values array."""
        result = _build_domain_targeting_object([])
        assert result["sets"][0]["values"] == []


class TestBuildPayloadWithSegmentTargeting:
    """Tests that segment targeting objects pass validation."""

    def test_segment_inclusion_targeting_accepted(self):
        """Segment inclusion targeting with ANY_OF should be accepted."""
        targeting = [
            {
                "targetingKeyID": 9,
                "keyName": "Country",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": "375"}]}],
            },
            {
                "targetingKeyID": 42,
                "keyName": "Segment",
                "targetingType": "standard",
                "sets": [
                    {
                        "operator": "ANY_OF",
                        "values": [{"value": "280"}, {"value": "3007"}],
                    }
                ],
            },
        ]
        payload = _build_marketplace_deal_payload(
            account_id=1499155,
            name="Test Segment Deal",
            external_deal_id="ELCANO-SEG-001",
            start_date="2026-03-27",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=198,
            targeting=targeting,
        )
        assert len(payload["targeting"]) == 2
        segment_obj = payload["targeting"][1]
        assert segment_obj["targetingKeyID"] == 42
        assert segment_obj["sets"][0]["operator"] == "ANY_OF"
        assert len(segment_obj["sets"][0]["values"]) == 2

    def test_segment_exclusion_targeting_accepted(self):
        """Segment exclusion targeting with NONE_OF should be accepted."""
        targeting = [
            {
                "targetingKeyID": 42,
                "keyName": "Segment",
                "targetingType": "standard",
                "sets": [
                    {
                        "operator": "NONE_OF",
                        "values": [{"value": "308129"}],
                    }
                ],
            },
        ]
        payload = _build_marketplace_deal_payload(
            account_id=1499155,
            name="Test Segment Exclusion",
            external_deal_id="ELCANO-SEG-002",
            start_date="2026-03-27",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=198,
            targeting=targeting,
        )
        assert len(payload["targeting"]) == 1
        segment_obj = payload["targeting"][0]
        assert segment_obj["sets"][0]["operator"] == "NONE_OF"
        assert segment_obj["sets"][0]["values"][0]["value"] == "308129"

    def test_combined_segment_include_and_exclude(self):
        """Both segment inclusion and exclusion in same payload should work."""
        targeting = [
            {
                "targetingKeyID": 3,
                "keyName": "DeviceType",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": "10"}]}],
            },
            {
                "targetingKeyID": 42,
                "keyName": "Segment",
                "targetingType": "standard",
                "sets": [
                    {
                        "operator": "ANY_OF",
                        "values": [{"value": "280"}, {"value": "3007"}],
                    }
                ],
            },
            {
                "targetingKeyID": 42,
                "keyName": "Segment",
                "targetingType": "standard",
                "sets": [
                    {
                        "operator": "NONE_OF",
                        "values": [{"value": "308129"}],
                    }
                ],
            },
        ]
        payload = _build_marketplace_deal_payload(
            account_id=1499155,
            name="Test Combined Segments",
            external_deal_id="ELCANO-SEG-003",
            start_date="2026-03-27",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=198,
            targeting=targeting,
        )
        assert len(payload["targeting"]) == 3
        include_seg = payload["targeting"][1]
        exclude_seg = payload["targeting"][2]
        assert include_seg["sets"][0]["operator"] == "ANY_OF"
        assert exclude_seg["sets"][0]["operator"] == "NONE_OF"


class TestCreateDealWithDomainOperator:
    """Tests for ix_create_marketplace_deal with domain_operator parameter."""

    @pytest.mark.asyncio
    async def test_domain_source_uses_none_of_by_default(self, mock_deal_api: respx.MockRouter, tmp_path):
        """When domain_source is provided without domain_operator, NONE_OF should be used."""
        domains = ["example.com", "test.org"]
        domain_file = tmp_path / "blocklist.csv"
        domain_file.write_text("domain\nexample.com\ntest.org\n")

        expected_fingerprint = hashlib.sha256(
            json.dumps(sorted(set(domains)), separators=(",", ":")).encode()
        ).hexdigest()

        mock_deal_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1499155,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_deal_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": [
                        {"targetingKeyID": 3, "key": "DeviceType"},
                        {"targetingKeyID": 120, "key": "Domain"},
                    ]
                },
            )
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(
                201,
                json={"internalDealID": 9999, "externalDealID": "TEST-DOM-001"},
            )

        mock_deal_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        mock_deal_api.get(f"{IX_DEALS_V3_ENDPOINT}/9999").mock(
            return_value=httpx.Response(
                200,
                json={
                    "internalDealID": 9999,
                    "targeting": [
                        {
                            "targetingKeyID": 120,
                            "keyName": "Domain",
                            "sets": [
                                {
                                    "operator": "NONE_OF",
                                    "values": [
                                        {"value": "example.com"},
                                        {"value": "test.org"},
                                    ],
                                }
                            ],
                        }
                    ],
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=1499155,
            name="Test Domain Blocklist",
            external_deal_id="TEST-DOM-001",
            start_date="2026-03-27",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=198,
            domain_source=str(domain_file),
            expected_domain_count=2,
            expected_domains_fingerprint=expected_fingerprint,
        )

        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)

        # Find the domain targeting object
        domain_targeting = None
        for t in payload.get("targeting", []):
            if t.get("targetingKeyID") == 120:
                domain_targeting = t
                break

        assert domain_targeting is not None, "Domain targeting object should be in payload"
        assert domain_targeting["sets"][0]["operator"] == "NONE_OF", (
            "Default domain_operator should be NONE_OF (blocklist)"
        )

    @pytest.mark.asyncio
    async def test_domain_source_with_explicit_any_of(self, mock_deal_api: respx.MockRouter, tmp_path):
        """When domain_operator='ANY_OF' is passed, domains should use ANY_OF."""
        domains = ["allowed.com", "permitted.org"]
        domain_file = tmp_path / "allowlist.csv"
        domain_file.write_text("domain\nallowed.com\npermitted.org\n")

        expected_fingerprint = hashlib.sha256(
            json.dumps(sorted(set(domains)), separators=(",", ":")).encode()
        ).hexdigest()

        mock_deal_api.get(IX_ACCOUNTS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1499155,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )
        )
        mock_deal_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": [
                        {"targetingKeyID": 3, "key": "DeviceType"},
                        {"targetingKeyID": 120, "key": "Domain"},
                    ]
                },
            )
        )

        captured_request = None

        def capture_create(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(
                201,
                json={"internalDealID": 9998, "externalDealID": "TEST-DOM-002"},
            )

        mock_deal_api.post(IX_DEALS_V3_ENDPOINT).mock(side_effect=capture_create)
        mock_deal_api.get(f"{IX_DEALS_V3_ENDPOINT}/9998").mock(
            return_value=httpx.Response(
                200,
                json={
                    "internalDealID": 9998,
                    "targeting": [
                        {
                            "targetingKeyID": 120,
                            "keyName": "Domain",
                            "sets": [
                                {
                                    "operator": "ANY_OF",
                                    "values": [
                                        {"value": "allowed.com"},
                                        {"value": "permitted.org"},
                                    ],
                                }
                            ],
                        }
                    ],
                },
            )
        )

        result = await ix_create_marketplace_deal(
            account_id=1499155,
            name="Test Domain Allowlist",
            external_deal_id="TEST-DOM-002",
            start_date="2026-03-27",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=198,
            domain_source=str(domain_file),
            domain_operator="ANY_OF",
            expected_domain_count=2,
            expected_domains_fingerprint=expected_fingerprint,
        )

        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)

        domain_targeting = None
        for t in payload.get("targeting", []):
            if t.get("targetingKeyID") == 120:
                domain_targeting = t
                break

        assert domain_targeting is not None
        assert domain_targeting["sets"][0]["operator"] == "ANY_OF", (
            "Explicit domain_operator='ANY_OF' should produce ANY_OF operator"
        )
