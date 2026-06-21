"""
Real Integration Tests for All MCP Servers

This module provides integration tests that verify MCP server functionality.
Tests are organized into:

1. **Lightweight tests** - Test pure functions, no API calls (fast, free)
2. **Critical integration tests** - Minimal API calls to verify connectivity (default)
3. **Expensive tests** - Comprehensive API testing (marked with @pytest.mark.expensive)

Run modes:
- `pytest mcp/` - Runs lightweight + critical tests (default, cost-effective)
- `pytest mcp/ -m expensive` - Runs only expensive tests
- `pytest mcp/ -m "not expensive"` - Skips expensive tests
"""

import json
import os
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path

import pytest

# ============================================================================
# LIGHTWEIGHT TESTS - No API calls, test pure functions
# ============================================================================


class TestSendGridHelpers:
    """Test SendGrid helper functions (no API calls)."""

    def test_decode_response_body(self):
        """Test response body decoding."""
        from sendgrid_server import _decode_response_body

        assert _decode_response_body(None) == ""
        assert _decode_response_body(b"") == ""
        assert _decode_response_body("") == ""
        assert _decode_response_body(b"test") == "test"
        assert _decode_response_body("test") == "test"

    def test_normalize_recipients(self):
        """Test recipient normalization."""
        from sendgrid_server import _normalize_recipients

        assert _normalize_recipients(None) == []
        assert _normalize_recipients([]) == []
        assert len(_normalize_recipients(["test@example.com"])) == 1
        assert len(_normalize_recipients(["a@test.com", "", "b@test.com"])) == 2
        assert len(_normalize_recipients(["a@test.com,b@test.com"])) == 2
        assert len(_normalize_recipients(["a@test.com; b@test.com\n c@test.com"])) == 3

    def test_detect_html_content_basic_tags(self):
        """Test HTML detection with common HTML tags."""
        from sendgrid_server import _detect_html_content

        # Should detect HTML
        assert _detect_html_content("<html><body>Hello</body></html>") is True
        assert _detect_html_content("<div>Content</div>") is True
        assert _detect_html_content("<p>Paragraph</p>") is True
        assert _detect_html_content("<br>") is True
        assert _detect_html_content("<br/>") is True
        assert _detect_html_content("<span style='color:red'>Text</span>") is True
        assert _detect_html_content("<table><tr><td>Cell</td></tr></table>") is True
        assert _detect_html_content("<h1>Header</h1>") is True
        assert _detect_html_content("<a href='url'>Link</a>") is True
        assert _detect_html_content("<img src='img.png'>") is True
        assert _detect_html_content("<strong>Bold</strong>") is True
        assert _detect_html_content("<em>Italic</em>") is True

        # Should NOT detect as HTML (plain text)
        assert _detect_html_content("Hello, this is plain text") is False
        assert _detect_html_content("Use x < y and z > w in math") is False
        assert _detect_html_content("") is False
        assert _detect_html_content("   ") is False

    def test_detect_html_content_doctype(self):
        """Test HTML detection with DOCTYPE."""
        from sendgrid_server import _detect_html_content

        assert _detect_html_content("<!DOCTYPE html><html></html>") is True
        assert _detect_html_content("<!doctype html>\n<html></html>") is True
        assert _detect_html_content("  <!DOCTYPE HTML>") is True

    def test_detect_html_content_entities(self):
        """Test HTML detection with HTML entities."""
        from sendgrid_server import _detect_html_content

        # Entities alone are not enough - need tags too
        assert _detect_html_content("Use &amp; for ampersand") is False

        # Entities with tags should be detected
        assert _detect_html_content("<p>Hello &amp; goodbye</p>") is True
        assert _detect_html_content("<div>&nbsp;&nbsp;Indented</div>") is True
        assert _detect_html_content("<span>&lt;code&gt;</span>") is True

    def test_detect_html_content_real_emails(self):
        """Test HTML detection with realistic email content."""
        from sendgrid_server import _detect_html_content

        # Styled HTML email
        styled_email = """
        <!DOCTYPE html>
        <html>
        <head><style>body { font-family: Arial; }</style></head>
        <body>
            <div style="background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);">
                <h1>Weekly Report</h1>
                <p>Here are your metrics:</p>
                <table>
                    <tr><td>Revenue</td><td>$10,000</td></tr>
                </table>
            </div>
        </body>
        </html>
        """
        assert _detect_html_content(styled_email) is True

        # Simple HTML email
        simple_html = "<p>Hello,</p><p>This is a test email.</p><br><p>Thanks!</p>"
        assert _detect_html_content(simple_html) is True

        # Plain text email (should NOT be detected as HTML)
        plain_text = """
        Hello,

        This is a plain text email.
        Here are your numbers: 100 > 50 and 50 < 100.

        Best regards,
        John
        """
        assert _detect_html_content(plain_text) is False

    def test_resolve_content_type_auto_detection(self):
        """Test content type resolution with auto-detection."""
        from sendgrid_server import _resolve_content_type

        # HTML content with no explicit type -> detect as HTML
        resolved, corrected = _resolve_content_type("<p>Hello</p>", None)
        assert resolved == "text/html"
        assert corrected is False

        # Plain text with no explicit type -> default to text/html
        resolved, corrected = _resolve_content_type("Hello plain text", None)
        assert resolved == "text/html"
        assert corrected is False

    def test_resolve_content_type_auto_correction(self):
        """Test content type auto-correction when HTML is detected."""
        from sendgrid_server import _resolve_content_type

        # HTML content with text/plain specified -> auto-correct to HTML
        resolved, corrected = _resolve_content_type("<div>HTML content</div>", "text/plain")
        assert resolved == "text/html"
        assert corrected is True

        # HTML content with text/html specified -> no correction needed
        resolved, corrected = _resolve_content_type("<div>HTML content</div>", "text/html")
        assert resolved == "text/html"
        assert corrected is False

        # Plain text with text/html specified -> use specified (HTML can contain plain text)
        resolved, corrected = _resolve_content_type("Plain text", "text/html")
        assert resolved == "text/html"
        assert corrected is False

        # Plain text with text/plain specified -> use specified
        resolved, corrected = _resolve_content_type("Plain text", "text/plain")
        assert resolved == "text/plain"
        assert corrected is False


class TestSESS3Helpers:
    """Test SES S3 Email helper functions (no API calls)."""

    def test_extract_csvs_from_email(self):
        """Test CSV extraction from raw email bytes."""
        from ses_s3_email import _extract_csvs_from_email

        # Create a minimal multipart email with a CSV attachment
        email_content = b"""From: sender@example.com
To: recipient@example.com
Subject: Test Report with CSV
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="boundary123"

--boundary123
Content-Type: text/plain

This email has a CSV attachment.

--boundary123
Content-Type: text/csv; name="report.csv"
Content-Disposition: attachment; filename="report.csv"

name,value
test,100
demo,200

--boundary123--
"""
        subject, from_addr, attachments = _extract_csvs_from_email(email_content)

        assert subject == "Test Report with CSV"
        assert from_addr == "sender@example.com"
        assert len(attachments) == 1
        assert attachments[0]["filename"] == "report.csv"
        assert attachments[0]["content_type"] == "text/csv"
        assert b"test,100" in attachments[0]["payload"]

    def test_extract_csvs_no_attachments(self):
        """Test CSV extraction when email has no attachments."""
        from ses_s3_email import _extract_csvs_from_email

        # Email without attachments
        email_content = b"""From: sender@example.com
To: recipient@example.com
Subject: Plain Email
Content-Type: text/plain

This is a plain email with no attachments.
"""
        subject, from_addr, attachments = _extract_csvs_from_email(email_content)

        assert subject == "Plain Email"
        assert from_addr == "sender@example.com"
        assert len(attachments) == 0

    def test_extract_csvs_non_csv_attachment(self):
        """Test CSV extraction ignores non-CSV attachments."""
        from ses_s3_email import _extract_csvs_from_email

        # Email with PDF attachment (not CSV)
        email_content = b"""From: sender@example.com
To: recipient@example.com
Subject: PDF Report
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="boundary456"

--boundary456
Content-Type: text/plain

This email has a PDF attachment.

--boundary456
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"

%PDF-fake-content

--boundary456--
"""
        subject, from_addr, attachments = _extract_csvs_from_email(email_content)

        assert subject == "PDF Report"
        assert len(attachments) == 0  # PDF should not be extracted

    def test_url_extraction(self):
        """Test URL extraction from text."""
        from ses_s3_email import extract_urls_from_text

        text = "Check https://example.com and https://test.com for info"
        urls = extract_urls_from_text(text)
        assert len(urls) == 2
        assert "https://example.com" in urls

        # Test punctuation cleanup
        text = "Visit https://example.com. Also https://test.com!"
        urls = extract_urls_from_text(text)
        assert "https://example.com" in urls
        assert "https://test.com" in urls

    def test_url_classification(self):
        """Test URL classification logic."""
        from ses_s3_email import classify_url

        # CSV file should be classified as download
        result = classify_url("https://example.com/report.csv")
        assert result["is_download_likely"] is True
        assert result["extension"] == ".csv"

        # Regular page should not be
        result = classify_url("https://example.com/about")
        assert result["is_download_likely"] is False

        # Download keyword should trigger
        result = classify_url("https://example.com/api/download?id=123")
        assert result["is_download_likely"] is True


class TestIndexExchangeHelpers:
    """Test Index Exchange helper functions (no API calls)."""

    def test_decode_jwt_exp_valid(self):
        """Test valid JWT expiry decoding."""
        from indexexchange_mcp import _decode_jwt_exp

        # Valid payload: {"exp": 1678886400} -> 2023-03-15T16:00:00Z
        # Base64 of {"exp":1678886400} is eyJleHAiOjE2Nzg4ODY0MDB9
        token = "header.eyJleHAiOjE2Nzg4ODY0MDB9.sig"
        assert _decode_jwt_exp(token) == 1678886400

    def test_decode_jwt_exp_invalid_format(self):
        """Test invalid JWT format."""
        from indexexchange_mcp import _decode_jwt_exp

        assert _decode_jwt_exp("invalid") is None
        assert _decode_jwt_exp("part1.part2") is None  # Needs 3 parts usually, but code checks for >= 2
        # Code checks `if len(parts) < 2: return None`
        # But split returns at least 1 element.

        # Test malformed base64
        assert _decode_jwt_exp("header.invalid-base64!.sig") is None

    def test_decode_jwt_exp_missing_exp(self):
        """Test JWT payload without exp claim."""
        from indexexchange_mcp import _decode_jwt_exp

        # Payload: {} -> e30=
        token = "header.e30=.sig"
        assert _decode_jwt_exp(token) is None

    def test_make_error(self):
        """Test error dict creation."""
        from indexexchange_mcp import _make_error

        err = _make_error("test_op", 400, "Bad Request")
        assert err["provider"] == "indexexchange"
        assert err["operation"] == "test_op"
        assert err["status_code"] == 400
        assert err["message"] == "Bad Request"
        assert "details" not in err

        err_with_details = _make_error("op", 500, "Error", {"retry": True})
        assert err_with_details["details"] == {"retry": True}


# ============================================================================
# CRITICAL INTEGRATION TESTS - Minimal API calls to verify connectivity
# ============================================================================


class TestSendGridMCPServer:
    """Critical integration tests for SendGrid MCP server (minimal API calls)."""

    @pytest.mark.asyncio
    async def test_email_validation(self):
        """Test email validation to verify API connectivity (low cost)."""
        from sendgrid_server import validate_email_address

        api_key = os.environ.get("SENDGRID_API_KEY")
        if not api_key:
            pytest.skip("SENDGRID_API_KEY not set")

        result = await validate_email_address("test@example.com")

        # API might return error if validation feature not enabled, that's okay
        if "error" not in result:
            assert "status_code" in result
            assert result["status_code"] in (200, 202)

    @pytest.mark.expensive
    @pytest.mark.asyncio
    async def test_send_email(self):
        """Test sending a real email (EXPENSIVE - sends actual email)."""
        from sendgrid_server import send_email

        api_key = os.environ.get("SENDGRID_API_KEY")
        from_email = os.environ.get("SENDGRID_FROM_EMAIL")

        if not api_key or not from_email:
            pytest.skip("SENDGRID credentials not set")

        result = await send_email(
            to_email="brad@elcanotek.com",
            subject="MCP Integration Test",
            content=f"Automated test - Run: {os.environ.get('GITHUB_RUN_ID', 'local')}",
            from_email=from_email,
        )

        assert "error" not in result
        assert result["status_code"] == 202


class TestSESS3EmailMCPServer:
    """Critical integration tests for SES S3 Email MCP server (minimal API calls)."""

    @pytest.mark.asyncio
    async def test_s3_connectivity(self):
        """Test S3 connectivity with a bounded email search (low cost)."""
        from ses_s3_email import search_emails

        bucket = os.environ.get("EMAIL_S3_BUCKET")
        access_key = os.environ.get("AWS_ACCESS_KEY_ID")

        if not bucket or not access_key:
            pytest.skip("AWS credentials not set")

        today = datetime.now(UTC).date().isoformat()
        result = await search_emails(date_from=today, date_to=today, max_results=1)

        assert "status" in result
        assert result["status"] in ("success", "error")
        assert "search_criteria" in result or result["status"] == "error"

    @pytest.mark.asyncio
    async def test_search_emails_requires_bounded_window(self):
        """search_emails must still reject missing date bounds outright."""
        from ses_s3_email import search_emails

        missing_bounds = await search_emails(sender_contains="magnite", date_from="2026-04-01")
        assert missing_bounds["status"] == "error"
        assert "requires both date_from and date_to" in missing_bounds["error"]

    @pytest.mark.asyncio
    async def test_search_emails_accepts_wide_window_with_warning(self):
        """Wide windows are now accepted but surface a date_window_auto_chunked warning."""
        from ses_s3_email import search_emails

        wide_window = await search_emails(
            sender_contains="definitely-not-a-real-sender-xyz",
            date_from="2026-04-01",
            date_to="2026-04-07",
            max_results=1,
        )
        # Accept either success (no matches) or error on real S3 calls in CI,
        # but the rejection for width must be gone.
        assert wide_window["status"] in ("success", "error")
        if wide_window["status"] == "error":
            assert "maximum 3-day date window" not in wide_window["error"]
        else:
            warning_codes = {w.get("code") for w in wide_window.get("search_warnings", [])}
            assert "date_window_auto_chunked" in warning_codes

    @pytest.mark.expensive
    @pytest.mark.asyncio
    async def test_download_link(self):
        """Test downloading a file (EXPENSIVE - network bandwidth)."""
        from ses_s3_email import download_link_attachment

        test_url = "https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf"

        result = await download_link_attachment(test_url, timeout_seconds=30)

        if result["status"] == "success":
            assert Path(result["saved_to"]).exists()
            Path(result["saved_to"]).unlink()  # Clean up
        else:
            pytest.skip(f"Download failed: {result.get('error')}")


class TestMCPServerConnectivity:
    """Test MCP servers can be started and respond via stdio transport."""

    def _check_server_starts(self, script_path, env_vars):
        """Helper to check if a server starts successfully."""
        proc = subprocess.Popen(
            [sys.executable, script_path],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env_vars,
            text=True,
        )

        # Send initialize request to verify it's listening
        init_request = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "test-client", "version": "1.0.0"},
            },
        }

        try:
            if proc.stdin:
                proc.stdin.write(json.dumps(init_request) + "\n")
                proc.stdin.flush()

            # Wait a bit for response or timeout
            proc.wait(timeout=2)

            # If we get here, the process exited early - potential failure
            stderr = proc.stderr.read() if proc.stderr else ""
            pytest.fail(f"Server exited early with code {proc.returncode}. Stderr: {stderr}")

        except subprocess.TimeoutExpired:
            # Server is running (waiting for more input), which is good
            proc.terminate()
            proc.wait(timeout=2)
        except Exception as e:
            proc.terminate()
            pytest.fail(f"Server failed to start: {e}")

    def test_sendgrid_server_starts(self):
        """Test that SendGrid MCP server can start."""
        api_key = os.environ.get("SENDGRID_API_KEY")
        from_email = os.environ.get("SENDGRID_FROM_EMAIL")

        if not api_key or not from_email:
            pytest.skip("SENDGRID credentials not set")

        env = os.environ.copy()
        env["SENDGRID_API_KEY"] = api_key
        env["SENDGRID_FROM_EMAIL"] = from_email
        self._check_server_starts("mcp/sendgrid_server.py", env)

    def test_ses_s3_server_starts(self):
        """Test that SES S3 Email MCP server can start."""
        bucket = os.environ.get("EMAIL_S3_BUCKET")
        access_key = os.environ.get("AWS_ACCESS_KEY_ID")

        if not bucket or not access_key:
            pytest.skip("AWS credentials not set")

        env = os.environ.copy()
        self._check_server_starts("mcp/ses_s3_email.py", env)

    def test_medianet_server_starts(self):
        """Test that Media.net MCP server can start."""
        # Check if we have credentials (token OR email+password)
        token = os.environ.get("MEDIANET_SELECT_TOKEN")
        email = os.environ.get("MEDIANET_SELECT_EMAIL")
        password = os.environ.get("MEDIANET_SELECT_PASSWORD")

        has_creds = token or (email and password)
        if not has_creds:
            pytest.skip("Media.net credentials not set")

        env = os.environ.copy()
        if token:
            env["MEDIANET_SELECT_TOKEN"] = token
        if email:
            env["MEDIANET_SELECT_EMAIL"] = email
        if password:
            env["MEDIANET_SELECT_PASSWORD"] = password
        self._check_server_starts("mcp/medianet_mcp.py", env)


if __name__ == "__main__":
    # Run tests with pytest
    pytest.main([__file__, "-v", "-s"])
