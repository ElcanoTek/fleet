"""Regression tests for OpenX channel-aware rendering-context defaults.

`DEFAULT_RENDERING_CONTEXTS` holds the per-channel ad_placement /
distribution_channel / device_types defaults that `_build_rendering_context`
auto-fills onto a deal payload when the caller doesn't override them.

Trader-canonical mapping:

    DISPLAY → ad_placement=BANNER, distribution=WEB+APP, devices=DESKTOP+MOBILE+TABLET
    OLV     → ad_placement=VIDEO,  distribution=WEB+APP, devices=DESKTOP+MOBILE+TABLET
    CTV     → ad_placement=CTV,    distribution=APP only, devices=CTV+SET_TOP_BOX
    OTT     → ad_placement=VIDEO,  distribution=APP only, devices=DESKTOP+MOBILE+TABLET
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import DEFAULT_RENDERING_CONTEXTS, _build_rendering_context, _infer_channel


class TestDefaultRenderingContexts:
    def test_display_banner_web_and_app(self):
        cfg = DEFAULT_RENDERING_CONTEXTS["DISPLAY"]
        assert cfg["ad_placement"] == {"op": "==", "val": "BANNER"}
        assert cfg["distribution_channel"] == {"op": "INTERSECTS", "val": "WEB,APP"}
        assert cfg["device_types"] == ["DESKTOP", "MOBILE", "TABLET"]

    def test_olv_is_video_not_banner(self):
        """The headline fix everywhere: OLV needs VIDEO ad_placement, not BANNER."""
        cfg = DEFAULT_RENDERING_CONTEXTS["OLV"]
        assert cfg["ad_placement"] == {"op": "==", "val": "VIDEO"}
        assert cfg["distribution_channel"] == {"op": "INTERSECTS", "val": "WEB,APP"}
        assert cfg["device_types"] == ["DESKTOP", "MOBILE", "TABLET"]

    def test_ctv_is_app_only(self):
        """CTV is always in-app even if the brief asks for 'all inventory'.

        Devices cover both Connected TVs and set-top boxes — the canonical TV
        inventory set OpenX exposes via ``rendering_context.device_type.tv_devices``.
        """
        cfg = DEFAULT_RENDERING_CONTEXTS["CTV"]
        assert cfg["ad_placement"] == {"op": "==", "val": "CTV"}
        assert cfg["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}
        assert cfg["device_types"] == ["CTV", "SET_TOP_BOX"]

    def test_ott_is_app_only_video_on_mobile(self):
        cfg = DEFAULT_RENDERING_CONTEXTS["OTT"]
        assert cfg["ad_placement"] == {"op": "==", "val": "VIDEO"}
        assert cfg["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}
        # Mobile video in-app: same device list as OLV, the distinction
        # is the APP-only distribution_channel.
        assert cfg["device_types"] == ["DESKTOP", "MOBILE", "TABLET"]

    def test_all_four_canonical_channels_present(self):
        assert set(DEFAULT_RENDERING_CONTEXTS) == {"DISPLAY", "OLV", "CTV", "OTT"}


class TestInferChannel:
    def test_explicit_channel_wins(self):
        for raw, expected in [
            ("display", "DISPLAY"),
            ("OLV", "OLV"),
            ("Ctv", "CTV"),
            ("ott", "OTT"),
        ]:
            assert _infer_channel({"channel": raw}, None) == expected

    def test_ctv_inferred_from_distribution_channel(self):
        rc = {"distribution_channel": {"val": "CTV"}}
        assert _infer_channel({}, rc) == "CTV"

    def test_video_ad_placement_infers_olv(self):
        rc = {"ad_placement": {"val": "VIDEO"}}
        assert _infer_channel({}, rc) == "OLV"

    def test_falls_back_to_display(self):
        assert _infer_channel({}, None) == "DISPLAY"

    def test_ctv_inferred_from_device_type_only(self):
        """device_type=['CTV'] without an explicit channel must still resolve
        to CTV. Pre-fix this fell through to DISPLAY (or to OLV when the
        agent also passed ad_placement=VIDEO), creating the Format=Video
        on TV devices misconfig OpenX flags in the UI."""
        assert _infer_channel({"device_type": ["CTV"]}, None) == "CTV"
        assert _infer_channel({"device_type": ["SET_TOP_BOX"]}, None) == "CTV"
        assert _infer_channel({"device_type": ["CTV", "SET_TOP_BOX"]}, None) == "CTV"

    def test_mixed_device_type_does_not_infer_ctv(self):
        """If devices mix CTV with non-CTV, fall back to the ad_placement /
        DISPLAY inference path — the agent's intent isn't unambiguously CTV."""
        assert _infer_channel({"device_type": ["CTV", "DESKTOP"]}, None) == "DISPLAY"

    def test_ctv_ad_placement_signal_overrides_video_signal(self):
        """If both CTV and VIDEO are present in rendering_context, CTV wins —
        VIDEO on TV-device inventory is the exact misconfig we're guarding
        against."""
        rc = {"ad_placement": {"val": "CTV"}}
        assert _infer_channel({}, rc) == "CTV"


class TestBuildRenderingContextForcesCTV:
    """Whenever the inferred channel is CTV, the built rendering_context
    MUST emit the CTV wire shape (ad_placement=CTV, distribution=APP) even
    if the caller passed conflicting explicit values. CTV's wire shape is
    a hard OpenX constraint, not a default that callers should be allowed
    to override piecemeal."""

    def test_explicit_video_ad_placement_overridden_when_channel_ctv(self):
        rc = _build_rendering_context(
            {"channel": "CTV", "rendering_context": {"ad_placement": {"op": "==", "val": "VIDEO"}}},
            ["CTV"],
        )
        assert rc["ad_placement"] == {"op": "==", "val": "CTV"}
        assert rc["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}

    def test_explicit_banner_ad_placement_overridden_when_channel_ctv(self):
        rc = _build_rendering_context(
            {"channel": "CTV", "rendering_context": {"ad_placement": {"op": "==", "val": "BANNER"}}},
            ["CTV"],
        )
        assert rc["ad_placement"] == {"op": "==", "val": "CTV"}

    def test_explicit_web_app_distribution_overridden_when_channel_ctv(self):
        """A trader who passes channel=CTV but accidentally also passes
        distribution_channel=WEB,APP must still end up with APP-only inventory."""
        rc = _build_rendering_context(
            {
                "channel": "CTV",
                "rendering_context": {"distribution_channel": {"op": "INTERSECTS", "val": "WEB,APP"}},
            },
            ["CTV"],
        )
        assert rc["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}

    def test_ctv_inferred_from_device_type_forces_wire_shape(self):
        """The headline bug: agent passes device_type=['CTV'] without
        channel=CTV and explicit ad_placement=VIDEO. Pre-fix that produced
        Format=Video on TV-device inventory. Post-fix the inferred CTV
        channel forces Format=CTV."""
        rc = _build_rendering_context(
            {
                "device_type": ["CTV"],
                "rendering_context": {"ad_placement": {"op": "==", "val": "VIDEO"}},
            },
            ["CTV"],
        )
        assert rc["ad_placement"] == {"op": "==", "val": "CTV"}
        assert rc["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}

    def test_ctv_defaults_emit_tv_devices_when_no_explicit_devices(self):
        """No explicit device_type → CTV defaults pick up CTV + SET_TOP_BOX,
        which normalize to tv_devices values."""
        rc = _build_rendering_context({"channel": "CTV"}, None)
        device_type = rc["device_type"]
        # Both Connected TV and set-top-box land in tv_devices, comma-joined.
        assert "tv_devices" in device_type
        tv_values = set(str(device_type["tv_devices"]).split(","))
        assert tv_values == {"tv", "set-top-box"}

    def test_non_ctv_channels_still_honor_explicit_ad_placement(self):
        """Don't accidentally apply the CTV override to DISPLAY/OLV channels —
        those callers must still be able to override defaults. OTT keeps the
        ad_placement override but forces distribution_channel — see the OTT
        test class below."""
        rc = _build_rendering_context(
            {"channel": "DISPLAY", "rendering_context": {"ad_placement": {"op": "==", "val": "VIDEO"}}},
            ["DESKTOP"],
        )
        assert rc["ad_placement"] == {"op": "==", "val": "VIDEO"}


class TestBuildRenderingContextForcesOTT:
    """OTT shares CTV's APP-only constraint. distribution_channel must be APP
    even if the caller passes WEB,APP (the natural OLV default an agent
    might emit while building a manual rendering_context). ad_placement
    stays configurable for the rare OTT case where Format != VIDEO."""

    def test_explicit_web_app_distribution_overridden_when_channel_ott(self):
        rc = _build_rendering_context(
            {
                "channel": "OTT",
                "rendering_context": {"distribution_channel": {"op": "INTERSECTS", "val": "WEB,APP"}},
            },
            ["MOBILE"],
        )
        assert rc["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}

    def test_explicit_video_ad_placement_preserved_when_channel_ott(self):
        """OTT's ad_placement is VIDEO by default but isn't a hard wire-shape
        constraint (unlike CTV's ad_placement=CTV). Caller-provided
        ad_placement should pass through."""
        rc = _build_rendering_context(
            {
                "channel": "OTT",
                "rendering_context": {"ad_placement": {"op": "==", "val": "VIDEO"}},
            },
            ["MOBILE"],
        )
        assert rc["ad_placement"] == {"op": "==", "val": "VIDEO"}

    def test_ott_defaults_applied_when_no_rendering_context(self):
        rc = _build_rendering_context({"channel": "OTT"}, ["MOBILE"])
        assert rc["ad_placement"] == {"op": "==", "val": "VIDEO"}
        assert rc["distribution_channel"] == {"op": "INTERSECTS", "val": "APP"}
