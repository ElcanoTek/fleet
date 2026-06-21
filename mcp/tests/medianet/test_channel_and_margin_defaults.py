"""Unit tests for Media.net channel-aware device defaults and curator-margin default.

Covers the pure-function helpers added in
`feat/medianet-channel-fee-default-and-quality-flags`:

- `_apply_mn_channel_device_defaults` (channel -> default device list)
- `_normalize_mn_channel` (display/olv/ctv canonicalization)
- `_make_mn_quality_flag` / `_blockers_to_mn_quality_flags` (structured flag shape)
"""

import pytest
from medianet_mcp import (
    ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT,
    MN_DEVICE_VALUES_CTV,
    MN_DEVICE_VALUES_DISPLAY,
    MN_MARGIN_TYPE_PERCENTAGE,
    _apply_mn_channel_device_defaults,
    _blockers_to_mn_quality_flags,
    _make_mn_quality_flag,
    _normalize_mn_channel,
)


class TestNormalizeMnChannel:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("display", "display"),
            ("DISPLAY", "display"),
            # OLV is its OWN channel — must NOT collapse to "display".
            # Earlier revisions returned "display" for OLV, which gave OLV
            # deals the Banner ad_format. The trader spec routes OLV to
            # Video format on the same device set as Display.
            ("olv", "olv"),
            ("OLV", "olv"),
            ("display_olv", "olv"),
            ("display/olv", "olv"),
            ("ctv", "ctv"),
            ("CTV", "ctv"),
            ("ott", "ott"),
            ("OTT", "ott"),
            (None, None),
            ("", None),
            ("audio", None),
        ],
    )
    def test_canonicalizes(self, raw, expected):
        assert _normalize_mn_channel(raw) == expected


class TestApplyMnChannelDeviceDefaults:
    def test_explicit_devices_passthrough(self):
        result, applied = _apply_mn_channel_device_defaults(["Mobile"], "ctv")
        assert result == ["Mobile"]
        assert applied is False

    def test_display_default(self):
        result, applied = _apply_mn_channel_device_defaults(None, "display")
        assert result == list(MN_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_olv_default(self):
        # OLV and Display share a device set; the difference is the
        # ad_format (Banner vs Video), not the devices.
        result, applied = _apply_mn_channel_device_defaults(None, "olv")
        assert result == list(MN_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_ctv_default(self):
        result, applied = _apply_mn_channel_device_defaults(None, "ctv")
        assert result == list(MN_DEVICE_VALUES_CTV)
        assert applied is True

    def test_ott_default(self):
        # OTT = Phone + Tablet (no PC, no CTV) — in-app mobile video.
        from medianet_mcp import MN_DEVICE_VALUES_OTT

        result, applied = _apply_mn_channel_device_defaults(None, "ott")
        assert result == list(MN_DEVICE_VALUES_OTT)
        assert applied is True

    def test_no_channel_passthrough(self):
        result, applied = _apply_mn_channel_device_defaults(None, None)
        assert result is None
        assert applied is False

    def test_unknown_channel_passthrough(self):
        result, applied = _apply_mn_channel_device_defaults(None, "audio")
        assert result is None
        assert applied is False


class TestMakeMnQualityFlag:
    def test_required_fields(self):
        flag = _make_mn_quality_flag("mn_test", "an impact")
        assert flag == {"flag": "mn_test", "impact": "an impact"}

    def test_extra_context_kept(self):
        flag = _make_mn_quality_flag("mn_test", "msg", channel="ctv", count=2)
        assert flag["channel"] == "ctv"
        assert flag["count"] == 2

    def test_none_context_dropped(self):
        flag = _make_mn_quality_flag("mn_test", "msg", drop=None, keep="x")
        assert "drop" not in flag
        assert flag["keep"] == "x"


class TestBlockersToMnQualityFlags:
    def test_maps_code_and_message(self):
        blockers = [
            {"code": "demand_partners_unresolved", "message": "Unknown DSP", "details": {"dsp": "Acme"}},
            {"code": "invalid_dates", "message": "end < start"},
        ]
        flags = _blockers_to_mn_quality_flags(blockers)
        assert flags[0]["flag"] == "mn_demand_partners_unresolved"
        assert flags[0]["impact"] == "Unknown DSP"
        assert flags[0]["dsp"] == "Acme"
        assert flags[1]["flag"] == "mn_invalid_dates"

    def test_already_prefixed_code_preserved(self):
        blockers = [{"code": "mn_already_prefixed", "message": "x"}]
        flags = _blockers_to_mn_quality_flags(blockers)
        assert flags[0]["flag"] == "mn_already_prefixed"

    def test_non_dict_skipped(self):
        flags = _blockers_to_mn_quality_flags([{"code": "ok", "message": "y"}, None, "garbage"])
        assert len(flags) == 1
        assert flags[0]["flag"] == "mn_ok"


class TestMarginDefaultConstants:
    def test_default_is_30_percent(self):
        assert ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT == 30.0

    def test_margin_type_percentage_is_1(self):
        assert MN_MARGIN_TYPE_PERCENTAGE == 1
