"""Regression tests for the PubMatic channel-aware ad_format defaults.

Before this fix, `pm_prepare_deal_from_prompt_inputs` defaulted `ad_formats`
to `[3]` (Banner) regardless of channel. The trader QA review of Reklaim
SMT deals caught this: OLV deals reached PubMatic with `ad_formats=[3]`
(Banner) instead of `[12]` (Video), and the operator had to fix the format
by hand in the SSP UI.

The Elcano canonical channel spec is:

    display → ad_formats=[3]  (Banner)
    olv     → ad_formats=[12] (Video)  — NOT a Display variant
    ctv     → ad_formats=[12] (Video)  — app-only inventory
    ott     → ad_formats=[12] (Video)  — rare; in-app mobile video
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from pubmatic_mcp import (
    PM_AD_FORMATS_DISPLAY,
    PM_AD_FORMATS_VIDEO,
    _apply_pm_channel_ad_format_defaults,
)


class TestApplyPmChannelAdFormatDefaults:
    def test_explicit_ad_formats_passthrough(self):
        result, applied = _apply_pm_channel_ad_format_defaults([12], "display")
        assert result == [12]
        assert applied is False

    def test_display_defaults_to_banner(self):
        result, applied = _apply_pm_channel_ad_format_defaults(None, "display")
        assert result == list(PM_AD_FORMATS_DISPLAY)
        assert result == [3]
        assert applied is True

    def test_olv_defaults_to_video(self):
        """The headline fix: OLV must get Video (12), not Banner (3)."""
        result, applied = _apply_pm_channel_ad_format_defaults(None, "olv")
        assert result == list(PM_AD_FORMATS_VIDEO)
        assert result == [12]
        assert applied is True

    @pytest.mark.parametrize("alias", ["olv", "OLV", "display_olv", "display/olv"])
    def test_olv_aliases_all_route_to_video(self, alias):
        result, applied = _apply_pm_channel_ad_format_defaults(None, alias)
        assert result == [12]
        assert applied is True

    def test_ctv_defaults_to_video(self):
        result, applied = _apply_pm_channel_ad_format_defaults(None, "ctv")
        assert result == list(PM_AD_FORMATS_VIDEO)
        assert applied is True

    def test_ott_defaults_to_video(self):
        result, applied = _apply_pm_channel_ad_format_defaults(None, "ott")
        assert result == list(PM_AD_FORMATS_VIDEO)
        assert applied is True

    def test_no_channel_no_ad_formats_passthrough(self):
        result, applied = _apply_pm_channel_ad_format_defaults(None, None)
        assert result is None
        assert applied is False

    def test_unknown_channel_passthrough(self):
        result, applied = _apply_pm_channel_ad_format_defaults(None, "audio")
        assert result is None
        assert applied is False

    def test_empty_list_treated_as_missing(self):
        """An empty list should trigger the channel default, not be returned as-is."""
        result, applied = _apply_pm_channel_ad_format_defaults([], "olv")
        assert result == [12]
        assert applied is True
