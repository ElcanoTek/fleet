"""Tests for the Media.net deal URL builder.

Media.net Select doesn't expose a deal-detail route — every deal_id
lands on the same /deals list page. We surface that list URL in the
execute response for parity with the other SSPs (PubMatic, IX, OpenX,
Xandr all return a deep-linkable URL). The email report relies on
the deal_id + display_name to disambiguate the row in the list.
"""

from medianet_mcp import MEDIANET_DEALS_LIST_URL, _build_medianet_deal_url


class TestBuildMedianetDealUrl:
    def test_returns_deals_list_url_when_no_id(self):
        assert _build_medianet_deal_url() == MEDIANET_DEALS_LIST_URL

    def test_returns_same_url_regardless_of_id(self):
        # The URL is constant — Media.net has no per-deal route.
        assert _build_medianet_deal_url("any-deal-id") == MEDIANET_DEALS_LIST_URL
        assert _build_medianet_deal_url("ANOTHER_ONE") == MEDIANET_DEALS_LIST_URL

    def test_explicit_empty_string_suppresses_url(self):
        # Escape hatch for callers that want to opt out.
        assert _build_medianet_deal_url("") is None

    def test_url_constant_is_select_media_net_deals(self):
        # Pin the URL string so a typo doesn't sneak through silently.
        assert MEDIANET_DEALS_LIST_URL == "https://select.media.net/deals"
