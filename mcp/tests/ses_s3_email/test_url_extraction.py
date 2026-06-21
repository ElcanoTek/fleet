"""
Unit tests for SES/S3 Email URL extraction and cleaning functions.

Tests cover:
- URL cleaning (newline removal, quoted-printable handling)
- Proofpoint URL decoding
- URL classification
- HTML and text URL extraction
"""

import os
import sys

# Add the mcp directory to the path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from ses_s3_email import (
    _extract_email_content,
    _looks_like_html_response,
    _pick_chase_target,
    classify_url,
    clean_url,
    extract_urls_from_html,
    extract_urls_from_text,
    is_click_tracker_url,
)


class TestCleanUrl:
    """Tests for the clean_url function."""

    def test_remove_newlines(self):
        """Should remove embedded newlines from URLs."""
        url = "https://example.com/path\nto/file"
        result = clean_url(url)
        assert result == "https://example.com/pathto/file"

    def test_remove_carriage_returns(self):
        """Should remove embedded carriage returns from URLs."""
        url = "https://example.com/path\rto/file"
        result = clean_url(url)
        assert result == "https://example.com/pathto/file"

    def test_remove_quoted_printable_soft_breaks(self):
        """Should remove quoted-printable soft line breaks (= before newline)."""
        url = "https://example.com/very=\nlong=\r\npath"
        result = clean_url(url)
        assert result == "https://example.com/verylongpath"

    def test_strip_whitespace(self):
        """Should strip leading and trailing whitespace."""
        url = "  https://example.com/path  "
        result = clean_url(url)
        assert result == "https://example.com/path"

    def test_remove_trailing_punctuation(self):
        """Should remove trailing punctuation."""
        url = "https://example.com/path.,;:!?"
        result = clean_url(url)
        assert result == "https://example.com/path"

    def test_empty_url(self):
        """Should handle empty strings."""
        assert clean_url("") == ""
        assert clean_url(None) == ""

    def test_complex_line_wrapped_url(self):
        """Should handle complex line-wrapped URLs from emails."""
        url = "https://desk.thetradedesk.com/\nreports/view/12345"
        result = clean_url(url)
        assert result == "https://desk.thetradedesk.com/reports/view/12345"


class TestClassifyUrl:
    """Tests for the classify_url function."""

    def test_csv_extension(self):
        """Should classify CSV URLs as download likely."""
        result = classify_url("https://example.com/data.csv")
        assert result["is_download_likely"] is True
        assert result["extension"] == ".csv"

    def test_pdf_extension(self):
        """Should classify PDF URLs as download likely."""
        result = classify_url("https://example.com/report.pdf")
        assert result["is_download_likely"] is True

    def test_download_keyword(self):
        """Should classify URLs with download keywords."""
        result = classify_url("https://example.com/api/download?id=123")
        assert result["is_download_likely"] is True
        assert result["has_download_keyword"] is True

    def test_regular_webpage(self):
        """Should not classify regular webpages as downloads."""
        result = classify_url("https://example.com/about")
        assert result["is_download_likely"] is False

    def test_image_extension_demoted(self):
        """Image-extension URLs (banners, logos, tracking pixels) MUST NOT
        be flagged as downloads — they were causing
        download_all_link_attachments to "succeed" with the email banner
        instead of the real report."""
        # The actual banner CDN URL from the failing Viant scenario.
        result = classify_url("http://cdn.mcauto-images-production.sendgrid.net/abc/def/3542x626.png")
        assert result["is_download_likely"] is False
        assert result["is_image_asset"] is True

    def test_image_extension_demoted_even_with_keyword(self):
        """A keyword like 'report' in the path doesn't override image demotion."""
        result = classify_url("https://example.com/report-banner.png")
        assert result["is_download_likely"] is False
        assert result["is_image_asset"] is True

    def test_sendgrid_click_tracker_recognized(self):
        """SendGrid /ls/click wrapper URLs (used by Viant DSP and many
        SaaS senders) must classify as download-likely even though they
        have no extension and no readable keyword in the encoded body."""
        result = classify_url("https://u5609542.ct.sendgrid.net/ls/click?upn=u001.encoded.payload")
        assert result["is_download_likely"] is True
        assert result["is_click_tracker"] is True

    def test_mailchimp_click_tracker_recognized(self):
        result = classify_url("https://example.list-manage.com/track/click?u=abc&id=xyz")
        assert result["is_click_tracker"] is True
        assert result["is_download_likely"] is True

    def test_hubspot_click_tracker_recognized(self):
        result = classify_url("https://abc.hubspotlinks.com/Btc/2X+113")
        assert result["is_click_tracker"] is True

    def test_generic_path_only_tracker(self):
        """Path-only patterns (/ls/click, /track/click) match even on
        unfamiliar hosts — many self-hosted ESPs reuse these paths."""
        result = classify_url("https://email.smaller-saas.io/ls/click?u=encoded")
        assert result["is_click_tracker"] is True


class TestIsClickTrackerUrl:
    """Tests for the is_click_tracker_url helper."""

    def test_sendgrid(self):
        assert is_click_tracker_url("https://u5609542.ct.sendgrid.net/ls/click?upn=abc")

    def test_non_tracker_returns_false(self):
        assert is_click_tracker_url("https://www.viantinc.com/") is False
        assert is_click_tracker_url("https://reports.example.com/files/abc.csv") is False

    def test_blank_input_safe(self):
        assert is_click_tracker_url("") is False

    def test_malformed_url_safe(self):
        # Empty host_suffix + empty path_substring entry must NOT match
        # everything (the dual-blank guard in is_click_tracker_url).
        assert is_click_tracker_url("https://random.example.com/some/path") is False


class TestExtractUrlsFromText:
    """Tests for extract_urls_from_text function."""

    def test_simple_url(self):
        """Should extract simple HTTP and HTTPS URLs."""
        text = "Check out https://example.com and http://test.com"
        urls = extract_urls_from_text(text)
        assert "https://example.com" in urls
        assert "http://test.com" in urls


class TestExtractUrlsFromHtml:
    """Tests for extract_urls_from_html function."""

    def test_simple_anchor_tag(self):
        """Should extract URLs from anchor tags."""
        html = '<a href="https://example.com/file.csv">Download</a>'
        links = extract_urls_from_html(html)
        assert len(links) == 1
        assert links[0]["url"] == "https://example.com/file.csv"
        assert links[0]["text"] == "Download"


class TestExtractEmailContent:
    """Tests for _extract_email_content body URL surfacing.

    Regression tests for the failing Viant DSP scenario where:
      - SendGrid wrapper URLs were not classified as downloads, causing
        has_payload=true searches to return 0 matches.
      - Embedded banner PNGs were classified as downloads, causing
        download_all_link_attachments to "succeed" with the brand banner
        instead of the report.
    """

    def _build_viant_email(self):
        import email as email_module

        raw = b"""From: dsp@viantinc.com
To: client@example.com
Subject: Your Scheduled Report - Test
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="bound"

--bound
Content-Type: text/plain; charset=utf-8

Click here ( https://u5609542.ct.sendgrid.net/ls/click?upn=u001.encoded )

--bound
Content-Type: text/html; charset=utf-8

<html><body>
<a href="https://u5609542.ct.sendgrid.net/ls/click?upn=u001.encoded">Click here</a>
<img src="http://cdn.mcauto-images-production.sendgrid.net/abc/banner.png">
<a href="https://www.viantinc.com/privacy">Privacy</a>
</body></html>
--bound--
"""
        return email_module.message_from_bytes(raw)

    def test_sendgrid_wrapper_in_download_links(self):
        msg = self._build_viant_email()
        content = _extract_email_content(msg, extract_body=True)
        assert content["download_links"], "SendGrid wrapper URL must be in download_links"
        assert all(d["is_click_tracker"] for d in content["download_links"])

    def test_banner_png_excluded_from_download_links(self):
        msg = self._build_viant_email()
        content = _extract_email_content(msg, extract_body=True)
        for d in content["download_links"]:
            assert ".png" not in d["url"].lower()

    def test_body_urls_lists_everything(self):
        """body_urls is the durable triage surface — every URL in the
        body, classified, regardless of download_likely. Ensures an
        agent can recover even when our heuristic misses."""
        msg = self._build_viant_email()
        content = _extract_email_content(msg, extract_body=True)
        urls = content["body_urls"]
        hosts = {u["domain"] for u in urls}
        assert "u5609542.ct.sendgrid.net" in hosts
        assert "cdn.mcauto-images-production.sendgrid.net" in hosts
        assert "www.viantinc.com" in hosts

        # Every entry has classification metadata
        for u in urls:
            assert "is_download_likely" in u
            assert "is_click_tracker" in u
            assert "is_image_asset" in u
            assert "url_length" in u
            assert "source" in u


class TestPickChaseTarget:
    """Tests for the HTML-chase fallback in download_link_attachment."""

    def test_picks_csv_over_branding(self):
        html = """
        <html><body>
          <p>If your download doesn't start automatically,
             <a href="https://reports.example.com/abc.csv?token=xyz">click here</a>.</p>
          <a href="https://www.example.com/privacy">Privacy</a>
          <img src="https://cdn.example.com/logo.png">
        </body></html>
        """
        target = _pick_chase_target(html, "https://example.com/click")
        assert target == "https://reports.example.com/abc.csv?token=xyz"

    def test_returns_none_for_branding_only_page(self):
        html = """
        <html><body>
          <a href="https://www.example.com">Home</a>
          <img src="https://cdn.example.com/banner.png">
        </body></html>
        """
        assert _pick_chase_target(html, "https://example.com/click") is None

    def test_prefers_non_tracker_over_tracker(self):
        """If the landing page contains both a real download and another
        tracker URL, pick the real download."""
        html = """
        <html><body>
          <a href="https://reports.example.com/data.csv">Direct CSV</a>
          <a href="https://example.list-manage.com/track/click?u=different">Re-tracked</a>
        </body></html>
        """
        target = _pick_chase_target(html, "https://example.com/origin")
        assert target == "https://reports.example.com/data.csv"


class TestLooksLikeHtmlResponse:
    """Tests for the HTML detection heuristic on httpx responses."""

    class _FakeResponse:
        def __init__(self, content_type, content=b""):
            self.headers = {"content-type": content_type}
            self.content = content

    def test_html_content_type(self):
        assert _looks_like_html_response(self._FakeResponse("text/html; charset=utf-8")) is True

    def test_csv_content_type(self):
        assert _looks_like_html_response(self._FakeResponse("text/csv")) is False

    def test_octet_stream_with_html_body_sniffs_as_html(self):
        """Some servers mislabel content-type. We sniff the body bytes."""
        assert (
            _looks_like_html_response(self._FakeResponse("application/octet-stream", b"<!DOCTYPE html><html>")) is True
        )

    def test_octet_stream_with_csv_body_is_not_html(self):
        assert _looks_like_html_response(self._FakeResponse("application/octet-stream", b"col1,col2\n1,2\n")) is False
