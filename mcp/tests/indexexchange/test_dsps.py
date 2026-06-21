"""
Tests for Index Exchange DSP tools.

Validates:
- ix_list_dsps: correct URL, method, headers
- ix_list_dsp_seats: correct path with dspID
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import ix_list_dsp_seats, ix_list_dsps

from .conftest import IX_DSPS_ENDPOINT, IX_LOGIN_ENDPOINT
from .fixtures import DSP_SEATS_RESPONSE, DSPS_RESPONSE, LOGIN_SUCCESS_RESPONSE


class TestListDSPs:
    """Tests for ix_list_dsps tool."""

    @pytest.mark.asyncio
    async def test_list_dsps_success(self, mock_ix_api: respx.MockRouter):
        """Test listing DSPs returns expected structure."""
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(return_value=httpx.Response(200, json=DSPS_RESPONSE))

        result = await ix_list_dsps()
        assert result["success"] is True
        assert len(result["dsps"]) == 3
        assert result["dsps"][0]["name"] == "The Trade Desk"

    @pytest.mark.asyncio
    async def test_list_dsps_with_filter(self, mock_ix_api: respx.MockRouter):
        """Test listing DSPs with validForClassID filter."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DSPS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(side_effect=capture)

        result = await ix_list_dsps(valid_for_class_id=1)
        assert result["success"] is True
        assert "validForClassID" in str(captured_request.url)

    @pytest.mark.asyncio
    async def test_list_dsps_correct_method_and_headers(self, mock_ix_api: respx.MockRouter):
        """Test that the request uses correct method, URL, and auth headers."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DSPS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(IX_DSPS_ENDPOINT).mock(side_effect=capture)

        await ix_list_dsps()
        assert captured_request.method == "GET"
        assert str(captured_request.url).startswith(IX_DSPS_ENDPOINT)
        assert "Bearer" in captured_request.headers.get("authorization", "")


class TestListDSPSeats:
    """Tests for ix_list_dsp_seats tool."""

    @pytest.mark.asyncio
    async def test_list_dsp_seats_success(self, mock_ix_api: respx.MockRouter):
        """Test listing DSP seats returns expected structure."""
        dsp_id = 1
        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(f"{IX_DSPS_ENDPOINT}/{dsp_id}/seats").mock(
            return_value=httpx.Response(200, json=DSP_SEATS_RESPONSE)
        )

        result = await ix_list_dsp_seats(dsp_id=dsp_id)
        assert result["success"] is True
        assert len(result["seats"]) == 2
        assert result["seats"][0]["seatName"] == "Seat Alpha"

    @pytest.mark.asyncio
    async def test_list_dsp_seats_correct_url(self, mock_ix_api: respx.MockRouter):
        """Test that the correct URL path is constructed with dsp_id."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=DSP_SEATS_RESPONSE)

        mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_ix_api.get(f"{IX_DSPS_ENDPOINT}/42/seats").mock(side_effect=capture)

        await ix_list_dsp_seats(dsp_id=42)
        assert "/dsps/42/seats" in str(captured_request.url)
