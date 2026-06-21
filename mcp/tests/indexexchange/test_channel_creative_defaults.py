"""Regression tests for Index Exchange channel/creative-format defaults.

Before this fix, `_normalize_deal_type` collapsed OLV into Display and
`_ensure_deal_type_targeting_defaults` emitted `Banner_ANY` for both — so
the trader saw OLV deals come through the Index UI with Display/Banner
format and had to fix each row by hand. The Elcano canonical spec is:

    display → devices=PC+Phone+Tablet, inventory=In-App+Web, creative=Banner_ANY
    olv     → devices=PC+Phone+Tablet, inventory=In-App+Web, creative=Video_ANY
    ctv     → devices=CTV+Connected device+STB, inventory=In-App ONLY,
              creative=Video_ANY
    ott     → devices=Phone+Tablet, inventory=In-App ONLY, creative=Video_ANY

These tests cover only the pure-function pieces — `_normalize_deal_type`
plus the constant tables — so they run without needing the
`/api/supply-configuration/v1/inventory-groups/targets` HTTP mock that
`_ensure_deal_type_targeting_defaults` requires for the full path.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    IX_CREATIVE_TYPE_SIZE_BANNER_ANY,
    IX_CREATIVE_TYPE_SIZE_VIDEO_ANY,
    IX_DEAL_TYPES_CTV,
    IX_DEAL_TYPES_DISPLAY,
    IX_DEAL_TYPES_OLV,
    IX_DEAL_TYPES_OTT,
    IX_DEVICE_VALUES_CTV,
    IX_DEVICE_VALUES_DISPLAY,
    IX_DEVICE_VALUES_OLV,
    IX_DEVICE_VALUES_OTT,
    IX_INVENTORY_CHANNEL_VALUES_APP_ONLY,
    IX_INVENTORY_CHANNEL_VALUES_DEFAULT,
    _normalize_deal_type,
)


class TestNormalizeDealType:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("display", "display"),
            ("DISPLAY", "display"),
            # OLV must NOT collapse to display anymore — that's the headline bug.
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
        assert _normalize_deal_type(raw) == expected


class TestDealTypeMembershipDisjoint:
    """No channel alias may live in more than one canonical bucket — that
    was the root cause of the OLV-treated-as-Display bug."""

    def test_olv_not_in_display(self):
        assert "olv" not in IX_DEAL_TYPES_DISPLAY
        assert "display_olv" not in IX_DEAL_TYPES_DISPLAY
        assert "display/olv" not in IX_DEAL_TYPES_DISPLAY

    def test_display_not_in_olv(self):
        assert "display" not in IX_DEAL_TYPES_OLV

    def test_ctv_disjoint(self):
        assert not IX_DEAL_TYPES_CTV & IX_DEAL_TYPES_DISPLAY
        assert not IX_DEAL_TYPES_CTV & IX_DEAL_TYPES_OLV
        assert not IX_DEAL_TYPES_CTV & IX_DEAL_TYPES_OTT

    def test_ott_disjoint(self):
        assert not IX_DEAL_TYPES_OTT & IX_DEAL_TYPES_DISPLAY
        assert not IX_DEAL_TYPES_OTT & IX_DEAL_TYPES_OLV


class TestCanonicalDeviceSets:
    """Sanity-check the per-channel device IDs against the trader spec."""

    def test_display_devices_are_pc_phone_tablet(self):
        # IX targetingKey "Device" IDs: 2=PC, 4=Phone, 5=Tablet
        assert set(IX_DEVICE_VALUES_DISPLAY) == {"2", "4", "5"}

    def test_olv_shares_display_device_set(self):
        """OLV and Display target the same device set; the difference is
        the creative format only."""
        assert set(IX_DEVICE_VALUES_OLV) == set(IX_DEVICE_VALUES_DISPLAY)

    def test_ctv_devices_are_tv_only(self):
        # 3=Connected TV, 6=Connected device, 7=Set-top box
        assert set(IX_DEVICE_VALUES_CTV) == {"3", "6", "7"}

    def test_ott_devices_are_phone_and_tablet_only(self):
        """OTT = mobile-app video; no PC, no CTV devices."""
        assert set(IX_DEVICE_VALUES_OTT) == {"4", "5"}


class TestInventoryChannelEnforcement:
    """CTV and OTT must force App-only inventory; Display/OLV stay In-App + Web."""

    def test_app_only_constant(self):
        assert IX_INVENTORY_CHANNEL_VALUES_APP_ONLY == ("App",)

    def test_default_includes_app_and_site(self):
        assert set(IX_INVENTORY_CHANNEL_VALUES_DEFAULT) == {"App", "Site"}


class TestCreativeFormatTokens:
    """Smoke check: the Banner and Video tokens are stable strings the IX
    API expects literally — typos here silently produce a wrong-format
    deal."""

    def test_banner_token(self):
        assert IX_CREATIVE_TYPE_SIZE_BANNER_ANY == "Banner_ANY"

    def test_video_token(self):
        assert IX_CREATIVE_TYPE_SIZE_VIDEO_ANY == "Video_ANY"
