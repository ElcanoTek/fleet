"""Tests for the PubMatic deal-detail URL builder.

The earlier `app.pubmatic.com/deals/curated/{id}` URL routed to a list
page that didn't open the specific deal in the trader's UI. The PubMatic
UI requires three query params on `apps.pubmatic.com/v3/common/pmc/deals`:
`dealId`, `dealName` (url-encoded), and `dealCategoryId=3` (Curated).
"""

from pubmatic_mcp import _build_pubmatic_deal_url


class TestBuildPubmaticDealUrl:
    def test_returns_none_when_curated_id_missing(self):
        assert _build_pubmatic_deal_url(None) is None

    def test_id_and_name_url_encoded(self):
        url = _build_pubmatic_deal_url(678896, deal_name="SignalForge_PubMatic_TRADR_Display_ELC07225_B14")
        assert url == (
            "https://apps.pubmatic.com/v3/common/pmc/deals"
            "?dealId=678896"
            "&dealName=SignalForge_PubMatic_TRADR_Display_ELC07225_B14"
            "&dealCategoryId=3"
        )

    def test_name_with_spaces_is_encoded(self):
        url = _build_pubmatic_deal_url(123, deal_name="My Deal With Spaces")
        assert "dealName=My%20Deal%20With%20Spaces" in url

    def test_name_with_special_chars_is_encoded(self):
        url = _build_pubmatic_deal_url(123, deal_name="Deal & Co. / Test")
        # & / and . / spaces all need encoding
        assert "dealName=Deal%20%26%20Co.%20%2F%20Test" in url

    def test_missing_name_yields_empty_dealname_param(self):
        # Edge case: response had no name. URL still navigates to the
        # category list filtered by dealId, which surfaces the deal.
        url = _build_pubmatic_deal_url(678896)
        assert url == ("https://apps.pubmatic.com/v3/common/pmc/deals?dealId=678896&dealName=&dealCategoryId=3")
