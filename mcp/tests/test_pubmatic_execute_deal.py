import os
import sys

import pytest
from openpyxl import Workbook

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import pubmatic_mcp


@pytest.fixture(autouse=True)
def _isolate_pubmatic_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so we never pick up live-run artifacts."""
    cache_root = tmp_path_factory.mktemp("pmcache")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    # In-process caches must also start empty.
    pubmatic_mcp._iab_taxonomy_cache = None
    pubmatic_mcp._dsp_list_cache = None
    yield
    pubmatic_mcp._iab_taxonomy_cache = None
    pubmatic_mcp._dsp_list_cache = None


class TestPmExecuteDealFromPromptInputs:
    @pytest.mark.asyncio
    async def test_executes_auction_package_flow_with_manual_publishers(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path
    ):
        workbook_path = tmp_path / "domains.xlsx"
        workbook = Workbook()
        sheet = workbook.active
        sheet.title = "Domains"
        sheet.append(["domain"])
        sheet.append(["example.com"])
        sheet.append(["https://www.valid.org/path"])
        sheet.append(["not-a-domain"])
        workbook.save(workbook_path)

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            assert kwargs["logged_in_owner_type_id"] == 5
            assert kwargs["seat_id"] == "seat-9"
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "seat-9",
            }, []

        async def fake_resolve_named_entities(**kwargs):
            if kwargs["kind"] == "publisher":
                return [901], []
            if kwargs["kind"] == "segment":
                return [801], []
            raise AssertionError("unexpected kind")

        async def fake_resolve_geo_ids(**kwargs):
            assert kwargs["geo_countries"] == ["us"]
            assert kwargs["geo_states"] == ["ca"]
            return [1001, 2002], []

        async def fake_create_targeting(payload):
            assert payload == {
                "ownerId": pubmatic_mcp.ELCANO_OWNER_ID,
                "ownerType": 5,
                "deviceMakeTargeting": 0,
                "audienceSegments": [{"id": 801}],
                "domainList": ["example.com", "valid.org"],
                "domainMatchType": 1,
                "geos": [1001, 2002],
                "deviceType": [7, 1],
                "iabCategories": [100, 200],
                "minViewabilityValue": 70,
            }
            return {"success": True, "targeting_id": 777, "result": {"id": 777}}

        async def fake_create_curated_deal(payload):
            assert payload["dealId"] == "PM-123"
            assert payload["targeting"] == 777
            assert payload["flooreCPM"] == 2.5
            assert payload["platforms"] == [1, 2]
            assert payload["adFormats"] == [3, 13]
            assert payload["priority"] == 7
            assert payload["hasMaxReach"] == 0
            assert payload["pubIds"] == [901]
            assert payload["publisherBlockList"] == [999]
            assert payload["startDate"] == "2026-04-01T00:00:00.000Z"
            assert payload["endDate"] == "2026-04-30T00:00:00.000Z"
            assert payload["dealDspBuyerMappings"] == [{"dspId": 42, "buyerId": 314, "seatId": "seat-9"}]
            return {"success": True, "result": {"id": 555, "dealId": "PM-123", "name": payload["name"]}}

        async def fake_get_curated_deal(**kwargs):
            assert kwargs["curated_id"] == 555
            return {"success": True, "result": {"id": 555, "dealId": "PM-123"}}

        async def fake_resolve_iab_categories(values):
            return [int(v) for v in values], []

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_named_entities", fake_resolve_named_entities)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_geo_ids", fake_resolve_geo_ids)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_iab_categories", fake_resolve_iab_categories)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_targeting", fake_create_targeting)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", fake_create_curated_deal)
        monkeypatch.setattr(pubmatic_mcp, "pm_get_curated_deal", fake_get_curated_deal)

        result = await pubmatic_mcp.pm_execute_deal_from_prompt_inputs(
            name="PubMatic Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            seat_id="seat-9",
            logged_in_owner_type_id=5,
            deal_id="PM-123",
            floor_ecpm=2.5,
            publisher_names=["Publisher A"],
            segment_names=["Sports Fans"],
            domain_file_path=str(workbook_path),
            domain_sheet="Domains",
            domain_column="domain",
            geo_countries=["us"],
            geo_states=["ca"],
            device_types=["Connected TV", "desktop"],
            iab_categories=[100, 200],
            viewability_threshold=70,
            ad_formats=[3, 13],
            platforms=[1, 2],
            priority=7,
            publisher_block_list=[999],
        )

        assert result["success"] is True
        assert result["targeting_id"] == 777
        assert result["deal"]["id"] == 555
        assert result["verification"]["success"] is True
        # The 30% curator-fee default fires whenever fee= is None, so its
        # warning lands at index 0 alongside the existing domain warning.
        # Warning text varies per MCP variant (Elcano default, Reklaim
        # variant, etc.) — ELCANO_FEE_RECIPIENT_NAME is aliased to the
        # variant-aware DEFAULT_FEE_RECIPIENT_NAME at module load.
        expected_fee_warning = (
            f"Applied default {pubmatic_mcp.ELCANO_FEE_RECIPIENT_NAME} curator fee: "
            f"{pubmatic_mcp.ELCANO_DEFAULT_FEE_VALUE_PERCENT}% of media. "
            "Pass fee= to override."
        )
        # No domain_match_operator was passed, so it defaults to allowlist
        # (domainMatchType=1, include) and the resolver echoes the applied
        # match type for verifiability.
        assert result["warnings"] == [
            expected_fee_warning,
            f"Dropped 1 invalid domains from {workbook_path}.",
            f"Applied domainMatchType=1 (include/allowlist) to 2 values from {workbook_path}.",
        ]
        # Quality flags should also surface the auto-applied fee.
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "pm_default_curator_fee_applied" in flag_names
        # Each prepare-phase quality flag must appear exactly once in the
        # execute response. pm_create_prepared_deal already seeds its
        # quality_flags from the prepared artifact, so concatenating the
        # preparation list a second time would double them.
        assert flag_names.count("pm_default_curator_fee_applied") == 1
        # PubMatic UI requires dealId + dealName + dealCategoryId=3 query params
        # to navigate directly to the deal. Spaces in the name are url-encoded.
        assert result["deal_url"] == (
            "https://apps.pubmatic.com/v3/common/pmc/deals?dealId=555&dealName=PubMatic%20Deal&dealCategoryId=3"
        )
        assert result["error"] is None

    @pytest.mark.asyncio
    async def test_skips_targeting_when_no_targeting_inputs(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "",
            }, []

        async def fake_resolve_named_entities(**kwargs):
            return [901], []

        async def fake_create_curated_deal(payload):
            assert payload["targeting"] is None
            assert payload["pubIds"] == [901]
            assert payload["hasMaxReach"] == 0
            return {"success": True, "result": {"id": 444, "dealId": payload["dealId"]}}

        async def fake_get_curated_deal(**kwargs):
            return {"success": True, "result": {"id": 444, "dealId": "PM-444"}}

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_named_entities", fake_resolve_named_entities)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", fake_create_curated_deal)
        monkeypatch.setattr(pubmatic_mcp, "pm_get_curated_deal", fake_get_curated_deal)

        result = await pubmatic_mcp.pm_execute_deal_from_prompt_inputs(
            name="Untargeted Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
            publisher_names=["Publisher A"],
        )

        assert result["success"] is True
        assert result["targeting_id"] is None

    @pytest.mark.asyncio
    async def test_execute_does_not_duplicate_prepare_phase_quality_flags(self, monkeypatch: pytest.MonkeyPatch):
        """Regression: prepare-phase quality_flags (e.g. the auto-applied
        curator fee, channel device defaults) used to appear twice in the
        execute response because pm_create_prepared_deal already seeds its
        list from the prepared artifact and pm_execute_deal_from_prompt_inputs
        was concatenating the preparation list a second time."""

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "",
            }, []

        async def fake_resolve_named_entities(**kwargs):
            return [901], []

        async def fake_create_targeting(payload):
            return {"success": True, "targeting_id": 777, "result": {"id": 777}}

        async def fake_create_curated_deal(payload):
            return {"success": True, "result": {"id": 999, "dealId": payload["dealId"], "name": payload["name"]}}

        async def fake_get_curated_deal(**kwargs):
            return {"success": True, "result": {"id": 999}}

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_named_entities", fake_resolve_named_entities)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_targeting", fake_create_targeting)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", fake_create_curated_deal)
        monkeypatch.setattr(pubmatic_mcp, "pm_get_curated_deal", fake_get_curated_deal)

        result = await pubmatic_mcp.pm_execute_deal_from_prompt_inputs(
            name="Dedupe Test",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
            deal_id="PM-DEDUPE",
            publisher_names=["Publisher A"],
            # Force BOTH prepare-phase defaults to fire: omit channel device
            # defaults trigger via channel=, and the fee default fires when
            # fee= is None.
            channel="display",
        )

        assert result["success"] is True
        flag_names = [f["flag"] for f in result["quality_flags"]]
        # Both prepare-phase defaults should appear EXACTLY once.
        assert flag_names.count("pm_default_curator_fee_applied") == 1
        assert flag_names.count("pm_default_channel_devices_applied") == 1

    @pytest.mark.asyncio
    async def test_requires_publishers_for_manual_mode(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "",
            }, []

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)

        result = await pubmatic_mcp.pm_execute_deal_from_prompt_inputs(
            name="Untargeted Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
        )

        assert result["success"] is False
        assert result["error"] == "publisher_ids or resolvable publisher_names are required when has_max_reach=0"

    @pytest.mark.asyncio
    async def test_validates_platforms_and_ad_formats(self):
        # 7 is CTV per the Curated Deals docs (audited 2026-06-12) and MUST be
        # accepted — the old allowed set wrongly rejected it. 3 ("Not Defined")
        # and out-of-range ids stay rejected.
        assert pubmatic_mcp._validate_allowed_ids([7], pubmatic_mcp.PUBMATIC_ALLOWED_PLATFORM_IDS, "platforms") == [7]

        with pytest.raises(ValueError, match="platforms contains unsupported values"):
            pubmatic_mcp._validate_allowed_ids([3], pubmatic_mcp.PUBMATIC_ALLOWED_PLATFORM_IDS, "platforms")

        with pytest.raises(ValueError, match="ad_formats contains unsupported values"):
            pubmatic_mcp._validate_allowed_ids([99], pubmatic_mcp.PUBMATIC_ALLOWED_AD_FORMAT_IDS, "ad_formats")

    @pytest.mark.asyncio
    async def test_prepare_returns_blockers_without_calling_apis(self, monkeypatch: pytest.MonkeyPatch):
        """Preparation surfaces blockers without ever calling create_targeting/create_curated_deal."""

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "",
            }, []

        async def must_not_be_called(*args, **kwargs):
            raise AssertionError("create API must not be invoked during preparation")

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_targeting", must_not_be_called)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", must_not_be_called)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Blocked Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
        )

        assert prepared["success"] is True
        assert prepared["ready_to_create"] is False
        assert any(blocker["code"] == "publishers_required_for_manual_mode" for blocker in prepared["blockers"])
        assert prepared["prepared_deal_id"] in pubmatic_mcp._prepared_pubmatic_deals

    @pytest.mark.asyncio
    async def test_create_prepared_deal_refuses_blocked_artifact(self, monkeypatch: pytest.MonkeyPatch):
        """Submit must refuse a blocked prepared artifact and never hit the PubMatic API."""

        async def must_not_be_called(*args, **kwargs):
            raise AssertionError("create API must not be invoked when artifact is blocked")

        monkeypatch.setattr(pubmatic_mcp, "pm_create_targeting", must_not_be_called)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", must_not_be_called)

        prepared_deal_id = "pubmatic-prepared-test-blocked"
        pubmatic_mcp._prepared_pubmatic_deals[prepared_deal_id] = {
            "prepared_deal_id": prepared_deal_id,
            "ready_to_create": False,
            "blocking_issues": ["DSP not found: Bogus DSP"],
            "blockers": [{"code": "dsp_buyer_unresolved", "message": "DSP not found: Bogus DSP"}],
            "warnings": [],
            "resolved_entities": {},
            "targeting_intent": None,
            "deal_intent": {},
            "logged_in_owner_type_id": 5,
        }

        try:
            result = await pubmatic_mcp.pm_create_prepared_deal(prepared_deal_id)
        finally:
            pubmatic_mcp._prepared_pubmatic_deals.pop(prepared_deal_id, None)

        assert result["success"] is False
        assert result["error"] == "Prepared PubMatic deal is blocked and cannot be created."
        assert result["blocking_issues"] == ["DSP not found: Bogus DSP"]
        assert result["deal"] is None
        assert result["targeting_id"] is None

    @pytest.mark.asyncio
    async def test_dsp_unresolved_blocker_includes_candidates(self, monkeypatch: pytest.MonkeyPatch):
        """When DSP resolution fails, the blocker must include the available DSP names so the agent can pick a real one."""

        async def fake_list_all_pmp_dsps(logged_in_owner_type_id: int):
            return [
                {"id": 377, "name": "The Trade Desk"},
                {"id": 26, "name": "DV360"},
                {"id": 80, "name": "Xandr DSP"},
            ]

        monkeypatch.setattr(pubmatic_mcp, "_list_all_pmp_dsps", fake_list_all_pmp_dsps)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Test Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="TRADR",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is False
        dsp_blocker = next(b for b in prepared["blockers"] if b["code"] == "dsp_buyer_unresolved")
        assert "TRADR" in dsp_blocker["message"]
        assert dsp_blocker["details"]["available_dsps"] == ["The Trade Desk", "DV360", "Xandr DSP"]
        assert dsp_blocker["details"]["available_dsp_count"] == 3
        assert dsp_blocker["details"]["dsp_name"] == "TRADR"

    @pytest.mark.asyncio
    async def test_buyer_seat_unresolved_blocker_includes_candidates(self, monkeypatch: pytest.MonkeyPatch):
        """When buyer-seat resolution fails, the blocker must include the buyer-seat pairs PubMatic returned."""

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            raise pubmatic_mcp.PubMaticResolutionError(
                "Buyer-seat mapping not found for buyer 'BrightArc Media' and seat '393'",
                dsp_id=42,
                dsp_name="The Trade Desk",
                buyer_name="BrightArc Media",
                seat_id="393",
                available_buyer_seats=[
                    {"buyerName": "Acme Trading", "seatId": "393"},
                    {"buyerName": "Brand X", "seatId": "412"},
                ],
                available_buyer_seat_count=2,
            )

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Test Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="BrightArc Media",
            seat_id="393",
            logged_in_owner_type_id=5,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is False
        dsp_blocker = next(b for b in prepared["blockers"] if b["code"] == "dsp_buyer_unresolved")
        assert dsp_blocker["details"]["dsp_name"] == "The Trade Desk"
        assert dsp_blocker["details"]["available_buyer_seats"] == [
            {"buyerName": "Acme Trading", "seatId": "393"},
            {"buyerName": "Brand X", "seatId": "412"},
        ]
        assert dsp_blocker["details"]["available_buyer_seat_count"] == 2

    @pytest.mark.asyncio
    async def test_raw_dsp_buyer_ids_skip_dsp_listing(self, monkeypatch: pytest.MonkeyPatch):
        """When dsp_id is supplied the DSP list endpoint is skipped, but seat-aware buyer
        validation still runs to catch the dspBuyerId-vs-canonical-id confusion."""

        async def dsp_list_must_not_run(*args, **kwargs):
            raise AssertionError("DSP listing must not run when dsp_id is supplied")

        async def fake_buyer_map(*, dsp_id, query=None, **kwargs):
            return {
                "success": True,
                "items": [
                    {
                        "dspId": dsp_id,
                        "dspName": "Test DSP",
                        "buyerId": 314,
                        "buyerName": "Acme Buyer",
                        "seatId": "bidswitch_393",
                        "dspBuyerId": 393,
                    }
                ],
            }

        monkeypatch.setattr(pubmatic_mcp, "_list_all_pmp_dsps", dsp_list_must_not_run)
        monkeypatch.setattr(pubmatic_mcp, "pm_discover_dsp_buyer_map", fake_buyer_map)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Raw IDs Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_id=42,
            buyer_id=314,
            seat_id="bidswitch_393",
            logged_in_owner_type_id=5,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is True
        mapping = prepared["resolved_entities"]["dsp_buyer_mapping"]
        assert mapping["dsp_id"] == 42
        assert mapping["buyer_id"] == 314
        assert mapping["seat_id"] == "bidswitch_393"
        assert prepared["resolved_entities"]["dsp_buyer_source"] == "raw_ids"
        assert prepared["deal_intent_preview"]["dealDspBuyerMappings"] == [
            {"dspId": 42, "buyerId": 314, "seatId": "bidswitch_393"}
        ]

    @pytest.mark.asyncio
    async def test_raw_buyer_id_corrects_dsp_buyer_id_to_canonical(self, monkeypatch: pytest.MonkeyPatch):
        """When the user passes the dspBuyerId as buyer_id (e.g. seat number 393),
        the resolver corrects it to PubMatic's canonical buyer.id and warns."""

        async def fake_buyer_map(*, dsp_id, query=None, **kwargs):
            return {
                "success": True,
                "items": [
                    {
                        "dspId": dsp_id,
                        "dspName": "The Trade Desk",
                        "buyerId": 33307,
                        "buyerName": "393",
                        "seatId": "393",
                        "dspBuyerId": 393,
                    }
                ],
            }

        monkeypatch.setattr(pubmatic_mcp, "pm_discover_dsp_buyer_map", fake_buyer_map)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Buyer ID Correction",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_id=377,
            buyer_id=393,
            seat_id="393",
            logged_in_owner_type_id=7,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is True
        mapping = prepared["resolved_entities"]["dsp_buyer_mapping"]
        assert mapping["buyer_id"] == 33307, "should resolve to canonical buyer.id, not the dspBuyerId"
        assert any("dspBuyerId" in w and "33307" in w for w in prepared["warnings"])

    @pytest.mark.asyncio
    async def test_hybrid_dsp_name_with_raw_buyer_id(self, monkeypatch: pytest.MonkeyPatch):
        """dsp_name + raw buyer_id: resolve DSP by name, validate buyer_id against the seat mapping."""

        async def fake_list_all_pmp_dsps(logged_in_owner_type_id: int):
            return [{"id": 377, "name": "The Trade Desk"}, {"id": 26, "name": "DV360"}]

        async def fake_buyer_map(*, dsp_id, query=None, **kwargs):
            return {
                "success": True,
                "items": [
                    {
                        "dspId": dsp_id,
                        "dspName": "The Trade Desk",
                        "buyerId": 393,
                        "buyerName": "Acme",
                        "seatId": "393",
                        "dspBuyerId": 393,
                    }
                ],
            }

        monkeypatch.setattr(pubmatic_mcp, "_list_all_pmp_dsps", fake_list_all_pmp_dsps)
        monkeypatch.setattr(pubmatic_mcp, "pm_discover_dsp_buyer_map", fake_buyer_map)

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Hybrid Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_id=393,
            seat_id="393",
            logged_in_owner_type_id=5,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is True
        mapping = prepared["resolved_entities"]["dsp_buyer_mapping"]
        assert mapping["dsp_id"] == 377
        assert mapping["dsp_name"] == "The Trade Desk"
        assert mapping["buyer_id"] == 393
        assert prepared["resolved_entities"]["dsp_buyer_source"] == "hybrid"

    @pytest.mark.asyncio
    async def test_missing_dsp_buyer_completely(self):
        """Neither names nor raw IDs -> structured blocker, no resolver call."""
        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="No DSP",
            start_date="2026-04-01",
            end_date="2026-04-30",
            logged_in_owner_type_id=5,
            has_max_reach=1,
        )

        assert prepared["ready_to_create"] is False
        codes = [b["code"] for b in prepared["blockers"]]
        assert "missing_dsp_buyer" in codes

    @pytest.mark.asyncio
    async def test_audience_segments_resolve_via_buyer_insights(self, monkeypatch: pytest.MonkeyPatch):
        """Segment names resolve via /v1/audience/buyerInsights/audiences (the documented Audience API for Buyers)."""

        captured_search_keys: list[str] = []

        class FakeClient:
            async def list_buyer_audiences(self, *, search_key, **_kwargs):
                captured_search_keys.append(search_key)
                key_lower = search_key.lower()
                # _audience_search_key picks the longest non-stop word: "Purchase",
                # "Maintenance", "Atlantis" for these inputs.
                catalog = {
                    "purchase": [
                        {"audienceId": 9001, "audienceName": "In-Market: Vehicle Purchase Intent"},
                        {"audienceId": 9002, "audienceName": "Home Purchase Shoppers"},
                    ],
                    "maintenance": [
                        {"audienceId": 9101, "audienceName": "Auto Service & Maintenance Shoppers"},
                    ],
                    "atlantis": [],
                }
                return {"items": catalog.get(key_lower, [])}

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        ids, warnings = await pubmatic_mcp._resolve_audience_segments(
            [
                "In-Market: Vehicle Purchase Intent",
                "Auto Service & Maintenance Shoppers",
                "Atlantis Mariners",
            ],
            logged_in_owner_type_id=7,
        )

        assert ids == [9001, 9101]
        assert any("Atlantis Mariners" in w for w in warnings)
        # The longest non-stop word from each input got passed to PubMatic.
        assert "Purchase" in captured_search_keys
        assert "Maintenance" in captured_search_keys

    @pytest.mark.asyncio
    async def test_disk_cache_round_trip(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        """Disk cache writes the value then reads it back when called again."""
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("PUBMATIC_CACHE_TTL_SECONDS", "3600")
        # First read: empty cache.
        assert pubmatic_mcp._cache_get("unit_test_key") is None
        pubmatic_mcp._cache_put("unit_test_key", {"value": [1, 2, 3]})
        cached = pubmatic_mcp._cache_get("unit_test_key")
        assert cached == {"value": [1, 2, 3]}
        # File actually exists on disk where we expect.
        assert (tmp_path / "cutlass" / "pubmatic" / "unit_test_key.json").is_file()

    @pytest.mark.asyncio
    async def test_disk_cache_disabled_when_ttl_zero(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        """Setting PUBMATIC_CACHE_TTL_SECONDS=0 fully disables disk cache."""
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("PUBMATIC_CACHE_TTL_SECONDS", "0")
        pubmatic_mcp._cache_put("unit_test_key", {"value": "ignored"})
        assert pubmatic_mcp._cache_get("unit_test_key") is None
        assert not (tmp_path / "cutlass" / "pubmatic" / "unit_test_key.json").exists()

    @pytest.mark.asyncio
    async def test_iab_taxonomy_uses_disk_cache_on_second_call(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        """Second _load_iab_taxonomy in a fresh process must read from disk and skip the API."""
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("PUBMATIC_CACHE_TTL_SECONDS", "3600")

        call_count = {"n": 0}

        class FakeClient:
            async def list_iab_categories(self, **_kwargs):
                call_count["n"] += 1
                return {
                    "items": [
                        {"id": 51, "iabId": "IAB2", "iabName": "Automotive", "subCategoryList": []},
                    ]
                }

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        # Wipe in-process cache to force the disk path.
        pubmatic_mcp._iab_taxonomy_cache = None
        first = await pubmatic_mcp._load_iab_taxonomy()
        assert call_count["n"] == 1
        assert any(item.get("id") == 51 for item in first)

        # Simulate fresh process: drop the in-process cache; disk should still have it.
        pubmatic_mcp._iab_taxonomy_cache = None
        second = await pubmatic_mcp._load_iab_taxonomy()
        assert call_count["n"] == 1, "second call must hit disk cache, not the API"
        assert second == first

    @pytest.mark.asyncio
    async def test_resolve_iab_categories_by_name_code_and_id(self, monkeypatch: pytest.MonkeyPatch):
        """IAB resolver accepts names, IAB codes, mixed-format strings, and numeric IDs."""
        fake_taxonomy = [
            {"id": 51, "iabId": "IAB2", "name": "Automotive"},
            {"id": 52, "iabId": "IAB2-1", "name": "Auto Parts"},
            {"id": 56, "iabId": "IAB2-5", "name": "Certified Pre-Owned"},
            {"id": 173, "iabId": "IAB8", "name": "Food & Drink"},
        ]

        async def fake_load():
            return fake_taxonomy

        monkeypatch.setattr(pubmatic_mcp, "_load_iab_taxonomy", fake_load)
        pubmatic_mcp._iab_taxonomy_cache = None

        ids, warnings = await pubmatic_mcp._resolve_iab_categories(
            [
                "Automotive",
                "IAB2-1",
                "IAB8 Food & Drink",
                56,
                "52",
            ]
        )
        assert ids == [51, 52, 173, 56]
        assert warnings == []

    @pytest.mark.asyncio
    async def test_resolve_iab_categories_warns_on_code_name_mismatch(self, monkeypatch: pytest.MonkeyPatch):
        """When the user gives "IAB2-5 Automotive" (bad code, valid name), prefer the name and warn."""
        fake_taxonomy = [
            {"id": 51, "iabId": "IAB2", "name": "Automotive"},
            {"id": 56, "iabId": "IAB2-5", "name": "Certified Pre-Owned"},
        ]

        async def fake_load():
            return fake_taxonomy

        monkeypatch.setattr(pubmatic_mcp, "_load_iab_taxonomy", fake_load)
        pubmatic_mcp._iab_taxonomy_cache = None

        ids, warnings = await pubmatic_mcp._resolve_iab_categories(["IAB2-5 Automotive"])
        assert ids == [51]
        assert any("IAB2-5" in w and "Certified Pre-Owned" in w and "Automotive" in w for w in warnings)

    @pytest.mark.asyncio
    async def test_resolve_iab_categories_unresolved_raises_with_details(self, monkeypatch: pytest.MonkeyPatch):
        """Unresolved inputs raise PubMaticResolutionError with structured details."""
        fake_taxonomy = [{"id": 51, "iabId": "IAB2", "name": "Automotive"}]

        async def fake_load():
            return fake_taxonomy

        monkeypatch.setattr(pubmatic_mcp, "_load_iab_taxonomy", fake_load)
        pubmatic_mcp._iab_taxonomy_cache = None

        with pytest.raises(pubmatic_mcp.PubMaticResolutionError) as excinfo:
            await pubmatic_mcp._resolve_iab_categories(["Mystery Category", 99999])
        err = excinfo.value
        assert "Mystery Category" in err.message
        codes = [u["reason"] for u in err.details["unresolved"]]
        assert "no_match" in codes
        assert "id_not_in_taxonomy" in codes

    @pytest.mark.asyncio
    async def test_iab_unresolved_becomes_blocker_in_prepare(self, monkeypatch: pytest.MonkeyPatch):
        """Unresolved IAB inputs surface as iab_categories_unresolved blocker, not a generic error."""
        fake_taxonomy = [{"id": 51, "iabId": "IAB2", "name": "Automotive"}]

        async def fake_load():
            return fake_taxonomy

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 377,
                "dsp_name": "The Trade Desk",
                "buyer_id": 393,
                "buyer_name": "buyer_id=393",
                "seat_id": "393",
            }, []

        monkeypatch.setattr(pubmatic_mcp, "_load_iab_taxonomy", fake_load)
        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        pubmatic_mcp._iab_taxonomy_cache = None

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Bad IAB Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_id=377,
            buyer_id=393,
            seat_id="393",
            logged_in_owner_type_id=5,
            has_max_reach=1,
            iab_categories=["Mystery Category"],
        )
        assert prepared["ready_to_create"] is False
        codes = [b["code"] for b in prepared["blockers"]]
        assert "iab_categories_unresolved" in codes
        blocker = next(b for b in prepared["blockers"] if b["code"] == "iab_categories_unresolved")
        assert blocker["details"]["iab_categories"] == ["Mystery Category"]

    @pytest.mark.asyncio
    async def test_geo_api_502_becomes_structured_blocker(self, monkeypatch: pytest.MonkeyPatch):
        """A PubMatic 502 on the geo endpoint must produce geo_lookup_failed, not preparation_error."""
        import httpx

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 377,
                "dsp_name": "The Trade Desk",
                "buyer_id": 393,
                "buyer_name": "buyer_id=393",
                "seat_id": "393",
            }, []

        class FakeClient:
            async def list_geos(self, **_kwargs):
                request = httpx.Request("GET", "https://api.pubmatic.com/v1/common/geo")
                response = httpx.Response(502, request=request)
                raise httpx.HTTPStatusError("502 Bad Gateway", request=request, response=response)

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        prepared = await pubmatic_mcp.pm_prepare_deal_from_prompt_inputs(
            name="Geo Outage Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_id=377,
            buyer_id=393,
            seat_id="393",
            logged_in_owner_type_id=5,
            has_max_reach=1,
            geo_countries=["US"],
            geo_states=["California"],
        )

        assert prepared["ready_to_create"] is False
        codes = [b["code"] for b in prepared["blockers"]]
        assert "geo_lookup_failed" in codes
        geo_blocker = next(b for b in prepared["blockers"] if b["code"] == "geo_lookup_failed")
        assert geo_blocker["details"]["status"] == 502
        assert geo_blocker["details"]["geo_states"] == ["California"]

    @pytest.mark.asyncio
    async def test_create_prepared_deal_rejects_unknown_id(self):
        result = await pubmatic_mcp.pm_create_prepared_deal("pubmatic-prepared-does-not-exist")
        assert result["success"] is False
        assert "not found" in result["error"].lower()

    @pytest.mark.asyncio
    async def test_execute_wrapper_reports_prepare_phase_on_blockers(self, monkeypatch: pytest.MonkeyPatch):
        """The wrapper must short-circuit at phase=prepare without calling submit when prepared artifact is blocked."""

        async def fake_resolve_dsp_buyer_mapping(**kwargs):
            return {
                "dsp_id": 42,
                "dsp_name": "The Trade Desk",
                "buyer_id": 314,
                "buyer_name": "Acme Buyer",
                "seat_id": "",
            }, []

        async def must_not_be_called(*args, **kwargs):
            raise AssertionError("submit must not run when preparation has blockers")

        monkeypatch.setattr(pubmatic_mcp, "_resolve_dsp_buyer_mapping", fake_resolve_dsp_buyer_mapping)
        monkeypatch.setattr(pubmatic_mcp, "pm_create_prepared_deal", must_not_be_called)

        result = await pubmatic_mcp.pm_execute_deal_from_prompt_inputs(
            name="Blocked Deal",
            start_date="2026-04-01",
            end_date="2026-04-30",
            dsp_name="The Trade Desk",
            buyer_name="Acme Buyer",
            logged_in_owner_type_id=5,
        )

        assert result["success"] is False
        assert result["phase"] == "prepare"
        assert result["error"] == "publisher_ids or resolvable publisher_names are required when has_max_reach=0"
        assert result["deal"] is None
        assert result["targeting_id"] is None
        assert result["preparation"]["ready_to_create"] is False

    @pytest.mark.asyncio
    async def test_resolve_geo_ids_uses_filtered_lookup(self, monkeypatch: pytest.MonkeyPatch):
        """Geo resolver issues per-name filtered queries (name+geoLevel+countryCode), not a list-all."""
        calls: list[dict] = []

        class FakeClient:
            async def list_geos(self, **kwargs):
                calls.append(kwargs)
                if kwargs.get("geo_level") == 1:
                    return {"items": [{"id": 232, "name": "United States", "countryCode": "US"}]}
                if kwargs.get("geo_level") == 2 and kwargs.get("name_like", "").lower().startswith("ca"):
                    return {"items": [{"id": 5024, "name": "California", "countryCode": "US"}]}
                if kwargs.get("geo_level") == 2:
                    return {"items": []}
                return {"items": []}

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        geo_ids, warnings = await pubmatic_mcp._resolve_geo_ids(
            geo_countries=["US"],
            geo_states=["California", "Atlantis"],
        )

        assert geo_ids == [232, 5024]
        assert warnings == ["Could not resolve PubMatic geo IDs for: Atlantis."]
        # Country lookup used country_code path; state lookup used name_like + country_code + geoLevel=2
        country_calls = [c for c in calls if c.get("geo_level") == 1]
        state_calls = [c for c in calls if c.get("geo_level") == 2]
        assert country_calls and country_calls[0].get("country_code") == "US"
        assert state_calls and all(c.get("country_code") == "US" for c in state_calls)
