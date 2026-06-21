#!/usr/bin/env python3
"""
Xandr MCP Server

A Model Context Protocol (MCP) server for programmatic deal management on the Xandr platform
(AppNexus). This is a dedicated MCP for the Xandr Deal Service REST API.

Runs within the Victoria Terminal container environment.
"""

import asyncio
import copy
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

import httpx
from mcp.server.fastmcp import FastMCP

_NON_ALNUM_RE = re.compile(r"[^a-z0-9]+")
_SPACE_RE = re.compile(r"\s+")


# Configure logging to stderr (not stdout for STDIO transport)
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger(__name__)

# Initialize FastMCP server
mcp = FastMCP("xandr_mcp")

# Constants
USER_AGENT = "victoria-terminal/1.0"
DEFAULT_TIMEOUT = 60.0
DEFAULT_REPORT_POLL_INTERVAL_SECONDS = 2.0
DEFAULT_REPORT_POLL_TIMEOUT_SECONDS = 180.0
DEFAULT_REPORT_DOWNLOAD_DIR = os.path.expanduser("~/Victoria/xandr_reports")

# Xandr API Configuration
DEFAULT_BASE_URL = "https://api.appnexus.com"

XANDR_CURATOR_REPORT_PRESETS: dict[str, dict[str, Any]] = {
    "curator_revenue_summary": {
        "description": "Curator revenue flow by day, curated deal, and buyer.",
        "report_type": "curator_analytics",
        "columns": ["day", "curated_deal", "buyer_member_name", "imps", "curator_revenue", "curator_margin"],
        "report_interval": "last_month",
    },
    "curator_supply_flow": {
        "description": "Curator supply flow by day, curated deal, seller, and publisher.",
        "report_type": "curator_analytics",
        "columns": [
            "day",
            "curated_deal",
            "seller_member_name",
            "publisher_name",
            "imps",
            "curator_total_cost",
            "curator_net_media_cost",
        ],
        "report_interval": "last_month",
    },
}

XANDR_REPORT_DIMENSION_ALIASES: dict[str, str] = {
    "day": "day",
    "date": "day",
    "daily": "day",
    "hour": "hour",
    "buyer": "buyer_member_name",
    "buyer member": "buyer_member_name",
    "curated deal": "curated_deal",
    "deal": "curated_deal",
    "publisher": "publisher_name",
    "seller": "seller_member_name",
    "brand": "brand_name",
    "country": "geo_country",
    "device": "device_type",
}

XANDR_REPORT_METRIC_ALIASES: dict[str, str] = {
    "impressions": "imps",
    "spend": "curator_revenue",
    "revenue": "curator_revenue",
    "margin": "curator_margin",
    "fees": "curator_tech_fees",
    "tech fees": "curator_tech_fees",
    "total marketplace fees": "curator_tech_fees",
    "net media cost": "curator_net_media_cost",
    "total cost": "curator_total_cost",
    "clicks": "clicks",
    "ctr": "ctr",
    "buyer cpm": "buyer_cpm",
}

XANDR_REPORT_TYPE_PRESET_ALIASES: dict[str, str] = {
    "curator revenue": "curator_revenue_summary",
    "curator revenue summary": "curator_revenue_summary",
    "supply flow": "curator_supply_flow",
    "curator supply flow": "curator_supply_flow",
}


class XandrClient:
    """
    Client for interacting with the Xandr (AppNexus) Deal Service REST API.

    Uses username/password authentication. A token is obtained via POST /auth
    and included as an Authorization header on subsequent requests.
    """

    def __init__(self):
        self.base_url = os.environ.get("XANDR_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.username = os.environ.get("XANDR_USERNAME", "")
        self.password = os.environ.get("XANDR_PASSWORD", "")
        self.seat_id = os.environ.get("XANDR_SEAT_ID", "")
        self._token = ""
        self._http_client: httpx.AsyncClient | None = None

    def _is_configured(self) -> bool:
        """Check if Xandr credentials are configured."""
        return bool(self.username) and bool(self.password)

    async def _get_http_client(self) -> httpx.AsyncClient:
        """Get or create the HTTP client."""
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _login(self) -> None:
        """
        Authenticate with Xandr and obtain an API token.

        POST {BASE_URL}/auth
        JSON: { "auth": { "username": "...", "password": "..." } }
        """
        if not self.username or not self.password:
            raise ValueError("Username and password required for login.")

        logger.info("Logging in to Xandr API")

        client = await self._get_http_client()
        login_url = f"{self.base_url}/auth"

        try:
            response = await client.post(
                login_url,
                json={"auth": {"username": self.username, "password": self.password}},
                headers={
                    "Content-Type": "application/json",
                    "User-Agent": USER_AGENT,
                },
            )
            response.raise_for_status()
            result = response.json()

            # Extract token from response - expected structure:
            # { "response": { "token": "..." } }
            token = result.get("response", {}).get("token")
            if not token:
                raise ValueError("Login successful but no token in response")

            self._token = token
            logger.info("Successfully obtained Xandr token")

        except httpx.HTTPStatusError as e:
            logger.error("Login failed with HTTP status %d", e.response.status_code)
            raise ValueError(f"Xandr login failed: HTTP {e.response.status_code}: {e.response.text}") from e
        except Exception as e:
            if isinstance(e, ValueError):
                raise
            logger.error("Login failed: %s", type(e).__name__)
            raise ValueError(f"Xandr login failed: {type(e).__name__}") from e

    async def _ensure_token(self) -> str:
        """Ensure we have a valid token, logging in if necessary."""
        if not self._is_configured():
            raise ValueError("Xandr not configured. Set XANDR_USERNAME and XANDR_PASSWORD environment variables.")

        if not self._token:
            await self._login()

        return self._token

    def _get_headers(self) -> dict[str, str]:
        """Get the request headers including authentication token."""
        return {
            "Authorization": self._token,
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
        Execute an HTTP request against the Xandr API.

        Handles token refresh on 401 responses. Xandr does not have a refresh
        token flow; on 401, we clear the token and re-login.
        """
        await self._ensure_token()

        client = await self._get_http_client()
        url = f"{self.base_url}{endpoint}"
        headers = self._get_headers()

        # Log without sensitive data
        logger.info("Executing %s request to Xandr: %s", method, endpoint)

        try:
            if method.upper() == "GET":
                response = await client.get(url, headers=headers, params=params)
            elif method.upper() == "POST":
                response = await client.post(url, headers=headers, json=json_data, params=params)
            elif method.upper() == "PUT":
                response = await client.put(url, headers=headers, json=json_data, params=params)
            else:
                # DELETE is intentionally unsupported — the Deal Service's
                # DELETE is permanent and unrecoverable ("cannot be reverted"
                # per the docs), and irreversible actions are kept off the
                # agent surface (same policy as OpenX dealArchive / TripleLift
                # deal deletion, 2026-06-11). Pause via active=false instead.
                raise ValueError(f"Unsupported HTTP method: {method}")

            # Check for 401 and retry with fresh token
            if response.status_code == 401 and retry_on_401:
                logger.warning("Received 401, attempting token refresh")
                self._token = ""  # Clear token to force re-login
                return await self._request(method, endpoint, json_data, params, retry_on_401=False)

            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error("HTTP error %d on %s %s", e.response.status_code, method, endpoint)
            raise

    async def verify_token(self) -> bool:
        """
        Verify that the current token is active by making a low-cost API call.

        Makes a GET request to /deal/meta to check token validity.

        Returns:
            True if the token is valid, False otherwise.

        Raises:
            Exception: Re-raises non-HTTP exceptions.
        """
        try:
            await self._request("GET", "/deal/meta")
            return True
        except httpx.HTTPStatusError:
            return False

    async def create_deal(self, payload: dict[str, Any]) -> dict[str, Any]:
        """
        Create a new deal using the Xandr Deal Service API.

        POST {BASE_URL}/deal
        JSON: { "deal": { ... } }
        """
        logger.info("Creating deal: %s", payload.get("deal", {}).get("name", "unnamed"))
        result = await self._request("POST", "/deal", json_data=payload)
        return result.get("response", result)

    async def update_deal(self, deal_id: int, payload: dict[str, Any]) -> dict[str, Any]:
        """
        Modify an existing deal (partial update — only sent fields change).

        PUT {BASE_URL}/deal?id={deal_id}
        JSON: { "deal": { ... } }
        """
        logger.info("Updating deal id=%d: %s", deal_id, sorted(payload.get("deal", {}).keys()))
        result = await self._request("PUT", "/deal", json_data=payload, params={"id": deal_id})
        return result.get("response", result)

    async def list_deals(self, member_id: int) -> list[dict[str, Any]]:
        """
        List existing deals for a given member.

        GET {BASE_URL}/deal?member_id={member_id}
        """
        logger.info("Listing deals for member_id=%d", member_id)
        result = await self._request("GET", "/deal", params={"member_id": member_id})
        return result.get("response", {}).get("deals", [])

    async def get_deal(self, deal_id: int) -> dict[str, Any] | None:
        """
        Fetch a single deal by its ID.

        GET {BASE_URL}/deal?id={deal_id}
        """
        logger.info("Getting deal id=%d", deal_id)
        result = await self._request("GET", "/deal", params={"id": deal_id})
        deal = result.get("response", {}).get("deal")
        return deal

    async def list_buyers(
        self,
        *,
        name_like: str | None = None,
        member_id: int | None = None,
        page_size: int = 100,
    ) -> list[dict[str, Any]]:
        """
        List buyer members. Used by the buyer-name resolver.

        GET {BASE_URL}/buyer
        Optional params: name (substring), member_id, num_elements, start_element.

        The exact param shape is not yet confirmed against the live API; the
        resolver always passes resolved IDs through unchanged (escape hatch),
        so a wrong query shape only affects fuzzy name resolution.
        """
        params: dict[str, Any] = {"num_elements": page_size}
        if name_like:
            params["name"] = name_like
        if member_id is not None:
            params["member_id"] = member_id
        logger.info("Listing Xandr buyers (name_like=%s, member_id=%s)", name_like, member_id)
        result = await self._request("GET", "/buyer", params=params)
        return result.get("response", {}).get("buyers", [])

    async def list_profiles(
        self,
        *,
        member_id: int | None = None,
        name_like: str | None = None,
        page_size: int = 100,
    ) -> list[dict[str, Any]]:
        """List profile objects. Used by deal-targeting attach flow.

        GET {BASE_URL}/profile
        """
        params: dict[str, Any] = {"num_elements": page_size}
        if name_like:
            params["search"] = name_like
        if member_id is not None:
            params["member_id"] = member_id
        result = await self._request("GET", "/profile", params=params)
        return result.get("response", {}).get("profiles", [])

    async def create_profile(self, payload: dict[str, Any]) -> dict[str, Any]:
        """Create a profile.

        POST {BASE_URL}/profile
        JSON: { "profile": { ... } }
        """
        logger.info("Creating Xandr profile: %s", payload.get("profile", {}).get("description", "unnamed"))
        result = await self._request("POST", "/profile", json_data=payload)
        return result.get("response", result)

    async def update_profile(self, profile_id: int, payload: dict[str, Any]) -> dict[str, Any]:
        """Update a profile by id.

        PUT {BASE_URL}/profile?id={profile_id}
        """
        logger.info("Updating Xandr profile id=%d", profile_id)
        result = await self._request("PUT", "/profile", json_data=payload, params={"id": profile_id})
        return result.get("response", result)

    async def create_line_item(
        self,
        payload: dict[str, Any],
        *,
        advertiser_id: int,
        member_id: int | None = None,
    ) -> dict[str, Any]:
        """Create a curated-deal line item.

        POST {BASE_URL}/line-item?advertiser_id=N&member_id=M
        JSON: { "line-item": { ... } }

        Per Microsoft Curate (Xandr) docs the curated-deal "line item"
        carries the curator margin (`valuation.min_margin_pct` /
        `valuation.min_margin_cpm`), references the deal via
        `deals[].id`, the insertion order via `insertion_orders[].id`,
        and the profile via `profile_id`. `line_item_subtype` MUST be
        "standard_curated" for curator deals.
        """
        params: dict[str, Any] = {"advertiser_id": int(advertiser_id)}
        if member_id is not None:
            params["member_id"] = int(member_id)
        logger.info(
            "Creating Xandr line item: %s (advertiser_id=%d)",
            payload.get("line-item", {}).get("name", "unnamed"),
            advertiser_id,
        )
        result = await self._request("POST", "/line-item", json_data=payload, params=params)
        return result.get("response", result)

    async def get_line_item(self, line_item_id: int) -> dict[str, Any] | None:
        """Fetch a curated-deal line item by id.

        GET {BASE_URL}/line-item?id={line_item_id}
        """
        result = await self._request("GET", "/line-item", params={"id": int(line_item_id)})
        return result.get("response", {}).get("line-item")

    async def list_segments(
        self,
        *,
        member_id: int | None = None,
        name_like: str | None = None,
        page_size: int = 100,
    ) -> list[dict[str, Any]]:
        """List audience segments (paginated).

        GET {BASE_URL}/segment

        The `search=` query param has not behaved reliably against the live
        API for segment names with punctuation (returns empty on inputs
        like "Cars & Auto_Chrysler Enthusiasts"). When `name_like` is set
        we still pass it as a hint, but if the first page is empty we fall
        back to a full catalog scan so the resolver can match client-side
        and surface candidates.
        """

        async def _fetch_pages(extra_params: dict[str, Any]) -> list[dict[str, Any]]:
            collected: list[dict[str, Any]] = []
            start = 0
            while True:
                params: dict[str, Any] = {"num_elements": page_size, "start_element": start, **extra_params}
                if member_id is not None:
                    params["member_id"] = member_id
                result = await self._request("GET", "/segment", params=params)
                response = result.get("response", {}) if isinstance(result, dict) else {}
                page = response.get("segments") or []
                if not isinstance(page, list) or not page:
                    break
                collected.extend(page)
                if len(page) < page_size:
                    break
                start += page_size
                if start > 10000:
                    break
            return collected

        if name_like:
            filtered = await _fetch_pages({"search": name_like})
            if filtered:
                return filtered
        return await _fetch_pages({})

    async def list_content_categories(
        self,
        *,
        name_like: str | None = None,
        page_size: int = 100,
    ) -> list[dict[str, Any]]:
        """List IAB content categories (paginated).

        GET {BASE_URL}/content-category

        The `search=` query param has not behaved reliably against the live
        API (returns empty for common IAB names like "Auto Parts"). We
        always page through the full catalog and match client-side; the
        catalog is small (~400 entries) and disk-cached, so this is cheap.
        The `name_like` arg is preserved for callers but ignored.
        """
        del name_like  # ignored; full catalog returned
        out: list[dict[str, Any]] = []
        start = 0
        while True:
            params: dict[str, Any] = {"num_elements": page_size, "start_element": start}
            result = await self._request("GET", "/content-category", params=params)
            response = result.get("response", {}) if isinstance(result, dict) else {}
            page = response.get("content-categories") or response.get("content_categories") or []
            if not isinstance(page, list) or not page:
                break
            out.extend(page)
            if len(page) < page_size:
                break
            start += page_size
            # Safety cap — Xandr's IAB catalog is well under 2k.
            if start > 5000:
                break
        return out

    async def list_countries(
        self,
        *,
        name_like: str | None = None,
        page_size: int = 300,
    ) -> list[dict[str, Any]]:
        """List supported countries.

        GET {BASE_URL}/country
        """
        params: dict[str, Any] = {"num_elements": page_size}
        if name_like:
            params["search"] = name_like
        result = await self._request("GET", "/country", params=params)
        return result.get("response", {}).get("countries", [])

    async def list_regions(
        self,
        *,
        country_id: int | None = None,
        name_like: str | None = None,
        page_size: int = 300,
    ) -> list[dict[str, Any]]:
        """List regions (state/province) optionally scoped to a country.

        GET {BASE_URL}/region
        """
        params: dict[str, Any] = {"num_elements": page_size}
        if country_id is not None:
            params["country_id"] = country_id
        if name_like:
            params["search"] = name_like
        result = await self._request("GET", "/region", params=params)
        return result.get("response", {}).get("regions", [])

    async def list_deal_lists(
        self,
        *,
        member_id: int | None = None,
        name_like: str | None = None,
        page_size: int = 100,
    ) -> list[dict[str, Any]]:
        """List deal-list (inventory whitelist/blocklist) objects.

        GET {BASE_URL}/deal-list
        """
        params: dict[str, Any] = {"num_elements": page_size}
        if name_like:
            params["search"] = name_like
        if member_id is not None:
            params["member_id"] = member_id
        result = await self._request("GET", "/deal-list", params=params)
        return result.get("response", {}).get("deal-lists", []) or result.get("response", {}).get("deal_lists", [])

    async def create_deal_list(self, payload: dict[str, Any]) -> dict[str, Any]:
        """Create a deal-list.

        POST {BASE_URL}/deal-list
        JSON: { "deal-list": { ... } }
        """
        result = await self._request("POST", "/deal-list", json_data=payload)
        return result.get("response", result)

    async def get_report_metadata(self, report_type: str | None = None) -> dict[str, Any]:
        params = {"meta": report_type} if report_type else {"meta": True}
        return await self._request("GET", "/report", params=params)

    async def request_report(self, payload: dict[str, Any]) -> dict[str, Any]:
        return await self._request("POST", "/report", json_data=payload)

    async def get_report_status(self, report_id: str) -> dict[str, Any]:
        return await self._request("GET", "/report", params={"id": report_id})

    async def download_report(self, report_id: str) -> tuple[bytes, str]:
        await self._ensure_token()
        client = await self._get_http_client()
        response = await client.get(
            f"{self.base_url}/report-download",
            headers=self._get_headers(),
            params={"id": report_id},
        )
        response.raise_for_status()
        return response.content, response.headers.get("content-type", "text/csv")


# Global client instance
_xandr_client: XandrClient | None = None


def get_xandr_client() -> XandrClient:
    """Get or create the Xandr client singleton."""
    global _xandr_client
    if _xandr_client is None:
        _xandr_client = XandrClient()
    return _xandr_client


# ──────────────────────────────────────────────────────────────────────────────
# Disk cache for stable Xandr lookup results (buyer member listings, etc.).
#
# Mirrors the PubMatic disk-cache pattern (commit 98d5200). Best-effort: any
# IO error or malformed cache file is treated as a miss. Disable with
# XANDR_CACHE_TTL_SECONDS=0.
# ──────────────────────────────────────────────────────────────────────────────


def _cache_dir() -> Path:
    base = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
    return Path(base) / "cutlass" / "xandr"


def _cache_ttl_seconds() -> int:
    raw = os.environ.get("XANDR_CACHE_TTL_SECONDS", "14400")
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


# ──────────────────────────────────────────────────────────────────────────────
# Name → ID resolvers for the locked-down deal-creation surface.
#
# Every resolver supports a raw-ID escape hatch (commit 5783a46 pattern from
# PubMatic): if the caller passes an int (or numeric string), it's returned
# unchanged with a synthetic name lookup, no API call. Name resolution is
# best-effort — failure surfaces blocker candidates rather than silently
# guessing.
# ──────────────────────────────────────────────────────────────────────────────


_XANDR_DEAL_TYPES: list[dict[str, Any]] = [
    {"id": 1, "name": "Open Auction"},
    {"id": 2, "name": "Private Auction"},
    {"id": 3, "name": "Programmatic Guaranteed"},
    {"id": 5, "name": "Curated"},
]


class XandrResolutionError(ValueError):
    """Raised when a name cannot be resolved to an ID. Carries blocker details."""

    def __init__(self, message: str, *, code: str, details: dict[str, Any]):
        super().__init__(message)
        self.message = message
        self.code = code
        self.details = details


def _coerce_numeric_id(value: Any) -> int | None:
    """Return value as int if it's an int or pure-numeric string; else None."""
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, str) and value.strip().lstrip("-").isdigit():
        return int(value.strip())
    return None


def _resolve_xandr_deal_type(value: Any) -> dict[str, Any]:
    """
    Resolve a deal type name or id to {"id": int, "name": str}.

    Path A (Private Auction) only ever uses id=2; the hardcoded enum covers
    Open Auction (1), Private Auction (2), and Programmatic Guaranteed (3) so
    this resolver makes no API call. New deal types added by Xandr would
    surface as XandrResolutionError with the known sample.
    """
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        for entry in _XANDR_DEAL_TYPES:
            if entry["id"] == numeric:
                return dict(entry)
        raise XandrResolutionError(
            f"Unknown Xandr deal type id: {numeric}",
            code="deal_type_unresolved",
            details={"input": numeric, "available": list(_XANDR_DEAL_TYPES)},
        )
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "deal_type must be a non-empty string or numeric id",
            code="deal_type_invalid_input",
            details={"input": value, "available": list(_XANDR_DEAL_TYPES)},
        )
    needle = value.strip().casefold()
    for entry in _XANDR_DEAL_TYPES:
        if entry["name"].casefold() == needle:
            return dict(entry)
    raise XandrResolutionError(
        f"Unknown Xandr deal type: {value!r}",
        code="deal_type_unresolved",
        details={"input": value, "available": list(_XANDR_DEAL_TYPES)},
    )


def _buyer_cache_key(*, member_id: int | None, name_like: str | None) -> str:
    parts = [
        f"m={member_id if member_id is not None else ''}",
        f"n={(name_like or '').casefold().replace('/', '_')}",
    ]
    return "buyers_" + "_".join(parts)


async def _xandr_list_buyers_cached(
    *,
    name_like: str | None = None,
    member_id: int | None = None,
) -> list[dict[str, Any]]:
    """Disk-cached wrapper around XandrClient.list_buyers.

    Xandr's GET /buyer endpoint returns 404 when the `name=` query param is
    set (the live API doesn't support name filtering on this endpoint), so
    we always list without `name_like` and match client-side. The argument
    is kept for backward-compat callers but ignored at the wire layer.
    """
    key = _buyer_cache_key(member_id=member_id, name_like=None)
    cached = _cache_get(key)
    if isinstance(cached, list):
        return cached
    client = get_xandr_client()
    buyers = await client.list_buyers(name_like=None, member_id=member_id)
    if isinstance(buyers, list):
        _cache_put(key, buyers)
    return buyers


def _pick_buyer_match(buyers: list[dict[str, Any]], needle: str) -> tuple[dict[str, Any] | None, list[dict[str, Any]]]:
    """
    Return (exact_match_or_None, candidate_list).

    Exact match is case-insensitive on `name`. Candidates are up to 5 fuzzy
    matches whose name contains the needle as a substring (also
    case-insensitive). If exactly one candidate matches the substring and
    no exact match exists, that single candidate is treated as exact.
    """
    if not needle:
        return None, []
    nfold = needle.casefold()
    exact: dict[str, Any] | None = None
    contains: list[dict[str, Any]] = []
    for buyer in buyers:
        name = buyer.get("name") if isinstance(buyer, dict) else None
        if not isinstance(name, str):
            continue
        nname = name.casefold()
        if nname == nfold:
            exact = buyer
            continue
        if nfold in nname:
            contains.append(buyer)
    if exact is None and len(contains) == 1:
        exact = contains[0]
        contains = []
    return exact, contains[:5]


async def _resolve_xandr_buyer(value: Any, *, member_id: int | None = None) -> dict[str, Any]:
    """
    Resolve a buyer name or id to {"id": int, "name": str | None}.

    Escape hatch: numeric input bypasses lookup entirely. Name input fans out
    to XandrClient.list_buyers (disk-cached) and matches case-insensitively.
    Ambiguous or missing matches raise XandrResolutionError carrying
    candidate matches for the prepare-step blocker payload. HTTP errors
    from the upstream `/buyer` endpoint are converted into XandrResolutionError
    so they surface as `xandr_unresolved_buyer` quality flags rather than
    raw httpx exceptions escaping out of the execute tool.
    """
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "buyer must be a non-empty string or numeric id",
            code="buyer_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    try:
        buyers = await _xandr_list_buyers_cached(member_id=member_id)
    except httpx.HTTPStatusError as exc:
        raise XandrResolutionError(
            f"Buyer lookup failed: HTTP {exc.response.status_code} from Xandr /buyer endpoint",
            code="buyer_lookup_failed",
            details={"input": value, "status_code": exc.response.status_code, "candidates": []},
        ) from exc
    exact, candidates = _pick_buyer_match(buyers, needle)
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name")}
    raise XandrResolutionError(
        f"Buyer not resolved: {value!r}",
        code="buyer_unresolved" if not candidates else "buyer_ambiguous",
        details={
            "input": value,
            "candidates": [
                {"id": c.get("id"), "name": c.get("name")} for c in candidates if isinstance(c.get("id"), int)
            ],
        },
    )


# Channel-aware targeting defaults. When the caller passes `channel` and
# omits explicit `device_types`, the execute flow auto-fills the device set
# for that channel by attaching a profile to the deal post-create. Mirrors
# the OpenX `DEFAULT_RENDERING_CONTEXTS` and IX `IX_DEVICE_VALUES_*` patterns.
XANDR_CHANNELS_DISPLAY: frozenset[str] = frozenset({"display", "olv", "display_olv", "display/olv"})
XANDR_CHANNELS_CTV: frozenset[str] = frozenset({"ctv"})
XANDR_CHANNELS_OTT: frozenset[str] = frozenset({"ott"})

# Xandr device-type ids. AppNexus profile.device_type_targets accepts numeric
# ids representing device categories. Names map to canonical AppNexus enum:
#   1 = Desktops & Laptops, 2 = Phones, 3 = Tablets,
#   4 = Connected TV, 5 = Set-top Box, 6 = Game Console.
# Resolver accepts case-insensitive names and aliases.
XANDR_DEVICE_TYPE_IDS: dict[str, int] = {
    "desktop": 1,
    "desktops & laptops": 1,
    "laptop": 1,
    "pc": 1,
    "mobile": 2,
    "phone": 2,
    "phones": 2,
    "tablet": 3,
    "tablets": 3,
    "ctv": 4,
    "connectedtv": 4,
    "connected tv": 4,
    "tv": 4,
    "settopbox": 5,
    "set-top box": 5,
    "set top box": 5,
    "stb": 5,
    "console": 6,
    "game console": 6,
}
XANDR_DEVICE_VALUES_DISPLAY: tuple[int, ...] = (1, 2, 3)  # desktop, phone, tablet
XANDR_DEVICE_VALUES_CTV: tuple[int, ...] = (4, 5)  # CTV + set-top box
# OTT = app-only mobile/tablet video (phone + tablet), distinct from CTV's
# TV-device set. Per cutlass/protocols/deal-brief.schema.yaml and IX's
# IX_DEVICE_VALUES_OTT (Phone+Tablet). NOTE: OpenX/PubMatic currently include
# desktop in their OTT device set — a cross-SSP inconsistency tracked separately.
XANDR_DEVICE_VALUES_OTT: tuple[int, ...] = (2, 3)  # phone, tablet

# Elcano-on-Xandr identity. Xandr Curate deals are line items, and a line
# item requires both an advertiser_id and an insertion_order_id. The trader
# confirmed the canonical Elcano member + default advertiser; insertion
# orders are partner-specific and the caller must always specify which one
# the deal goes under.
ELCANO_XANDR_MEMBER_ID: int = 17094  # Hyphatec LLC (curator)
ELCANO_XANDR_DEFAULT_ADVERTISER_ID: int = 11447334  # Elcano_Marketplaces

# Xandr deal-type id 5 ("Curated") with version=2 is required for Curate-
# product deals per Microsoft Curate docs. The legacy /deal type.id=2
# ("Private Auction") creates a non-Curate deal that won't serve via the
# Curate workflow.
XANDR_DEAL_TYPE_ID_CURATED: int = 5
XANDR_DEAL_TYPE_NAME_CURATED: str = "Curated"
XANDR_DEAL_VERSION_CURATED: int = 2

# Curated-deal-line-item canonical fields. The line item lives at /line-item
# with line_item_subtype="standard_curated"; revenue_type "vcpm" is the
# Standard / Dynamic CPM auction (curator default), "cpm" is Fixed Price.
XANDR_LINE_ITEM_SUBTYPE_CURATED: str = "standard_curated"
XANDR_REVENUE_TYPE_DYNAMIC_CPM: str = "vcpm"
XANDR_REVENUE_TYPE_FIXED_CPM: str = "cpm"

# Default IO catalog seeded from the trader's snapshot. The MCP uses this
# to resolve `insertion_order_name` -> id without requiring a live API
# call on every deal-creation. The resolver looks up the live list when
# names don't match the cache (e.g., a newly-created IO).
XANDR_INSERTION_ORDER_SEED_CATALOG: tuple[dict[str, Any], ...] = (
    {"id": 11133454, "name": "Dealer", "advertiser_id": 11447334, "state": "active"},
    {"id": 11133456, "name": "Dealer - Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11144417, "name": "Elcano Testing", "advertiser_id": 11460661, "state": "active"},
    {"id": 11149863, "name": "Nike", "advertiser_id": 11462862, "state": "active"},
    {"id": 11196963, "name": "Ribeye", "advertiser_id": 11567334, "state": "active"},
    {"id": 11316257, "name": "Elcano - Premium Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11316592, "name": "Elcano - QC Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11318961, "name": "Elcano - X Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11319231, "name": "Elcano - C - Premium Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11325830, "name": "Elcano - VDX Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11327904, "name": "Elcano - Valassis Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11334692, "name": "Elcano - GrowthNet Marketplaces", "advertiser_id": 11447334, "state": "active"},
    {"id": 11336881, "name": "Elcano - Yahoo Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11377038, "name": "Elcano Talon Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11388774, "name": "Elcano - Nano Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11442095, "name": "Elcano - Axel Springer Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11443185, "name": "Elcano – Marketplace Pro", "advertiser_id": 11447334, "state": "active"},
    {"id": 11463768, "name": "Testing 1243", "advertiser_id": 11769960, "state": "active"},
    {"id": 11464683, "name": "Infillion - JB Deal Request", "advertiser_id": 11770363, "state": "active"},
    {"id": 11504911, "name": "Elcano - Cadent/Adtheorant Marketplace", "advertiser_id": 11447334, "state": "active"},
    {"id": 11544275, "name": "Elcano - Soundwave", "advertiser_id": 11447334, "state": "inactive"},
    {"id": 11549137, "name": "Elcano - PA Dept of Health", "advertiser_id": 11770363, "state": "active"},
    {"id": 11563696, "name": "Elcano - Tarsus", "advertiser_id": 11770363, "state": "active"},
    {"id": 11608061, "name": "Elcano - VA Lottery", "advertiser_id": 11447334, "state": "active"},
    {"id": 11640422, "name": "Elcano - Blue Cross Idaho", "advertiser_id": 11770363, "state": "active"},
    {"id": 11653633, "name": "Elcano - Optimal", "advertiser_id": 11770363, "state": "active"},
    {"id": 11769437, "name": "Soundwave - MCV", "advertiser_id": 11770363, "state": "active"},
    {"id": 11771318, "name": "Soundwave - VA DMV", "advertiser_id": 11770363, "state": "active"},
    {"id": 11782676, "name": "Adgenuity - Visit Williamsburg - ELC00056", "advertiser_id": 12058757, "state": "active"},
    {"id": 11803100, "name": "soundwave - Axel", "advertiser_id": 11770363, "state": "active"},
    {"id": 11856243, "name": "Elcano - OMG Marketplace", "advertiser_id": 11395230, "state": "active"},
    {"id": 11874590, "name": "Elcano - Claritas", "advertiser_id": 11447334, "state": "active"},
    {"id": 11056826, "name": "Media Cause_Uncommon Schools_2.7.2025", "advertiser_id": 11378209, "state": "active"},
)


def _normalize_xandr_channel_hint(channel: str | None) -> str | None:
    """Return canonical "display"/"ctv"/"ott"/None for a free-form channel hint.

    NOTE: This is distinct from `_resolve_xandr_deal_type`, which resolves
    Xandr's deal-type-id enum (Open/Private/PG). The channel hint indicates
    the trader's intent (display vs CTV vs OTT) and drives device-default
    targeting via the profile-attach flow in
    `xandr_execute_deal_from_prompt_inputs`.
    """
    if channel is None:
        return None
    normalized = str(channel).strip().lower()
    if normalized in XANDR_CHANNELS_DISPLAY:
        return "display"
    if normalized in XANDR_CHANNELS_CTV:
        return "ctv"
    if normalized in XANDR_CHANNELS_OTT:
        return "ott"
    return None


def _apply_xandr_channel_device_defaults(
    device_types: list[Any] | None,
    channel: str | None,
) -> tuple[list[Any] | None, bool]:
    """Auto-fill device_types from a channel hint when caller didn't supply any.

    Returns (resolved_device_types, applied_default).
    """
    if device_types:
        return device_types, False
    canonical = _normalize_xandr_channel_hint(channel)
    if canonical == "display":
        return list(XANDR_DEVICE_VALUES_DISPLAY), True
    if canonical == "ctv":
        return list(XANDR_DEVICE_VALUES_CTV), True
    if canonical == "ott":
        return list(XANDR_DEVICE_VALUES_OTT), True
    return device_types, False


def _resolve_xandr_device_type_id(value: Any) -> int:
    """Resolve a device-type name OR numeric id to a Xandr device-type id."""
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return numeric
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "device_type must be a non-empty name or numeric id",
            code="device_type_invalid_input",
            details={"input": value, "available_aliases": sorted(XANDR_DEVICE_TYPE_IDS)},
        )
    needle = value.strip().casefold()
    if needle in XANDR_DEVICE_TYPE_IDS:
        return XANDR_DEVICE_TYPE_IDS[needle]
    # Strip whitespace/punctuation for a final lookup
    normalized = _NON_ALNUM_RE.sub("", needle)
    for alias, type_id in XANDR_DEVICE_TYPE_IDS.items():
        if _NON_ALNUM_RE.sub("", alias) == normalized:
            return type_id
    raise XandrResolutionError(
        f"Unknown Xandr device type: {value!r}",
        code="device_type_unresolved",
        details={"input": value, "available_aliases": sorted(XANDR_DEVICE_TYPE_IDS)},
    )


def _entity_cache_key(prefix: str, *parts: Any) -> str:
    """Stable cache key from a prefix + ordered parts (None becomes '')."""
    return prefix + "_" + "_".join(str(p or "").casefold().replace("/", "_") for p in parts)


def _pick_entity_match(
    entries: list[dict[str, Any]],
    needle: str,
    *,
    name_keys: tuple[str, ...] = ("name", "code"),
) -> tuple[dict[str, Any] | None, list[dict[str, Any]]]:
    """Return (exact_or_None, fuzzy_candidates) for a list of catalog entries.

    Match priority:
      1. Exact case-insensitive equality on any name_key.
      2. Bidirectional substring match (needle in value OR value in needle).
      3. Token overlap match (>=1 alphanumeric token shared, length >=3).

    The token-overlap pass is what surfaces useful candidates when the
    trader's input doesn't substring-match the catalog (e.g. "Auto Parts"
    vs "Automotive Parts & Accessories"). Without it, the unresolved
    quality flag carries an empty `candidates: []` and the agent has no
    debugging signal — exactly what broke the live Xandr run.
    """
    if not needle:
        return None, []
    nfold = needle.casefold()
    needle_tokens = {t for t in re.findall(r"[a-z0-9]{3,}", nfold) if t}
    exact: dict[str, Any] | None = None
    contains: list[dict[str, Any]] = []
    token_matches: list[dict[str, Any]] = []
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        matched_substring = False
        matched_token = False
        for key in name_keys:
            value = entry.get(key)
            if not isinstance(value, str):
                continue
            vfold = value.casefold()
            if vfold == nfold:
                exact = entry
                break
            if nfold in vfold or vfold in nfold:
                matched_substring = True
                continue
            if needle_tokens:
                value_tokens = {t for t in re.findall(r"[a-z0-9]{3,}", vfold) if t}
                if needle_tokens & value_tokens:
                    matched_token = True
        if exact is not None:
            break
        if matched_substring:
            contains.append(entry)
        elif matched_token:
            token_matches.append(entry)
    if exact is None and len(contains) == 1:
        exact = contains[0]
        contains = []
    candidates = contains + token_matches
    return exact, candidates[:5]


async def _resolve_xandr_country(value: Any) -> dict[str, Any]:
    """Resolve a country name OR numeric id OR ISO-2 code to {id, name, code}."""
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None, "code": None}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "country must be a non-empty name, ISO-2 code, or numeric id",
            code="country_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    cache_key = _entity_cache_key("xandr_countries", needle)
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        countries = cached
    else:
        client = get_xandr_client()
        countries = await client.list_countries(name_like=needle)
        if isinstance(countries, list):
            _cache_put(cache_key, countries)
    exact, candidates = _pick_entity_match(countries, needle, name_keys=("name", "code"))
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name"), "code": exact.get("code")}
    raise XandrResolutionError(
        f"Country not resolved: {value!r}",
        code="country_unresolved" if not candidates else "country_ambiguous",
        details={"input": value, "candidates": [{"id": c.get("id"), "name": c.get("name")} for c in candidates]},
    )


async def _resolve_xandr_region(value: Any, *, country_id: int | None = None) -> dict[str, Any]:
    """Resolve a region (state/province) name OR id (optionally scoped by country)."""
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None, "country_id": country_id}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "region must be a non-empty name or numeric id",
            code="region_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    cache_key = _entity_cache_key("xandr_regions", country_id, needle)
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        regions = cached
    else:
        client = get_xandr_client()
        regions = await client.list_regions(country_id=country_id, name_like=needle)
        if isinstance(regions, list):
            _cache_put(cache_key, regions)
    exact, candidates = _pick_entity_match(regions, needle, name_keys=("name", "code"))
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name"), "country_id": country_id}
    raise XandrResolutionError(
        f"Region not resolved: {value!r}",
        code="region_unresolved" if not candidates else "region_ambiguous",
        details={
            "input": value,
            "country_id": country_id,
            "candidates": [{"id": c.get("id"), "name": c.get("name")} for c in candidates],
        },
    )


async def _xandr_iab_catalog_cached() -> list[dict[str, Any]]:
    """Disk-cached full IAB content-category catalog."""
    cache_key = "xandr_iab_full"
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        return cached
    client = get_xandr_client()
    categories = await client.list_content_categories()
    if isinstance(categories, list):
        _cache_put(cache_key, categories)
    return categories or []


async def _resolve_xandr_iab_category(value: Any) -> dict[str, Any]:
    """Resolve an IAB content-category name/code/id to {id, name, code}.

    Matches against the full catalog (loaded once and cached) on `name`,
    `code`, and the IAB sub-id format. When no exact match is found, the
    quality-flag `candidates` list is populated with the top substring
    matches so the agent can suggest fixes to the trader.
    """
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None, "code": None}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "iab_category must be a non-empty name, code, or numeric id",
            code="iab_category_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    try:
        categories = await _xandr_iab_catalog_cached()
    except httpx.HTTPStatusError as exc:
        raise XandrResolutionError(
            f"IAB catalog lookup failed: HTTP {exc.response.status_code}",
            code="iab_category_lookup_failed",
            details={"input": value, "status_code": exc.response.status_code, "candidates": []},
        ) from exc
    exact, candidates = _pick_entity_match(categories, needle, name_keys=("name", "code", "iab_id"))
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name"), "code": exact.get("code")}
    raise XandrResolutionError(
        f"IAB content category not resolved: {value!r}",
        code="iab_category_unresolved" if not candidates else "iab_category_ambiguous",
        details={
            "input": value,
            "candidates": [{"id": c.get("id"), "name": c.get("name"), "code": c.get("code")} for c in candidates],
        },
    )


async def _resolve_xandr_segment(value: Any, *, member_id: int | None = None) -> dict[str, Any]:
    """Resolve a segment name OR numeric id."""
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "segment must be a non-empty name or numeric id",
            code="segment_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    cache_key = _entity_cache_key("xandr_segments", member_id, needle)
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        segments = cached
    else:
        client = get_xandr_client()
        try:
            segments = await client.list_segments(member_id=member_id, name_like=needle)
        except httpx.HTTPStatusError as exc:
            raise XandrResolutionError(
                f"Segment lookup failed: HTTP {exc.response.status_code}",
                code="segment_lookup_failed",
                details={"input": value, "status_code": exc.response.status_code, "candidates": []},
            ) from exc
        if isinstance(segments, list):
            _cache_put(cache_key, segments)
    exact, candidates = _pick_entity_match(segments, needle, name_keys=("name", "short_name"))
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name")}
    raise XandrResolutionError(
        f"Segment not resolved: {value!r}",
        code="segment_unresolved" if not candidates else "segment_ambiguous",
        details={"input": value, "candidates": [{"id": c.get("id"), "name": c.get("name")} for c in candidates]},
    )


async def _resolve_xandr_deal_list(value: Any, *, member_id: int | None = None) -> dict[str, Any]:
    """Resolve a deal-list name OR numeric id."""
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        return {"id": numeric, "name": None}
    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "deal_list must be a non-empty name or numeric id",
            code="deal_list_invalid_input",
            details={"input": value},
        )
    needle = value.strip()
    cache_key = _entity_cache_key("xandr_deal_lists", member_id, needle)
    cached = _cache_get(cache_key)
    if isinstance(cached, list):
        deal_lists = cached
    else:
        client = get_xandr_client()
        deal_lists = await client.list_deal_lists(member_id=member_id, name_like=needle)
        if isinstance(deal_lists, list):
            _cache_put(cache_key, deal_lists)
    exact, candidates = _pick_entity_match(deal_lists, needle, name_keys=("name",))
    if exact is not None and isinstance(exact.get("id"), int):
        return {"id": int(exact["id"]), "name": exact.get("name")}
    raise XandrResolutionError(
        f"Deal list not resolved: {value!r}",
        code="deal_list_unresolved" if not candidates else "deal_list_ambiguous",
        details={"input": value, "candidates": [{"id": c.get("id"), "name": c.get("name")} for c in candidates]},
    )


def _normalize_xandr_io_name(value: str) -> str:
    """Normalize an IO name for comparison: lowercase, collapse whitespace,
    treat – (en-dash) and - (hyphen) as equivalent."""
    text = str(value).strip().lower()
    text = text.replace("–", "-").replace("—", "-")
    return _SPACE_RE.sub(" ", text)


async def _resolve_xandr_insertion_order(value: Any) -> dict[str, Any]:
    """Resolve an IO name OR numeric id to {id, name, advertiser_id, state}.

    Resolution order:
      1. Numeric input → return {id, name=None, advertiser_id=None, state=None}
         (caller should re-fetch if it needs the parent advertiser).
      2. String input → match against XANDR_INSERTION_ORDER_SEED_CATALOG
         using normalized comparison (case-insensitive, whitespace-tolerant,
         en-dash / hyphen equivalence).
      3. Inactive IOs raise XandrResolutionError — Curate deals on inactive
         IOs are rejected by Xandr server-side, so fail fast on the client.
    """
    numeric = _coerce_numeric_id(value)
    if numeric is not None:
        for entry in XANDR_INSERTION_ORDER_SEED_CATALOG:
            if entry["id"] == numeric:
                if entry["state"] != "active":
                    raise XandrResolutionError(
                        f"Insertion order {entry['name']!r} (id {numeric}) is inactive — "
                        "Xandr Curate rejects deals on inactive IOs.",
                        code="insertion_order_inactive",
                        details={"input": numeric, "name": entry["name"], "state": entry["state"]},
                    )
                return dict(entry)
        # Unknown numeric id — pass through; the line-item create call will
        # fail with a Xandr server error if the id is bad. This is the
        # escape hatch for newly-created IOs not yet in our seed catalog.
        return {"id": numeric, "name": None, "advertiser_id": None, "state": None}

    if not isinstance(value, str) or not value.strip():
        raise XandrResolutionError(
            "insertion_order must be a non-empty name or numeric id",
            code="insertion_order_invalid_input",
            details={"input": value},
        )

    needle = _normalize_xandr_io_name(value)
    exact: dict[str, Any] | None = None
    contains: list[dict[str, Any]] = []
    for entry in XANDR_INSERTION_ORDER_SEED_CATALOG:
        haystack = _normalize_xandr_io_name(entry["name"])
        if haystack == needle:
            exact = entry
            break
        if needle in haystack:
            contains.append(entry)
    if exact is None and len(contains) == 1:
        exact = contains[0]
        contains = []

    if exact is None:
        raise XandrResolutionError(
            f"Insertion order not resolved: {value!r}",
            code="insertion_order_unresolved" if not contains else "insertion_order_ambiguous",
            details={
                "input": value,
                "candidates": [{"id": c["id"], "name": c["name"]} for c in contains[:5]],
            },
        )

    if exact["state"] != "active":
        raise XandrResolutionError(
            f"Insertion order {exact['name']!r} is inactive — Xandr Curate rejects deals on inactive IOs.",
            code="insertion_order_inactive",
            details={"input": value, "id": exact["id"], "state": exact["state"]},
        )

    return dict(exact)


def _build_xandr_profile_payload(
    *,
    name: str,
    member_id: int | None,
    device_type_ids: list[int] | None,
    country_targets: list[dict[str, Any]] | None,
    region_targets: list[dict[str, Any]] | None,
    content_category_targets: list[dict[str, Any]] | None,
    segment_targets: list[dict[str, Any]] | None,
    deal_list_targets: list[dict[str, Any]] | None,
) -> dict[str, Any]:
    """Construct a Xandr profile payload from resolved targeting components."""
    profile: dict[str, Any] = {
        "description": name,
        "is_template": False,
    }
    if member_id is not None:
        profile["member_id"] = int(member_id)
    if device_type_ids:
        profile["device_type_targets"] = [{"device_type": int(tid)} for tid in device_type_ids]
        profile["device_type_action"] = "include"
    if country_targets:
        profile["country_targets"] = [
            {"id": int(c["id"]), "name": c.get("name")} for c in country_targets if isinstance(c.get("id"), int)
        ]
        profile["country_action"] = "include"
    if region_targets:
        profile["region_targets"] = [
            {"id": int(r["id"]), "name": r.get("name")} for r in region_targets if isinstance(r.get("id"), int)
        ]
        profile["region_action"] = "include"
    if content_category_targets:
        # Microsoft Curate requires the `platform_*` variants on curated-deal
        # line item profiles. Per docs: "You cannot use `placement_targets`,
        # `publisher_targets`, or `content_category_targets` with a curated
        # deal line item."
        profile["platform_content_category_targets"] = [
            {"id": int(c["id"]), "name": c.get("name"), "action": "include"}
            for c in content_category_targets
            if isinstance(c.get("id"), int)
        ]
    if segment_targets:
        profile["segment_targets"] = [
            {"id": int(s["id"]), "action": "include"} for s in segment_targets if isinstance(s.get("id"), int)
        ]
        profile["segment_boolean_operator"] = "or"
    if deal_list_targets:
        profile["deal_list_targets"] = [
            {"id": int(d["id"]), "action": "include"} for d in deal_list_targets if isinstance(d.get("id"), int)
        ]
    return {"profile": profile}


def _make_xandr_quality_flag(flag: str, impact: str, **context: Any) -> dict[str, Any]:
    """Build a structured quality_flags entry (mirrors the IX/PM/MN pattern)."""
    entry: dict[str, Any] = {"flag": flag, "impact": impact}
    for key, value in context.items():
        if value is not None:
            entry[key] = value
    return entry


# Elcano default curator margin on Xandr Curate. Mirrors the 30% PoM
# default already shipped on OpenX/IX/PubMatic/Media.net.
ELCANO_DEFAULT_XANDR_MARGIN_PERCENT: float = 30.0
XANDR_MARGIN_TYPE_PERCENTAGE: str = "percentage"
XANDR_MARGIN_TYPE_CPM: str = "cpm"


def _resolve_xandr_curator_margin(
    margin_percent: float | None,
    margin_cpm: float | None,
) -> tuple[str, float, bool]:
    """Resolve the curator margin from caller inputs.

    Returns (margin_type, margin_value, applied_default).

    - If both inputs are supplied, raise ValueError (mutually exclusive).
    - If margin_cpm is supplied, return ("cpm", value, False).
    - If margin_percent is supplied, return ("percentage", value, False).
    - If neither, return the 30% default (applied_default=True).
    """
    if margin_percent is not None and margin_cpm is not None:
        raise ValueError("margin_percent and margin_cpm are mutually exclusive — pass only one.")
    if margin_cpm is not None:
        return XANDR_MARGIN_TYPE_CPM, float(margin_cpm), False
    if margin_percent is not None:
        return XANDR_MARGIN_TYPE_PERCENTAGE, float(margin_percent), False
    return XANDR_MARGIN_TYPE_PERCENTAGE, ELCANO_DEFAULT_XANDR_MARGIN_PERCENT, True


def _xandr_ad_types_from_channel(channel: str | None) -> list[str]:
    """Map a channel hint to Xandr line-item ad_types.

    display/olv -> ["banner"] for display, ["video"] for olv
    ctv -> ["video"]
    ott -> ["video"]
    None -> default ["banner"]

    The caller can override by passing an explicit ad_types list.
    """
    canonical = _normalize_xandr_channel_hint(channel)
    if canonical in ("ctv", "ott"):
        return ["video"]
    if channel is not None and str(channel).strip().lower() in {"olv", "display_olv", "display/olv"}:
        return ["video"]
    return ["banner"]


def _build_xandr_line_item_payload(
    *,
    name: str,
    deal_id: int,
    insertion_order_id: int,
    profile_id: int | None,
    margin_type: str,
    margin_value: float,
    revenue_type: str,
    revenue_value: float | None,
    floor_price: float | None,
    ad_types: list[str],
    start_date: str,
    end_date: str | None,
    state: str = "active",
    supply_strategy_rtb: bool = False,
    supply_strategy_deals: bool = True,
) -> dict[str, Any]:
    """Construct the Xandr `/line-item` payload for a curated-deal line item.

    Wrapper key MUST be "line-item" (with hyphen) per Curate docs. Required
    fields: line_item_subtype="standard_curated", deals[{id}],
    insertion_orders[{id}], ad_types, revenue_type, valuation, auction_event,
    supply_strategies, budget_intervals[{start_date}].

    Margin lives in `valuation.min_margin_pct` (Percentage) or
    `valuation.min_margin_cpm` (CPM). For revenue_type="vcpm" (Standard /
    Dynamic CPM), `valuation.min_revenue_value` is the floor; for
    revenue_type="cpm" (Fixed Price), the line-item-level `revenue_value`
    is the fixed rate.
    """
    valuation: dict[str, Any] = {}
    if margin_type == XANDR_MARGIN_TYPE_PERCENTAGE:
        valuation["min_margin_pct"] = float(margin_value)
        valuation["min_margin_cpm"] = None
    elif margin_type == XANDR_MARGIN_TYPE_CPM:
        valuation["min_margin_cpm"] = float(margin_value)
        valuation["min_margin_pct"] = None

    if revenue_type == XANDR_REVENUE_TYPE_DYNAMIC_CPM:
        valuation["min_revenue_value"] = float(floor_price) if floor_price is not None else None
        resolved_revenue_value: float | None = None
    else:  # cpm / fixed
        valuation["min_revenue_value"] = None
        resolved_revenue_value = float(revenue_value) if revenue_value is not None else None

    budget_interval: dict[str, Any] = {"start_date": start_date}
    if end_date is not None:
        budget_interval["end_date"] = end_date

    line_item: dict[str, Any] = {
        "name": name,
        "ad_types": list(ad_types),
        "line_item_subtype": XANDR_LINE_ITEM_SUBTYPE_CURATED,
        "state": state,
        "auction_event": {
            "kpi_auction_type_id": 1,
            "payment_auction_type_id": 1,
            "revenue_auction_type_id": 1,
        },
        "budget_intervals": [budget_interval],
        "deals": [{"id": int(deal_id)}],
        "insertion_orders": [{"id": int(insertion_order_id)}],
        "supply_strategies": {
            "managed": False,
            "rtb": bool(supply_strategy_rtb),
            "deals": bool(supply_strategy_deals),
        },
        "revenue_type": revenue_type,
        "revenue_value": resolved_revenue_value,
        "valuation": valuation,
    }
    if profile_id is not None:
        line_item["profile_id"] = int(profile_id)
    return {"line-item": line_item}


def _build_xandr_deal_url(line_item_id: int | None) -> str | None:
    """Build a Xandr Curate console URL for a created curated-deal line item.

    The Curate UI displays deals AS line items, and the deep link is keyed
    on the line item id (not the deal id). Trader-confirmed working URL:

        https://curate.xandr.com/smw/line-items?line_item_id=29338518

    Note the host is `curate.xandr.com` (not `console.xandr.com`), the path
    is `/smw/line-items`, and the id rides as a query param. The earlier
    `console.xandr.com/curate/deals/{deal_id}` form routed to a non-existent
    page in production. Returns None when the line_item_id is missing.
    """
    if not isinstance(line_item_id, int):
        return None
    return f"https://curate.xandr.com/smw/line-items?line_item_id={line_item_id}"


def _write_xandr_report_file(
    *,
    report_id: str,
    content: bytes,
    filename_hint: str | None,
    output_dir: str | None = None,
) -> dict[str, Any]:
    # Operators can pass output_dir per-call (typically the per-conversation
    # workspace dir from the system prompt). The default
    # ~/Victoria/xandr_reports is fine locally but read-only on production
    # hosts running under systemd's ProtectSystem=strict — see magnite_mcp.py
    # for the rationale on dropping the *_DOWNLOAD_DIR env-var override.
    download_dir = Path(output_dir) if output_dir else Path(DEFAULT_REPORT_DOWNLOAD_DIR)
    download_dir = download_dir.expanduser()
    download_dir.mkdir(parents=True, exist_ok=True)
    base_name = (filename_hint or f"xandr_report_{report_id}.csv").strip() or f"xandr_report_{report_id}.csv"
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


def _normalize_xandr_download_content_type(content: bytes, content_type: str) -> str:
    if content_type.lower().startswith("text/html"):
        prefix = content[:128].lstrip()
        if prefix.startswith(b"<"):
            return content_type
        return "text/csv"
    return content_type


def _resolve_xandr_relative_date_range(last_n_days: int) -> tuple[str, str]:
    now = datetime.now(UTC).replace(minute=0, second=0, microsecond=0)
    start = (now - timedelta(days=max(last_n_days - 1, 0))).replace(hour=0)
    end = (now + timedelta(days=1)).replace(hour=0)
    return start.strftime("%Y-%m-%d %H:%M:%S"), end.strftime("%Y-%m-%d %H:%M:%S")


def _resolve_xandr_fields(requested_values: list[str] | None, alias_map: dict[str, str]) -> list[str]:
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


def _resolve_xandr_report_preset(report_type: str | None) -> str | None:
    if report_type is None:
        return None
    normalized = report_type.strip().lower()
    if not normalized:
        return None
    if normalized in XANDR_CURATOR_REPORT_PRESETS:
        return normalized
    return XANDR_REPORT_TYPE_PRESET_ALIASES.get(normalized)


# =============================================================================
# MCP Tools
# =============================================================================


@mcp.tool()
async def create_xandr_deal(payload: dict[str, Any]) -> dict[str, Any]:
    """
    Create a new programmatic deal on the Xandr platform.

    This is a CRITICAL action that requires self-audit confirmation before execution.

    The payload must contain a "deal" object with Xandr Deal Service fields.
    Required fields inside the "deal" object include:
    - name: Human-readable deal name
    - code: Deal code string (must be unique)
    - type: Deal type object with id (e.g., {"id": 2, "name": "Private Auction"})
    - buyer: Single buyer object (e.g., {"id": 123, "name": "Test Buyer"})
    - ask_price: Floor price value (double)

    Optional fields inside the "deal" object:
    - description: Description of the deal
    - active: Whether the deal is active (boolean, default true)
    - start_date: Start date in "YYYY-MM-DD HH:MM:SS" format
    - end_date: End date (null for always-on)
    - payment_type: Payment type string ("default" or "cpvm")
    - currency: Currency code (e.g., "USD")
    - use_deal_floor: Whether to use a deal floor price (boolean)
    - member_id: The seller's member ID

    For backward compatibility, the following field names are also accepted and
    will be automatically transformed:
    - deal_type -> type
    - buyers (list) -> buyer (first element)
    - state ("active"/"inactive") -> active (true/false)
    - payment_type (object with "name") -> payment_type (lowercase string)
    - floor_price -> ask_price

    Args:
        payload: Dictionary containing the deal configuration matching Xandr API schema.
                 Must have a top-level "deal" key.

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the deal was created
            - data: The created deal object from the API response (if successful)
            - error: Error message (if failed)
    """
    logger.info("create_xandr_deal called with payload keys: %s", list(payload.keys()))

    # Validate top-level structure
    deal = payload.get("deal")
    if deal is None or not isinstance(deal, dict):
        return {
            "success": False,
            "error": "Payload must contain a 'deal' object.",
        }

    # Deep copy to avoid mutating the caller's payload
    payload = copy.deepcopy(payload)
    deal = payload["deal"]

    # --- Payload transformation for backward compatibility ---

    # Rename deal_type -> type
    if "deal_type" in deal and "type" not in deal:
        deal["type"] = deal.pop("deal_type")

    # Convert buyers (list) -> buyer (single object)
    if "buyers" in deal and "buyer" not in deal:
        buyers = deal.pop("buyers")
        if isinstance(buyers, list) and len(buyers) > 0:
            deal["buyer"] = buyers[0]

    # Convert state (string) -> active (boolean)
    if "state" in deal and "active" not in deal:
        state = deal.pop("state")
        deal["active"] = state == "active"

    # Convert payment_type from object to lowercase string
    if "payment_type" in deal and isinstance(deal["payment_type"], dict):
        name = deal["payment_type"].get("name", "")
        deal["payment_type"] = name.lower()

    # Rename floor_price -> ask_price
    if "floor_price" in deal and "ask_price" not in deal:
        deal["ask_price"] = deal.pop("floor_price")

    # Validate required fields within the deal object
    required_fields = ["buyer", "code", "type"]
    missing_fields = [f for f in required_fields if f not in deal]
    if missing_fields:
        error_msg = f"Missing required fields in deal object: {', '.join(missing_fields)}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }

    try:
        client = get_xandr_client()
        data = await client.create_deal(payload)

        logger.info("Deal created successfully: %s", deal.get("name"))
        return {
            "success": True,
            "data": data,
        }

    except ValueError as e:
        error_msg = f"Failed to create deal: {e}"
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
        error_msg = f"Failed to create deal: {e}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


@mcp.tool()
async def list_xandr_deals(member_id: int) -> dict[str, Any]:
    """
    Query and return a list of existing deals from the Xandr platform for a given member.

    Use this to review existing deals and their configurations.

    Args:
        member_id: The member ID to list deals for

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the query succeeded
            - deals: List of deal objects (if successful)
            - error: Error message (if failed)
    """
    logger.info("list_xandr_deals called (member_id=%d)", member_id)

    try:
        client = get_xandr_client()
        deals = await client.list_deals(member_id=member_id)

        logger.info("Found %d deals", len(deals))
        return {
            "success": True,
            "deals": deals,
        }

    except ValueError as e:
        error_msg = f"Failed to list deals: {e}"
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
        error_msg = f"Failed to list deals: {e}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


@mcp.tool()
async def get_xandr_deal(deal_id: int) -> dict[str, Any]:
    """
    Fetch full details for a specific deal from the Xandr platform by its ID.

    Args:
        deal_id: The numeric ID of the deal to retrieve

    Returns:
        Dictionary containing:
            - success: Boolean indicating if the query succeeded
            - deal: Complete deal object with all fields (if successful)
            - error: Error message (if failed)
    """
    logger.info("get_xandr_deal called with deal_id: %d", deal_id)

    try:
        client = get_xandr_client()
        deal = await client.get_deal(deal_id=deal_id)

        if not deal:
            return {
                "success": False,
                "error": f"Deal not found: {deal_id}",
            }

        logger.info("Retrieved deal: %s", deal.get("name", deal_id))
        return {
            "success": True,
            "deal": deal,
        }

    except ValueError as e:
        error_msg = f"Failed to get deal: {e}"
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
        error_msg = f"Failed to get deal: {e}"
        logger.error(error_msg)
        return {
            "success": False,
            "error": error_msg,
        }


# Deal-update guardrails per the Deal Service docs (audited 2026-06-12):
# buyer/buyer_seats/buyer_members/buyer_bidders can be set on POST but not
# changed on PUT ("If you want to change the buyer, you need to create a new
# deal"); code is the bid-request deal ID DSPs target, so changing it breaks
# live buyer targeting; is_archived removes the deal from auctions and is an
# irreversibility-adjacent action kept off the agent surface (pause via
# active=false instead).
_XANDR_UPDATE_FORBIDDEN_FIELDS = frozenset(
    {"buyer", "buyer_seats", "buyer_members", "buyer_bidders", "code", "is_archived", "seller"}
)

_XANDR_AUCTION_TYPE_ALIASES: dict[str, int] = {
    "1": 1,
    "first": 1,
    "first_price": 1,
    "2": 2,
    "standard": 2,
    "standard_price": 2,
    "second": 2,
    "3": 3,
    "fixed": 3,
    "fixed_price": 3,
}

_XANDR_DATETIME_RE = re.compile(r"^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$")
_XANDR_DATE_ONLY_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def _normalize_xandr_datetime(field_name: str, value: str, *, end_of_day: bool = False) -> str:
    """Normalize a date input to the Deal Service's "YYYY-MM-DD HH:MM:SS" format."""
    value = value.strip()
    if _XANDR_DATE_ONLY_RE.match(value):
        return f"{value} 23:59:59" if end_of_day else f"{value} 00:00:00"
    if _XANDR_DATETIME_RE.match(value):
        return value
    raise ValueError(f"{field_name} must be 'YYYY-MM-DD' or 'YYYY-MM-DD HH:MM:SS', got {value!r}")


def _build_xandr_deal_update(
    current_deal: dict[str, Any],
    *,
    name: str | None = None,
    ask_price: float | None = None,
    active: bool | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    auction_type: str | int | None = None,
    priority: int | None = None,
    currency: str | None = None,
    description: str | None = None,
    payment_type: str | None = None,
    use_deal_floor: bool | None = None,
    payload_overrides: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Build the partial PUT /deal body. Pure Python — unit-testable.

    Only supplied fields are included (the Deal Service PUT is a genuine
    partial update). ask_price is the one exception: the docs mark it
    Required On PUT, so when the caller doesn't change it the current value
    is round-tripped from the fetched deal. Raises ValueError on validation
    failures and on forbidden fields (buyer/code/is_archived/...).
    """
    deal: dict[str, Any] = {}

    if name is not None:
        if not name.strip() or len(name) > 255:
            raise ValueError(f"name must be 1-255 characters, got length {len(name)}")
        deal["name"] = name
    if active is not None:
        deal["active"] = bool(active)
    if start_date is not None:
        deal["start_date"] = _normalize_xandr_datetime("start_date", start_date)
    if end_date is not None:
        deal["end_date"] = _normalize_xandr_datetime("end_date", end_date, end_of_day=True)
    effective_start = deal.get("start_date", current_deal.get("start_date"))
    effective_end = deal.get("end_date", current_deal.get("end_date"))
    dates_touched = start_date is not None or end_date is not None
    if dates_touched and effective_start and effective_end and str(effective_end) < str(effective_start):
        raise ValueError(f"end_date {effective_end!r} cannot be before start_date {effective_start!r}")

    if auction_type is not None:
        resolved_auction = _XANDR_AUCTION_TYPE_ALIASES.get(str(auction_type).strip().lower())
        if resolved_auction is None:
            raise ValueError("auction_type must be 1/'first', 2/'standard', or 3/'fixed'")
        deal["auction_type"] = {"id": resolved_auction}
    if priority is not None:
        if not 1 <= int(priority) <= 20:
            raise ValueError(f"priority must be 1-20 (20 = highest), got {priority}")
        deal["priority"] = int(priority)
    if currency is not None:
        deal["currency"] = currency
    if description is not None:
        deal["description"] = description
    if payment_type is not None:
        if payment_type not in ("default", "cpvm"):
            raise ValueError("payment_type must be 'default' or 'cpvm'")
        deal["payment_type"] = payment_type
    if use_deal_floor is not None:
        deal["use_deal_floor"] = bool(use_deal_floor)

    if payload_overrides:
        forbidden = sorted(_XANDR_UPDATE_FORBIDDEN_FIELDS & set(payload_overrides))
        if forbidden:
            raise ValueError(
                f"payload_overrides may not change {forbidden}: buyer fields are POST-only (a buyer "
                "change requires a new deal), code is the bid-request deal ID live DSP targeting "
                "depends on, and is_archived is kept off the agent surface — pause via active=false."
            )
        deal.update(dict(payload_overrides))

    if ask_price is not None:
        if float(ask_price) < 0:
            raise ValueError(f"ask_price must be >= 0, got {ask_price}")
        deal["ask_price"] = float(ask_price)

    if not deal:
        raise ValueError("No update fields provided — pass at least one field to change.")

    if "ask_price" not in deal and current_deal.get("ask_price") is not None:
        # The docs mark ask_price Required On PUT — round-trip the current
        # value so a name-only update can't accidentally clear the price.
        deal["ask_price"] = current_deal["ask_price"]

    deal["id"] = current_deal.get("id")
    return {"deal": deal}


@mcp.tool()
async def update_xandr_deal(
    deal_id: int,
    name: str | None = None,
    ask_price: float | None = None,
    active: bool | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    auction_type: str | int | None = None,
    priority: int | None = None,
    currency: str | None = None,
    description: str | None = None,
    payment_type: str | None = None,
    use_deal_floor: bool | None = None,
    payload_overrides: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Update an existing Xandr deal (PUT /deal?id={deal_id}).

    The Deal Service PUT is a genuine PARTIAL update — only the fields you
    pass change; everything else (targeting profile, brands, media types,
    buyer) is left untouched. Pause/resume is active=false/true.

    Not changeable here, by design:
    - buyer / buyer_seats: POST-only per the docs — changing the buyer
      requires creating a new deal.
    - code: the bid-request deal ID that live DSP targeting depends on.
    - is_archived / deletion: the Deal Service DELETE is permanent and
      unrecoverable; irreversible actions are kept off the agent surface.
      Pause the deal instead.
    - Targeting: lives on the deal's PROFILE (profile_id), not the deal
      object — targeting edits are out of this tool's scope.

    Args:
        deal_id: Numeric Xandr deal ID (from list_xandr_deals / create).
        name: New deal name (1-255 chars).
        ask_price: New price shown to the buyer (the minimum bid). When
            omitted, the current value is round-tripped (the API requires
            ask_price on PUT).
        active: false pauses the deal, true resumes it.
        start_date / end_date: "YYYY-MM-DD" (normalized to start/end of day)
            or "YYYY-MM-DD HH:MM:SS", local time per the Deal Service.
        auction_type: 1/'first', 2/'standard', or 3/'fixed'.
        priority: 1-20 (20 highest); applies to Private Auction deals.
        currency: Currency code for the price (e.g. "USD").
        description: Buyer-facing deal description.
        payment_type: 'default' or 'cpvm' (viewable CPM).
        use_deal_floor: Whether the deal floor applies.
        payload_overrides: Raw Deal Service fields merged last (e.g. brands,
            categories, allowed_media_types). Buyer fields, code, and
            is_archived are rejected.

    Returns:
        {"success": True, "deal": <updated deal>, "updated_fields": [...],
         "verification": {...}} or {"success": False, "error": ...}.
    """
    logger.info("update_xandr_deal called for deal_id=%d", deal_id)
    try:
        client = get_xandr_client()

        current_deal = await client.get_deal(deal_id)
        if not current_deal:
            return {"success": False, "error": f"Deal not found: {deal_id}"}

        payload = _build_xandr_deal_update(
            current_deal,
            name=name,
            ask_price=ask_price,
            active=active,
            start_date=start_date,
            end_date=end_date,
            auction_type=auction_type,
            priority=priority,
            currency=currency,
            description=description,
            payment_type=payment_type,
            use_deal_floor=use_deal_floor,
            payload_overrides=payload_overrides,
        )

        result = await client.update_deal(deal_id, payload)
        updated_deal = result.get("deal", result)

        verification: dict[str, Any] | None = None
        try:
            verification = await client.get_deal(deal_id)
        except Exception as verify_exc:  # noqa: BLE001 - verification is best-effort
            logger.warning("Post-update verification fetch failed: %s", verify_exc)

        return {
            "success": True,
            "deal": updated_deal,
            "updated_fields": sorted(k for k in payload["deal"] if k != "id"),
            "verification": verification,
        }
    except ValueError as e:
        return {"success": False, "error": f"Failed to update deal: {e}"}
    except httpx.HTTPStatusError as e:
        return {"success": False, "error": f"Failed to update deal: HTTP {e.response.status_code}: {e.response.text}"}
    except Exception as e:
        return {"success": False, "error": f"Failed to update deal: {e}"}


@mcp.tool()
async def xandr_auth_status() -> dict[str, Any]:
    """
    Check the current authentication status for the Xandr API.

    Verifies that credentials are configured and that the current token is
    active by making a lightweight API call (GET /deal/meta).

    Returns:
        Dictionary containing:
            - configured: Boolean indicating if credentials are set
            - authenticated: Boolean indicating if the token is valid
            - error: Error message (only present if authentication failed)
    """
    logger.info("xandr_auth_status called")

    client = get_xandr_client()

    if not client._is_configured():
        return {
            "configured": False,
            "authenticated": False,
            "error": "Xandr credentials not configured. Set XANDR_USERNAME and XANDR_PASSWORD.",
        }

    try:
        authenticated = await client.verify_token()
        if authenticated:
            return {
                "configured": True,
                "authenticated": True,
            }
        else:
            return {
                "configured": True,
                "authenticated": False,
                "error": "Token verification failed: API returned an error.",
            }
    except Exception as e:
        return {
            "configured": True,
            "authenticated": False,
            "error": f"Token verification failed: {e}",
        }


@mcp.tool()
async def xandr_list_reporting_presets() -> dict[str, Any]:
    presets: dict[str, Any] = {}
    for preset_name, preset in XANDR_CURATOR_REPORT_PRESETS.items():
        presets[preset_name] = {
            "description": preset["description"],
            "report_type": preset["report_type"],
            "columns": preset["columns"],
            "report_interval": preset["report_interval"],
        }
    return {"success": True, "presets": presets}


@mcp.tool()
async def xandr_reporting_healthcheck(report_type: str = "curator_analytics") -> dict[str, Any]:
    try:
        client = get_xandr_client()
        metadata = await client.get_report_metadata(report_type=report_type)
        meta = metadata.get("response", {}).get("meta")
        return {
            "success": True,
            "report_type": report_type,
            "has_metadata": meta is not None,
            "time_granularity": meta.get("time_granularity") if isinstance(meta, dict) else None,
            "column_count": len(meta.get("columns", [])) if isinstance(meta, dict) else None,
            "filter_count": len(meta.get("filters", [])) if isinstance(meta, dict) else None,
            "time_intervals": meta.get("time_intervals") if isinstance(meta, dict) else None,
        }
    except Exception as e:
        return {"success": False, "error": f"Failed Xandr reporting healthcheck: {e}"}


@mcp.tool()
async def xandr_run_report(
    report_type: str,
    columns: list[str],
    report_interval: str | None = None,
    filters: list[dict[str, Any]] | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    format: str = "csv",
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = DEFAULT_REPORT_POLL_TIMEOUT_SECONDS,
    poll_interval_seconds: float = DEFAULT_REPORT_POLL_INTERVAL_SECONDS,
) -> dict[str, Any]:
    """Run a Xandr report. Pass output_dir = workspace path so the agent's
    other tools can read the result on hardened hosts."""
    try:
        report_request: dict[str, Any] = {
            "report": {
                "report_type": report_type,
                "columns": columns,
                "format": format,
            }
        }
        if report_interval:
            report_request["report"]["report_interval"] = report_interval
        if filters:
            report_request["report"]["filters"] = filters
        if start_date:
            report_request["report"]["start_date"] = start_date
        if end_date:
            report_request["report"]["end_date"] = end_date

        client = get_xandr_client()
        created = await client.request_report(report_request)
        report_id = created.get("response", {}).get("report_id")
        if not report_id:
            return {"success": False, "error": "Xandr report request succeeded but no report_id was returned."}

        deadline = time.time() + max(poll_timeout_seconds, 0)
        sleep_seconds = max(poll_interval_seconds, 0.1)
        last_status: dict[str, Any] | None = None

        while True:
            status_response = await client.get_report_status(report_id)
            execution_status = status_response.get("response", {}).get("execution_status")
            report_details = status_response.get("response", {}).get("report", {})
            last_status = {
                "execution_status": execution_status,
                "report": report_details,
            }
            if str(execution_status).lower() == "ready":
                content, content_type = await client.download_report(report_id)
                download = _write_xandr_report_file(
                    report_id=report_id,
                    content=content,
                    filename_hint=filename_hint,
                    output_dir=output_dir,
                )
                download["content_type"] = _normalize_xandr_download_content_type(content, content_type)
                return {
                    "success": True,
                    "report_id": report_id,
                    "report_type": report_type,
                    "execution_status": execution_status,
                    "report": report_details,
                    "download": download,
                }
            if str(execution_status).lower() in {"failed", "error"}:
                return {
                    "success": False,
                    "report_id": report_id,
                    "execution_status": execution_status,
                    "report": report_details,
                    "error": "Xandr report execution failed before download became available.",
                }
            if time.time() >= deadline:
                return {
                    "success": False,
                    "report_id": report_id,
                    "error": "Xandr report polling timed out before the report was ready.",
                    "status": last_status,
                }
            # await: time.sleep would block the FastMCP event loop, freezing
            # every other in-flight tool call on this server for the poll window.
            await asyncio.sleep(sleep_seconds)
    except Exception as e:
        return {"success": False, "error": f"Failed to run Xandr report: {e}"}


@mcp.tool()
async def xandr_run_curator_report(
    preset: str = "curator_revenue_summary",
    report_interval: str | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = DEFAULT_REPORT_POLL_TIMEOUT_SECONDS,
    poll_interval_seconds: float = DEFAULT_REPORT_POLL_INTERVAL_SECONDS,
) -> dict[str, Any]:
    """Run a Xandr curator preset report. Pass output_dir = workspace path."""
    preset_config = XANDR_CURATOR_REPORT_PRESETS.get(preset)
    if preset_config is None:
        return {
            "success": False,
            "error": f"Unknown Xandr reporting preset: {preset}. Valid presets: {sorted(XANDR_CURATOR_REPORT_PRESETS)}",
        }

    result = await xandr_run_report(
        report_type=preset_config["report_type"],
        columns=list(preset_config["columns"]),
        report_interval=report_interval or preset_config.get("report_interval"),
        start_date=start_date,
        end_date=end_date,
        filename_hint=filename_hint,
        output_dir=output_dir,
        poll_timeout_seconds=poll_timeout_seconds,
        poll_interval_seconds=poll_interval_seconds,
    )
    if result.get("success"):
        result["preset"] = preset
    return result


@mcp.tool()
async def xandr_run_curator_report_from_prompt_inputs(
    breakdowns: list[str] | None = None,
    metrics: list[str] | None = None,
    report_type: str | None = None,
    last_n_days: int = 30,
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = DEFAULT_REPORT_POLL_TIMEOUT_SECONDS,
    poll_interval_seconds: float = DEFAULT_REPORT_POLL_INTERVAL_SECONDS,
) -> dict[str, Any]:
    """Run a curator report from human-readable prompt inputs.

    Example inputs:
    - breakdowns: ["day", "deal", "buyer"]
    - metrics: ["impressions", "spend", "margin"]
    - report_type: "curator revenue"

    output_dir: Optional absolute path for the downloaded CSV. Pass the
        per-conversation workspace dir so other tools can read it.
    """
    selected_preset = _resolve_xandr_report_preset(report_type)
    if selected_preset and not breakdowns and not metrics:
        start_date, end_date = _resolve_xandr_relative_date_range(last_n_days)
        result = await xandr_run_curator_report(
            preset=selected_preset,
            start_date=start_date,
            end_date=end_date,
            filename_hint=filename_hint,
            output_dir=output_dir,
            poll_timeout_seconds=poll_timeout_seconds,
            poll_interval_seconds=poll_interval_seconds,
        )
        if result.get("success"):
            result["selected_preset"] = selected_preset
        return result

    resolved_dimensions = _resolve_xandr_fields(breakdowns, XANDR_REPORT_DIMENSION_ALIASES)
    resolved_metrics = _resolve_xandr_fields(metrics, XANDR_REPORT_METRIC_ALIASES)

    if not resolved_dimensions:
        resolved_dimensions = ["day", "curated_deal", "buyer_member_name"]
    if not resolved_metrics:
        resolved_metrics = ["imps", "curator_revenue", "curator_margin"]

    start_date, end_date = _resolve_xandr_relative_date_range(last_n_days)
    result = await xandr_run_report(
        report_type="curator_analytics",
        columns=resolved_dimensions + resolved_metrics,
        start_date=start_date,
        end_date=end_date,
        filename_hint=filename_hint,
        output_dir=output_dir,
        poll_timeout_seconds=poll_timeout_seconds,
        poll_interval_seconds=poll_interval_seconds,
    )
    if result.get("success"):
        result["resolved_breakdowns"] = resolved_dimensions
        result["resolved_metrics"] = resolved_metrics
        if selected_preset:
            result["selected_preset"] = selected_preset
    return result


@mcp.tool()
async def xandr_execute_deal_from_prompt_inputs(
    name: str,
    code: str,
    buyer: Any,
    insertion_order_name: str | None = None,
    insertion_order_id: int | None = None,
    deal_type: Any = "Curated",
    start_date: str | None = None,
    end_date: str | None = None,
    ask_price: float | None = None,
    revenue_type: str = "vcpm",
    revenue_value: float | None = None,
    margin_percent: float | None = None,
    margin_cpm: float | None = None,
    ad_types: list[str] | None = None,
    payment_type: str | None = None,
    currency: str = "USD",
    active: bool = True,
    use_deal_floor: bool = True,
    description: str | None = None,
    member_id: int | None = None,
    channel: str | None = None,
    device_types: list[Any] | None = None,
    geo_countries: list[Any] | None = None,
    geo_states: list[Any] | None = None,
    iab_categories: list[Any] | None = None,
    segment_names: list[Any] | None = None,
    deal_list_names: list[Any] | None = None,
) -> dict[str, Any]:
    """Run the full Microsoft Curate (Xandr) curated-deal workflow in one call.

    The Curate API requires THREE chained POSTs for a curated deal:
      1. POST `/deal` with `type.id=5` ("Curated") and `version=2` -> deal_id
      2. POST `/profile` with platform_*-prefixed targeting (when supplied)
         -> profile_id
      3. POST `/line-item` with `line_item_subtype="standard_curated"`,
         referencing deal_id, insertion_order_id, advertiser_id, profile_id;
         this is where `valuation.min_margin_pct` (or `min_margin_cpm`)
         lives -> line_item_id

    The line item is what actually shows up as a "deal" in the Curate UI.

    Insertion order is REQUIRED: the caller must pass either
    `insertion_order_name` (resolved against the seeded IO catalog) or
    `insertion_order_id` (numeric escape hatch). The MCP derives
    `advertiser_id` from the resolved IO so deals always land under the
    correct partner-specific advertiser.

    Curator margin defaults to 30% Percentage (matches the Elcano default
    on OpenX/IX/PubMatic/Media.net) when neither margin_percent nor
    margin_cpm is supplied. An `xandr_default_curator_margin_applied`
    quality flag is emitted to surface what was applied.

    Args:
        name: Human-readable deal name. Also used as the line-item name
            (with " (Elcano line item)" suffix).
        code: Unique deal code string.
        buyer: Buyer name (resolved via list_buyers) or numeric buyer id.
        insertion_order_name: IO name (matched against the seed catalog,
            case-insensitive, en-dash/hyphen tolerant). EITHER this OR
            insertion_order_id is required.
        insertion_order_id: Numeric IO id. Skips name resolution.
        deal_type: Xandr deal-type name or id. Defaults to "Curated" (id=5).
            Accepted: "Curated" (5) for curator deals, plus legacy
            "Open Auction" (1), "Private Auction" (2),
            "Programmatic Guaranteed" (3) — but only "Curated" actually
            shows in the Curate UI.
        start_date: "YYYY-MM-DD HH:MM:SS" or None for immediate.
        end_date: "YYYY-MM-DD HH:MM:SS" or None for always-on.
        ask_price: Floor price (CPM); used as `valuation.min_revenue_value`
            when revenue_type="vcpm".
        revenue_type: "vcpm" (Standard / Dynamic CPM, default) or "cpm"
            (Fixed Price). Curator standard is "vcpm".
        revenue_value: Required when revenue_type="cpm" — the fixed CPM rate.
        margin_percent: Curator margin as percentage of buyer bid. Mutually
            exclusive with margin_cpm. Defaults to 30% when both omitted.
        margin_cpm: Curator margin as fixed CPM deduction. Mutually exclusive.
        ad_types: ["banner"] / ["video"] / ["native"]. Defaults to derive
            from channel: display->banner, olv/ctv/ott->video.
        payment_type: "default" or "cpvm".
        currency: Currency code (default "USD").
        active: Whether the deal + line item start active. Default true.
        use_deal_floor: Whether the deal_floor is the operative floor.
        description: Optional description.
        member_id: Optional Xandr member id. Defaults to ELCANO_XANDR_MEMBER_ID
            when not supplied.
        channel: "display"/"olv"/"ctv"/"ott". When `device_types` is omitted,
            the canonical Xandr device set for that channel is auto-applied
            (display/olv -> Desktop+Phone+Tablet, ctv -> CTV+STB, ott ->
            Phone+Tablet). Also drives the default ad_types if not explicit.
        device_types: List of Xandr device-type names or numeric ids.
        geo_countries: List of country names, ISO-2 codes, or numeric ids.
        geo_states: List of region/state names or numeric ids.
        iab_categories: List of IAB content-category names, codes, or ids.
            Auto-uses the Curate-required `platform_content_category_targets`
            field on the profile.
        segment_names: List of audience segment names or numeric ids.
        deal_list_names: List of deal-list (inventory whitelist) names or ids.

    Returns:
        Dict with: success, deal_url, deal, verification, quality_flags,
        warnings, profile_id, line_item_id, advertiser_id,
        insertion_order_id, error.
    """
    logger.info("xandr_execute_deal_from_prompt_inputs called: %s", name)

    warnings: list[str] = []
    quality_flags: list[dict[str, Any]] = []

    resolved_member_id = int(member_id) if member_id is not None else ELCANO_XANDR_MEMBER_ID
    if start_date is None:
        # Xandr line-items require a budget_intervals[0].start_date. Default
        # to "now" in US/Eastern (the IO catalog timezone) when caller omits.
        start_date = datetime.now(UTC).strftime("%Y-%m-%d %H:%M:%S")

    # Insertion order is REQUIRED — the line-item create call needs both an
    # IO id and an advertiser id.
    if insertion_order_name is None and insertion_order_id is None:
        message = (
            "insertion_order_name or insertion_order_id is required — Xandr Curate deals "
            "live as line items under a partner-specific IO. Pass one of: a name from "
            "XANDR_INSERTION_ORDER_SEED_CATALOG (e.g. 'Marketplace Pro') or a numeric id."
        )
        quality_flags.append(_make_xandr_quality_flag("xandr_missing_insertion_order", message))
        return {
            "success": False,
            "phase": "validate",
            "deal_url": None,
            "deal": None,
            "verification": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": message,
        }

    # Resolve insertion order -> derive advertiser_id from the resolved IO
    io_input: Any = insertion_order_id if insertion_order_id is not None else insertion_order_name
    try:
        resolved_io = await _resolve_xandr_insertion_order(io_input)
    except XandrResolutionError as exc:
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_unresolved_insertion_order",
                exc.message,
                input=exc.details.get("input"),
                candidates=exc.details.get("candidates"),
            )
        )
        return {
            "success": False,
            "phase": "resolve",
            "deal_url": None,
            "deal": None,
            "verification": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": exc.message,
        }
    resolved_io_id = int(resolved_io["id"])
    resolved_advertiser_id = (
        int(resolved_io["advertiser_id"])
        if resolved_io.get("advertiser_id") is not None
        else ELCANO_XANDR_DEFAULT_ADVERTISER_ID
    )

    # Channel hint -> auto-fill device_types if not explicit
    canonical_channel = _normalize_xandr_channel_hint(channel)
    device_types, applied_channel_default = _apply_xandr_channel_device_defaults(device_types, channel)
    if applied_channel_default:
        warnings.append(
            f"Applied Xandr default device targeting for {canonical_channel} channel: {device_types}. "
            "Pass device_types= to override."
        )
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_default_channel_devices_applied",
                f"Auto-filled device_types from channel={canonical_channel!r}.",
                channel=canonical_channel,
                device_types=list(device_types or []),
            )
        )

    # ad_types: explicit > derived from channel
    resolved_ad_types = list(ad_types) if ad_types else _xandr_ad_types_from_channel(channel)

    # Resolve buyer
    try:
        resolved_buyer = await _resolve_xandr_buyer(buyer, member_id=member_id)
    except XandrResolutionError as exc:
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_unresolved_buyer",
                exc.message,
                input=exc.details.get("input"),
                candidates=exc.details.get("candidates"),
            )
        )
        return {
            "success": False,
            "phase": "resolve",
            "deal_url": None,
            "deal": None,
            "verification": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": exc.message,
        }

    # Resolve deal type
    try:
        resolved_deal_type = _resolve_xandr_deal_type(deal_type)
    except XandrResolutionError as exc:
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_unresolved_deal_type",
                exc.message,
                input=exc.details.get("input"),
                available=exc.details.get("available"),
            )
        )
        return {
            "success": False,
            "phase": "resolve",
            "deal_url": None,
            "deal": None,
            "verification": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": exc.message,
        }

    # Resolve targeting entities. Each unresolved value emits its own
    # quality_flag and the entity is dropped — partial targeting still
    # creates a deal so the trader can fix it post-hoc rather than blowing
    # up the entire run.
    resolved_device_type_ids: list[int] = []
    for device_value in device_types or []:
        try:
            resolved_device_type_ids.append(_resolve_xandr_device_type_id(device_value))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_device_type",
                    exc.message,
                    input=exc.details.get("input"),
                )
            )

    resolved_country_targets: list[dict[str, Any]] = []
    for country_value in geo_countries or []:
        try:
            resolved_country_targets.append(await _resolve_xandr_country(country_value))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_country",
                    exc.message,
                    input=exc.details.get("input"),
                    candidates=exc.details.get("candidates"),
                )
            )

    primary_country_id = resolved_country_targets[0].get("id") if resolved_country_targets else None
    resolved_region_targets: list[dict[str, Any]] = []
    for region_value in geo_states or []:
        try:
            resolved_region_targets.append(await _resolve_xandr_region(region_value, country_id=primary_country_id))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_region",
                    exc.message,
                    input=exc.details.get("input"),
                    country_id=primary_country_id,
                    candidates=exc.details.get("candidates"),
                )
            )

    resolved_iab_targets: list[dict[str, Any]] = []
    for iab_value in iab_categories or []:
        try:
            resolved_iab_targets.append(await _resolve_xandr_iab_category(iab_value))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_iab_category",
                    exc.message,
                    input=exc.details.get("input"),
                    candidates=exc.details.get("candidates"),
                )
            )

    resolved_segment_targets: list[dict[str, Any]] = []
    for segment_value in segment_names or []:
        try:
            resolved_segment_targets.append(await _resolve_xandr_segment(segment_value, member_id=member_id))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_segment",
                    exc.message,
                    input=exc.details.get("input"),
                    candidates=exc.details.get("candidates"),
                )
            )

    resolved_deal_list_targets: list[dict[str, Any]] = []
    for deal_list_value in deal_list_names or []:
        try:
            resolved_deal_list_targets.append(await _resolve_xandr_deal_list(deal_list_value, member_id=member_id))
        except XandrResolutionError as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_unresolved_deal_list",
                    exc.message,
                    input=exc.details.get("input"),
                    candidates=exc.details.get("candidates"),
                )
            )

    # Build + create profile if ANY targeting was resolved.
    #
    # Two failure modes worth distinguishing:
    #   (a) Targeting was requested but everything substantive failed to
    #       resolve (e.g., all IAB names came back unresolved AND no
    #       segments resolved AND no geo regions resolved). Building a
    #       profile from just default devices would advertise targeting
    #       that doesn't exist, so skip the create and emit a clear P0
    #       `xandr_no_targeting_attached` flag.
    #   (b) Targeting resolved fine but the /profile endpoint rejected
    #       the payload (no numeric id back). Same flag, separate cause.
    profile_id: int | None = None
    targeting_attached = False
    substantive_resolved_kinds = sum(
        1
        for kind in (
            resolved_country_targets,
            resolved_region_targets,
            resolved_iab_targets,
            resolved_segment_targets,
            resolved_deal_list_targets,
        )
        if kind
    )
    substantive_requested = any(
        bool(values) for values in (geo_countries, geo_states, iab_categories, segment_names, deal_list_names)
    )
    has_any_targeting = bool(resolved_device_type_ids) or substantive_resolved_kinds > 0

    if substantive_requested and substantive_resolved_kinds == 0:
        # Trader asked for substantive targeting; none of it resolved.
        # Don't paper over with a devices-only profile.
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_no_targeting_attached",
                "Substantive targeting was requested but none of it resolved against "
                "the Xandr catalog. Deal will be created WITHOUT a targeting profile — "
                "geo, IAB, segment, and deal-list filters are NOT in effect. See the "
                "individual xandr_unresolved_* flags for what to fix.",
            )
        )
    elif has_any_targeting:
        profile_payload = _build_xandr_profile_payload(
            name=f"{name} (Elcano profile)",
            member_id=member_id,
            device_type_ids=resolved_device_type_ids or None,
            country_targets=resolved_country_targets or None,
            region_targets=resolved_region_targets or None,
            content_category_targets=resolved_iab_targets or None,
            segment_targets=resolved_segment_targets or None,
            deal_list_targets=resolved_deal_list_targets or None,
        )
        try:
            client = get_xandr_client()
            profile_response = await client.create_profile(profile_payload)
            created_profile = (
                profile_response.get("profile")
                if isinstance(profile_response, dict) and isinstance(profile_response.get("profile"), dict)
                else profile_response
            )
            if isinstance(created_profile, dict) and isinstance(created_profile.get("id"), int):
                profile_id = int(created_profile["id"])
                targeting_attached = True
            else:
                quality_flags.append(
                    _make_xandr_quality_flag(
                        "xandr_no_targeting_attached",
                        "Profile create response did not include a numeric id; deal will "
                        "be created WITHOUT a targeting profile.",
                    )
                )
        except (httpx.HTTPStatusError, ValueError, RuntimeError) as exc:
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_no_targeting_attached",
                    f"Failed to create Xandr profile: {exc}. Deal will be created WITHOUT a targeting profile.",
                )
            )

    # ---- Step 4: POST /deal (Curated, version=2) ----
    # Note: profile_id does NOT live on the deal — it attaches via the
    # line item below. The deal payload only carries the curator/buyer
    # relationship and the deal-type metadata.
    deal_object: dict[str, Any] = {
        "name": name,
        "code": code,
        "type": resolved_deal_type,
        "version": XANDR_DEAL_VERSION_CURATED,
        "buyer": {"id": resolved_buyer["id"]},
        "active": bool(active),
        "currency": currency,
        "use_deal_floor": bool(use_deal_floor),
    }
    if resolved_buyer.get("name"):
        deal_object["buyer"]["name"] = resolved_buyer["name"]
    if start_date is not None:
        deal_object["start_date"] = start_date
    if end_date is not None:
        deal_object["end_date"] = end_date
    if ask_price is not None:
        deal_object["ask_price"] = float(ask_price)
    if payment_type is not None:
        deal_object["payment_type"] = payment_type
    if description is not None:
        deal_object["description"] = description
    deal_object["member_id"] = resolved_member_id

    if resolved_deal_type.get("id") != XANDR_DEAL_TYPE_ID_CURATED:
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_non_curate_deal_type",
                f"deal_type {resolved_deal_type.get('name')!r} is not 'Curated' (id=5). "
                "Microsoft Curate UI only displays deals with type.id=5; pass "
                "deal_type='Curated' for the standard curator workflow.",
                resolved_deal_type=resolved_deal_type,
            )
        )

    create_result = await create_xandr_deal({"deal": deal_object})
    if not create_result.get("success"):
        error_message = create_result.get("error") or "Xandr create_deal call failed."
        quality_flags.append(_make_xandr_quality_flag("xandr_create_call_failed", error_message))
        return {
            "success": False,
            "phase": "create",
            "deal_url": None,
            "deal": None,
            "verification": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": error_message,
        }

    created_deal = create_result.get("data") or {}
    if isinstance(created_deal, dict) and isinstance(created_deal.get("deal"), dict):
        created_deal = created_deal["deal"]
    created_deal_id = created_deal.get("id") if isinstance(created_deal, dict) else None

    # ---- Step 6: POST /line-item — carries the curator margin and ties
    # everything (deal + IO + profile) together. This is the resource that
    # actually shows up as a "deal" in the Curate UI. ----
    line_item_id: int | None = None
    if isinstance(created_deal_id, int):
        try:
            margin_type, margin_value, applied_margin_default = _resolve_xandr_curator_margin(
                margin_percent, margin_cpm
            )
        except ValueError as exc:
            quality_flags.append(_make_xandr_quality_flag("xandr_invalid_margin_inputs", str(exc)))
        else:
            if applied_margin_default:
                warnings.append(
                    f"Applied default Elcano curator margin: {ELCANO_DEFAULT_XANDR_MARGIN_PERCENT:g}% "
                    "(Percentage of buyer bid). Pass margin_percent= or margin_cpm= to override."
                )
                quality_flags.append(
                    _make_xandr_quality_flag(
                        "xandr_default_curator_margin_applied",
                        f"Auto-applied flat {ELCANO_DEFAULT_XANDR_MARGIN_PERCENT:g}% Percentage curator margin.",
                        margin_type=margin_type,
                        margin_value=margin_value,
                    )
                )

            line_item_payload = _build_xandr_line_item_payload(
                name=f"{name} (Elcano line item)",
                deal_id=created_deal_id,
                insertion_order_id=resolved_io_id,
                profile_id=profile_id,
                margin_type=margin_type,
                margin_value=margin_value,
                revenue_type=revenue_type,
                revenue_value=revenue_value,
                floor_price=ask_price,
                ad_types=resolved_ad_types,
                start_date=start_date,
                end_date=end_date,
                state="active" if active else "inactive",
            )
            try:
                client = get_xandr_client()
                line_item_response = await client.create_line_item(
                    line_item_payload,
                    advertiser_id=resolved_advertiser_id,
                    member_id=resolved_member_id,
                )
                created_line_item = (
                    line_item_response.get("line-item")
                    if isinstance(line_item_response, dict) and isinstance(line_item_response.get("line-item"), dict)
                    else line_item_response
                )
                if isinstance(created_line_item, dict) and isinstance(created_line_item.get("id"), int):
                    line_item_id = int(created_line_item["id"])
                else:
                    quality_flags.append(
                        _make_xandr_quality_flag(
                            "xandr_line_item_create_failed",
                            "Line-item create response did not include a numeric id; the deal "
                            "exists but won't appear as a serving line item in the Curate UI.",
                        )
                    )
            except (httpx.HTTPStatusError, ValueError, RuntimeError) as exc:
                quality_flags.append(
                    _make_xandr_quality_flag(
                        "xandr_line_item_create_failed",
                        f"Failed to create curated-deal line item: {exc}",
                    )
                )

    # ---- Verification re-fetch ----
    verification: dict[str, Any] | None = None
    if isinstance(created_deal_id, int):
        try:
            verification = await get_xandr_deal(created_deal_id)
            if isinstance(verification, dict) and not verification.get("success"):
                quality_flags.append(
                    _make_xandr_quality_flag(
                        "xandr_verification_failed",
                        verification.get("error") or "Xandr verification re-fetch failed.",
                        deal_id=created_deal_id,
                    )
                )
        except Exception as exc:
            verification = {"success": False, "error": f"Verification call failed: {exc}"}
            quality_flags.append(
                _make_xandr_quality_flag(
                    "xandr_verification_failed",
                    f"Verification call failed: {exc}",
                    deal_id=created_deal_id,
                )
            )
    else:
        quality_flags.append(
            _make_xandr_quality_flag(
                "xandr_verification_failed",
                "Create response did not include a numeric deal id.",
            )
        )

    # The Curate UI deep-links by line_item_id (not deal_id). When the
    # line-item create failed, line_item_id is None and the URL falls back
    # to None — better an empty link in the report than a broken one.
    deal_url = _build_xandr_deal_url(line_item_id)

    verify_succeeded = isinstance(verification, dict) and bool(verification.get("success"))
    return {
        "success": True,
        "phase": "verify" if verify_succeeded else "create",
        "deal_url": deal_url,
        "deal": created_deal,
        "profile_id": profile_id,
        "line_item_id": line_item_id,
        "advertiser_id": resolved_advertiser_id,
        "insertion_order_id": resolved_io_id,
        # Explicit signal so the email report can prominently surface
        # "deal is live but un-targeted" when profile creation didn't
        # land. The agent can also use this to downgrade Status to
        # COMPLETE_WITH_FLAGS even when the deal+line-item exist.
        "targeting_attached": targeting_attached,
        "verification": verification,
        "warnings": warnings,
        "quality_flags": quality_flags,
        "error": None,
    }


# =============================================================================
# Main Entry Point
# =============================================================================

if __name__ == "__main__":
    logger.info("Starting Xandr MCP Server")

    # Check for Xandr credentials
    has_credentials = bool(os.environ.get("XANDR_USERNAME")) and bool(os.environ.get("XANDR_PASSWORD"))

    if not has_credentials:
        logger.warning("Xandr not configured. Set XANDR_USERNAME and XANDR_PASSWORD to enable deal management.")

    try:
        # Use stdio transport (default for FastMCP)
        mcp.run(transport="stdio")
    except Exception as e:
        logger.error("Failed to start server: %s", e)
        sys.exit(1)
