"""Regression tests for the Media.net resolution bugs surfaced by deal
SF_MN_TDM_DISP_ELC07225_B14 (the agent had to retry four times because the
MCP's auto-fills, geo lookup, viewability typing, and demand-partner naming
all rejected inputs the protocol promised would work)."""

import medianet_mcp
import pytest
from medianet_mcp import (
    MN_DEVICE_VALUES_CTV,
    MN_DEVICE_VALUES_DISPLAY,
    _coerce_mn_percent,
    _resolve_demand_partners,
    _resolve_geo,
)


@pytest.fixture(autouse=True)
def _isolate_mn_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so we never pick up
    artifacts written by a prior live run."""
    cache_root = tmp_path_factory.mktemp("mncache_resolution")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    medianet_mcp._entity_cache.clear()
    yield
    medianet_mcp._entity_cache.clear()


class TestChannelDeviceDefaultsMatchCatalog:
    """The display/CTV defaults are passed straight into the device-resolver,
    so they must be names the Media.net devices catalog actually contains."""

    def test_display_uses_catalog_compatible_names(self):
        # Names verified against Media.net's /v1/entities/devices response.
        assert MN_DEVICE_VALUES_DISPLAY == (
            "Personal Computer/Desktop",
            "Phone/Mobile",
            "Tablet",
        )

    def test_ctv_uses_catalog_compatible_names(self):
        assert MN_DEVICE_VALUES_CTV == ("Connected TV",)


class TestResolveGeoProductionCatalogShape:
    """In production Media.net's countries entity returns rows where `id`
    IS the ISO-2 code (no separate `code` field). The previous resolver
    only looked at `code`, so every "US"/"United States"/"USA" input
    fell through to unresolved."""

    @pytest.mark.asyncio
    async def test_resolves_when_id_is_iso_code(self, monkeypatch: pytest.MonkeyPatch):
        countries = [
            {"id": "US", "name": "UNITED STATES OF AMERICA (THE)"},
            {"id": "IN", "name": "INDIA"},
        ]

        class FakeClient:
            async def list_entity(self, entity_name):
                assert entity_name == "countries"
                return countries

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        out, _ = await _resolve_geo(["US", "India"])
        assert out == [
            {"geo_type": "country", "id": "US", "is_excluded": False},
            {"geo_type": "country", "id": "IN", "is_excluded": False},
        ]

    @pytest.mark.asyncio
    async def test_failure_sample_is_populated_when_only_id_field(self, monkeypatch: pytest.MonkeyPatch):
        # Regression: previously the failure sample was [] because it
        # only read `code`, so the agent had no debugging signal.
        countries = [
            {"id": "US", "name": "UNITED STATES"},
            {"id": "IN", "name": "INDIA"},
            {"id": "GB", "name": "UNITED KINGDOM"},
        ]

        class FakeClient:
            async def list_entity(self, entity_name):
                assert entity_name == "countries"
                return countries

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        with pytest.raises(medianet_mcp.MediaNetResolutionError) as exc_info:
            await _resolve_geo(["Atlantis"])
        details = exc_info.value.details
        assert details["available_country_count"] == 3
        assert details["available_country_sample"], "sample must surface country labels"

    @pytest.mark.asyncio
    async def test_skips_purely_numeric_id_to_avoid_invalid_payload(self, monkeypatch: pytest.MonkeyPatch):
        # If the catalog row exposes only a numeric primary-key id (no code,
        # no recognizable ISO string), we cannot use it as the deal-payload
        # geo id. Skip the row rather than emitting a payload that the API
        # will reject.
        countries = [{"id": 42, "name": "United States"}]

        class FakeClient:
            async def list_entity(self, entity_name):
                assert entity_name == "countries"
                return countries

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        with pytest.raises(medianet_mcp.MediaNetResolutionError):
            await _resolve_geo(["US"])


class TestCoerceMnPercent:
    """viewability_min/max must be integer 0-100 in the deal payload, but
    trader prompts historically pass fractions (e.g. 0.70) because that's
    the IX/PubMatic/OpenX convention."""

    def test_none_passes_through(self):
        assert _coerce_mn_percent(None) is None

    def test_fraction_converts_to_percent(self):
        assert _coerce_mn_percent(0.70) == 70

    def test_one_treated_as_fraction(self):
        # 1.0 is the upper bound of the fraction range.
        assert _coerce_mn_percent(1) == 100

    def test_integer_percent_passes_through(self):
        assert _coerce_mn_percent(70) == 70
        assert _coerce_mn_percent(100) == 100

    def test_zero_passes_through(self):
        assert _coerce_mn_percent(0) == 0

    def test_rejects_negative(self):
        with pytest.raises(ValueError):
            _coerce_mn_percent(-1)

    def test_rejects_above_one_hundred(self):
        with pytest.raises(ValueError):
            _coerce_mn_percent(150)

    def test_rejects_non_numeric(self):
        with pytest.raises(ValueError):
            _coerce_mn_percent("not a number")


class TestDemandPartnerAliases:
    """Trader prompts use the canonical SSP-wide DSP names ("The Trade Desk")
    that don't match Media.net's compact catalog ids ("TTD"). Alias them
    server-side so callers don't have to know per-SSP shorthand."""

    @pytest.mark.asyncio
    async def test_aliases_the_trade_desk_to_ttd(self, monkeypatch: pytest.MonkeyPatch):
        items = [{"id": "TTD", "name": "TTD"}, {"id": "DV 360", "name": "DV 360"}]

        class FakeClient:
            async def list_demand_partners(self, ad_format_id):
                assert ad_format_id == 0
                return items

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        resolved, warnings = await _resolve_demand_partners(["The Trade Desk"], ad_format_id=0)
        assert resolved == ["TTD"]
        assert any("aliased" in w.lower() for w in warnings)

    @pytest.mark.asyncio
    async def test_aliases_dv360_variants(self, monkeypatch: pytest.MonkeyPatch):
        items = [{"id": "DV 360", "name": "DV 360"}]

        class FakeClient:
            async def list_demand_partners(self, ad_format_id):
                assert ad_format_id == 0
                return items

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        resolved, warnings = await _resolve_demand_partners(["DV360"], ad_format_id=0)
        assert resolved == ["DV 360"]
        assert any("aliased" in w.lower() for w in warnings)

    @pytest.mark.asyncio
    async def test_no_alias_when_already_canonical(self, monkeypatch: pytest.MonkeyPatch):
        items = [{"id": "TTD", "name": "TTD"}]

        class FakeClient:
            async def list_demand_partners(self, ad_format_id):
                assert ad_format_id == 0
                return items

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        resolved, warnings = await _resolve_demand_partners(["TTD"], ad_format_id=0)
        assert resolved == ["TTD"]
        # No alias warning when the input already matched a canonical id.
        assert warnings == []
