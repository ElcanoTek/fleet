"""Unit tests for the PubMatic channel-aware device defaults and curator-fee defaults.

These cover the pure-function helpers added in
`feat/pubmatic-channel-and-quality-flags`:

- `_apply_pm_channel_device_defaults` (channel -> default device list)
- `_resolve_pm_deal_fees` (None -> 30% Elcano default; explicit value -> passthrough)
- `_build_elcano_curator_fee_entry` (canonical dealFees entry shape)
- `_make_pm_quality_flag` / `_blockers_to_quality_flags` (structured flag shape)
"""

import pytest
from pubmatic_mcp import (
    ELCANO_DEFAULT_FEE_VALUE_PERCENT,
    ELCANO_FEE_RECIPIENT_NAME,
    ELCANO_OWNER_ID,
    PM_DEVICE_VALUES_CTV,
    PM_DEVICE_VALUES_DISPLAY,
    PM_FEE_RECIPIENT_TYPE_BUYER,
    PM_FEE_TYPE_TRANSACTION,
    PM_FEE_VALUE_TYPE_PERCENTAGE,
    _apply_pm_channel_device_defaults,
    _blockers_to_quality_flags,
    _build_elcano_curator_fee_entry,
    _make_pm_quality_flag,
    _normalize_pm_channel,
    _resolve_pm_deal_fees,
)


class TestNormalizePmChannel:
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
            ("nonsense", None),
        ],
    )
    def test_canonicalizes(self, raw, expected):
        assert _normalize_pm_channel(raw) == expected


class TestApplyPmChannelDeviceDefaults:
    def test_explicit_devices_passthrough(self):
        result, applied = _apply_pm_channel_device_defaults(["desktop"], "ctv")
        assert result == ["desktop"]
        assert applied is False

    def test_display_default(self):
        result, applied = _apply_pm_channel_device_defaults(None, "display")
        assert result == list(PM_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_olv_default(self):
        result, applied = _apply_pm_channel_device_defaults(None, "olv")
        # OLV and Display share a device set (desktop+mobile+tablet) — the
        # difference between them is the ad_format (Banner vs Video), not
        # the devices.
        assert result == list(PM_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_ctv_default(self):
        result, applied = _apply_pm_channel_device_defaults(None, "ctv")
        assert result == list(PM_DEVICE_VALUES_CTV)
        assert applied is True

    def test_ott_default(self):
        result, applied = _apply_pm_channel_device_defaults(None, "ott")
        # OTT also shares the Display device set; the OTT-specific
        # distinction (in-app only) lives in inventory targeting rather
        # than the device list.
        assert result == list(PM_DEVICE_VALUES_DISPLAY)
        assert applied is True

    def test_no_channel_no_devices_passthrough(self):
        result, applied = _apply_pm_channel_device_defaults(None, None)
        assert result is None
        assert applied is False

    def test_unknown_channel_passthrough(self):
        result, applied = _apply_pm_channel_device_defaults(None, "audio")
        assert result is None
        assert applied is False


class TestBuildElcanoCuratorFeeEntry:
    def test_default_30_percent_shape(self):
        entry = _build_elcano_curator_fee_entry(30.0)
        assert entry == {
            "recipientName": ELCANO_FEE_RECIPIENT_NAME,
            "recipientId": ELCANO_OWNER_ID,
            "recipientTypeId": PM_FEE_RECIPIENT_TYPE_BUYER,
            "feeValue": 30.0,
            "feeValueType": PM_FEE_VALUE_TYPE_PERCENTAGE,
            "feeType": PM_FEE_TYPE_TRANSACTION,
        }

    def test_custom_value_passes_through(self):
        entry = _build_elcano_curator_fee_entry(15.5)
        assert entry["feeValue"] == 15.5
        assert entry["feeValueType"] == 0


class TestResolvePmDealFees:
    def test_none_applies_default_30_percent(self):
        fees, applied = _resolve_pm_deal_fees(None)
        assert applied is True
        assert len(fees) == 1
        assert fees[0]["feeValue"] == ELCANO_DEFAULT_FEE_VALUE_PERCENT
        assert fees[0]["recipientName"] == ELCANO_FEE_RECIPIENT_NAME
        assert fees[0]["recipientId"] == ELCANO_OWNER_ID
        assert fees[0]["feeValueType"] == 0
        assert fees[0]["feeType"] == 0

    def test_explicit_dict_wraps_into_list(self):
        custom = {"recipientName": "Other", "recipientId": 99, "feeValue": 10.0}
        fees, applied = _resolve_pm_deal_fees(custom)
        assert applied is False
        assert fees == [custom]

    def test_explicit_list_passes_through(self):
        custom = [
            {"recipientName": "A", "recipientId": 1, "feeValue": 5.0},
            {"recipientName": "B", "recipientId": 2, "feeValue": 7.5},
        ]
        fees, applied = _resolve_pm_deal_fees(custom)
        assert applied is False
        assert fees == custom

    def test_invalid_type_raises(self):
        with pytest.raises(ValueError, match="fee must be a dict, list of dicts, or None"):
            _resolve_pm_deal_fees(42)


class TestMakePmQualityFlag:
    def test_required_fields(self):
        flag = _make_pm_quality_flag("pm_test", "something happened")
        assert flag == {"flag": "pm_test", "impact": "something happened"}

    def test_extra_context_included(self):
        flag = _make_pm_quality_flag("pm_test", "msg", channel="ctv", count=3)
        assert flag["channel"] == "ctv"
        assert flag["count"] == 3

    def test_none_context_dropped(self):
        flag = _make_pm_quality_flag("pm_test", "msg", maybe=None, kept="value")
        assert "maybe" not in flag
        assert flag["kept"] == "value"


class TestBlockersToQualityFlags:
    def test_maps_code_and_message(self):
        blockers = [
            {"code": "dsp_buyer_unresolved", "message": "Could not find DSP", "dsp_name": "Acme"},
            {"code": "invalid_dates", "message": "end < start"},
        ]
        flags = _blockers_to_quality_flags(blockers)
        assert flags[0]["flag"] == "pm_dsp_buyer_unresolved"
        assert flags[0]["impact"] == "Could not find DSP"
        assert flags[0]["dsp_name"] == "Acme"
        assert flags[1]["flag"] == "pm_invalid_dates"

    def test_already_prefixed_code_not_double_prefixed(self):
        blockers = [{"code": "pm_already_prefixed", "message": "ok"}]
        flags = _blockers_to_quality_flags(blockers)
        assert flags[0]["flag"] == "pm_already_prefixed"

    def test_non_dict_blocker_skipped(self):
        flags = _blockers_to_quality_flags([{"code": "valid", "message": "x"}, "garbage", None])
        assert len(flags) == 1
        assert flags[0]["flag"] == "pm_valid"
