"""
Tests for the validate_domains MCP tool.

This tool is local-only (no HTTP calls) and validates domain format strings.
"""

import os

# Import from parent mcp directory
import sys

import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_validate_domains


class TestValidateDomains:
    """Tests for validate_domains tool - local validation, no HTTP calls."""

    @pytest.mark.asyncio
    async def test_valid_domains(self):
        """Test that correctly formatted domains pass validation."""
        domains = [
            "example.com",
            "sub.domain.example.com",
            "my-site.co.uk",
            "test123.org",
            "a.io",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 5
        assert len(result["invalid"]) == 0
        assert "All 5 domains are valid" in result["summary"]

    @pytest.mark.asyncio
    async def test_invalid_domains_with_protocol(self):
        """Test that domains with http/https protocol are rejected."""
        domains = [
            "http://example.com",
            "https://example.com",
            "//example.com",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 3

        for invalid in result["invalid"]:
            assert "protocol" in invalid["reason"].lower()

    @pytest.mark.asyncio
    async def test_invalid_domains_with_path(self):
        """Test that domains with paths are rejected."""
        domains = [
            "example.com/path",
            "example.com/path/to/page",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 2

        for invalid in result["invalid"]:
            assert "path" in invalid["reason"].lower()

    @pytest.mark.asyncio
    async def test_invalid_domains_with_query_string(self):
        """Test that domains with query strings are rejected."""
        domains = [
            "example.com?query=value",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 1
        assert "query" in result["invalid"][0]["reason"].lower()

    @pytest.mark.asyncio
    async def test_invalid_domains_with_port(self):
        """Test that domains with port numbers are rejected."""
        domains = [
            "example.com:8080",
            "localhost:3000",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 2

        for invalid in result["invalid"]:
            assert "port" in invalid["reason"].lower()

    @pytest.mark.asyncio
    async def test_invalid_domain_format(self):
        """Test that malformed domains are rejected."""
        domains = [
            "-invalid.com",  # Starts with hyphen
            ".com",  # Missing domain name
            "nodot",  # No TLD
            "",  # Empty
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 4

    @pytest.mark.asyncio
    async def test_mixed_valid_and_invalid(self):
        """Test mixed list of valid and invalid domains."""
        domains = [
            "valid.com",
            "https://invalid.com",
            "also-valid.org",
            "invalid.com/path",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 2
        assert len(result["invalid"]) == 2
        assert "valid.com" in result["valid"]
        assert "also-valid.org" in result["valid"]

    @pytest.mark.asyncio
    async def test_domains_are_normalized_lowercase(self):
        """Test that domains are converted to lowercase."""
        domains = [
            "EXAMPLE.COM",
            "Test.Org",
            "MixedCase.Co.Uk",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 3
        assert "example.com" in result["valid"]
        assert "test.org" in result["valid"]
        assert "mixedcase.co.uk" in result["valid"]

    @pytest.mark.asyncio
    async def test_domains_are_stripped(self):
        """Test that whitespace is trimmed from domains."""
        domains = [
            "  example.com  ",
            "\texample.org\n",
        ]

        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 2
        assert "example.com" in result["valid"]
        assert "example.org" in result["valid"]

    @pytest.mark.asyncio
    async def test_empty_list(self):
        """Test handling of empty domain list."""
        result = await ox_validate_domains([])

        assert len(result["valid"]) == 0
        assert len(result["invalid"]) == 0
        assert "All 0 domains are valid" in result["summary"]

    @pytest.mark.asyncio
    async def test_no_http_calls_made(self, mock_openx_graphql: respx.MockRouter):
        """Verify that validate_domains makes NO HTTP calls.

        This is a critical test - if any HTTP request is made, respx will fail.
        """
        domains = ["example.com", "test.org"]

        # With respx mock active, any real HTTP call would raise an error
        result = await ox_validate_domains(domains)

        assert len(result["valid"]) == 2
        # Verify no routes were called (none were configured, and none should be needed)
        assert mock_openx_graphql.calls.call_count == 0
