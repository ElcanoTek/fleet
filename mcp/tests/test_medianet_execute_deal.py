import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import medianet_mcp


@pytest.fixture(autouse=True)
def _isolate_medianet_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so we never pick up live-run artifacts."""
    cache_root = tmp_path_factory.mktemp("mncache")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    medianet_mcp._entity_cache.clear()
    yield
    medianet_mcp._entity_cache.clear()


class TestMnExecuteDealFromPromptInputs:
    @pytest.mark.asyncio
    async def test_disk_cache_round_trip(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("MEDIANET_CACHE_TTL_SECONDS", "3600")
        assert medianet_mcp._cache_get("unit_test_key") is None
        medianet_mcp._cache_put("unit_test_key", {"value": [1, 2]})
        assert medianet_mcp._cache_get("unit_test_key") == {"value": [1, 2]}
        assert (tmp_path / "cutlass" / "medianet" / "unit_test_key.json").is_file()

    @pytest.mark.asyncio
    async def test_disk_cache_disabled_when_ttl_zero(self, monkeypatch: pytest.MonkeyPatch, tmp_path):
        monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
        monkeypatch.setenv("MEDIANET_CACHE_TTL_SECONDS", "0")
        medianet_mcp._cache_put("k", {"v": 1})
        assert medianet_mcp._cache_get("k") is None

    @pytest.mark.asyncio
    async def test_resolve_entity_by_name_and_id(self, monkeypatch: pytest.MonkeyPatch):
        """Entity resolver matches by name (case-insensitive), by code, and by direct id."""
        fake_devices = [
            {"id": 1, "name": "Mobile/Tablet"},
            {"id": 2, "name": "Personal Computer"},
            {"id": 3, "name": "Connected TV"},
        ]

        class FakeClient:
            async def list_entity(self, entity_name):
                assert entity_name == "devices"
                return fake_devices

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())

        ids, warns = await medianet_mcp._resolve_entity("devices", ["Mobile/Tablet", "personal computer", 3])
        assert ids == [1, 2, 3]
        assert warns == []

    @pytest.mark.asyncio
    async def test_resolve_entity_unresolved_raises_with_sample(self, monkeypatch: pytest.MonkeyPatch):
        """Unresolved values raise MediaNetResolutionError carrying available_sample."""

        class FakeClient:
            async def list_entity(self, _entity_name):
                return [{"id": 1, "name": "Mobile/Tablet"}, {"id": 2, "name": "Personal Computer"}]

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        with pytest.raises(medianet_mcp.MediaNetResolutionError) as excinfo:
            await medianet_mcp._resolve_entity("devices", ["Atlantis"])
        err = excinfo.value
        assert "Atlantis" in err.message
        assert err.details["unresolved"] == ["Atlantis"]
        assert "Mobile/Tablet" in err.details["available_sample"]

    @pytest.mark.asyncio
    async def test_resolve_demand_partners_by_name_and_id(self, monkeypatch: pytest.MonkeyPatch):
        class FakeClient:
            async def list_demand_partners(self, **_kwargs):
                return [
                    {"id": "DV 360", "name": "DV 360"},
                    {"id": "TTD", "name": "The Trade Desk"},
                ]

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        ids, _ = await medianet_mcp._resolve_demand_partners(["The Trade Desk", "DV 360"], ad_format_id=0)
        assert ids == ["TTD", "DV 360"]

    @pytest.mark.asyncio
    async def test_resolve_geo_country_codes_and_full_entries(self, monkeypatch: pytest.MonkeyPatch):
        countries = [
            {"id": 1, "name": "United States", "code": "US"},
            {"id": 2, "name": "India", "code": "IN"},
        ]

        class FakeClient:
            async def list_entity(self, entity_name):
                assert entity_name == "countries"
                return countries

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        out, _ = await medianet_mcp._resolve_geo(
            [
                "US",
                "India",
                {"geo_type": "state", "id": "US$AK", "is_excluded": True},
            ]
        )
        assert out == [
            {"geo_type": "country", "id": "US", "is_excluded": False},
            {"geo_type": "country", "id": "IN", "is_excluded": False},
            {"geo_type": "state", "id": "US$AK", "is_excluded": True},
        ]

    @pytest.mark.asyncio
    async def test_prepare_validates_deal_id_and_dates(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_resolve_demand_partners(values, *, ad_format_id):
            return [str(v) for v in values], []

        monkeypatch.setattr(medianet_mcp, "_resolve_demand_partners", fake_resolve_demand_partners)
        prepared = await medianet_mcp.mn_prepare_deal_from_prompt_inputs(
            deal_id="bad/id",  # forbidden char
            display_name="x" * 31,  # over length
            start_date="2026-05-31",
            end_date="2026-05-01",  # before start
            ad_format=99,  # invalid
            margin=200,  # over percentage cap
            margin_type=1,
            demand_partners=["DV 360"],
        )
        assert prepared["ready_to_create"] is False
        codes = [b["code"] for b in prepared["blockers"]]
        assert "invalid_deal_id" in codes
        assert "invalid_display_name" in codes
        assert "invalid_date_order" in codes
        assert "invalid_ad_format" in codes
        assert "invalid_margin" in codes

    @pytest.mark.asyncio
    async def test_execute_short_circuits_to_prepare_phase_on_blockers(self, monkeypatch: pytest.MonkeyPatch):
        async def must_not_call_create_prepared(*args, **kwargs):
            raise AssertionError("submit must not run when prepare blocks")

        monkeypatch.setattr(medianet_mcp, "mn_create_prepared_deal", must_not_call_create_prepared)
        result = await medianet_mcp.mn_execute_deal_from_prompt_inputs(
            deal_id="ok-id",
            display_name="ok-name",
            start_date="2026-05-01",
            ad_format=0,
            margin=10,
            demand_partners=[],  # missing required
        )
        assert result["success"] is False
        assert result["phase"] == "prepare"

    @pytest.mark.asyncio
    async def test_execute_happy_path_creates_and_verifies(self, monkeypatch: pytest.MonkeyPatch):
        async def fake_resolve_demand_partners(values, *, ad_format_id):
            return [str(v) for v in values], []

        captured_payload: dict = {}

        class FakeClient:
            async def create_deal(self, payload):
                captured_payload.update(payload)
                return {"deal_id": payload["deal_id"], "display_name": payload["display_name"]}

        async def fake_get_deal(deal_id):
            return {"success": True, "deal": {"deal_id": deal_id, "status": 1}}

        monkeypatch.setattr(medianet_mcp, "_resolve_demand_partners", fake_resolve_demand_partners)
        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        monkeypatch.setattr(medianet_mcp, "mn_get_deal", fake_get_deal)

        result = await medianet_mcp.mn_execute_deal_from_prompt_inputs(
            deal_id="mn-test-001",
            display_name="MN Test Deal",
            start_date="2026-05-01",
            end_date="2026-05-31",
            ad_format=0,
            margin=10,
            demand_partners=["DV 360"],
            bid_floor=2.5,
        )
        assert result["success"] is True
        assert result["deal"]["deal_id"] == "mn-test-001"
        assert result["verification"]["success"] is True
        # Verified-working defaults applied.
        assert captured_payload["margin_type"] == 1
        assert captured_payload["status"] == 1
        assert captured_payload["environments"] == ["Web"]
        assert captured_payload["bid_floor"] == 2.5
        # Media.net Select has no deal-detail URL; deal_url surfaces the
        # deals-list page so traders can locate the row by deal_id.
        assert result["deal_url"] == "https://select.media.net/deals"

    @pytest.mark.asyncio
    async def test_execute_coerces_viewability_fraction_and_dedupes_quality_flags(
        self, monkeypatch: pytest.MonkeyPatch
    ):
        """Two regressions in one path:

        1. viewability_min=0.70 used to reach the API as a float and trigger
           HTTP 422 ("must be an integer"). _coerce_mn_percent now normalizes
           it to 70 server-side.

        2. mn_create_prepared_deal seeds its quality_flags from the prepared
           artifact, and execute used to concatenate preparation's flags on
           top — so mn_default_curator_margin_applied surfaced twice in the
           response. Each prepare-phase flag must appear exactly once.
        """

        async def fake_resolve_demand_partners(values, *, ad_format_id):
            return [str(v) for v in values], []

        captured_payload: dict = {}

        class FakeClient:
            async def create_deal(self, payload):
                captured_payload.update(payload)
                return {"deal_id": payload["deal_id"], "display_name": payload["display_name"]}

        async def fake_get_deal(deal_id):
            return {"success": True, "deal": {"deal_id": deal_id, "status": 1}}

        monkeypatch.setattr(medianet_mcp, "_resolve_demand_partners", fake_resolve_demand_partners)
        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        monkeypatch.setattr(medianet_mcp, "mn_get_deal", fake_get_deal)

        result = await medianet_mcp.mn_execute_deal_from_prompt_inputs(
            deal_id="mn-viewability-001",
            display_name="MN Viewability",
            start_date="2026-05-01",
            end_date="2026-05-31",
            ad_format=0,
            demand_partners=["DV 360"],
            # margin omitted -> mn_default_curator_margin_applied fires.
            viewability_min=0.70,
        )
        assert result["success"] is True
        # 1. Fraction was coerced to integer percent.
        assert captured_payload["viewability"] == {"min": 70, "max": None}
        # 2. Each prepare-phase flag appears exactly once.
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert flag_names.count("mn_default_curator_margin_applied") == 1

    @pytest.mark.asyncio
    async def test_create_prepared_deal_refuses_blocked_artifact(self, monkeypatch: pytest.MonkeyPatch):
        async def must_not_be_called(payload):
            raise AssertionError("create API must not be invoked when artifact is blocked")

        class FakeClient:
            create_deal = staticmethod(must_not_be_called)

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())

        prepared_id = "medianet-prepared-test-blocked"
        medianet_mcp._prepared_medianet_deals[prepared_id] = {
            "prepared_deal_id": prepared_id,
            "ready_to_create": False,
            "blocking_issues": ["bad input"],
            "blockers": [{"code": "invalid_deal_id", "message": "bad input"}],
            "warnings": [],
            "resolved_entities": {},
            "deal_intent": {},
        }
        try:
            result = await medianet_mcp.mn_create_prepared_deal(prepared_id)
        finally:
            medianet_mcp._prepared_medianet_deals.pop(prepared_id, None)
        assert result["success"] is False
        assert "blocked" in result["error"].lower()

    @pytest.mark.asyncio
    async def test_create_prepared_deal_rejects_unknown_id(self):
        result = await medianet_mcp.mn_create_prepared_deal("medianet-prepared-does-not-exist")
        assert result["success"] is False
        assert "not found" in result["error"].lower()
