#!/usr/bin/env python3
"""
Media.net Select MCP Server

A Model Context Protocol (MCP) server for programmatic deal creation on Media.net Select.
This is a dedicated MCP for the Media.net Select REST API.

Runs within the Victoria Terminal container environment.
"""

import asyncio
import hashlib
import json
import logging
import os
import re
import sys
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any
from uuid import uuid4

import httpx
from mcp.server.fastmcp import FastMCP

_NON_ALNUM_RE = re.compile(r"[^a-z0-9]+")
_DEAL_ID_RE = re.compile(r"[#%$@*&?!`~\"',/\\|(){}\[\]+=^:]")
_DATE_ONLY_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


# Configure logging to stderr (not stdout for STDIO transport)
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger(__name__)

# Initialize FastMCP server
mcp = FastMCP("medianet_mcp")

# Constants
USER_AGENT = "victoria-terminal/1.0"
DEFAULT_TIMEOUT = 60.0
DEFAULT_REPORT_BASE_URL = "https://select-analytics.media.net"
DEFAULT_REPORT_POLL_INTERVAL_SECONDS = 2.0
DEFAULT_REPORT_POLL_TIMEOUT_SECONDS = 180.0
DEFAULT_REPORT_DOWNLOAD_DIR = os.path.expanduser("~/Victoria/medianet_reports")

MEDIANET_REPORT_DIMENSION_ALIASES: dict[str, str] = {
    "day": "day",
    "date": "day",
    "daily": "day",
    "week": "week",
    "hour": "hour",
    "dsp": "dsp",
    "deal": "deal_name",
    "deal name": "deal_name",
    "deal id": "deal",
    "country": "country",
    "device": "device_category",
    "device category": "device_category",
    "supply partner": "supply_partner",
    "demand partner": "demand_partner",
    "environment": "environment",
    "domain": "domain",
    "browser": "browser",
    "os": "os",
    "advertiser": "advertiser_label",
    "agency": "agency_label",
}

MEDIANET_REPORT_METRIC_ALIASES: dict[str, str] = {
    "impressions": "ad_impressions",
    "ad impressions": "ad_impressions",
    "spend": "advertiser_spend",
    "advertiser spend": "advertiser_spend",
    "revenue": "advertiser_spend",
    "margin": "deal_margin",
    "deal margin": "deal_margin",
    "fees": "deal_margin",
    "bid rate": "bid_rate",
    "win rate": "win_rate",
    "cpm": "cpm",
    "avails": "avails",
    "requests": "avails",
    "valid bids": "valid_provider_slot_impressions",
    "video completion": "video_completion_percent",
}

# Media.net API Configuration
DEFAULT_BASE_URL = "https://select.media.net"

# Regex pattern for valid domain
# - No protocol prefix
# - Each label: starts/ends with alphanumeric, can contain hyphens
# - At least one dot separating labels
DOMAIN_PATTERN = re.compile(
    r"^(?!-)"  # Cannot start with hyphen
    r"(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)"  # Labels with dots
    r"+[a-zA-Z]{2,}$"  # TLD (at least 2 chars)
)


class MediaNetClient:
    """
    Client for interacting with the Media.net Select REST API.

    Uses token-based authentication. Token can be provided directly via
    MEDIANET_SELECT_TOKEN or obtained via login with email/password.
    """

    def __init__(self):
        self.base_url = os.environ.get("MEDIANET_SELECT_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.email = os.environ.get("MEDIANET_SELECT_EMAIL", "")
        self.password = os.environ.get("MEDIANET_SELECT_PASSWORD", "")
        self._token = os.environ.get("MEDIANET_SELECT_TOKEN", "")
        self._http_client: httpx.AsyncClient | None = None

    def _is_configured(self) -> bool:
        """Check if Media.net credentials are configured."""
        return bool(self._token) or (bool(self.email) and bool(self.password))

    async def _get_http_client(self) -> httpx.AsyncClient:
        """Get or create the HTTP client."""
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _ensure_token(self) -> str:
        """Ensure we have a valid token, logging in if necessary."""
        if not self._is_configured():
            raise ValueError(
                "Media.net not configured. Set MEDIANET_SELECT_TOKEN or both "
                "MEDIANET_SELECT_EMAIL and MEDIANET_SELECT_PASSWORD environment variables."
            )

        if not self._token and self.email and self.password:
            await self._login()

        return self._token

    async def _login(self) -> None:
        """
        Authenticate with Media.net and obtain a token.

        POST {BASE_URL}/api/login
        JSON: { "user_email": "...", "password": "..." }
        """
        if not self.email or not self.password:
            raise ValueError("Email and password required for login.")

        logger.info("Logging in to Media.net Select API")

        client = await self._get_http_client()
        login_url = f"{self.base_url}/api/login"

        try:
            response = await client.post(
                login_url,
                json={"user_email": self.email, "password": self.password},
                headers={
                    "Content-Type": "application/json",
                    "User-Agent": USER_AGENT,
                },
            )
            response.raise_for_status()
            result = response.json()

            token = result.get("data", {}).get("token")
            if not token:
                raise ValueError("Login successful but no token in response")

            self._token = token
            logger.info("Successfully obtained Media.net token")

        except httpx.HTTPStatusError as e:
            logger.error("Login failed with HTTP status %d", e.response.status_code)
            raise ValueError(f"Media.net login failed: HTTP {e.response.status_code}: {e.response.text}") from e
        except Exception as e:
            logger.error("Login failed: %s", type(e).__name__)
            raise ValueError(f"Media.net login failed: {type(e).__name__}") from e

    def _get_headers(self) -> dict[str, str]:
        """Get the request headers including authentication token."""
        return {
            "token": self._token,
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        }

    async def _request(
        self,
        method: str,
        endpoint: str,
        json_data: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
        retry_on_401: bool = True,
    ) -> dict[str, Any]:
        """
        Execute an HTTP request against the Media.net API.

        Handles token refresh on 401 responses.
        """
        await self._ensure_token()

        client = await self._get_http_client()
        url = f"{self.base_url}{endpoint}"
        headers = self._get_headers()

        logger.info("Executing %s request to Media.net: %s", method, endpoint)

        try:
            if method.upper() == "GET":
                response = await client.get(url, headers=headers, params=params)
            elif method.upper() == "POST":
                response = await client.post(url, headers=headers, json=json_data, params=params)
            elif method.upper() == "PUT":
                response = await client.put(url, headers=headers, json=json_data, params=params)
            elif method.upper() == "DELETE":
                response = await client.delete(url, headers=headers, params=params)
            else:
                raise ValueError(f"Unsupported HTTP method: {method}")

            if response.status_code == 401 and retry_on_401:
                # Only clear the token when a re-login path exists. With a
                # static env token (MEDIANET_SELECT_TOKEN, no email/password),
                # clearing it would make every later call fail with a
                # misleading "Media.net not configured" instead of the real
                # 401, and the retry below would be sent with no usable token.
                if self.email and self.password:
                    logger.warning("Received 401, attempting token refresh")
                    self._token = ""
                    return await self._request(method, endpoint, json_data, params, retry_on_401=False)
                logger.error("Received 401 with a static MEDIANET_SELECT_TOKEN; token is expired or invalid")

            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error("HTTP error %d on %s %s", e.response.status_code, method, endpoint)
            raise

    async def create_deal(self, payload: dict[str, Any]) -> dict[str, Any]:
        logger.info("Creating deal: %s", payload.get("display_name", "unnamed"))
        result = await self._request("POST", "/api/v2/deals", json_data=payload)
        return result.get("data", result)

    async def list_deals(
        self,
        page_no: int = 1,
        page_size: int = 50,
        status: list[int] | None = None,
        deal_ids: list[str] | None = None,
    ) -> list[dict[str, Any]]:
        params: list[tuple[str, Any]] = [
            ("page_no", page_no),
            ("page_size", page_size),
        ]

        if status:
            for s in status:
                params.append(("filter[status][]", s))

        if deal_ids:
            for d in deal_ids:
                params.append(("filter[deal_id][]", d))

        logger.info("Listing deals (page_no=%d, page_size=%d)", page_no, page_size)

        client = await self._get_http_client()
        await self._ensure_token()

        url = f"{self.base_url}/api/v2/deals"
        headers = self._get_headers()

        response = await client.get(url, headers=headers, params=params)

        if response.status_code == 401:
            logger.warning("Received 401, attempting token refresh")
            self._token = ""
            await self._ensure_token()
            headers = self._get_headers()
            response = await client.get(url, headers=headers, params=params)

        response.raise_for_status()
        result = response.json()
        return result.get("data", [])

    async def list_demand_partners(self, ad_format_id: int = 0) -> list[dict[str, Any]]:
        logger.info("Listing demand partners for ad_format_id=%d", ad_format_id)
        result = await self._request("GET", f"/api/v2/deals/ad-formats/{ad_format_id}/demand-partners")
        return result.get("data", [])

    async def list_entity(self, entity_name: str) -> list[dict[str, Any]]:
        """Generic getter for /api/v2/deals/{entity_name} catalog endpoints.

        Covers: ad-sizes, browsers, content-categories, countries, devices,
        operating-systems, video-placements, video-playback-methods,
        video-player-sizes, device-languages, app-platforms, app-categories,
        app-content-ratings, app-prices, app-user-ratings, app-ratings.
        """
        result = await self._request("GET", f"/api/v2/deals/{entity_name}")
        return result.get("data", [])

    async def list_segments(self, segment_group: str) -> list[dict[str, Any]]:
        """List segments within a segment group.

        Accepted groups: contextual-segments, experian-syndicated-segments,
        first-party-segments, experian-custom-segments.
        """
        result = await self._request("GET", f"/api/v2/deals/{segment_group}")
        return result.get("data", [])

    async def list_domain_groups(self) -> list[dict[str, Any]]:
        result = await self._request("GET", "/api/v2/deals/domain-groups")
        return result.get("data", [])

    async def list_url_groups(self) -> list[dict[str, Any]]:
        result = await self._request("GET", "/api/v2/deals/url-groups")
        return result.get("data", [])

    async def list_ip_groups(self) -> list[dict[str, Any]]:
        result = await self._request("GET", "/api/v2/deals/ip-groups")
        return result.get("data", [])

    async def update_deal(self, deal_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        logger.info("Updating deal %s", deal_id)
        result = await self._request("PUT", f"/api/v2/deals/{deal_id}", json_data=payload)
        return result.get("data", result)

    async def verify_token(self) -> bool:
        await self._ensure_token()
        await self._request("GET", "/api/v2/deals/devices")
        return True


class MediaNetReportingClient:
    def __init__(self):
        self.base_url = os.environ.get("MEDIANET_REPORT_BASE_URL", DEFAULT_REPORT_BASE_URL).rstrip("/")
        self.email = os.environ.get("MEDIANET_SELECT_EMAIL", "")
        self.password = os.environ.get("MEDIANET_SELECT_PASSWORD", "")
        self._token = os.environ.get("MEDIANET_REPORT_TOKEN", "")
        self._http_client: httpx.AsyncClient | None = None

    def _is_configured(self) -> bool:
        return bool(self._token) or (bool(self.email) and bool(self.password))

    async def _get_http_client(self) -> httpx.AsyncClient:
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _login(self) -> None:
        if not self.email or not self.password:
            raise ValueError("Media.net reporting login requires MEDIANET_SELECT_EMAIL and MEDIANET_SELECT_PASSWORD.")

        client = await self._get_http_client()
        response = await client.post(
            f"{self.base_url}/backend/rest/api/v1/login",
            json={"email": self.email, "password": self.password},
            headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
            follow_redirects=False,
        )
        response.raise_for_status()
        data = response.json()
        token = data.get("token")
        if not token:
            raise ValueError("Media.net reporting login succeeded but no token was returned.")
        self._token = token

    async def _ensure_token(self) -> str:
        if not self._is_configured():
            raise ValueError(
                "Media.net reporting is not configured. Set MEDIANET_REPORT_TOKEN or MEDIANET_SELECT_EMAIL and MEDIANET_SELECT_PASSWORD."
            )
        if not self._token:
            await self._login()
        return self._token

    def _get_headers(self) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self._token}",
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        }

    async def _request(
        self,
        method: str,
        endpoint: str,
        *,
        json_data: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
        retry_on_401: bool = True,
    ) -> dict[str, Any]:
        await self._ensure_token()
        client = await self._get_http_client()
        response = await client.request(
            method=method,
            url=f"{self.base_url}{endpoint}",
            headers=self._get_headers(),
            json=json_data,
            params=params,
        )
        if response.status_code == 401 and retry_on_401:
            self._token = ""
            return await self._request(method, endpoint, json_data=json_data, params=params, retry_on_401=False)
        response.raise_for_status()
        return response.json() if response.content else {}

    async def list_views(self) -> list[dict[str, Any]]:
        data = await self._request("GET", "/backend/rest/api/v1/views")
        return data if isinstance(data, list) else []

    async def get_view_info(self, view_id: int) -> dict[str, Any]:
        return await self._request("GET", f"/backend/rest/api/v1/views/{view_id}")

    async def get_dimensions(self, view_id: int) -> list[dict[str, Any]]:
        data = await self._request("GET", "/backend/rest/api/v1/dimensions", params={"viewId": view_id})
        return data if isinstance(data, list) else []

    async def get_metrics(self, view_id: int) -> list[dict[str, Any]]:
        data = await self._request("GET", "/backend/rest/api/v1/metrics", params={"viewId": view_id})
        return data if isinstance(data, list) else []

    async def get_relations(self, view_id: int, dimensions: list[str]) -> list[dict[str, Any]]:
        data = await self._request(
            "POST",
            "/backend/rest/api/v1/relations",
            json_data={"viewId": view_id, "dimensions": dimensions},
        )
        return data if isinstance(data, list) else []

    async def fetch_data(self, payload: dict[str, Any]) -> dict[str, Any]:
        return await self._request("POST", "/backend/rest/api/v1/fetchData", json_data=payload)

    async def submit_queue_data(self, payload: dict[str, Any]) -> dict[str, Any]:
        return await self._request("POST", "/backend/rest/api/v1/queueData/submit", json_data=payload)

    async def get_queue_progress(self, queue_id: str) -> dict[str, Any]:
        return await self._request("GET", "/backend/rest/api/v1/queueData/progress", params={"queueId": queue_id})

    async def cancel_queue(self, queue_id: str) -> dict[str, Any]:
        return await self._request("POST", "/backend/rest/api/v1/queueData/cancel", json_data={"queueId": queue_id})

    async def download_queue_data(self, queue_id: str) -> tuple[bytes, str]:
        await self._ensure_token()
        client = await self._get_http_client()
        response = await client.get(
            f"{self.base_url}/backend/rest/api/v1/queueData/download",
            headers=self._get_headers(),
            params={"queueId": queue_id},
        )
        response.raise_for_status()
        return response.content, response.headers.get("content-type", "text/csv")


# Global client instance
_medianet_client: MediaNetClient | None = None
_medianet_reporting_client: MediaNetReportingClient | None = None
_prepared_medianet_deals: dict[str, dict[str, Any]] = {}
_entity_cache: dict[str, list[dict[str, Any]]] = {}


def get_medianet_client() -> MediaNetClient:
    """Get or create the Media.net client singleton."""
    global _medianet_client
    if _medianet_client is None:
        _medianet_client = MediaNetClient()
    return _medianet_client


# ──────────────────────────────────────────────────────────────────────────────
# Disk cache for stable Media.net catalog lookups (entities, demand partners,
# segments, domain/url/ip groups).
#
# Most catalog lookups are stable for hours, but every cutlass run otherwise
# re-fetches them. Caching to disk with a 4h TTL eliminates the second-and-
# onwards run cost.
#
# Disable with MEDIANET_CACHE_TTL_SECONDS=0.
# ──────────────────────────────────────────────────────────────────────────────


def _cache_dir() -> Path:
    base = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
    return Path(base) / "cutlass" / "medianet"


def _cache_ttl_seconds() -> int:
    raw = os.environ.get("MEDIANET_CACHE_TTL_SECONDS", "14400")
    try:
        return max(0, int(raw))
    except ValueError:
        return 14400


def _cache_get(key: str) -> Any | None:
    """Return the cached value for `key` if present and fresh; else None."""
    ttl = _cache_ttl_seconds()
    if ttl <= 0:
        return None
    path = _cache_dir() / f"{key}.json"
    if not path.is_file():
        return None
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return None
    if not isinstance(payload, dict) or "stored_at" not in payload or "value" not in payload:
        return None
    stored_at = payload.get("stored_at")
    if not isinstance(stored_at, (int, float)):
        return None
    if (datetime.now(UTC).timestamp() - stored_at) > ttl:
        return None
    return payload.get("value")


def _cache_put(key: str, value: Any) -> None:
    """Best-effort write of `value` to the cache; ignore IO errors."""
    if _cache_ttl_seconds() <= 0:
        return
    try:
        cache_dir = _cache_dir()
        cache_dir.mkdir(parents=True, exist_ok=True)
        path = cache_dir / f"{key}.json"
        tmp = path.with_suffix(".tmp")
        tmp.write_text(
            json.dumps({"stored_at": datetime.now(UTC).timestamp(), "value": value}),
            encoding="utf-8",
        )
        tmp.replace(path)
    except OSError:
        pass


def _make_blocker(code: str, message: str, **details: Any) -> dict[str, Any]:
    blocker: dict[str, Any] = {"code": code, "message": message}
    if details:
        blocker["details"] = details
    return blocker


# Channel-aware device + ad-format defaults. When the caller passes `channel`
# and omits `devices` / `ad_format`, the prepare flow auto-fills both with
# Media.net's canonical values for that channel. Mirrors the OpenX
# `DEFAULT_RENDERING_CONTEXTS`, the PubMatic `_apply_pm_channel_*` helpers,
# and the IX `_ensure_deal_type_targeting_defaults` pattern.
#
# Per the Elcano trader spec:
#
#   Channel | ad_format | devices                            | Notes
#   --------|-----------|------------------------------------|----------------
#   display | 0 Banner  | PC/Desktop + Phone/Mobile + Tablet |
#   olv     | 1 Video   | PC/Desktop + Phone/Mobile + Tablet | NOT a Display
#   ctv     | 1 Video   | Connected TV                       | App-only
#   ott     | 1 Video   | Phone/Mobile + Tablet              | App-only mobile
#
# Names MUST match Media.net's devices catalog exactly — the channel-default
# values are passed straight into `_resolve_entity("devices", ...)` and an
# off-by-one (e.g. "Desktop" instead of "Personal Computer/Desktop") fails
# resolution and blocks the whole deal. Verified against /v1/entities/devices.
#
# Media.net ad_format integers:
#   0 = Banner   1 = Video   2 = Native
# (Note: prior docstring/validation messages disagreed on the
#  Native↔Video numbering — this constant block is the source of truth.)
MN_DEVICE_VALUES_DISPLAY: tuple[str, ...] = (
    "Personal Computer/Desktop",
    "Phone/Mobile",
    "Tablet",
)
MN_DEVICE_VALUES_OLV: tuple[str, ...] = (
    "Personal Computer/Desktop",
    "Phone/Mobile",
    "Tablet",
)
MN_DEVICE_VALUES_CTV: tuple[str, ...] = ("Connected TV",)
MN_DEVICE_VALUES_OTT: tuple[str, ...] = ("Phone/Mobile", "Tablet")
MN_AD_FORMAT_BANNER: int = 0
MN_AD_FORMAT_VIDEO: int = 1
MN_AD_FORMAT_NATIVE: int = 2
MN_CHANNELS_DISPLAY: frozenset[str] = frozenset({"display"})
MN_CHANNELS_OLV: frozenset[str] = frozenset({"olv", "display_olv", "display/olv"})
MN_CHANNELS_CTV: frozenset[str] = frozenset({"ctv"})
MN_CHANNELS_OTT: frozenset[str] = frozenset({"ott"})

# Elcano curator-margin default. Media.net expresses curator margins via the
# `margin` field with `margin_type=1` (Percentage). Default to 30% unless the
# caller passes an explicit value.
ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT: float = 30.0
MN_MARGIN_TYPE_PERCENTAGE: int = 1


def _normalize_mn_channel(channel: str | None) -> str | None:
    """Return canonical "display", "olv", "ctv", "ott", or None for an input channel hint."""
    if channel is None:
        return None
    normalized = str(channel).strip().lower()
    if normalized in MN_CHANNELS_DISPLAY:
        return "display"
    if normalized in MN_CHANNELS_OLV:
        return "olv"
    if normalized in MN_CHANNELS_CTV:
        return "ctv"
    if normalized in MN_CHANNELS_OTT:
        return "ott"
    return None


_MN_CHANNEL_DEVICE_DEFAULTS: dict[str, tuple[str, ...]] = {
    "display": MN_DEVICE_VALUES_DISPLAY,
    "olv": MN_DEVICE_VALUES_OLV,
    "ctv": MN_DEVICE_VALUES_CTV,
    "ott": MN_DEVICE_VALUES_OTT,
}

_MN_CHANNEL_AD_FORMAT_DEFAULTS: dict[str, int] = {
    "display": MN_AD_FORMAT_BANNER,
    "olv": MN_AD_FORMAT_VIDEO,
    "ctv": MN_AD_FORMAT_VIDEO,
    "ott": MN_AD_FORMAT_VIDEO,
}


def _apply_mn_channel_device_defaults(
    devices: list[Any] | None,
    channel: str | None,
) -> tuple[list[Any] | None, bool]:
    """Auto-fill devices from a channel hint when caller didn't supply any.

    Returns (resolved_devices, applied_default). Channel defaults only kick
    in when `devices` is None or empty.
    """
    if devices:
        return devices, False
    canonical = _normalize_mn_channel(channel)
    if canonical and canonical in _MN_CHANNEL_DEVICE_DEFAULTS:
        return list(_MN_CHANNEL_DEVICE_DEFAULTS[canonical]), True
    return devices, False


def _apply_mn_channel_ad_format_default(
    ad_format: int | None,
    channel: str | None,
) -> tuple[int | None, bool]:
    """Auto-fill `ad_format` from a channel hint when caller didn't supply one.

    Returns (resolved_ad_format, applied_default). Returns 0 (Banner) for
    display, 1 (Video) for olv/ctv/ott. When `ad_format` is already set or
    `channel` is unrecognised, returns the caller's value unchanged.
    """
    if ad_format is not None:
        return ad_format, False
    canonical = _normalize_mn_channel(channel)
    if canonical and canonical in _MN_CHANNEL_AD_FORMAT_DEFAULTS:
        return _MN_CHANNEL_AD_FORMAT_DEFAULTS[canonical], True
    return ad_format, False


def _make_mn_quality_flag(flag: str, impact: str, **context: Any) -> dict[str, Any]:
    """Build a structured quality_flags entry (mirrors the IX/PM pattern)."""
    entry: dict[str, Any] = {"flag": flag, "impact": impact}
    for key, value in context.items():
        if value is not None:
            entry[key] = value
    return entry


# Media.net Select doesn't expose a deal-detail page — there's no per-deal
# URL to deep-link into. The Deals list at this URL is the closest the
# trader UI gets, so we surface it in `deal_url` for parity with every
# other SSP. The email report should still render the deal_id and
# display_name prominently so the trader can locate the row in the list.
MEDIANET_DEALS_LIST_URL = "https://select.media.net/deals"


def _build_medianet_deal_url(deal_id: str | None = None) -> str | None:
    """Return the Media.net Select deals-list URL.

    Media.net Select has no deal-detail route — every deal ID lands on the
    same `/deals` list page. We accept an optional `deal_id` for symmetry
    with the other SSP URL builders, but it does NOT affect the output.
    Returns None only when explicit empty-string is passed (lets callers
    suppress the URL entirely if they want).
    """
    if deal_id == "":
        return None
    return MEDIANET_DEALS_LIST_URL


def _coerce_mn_percent(value: float | int | None) -> int | None:
    """Normalize a viewability/percent input to the integer 0-100 form Media.net expects.

    The Media.net API stores `viewability.min`/`viewability.max` as integer
    percent values (0-100) and rejects floats with HTTP 422
    (`{"viewability.min": ["The viewability.min field must be an integer."]}`).
    Trader prompts historically passed fractions (e.g. `0.70`) because that's
    the IX/PubMatic/OpenX convention. Normalize transparently:

      - None              -> None
      - 0                 -> 0
      - 0 < x <= 1        -> round(x * 100)   (treated as fraction)
      - 1 < x <= 100      -> int(round(x))    (treated as percent integer)
      - otherwise         -> raises ValueError (caller surfaces a blocker)
    """
    if value is None:
        return None
    try:
        numeric = float(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"viewability/percent value {value!r} is not numeric") from exc
    if numeric == 0:
        return 0
    if 0 < numeric <= 1:
        return int(round(numeric * 100))
    if 1 < numeric <= 100:
        return int(round(numeric))
    raise ValueError(f"viewability/percent value {value!r} is outside the 0-100 range")


def _blockers_to_mn_quality_flags(blockers: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Surface existing blockers as structured quality_flags."""
    quality_flags: list[dict[str, Any]] = []
    for blocker in blockers:
        if not isinstance(blocker, dict):
            continue
        code = blocker.get("code") or "mn_blocker"
        message = blocker.get("message") or ""
        details = blocker.get("details") or {}
        flag_name = code if code.startswith("mn_") else f"mn_{code}"
        quality_flags.append(_make_mn_quality_flag(flag_name, message, **details))
    return quality_flags


class MediaNetResolutionError(Exception):
    """Raised when an MCP-side name -> ID resolution fails.

    Carries structured `details` so callers can surface candidate matches
    in the prepared-deal artifact's blocker, giving the agent an escape
    hatch when its name guess doesn't match Media.net's catalog.
    """

    def __init__(self, message: str, **details: Any) -> None:
        super().__init__(message)
        self.message = message
        self.details: dict[str, Any] = details


def get_medianet_reporting_client() -> MediaNetReportingClient:
    global _medianet_reporting_client
    if _medianet_reporting_client is None:
        _medianet_reporting_client = MediaNetReportingClient()
    return _medianet_reporting_client


def _write_medianet_report_file(
    *,
    queue_id: str,
    content: bytes,
    filename_hint: str | None,
    output_dir: str | None = None,
) -> dict[str, Any]:
    # Operators can pass output_dir per-call (typically the per-conversation
    # workspace dir from the system prompt). The default
    # ~/Victoria/medianet_reports is fine locally but read-only on production
    # hosts under systemd's ProtectSystem=strict — see magnite_mcp.py for the
    # rationale on dropping the *_DOWNLOAD_DIR env-var override.
    download_dir = Path(output_dir) if output_dir else Path(DEFAULT_REPORT_DOWNLOAD_DIR)
    download_dir = download_dir.expanduser()
    download_dir.mkdir(parents=True, exist_ok=True)
    base_name = (filename_hint or f"{queue_id}---results.csv").strip() or f"{queue_id}---results.csv"
    if "." not in Path(base_name).name:
        base_name = f"{base_name}.csv"
    filepath = download_dir / Path(base_name).name
    filepath.write_bytes(content)
    return {
        "success": True,
        "path": str(filepath),
        "bytes": len(content),
        "sha256": hashlib.sha256(content).hexdigest(),
    }


def _resolve_medianet_fields(requested_values: list[str] | None, alias_map: dict[str, str]) -> list[str]:
    resolved_values: list[str] = []
    for requested_value in requested_values or []:
        normalized = requested_value.strip().lower()
        if not normalized:
            continue
        resolved_values.append(alias_map.get(normalized, requested_value))
    deduped: list[str] = []
    seen: set[str] = set()
    for value in resolved_values:
        if value not in seen:
            deduped.append(value)
            seen.add(value)
    return deduped


# =============================================================================
# MCP Tools
# =============================================================================


def _transform_deal_payload(payload: dict[str, Any]) -> dict[str, Any]:
    """
    Transform a deal payload from legacy format to the correct API format.

    Handles backward compatibility by converting old field names and formats
    to match the Media.net Select API v9 specification.
    """
    transformed = dict(payload)

    # Rename bidders -> demand_partners and extract IDs into a list of strings
    if "bidders" in transformed:
        bidders = transformed.pop("bidders")
        if bidders and isinstance(bidders[0], dict):
            transformed["demand_partners"] = [str(b.get("id", b)) for b in bidders]
        else:
            transformed["demand_partners"] = [str(b) for b in bidders]

    # Rename floor_price -> bid_floor
    if "floor_price" in transformed:
        transformed["bid_floor"] = transformed.pop("floor_price")

    # Convert margin_type from string to integer
    if "margin_type" in transformed and isinstance(transformed["margin_type"], str):
        margin_type_map = {"fixed": 0, "percentage": 1}
        transformed["margin_type"] = margin_type_map.get(transformed["margin_type"].lower(), transformed["margin_type"])

    # Convert status 0 (old inactive) to -1 (correct inactive)
    if "status" in transformed and transformed["status"] == 0:
        transformed["status"] = -1

    # Capitalize environment values
    if "environments" in transformed and isinstance(transformed["environments"], list):
        transformed["environments"] = [env.capitalize() for env in transformed["environments"]]

    # Remove deprecated is_always_on field
    transformed.pop("is_always_on", None)

    return transformed


# =============================================================================
# Resolvers — server-side name -> ID lookups for the deal-creation flow
# =============================================================================


_MAX_CANDIDATES = 50

# Allowed entity catalogs (the GET /api/v2/deals/{entity_name} endpoints).
# Each entry maps a canonical kind to the URL path segment it lives at.
_ENTITY_KINDS = {
    "ad_sizes": "ad-sizes",
    "browsers": "browsers",
    "content_categories": "content-categories",
    "countries": "countries",
    "devices": "devices",
    "operating_systems": "operating-systems",
    "video_placements": "video-placements",
    "video_playback_methods": "video-playback-methods",
    "video_player_sizes": "video-player-sizes",
    "device_languages": "device-languages",
    "app_platforms": "app-platforms",
    "app_categories": "app-categories",
    "app_content_ratings": "app-content-ratings",
    "app_prices": "app-prices",
    "app_user_ratings": "app-user-ratings",
    "app_ratings": "app-ratings",
}

_SEGMENT_GROUP_PATHS = {
    "contextual_segments": "contextual-segments",
    "first_party_segments": "first-party-segments",
    "experian_syndicated_segments": "experian-syndicated-segments",
    "experian_custom_segments": "experian-custom-segments",
}


def _normalize_lookup_text(value: Any) -> str:
    return _NON_ALNUM_RE.sub("", str(value or "").strip().lower())


async def _load_entity_catalog(kind: str) -> list[dict[str, Any]]:
    """Fetch (and cache) the catalog for `kind`.

    First in-process, then disk cache, then live API. Returns the raw list
    of {id, name} (or {id, code, name} for countries) entries.
    """
    if kind not in _ENTITY_KINDS:
        raise MediaNetResolutionError(f"Unsupported entity kind: {kind}", kind=kind)
    if kind in _entity_cache:
        return _entity_cache[kind]
    cached = _cache_get(f"entity_{kind}")
    if isinstance(cached, list):
        _entity_cache[kind] = cached
        return cached
    client = get_medianet_client()
    items = await client.list_entity(_ENTITY_KINDS[kind])
    items = [item for item in items if isinstance(item, dict)]
    _entity_cache[kind] = items
    _cache_put(f"entity_{kind}", items)
    return items


def _resolve_one_entity(item_list: list[dict[str, Any]], raw: Any) -> Any | None:
    """Match `raw` (str|int) against a catalog item and return its id, or None.

    Each catalog entry has `id` (int or str) and `name`; some (countries) also
    carry `code`. Match by exact id, normalized name, or normalized code.
    """
    if isinstance(raw, bool):
        return None
    if isinstance(raw, int):
        for item in item_list:
            if item.get("id") == raw:
                return raw
        return None
    text = str(raw).strip()
    if not text:
        return None
    normalized = _normalize_lookup_text(text)
    if text.lstrip("-").isdigit():
        as_int = int(text)
        for item in item_list:
            if item.get("id") == as_int:
                return as_int
    for item in item_list:
        item_id = item.get("id")
        for field in ("name", "code"):
            if _normalize_lookup_text(item.get(field)) == normalized:
                return item_id
        if isinstance(item_id, str) and _normalize_lookup_text(item_id) == normalized:
            return item_id
    return None


async def _resolve_entity(kind: str, values: list[Any]) -> tuple[list[Any], list[str]]:
    """Resolve a list of names/ids for a given entity catalog.

    Returns (resolved_ids, warnings). Raises MediaNetResolutionError when
    any value can't be resolved, with a `unresolved` details list and a
    sample of available names so the agent can correct its input.
    """
    if not values:
        return [], []
    catalog = await _load_entity_catalog(kind)
    warnings: list[str] = []
    resolved: list[Any] = []
    unresolved: list[Any] = []
    for raw in values:
        match = _resolve_one_entity(catalog, raw)
        if match is None:
            unresolved.append(raw)
            continue
        resolved.append(match)
    if unresolved:
        sample = [item.get("name") for item in catalog[:_MAX_CANDIDATES] if item.get("name")]
        raise MediaNetResolutionError(
            f"Could not resolve Media.net {kind} for: {unresolved}",
            kind=kind,
            unresolved=unresolved,
            available_sample=sample,
            available_count=len(catalog),
        )
    # Dedupe preserving order.
    seen: set[Any] = set()
    deduped: list[Any] = []
    for v in resolved:
        if v not in seen:
            deduped.append(v)
            seen.add(v)
    return deduped, warnings


# Cross-SSP canonical DSP names that don't match Media.net's catalog ids
# (which are short codes like "TTD"). Trader prompts and the deal-name spec
# use the universal long form ("The Trade Desk") across every SSP — alias
# them here so callers don't have to learn per-SSP shorthand.
MN_DEMAND_PARTNER_ALIASES: dict[str, str] = {
    "thetradedesk": "TTD",
    "tradedesk": "TTD",
    "ttd": "TTD",
    "dv360": "DV 360",
    "displayvideo360": "DV 360",
}


async def _resolve_demand_partners(names_or_ids: list[Any], *, ad_format_id: int) -> tuple[list[str], list[str]]:
    """Resolve demand-partner names or IDs for a given ad format.

    Media.net's demand-partner ids are strings (e.g. "DV 360", "TTD"). Caller
    may pass either the id directly or the human-readable name; the resolver
    handles both. Common cross-SSP aliases (e.g. "The Trade Desk" -> "TTD")
    are applied before the catalog lookup.
    """
    if not names_or_ids:
        return [], []
    cache_key = f"demand_partners_af{ad_format_id}"
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        items = cached
    else:
        client = get_medianet_client()
        items = await client.list_demand_partners(ad_format_id=ad_format_id)
        items = [item for item in items if isinstance(item, dict)]
        _cache_put(cache_key, items)

    by_id = {str(item.get("id")): item for item in items if item.get("id") is not None}
    by_name = {_normalize_lookup_text(item.get("name")): item for item in items if item.get("name")}

    resolved: list[str] = []
    warnings: list[str] = []
    unresolved: list[Any] = []
    for raw in names_or_ids:
        text = str(raw).strip()
        if not text:
            continue
        normalized = _normalize_lookup_text(text)
        aliased = MN_DEMAND_PARTNER_ALIASES.get(normalized)
        if aliased and aliased in by_id:
            # Skip the warning when the caller already typed the canonical id
            # literally — emit it only when we're substituting a visibly
            # different string (e.g. "DV360" -> "DV 360", "The Trade Desk" ->
            # "TTD"), since that's the change a trader needs to see.
            if text != aliased:
                warnings.append(f"Aliased demand partner {raw!r} to Media.net id {aliased!r}.")
            resolved.append(aliased)
            continue
        match = by_id.get(text)
        if match is None:
            match = by_name.get(_normalize_lookup_text(text))
        if match is None:
            unresolved.append(raw)
            continue
        partner_id = match.get("id")
        if partner_id is None:
            unresolved.append(raw)
            continue
        resolved.append(str(partner_id))

    if unresolved:
        sample = [
            {"id": item.get("id"), "name": item.get("name")} for item in items[:_MAX_CANDIDATES] if item.get("id")
        ]
        raise MediaNetResolutionError(
            f"Could not resolve Media.net demand partners for: {unresolved}",
            ad_format_id=ad_format_id,
            unresolved=unresolved,
            available_sample=sample,
            available_count=len(items),
        )
    # Dedupe.
    seen: set[str] = set()
    deduped: list[str] = []
    for v in resolved:
        if v not in seen:
            deduped.append(v)
            seen.add(v)
    return deduped, warnings


async def _resolve_segments(group: str, names_or_ids: list[Any]) -> tuple[list[str], list[str]]:
    """Resolve segment names within a segment group.

    `group` must be one of the keys in _SEGMENT_GROUP_PATHS. Per Media.net's
    payload spec, segment ids are sent as strings — we coerce on output.
    """
    if not names_or_ids:
        return [], []
    if group not in _SEGMENT_GROUP_PATHS:
        raise MediaNetResolutionError(f"Unsupported segment group: {group}", group=group)

    cache_key = f"segments_{group}"
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        items = cached
    else:
        client = get_medianet_client()
        items = await client.list_segments(_SEGMENT_GROUP_PATHS[group])
        items = [item for item in items if isinstance(item, dict)]
        _cache_put(cache_key, items)

    active_only = [item for item in items if item.get("status", 1) != -1]
    by_id = {str(item.get("id")): item for item in active_only if item.get("id") is not None}
    by_name = {_normalize_lookup_text(item.get("name")): item for item in active_only if item.get("name")}

    resolved: list[str] = []
    unresolved: list[Any] = []
    for raw in names_or_ids:
        text = str(raw).strip()
        if not text:
            continue
        match = by_id.get(text) or by_name.get(_normalize_lookup_text(text))
        if match is None:
            unresolved.append(raw)
            continue
        seg_id = match.get("id")
        if seg_id is None:
            unresolved.append(raw)
            continue
        resolved.append(str(seg_id))

    if unresolved:
        sample = [
            {"id": item.get("id"), "name": item.get("name")} for item in active_only[:_MAX_CANDIDATES] if item.get("id")
        ]
        raise MediaNetResolutionError(
            f"Could not resolve Media.net {group} segments for: {unresolved}",
            group=group,
            unresolved=unresolved,
            available_sample=sample,
            available_count=len(active_only),
        )
    seen: set[str] = set()
    deduped: list[str] = []
    for v in resolved:
        if v not in seen:
            deduped.append(v)
            seen.add(v)
    return deduped, []


async def _resolve_geo(targets: list[Any]) -> tuple[list[dict[str, Any]], list[str]]:
    """Normalize geo targeting entries.

    Each target may be:
      - {"geo_type": "country|state|city|zipcode|dma", "id": "...", "is_excluded": bool}
        passed through as-is after light validation, OR
      - a 2-letter country code string (resolved to country entity), OR
      - a country name string (resolved via the countries entity).

    Returns (geo_array, warnings). The geo_array is the value that goes into
    the deal payload's `geo` field.
    """
    if not targets:
        return [], []
    countries_catalog = await _load_entity_catalog("countries")
    # Media.net's countries entity has shipped two shapes:
    #   - {"id": 1, "code": "US", "name": "United States"}  (numeric PK + ISO code)
    #   - {"id": "US", "name": "..."}                       (id IS the ISO code)
    # The deal payload always wants the ISO code as the `id` field, so prefer
    # `code` when present and fall back to `id` otherwise (skipping purely
    # numeric ids since those aren't valid deal-payload geo ids).
    code_to_country: dict[str, str] = {}
    name_to_country: dict[str, str] = {}
    for item in countries_catalog:
        code_value = item.get("code")
        id_value = item.get("id")
        canonical: str | None = None
        if code_value:
            canonical = str(code_value)
        elif isinstance(id_value, str) and id_value.strip() and not id_value.strip().isdigit():
            canonical = id_value.strip()
        if not canonical:
            continue
        for field in ("id", "code"):
            value = item.get(field)
            if value:
                code_to_country.setdefault(_normalize_lookup_text(value), canonical)
        name = item.get("name")
        if name:
            name_to_country.setdefault(_normalize_lookup_text(name), canonical)

    out: list[dict[str, Any]] = []
    unresolved: list[Any] = []

    for raw in targets:
        if isinstance(raw, dict) and raw.get("geo_type") and raw.get("id"):
            entry = {
                "geo_type": str(raw["geo_type"]).strip().lower(),
                "id": str(raw["id"]).strip(),
                "is_excluded": bool(raw.get("is_excluded", False)),
            }
            if entry["geo_type"] not in {"country", "state", "city", "zipcode", "dma"}:
                unresolved.append(raw)
                continue
            out.append(entry)
            continue
        if isinstance(raw, str):
            normalized = _normalize_lookup_text(raw)
            country_code = code_to_country.get(normalized) or name_to_country.get(normalized)
            if country_code:
                out.append({"geo_type": "country", "id": str(country_code), "is_excluded": False})
                continue
        unresolved.append(raw)

    if unresolved:
        sample: list[str] = []
        for item in countries_catalog[:_MAX_CANDIDATES]:
            label = item.get("code") or item.get("name") or item.get("id")
            if label:
                sample.append(str(label))
        raise MediaNetResolutionError(
            f"Could not resolve Media.net geo targets for: {unresolved}",
            unresolved=unresolved,
            available_country_sample=sample,
            available_country_count=len(countries_catalog),
        )
    return out, []


async def _resolve_group_id(kind: str, name_or_id: Any, *, fetcher: str) -> tuple[str | None, str | None]:
    """Resolve a domain/url/ip group name to its numeric internal `id`.

    Returns (id_string, warning_or_none). When `name_or_id` is already
    a numeric id, returns it as-is.
    """
    if name_or_id is None:
        return None, None
    text = str(name_or_id).strip()
    if not text:
        return None, None
    if text.isdigit():
        return text, None

    cache_key = f"{kind}_groups"
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        items = cached
    else:
        client = get_medianet_client()
        client_method = getattr(client, fetcher)
        items = await client_method()
        items = [item for item in items if isinstance(item, dict)]
        _cache_put(cache_key, items)

    normalized = _normalize_lookup_text(text)
    for item in items:
        if (
            _normalize_lookup_text(item.get("name")) == normalized
            or _normalize_lookup_text(item.get("group_id")) == normalized
        ):
            internal_id = item.get("id")
            if internal_id is not None:
                return str(internal_id), None
    sample = [
        {"id": item.get("id"), "name": item.get("name"), "group_id": item.get("group_id")}
        for item in items[:_MAX_CANDIDATES]
    ]
    raise MediaNetResolutionError(
        f"Could not resolve Media.net {kind} group for: {name_or_id!r}",
        kind=kind,
        unresolved=[name_or_id],
        available_sample=sample,
        available_count=len(items),
    )


@mcp.tool()
async def mn_create_deal(payload: dict[str, Any]) -> dict[str, Any]:
    """
    Create a new programmatic deal on Media.net Select.

    This is a CRITICAL action that requires self-audit confirmation before execution.

    The payload should contain Media.net-specific fields. Required fields include:
    - deal_id: Unique identifier for the deal
    - display_name: Human-readable deal name
    - start_date: ISO 8601 formatted start date
    - ad_format: Ad format type (0=Banner, 1=Video, 2=Native)
    - margin: Margin value
    - margin_type: Type of margin (0=Fixed, 1=Percentage)
    - demand_partners: List of demand partner ID strings
    - environments: List of environments (e.g., ["Web", "App"])
    - status: Deal status (1=active, -1=inactive)

    Optional fields:
    - end_date: ISO 8601 formatted end date
    - bid_floor: Floor price for the deal
    - domains: List of domain targeting
    - geos: Geographic targeting
    - devices: Device targeting

    Args:
        payload: Dictionary containing the deal configuration matching Media.net API schema

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the deal was created
            - deal: The created deal object (if successful)
            - error: Error message (if failed)
    """
    logger.info("mn_create_deal called with payload keys: %s", list(payload.keys()))

    # Validate required fields (accept both old and new field names)
    required_fields = [
        "deal_id",
        "display_name",
        "start_date",
        "ad_format",
        "margin",
        "margin_type",
        ("demand_partners", "bidders"),
        "environments",
        "status",
    ]

    missing_fields = []
    for f in required_fields:
        if isinstance(f, tuple):
            if not any(name in payload for name in f):
                missing_fields.append(f[0])
        elif f not in payload:
            missing_fields.append(f)

    if missing_fields:
        error_msg = f"Missing required fields: {', '.join(missing_fields)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }

    # Transform payload to correct API format
    api_payload = _transform_deal_payload(payload)

    try:
        client = get_medianet_client()
        deal = await client.create_deal(api_payload)

        logger.info("Deal created successfully: %s", payload.get("deal_id"))
        return {
            "success": True,
            "deal": deal,
        }

    except ValueError as e:
        error_msg = f"Failed to create deal: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except httpx.HTTPStatusError as e:
        error_msg = f"Failed to create deal: HTTP {e.response.status_code}: {e.response.text}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except Exception as e:
        error_msg = f"Failed to create deal: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


@mcp.tool()
async def mn_list_deals(
    page_no: int = 1,
    page_size: int = 50,
    status: list[int] | None = None,
    deal_ids: list[str] | None = None,
) -> dict[str, Any]:
    """
    Query and return a list of existing deals from Media.net Select.

    Use this to review existing deals and their configurations.

    Args:
        page_no: Page number for pagination (default: 1)
        page_size: Number of deals per page (default: 50)
        status: Optional list of status codes to filter by (e.g., [1] for active)
        deal_ids: Optional list of specific deal IDs to retrieve

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the query succeeded
            - deals: List of deal objects with basic information
            - error: Error message (if failed)
    """
    logger.info("mn_list_deals called (page_no=%d, page_size=%d)", page_no, page_size)

    try:
        client = get_medianet_client()
        deals = await client.list_deals(
            page_no=page_no,
            page_size=page_size,
            status=status,
            deal_ids=deal_ids,
        )

        logger.info("Found %d deals", len(deals))
        return {
            "success": True,
            "deals": deals,
        }

    except ValueError as e:
        error_msg = f"Failed to list deals: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except httpx.HTTPStatusError as e:
        error_msg = f"Failed to list deals: HTTP {e.response.status_code}: {e.response.text}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except Exception as e:
        error_msg = f"Failed to list deals: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


@mcp.tool()
async def mn_get_deal(deal_id: str) -> dict[str, Any]:
    """
    Fetch full details for a specific deal from Media.net Select.

    Uses the list_deals endpoint with a filter to retrieve a single deal.

    Args:
        deal_id: The unique identifier of the deal to retrieve

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the query succeeded
            - deal: Complete deal object with all fields
            - error: Error message (if failed)
    """
    logger.info("mn_get_deal called with deal_id: %s", deal_id)

    try:
        client = get_medianet_client()
        deals = await client.list_deals(deal_ids=[deal_id])

        if not deals:
            return {
                "success": False,
                "error": f"Deal not found: {deal_id}",
            }

        deal = deals[0]
        logger.info("Retrieved deal: %s", deal.get("display_name", deal_id))
        return {
            "success": True,
            "deal": deal,
        }

    except ValueError as e:
        error_msg = f"Failed to get deal: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except httpx.HTTPStatusError as e:
        error_msg = f"Failed to get deal: HTTP {e.response.status_code}: {e.response.text}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except Exception as e:
        error_msg = f"Failed to get deal: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


_MN_ENVIRONMENT_LETTER_CODES = {"A": "App", "W": "Web"}

_MN_UPDATE_STATUS_ALIASES: dict[str, int] = {
    "active": 1,
    "resume": 1,
    "1": 1,
    "paused": -1,
    "pause": -1,
    "inactive": -1,
    "-1": -1,
}


def _deal_response_to_update_payload(deal: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
    """Map a list-deals response object to a PUT /api/v2/deals/{id} body.

    Media.net's update is FULL-REPLACEMENT ("any parameter not passed will be
    set to null"), so an update must round-trip the deal's current
    definition. Differences handled here (per API Guide v9):
      - deal_id MUST NOT be present in an update body — dropped.
      - demand_partners come back as objects with response-only sync info;
        the request wants the id strings.
      - explicit nulls and all-null range objects (video/vcr/viewability)
        are dropped — absent and null are equivalent under full replacement.
      - response statuses 0 (Expired) and 3 (Throttled) are system states
        that cannot be SET — dropped, with a warning.
      - single-letter environment codes observed in responses ("A"/"W") are
        mapped back to the request values ("App"/"Web").

    Returns (payload, warnings). Pure Python, no network — unit-testable.
    """
    warnings: list[str] = []
    payload: dict[str, Any] = {}

    for key, value in deal.items():
        if value is None:
            continue
        if key in ("deal_id", "created_at", "updated_at"):
            continue
        if key in ("video", "vcr", "viewability") and isinstance(value, dict):
            if all(v is None for v in value.values()):
                continue
            payload[key] = value
            continue
        if key == "demand_partners" and isinstance(value, list):
            ids = [item.get("id", item) if isinstance(item, dict) else item for item in value]
            payload[key] = [str(i) for i in ids if i is not None]
            continue
        if key == "environments" and isinstance(value, list):
            mapped = [_MN_ENVIRONMENT_LETTER_CODES.get(str(env), str(env)) for env in value]
            if mapped != list(value):
                warnings.append(f"Mapped response environment codes {value} -> {mapped} for the update body.")
            payload[key] = mapped
            continue
        if key == "status":
            if value in (1, -1):
                payload[key] = value
            else:
                warnings.append(
                    f"Current status {value} is a Media.net system state (0=Expired, 3=Throttled) and "
                    "cannot be set — omitted from the update body."
                )
            continue
        payload[key] = value

    return payload, warnings


@mcp.tool()
async def mn_update_deal(
    deal_id: str,
    display_name: str | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    bid_floor: float | None = None,
    margin: float | None = None,
    margin_type: int | None = None,
    ad_format: int | None = None,
    status: str | int | None = None,
    demand_partners: list[str] | None = None,
    payload_overrides: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Update an existing Media.net Select deal (PUT /api/v2/deals/{deal_id}).

    Media.net's update endpoint is FULL-REPLACEMENT — the body is the same
    schema as create, and per the API guide "any parameter not passed will be
    set to null". This tool therefore does read-modify-write for you: it
    fetches the deal, maps the response back into a request body (dropping
    deal_id, which must not appear in an update), overlays ONLY the arguments
    you pass, submits the PUT, and verifies with a re-fetch. You still only
    pass what you want changed.

    Pause/resume IS this tool: status accepts "paused"/"active" (or -1/1).
    Statuses 0 (Expired) and 3 (Throttled) are Media.net system states and
    cannot be set. There is no delete endpoint in the Select API — pausing is
    the stop path.

    Args:
        deal_id: The deal's unique id (path parameter only — never sent in
            the update body).
        display_name: New name, max 30 characters.
        start_date / end_date: YYYY-MM-DD. end_date may be omitted entirely
            for always-on deals (v9 deprecated is_always_on).
        bid_floor: New CPM floor.
        margin: New margin value (0-25 when margin_type=0 Fixed USD;
            0-50 when margin_type=1 Percentage).
        margin_type: 0 (Fixed) or 1 (Percentage).
        ad_format: 0 (Banner), 1 (Native), 2 (Video).
        status: "active"/1 or "paused"/-1.
        demand_partners: Replacement demand-partner id list (the WHOLE list).
        payload_overrides: Raw request-body fields merged LAST — the escape
            hatch for targeting fields without a dedicated argument (geo,
            devices, segments, publisher_domains, ...). Remember these
            REPLACE the deal's current value for that field.

    Returns:
        {"success": True, "deal": <PUT response>, "updated_fields": [...],
         "warnings": [...], "verification": {...}} or
        {"success": False, "error": ...}.
    """
    logger.info("mn_update_deal called for deal_id=%s", deal_id)
    try:
        client = get_medianet_client()

        current_deals = await client.list_deals(deal_ids=[deal_id])
        if not current_deals:
            return {"success": False, "error": f"Deal not found: {deal_id}"}
        payload, warnings = _deal_response_to_update_payload(current_deals[0])

        changes: dict[str, Any] = {}
        if display_name is not None:
            if not display_name.strip() or len(display_name) > 30:
                return {"success": False, "error": "display_name must be 1-30 characters"}
            changes["display_name"] = display_name
        if start_date is not None:
            changes["start_date"] = start_date
        if end_date is not None:
            changes["end_date"] = end_date
        if bid_floor is not None:
            changes["bid_floor"] = float(bid_floor)
        if margin_type is not None:
            if margin_type not in (0, 1):
                return {"success": False, "error": "margin_type must be 0 (Fixed) or 1 (Percentage)"}
            changes["margin_type"] = margin_type
        if margin is not None:
            effective_margin_type = margin_type if margin_type is not None else payload.get("margin_type", 1)
            maximum = 25 if effective_margin_type == 0 else 50
            if not 0 <= float(margin) <= maximum:
                return {
                    "success": False,
                    "error": f"margin must be 0-{maximum} for margin_type={effective_margin_type}",
                }
            changes["margin"] = float(margin)
        if ad_format is not None:
            if ad_format not in (0, 1, 2):
                return {"success": False, "error": "ad_format must be 0 (Banner), 1 (Native), or 2 (Video)"}
            changes["ad_format"] = ad_format
        if status is not None:
            resolved_status = _MN_UPDATE_STATUS_ALIASES.get(str(status).strip().lower())
            if resolved_status is None:
                return {
                    "success": False,
                    "error": "status must resolve to 1/'active' or -1/'paused' "
                    "(0=Expired and 3=Throttled are system states).",
                }
            changes["status"] = resolved_status
        if demand_partners is not None:
            if not demand_partners:
                return {"success": False, "error": "demand_partners cannot be replaced with an empty list"}
            changes["demand_partners"] = [str(dp) for dp in demand_partners]
        if payload_overrides:
            overrides = dict(payload_overrides)
            if overrides.pop("deal_id", None) is not None:
                warnings.append("deal_id is never sent in an update body — dropped from payload_overrides.")
            changes.update(overrides)

        if not changes:
            return {"success": False, "error": "No update fields provided — pass at least one field to change."}

        payload.update(changes)
        payload.pop("deal_id", None)

        data = await client.update_deal(deal_id, payload)

        verification: dict[str, Any] | None = None
        try:
            verification = await mn_get_deal(deal_id)
        except Exception as verify_exc:  # noqa: BLE001 - verification is best-effort
            warnings.append(f"Post-update verification fetch failed: {verify_exc}")

        return {
            "success": True,
            "deal": data,
            "deal_url": _build_medianet_deal_url(),
            "updated_fields": sorted(changes.keys()),
            "warnings": warnings,
            "verification": verification,
        }
    except ValueError as e:
        return {"success": False, "error": f"Failed to update deal: {e}"}
    except httpx.HTTPStatusError as e:
        return {"success": False, "error": f"Failed to update deal: HTTP {e.response.status_code}: {e.response.text}"}
    except Exception as e:
        return {"success": False, "error": f"Failed to update deal: {e}"}


@mcp.tool()
async def mn_list_demand_partners(ad_format_id: int = 0) -> dict[str, Any]:
    """
    Query and return available demand partners from Media.net Select.

    Use this to discover valid demand partner IDs before creating a deal.

    Args:
        ad_format_id: The ad format ID to get demand partners for (default: 0 for Banner)
            - 0: Banner
            - 1: Video
            - 2: Native

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the query succeeded
            - demand_partners: List of demand partner objects with id and name
            - error: Error message (if failed)
    """
    logger.info("mn_list_demand_partners called for ad_format_id=%d", ad_format_id)

    try:
        client = get_medianet_client()
        partners = await client.list_demand_partners(ad_format_id=ad_format_id)

        demand_partners = [{"id": b.get("id"), "name": b.get("name")} for b in partners]

        logger.info("Found %d demand partners", len(demand_partners))
        return {
            "success": True,
            "demand_partners": demand_partners,
        }

    except ValueError as e:
        error_msg = f"Failed to list demand partners: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except httpx.HTTPStatusError as e:
        error_msg = f"Failed to list demand partners: HTTP {e.response.status_code}: {e.response.text}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }
    except Exception as e:
        error_msg = f"Failed to list demand partners: {str(e)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


@mcp.tool()
async def mn_auth_status() -> dict[str, Any]:
    """
    Check the authentication status of the Media.net Select API connection.

    Verifies that credentials are configured and the token is valid by making
    a low-cost API call.

    Returns:
        Dictionary containing:
            - configured: Boolean indicating if credentials are set
            - authenticated: Boolean indicating if the token is valid
            - error: Error message (if authentication failed)
    """
    logger.info("mn_auth_status called")

    client = get_medianet_client()

    if not client._is_configured():
        return {
            "configured": False,
            "authenticated": False,
            "error": "Media.net not configured. Set MEDIANET_SELECT_TOKEN or both "
            "MEDIANET_SELECT_EMAIL and MEDIANET_SELECT_PASSWORD environment variables.",
        }

    try:
        await client.verify_token()
        return {
            "configured": True,
            "authenticated": True,
        }
    except ValueError as e:
        return {
            "configured": True,
            "authenticated": False,
            "error": str(e),
        }
    except httpx.HTTPStatusError as e:
        return {
            "configured": True,
            "authenticated": False,
            "error": f"HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "configured": True,
            "authenticated": False,
            "error": str(e),
        }


@mcp.tool()
async def mn_validate_domains(domains: list[str]) -> dict[str, Any]:
    """
    Validate a list of domain strings for correct formatting.

    This is a local validation tool (no API call) that mimics the UI's
    validation step before submitting domain allowlists or blocklists.

    Valid domain format:
    - Must contain at least one dot
    - Must not contain protocols (http://, https://)
    - Must not contain paths (/)
    - Must use valid characters (alphanumeric, hyphens, dots)
    - Each label (part between dots) must start and end with alphanumeric

    Examples of valid domains:
    - example.com
    - sub.domain.example.com
    - my-site.co.uk

    Examples of invalid domains:
    - https://example.com (contains protocol)
    - example.com/path (contains path)
    - -invalid.com (starts with hyphen)
    - .com (missing domain name)

    Args:
        domains: List of domain strings to validate

    Returns:
        Dictionary containing:
            - valid: List of valid domain strings
            - invalid: List of dictionaries with domain and reason for invalidity
            - summary: Human-readable summary of validation results
    """
    logger.info("mn_validate_domains called with %d domains", len(domains))

    valid_domains: list[str] = []
    invalid_domains: list[dict[str, str]] = []

    for domain in domains:
        domain = domain.strip().lower()

        if not domain:
            invalid_domains.append({"domain": domain, "reason": "Empty domain"})
            continue

        # Check for protocol
        if domain.startswith(("http://", "https://", "//")):
            invalid_domains.append({"domain": domain, "reason": "Contains protocol (remove http:// or https://)"})
            continue

        # Check for path
        if "/" in domain:
            invalid_domains.append({"domain": domain, "reason": "Contains path (remove everything after /)"})
            continue

        # Check for query string
        if "?" in domain:
            invalid_domains.append(
                {"domain": domain, "reason": "Contains query string (remove ? and everything after)"}
            )
            continue

        # Check for port
        if ":" in domain:
            invalid_domains.append({"domain": domain, "reason": "Contains port number (remove :port)"})
            continue

        # Check for whitespace
        if " " in domain:
            invalid_domains.append({"domain": domain, "reason": "Contains whitespace"})
            continue

        # Check against regex pattern
        if not DOMAIN_PATTERN.match(domain):
            invalid_domains.append({"domain": domain, "reason": "Invalid domain format"})
            continue

        valid_domains.append(domain)

    # Generate summary
    total = len(domains)
    valid_count = len(valid_domains)
    invalid_count = len(invalid_domains)

    if invalid_count == 0:
        summary = f"All {total} domains are valid."
    elif valid_count == 0:
        summary = f"All {total} domains are invalid. Please review and correct them."
    else:
        summary = (
            f"{valid_count} of {total} domains are valid. {invalid_count} domains have issues that need correction."
        )

    logger.info(summary)

    return {
        "valid": valid_domains,
        "invalid": invalid_domains,
        "summary": summary,
    }


# =============================================================================
# Prepare / Submit / Execute — the agent-facing deal-creation flow
# =============================================================================

# Verified-working defaults per Media.net Select API guide:
#  - margin_type 1 (Percentage) is the curator-typical revenue model.
#  - status 1 (Active).
#  - environments default to ["Web"]; "App" requires app_platform/app_categories.
#  - ad_format defaults to 0 (Banner).


def _validate_deal_id(deal_id: str) -> str | None:
    """Return an error message if the deal_id is invalid, else None."""
    if not deal_id or not deal_id.strip():
        return "deal_id is required"
    deal_id = deal_id.strip()
    if len(deal_id) > 30:
        return f"deal_id is {len(deal_id)} chars; Media.net rejects > 30"
    if _DEAL_ID_RE.search(deal_id):
        return "deal_id contains forbidden chars (one of: # % $ @ * & ? ! ` ~ \" ' , / \\ | ( ) { } [ ] + = ^ :)"
    return None


def _normalize_iso_date(value: Any, field: str) -> str | None:
    """Validate YYYY-MM-DD; return normalized string or raise ValueError."""
    if value is None:
        return None
    text = str(value).strip()
    if not text:
        return None
    if not _DATE_ONLY_RE.match(text):
        raise ValueError(f"{field} must be YYYY-MM-DD; got {value!r}")
    # Validate it parses as a real date.
    try:
        datetime.strptime(text, "%Y-%m-%d")
    except ValueError as exc:
        raise ValueError(f"{field} is not a valid date: {value!r}") from exc
    return text


async def _build_prepared_medianet_deal(
    *,
    deal_id: str,
    display_name: str,
    start_date: str,
    end_date: str | None,
    ad_format: int | None,
    margin: float | None,
    margin_type: int,
    demand_partners: list[Any],
    environments: list[str],
    channel: str | None = None,
    bid_floor: float | None,
    status: int,
    whitelisted_seats: list[dict[str, Any]] | None,
    whitelisted_domains: list[str] | None,
    publisher_domains: dict[str, Any] | None,
    publisher_urls: dict[str, Any] | None,
    domain_group: dict[str, Any] | None,
    url_group: dict[str, Any] | None,
    ip_group: dict[str, Any] | None,
    devices: list[Any] | None,
    operating_systems: list[Any] | None,
    browsers: list[Any] | None,
    device_languages: list[Any] | None,
    geo: list[Any] | None,
    ad_sizes: list[Any] | None,
    video_min: float | None,
    video_max: float | None,
    video_placements: list[Any] | None,
    video_player_sizes: list[Any] | None,
    video_playback_methods: list[Any] | None,
    skippable: bool | None,
    viewability_min: float | None,
    viewability_max: float | None,
    vcr_min: float | None,
    vcr_max: float | None,
    content_categories: list[Any] | None,
    first_party_segments: list[Any] | None,
    experian_custom_segments: list[Any] | None,
    experian_syndicated_segments: list[Any] | None,
    contextual_segments: list[Any] | None,
    app_platform: int | None,
    app_categories: dict[str, Any] | None,
    app_content_ratings: list[Any] | None,
    app_prices: list[Any] | None,
    app_user_ratings: list[Any] | None,
    app_ratings: list[Any] | None,
) -> dict[str, Any]:
    """Resolve all Media.net deal inputs and assemble a prepared artifact.

    Resolution failures become structured blockers rather than raised
    exceptions so the prepared artifact always carries a complete picture
    of what the agent needs to fix before submitting.
    """
    warnings: list[str] = []
    blockers: list[dict[str, Any]] = []
    resolved: dict[str, Any] = {}
    quality_flags: list[dict[str, Any]] = []

    # Channel-aware device defaults: when the caller passes channel and
    # omits devices, expand to the canonical Media.net device set for that
    # channel before name resolution.
    devices, applied_channel_default = _apply_mn_channel_device_defaults(devices, channel)
    if applied_channel_default:
        canonical_channel = _normalize_mn_channel(channel)
        warnings.append(
            f"Applied Media.net default device targeting for {canonical_channel} channel: {devices}. "
            "Pass devices= to override."
        )
        quality_flags.append(
            _make_mn_quality_flag(
                "mn_default_channel_devices_applied",
                f"Auto-filled devices from channel={canonical_channel!r}.",
                channel=canonical_channel,
                devices=list(devices or []),
            )
        )

    ad_format, applied_ad_format_default = _apply_mn_channel_ad_format_default(ad_format, channel)
    if applied_ad_format_default:
        canonical_channel = _normalize_mn_channel(channel)
        warnings.append(
            f"Applied Media.net default ad_format for {canonical_channel} channel: {ad_format}. "
            "Pass ad_format= to override."
        )
        quality_flags.append(
            _make_mn_quality_flag(
                "mn_default_channel_ad_format_applied",
                f"Auto-filled ad_format from channel={canonical_channel!r}.",
                channel=canonical_channel,
                ad_format=ad_format,
            )
        )
    if ad_format is None:
        # No channel hint and no explicit ad_format — preserve the
        # historical Banner fallback so single-deal Banner workflows that
        # never set channel keep working.
        ad_format = MN_AD_FORMAT_BANNER

    # Curator-margin default: Media.net's `margin` + `margin_type=1` (Percentage)
    # is the curator-fee surface. Default to a flat 30% when the caller omits it.
    if margin is None:
        margin = ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT
        margin_type = MN_MARGIN_TYPE_PERCENTAGE
        warnings.append(
            f"Applied default Elcano curator margin: {ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT}% (margin_type=1). "
            "Pass margin= to override."
        )
        quality_flags.append(
            _make_mn_quality_flag(
                "mn_default_curator_margin_applied",
                f"Auto-applied flat {ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT}% Percentage margin for Elcano.",
                margin_percent=ELCANO_DEFAULT_CURATOR_MARGIN_PERCENT,
                margin_type=MN_MARGIN_TYPE_PERCENTAGE,
            )
        )

    # Identifier / dates / enums
    err = _validate_deal_id(deal_id)
    if err:
        blockers.append(_make_blocker("invalid_deal_id", err, deal_id=deal_id))

    if not display_name or len(display_name.strip()) > 30:
        blockers.append(
            _make_blocker(
                "invalid_display_name",
                "display_name is required and must be <= 30 chars",
                display_name=display_name,
            )
        )

    try:
        normalized_start = _normalize_iso_date(start_date, "start_date")
        normalized_end = _normalize_iso_date(end_date, "end_date")
        if normalized_end and normalized_start and normalized_end < normalized_start:
            blockers.append(
                _make_blocker(
                    "invalid_date_order",
                    f"end_date {normalized_end} must be >= start_date {normalized_start}",
                )
            )
    except ValueError as exc:
        blockers.append(_make_blocker("invalid_dates", str(exc)))
        normalized_start = None
        normalized_end = None

    if ad_format not in (0, 1, 2):
        blockers.append(
            _make_blocker(
                "invalid_ad_format",
                "ad_format must be 0 (Banner), 1 (Video), or 2 (Native)",
                ad_format=ad_format,
            )
        )
    if margin_type not in (0, 1):
        blockers.append(
            _make_blocker(
                "invalid_margin_type",
                "margin_type must be 0 (Fixed) or 1 (Percentage)",
                margin_type=margin_type,
            )
        )
    if status not in (1, -1, -2):
        blockers.append(
            _make_blocker(
                "invalid_status",
                "status must be 1 (Active), -1 (Inactive), or -2 (Archived) at create time",
                status=status,
            )
        )
    if margin_type == 0 and margin > 25:
        blockers.append(_make_blocker("invalid_margin", "Fixed margin must be <= $25"))
    if margin_type == 1 and (margin < 0 or margin > 50):
        blockers.append(_make_blocker("invalid_margin", "Percentage margin must be 0-50"))

    # Environments
    normalized_envs: list[str] = []
    for env in environments or []:
        text = str(env).strip().capitalize()
        if text not in ("Web", "App"):
            blockers.append(
                _make_blocker(
                    "invalid_environment",
                    "environments entries must be 'Web' or 'App'",
                    environment=env,
                )
            )
            continue
        normalized_envs.append(text)
    if not normalized_envs:
        blockers.append(_make_blocker("missing_environments", "environments must include 'Web' and/or 'App'"))

    # Demand partners (resolve names -> ids)
    resolved_partners: list[str] = []
    if not demand_partners:
        blockers.append(_make_blocker("missing_demand_partners", "demand_partners is required"))
    else:
        try:
            resolved_partners, dp_warnings = await _resolve_demand_partners(demand_partners, ad_format_id=ad_format)
            warnings.extend(dp_warnings)
            if dp_warnings:
                quality_flags.append(
                    _make_mn_quality_flag(
                        "mn_demand_partner_aliased",
                        "Aliased one or more demand partner names to Media.net catalog ids.",
                        details=dp_warnings,
                    )
                )
        except MediaNetResolutionError as exc:
            blockers.append(_make_blocker("demand_partners_unresolved", exc.message, **exc.details))
    resolved["demand_partners"] = resolved_partners

    # Entity resolutions (each catches its own MediaNetResolutionError)
    async def _try_entity(kind: str, values: list[Any] | None) -> list[Any]:
        if not values:
            return []
        try:
            ids, warns = await _resolve_entity(kind, values)
            warnings.extend(warns)
            return ids
        except MediaNetResolutionError as exc:
            blockers.append(_make_blocker(f"{kind}_unresolved", exc.message, **exc.details))
            return []

    resolved_devices = await _try_entity("devices", devices)
    resolved_operating_systems = await _try_entity("operating_systems", operating_systems)
    resolved_browsers = await _try_entity("browsers", browsers)
    resolved_device_languages = await _try_entity("device_languages", device_languages)
    resolved_ad_sizes = await _try_entity("ad_sizes", ad_sizes)
    resolved_video_placements = await _try_entity("video_placements", video_placements)
    resolved_video_player_sizes = await _try_entity("video_player_sizes", video_player_sizes)
    resolved_video_playback_methods = await _try_entity("video_playback_methods", video_playback_methods)
    resolved_content_categories = await _try_entity("content_categories", content_categories)
    resolved_app_content_ratings = await _try_entity("app_content_ratings", app_content_ratings)
    resolved_app_prices = await _try_entity("app_prices", app_prices)
    resolved_app_user_ratings = await _try_entity("app_user_ratings", app_user_ratings)
    resolved_app_ratings = await _try_entity("app_ratings", app_ratings)

    # Segments per group
    async def _try_segments(group: str, values: list[Any] | None) -> list[str]:
        if not values:
            return []
        try:
            ids, warns = await _resolve_segments(group, values)
            warnings.extend(warns)
            return ids
        except MediaNetResolutionError as exc:
            blockers.append(_make_blocker("segments_unresolved", exc.message, **exc.details))
            return []

    resolved_first_party = await _try_segments("first_party_segments", first_party_segments)
    resolved_experian_custom = await _try_segments("experian_custom_segments", experian_custom_segments)
    resolved_experian_synd = await _try_segments("experian_syndicated_segments", experian_syndicated_segments)
    resolved_contextual = await _try_segments("contextual_segments", contextual_segments)

    # Geo
    resolved_geo: list[dict[str, Any]] = []
    if geo:
        try:
            resolved_geo, geo_warns = await _resolve_geo(geo)
            warnings.extend(geo_warns)
        except MediaNetResolutionError as exc:
            blockers.append(_make_blocker("geo_unresolved", exc.message, **exc.details))

    # App categories — special: object with values + is_excluded
    resolved_app_categories: dict[str, Any] | None = None
    if app_categories:
        cat_values = app_categories.get("values") if isinstance(app_categories, dict) else None
        is_excluded = bool(app_categories.get("is_excluded", False)) if isinstance(app_categories, dict) else False
        if cat_values:
            try:
                ids, app_warns = await _resolve_entity("app_categories", cat_values)
                warnings.extend(app_warns)
                resolved_app_categories = {"values": ids, "is_excluded": is_excluded}
            except MediaNetResolutionError as exc:
                blockers.append(_make_blocker("app_categories_unresolved", exc.message, **exc.details))

    # Group resolutions — name -> internal numeric id
    async def _resolve_group_payload(
        kind: str, fetcher: str, group_obj: dict[str, Any] | None
    ) -> dict[str, Any] | None:
        if not group_obj or not isinstance(group_obj, dict):
            return None
        values = group_obj.get("values") or []
        if not values:
            return None
        is_excluded = bool(group_obj.get("is_excluded", False))
        resolved_ids: list[str] = []
        for v in values:
            try:
                gid, _ = await _resolve_group_id(kind, v, fetcher=fetcher)
                if gid is not None:
                    resolved_ids.append(gid)
            except MediaNetResolutionError as exc:
                blockers.append(_make_blocker(f"{kind}_group_unresolved", exc.message, **exc.details))
        return {"values": resolved_ids, "is_excluded": is_excluded} if resolved_ids else None

    resolved_domain_group = await _resolve_group_payload("domain", "list_domain_groups", domain_group)
    resolved_url_group = await _resolve_group_payload("url", "list_url_groups", url_group)
    resolved_ip_group = await _resolve_group_payload("ip", "list_ip_groups", ip_group)

    # App-environment guard: if environments is App-only, app_platform is required.
    is_app_only = normalized_envs == ["App"]
    if is_app_only and app_platform is None:
        blockers.append(
            _make_blocker(
                "missing_app_platform",
                "app_platform is required when environments is ['App'] only",
            )
        )

    # Assemble deal payload (Media.net's POST /api/v2/deals shape).
    deal_payload: dict[str, Any] = {}
    if not blockers and normalized_start:
        deal_payload = {
            "deal_id": deal_id.strip(),
            "display_name": display_name.strip(),
            "start_date": normalized_start,
            "ad_format": int(ad_format),
            "margin": float(margin),
            "margin_type": int(margin_type),
            "demand_partners": resolved_partners,
            "environments": normalized_envs,
            "status": int(status),
        }
        if normalized_end:
            deal_payload["end_date"] = normalized_end
        if bid_floor is not None:
            deal_payload["bid_floor"] = float(bid_floor)
        if whitelisted_seats:
            deal_payload["whitelisted_seats"] = whitelisted_seats
        if whitelisted_domains:
            deal_payload["whitelisted_domains"] = whitelisted_domains
        if publisher_domains:
            deal_payload["publisher_domains"] = publisher_domains
        if publisher_urls:
            deal_payload["publisher_urls"] = publisher_urls
        if resolved_domain_group:
            deal_payload["domain_group"] = resolved_domain_group
        if resolved_url_group:
            deal_payload["url_group"] = resolved_url_group
        if resolved_ip_group:
            deal_payload["ip_group"] = resolved_ip_group
        if resolved_devices:
            deal_payload["devices"] = resolved_devices
        if resolved_operating_systems:
            deal_payload["operating_systems"] = resolved_operating_systems
        if resolved_browsers:
            deal_payload["browsers"] = resolved_browsers
        if resolved_device_languages:
            deal_payload["device_languages"] = resolved_device_languages
        if resolved_geo:
            deal_payload["geo"] = resolved_geo
        if resolved_ad_sizes:
            deal_payload["ad_sizes"] = resolved_ad_sizes
        if video_min is not None or video_max is not None:
            deal_payload["video"] = {"min": video_min, "max": video_max}
        if resolved_video_placements:
            deal_payload["video_placements"] = resolved_video_placements
        if resolved_video_player_sizes:
            deal_payload["video_player_sizes"] = resolved_video_player_sizes
        if resolved_video_playback_methods:
            deal_payload["video_playback_methods"] = resolved_video_playback_methods
        if skippable is not None:
            deal_payload["skippable"] = bool(skippable)
        if viewability_min is not None or viewability_max is not None:
            try:
                deal_payload["viewability"] = {
                    "min": _coerce_mn_percent(viewability_min),
                    "max": _coerce_mn_percent(viewability_max),
                }
            except ValueError as exc:
                blockers.append(_make_blocker("viewability_invalid", str(exc)))
        if vcr_min is not None or vcr_max is not None:
            deal_payload["vcr"] = {"min": vcr_min, "max": vcr_max}
        if resolved_content_categories:
            deal_payload["content_categories"] = resolved_content_categories
        if resolved_first_party:
            deal_payload["first_party_segments"] = resolved_first_party
        if resolved_experian_custom:
            deal_payload["experian_custom_segments"] = resolved_experian_custom
        if resolved_experian_synd:
            deal_payload["experian_syndicated_segments"] = resolved_experian_synd
        if resolved_contextual:
            deal_payload["contextual_segments"] = resolved_contextual
        if app_platform is not None:
            deal_payload["app_platform"] = int(app_platform)
        if resolved_app_categories:
            deal_payload["app_categories"] = resolved_app_categories
        if resolved_app_content_ratings:
            deal_payload["app_content_ratings"] = resolved_app_content_ratings
        if resolved_app_prices:
            deal_payload["app_prices"] = resolved_app_prices
        if resolved_app_user_ratings:
            deal_payload["app_user_ratings"] = resolved_app_user_ratings
        if resolved_app_ratings:
            deal_payload["app_ratings"] = resolved_app_ratings

    # Capture resolved entities for the agent's report.
    resolved.update(
        {
            "devices": resolved_devices,
            "operating_systems": resolved_operating_systems,
            "browsers": resolved_browsers,
            "device_languages": resolved_device_languages,
            "ad_sizes": resolved_ad_sizes,
            "geo": resolved_geo,
            "content_categories": resolved_content_categories,
            "first_party_segments": resolved_first_party,
            "experian_custom_segments": resolved_experian_custom,
            "experian_syndicated_segments": resolved_experian_synd,
            "contextual_segments": resolved_contextual,
        }
    )

    quality_flags.extend(_blockers_to_mn_quality_flags(blockers))

    prepared_deal_id = f"medianet-prepared-{uuid4()}"
    prepared = {
        "prepared_deal_id": prepared_deal_id,
        "ready_to_create": not blockers,
        "blocking_issues": [b["message"] for b in blockers],
        "blockers": blockers,
        "warnings": warnings,
        "quality_flags": quality_flags,
        "resolved_entities": resolved,
        "deal_intent": deal_payload,
    }
    _prepared_medianet_deals[prepared_deal_id] = prepared
    return prepared


@mcp.tool()
async def mn_prepare_deal_from_prompt_inputs(
    deal_id: str,
    display_name: str,
    start_date: str,
    ad_format: int | None,
    demand_partners: list[Any],
    margin: float | None = None,
    end_date: str | None = None,
    margin_type: int = 1,
    environments: list[str] | None = None,
    bid_floor: float | None = None,
    status: int = 1,
    whitelisted_seats: list[dict[str, Any]] | None = None,
    whitelisted_domains: list[str] | None = None,
    publisher_domains: dict[str, Any] | None = None,
    publisher_urls: dict[str, Any] | None = None,
    domain_group: dict[str, Any] | None = None,
    url_group: dict[str, Any] | None = None,
    ip_group: dict[str, Any] | None = None,
    devices: list[Any] | None = None,
    operating_systems: list[Any] | None = None,
    browsers: list[Any] | None = None,
    device_languages: list[Any] | None = None,
    geo: list[Any] | None = None,
    ad_sizes: list[Any] | None = None,
    video_min: float | None = None,
    video_max: float | None = None,
    video_placements: list[Any] | None = None,
    video_player_sizes: list[Any] | None = None,
    video_playback_methods: list[Any] | None = None,
    skippable: bool | None = None,
    viewability_min: float | None = None,
    viewability_max: float | None = None,
    vcr_min: float | None = None,
    vcr_max: float | None = None,
    content_categories: list[Any] | None = None,
    first_party_segments: list[Any] | None = None,
    experian_custom_segments: list[Any] | None = None,
    experian_syndicated_segments: list[Any] | None = None,
    contextual_segments: list[Any] | None = None,
    app_platform: int | None = None,
    app_categories: dict[str, Any] | None = None,
    app_content_ratings: list[Any] | None = None,
    app_prices: list[Any] | None = None,
    app_user_ratings: list[Any] | None = None,
    app_ratings: list[Any] | None = None,
    channel: str | None = None,
) -> dict[str, Any]:
    """Resolve human-readable Media.net deal inputs into a server-side draft.

    All name -> ID resolution (devices, browsers, OSes, content categories,
    segments, geo, demand partners, domain/url/ip groups) happens server-side
    via the documented Media.net catalog endpoints. The returned
    prepared_deal_id is submitted via mn_create_prepared_deal to actually
    create the deal. Inspect ready_to_create and blocking_issues before
    submitting.

    Verified-working defaults applied when omitted:
      - margin_type=1 (Percentage)
      - margin=30.0 (flat 30% Elcano curator margin) — emits
        `mn_default_curator_margin_applied`
      - status=1 (Active)
      - environments=["Web"]

    channel accepts "display", "olv", "ctv", or "ott". When devices is omitted,
    the canonical Media.net device set for that channel is auto-applied
    (display/olv → PC+Phone+Tablet, ctv → Connected TV, ott → Phone+Tablet)
    and `mn_default_channel_devices_applied` is emitted. When ad_format is
    omitted, it is derived from channel as well: display → 0 (Banner);
    olv/ctv/ott → 1 (Video); `mn_default_channel_ad_format_applied` is
    emitted.

    Polymorphic inputs: most fields accept either names ("Mobile/Tablet",
    "Chrome", "IAB1-3") or numeric/string IDs. The MCP resolves names against
    the live Media.net catalog and surfaces unresolved values as structured
    blockers with a sample of available names.
    """
    logger.info("mn_prepare_deal_from_prompt_inputs called: %s", display_name)
    try:
        prepared = await _build_prepared_medianet_deal(
            deal_id=deal_id,
            display_name=display_name,
            start_date=start_date,
            end_date=end_date,
            ad_format=ad_format,
            margin=margin,
            margin_type=margin_type,
            demand_partners=demand_partners,
            environments=environments or ["Web"],
            bid_floor=bid_floor,
            status=status,
            whitelisted_seats=whitelisted_seats,
            whitelisted_domains=whitelisted_domains,
            publisher_domains=publisher_domains,
            publisher_urls=publisher_urls,
            domain_group=domain_group,
            url_group=url_group,
            ip_group=ip_group,
            devices=devices,
            operating_systems=operating_systems,
            browsers=browsers,
            device_languages=device_languages,
            geo=geo,
            ad_sizes=ad_sizes,
            video_min=video_min,
            video_max=video_max,
            video_placements=video_placements,
            video_player_sizes=video_player_sizes,
            video_playback_methods=video_playback_methods,
            skippable=skippable,
            viewability_min=viewability_min,
            viewability_max=viewability_max,
            vcr_min=vcr_min,
            vcr_max=vcr_max,
            content_categories=content_categories,
            first_party_segments=first_party_segments,
            experian_custom_segments=experian_custom_segments,
            experian_syndicated_segments=experian_syndicated_segments,
            contextual_segments=contextual_segments,
            app_platform=app_platform,
            app_categories=app_categories,
            app_content_ratings=app_content_ratings,
            app_prices=app_prices,
            app_user_ratings=app_user_ratings,
            app_ratings=app_ratings,
            channel=channel,
        )
        return {
            "success": True,
            "prepared_deal_id": prepared["prepared_deal_id"],
            "ready_to_create": prepared["ready_to_create"],
            "blocking_issues": prepared["blocking_issues"],
            "blockers": prepared["blockers"],
            "warnings": prepared["warnings"],
            "quality_flags": prepared.get("quality_flags", []),
            "resolved_entities": prepared["resolved_entities"],
            "deal_intent_preview": prepared["deal_intent"],
        }
    except Exception as exc:
        logger.error("mn_prepare_deal_from_prompt_inputs failed: %s", exc)
        return {
            "success": False,
            "ready_to_create": False,
            "blocking_issues": [str(exc)],
            "blockers": [_make_blocker("preparation_error", str(exc))],
            "warnings": [],
            "quality_flags": [
                _make_mn_quality_flag(
                    "mn_preparation_error",
                    str(exc),
                )
            ],
            "error": str(exc),
        }


@mcp.tool()
async def mn_create_prepared_deal(prepared_deal_id: str) -> dict[str, Any]:
    """Submit a previously prepared Media.net deal artifact and verify it.

    Refuses to submit if the prepared artifact has unresolved blocking_issues.
    On success, calls mn_get_deal to verify the created record.
    """
    logger.info("mn_create_prepared_deal called: %s", prepared_deal_id)
    prepared = _prepared_medianet_deals.get(prepared_deal_id)
    if prepared is None:
        return {
            "success": False,
            "deal": None,
            "warnings": [],
            "quality_flags": [
                _make_mn_quality_flag(
                    "mn_prepared_deal_not_found",
                    f"Prepared Media.net deal not found: {prepared_deal_id}",
                    prepared_deal_id=prepared_deal_id,
                )
            ],
            "error": f"Prepared Media.net deal not found: {prepared_deal_id}",
            "verification": None,
        }
    if not prepared["ready_to_create"]:
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": prepared["warnings"],
            "quality_flags": list(prepared.get("quality_flags", [])),
            "error": "Prepared Media.net deal is blocked and cannot be created.",
            "blocking_issues": prepared["blocking_issues"],
            "blockers": prepared["blockers"],
            "verification": None,
        }
    if prepared.get("created"):
        # The create POST for this artifact already succeeded — re-submitting
        # would create a duplicate live deal on Media.net. Return the
        # recorded outcome instead.
        recorded = prepared.get("created_result")
        if isinstance(recorded, dict):
            replay = dict(recorded)
            replay["replayed"] = True
            return replay
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": list(prepared.get("warnings", [])),
            "quality_flags": [
                _make_mn_quality_flag(
                    "mn_deal_already_created",
                    "This prepared deal was already submitted and the deal exists on Media.net. "
                    "Do NOT prepare/submit it again; use mn_list_deals to locate it.",
                    prepared_deal_id=prepared_deal_id,
                )
            ],
            "error": "Prepared deal already submitted; refusing to create a duplicate.",
            "verification": None,
        }
    warnings = list(prepared["warnings"])
    quality_flags: list[dict[str, Any]] = list(prepared.get("quality_flags", []))
    deal_response: dict[str, Any] | None = None
    verification: dict[str, Any] | None = None
    try:
        client = get_medianet_client()
        deal_response = await client.create_deal(prepared["deal_intent"])
        # The deal now exists on Media.net — mark the artifact consumed so a
        # retry cannot double-create.
        prepared["created"] = True
        deal_id = prepared["deal_intent"].get("deal_id")
        if deal_id:
            try:
                verification = await mn_get_deal(deal_id)
                if isinstance(verification, dict) and not verification.get("success"):
                    quality_flags.append(
                        _make_mn_quality_flag(
                            "mn_verification_failed",
                            verification.get("error") or "Media.net verification re-fetch failed.",
                            deal_id=deal_id,
                        )
                    )
            except Exception as ver_exc:
                verification = {"success": False, "error": f"Verification call failed: {ver_exc}"}
                quality_flags.append(
                    _make_mn_quality_flag(
                        "mn_verification_failed",
                        f"Verification call failed: {ver_exc}",
                        deal_id=deal_id,
                    )
                )
        result = {
            "success": True,
            "prepared_deal_id": prepared_deal_id,
            "deal": deal_response,
            "deal_url": _build_medianet_deal_url(),
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": None,
            "verification": verification,
        }
        prepared["created_result"] = result
        return result
    except httpx.HTTPStatusError as exc:
        error_message = f"Failed to create Media.net deal: HTTP {exc.response.status_code}: {exc.response.text[:300]}"
        quality_flags.append(_make_mn_quality_flag("mn_create_call_failed", error_message))
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": error_message,
            "verification": None,
        }
    except Exception as exc:
        error_message = f"Failed to create Media.net deal: {exc}"
        quality_flags.append(_make_mn_quality_flag("mn_create_call_failed", error_message))
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": error_message,
            "verification": None,
        }


@mcp.tool()
async def mn_execute_deal_from_prompt_inputs(
    deal_id: str,
    display_name: str,
    start_date: str,
    ad_format: int | None,
    demand_partners: list[Any],
    margin: float | None = None,
    end_date: str | None = None,
    margin_type: int = 1,
    environments: list[str] | None = None,
    bid_floor: float | None = None,
    status: int = 1,
    whitelisted_seats: list[dict[str, Any]] | None = None,
    whitelisted_domains: list[str] | None = None,
    publisher_domains: dict[str, Any] | None = None,
    publisher_urls: dict[str, Any] | None = None,
    domain_group: dict[str, Any] | None = None,
    url_group: dict[str, Any] | None = None,
    ip_group: dict[str, Any] | None = None,
    devices: list[Any] | None = None,
    operating_systems: list[Any] | None = None,
    browsers: list[Any] | None = None,
    device_languages: list[Any] | None = None,
    geo: list[Any] | None = None,
    ad_sizes: list[Any] | None = None,
    video_min: float | None = None,
    video_max: float | None = None,
    video_placements: list[Any] | None = None,
    video_player_sizes: list[Any] | None = None,
    video_playback_methods: list[Any] | None = None,
    skippable: bool | None = None,
    viewability_min: float | None = None,
    viewability_max: float | None = None,
    vcr_min: float | None = None,
    vcr_max: float | None = None,
    content_categories: list[Any] | None = None,
    first_party_segments: list[Any] | None = None,
    experian_custom_segments: list[Any] | None = None,
    experian_syndicated_segments: list[Any] | None = None,
    contextual_segments: list[Any] | None = None,
    app_platform: int | None = None,
    app_categories: dict[str, Any] | None = None,
    app_content_ratings: list[Any] | None = None,
    app_prices: list[Any] | None = None,
    app_user_ratings: list[Any] | None = None,
    app_ratings: list[Any] | None = None,
    channel: str | None = None,
) -> dict[str, Any]:
    """Prepare, submit, and verify a Media.net deal in one call.

    Thin wrapper over mn_prepare_deal_from_prompt_inputs and
    mn_create_prepared_deal. Use the two-step pair when you want to
    inspect the resolved artifact before committing.

    margin defaults to 30.0 (Percentage curator margin) when omitted.
    channel accepts "display"/"olv"/"ctv"/"ott" and auto-fills both devices
    and ad_format when those are omitted (see mn_prepare_deal_from_prompt_inputs
    for the per-channel canonical values).
    """
    logger.info("mn_execute_deal_from_prompt_inputs called: %s", display_name)
    preparation = await mn_prepare_deal_from_prompt_inputs(
        deal_id=deal_id,
        display_name=display_name,
        start_date=start_date,
        end_date=end_date,
        ad_format=ad_format,
        margin=margin,
        margin_type=margin_type,
        demand_partners=demand_partners,
        environments=environments,
        bid_floor=bid_floor,
        status=status,
        whitelisted_seats=whitelisted_seats,
        whitelisted_domains=whitelisted_domains,
        publisher_domains=publisher_domains,
        publisher_urls=publisher_urls,
        domain_group=domain_group,
        url_group=url_group,
        ip_group=ip_group,
        devices=devices,
        operating_systems=operating_systems,
        browsers=browsers,
        device_languages=device_languages,
        geo=geo,
        ad_sizes=ad_sizes,
        video_min=video_min,
        video_max=video_max,
        video_placements=video_placements,
        video_player_sizes=video_player_sizes,
        video_playback_methods=video_playback_methods,
        skippable=skippable,
        viewability_min=viewability_min,
        viewability_max=viewability_max,
        vcr_min=vcr_min,
        vcr_max=vcr_max,
        content_categories=content_categories,
        first_party_segments=first_party_segments,
        experian_custom_segments=experian_custom_segments,
        experian_syndicated_segments=experian_syndicated_segments,
        contextual_segments=contextual_segments,
        app_platform=app_platform,
        app_categories=app_categories,
        app_content_ratings=app_content_ratings,
        app_prices=app_prices,
        app_user_ratings=app_user_ratings,
        app_ratings=app_ratings,
        channel=channel,
    )
    if not preparation.get("ready_to_create"):
        blocking_issues = preparation.get("blocking_issues") or []
        error_message = (
            blocking_issues[0]
            if blocking_issues
            else (preparation.get("error") or "Preparation did not produce a creatable artifact.")
        )
        return {
            "success": False,
            "phase": "prepare",
            "deal": None,
            "warnings": preparation.get("warnings", []),
            "quality_flags": list(preparation.get("quality_flags", [])),
            "error": error_message,
            "verification": None,
            "preparation": preparation,
        }
    creation = await mn_create_prepared_deal(preparation["prepared_deal_id"])
    combined_warnings: list[str] = []
    seen_w: set[str] = set()
    for w in list(preparation.get("warnings", [])) + list(creation.get("warnings", [])):
        if w not in seen_w:
            combined_warnings.append(w)
            seen_w.add(w)
    # mn_create_prepared_deal already seeds its quality_flags from the
    # prepared artifact (see line ~2124), so concatenating preparation's
    # flags here would double them (e.g. mn_default_curator_margin_applied
    # appearing twice). Take creation's list as the canonical merged result.
    combined_quality_flags = list(creation.get("quality_flags", []))
    return {
        "success": creation.get("success", False),
        "phase": "verify" if (creation.get("verification") or {}).get("success") else "create",
        "deal": creation.get("deal"),
        # Media.net Select has no deal-detail URL; this points to the
        # deals-list page so the trader can locate the row by deal_id /
        # display_name. See _build_medianet_deal_url docstring.
        "deal_url": creation.get("deal_url"),
        "warnings": combined_warnings,
        "quality_flags": combined_quality_flags,
        "error": creation.get("error"),
        "verification": creation.get("verification"),
        "preparation": preparation,
        "creation": creation,
    }


@mcp.tool()
async def mn_reporting_healthcheck() -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        views = await client.list_views()
        return {
            "success": True,
            "view_count": len(views),
            "sample_views": views[:5],
        }
    except Exception as e:
        return {"success": False, "error": f"Failed Media.net reporting healthcheck: {e}"}


@mcp.tool()
async def mn_list_report_views() -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        views = await client.list_views()
        return {"success": True, "views": views}
    except Exception as e:
        return {"success": False, "error": f"Failed to list Media.net report views: {e}"}


@mcp.tool()
async def mn_get_report_view_info(view_id: int) -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        info = await client.get_view_info(view_id)
        return {"success": True, "view": info}
    except Exception as e:
        return {"success": False, "error": f"Failed to get Media.net report view info: {e}"}


@mcp.tool()
async def mn_get_report_dimensions(view_id: int) -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        dimensions = await client.get_dimensions(view_id)
        return {"success": True, "dimensions": dimensions}
    except Exception as e:
        return {"success": False, "error": f"Failed to get Media.net dimensions: {e}"}


@mcp.tool()
async def mn_get_report_metrics(view_id: int) -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        metrics = await client.get_metrics(view_id)
        return {"success": True, "metrics": metrics}
    except Exception as e:
        return {"success": False, "error": f"Failed to get Media.net metrics: {e}"}


@mcp.tool()
async def mn_get_report_relations(view_id: int, dimensions: list[str]) -> dict[str, Any]:
    try:
        client = get_medianet_reporting_client()
        relations = await client.get_relations(view_id, dimensions)
        return {"success": True, "relations": relations}
    except Exception as e:
        return {"success": False, "error": f"Failed to get Media.net relations: {e}"}


_MEDIANET_DATETIME_FORMAT = "%Y-%m-%dT%H:%M"
_MEDIANET_DATETIME_INPUT_FORMATS = (
    "%Y-%m-%dT%H:%M",
    "%Y-%m-%dT%H:%M:%S",
    "%Y-%m-%d %H:%M",
    "%Y-%m-%d %H:%M:%S",
    "%Y-%m-%d",
)


def _normalize_medianet_datetime(value: str, field_name: str) -> str:
    """Coerce common datetime inputs to Media.net's required ``yyyy-MM-dd'T'HH:mm``.

    The reporting API rejects any other shape (e.g. ``2026-05-26`` or
    ``2026-05-26 00:00:00``) with HTTP 422
    (``Invalid format for startDateTime``). Agents naturally produce those
    looser forms, so we normalize here rather than make every caller remember
    the exact pattern.
    """
    cleaned = str(value).strip().replace("Z", "")
    for fmt in _MEDIANET_DATETIME_INPUT_FORMATS:
        try:
            return datetime.strptime(cleaned, fmt).strftime(_MEDIANET_DATETIME_FORMAT)
        except ValueError:
            continue
    raise ValueError(f"{field_name} must be a date or datetime like '2026-05-26' or '2026-05-26T00:00' (got {value!r})")


@mcp.tool()
async def mn_fetch_report_data(
    view_id: int,
    start_date_time: str,
    end_date_time: str,
    threshold: int,
    dimensions: list[str],
    metrics: list[str],
    sort_by_metric: dict[str, Any],
    filters: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Single-shot synchronous fetch of Media.net report rows.

    For most "pull a Media.net report for me" requests, prefer
    mn_run_report_from_prompt_inputs (which handles dimension/metric
    aliases AND uses the queue path with internal polling) or
    mn_queue_report_data (the queue path directly). fetchData is the
    sync endpoint — it returns whatever it has in one call. If the
    rows you want are larger than what the sync path returns OR the
    response is empty, that's a signal to switch to the queue path,
    not to retry fetchData.

    DO NOT loop on this tool. Calling fetchData repeatedly with the
    same arguments is a no-op (no pagination cursor), the data is
    static, and each call burns LLM tokens. If you need more rows,
    call mn_queue_report_data; if you need to wait for slow data,
    that tool already polls internally.
    """
    try:
        client = get_medianet_reporting_client()
        payload = {
            "viewId": view_id,
            "startDateTime": _normalize_medianet_datetime(start_date_time, "start_date_time"),
            "endDateTime": _normalize_medianet_datetime(end_date_time, "end_date_time"),
            "threshold": threshold,
            "dimensions": dimensions,
            "metrics": metrics,
            "sortByMetric": sort_by_metric,
        }
        if filters:
            payload["filters"] = filters
        data = await client.fetch_data(payload)
        return {"success": True, **data}
    except httpx.HTTPStatusError as e:
        details = None
        try:
            details = e.response.text
        except Exception:  # noqa: BLE001 - body capture is best-effort
            details = None
        return {
            "success": False,
            "error": f"Failed to fetch Media.net report data: HTTP {e.response.status_code}: {details}",
        }
    except Exception as e:
        return {"success": False, "error": f"Failed to fetch Media.net report data: {e}"}


@mcp.tool()
async def mn_queue_report_data(
    view_id: int,
    start_date_time: str,
    end_date_time: str,
    dimensions: list[str],
    metrics: list[str],
    sort_by_metric: dict[str, Any],
    filters: list[dict[str, Any]] | None = None,
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = DEFAULT_REPORT_POLL_TIMEOUT_SECONDS,
    poll_interval_seconds: float = DEFAULT_REPORT_POLL_INTERVAL_SECONDS,
) -> dict[str, Any]:
    """Queue and download a Media.net report.

    output_dir: Optional absolute path the CSV is written into. Pass the
        per-conversation workspace dir so other tools can read it.
    """
    try:
        client = get_medianet_reporting_client()
        payload = {
            "viewId": view_id,
            "startDateTime": _normalize_medianet_datetime(start_date_time, "start_date_time"),
            "endDateTime": _normalize_medianet_datetime(end_date_time, "end_date_time"),
            "dimensions": dimensions,
            "metrics": metrics,
            "sortByMetric": sort_by_metric,
        }
        if filters:
            payload["filters"] = filters

        submitted = await client.submit_queue_data(payload)
        queue_id = submitted.get("queueId")
        if not queue_id:
            return {"success": False, "error": "Media.net queue submit succeeded but no queueId was returned."}

        deadline = time.time() + max(poll_timeout_seconds, 0)
        sleep_seconds = max(poll_interval_seconds, 0.1)
        last_progress: dict[str, Any] | None = None

        while True:
            progress = await client.get_queue_progress(queue_id)
            last_progress = progress
            queue_status = str(progress.get("queueStatus", ""))
            if queue_status == "SUCCEEDED":
                content, content_type = await client.download_queue_data(queue_id)
                download = _write_medianet_report_file(
                    queue_id=queue_id,
                    content=content,
                    filename_hint=filename_hint,
                    output_dir=output_dir,
                )
                download["content_type"] = content_type
                return {
                    "success": True,
                    "queueId": queue_id,
                    "queueStatus": queue_status,
                    "headers": submitted.get("headers"),
                    "download": download,
                }
            if queue_status in {
                "SUCCEEDED_NO_DATA_FOUND",
                "SUCCEEDED_DOWNLOAD_LIMIT_EXCEEDED",
                "FAILED",
                "EXPIRED",
                "CANCELLED",
            }:
                return {
                    "success": False,
                    "queueId": queue_id,
                    "queueStatus": queue_status,
                    "headers": submitted.get("headers"),
                    "progress": progress,
                    "error": f"Media.net queue finished with status {queue_status}.",
                }
            if time.time() >= deadline:
                return {
                    "success": False,
                    "queueId": queue_id,
                    "progress": last_progress,
                    "error": "Media.net queue polling timed out before completion.",
                }
            # await: time.sleep would block the FastMCP event loop, freezing
            # every other in-flight tool call on this server for the poll window.
            await asyncio.sleep(sleep_seconds)
    except httpx.HTTPStatusError as e:
        details = None
        try:
            details = e.response.text
        except Exception:  # noqa: BLE001 - body capture is best-effort
            details = None
        return {
            "success": False,
            "error": f"Failed to queue Media.net report data: HTTP {e.response.status_code}: {details}",
        }
    except Exception as e:
        return {"success": False, "error": f"Failed to queue Media.net report data: {e}"}


@mcp.tool()
async def mn_run_report_from_prompt_inputs(
    view_id: int = 1045,
    start_date_time: str | None = None,
    end_date_time: str | None = None,
    breakdowns: list[str] | None = None,
    metrics: list[str] | None = None,
    filters: list[dict[str, Any]] | None = None,
    threshold: int = 100,
    sort_by_metric: dict[str, Any] | None = None,
    queue: bool = True,
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = DEFAULT_REPORT_POLL_TIMEOUT_SECONDS,
    poll_interval_seconds: float = DEFAULT_REPORT_POLL_INTERVAL_SECONDS,
) -> dict[str, Any]:
    """Run a Media.net report from human-readable prompt inputs.

    Example inputs:
    - breakdowns: ["day", "deal", "DSP"]
    - metrics: ["impressions", "spend", "bid rate"]

    output_dir: Optional absolute path for the queued CSV. Pass the
        per-conversation workspace dir so other tools can read it.
        (Only used when queue=True; the sync fetch returns rows inline.)
    """
    resolved_dimensions = _resolve_medianet_fields(breakdowns, MEDIANET_REPORT_DIMENSION_ALIASES)
    resolved_metrics = _resolve_medianet_fields(metrics, MEDIANET_REPORT_METRIC_ALIASES)

    if not resolved_dimensions:
        resolved_dimensions = ["day", "deal_name", "dsp"]
    if not resolved_metrics:
        resolved_metrics = ["ad_impressions", "advertiser_spend", "deal_margin"]

    # Default to a relative trailing-30-day window. A fixed literal window
    # here silently goes stale: an agent omitting dates would report old (or
    # empty) data marked success=True.
    now_utc = datetime.now(UTC)
    request_start = start_date_time or (now_utc - timedelta(days=30)).strftime("%Y-%m-%dT00:00")
    request_end = end_date_time or now_utc.strftime("%Y-%m-%dT%H:%M")
    metric_sort = sort_by_metric or {"by": resolved_metrics[0], "order": "DESC"}

    if queue:
        result = await mn_queue_report_data(
            view_id=view_id,
            start_date_time=request_start,
            end_date_time=request_end,
            dimensions=resolved_dimensions,
            metrics=resolved_metrics,
            filters=filters,
            sort_by_metric=metric_sort,
            filename_hint=filename_hint,
            output_dir=output_dir,
            poll_timeout_seconds=poll_timeout_seconds,
            poll_interval_seconds=poll_interval_seconds,
        )
    else:
        result = await mn_fetch_report_data(
            view_id=view_id,
            start_date_time=request_start,
            end_date_time=request_end,
            threshold=threshold,
            dimensions=resolved_dimensions,
            metrics=resolved_metrics,
            filters=filters,
            sort_by_metric=metric_sort,
        )

    if result.get("success"):
        result["resolved_breakdowns"] = resolved_dimensions
        result["resolved_metrics"] = resolved_metrics
        result["view_id"] = view_id
    return result


# =============================================================================
# Main Entry Point
# =============================================================================

if __name__ == "__main__":
    logger.info("Starting Media.net MCP Server")

    # Check for Media.net credentials
    has_token = bool(os.environ.get("MEDIANET_SELECT_TOKEN"))
    has_credentials = bool(os.environ.get("MEDIANET_SELECT_EMAIL")) and bool(os.environ.get("MEDIANET_SELECT_PASSWORD"))

    if not has_token and not has_credentials:
        logger.warning(
            "Media.net not configured. Set MEDIANET_SELECT_TOKEN or both "
            "MEDIANET_SELECT_EMAIL and MEDIANET_SELECT_PASSWORD to enable deal creation."
        )

    try:
        # Use stdio transport (default for FastMCP)
        mcp.run(transport="stdio")
    except Exception as e:
        logger.error("Failed to start server: %s", e)
        sys.exit(1)
