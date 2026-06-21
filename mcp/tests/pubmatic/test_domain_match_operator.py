"""Tests for PubMatic domain/app-bundle match-operator → domainMatchType.

PubMatic's domainMatchType enum is 1 = include (allowlist) and 2 = exclude
(blocklist). The MCP previously hardcoded 1; these tests lock in that an
explicit operator routes to the correct enum value and reaches the targeting
payload, so an allowlist file is never silently applied as an exclusion.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from pubmatic_mcp import (
    PUBMATIC_DOMAIN_MATCH_TYPE_EXCLUDE,
    PUBMATIC_DOMAIN_MATCH_TYPE_INCLUDE,
    _build_targeting_payload_from_prompt_inputs,
    _resolve_domain_match_type,
)


class TestResolveDomainMatchType:
    def test_allowlist_is_include_1(self):
        assert _resolve_domain_match_type("allowlist") == PUBMATIC_DOMAIN_MATCH_TYPE_INCLUDE == 1

    def test_blocklist_is_exclude_2(self):
        assert _resolve_domain_match_type("blocklist") == PUBMATIC_DOMAIN_MATCH_TYPE_EXCLUDE == 2

    def test_none_defaults_to_include(self):
        # Preserves the historical hardcoded behavior (matchType=1).
        assert _resolve_domain_match_type(None) == PUBMATIC_DOMAIN_MATCH_TYPE_INCLUDE

    def test_case_insensitive(self):
        assert _resolve_domain_match_type("AllowList") == 1
        assert _resolve_domain_match_type("  BLOCKLIST ") == 2

    def test_unknown_raises(self):
        with pytest.raises(ValueError, match="domain_match_operator"):
            _resolve_domain_match_type("whitelist")


class TestTargetingPayloadMatchType:
    @pytest.mark.asyncio
    async def test_blocklist_routes_to_exclude_in_payload(self):
        payload, _ = await _build_targeting_payload_from_prompt_inputs(
            logged_in_owner_type_id=7,
            publisher_ids=None,
            segment_ids=None,
            domains=["example.com", "com.zumobi.msnbc", "523428113"],
            geo_countries=None,
            geo_states=None,
            device_types=None,
            iab_category_ids=None,
            viewability_threshold=None,
            domain_match_type=PUBMATIC_DOMAIN_MATCH_TYPE_EXCLUDE,
        )
        assert payload["domainMatchType"] == 2
        assert set(payload["domainList"]) == {"example.com", "com.zumobi.msnbc", "523428113"}

    @pytest.mark.asyncio
    async def test_default_match_type_is_include(self):
        payload, _ = await _build_targeting_payload_from_prompt_inputs(
            logged_in_owner_type_id=7,
            publisher_ids=None,
            segment_ids=None,
            domains=["example.com"],
            geo_countries=None,
            geo_states=None,
            device_types=None,
            iab_category_ids=None,
            viewability_threshold=None,
        )
        assert payload["domainMatchType"] == 1
