"""Unit tests for OpenX structured quality_flags.

Covers helpers added in `feat/openx-quality-flags`:

- `_make_ox_quality_flag` (entry shape)
- `_blockers_to_ox_quality_flags` (blockers -> structured flags)

Plus the threading: `ox_create_prepared_deal` surfaces `quality_flags` for
both the prepared-not-found and ready-to-create=False early paths, and
the create-success path passes the prepared flags through.
"""

import pytest
from openx_mcp import (
    _blockers_to_ox_quality_flags,
    _make_ox_quality_flag,
    ox_create_prepared_deal,
)


class TestMakeOxQualityFlag:
    def test_required_fields(self):
        flag = _make_ox_quality_flag("ox_test", "an impact")
        assert flag == {"flag": "ox_test", "impact": "an impact"}

    def test_extra_context_kept(self):
        flag = _make_ox_quality_flag("ox_test", "msg", deal_id=5, name="X")
        assert flag["deal_id"] == 5
        assert flag["name"] == "X"

    def test_none_dropped(self):
        flag = _make_ox_quality_flag("ox_test", "msg", drop=None, keep="x")
        assert "drop" not in flag
        assert flag["keep"] == "x"


class TestBlockersToOxQualityFlags:
    def test_maps_code_and_message(self):
        blockers = [
            {"code": "demand_partner_unresolved", "message": "Unknown DSP", "details": {"input": "Acme"}},
            {"code": "missing_publisher_file", "message": "no file"},
        ]
        flags = _blockers_to_ox_quality_flags(blockers)
        assert flags[0]["flag"] == "ox_demand_partner_unresolved"
        assert flags[0]["impact"] == "Unknown DSP"
        assert flags[0]["input"] == "Acme"
        assert flags[1]["flag"] == "ox_missing_publisher_file"

    def test_already_prefixed_code_preserved(self):
        blockers = [{"code": "ox_already_prefixed", "message": "x"}]
        flags = _blockers_to_ox_quality_flags(blockers)
        assert flags[0]["flag"] == "ox_already_prefixed"

    def test_non_dict_skipped(self):
        flags = _blockers_to_ox_quality_flags([{"code": "ok", "message": "y"}, None, "garbage"])
        assert len(flags) == 1
        assert flags[0]["flag"] == "ox_ok"


class TestCreatePreparedDealQualityFlags:
    @pytest.mark.asyncio
    async def test_unknown_prepared_deal_id_emits_flag(self):
        result = await ox_create_prepared_deal("openx-prepared-nonexistent-uuid")
        assert result["success"] is False
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "ox_prepared_deal_not_found" in flag_names

    @pytest.mark.asyncio
    async def test_blocked_artifact_surfaces_quality_flags(self):
        import openx_mcp

        prepared_id = "openx-prepared-blocked-uuid"
        existing_flag = _make_ox_quality_flag("ox_test_blocker", "test impact")
        openx_mcp._prepared_openx_deals[prepared_id] = {
            "prepared_deal_id": prepared_id,
            "ready_to_create": False,
            "blocking_issues": ["bad input"],
            "blockers": [{"code": "test_blocker", "message": "bad input"}],
            "warnings": [],
            "quality_flags": [existing_flag],
            "create_args": {},
        }
        try:
            result = await ox_create_prepared_deal(prepared_id)
        finally:
            openx_mcp._prepared_openx_deals.pop(prepared_id, None)
        assert result["success"] is False
        assert "blocked" in result["error"].lower()
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "ox_test_blocker" in flag_names
