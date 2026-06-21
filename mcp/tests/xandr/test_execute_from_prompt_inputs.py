"""Unit and integration tests for xandr_execute_deal_from_prompt_inputs.

Covers helpers added in `feat/xandr-execute-from-prompt-inputs`:

- `_normalize_xandr_channel_hint`
- `_make_xandr_quality_flag`
- `_build_xandr_deal_url`

And the end-to-end execute flow (auth -> resolve buyer -> create -> verify),
with quality_flags surfaced for unresolved buyers, unresolved deal types,
create-call failures, and verification failures.
"""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from xandr_mcp import (
    XandrResolutionError,
    _build_xandr_deal_url,
    _make_xandr_quality_flag,
    _normalize_xandr_channel_hint,
    xandr_execute_deal_from_prompt_inputs,
)

from .conftest import XANDR_AUTH_ENDPOINT, XANDR_BASE_URL, XANDR_DEAL_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE


@pytest.fixture(autouse=True)
def _isolate_xandr_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    cache_root = tmp_path_factory.mktemp("xacache")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


XANDR_BUYER_ENDPOINT = f"{XANDR_BASE_URL}/buyer"


def _buyers_response(buyers: list[dict]) -> dict:
    return {
        "response": {
            "status": "OK",
            "count": len(buyers),
            "buyers": buyers,
            "dbg_info": {"instance": "api-test", "time": 10},
        }
    }


def _create_deal_response(deal_id: int, **overrides) -> dict:
    deal = {
        "id": deal_id,
        "name": "Test Deal",
        "code": "test_deal",
        "state": "active",
        "deal_type": {"id": 2, "name": "Private Auction"},
        "buyers": [{"id": 123, "name": "Acme DSP"}],
        **overrides,
    }
    return {"response": {"status": "OK", "deal": deal, "dbg_info": {"instance": "x", "time": 5}}}


def _get_deal_response(deal_id: int, **overrides) -> dict:
    deal = {
        "id": deal_id,
        "name": "Test Deal",
        "code": "test_deal",
        "state": "active",
        "deal_type": {"id": 2, "name": "Private Auction"},
        "buyers": [{"id": 123, "name": "Acme DSP"}],
        **overrides,
    }
    return {"response": {"status": "OK", "deal": deal}}


XANDR_LINE_ITEM_ENDPOINT = f"{XANDR_BASE_URL}/line-item"


def _line_item_response(line_item_id: int = 8800, deal_id: int = 5010, **overrides) -> dict:
    """Standard line-item create response. Every successful execute call now
    creates a line item to carry the curator margin and the IO/profile/deal
    relationship."""
    line_item = {
        "id": line_item_id,
        "name": "Test (Elcano line item)",
        "deals": [{"id": deal_id}],
        "line_item_subtype": "standard_curated",
        "active": True,
        **overrides,
    }
    return {"response": {"status": "OK", "line-item": line_item}}


def _mock_io_test_endpoints(mock_router: respx.MockRouter, *, deal_id: int) -> None:
    """Add the standard `/line-item` POST mock used by happy-path tests.

    Tests should specify `insertion_order_name="Marketplace Pro"` (or any
    other from the seed catalog) on the execute call so the IO resolver
    finds the IO without an HTTP roundtrip.
    """
    mock_router.post(XANDR_LINE_ITEM_ENDPOINT).mock(
        return_value=httpx.Response(200, json=_line_item_response(line_item_id=8800 + deal_id, deal_id=deal_id)),
    )


class TestNormalizeChannelHint:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("display", "display"),
            ("DISPLAY", "display"),
            ("olv", "display"),
            ("display/olv", "display"),
            ("ctv", "ctv"),
            ("CTV", "ctv"),
            ("ott", "ott"),
            ("OTT", "ott"),
            (None, None),
            ("", None),
            ("audio", None),
        ],
    )
    def test_canonicalizes(self, raw, expected):
        assert _normalize_xandr_channel_hint(raw) == expected


class TestMakeQualityFlag:
    def test_required_fields(self):
        flag = _make_xandr_quality_flag("xandr_test", "msg")
        assert flag == {"flag": "xandr_test", "impact": "msg"}

    def test_extra_context_kept(self):
        flag = _make_xandr_quality_flag("xandr_test", "msg", deal_id=5, name="X")
        assert flag["deal_id"] == 5
        assert flag["name"] == "X"

    def test_none_dropped(self):
        flag = _make_xandr_quality_flag("xandr_test", "msg", drop=None, keep="x")
        assert "drop" not in flag


class TestBuildDealUrl:
    """Curate UI deep-links by line_item_id, not deal_id, on host
    curate.xandr.com (NOT console.xandr.com). Trader-confirmed working URL:
    https://curate.xandr.com/smw/line-items?line_item_id=29338518.
    """

    def test_with_line_item_id(self):
        assert _build_xandr_deal_url(29338518) == "https://curate.xandr.com/smw/line-items?line_item_id=29338518"

    def test_none_id(self):
        assert _build_xandr_deal_url(None) is None

    def test_non_int_returns_none(self):
        assert _build_xandr_deal_url("not-an-int") is None  # type: ignore[arg-type]


class TestExecuteFromPromptInputsHappyPath:
    @pytest.mark.asyncio
    async def test_resolves_buyer_creates_and_verifies(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_BUYER_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_buyers_response([{"id": 123, "name": "Acme DSP"}])),
        )
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_create_deal_response(deal_id=5010)),
        )
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_get_deal_response(deal_id=5010)),
        )
        _mock_io_test_endpoints(mock_xandr_api, deal_id=5010)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test Deal",
            code="test_deal",
            buyer="Acme DSP",
            insertion_order_name="Marketplace Pro",
            ask_price=2.5,
        )

        assert result["success"] is True
        assert result["phase"] == "verify"
        assert result["deal"]["id"] == 5010
        # Deep-linked by line_item_id (8800 + 5010 = 13810 from the test mock).
        assert result["deal_url"] == "https://curate.xandr.com/smw/line-items?line_item_id=13810"
        assert result["verification"]["success"] is True
        assert result["error"] is None

    @pytest.mark.asyncio
    async def test_numeric_buyer_id_skips_lookup(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        # No /buyer mock — numeric buyer should bypass lookup
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_create_deal_response(deal_id=5011)),
        )
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_get_deal_response(deal_id=5011)),
        )
        _mock_io_test_endpoints(mock_xandr_api, deal_id=5011)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=999,
            insertion_order_name="Marketplace Pro",
        )
        assert result["success"] is True
        assert result["deal"]["id"] == 5011

    @pytest.mark.asyncio
    async def test_channel_drives_device_defaults_and_creates_profile(self, mock_xandr_api: respx.MockRouter):
        captured_profile_payload: dict = {}

        def capture_profile(request: httpx.Request) -> httpx.Response:
            nonlocal captured_profile_payload
            captured_profile_payload = (
                httpx.URL(request.url).path == "/profile"
                and __import__("json").loads(request.content)
                or captured_profile_payload
            )
            return httpx.Response(200, json={"response": {"status": "OK", "profile": {"id": 7777}}})

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(f"{XANDR_BASE_URL}/profile").mock(side_effect=capture_profile)
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_create_deal_response(deal_id=5012)),
        )
        mock_xandr_api.get(url__startswith=XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_get_deal_response(deal_id=5012)),
        )
        _mock_io_test_endpoints(mock_xandr_api, deal_id=5012)

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=999,
            insertion_order_name="Marketplace Pro",
            channel="ctv",
        )
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_default_channel_devices_applied" in flag_names
        ctv_flag = next(f for f in result["quality_flags"] if f["flag"] == "xandr_default_channel_devices_applied")
        assert ctv_flag["channel"] == "ctv"
        assert ctv_flag["device_types"] == [4, 5]
        # Profile was created with the canonical CTV device set + attached to deal
        assert result["profile_id"] == 7777
        assert captured_profile_payload["profile"]["device_type_targets"] == [
            {"device_type": 4},
            {"device_type": 5},
        ]


class TestExecuteFromPromptInputsFailures:
    @pytest.mark.asyncio
    async def test_unresolved_buyer_emits_quality_flag(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_BUYER_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_buyers_response([])),
        )

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer="Nonexistent DSP",
            insertion_order_name="Marketplace Pro",
        )
        assert result["success"] is False
        assert result["phase"] == "resolve"
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_unresolved_buyer" in flag_names

    @pytest.mark.asyncio
    async def test_unresolved_deal_type_emits_quality_flag(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=123,
            insertion_order_name="Marketplace Pro",
            deal_type="Nonexistent Type",
        )
        assert result["success"] is False
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_unresolved_deal_type" in flag_names

    @pytest.mark.asyncio
    async def test_create_call_failure_emits_quality_flag(self, mock_xandr_api: respx.MockRouter):
        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.post(XANDR_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(500, json={"error": "boom"}),
        )

        result = await xandr_execute_deal_from_prompt_inputs(
            name="Test",
            code="test",
            buyer=123,
            insertion_order_name="Marketplace Pro",
        )
        assert result["success"] is False
        assert result["phase"] == "create"
        flag_names = [f["flag"] for f in result["quality_flags"]]
        assert "xandr_create_call_failed" in flag_names


class TestResolveXandrBuyerErrorRaising:
    """Sanity: existing buyer resolver still raises XandrResolutionError on miss."""

    @pytest.mark.asyncio
    async def test_resolver_raises_with_candidates(self, mock_xandr_api: respx.MockRouter):
        from xandr_mcp import _resolve_xandr_buyer

        mock_xandr_api.post(XANDR_AUTH_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
        mock_xandr_api.get(url__startswith=XANDR_BUYER_ENDPOINT).mock(
            return_value=httpx.Response(200, json=_buyers_response([])),
        )
        with pytest.raises(XandrResolutionError) as excinfo:
            await _resolve_xandr_buyer("Whatever DSP")
        assert excinfo.value.code in {"buyer_unresolved", "buyer_ambiguous"}
