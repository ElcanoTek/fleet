"""Regression tests for Media.net channel-aware ad_format defaults.

Before this fix, `mn_prepare_deal_from_prompt_inputs` required the caller
to pass `ad_format` explicitly and there was no mapping from channel.
OLV deals reached Media.net with whatever ad_format the agent guessed —
usually 0 (Banner) — and showed up as Display in the SSP UI. The
trader spec is:

    display → ad_format=0 (Banner)
    olv     → ad_format=1 (Video)  — NOT a Display variant
    ctv     → ad_format=1 (Video)  — app-only
    ott     → ad_format=1 (Video)  — rare; in-app mobile video

This file also pins down the Media.net ad_format integer mapping after
the docstring/validation message disagreed on whether 1 was Native or
Video. Source of truth from the constants module: 0=Banner, 1=Video,
2=Native.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from medianet_mcp import (
    MN_AD_FORMAT_BANNER,
    MN_AD_FORMAT_NATIVE,
    MN_AD_FORMAT_VIDEO,
    _apply_mn_channel_ad_format_default,
)


class TestMnAdFormatConstants:
    """Source-of-truth pin for the ad_format integer mapping."""

    def test_banner_is_zero(self):
        assert MN_AD_FORMAT_BANNER == 0

    def test_video_is_one(self):
        assert MN_AD_FORMAT_VIDEO == 1

    def test_native_is_two(self):
        assert MN_AD_FORMAT_NATIVE == 2


class TestApplyMnChannelAdFormatDefault:
    def test_explicit_ad_format_passthrough(self):
        result, applied = _apply_mn_channel_ad_format_default(2, "olv")
        assert result == 2  # caller wins
        assert applied is False

    def test_display_defaults_to_banner(self):
        result, applied = _apply_mn_channel_ad_format_default(None, "display")
        assert result == MN_AD_FORMAT_BANNER
        assert applied is True

    def test_olv_defaults_to_video(self):
        """The headline fix: OLV must get Video (1), not Banner (0)."""
        result, applied = _apply_mn_channel_ad_format_default(None, "olv")
        assert result == MN_AD_FORMAT_VIDEO
        assert applied is True

    def test_ctv_defaults_to_video(self):
        result, applied = _apply_mn_channel_ad_format_default(None, "ctv")
        assert result == MN_AD_FORMAT_VIDEO
        assert applied is True

    def test_ott_defaults_to_video(self):
        result, applied = _apply_mn_channel_ad_format_default(None, "ott")
        assert result == MN_AD_FORMAT_VIDEO
        assert applied is True

    def test_no_channel_no_default(self):
        result, applied = _apply_mn_channel_ad_format_default(None, None)
        assert result is None
        assert applied is False

    def test_unknown_channel_no_default(self):
        result, applied = _apply_mn_channel_ad_format_default(None, "audio")
        assert result is None
        assert applied is False
