#!/usr/bin/env python3
"""
Magnite MCP Server

A Model Context Protocol (MCP) server for the Magnite platform (formerly
Rubicon Project / Telaria), covering two API surfaces that share one set of
HTTP Basic credentials (MAGNITE_ACCESS_KEY / MAGNITE_SECRET_KEY):

1. DV+ Performance Analytics (reporting) — api.rubiconproject.com.
2. ClearLine Curation Demand Management (deal creation/management) —
   dmg.rubiconproject.com. Added with API guide v2.0 (June 2026); the
   Magnite rep confirmed the reporting keys/secrets are reused here.

Demand Management requests are routed per platform via the `source` query
parameter: "SpringServe" for CTV inventory, "DV+" for display/online video.

Runs within the Victoria Terminal container environment.
"""

import asyncio
import copy
import hashlib
import logging
import os
import re
import sys
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import httpx
from mcp.server.fastmcp import FastMCP

# Configure logging to stderr (not stdout for STDIO transport)
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger(__name__)

# Initialize FastMCP server
mcp = FastMCP("magnite_mcp")

# Constants
USER_AGENT = "victoria-terminal/1.0"
DEFAULT_TIMEOUT = 60.0

# DV+ Performance Analytics API Configuration. The legacy Seller-Platform
# REST API (api.tremorhub.com) was used for deal CRUD; deal management now
# goes through the ClearLine Curation Demand Management API below.
DEFAULT_DV_BASE_URL = "https://api.rubiconproject.com"
DEFAULT_DOWNLOAD_DIR = os.path.expanduser("~/Victoria/magnite_reports")

# ClearLine Curation Demand Management API (deal creation/management).
# QA/staging host is https://dmg-qa.rubiconproject.com — set
# MAGNITE_DMG_BASE_URL to point there during integration testing.
DEFAULT_DMG_BASE_URL = "https://dmg.rubiconproject.com"

# `source` routes Demand Management requests to the right Magnite platform.
# httpx percent-encodes the "+" in query params, which is exactly what the
# API requires (?source=DV%2B).
SOURCE_SPRINGSERVE = "SpringServe"
SOURCE_DVPLUS = "DV+"

# Channel/source aliases accepted from prompts. CTV inventory lives on
# SpringServe; display and online video (OLV) live on DV+.
MAGNITE_SOURCE_ALIASES: dict[str, str] = {
    "springserve": SOURCE_SPRINGSERVE,
    "ctv": SOURCE_SPRINGSERVE,
    "streaming": SOURCE_SPRINGSERVE,
    "dv+": SOURCE_DVPLUS,
    "dvplus": SOURCE_DVPLUS,
    "dv": SOURCE_DVPLUS,
    "display": SOURCE_DVPLUS,
    "olv": SOURCE_DVPLUS,
    "online video": SOURCE_DVPLUS,
    "web": SOURCE_DVPLUS,
}

MAGNITE_GEO_METADATA_KINDS = {"countries", "regions", "cities", "metro-areas"}
MAGNITE_TARGETING_LIST_KINDS = {"app-bundles", "domains"}
MAGNITE_PRICE_TYPES = {"Market Rate", "CPM", "Market Rate with Minimum"}
MAGNITE_REV_SHARE_MODELS = {"CPM", "Percent"}
MAGNITE_PRICE_BEHAVIORS = {"Auction", "Fixed"}

# Default curator rev share applied by the prompt-inputs flow when the
# caller does not specify one. The API guide's SpringServe example uses
# "revShareModel": "Percent" with value 0.25 for a 25% share, i.e. the
# Percent scale is a FRACTION. 0.30 = the flat 30% Elcano curator margin
# (mirrors the Media.net MCP default). Always surfaced as a quality flag.
DEFAULT_CURATOR_REV_SHARE_MODEL = "Percent"
DEFAULT_CURATOR_REV_SHARE_VALUE = 0.30

# Magnite scopes DV+ Performance Analytics requests by mp-vendor ID
# (`account=mp-vendor/<id>`). One operator login can carry multiple
# marketplaceVendor contexts. The numeric IDs below are the active
# Elcano-affiliated vendors as surfaced by the Magnite UI's /me endpoint;
# they are stored as strings because that is how the env var arrives and
# how the URL parameter is formatted downstream.
#
# To switch the default vendor per MCP variant subprocess, set the
# variant-suffixed env var (e.g. MAGNITE_ACCOUNT_ID_RAPTIVE=216). The
# Cutlass mcp loader rewrites it to MAGNITE_ACCOUNT_ID before the
# subprocess sees it, so the Python code just reads one env var.
MAGNITE_VENDOR_LABELS: dict[str, str] = {
    "27": "The Weather Company",
    "102": "Elcano",
    "216": "Raptive",
}

MAGNITE_REPORT_DIMENSION_ALIASES: dict[str, str] = {
    "day": "date",
    "date": "date",
    "daily": "date",
    "site": "site",
    "site domain": "site",
    "deal": "marketplace_deal_name",
    "deal name": "marketplace_deal_name",
    "dsp": "partner",
    "buyer": "partner",
    "country": "country",
}

MAGNITE_REPORT_METRIC_ALIASES: dict[str, str] = {
    "impressions": "paid_impression",
    "paid impressions": "paid_impression",
    "spend": "buyer_spend",
    "buyer spend": "buyer_spend",
    "revenue": "buyer_spend",
    "requests": "bid_requests",
    "bid requests": "bid_requests",
    "margin": "curator_rev_share",
    "fees": "curator_platform_fee",
    "total marketplace fees": "curator_platform_fee",
    "platform fee": "curator_platform_fee",
    "platform fee percentage": "curator_platform_fee_percentage",
    "cpm": "cpm",
    "bid responses": "bid_responses",
    "publisher gross revenue": "publisher_gross_revenue",
}

DATE_ONLY_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def _make_error(operation: str, status_code: int | None, message: str, details: Any = None) -> dict[str, Any]:
    """Create a normalized error dict."""
    err: dict[str, Any] = {
        "provider": "magnite",
        "operation": operation,
        "status_code": status_code,
        "message": message,
    }
    if details is not None:
        err["details"] = details
    return err


def _normalize_iso8601_datetime(value: str, field_name: str) -> str:
    """Normalize DV+ date inputs to timezone-aware ISO-8601 strings."""
    if DATE_ONLY_RE.fullmatch(value):
        return f"{value}T00:00:00Z"

    candidate = value.replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError as exc:
        raise ValueError(f"{field_name} must be ISO-8601 with timezone, for example 2026-03-01T00:00:00Z.") from exc

    if parsed.tzinfo is None:
        raise ValueError(f"{field_name} must include a timezone offset or Z suffix, for example 2026-03-01T00:00:00Z.")

    return value


def _normalize_source(value: str | None, field_name: str = "source") -> str:
    """Normalize a source/channel token to the canonical API value.

    Accepts the API values ("SpringServe", "DV+", case-insensitive) plus
    channel-style aliases ("ctv" -> SpringServe; "display"/"olv" -> DV+).
    """
    normalized = str(value or "").strip().lower()
    if not normalized:
        raise ValueError(f"{field_name} is required. Use 'SpringServe' (CTV) or 'DV+' (display / online video).")
    resolved = MAGNITE_SOURCE_ALIASES.get(normalized)
    if resolved is None:
        raise ValueError(
            f"Unsupported {field_name} {value!r}. Use 'SpringServe' (CTV) or 'DV+' (display / online video)."
        )
    return resolved


def _normalize_deal_end_date(value: str, field_name: str = "endDate") -> str:
    """Like _normalize_iso8601_datetime, but date-only inputs become end-of-day."""
    if DATE_ONLY_RE.fullmatch(value):
        return f"{value}T23:59:59Z"
    return _normalize_iso8601_datetime(value, field_name)


def _make_blocker(code: str, message: str, **details: Any) -> dict[str, Any]:
    """Structured blocker entry for the prepare/execute deal flow."""
    blocker: dict[str, Any] = {"code": code, "message": message}
    if details:
        blocker["details"] = details
    return blocker


def _make_quality_flag(flag: str, impact: str, **context: Any) -> dict[str, Any]:
    """Structured quality-flag entry rendered by protocol reports."""
    entry: dict[str, Any] = {"flag": flag, "impact": impact}
    if context:
        entry["context"] = context
    return entry


def _resolve_magnite_fields(requested_values: list[str] | None, alias_map: dict[str, str]) -> list[str]:
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


class MagniteClient:
    """
    Client for the Magnite APIs.

    Covers two surfaces behind the same HTTP Basic credentials
    (MAGNITE_ACCESS_KEY / MAGNITE_SECRET_KEY), scoped to MAGNITE_ACCOUNT_ID
    via the `account=mp-vendor/<id>` query parameter:

    - DV+ Performance Analytics reporting (MAGNITE_DV_BASE_URL).
    - ClearLine Curation Demand Management deal CRUD (MAGNITE_DMG_BASE_URL),
      where every call additionally carries a `source` parameter routing it
      to SpringServe (CTV) or DV+ (display / online video).
    """

    def __init__(self):
        self.access_key = os.environ.get("MAGNITE_ACCESS_KEY", "")
        self.secret_key = os.environ.get("MAGNITE_SECRET_KEY", "")
        self._http_client: httpx.AsyncClient | None = None
        self.dv_base_url = os.environ.get("MAGNITE_DV_BASE_URL", DEFAULT_DV_BASE_URL).rstrip("/")
        self.dmg_base_url = os.environ.get("MAGNITE_DMG_BASE_URL", DEFAULT_DMG_BASE_URL).rstrip("/")
        self.account_id = os.environ.get("MAGNITE_ACCOUNT_ID", "")
        # Always use the default — we used to honor MAGNITE_DOWNLOAD_DIR but
        # operators routinely shipped MacOS paths into a Linux deploy (e.g.
        # `MAGNITE_DOWNLOAD_DIR=/Users/...` from a developer's .bashrc) and
        # reports landed in a surprising place. The default is workable on
        # every platform and operators who really need a different dir can
        # pass output_dir per-call rather than poisoning the whole MCP via env.
        self._download_dir = DEFAULT_DOWNLOAD_DIR

    def _is_configured(self) -> bool:
        """Check if Magnite credentials are configured."""
        return bool(self.access_key) and bool(self.secret_key)

    async def _get_http_client(self) -> httpx.AsyncClient:
        """Get or create the HTTP client."""
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _dv_request(
        self,
        method: str,
        path: str,
        json_data: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
        stream: bool = False,
    ) -> httpx.Response | dict[str, Any]:
        """
        Execute an HTTP request against the Magnite DV+ Performance Analytics API.

        Uses HTTP Basic Authentication (API Key as username, API Secret as password).
        """
        if not self.access_key or not self.secret_key:
            raise ValueError("Magnite not configured. Set MAGNITE_ACCESS_KEY and MAGNITE_SECRET_KEY.")
        if not self.account_id:
            raise ValueError("Magnite DV+ account ID not configured. Set MAGNITE_ACCOUNT_ID.")

        client = await self._get_http_client()
        url = f"{self.dv_base_url}{path}"

        if params is None:
            params = {}
        params["account"] = f"mp-vendor/{self.account_id}"

        logger.info("Executing %s DV+ request: %s", method, path)

        response = await client.request(
            method.upper(),
            url,
            auth=(self.access_key, self.secret_key),
            json=json_data,
            params=params,
            headers={"User-Agent": USER_AGENT},
        )
        response.raise_for_status()

        if stream:
            return response
        return response.json()

    async def create_offline_report(self, criteria: dict[str, Any]) -> dict[str, Any]:
        logger.info("Creating DV+ offline report")
        return await self._dv_request("POST", "/analytics/v2/default", json_data={"criteria": criteria})

    async def check_report_status(self, report_id: int) -> dict[str, Any]:
        logger.info("Checking DV+ report status for report %d", report_id)
        return await self._dv_request("GET", f"/analytics/v2/default/{report_id}")

    async def list_reports(self, date_range: str = "today") -> dict[str, Any]:
        logger.info("Listing DV+ reports for date_range=%s", date_range)
        return await self._dv_request("GET", "/analytics/v2/default", params={"date_range": date_range})

    async def download_report_data(
        self,
        report_id: int,
        fmt: str = "json",
        page: int = 1,
        size: int = 50000,
    ) -> dict[str, Any] | httpx.Response:
        logger.info("Downloading DV+ report %d (format=%s, page=%d)", report_id, fmt, page)
        params = {"format": fmt, "page": page, "size": size}
        return await self._dv_request(
            "GET",
            f"/analytics/v2/default/{report_id}/data",
            params=params,
            stream=fmt == "csv",
        )

    # -------------------------------------------------------------------------
    # ClearLine Curation Demand Management API (deal creation/management)
    # -------------------------------------------------------------------------

    async def _dmg_request(
        self,
        method: str,
        path: str,
        source: str,
        json_data: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """
        Execute an HTTP request against the Magnite Demand Management API.

        Uses the same HTTP Basic credentials as the reporting surface, plus
        the required `account=mp-vendor/<id>` and `source` query parameters.
        """
        if not self.access_key or not self.secret_key:
            raise ValueError("Magnite not configured. Set MAGNITE_ACCESS_KEY and MAGNITE_SECRET_KEY.")
        if not self.account_id:
            raise ValueError("Magnite account ID not configured. Set MAGNITE_ACCOUNT_ID.")

        client = await self._get_http_client()
        url = f"{self.dmg_base_url}{path}"

        merged_params: dict[str, Any] = dict(params or {})
        merged_params["account"] = f"mp-vendor/{self.account_id}"
        merged_params["source"] = source

        logger.info("Executing %s DMG request: %s (source=%s)", method, path, source)

        response = await client.request(
            method.upper(),
            url,
            auth=(self.access_key, self.secret_key),
            json=json_data,
            params=merged_params,
            headers={"User-Agent": USER_AGENT},
        )
        response.raise_for_status()
        if not response.content:
            return {}
        return response.json()

    async def dmg_create_deal(self, source: str, payload: dict[str, Any]) -> dict[str, Any]:
        """POST /api/v1/deals — returns 201 + full deal details."""
        return await self._dmg_request("POST", "/api/v1/deals", source, json_data=payload)

    async def dmg_get_deal(self, deal_id: str, source: str) -> dict[str, Any]:
        """GET /api/v1/deals/{ID}"""
        return await self._dmg_request("GET", f"/api/v1/deals/{deal_id}", source)

    async def dmg_update_deal(self, deal_id: str, source: str, payload: dict[str, Any]) -> dict[str, Any]:
        """PUT /api/v1/deals/{ID} — partial update; returns full deal details."""
        return await self._dmg_request("PUT", f"/api/v1/deals/{deal_id}", source, json_data=payload)

    async def dmg_set_deal_status(self, deal_id: str, source: str, activate: bool) -> dict[str, Any]:
        """POST /api/v1/deals/{ID}/activate or /deactivate."""
        action = "activate" if activate else "deactivate"
        return await self._dmg_request("POST", f"/api/v1/deals/{deal_id}/{action}", source)

    async def dmg_list_marketplaces(self, source: str, marketplace_id: int | None = None) -> dict[str, Any]:
        """GET /api/v1/marketplaces (optionally /{ID})."""
        path = "/api/v1/marketplaces" if marketplace_id is None else f"/api/v1/marketplaces/{marketplace_id}"
        return await self._dmg_request("GET", path, source)

    async def dmg_list_dsps(self, source: str) -> dict[str, Any]:
        """GET /api/v1/dsps"""
        return await self._dmg_request("GET", "/api/v1/dsps", source)

    async def dmg_list_dsp_buyers(self, dsp_id: int, source: str) -> dict[str, Any]:
        """GET /api/v1/dsps/{ID}/buyers"""
        return await self._dmg_request("GET", f"/api/v1/dsps/{dsp_id}/buyers", source)

    async def dmg_list_publishers(
        self, source: str, marketplace_id: int, size_ids: list[int] | None = None
    ) -> dict[str, Any]:
        """GET /api/v1/publishers — publishers available in the marketplace."""
        params: dict[str, Any] = {"marketplaceId": marketplace_id}
        if size_ids:
            # sizeIds (DV+ only): comma-separated DV+ size IDs to filter publishers.
            params["sizeIds"] = ",".join(str(s) for s in size_ids)
        return await self._dmg_request("GET", "/api/v1/publishers", source, params=params)

    async def dmg_list_metadata(
        self, kind: str, source: str, size: int | None = None, search: str | None = None
    ) -> dict[str, Any]:
        """GET /api/v1/metadata/{kind} — countries/regions/cities/metro-areas/audience-segments."""
        params: dict[str, Any] = {}
        if size is not None:
            params["size"] = size
        if search:
            params["search"] = search
        return await self._dmg_request("GET", f"/api/v1/metadata/{kind}", source, params=params)

    async def dmg_list_ad_formats(self, marketplace_id: int) -> dict[str, Any]:
        """GET /api/v1/metadata/ad-formats — DV+ only."""
        return await self._dmg_request(
            "GET", "/api/v1/metadata/ad-formats", SOURCE_DVPLUS, params={"marketplaceId": marketplace_id}
        )

    async def dmg_list_targeting_lists(
        self, kind: str, source: str, size: int | None = None, search: str | None = None
    ) -> dict[str, Any] | list[dict[str, Any]]:
        """GET /api/v1/targeting-lists/{kind} — app-bundles or domains."""
        params: dict[str, Any] = {}
        if size is not None:
            params["size"] = size
        if search:
            params["search"] = search
        return await self._dmg_request("GET", f"/api/v1/targeting-lists/{kind}", source, params=params)

    async def dmg_create_rtd_signal(self, source: str, value: str) -> dict[str, Any]:
        """POST /api/v1/real-time-data-signals"""
        return await self._dmg_request("POST", "/api/v1/real-time-data-signals", source, json_data={"value": value})

    async def dmg_get_rtd_signal(self, rtd_id: int, source: str) -> dict[str, Any]:
        """GET /api/v1/real-time-data-signals/{RTD_ID}"""
        return await self._dmg_request("GET", f"/api/v1/real-time-data-signals/{rtd_id}", source)

    async def dmg_update_rtd_signal(self, rtd_id: int, source: str, value: str) -> dict[str, Any]:
        """PUT /api/v1/real-time-data-signals/{RTD_ID}"""
        return await self._dmg_request(
            "PUT", f"/api/v1/real-time-data-signals/{rtd_id}", source, json_data={"value": value}
        )

    async def dmg_list_rtd_signals(
        self, source: str, size: int | None = None, search: str | None = None
    ) -> dict[str, Any]:
        """GET /api/v1/real-time-data-signals"""
        params: dict[str, Any] = {}
        if size is not None:
            params["size"] = size
        if search:
            params["search"] = search
        return await self._dmg_request("GET", "/api/v1/real-time-data-signals", source, params=params)


# Global client instance
_magnite_client: MagniteClient | None = None


def get_magnite_client() -> MagniteClient:
    """Get or create the Magnite client singleton."""
    global _magnite_client
    if _magnite_client is None:
        _magnite_client = MagniteClient()
    return _magnite_client


# =============================================================================
# MCP Tools
# =============================================================================


@mcp.tool()
async def magnite_auth_status() -> dict[str, Any]:
    """
    Reports whether Magnite credentials are present in the environment.

    Magnite uses HTTP Basic Authentication on every request (both the DV+
    reporting surface and the ClearLine Demand Management deal surface share
    the same keys), so there is no separate "logged in" state to surface —
    `configured` is the only thing the agent needs. `has_account_id`
    confirms the mp-vendor scope is set (required for every call).

    Returns:
        configured: ACCESS_KEY and SECRET_KEY are both present.
        has_account_id: MAGNITE_ACCOUNT_ID is set.
        account_id: The configured mp-vendor ID as a string (or "" when unset).
        vendor_label: Human-readable label for the configured vendor when
            recognised (e.g. "Elcano", "The Weather Company", "Raptive"),
            or null when account_id is unset or refers to an unknown vendor.
        dv_base_url: Reporting API host.
        dmg_base_url: Demand Management (deal CRUD) API host.
    """
    client = get_magnite_client()
    return {
        "configured": client._is_configured(),
        "has_account_id": bool(client.account_id),
        "account_id": client.account_id,
        "vendor_label": MAGNITE_VENDOR_LABELS.get(client.account_id) if client.account_id else None,
        "dv_base_url": client.dv_base_url,
        "dmg_base_url": client.dmg_base_url,
    }


@mcp.tool()
async def magnite_create_offline_report(
    dimensions: list[str],
    metrics: list[str],
    date_range: str | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    filters: str | None = None,
    timezone: str | None = None,
    currency: str = "USD",
    limit: int = 500000,
) -> dict[str, Any]:
    """
    Creates a new offline report on the Magnite DV+ Performance Analytics API.

    The report is queued for asynchronous generation. Use magnite_check_report_status
    to poll until the status is "success", then use magnite_download_report to retrieve data.

    You must provide either date_range OR both start_date and end_date.
    """
    if not date_range and not (start_date and end_date):
        return {
            "success": False,
            "error": _make_error(
                "create_offline_report",
                None,
                "Either date_range or both start_date and end_date must be provided.",
            ),
        }

    normalized_start_date = start_date
    normalized_end_date = end_date
    if not date_range:
        try:
            normalized_start_date = _normalize_iso8601_datetime(start_date, "start_date")
            normalized_end_date = _normalize_iso8601_datetime(end_date, "end_date")
        except ValueError as e:
            return {"success": False, "error": _make_error("create_offline_report", None, str(e))}

    criteria: dict[str, Any] = {
        "dimension": ",".join(dimensions),
        "metric": ",".join(metrics),
        "limit": limit,
        "currency": currency,
        "timezone": timezone,
        "filters": filters,
    }

    if date_range:
        criteria["date_range"] = date_range
        criteria["start"] = None
        criteria["end"] = None
    else:
        criteria["start"] = normalized_start_date
        criteria["end"] = normalized_end_date
        criteria["date_range"] = None

    try:
        client = get_magnite_client()
        result = await client.create_offline_report(criteria)
        logger.info("Offline report created: %s", result.get("offline_report_id"))
        return {
            "success": True,
            "offline_report_id": result.get("offline_report_id"),
            "status": result.get("status"),
            "created": result.get("created"),
            "updated": result.get("updated"),
        }
    except ValueError as e:
        return {"success": False, "error": _make_error("create_offline_report", None, str(e))}
    except httpx.HTTPStatusError as e:
        details = None
        try:
            details = e.response.text
        except Exception:  # noqa: BLE001 - body capture is best-effort
            details = None
        return {
            "success": False,
            "error": _make_error(
                "create_offline_report", e.response.status_code, f"HTTP {e.response.status_code}", details
            ),
        }
    except Exception as e:
        return {"success": False, "error": _make_error("create_offline_report", None, str(e))}


@mcp.tool()
async def magnite_check_report_status(report_id: int) -> dict[str, Any]:
    """
    Checks the status of a Magnite DV+ offline report.

    Possible statuses: "queued", "pending", "success", "error", "canceled".
    Poll this endpoint until status is "success" before downloading data.
    """
    try:
        client = get_magnite_client()
        result = await client.check_report_status(report_id)
        return {
            "success": True,
            "offline_report_id": result.get("offline_report_id"),
            "status": result.get("status"),
            "created": result.get("created"),
            "updated": result.get("updated"),
        }
    except ValueError as e:
        return {"success": False, "error": _make_error("check_report_status", None, str(e))}
    except httpx.HTTPStatusError as e:
        details = None
        try:
            details = e.response.text
        except Exception:  # noqa: BLE001 - body capture is best-effort
            details = None
        return {
            "success": False,
            "error": _make_error(
                "check_report_status", e.response.status_code, f"HTTP {e.response.status_code}", details
            ),
        }
    except Exception as e:
        return {"success": False, "error": _make_error("check_report_status", None, str(e))}


@mcp.tool()
async def magnite_list_reports(date_range: str = "today") -> dict[str, Any]:
    """
    Lists all offline reports for the Magnite DV+ account.

    Args:
        date_range: Filter by date range. Valid values include "today", "yesterday", "week_to_date",
            "month_to_date", "year_to_date", "last_3", "last_7", "last_14", "last_30",
            "prior_week", and "prior_month".
    """
    try:
        client = get_magnite_client()
        result = await client.list_reports(date_range)
        return {
            "success": True,
            "reports": result.get("content", []),
            "page": result.get("page"),
        }
    except ValueError as e:
        return {"success": False, "error": _make_error("list_reports", None, str(e))}
    except httpx.HTTPStatusError as e:
        details = None
        try:
            details = e.response.json()
        except Exception:
            details = e.response.text
        return {
            "success": False,
            "error": _make_error("list_reports", e.response.status_code, f"HTTP {e.response.status_code}", details),
        }
    except Exception as e:
        return {"success": False, "error": _make_error("list_reports", None, str(e))}


@mcp.tool()
async def magnite_download_report(
    report_id: int,
    format: str = "json",
    page: int = 1,
    size: int = 50000,
    output_dir: str | None = None,
) -> dict[str, Any]:
    """
    Downloads data from a completed Magnite DV+ offline report.

    The report must have status "success" (check with magnite_check_report_status first).
    If the report is not ready, the API returns HTTP 409.

    For CSV format, the file is saved to disk and the path is returned.
    For JSON format, the data is returned inline.

    output_dir: Optional absolute path the CSV is written into. Pass the
        per-conversation workspace dir so other tools can read the file.
        Defaults to ~/Victoria/magnite_reports for local use; on a hardened
        production host that path is read-only.
    """
    try:
        client = get_magnite_client()
        result = await client.download_report_data(report_id, fmt=format, page=page, size=size)

        if format == "csv" and isinstance(result, httpx.Response):
            download_dir = Path(output_dir) if output_dir else Path(client._download_dir)
            download_dir = download_dir.expanduser()
            download_dir.mkdir(parents=True, exist_ok=True)
            filepath = download_dir / f"magnite_report_{report_id}_page{page}.csv"

            content = result.content
            sha256 = hashlib.sha256(content).hexdigest()
            filepath.write_bytes(content)

            logger.info("Saved report %d to %s (%d bytes)", report_id, filepath, len(content))
            return {
                "success": True,
                "path": str(filepath),
                "bytes": len(content),
                "sha256": sha256,
                "content_type": result.headers.get("content-type", "text/csv"),
            }

        return {
            "success": True,
            "content": result.get("content", []),
            "page": result.get("page"),
        }

    except ValueError as e:
        return {"success": False, "error": _make_error("download_report", None, str(e))}
    except httpx.HTTPStatusError as e:
        detail = f"HTTP {e.response.status_code}"
        if e.response.status_code == 409:
            detail = "Report not ready. Poll magnite_check_report_status until status is 'success'."
        return {"success": False, "error": _make_error("download_report", e.response.status_code, detail)}
    except Exception as e:
        return {"success": False, "error": _make_error("download_report", None, str(e))}


@mcp.tool()
async def magnite_run_report_from_prompt_inputs(
    breakdowns: list[str] | None = None,
    metrics: list[str] | None = None,
    date_range: str | None = "last_3",
    start_date: str | None = None,
    end_date: str | None = None,
    timezone: str | None = None,
    currency: str = "USD",
    limit: int = 500000,
    download: bool = True,
    filename_hint: str | None = None,
    output_dir: str | None = None,
    poll_timeout_seconds: float = 180.0,
    poll_interval_seconds: float = 5.0,
) -> dict[str, Any]:
    """Run a Magnite DV+ report from human-readable prompt inputs.

    Example inputs:
    - breakdowns: ["day", "deal", "DSP"]
    - metrics: ["impressions", "spend", "margin"]

    output_dir: Optional absolute path to write the CSV into. Pass the
        per-conversation workspace dir so the agent's other tools can
        read it.
    """
    resolved_dimensions = _resolve_magnite_fields(breakdowns, MAGNITE_REPORT_DIMENSION_ALIASES)
    resolved_metrics = _resolve_magnite_fields(metrics, MAGNITE_REPORT_METRIC_ALIASES)

    if not resolved_dimensions:
        resolved_dimensions = ["date", "marketplace_deal_name", "partner"]
    if not resolved_metrics:
        resolved_metrics = ["paid_impression", "buyer_spend", "curator_platform_fee"]

    created = await magnite_create_offline_report(
        dimensions=resolved_dimensions,
        metrics=resolved_metrics,
        date_range=date_range,
        start_date=start_date,
        end_date=end_date,
        timezone=timezone,
        currency=currency,
        limit=limit,
    )
    if not created.get("success"):
        return created

    report_id = created.get("offline_report_id")
    if report_id is None:
        return {"success": False, "error": _make_error("run_report_from_prompt_inputs", None, "No report ID returned.")}

    status = await magnite_check_report_status(report_id)
    if not status.get("success"):
        return status

    deadline = time.time() + max(poll_timeout_seconds, 0)
    sleep_seconds = max(poll_interval_seconds, 0.1)
    while str(status.get("status", "")).lower() in {"queued", "pending"}:
        if time.time() >= deadline:
            return {
                "success": False,
                "offline_report_id": report_id,
                "status": status.get("status"),
                "error": _make_error(
                    "run_report_from_prompt_inputs",
                    None,
                    "Magnite report polling timed out before status became 'success'.",
                ),
            }
        # await: time.sleep would block the FastMCP event loop, freezing
        # every other in-flight tool call on this server for the poll window.
        await asyncio.sleep(sleep_seconds)
        status = await magnite_check_report_status(report_id)
        if not status.get("success"):
            return status

    if str(status.get("status", "")).lower() != "success":
        return {
            "success": False,
            "offline_report_id": report_id,
            "status": status.get("status"),
            "error": _make_error(
                "run_report_from_prompt_inputs",
                None,
                f"Magnite report finished with status '{status.get('status')}'.",
            ),
        }

    downloaded = await magnite_download_report(
        report_id=report_id,
        format="csv" if download else "json",
        output_dir=output_dir,
    )
    if not downloaded.get("success"):
        return downloaded

    response = {
        "success": True,
        "offline_report_id": report_id,
        "status": status.get("status"),
        "resolved_breakdowns": resolved_dimensions,
        "resolved_metrics": resolved_metrics,
    }
    if download:
        response["download"] = downloaded
    else:
        response.update(downloaded)
    if filename_hint and download and response.get("download", {}).get("path"):
        response["filename_hint"] = filename_hint
    return response


# =============================================================================
# Demand Management (ClearLine Curation) — deal CRUD + reference data
# =============================================================================


def _dmg_exception_error(operation: str, exc: Exception) -> dict[str, Any]:
    """Normalize an exception from a Demand Management call into the error dict shape."""
    if isinstance(exc, httpx.HTTPStatusError):
        try:
            details: Any = exc.response.json()
        except Exception:  # noqa: BLE001 - body capture is best-effort
            details = exc.response.text
        return _make_error(operation, exc.response.status_code, f"HTTP {exc.response.status_code}", details)
    return _make_error(operation, None, str(exc))


def _dmg_page_content(result: Any) -> list[dict[str, Any]]:
    """Unwrap a paginated {page, content} response; targeting lists return bare arrays."""
    if isinstance(result, list):
        return result
    if isinstance(result, dict):
        content = result.get("content")
        if isinstance(content, list):
            return content
    return []


def _validate_deal_payload_minimums(payload: dict[str, Any]) -> list[str]:
    """Check the API guide's minimal creation requirements: deal name, flight
    dates, at least one DSP (with at least one buyer) and one publisher+pricing."""
    issues: list[str] = []
    if not str(payload.get("name") or "").strip():
        issues.append("name is required")
    if not (payload.get("marketplace") or {}).get("id"):
        issues.append("marketplace.id is required (immutable after creation)")
    if not payload.get("startDate"):
        issues.append("startDate is required (ISO-8601)")
    if not payload.get("endDate"):
        issues.append("endDate is required (ISO-8601)")
    dsps = payload.get("dsps")
    if not isinstance(dsps, list) or not dsps:
        issues.append("dsps is required (at least one DSP)")
    else:
        for i, dsp in enumerate(dsps):
            buyers = dsp.get("buyers") if isinstance(dsp, dict) else None
            if not isinstance(buyers, list) or not buyers:
                issues.append(f"dsps[{i}].buyers is required (at least one buyer per DSP)")
    pricing = payload.get("curatorPricing")
    if not isinstance(pricing, dict):
        issues.append("curatorPricing is required")
    else:
        groups = pricing.get("publisherRevShares")
        if not isinstance(groups, list) or not groups:
            issues.append("curatorPricing.publisherRevShares is required (at least one group)")
        else:
            for i, group in enumerate(groups):
                publishers = group.get("publishers") if isinstance(group, dict) else None
                if not isinstance(publishers, list) or not publishers:
                    issues.append(f"curatorPricing.publisherRevShares[{i}].publishers requires at least one publisher")
    return issues


@mcp.tool()
async def magnite_create_deal(source: str, payload: dict[str, Any]) -> dict[str, Any]:
    """
    Create a ClearLine Curation deal on Magnite (POST /api/v1/deals).

    This is a CRITICAL action that requires self-audit confirmation before
    execution. Prefer magnite_execute_deal_from_prompt_inputs, which resolves
    names to IDs server-side; this low-level tool submits a raw API payload.

    Args:
        source: "SpringServe" (CTV) or "DV+" (display / online video).
            Channel aliases ("ctv", "display", "olv") are accepted.
        payload: Raw Demand Management deal payload. Minimal requirements:
            name, marketplace.id, startDate, endDate, dsps (>=1 DSP with >=1
            buyer each), curatorPricing with >=1 publisherRevShares group of
            >=1 publisher. `type` defaults to "Curator" (the only allowed
            value). `id`, `status`, `source`, and buyer `code` are read-only
            (system generated); all deals are created Active.

    Date fields use ISO-8601; bare YYYY-MM-DD inputs are normalized to
    start-of-day (startDate) / end-of-day (endDate) UTC.
    """
    logger.info("magnite_create_deal called")
    try:
        resolved_source = _normalize_source(source)
        payload_copy = copy.deepcopy(payload) if payload else {}
        payload_copy.setdefault("type", "Curator")
        if payload_copy.get("startDate"):
            payload_copy["startDate"] = _normalize_iso8601_datetime(str(payload_copy["startDate"]), "startDate")
        if payload_copy.get("endDate"):
            payload_copy["endDate"] = _normalize_deal_end_date(str(payload_copy["endDate"]), "endDate")

        issues = _validate_deal_payload_minimums(payload_copy)
        if issues:
            return {
                "success": False,
                "error": _make_error("create_deal", None, "Payload fails minimal requirements", issues),
            }

        client = get_magnite_client()
        deal = await client.dmg_create_deal(resolved_source, payload_copy)
        logger.info("Magnite deal created: %s", deal.get("id"))
        return {"success": True, "deal": deal, "deal_id": deal.get("id")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("create_deal", e)}


@mcp.tool()
async def magnite_get_deal(deal_id: str, source: str) -> dict[str, Any]:
    """
    Retrieve a ClearLine Curation deal by ID (GET /api/v1/deals/{ID}).

    Note: a "list all deals" endpoint does not exist yet — the API guide
    slates it for v3.0. Until then deals are retrieved individually by the
    DEAL_ID returned at creation (e.g. "MGNI-CD-2002-100").
    """
    logger.info("magnite_get_deal called for deal_id=%s", deal_id)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        deal = await client.dmg_get_deal(deal_id, resolved_source)
        return {"success": True, "deal": deal}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("get_deal", e)}


@mcp.tool()
async def magnite_update_deal(deal_id: str, source: str, payload: dict[str, Any]) -> dict[str, Any]:
    """
    Update a ClearLine Curation deal (PUT /api/v1/deals/{ID}).

    Partial-update semantics — send only the fields you want to change, BUT
    component-level replacement applies: updating a component (e.g.
    targeting.audience or curatorPricing) replaces that ENTIRE object, while
    sibling components are left untouched. To change one value inside
    geography you must send the whole geography object. To delete a section
    send it as {} or null (explicit nullification), e.g. "contextual": null.

    marketplace.id is immutable; id/status/source/buyer code are read-only.
    Use magnite_activate_deal / magnite_deactivate_deal for status changes.
    """
    logger.info("magnite_update_deal called for deal_id=%s", deal_id)
    try:
        resolved_source = _normalize_source(source)
        payload_copy = copy.deepcopy(payload) if payload else {}
        if not payload_copy:
            return {"success": False, "error": _make_error("update_deal", None, "payload is required")}
        if payload_copy.get("startDate"):
            payload_copy["startDate"] = _normalize_iso8601_datetime(str(payload_copy["startDate"]), "startDate")
        if payload_copy.get("endDate"):
            payload_copy["endDate"] = _normalize_deal_end_date(str(payload_copy["endDate"]), "endDate")
        client = get_magnite_client()
        deal = await client.dmg_update_deal(deal_id, resolved_source, payload_copy)
        return {"success": True, "deal": deal}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("update_deal", e)}


@mcp.tool()
async def magnite_activate_deal(deal_id: str, source: str) -> dict[str, Any]:
    """Activate a ClearLine Curation deal (POST /api/v1/deals/{ID}/activate)."""
    logger.info("magnite_activate_deal called for deal_id=%s", deal_id)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_set_deal_status(deal_id, resolved_source, activate=True)
        return {"success": True, "deal": result}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("activate_deal", e)}


@mcp.tool()
async def magnite_deactivate_deal(deal_id: str, source: str) -> dict[str, Any]:
    """Deactivate a ClearLine Curation deal (POST /api/v1/deals/{ID}/deactivate)."""
    logger.info("magnite_deactivate_deal called for deal_id=%s", deal_id)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_set_deal_status(deal_id, resolved_source, activate=False)
        return {"success": True, "deal": result}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("deactivate_deal", e)}


@mcp.tool()
async def magnite_list_marketplaces(source: str, marketplace_id: int | None = None) -> dict[str, Any]:
    """
    List ClearLine marketplaces owned by the calling curator (GET /api/v1/marketplaces).

    A Curation account has at least one marketplace; the marketplace ID is a
    required deal-creation input (immutable after creation). Pass
    marketplace_id to fetch a single marketplace's details.
    """
    logger.info("magnite_list_marketplaces called (source=%s)", source)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_marketplaces(resolved_source, marketplace_id)
        if marketplace_id is not None:
            return {"success": True, "marketplace": result}
        return {"success": True, "marketplaces": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_marketplaces", e)}


@mcp.tool()
async def magnite_list_dsps(source: str) -> dict[str, Any]:
    """List DSPs available on the given source (GET /api/v1/dsps), sorted by ID."""
    logger.info("magnite_list_dsps called (source=%s)", source)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_dsps(resolved_source)
        return {"success": True, "dsps": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_dsps", e)}


@mcp.tool()
async def magnite_list_dsp_buyers(dsp_id: int, source: str) -> dict[str, Any]:
    """
    List buyers associated with a DSP (GET /api/v1/dsps/{ID}/buyers).

    The buyer `code` field (the "seat token") is read-only and generated by
    the system on the deal's DSP/buyer association.
    """
    logger.info("magnite_list_dsp_buyers called (dsp_id=%d, source=%s)", dsp_id, source)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_dsp_buyers(dsp_id, resolved_source)
        return {"success": True, "buyers": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_dsp_buyers", e)}


@mcp.tool()
async def magnite_list_publishers(
    source: str, marketplace_id: int, size_ids: list[int] | None = None
) -> dict[str, Any]:
    """
    List publishers available in a marketplace (GET /api/v1/publishers).

    Each publisher carries `minimumPriceFloor` ({cpm, currency}) — the
    minimum price floor for that publisher within the marketplace. Honor it
    when setting CPM pricing. size_ids (DV+ only) filters publishers by
    DV+ size IDs.
    """
    logger.info("magnite_list_publishers called (source=%s, marketplace_id=%d)", source, marketplace_id)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_publishers(resolved_source, marketplace_id, size_ids)
        return {"success": True, "publishers": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_publishers", e)}


@mcp.tool()
async def magnite_list_audience_segments(
    source: str, size: int | None = None, search: str | None = None
) -> dict[str, Any]:
    """
    List audience segments (GET /api/v1/metadata/audience-segments).

    ONLY available for source=SpringServe (CTV) today. DV+ audience segments
    are slated for API v3.0 (Magnite's Audience-platform upgrade, ETA end of
    June 2026 per our rep) — a DV+ call returns a structured error until then.

    size: page size (default 50, max 1000). search: min 2 characters.
    """
    logger.info("magnite_list_audience_segments called (source=%s)", source)
    try:
        resolved_source = _normalize_source(source)
        if resolved_source != SOURCE_SPRINGSERVE:
            return {
                "success": False,
                "error": _make_error(
                    "list_audience_segments",
                    None,
                    "Audience segments are only available for source=SpringServe today. "
                    "DV+ audience support arrives with API v3.0 (ETA end of June 2026).",
                ),
            }
        client = get_magnite_client()
        result = await client.dmg_list_metadata("audience-segments", resolved_source, size, search)
        return {"success": True, "segments": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_audience_segments", e)}


@mcp.tool()
async def magnite_list_geo_values(
    kind: str, source: str, size: int | None = None, search: str | None = None
) -> dict[str, Any]:
    """
    List geo targeting values (GET /api/v1/metadata/{kind}).

    kind: one of "countries", "regions", "cities", "metro-areas".
    Entries are {value, label} pairs (e.g. {"value": "US-AL", "label":
    "Alabama (US-AL)"}) used verbatim in targeting.geography.

    Pagination caveat from the API guide: geo metadata returns only the
    first page (max 1000 results) — use `search` (min 2 chars) to narrow
    regions/cities/metros instead of paging.
    """
    logger.info("magnite_list_geo_values called (kind=%s, source=%s)", kind, source)
    try:
        normalized_kind = kind.strip().lower()
        if normalized_kind not in MAGNITE_GEO_METADATA_KINDS:
            return {
                "success": False,
                "error": _make_error(
                    "list_geo_values", None, f"kind must be one of: {', '.join(sorted(MAGNITE_GEO_METADATA_KINDS))}"
                ),
            }
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_metadata(normalized_kind, resolved_source, size, search)
        return {"success": True, "values": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_geo_values", e)}


@mcp.tool()
async def magnite_list_ad_formats(marketplace_id: int) -> dict[str, Any]:
    """
    List ad formats and sizes for a marketplace (GET /api/v1/metadata/ad-formats).

    DV+ only — ad formats do not apply to SpringServe. Constraints when
    using the returned size IDs on a deal: only one format per deal, max 15
    sizes, and mixing sizes of different format types (Video, Display, ...)
    is not allowed. For Audio deals pass `feedTypes` (feed type + sizes per
    feed) instead of `sizes`.
    """
    logger.info("magnite_list_ad_formats called (marketplace_id=%d)", marketplace_id)
    try:
        client = get_magnite_client()
        result = await client.dmg_list_ad_formats(marketplace_id)
        return {"success": True, "ad_formats": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_ad_formats", e)}


@mcp.tool()
async def magnite_list_targeting_lists(
    kind: str, source: str, size: int | None = None, search: str | None = None
) -> dict[str, Any]:
    """
    List curator-owned targeting lists (GET /api/v1/targeting-lists/{kind}).

    kind: "app-bundles" or "domains". The returned IDs go into
    targeting.appBundleList / targeting.domainList as
    {"type": "Allow"|"Block", "values": [{"id": <id>}]}.
    """
    logger.info("magnite_list_targeting_lists called (kind=%s, source=%s)", kind, source)
    try:
        normalized_kind = kind.strip().lower()
        if normalized_kind not in MAGNITE_TARGETING_LIST_KINDS:
            return {
                "success": False,
                "error": _make_error(
                    "list_targeting_lists",
                    None,
                    f"kind must be one of: {', '.join(sorted(MAGNITE_TARGETING_LIST_KINDS))}",
                ),
            }
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_targeting_lists(normalized_kind, resolved_source, size, search)
        return {"success": True, "lists": _dmg_page_content(result)}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_targeting_lists", e)}


@mcp.tool()
async def magnite_create_rtd_signal(source: str, value: str) -> dict[str, Any]:
    """
    Create an RTD (real-time data) signal (POST /api/v1/real-time-data-signals).

    Supported on both DV+ and SpringServe — pass the matching source. `value`
    is the only writable field. The returned {id, value} is referenced from
    deal targeting under targeting.contextual.rtdSignals.
    """
    logger.info("magnite_create_rtd_signal called (source=%s)", source)
    try:
        resolved_source = _normalize_source(source)
        if not str(value or "").strip():
            return {"success": False, "error": _make_error("create_rtd_signal", None, "value is required")}
        client = get_magnite_client()
        signal = await client.dmg_create_rtd_signal(resolved_source, value)
        return {"success": True, "signal": signal}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("create_rtd_signal", e)}


@mcp.tool()
async def magnite_get_rtd_signal(rtd_id: int, source: str) -> dict[str, Any]:
    """Retrieve an RTD signal by ID (GET /api/v1/real-time-data-signals/{RTD_ID})."""
    logger.info("magnite_get_rtd_signal called (rtd_id=%d)", rtd_id)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        signal = await client.dmg_get_rtd_signal(rtd_id, resolved_source)
        return {"success": True, "signal": signal}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("get_rtd_signal", e)}


@mcp.tool()
async def magnite_update_rtd_signal(rtd_id: int, source: str, value: str) -> dict[str, Any]:
    """Update an RTD signal's value (PUT /api/v1/real-time-data-signals/{RTD_ID})."""
    logger.info("magnite_update_rtd_signal called (rtd_id=%d)", rtd_id)
    try:
        resolved_source = _normalize_source(source)
        if not str(value or "").strip():
            return {"success": False, "error": _make_error("update_rtd_signal", None, "value is required")}
        client = get_magnite_client()
        signal = await client.dmg_update_rtd_signal(rtd_id, resolved_source, value)
        return {"success": True, "signal": signal}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("update_rtd_signal", e)}


@mcp.tool()
async def magnite_list_rtd_signals(source: str, size: int | None = None, search: str | None = None) -> dict[str, Any]:
    """List RTD signals (GET /api/v1/real-time-data-signals). search: min 2 chars."""
    logger.info("magnite_list_rtd_signals called (source=%s)", source)
    try:
        resolved_source = _normalize_source(source)
        client = get_magnite_client()
        result = await client.dmg_list_rtd_signals(resolved_source, size, search)
        return {"success": True, "signals": _dmg_page_content(result), "page": result.get("page")}
    except Exception as e:
        return {"success": False, "error": _dmg_exception_error("list_rtd_signals", e)}


# =============================================================================
# Prepare / Submit / Execute — the agent-facing deal-creation flow
# =============================================================================

# In-memory prepared-deal artifacts, keyed by prepared_deal_id. Consume-once:
# once a create POST succeeds the artifact is marked created and replays the
# recorded outcome instead of double-creating a live deal.
_prepared_magnite_deals: dict[str, dict[str, Any]] = {}


def _normalize_lookup_text(value: Any) -> str:
    return str(value or "").strip().lower()


def _match_catalog_item(items: list[dict[str, Any]], raw: Any, *, name_key: str = "name") -> dict[str, Any] | None:
    """Match a name-or-id token against a Demand Management catalog listing.

    Numeric tokens match `id` exactly; strings match `name_key`
    case-insensitively, falling back to a UNIQUE case-insensitive substring.
    Returns None when nothing (or more than one substring candidate) matches.
    """
    if isinstance(raw, bool):
        return None
    if isinstance(raw, int) or (isinstance(raw, str) and raw.strip().isdigit()):
        wanted_id = int(raw)
        for item in items:
            if item.get("id") == wanted_id:
                return item
        return None
    wanted = _normalize_lookup_text(raw)
    if not wanted:
        return None
    for item in items:
        if _normalize_lookup_text(item.get(name_key)) == wanted:
            return item
    substring_hits = [item for item in items if wanted in _normalize_lookup_text(item.get(name_key))]
    if len(substring_hits) == 1:
        return substring_hits[0]
    return None


def _match_geo_value(items: list[dict[str, Any]], raw: Any) -> dict[str, Any] | None:
    """Match a geo token against {value, label} metadata entries.

    Matches `value` exactly (case-insensitive, e.g. "US"), then `label`
    exactly, then a unique label substring ("United States").
    """
    wanted = _normalize_lookup_text(raw)
    if not wanted:
        return None
    for item in items:
        if _normalize_lookup_text(item.get("value")) == wanted:
            return {"value": item.get("value"), "label": item.get("label")}
    for item in items:
        if _normalize_lookup_text(item.get("label")) == wanted:
            return {"value": item.get("value"), "label": item.get("label")}
    substring_hits = [item for item in items if wanted in _normalize_lookup_text(item.get("label"))]
    if len(substring_hits) == 1:
        hit = substring_hits[0]
        return {"value": hit.get("value"), "label": hit.get("label")}
    return None


def _catalog_sample(items: list[dict[str, Any]], *, name_key: str = "name", limit: int = 10) -> list[str]:
    return [str(item.get(name_key)) for item in items[:limit]]


async def _build_prepared_magnite_deal(  # noqa: C901 - linear preparation pipeline
    *,
    deal_name: str,
    marketplace: Any,
    dsps: list[Any],
    publishers: list[Any] | None,
    channel: str | None,
    source: str | None,
    start_date: str | None,
    end_date: str | None,
    floor: float | None,
    rev_share_model: str | None,
    rev_share_value: float | None,
    price_type: str | None,
    price_behavior: str | None,
    currency: str,
    geo_countries_include: list[str] | None,
    geo_countries_exclude: list[str] | None,
    audience_segments: list[Any] | None,
    audience_segments_block: list[Any] | None,
    sizes: list[Any] | None,
    targeting: dict[str, Any] | None,
    extra: dict[str, Any] | None,
) -> dict[str, Any]:
    """Resolve prompt inputs into a creatable Demand Management payload."""
    blockers: list[dict[str, Any]] = []
    warnings: list[str] = []
    quality_flags: list[dict[str, Any]] = []
    resolved_entities: dict[str, Any] = {}
    client = get_magnite_client()

    # --- source / channel ---------------------------------------------------
    resolved_source = ""
    try:
        resolved_source = _normalize_source(channel or source, "channel/source")
        if channel and source:
            cross_check = _normalize_source(source)
            if cross_check != resolved_source:
                blockers.append(
                    _make_blocker(
                        "source_channel_conflict",
                        f"channel {channel!r} maps to {resolved_source} but source {source!r} maps to "
                        f"{cross_check}. Pass one or make them agree.",
                    )
                )
    except ValueError as exc:
        blockers.append(_make_blocker("source_unresolved", str(exc)))
    resolved_entities["source"] = resolved_source

    # --- flight dates ---------------------------------------------------------
    start_iso = ""
    end_iso = ""
    if not start_date:
        start_iso = datetime.now(UTC).strftime("%Y-%m-%dT00:00:00Z")
        quality_flags.append(
            _make_quality_flag(
                "magnite_default_start_date_applied",
                f"start_date omitted; defaulted to today UTC ({start_iso}).",
            )
        )
    else:
        try:
            start_iso = _normalize_iso8601_datetime(start_date, "start_date")
        except ValueError as exc:
            blockers.append(_make_blocker("start_date_invalid", str(exc)))
    if not end_date:
        blockers.append(
            _make_blocker("end_date_missing", "end_date is required — Magnite deals need explicit flight dates.")
        )
    else:
        try:
            end_iso = _normalize_deal_end_date(end_date, "end_date")
        except ValueError as exc:
            blockers.append(_make_blocker("end_date_invalid", str(exc)))

    if not str(deal_name or "").strip():
        blockers.append(_make_blocker("deal_name_missing", "deal_name is required."))

    # --- marketplace ----------------------------------------------------------
    marketplace_id: int | None = None
    if resolved_source:
        try:
            marketplace_listing = _dmg_page_content(await client.dmg_list_marketplaces(resolved_source))
            marketplace_item = _match_catalog_item(marketplace_listing, marketplace)
            if marketplace_item is None:
                blockers.append(
                    _make_blocker(
                        "marketplace_unresolved",
                        f"Could not resolve marketplace {marketplace!r} on {resolved_source}.",
                        available=_catalog_sample(marketplace_listing),
                    )
                )
            else:
                marketplace_id = int(marketplace_item["id"])
                resolved_entities["marketplace"] = marketplace_item
        except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
            blockers.append(_make_blocker("marketplace_lookup_failed", str(exc)))

    # --- DSPs + buyers ----------------------------------------------------------
    resolved_dsps: list[dict[str, Any]] = []
    if resolved_source:
        if not dsps:
            blockers.append(_make_blocker("dsps_missing", "At least one DSP (with at least one buyer) is required."))
        else:
            try:
                dsp_listing = _dmg_page_content(await client.dmg_list_dsps(resolved_source))
            except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
                dsp_listing = []
                blockers.append(_make_blocker("dsp_lookup_failed", str(exc)))
            for entry in dsps:
                if isinstance(entry, dict):
                    dsp_ref = entry.get("dsp", entry.get("id", entry.get("name")))
                    buyer_refs = entry.get("buyers") or []
                else:
                    dsp_ref = entry
                    buyer_refs = []
                dsp_item = _match_catalog_item(dsp_listing, dsp_ref)
                if dsp_item is None:
                    blockers.append(
                        _make_blocker(
                            "dsp_unresolved",
                            f"Could not resolve DSP {dsp_ref!r} on {resolved_source}.",
                            available=_catalog_sample(dsp_listing),
                        )
                    )
                    continue
                dsp_id = int(dsp_item["id"])
                try:
                    buyer_listing = _dmg_page_content(await client.dmg_list_dsp_buyers(dsp_id, resolved_source))
                except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
                    blockers.append(_make_blocker("buyer_lookup_failed", str(exc), dsp_id=dsp_id))
                    continue
                resolved_buyers: list[dict[str, Any]] = []
                if buyer_refs:
                    for buyer_ref in buyer_refs:
                        buyer_item = _match_catalog_item(buyer_listing, buyer_ref)
                        if buyer_item is None:
                            blockers.append(
                                _make_blocker(
                                    "buyer_unresolved",
                                    f"Could not resolve buyer {buyer_ref!r} for DSP {dsp_item.get('name', dsp_id)!r}.",
                                    available=_catalog_sample(buyer_listing),
                                )
                            )
                        else:
                            resolved_buyers.append({"id": int(buyer_item["id"])})
                elif len(buyer_listing) == 1:
                    # Poka-yoke: a bare DSP token is unambiguous only when the
                    # DSP has exactly one buyer.
                    resolved_buyers.append({"id": int(buyer_listing[0]["id"])})
                    quality_flags.append(
                        _make_quality_flag(
                            "magnite_single_buyer_auto_selected",
                            f"DSP {dsp_item.get('name', dsp_id)!r} has exactly one buyer "
                            f"({buyer_listing[0].get('name')}); selected automatically.",
                        )
                    )
                else:
                    blockers.append(
                        _make_blocker(
                            "buyers_missing",
                            f"DSP {dsp_item.get('name', dsp_id)!r} has {len(buyer_listing)} buyers — specify "
                            "which buyer(s) the deal is for.",
                            available=_catalog_sample(buyer_listing),
                        )
                    )
                if resolved_buyers:
                    resolved_dsps.append({"id": dsp_id, "buyers": resolved_buyers})
    resolved_entities["dsps"] = resolved_dsps

    # --- publishers --------------------------------------------------------------
    resolved_publishers: list[dict[str, Any]] = []
    if resolved_source and marketplace_id is not None:
        if not publishers:
            blockers.append(
                _make_blocker(
                    "publishers_missing",
                    "At least one publisher is required. Use magnite_list_publishers to discover the "
                    "marketplace's publishers (deliberately NOT defaulted to all publishers).",
                )
            )
        else:
            try:
                publisher_listing = _dmg_page_content(await client.dmg_list_publishers(resolved_source, marketplace_id))
            except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
                publisher_listing = []
                blockers.append(_make_blocker("publisher_lookup_failed", str(exc)))
            for publisher_ref in publishers:
                publisher_item = _match_catalog_item(publisher_listing, publisher_ref)
                if publisher_item is None:
                    blockers.append(
                        _make_blocker(
                            "publisher_unresolved",
                            f"Could not resolve publisher {publisher_ref!r} in marketplace {marketplace_id}.",
                            available=_catalog_sample(publisher_listing),
                        )
                    )
                    continue
                resolved_publishers.append({"id": int(publisher_item["id"])})
                min_floor = (publisher_item.get("minimumPriceFloor") or {}).get("cpm")
                if floor is not None and isinstance(min_floor, int | float) and float(floor) < float(min_floor):
                    warnings.append(
                        f"floor {floor} is below publisher {publisher_item.get('name')!r} minimum price floor "
                        f"({min_floor}) — Magnite may reject or clamp it."
                    )
    resolved_entities["publishers"] = resolved_publishers

    # --- pricing ----------------------------------------------------------------
    effective_rev_share_model = rev_share_model or DEFAULT_CURATOR_REV_SHARE_MODEL
    if effective_rev_share_model not in MAGNITE_REV_SHARE_MODELS:
        blockers.append(
            _make_blocker(
                "rev_share_model_invalid",
                f"rev_share_model must be one of: {', '.join(sorted(MAGNITE_REV_SHARE_MODELS))}.",
            )
        )
    effective_rev_share_value = rev_share_value
    if effective_rev_share_value is None:
        if rev_share_model is None:
            effective_rev_share_value = DEFAULT_CURATOR_REV_SHARE_VALUE
            quality_flags.append(
                _make_quality_flag(
                    "magnite_default_curator_rev_share_applied",
                    "rev_share omitted; defaulted to Percent 0.30 (flat 30% Elcano curator margin — the API "
                    "guide's Percent scale is a fraction, e.g. 0.25 = 25%). Verify against the deal economics.",
                )
            )
        else:
            blockers.append(
                _make_blocker("rev_share_value_missing", "rev_share_value is required when rev_share_model is set.")
            )

    effective_price_type = price_type
    if floor is not None and effective_price_type is None:
        effective_price_type = "CPM"
    if effective_price_type is not None and effective_price_type not in MAGNITE_PRICE_TYPES:
        blockers.append(
            _make_blocker("price_type_invalid", f"price_type must be one of: {', '.join(sorted(MAGNITE_PRICE_TYPES))}.")
        )
    if effective_price_type == "Market Rate with Minimum" and resolved_source == SOURCE_SPRINGSERVE:
        blockers.append(
            _make_blocker(
                "price_type_unsupported_on_springserve",
                "'Market Rate with Minimum' is not yet supported on SpringServe — use Market Rate or CPM.",
            )
        )
    if effective_price_type == "CPM" and floor is None:
        blockers.append(_make_blocker("cpm_price_missing", "price_type=CPM requires `floor` (the explicit CPM)."))
    effective_price_behavior = price_behavior
    if effective_price_type == "CPM" and effective_price_behavior is None:
        # priceBehavior is required when priceType is CPM. Auction = the
        # CPM acts as a floor; Fixed = the clearing price.
        effective_price_behavior = "Auction"
        quality_flags.append(
            _make_quality_flag(
                "magnite_default_price_behavior_applied",
                "price_behavior omitted; defaulted to 'Auction' (CPM acts as a floor).",
            )
        )
    if effective_price_behavior is not None and effective_price_behavior not in MAGNITE_PRICE_BEHAVIORS:
        blockers.append(
            _make_blocker(
                "price_behavior_invalid",
                f"price_behavior must be one of: {', '.join(sorted(MAGNITE_PRICE_BEHAVIORS))}.",
            )
        )

    curator_pricing: dict[str, Any] = {
        "revShareModel": effective_rev_share_model,
        "currency": currency,
    }
    if effective_price_type:
        curator_pricing["priceType"] = effective_price_type
    if effective_price_behavior:
        curator_pricing["priceBehavior"] = effective_price_behavior
    rev_share_group: dict[str, Any] = {
        "value": effective_rev_share_value,
        "publishers": resolved_publishers,
    }
    if floor is not None and effective_price_type == "CPM":
        rev_share_group["cpm"] = floor
    curator_pricing["publisherRevShares"] = [rev_share_group]

    # --- targeting -----------------------------------------------------------------
    built_targeting: dict[str, Any] = {}
    if geo_countries_include and geo_countries_exclude:
        blockers.append(
            _make_blocker(
                "geo_country_conflict",
                "Pass geo_countries_include OR geo_countries_exclude, not both — the country component "
                "carries a single Include/Exclude type.",
            )
        )
    elif (geo_countries_include or geo_countries_exclude) and resolved_source:
        geo_refs = geo_countries_include or geo_countries_exclude or []
        geo_type = "Include" if geo_countries_include else "Exclude"
        try:
            country_listing = _dmg_page_content(await client.dmg_list_metadata("countries", resolved_source, size=1000))
        except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
            country_listing = []
            blockers.append(_make_blocker("country_lookup_failed", str(exc)))
        geo_values: list[dict[str, Any]] = []
        for geo_ref in geo_refs:
            geo_value = _match_geo_value(country_listing, geo_ref)
            if geo_value is None:
                blockers.append(
                    _make_blocker(
                        "country_unresolved",
                        f"Could not resolve country {geo_ref!r}. Use 2-letter codes ('US') or full names.",
                    )
                )
            else:
                geo_values.append(geo_value)
        if geo_values:
            built_targeting["geography"] = {"country": {"type": geo_type, "values": geo_values}}

    if audience_segments or audience_segments_block:
        if resolved_source and resolved_source != SOURCE_SPRINGSERVE:
            blockers.append(
                _make_blocker(
                    "audience_segments_unsupported_on_dvplus",
                    "Audience segments are only supported on SpringServe (CTV) today. DV+ audience support "
                    "arrives with Magnite API v3.0 (ETA end of June 2026) — re-run then, or drop the segments.",
                )
            )
        elif resolved_source:
            try:
                segment_listing = _dmg_page_content(
                    await client.dmg_list_metadata("audience-segments", resolved_source, size=1000)
                )
            except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
                segment_listing = []
                blockers.append(_make_blocker("segment_lookup_failed", str(exc)))
            segment_lists: list[dict[str, Any]] = []
            for list_type, refs in (("Allow", audience_segments), ("Block", audience_segments_block)):
                if not refs:
                    continue
                resolved_segments: list[dict[str, Any]] = []
                for segment_ref in refs:
                    segment_item = _match_catalog_item(segment_listing, segment_ref)
                    if segment_item is None:
                        blockers.append(
                            _make_blocker(
                                "segment_unresolved",
                                f"Could not resolve audience segment {segment_ref!r}.",
                                available=_catalog_sample(segment_listing),
                            )
                        )
                    else:
                        resolved_segments.append({"id": int(segment_item["id"])})
                if resolved_segments:
                    segment_lists.append({"type": list_type, "segments": resolved_segments})
            if segment_lists:
                # Lists within a group are ANDed; segments inside an Allow
                # list are ANY OF (OR). One group keeps the common
                # "(NONE OF blocked) AND (ANY OF allowed)" logic.
                built_targeting["audience"] = {"segmentGroups": [{"segmentLists": segment_lists}]}

    resolved_sizes: list[dict[str, Any]] = []
    if sizes:
        if resolved_source == SOURCE_SPRINGSERVE:
            blockers.append(
                _make_blocker("sizes_unsupported_on_springserve", "Ad formats/sizes only apply to source=DV+.")
            )
        elif marketplace_id is not None:
            try:
                format_listing = _dmg_page_content(await client.dmg_list_ad_formats(marketplace_id))
            except Exception as exc:  # noqa: BLE001 - surfaced as a structured blocker
                format_listing = []
                blockers.append(_make_blocker("ad_format_lookup_failed", str(exc)))
            formats_seen: set[str] = set()
            for size_ref in sizes:
                size_item = _match_catalog_item(format_listing, size_ref)
                if size_item is None:
                    blockers.append(
                        _make_blocker(
                            "size_unresolved",
                            f"Could not resolve ad size {size_ref!r}.",
                            available=_catalog_sample(format_listing),
                        )
                    )
                else:
                    resolved_sizes.append({"id": int(size_item["id"])})
                    if size_item.get("format"):
                        formats_seen.add(str(size_item["format"]))
            if len(formats_seen) > 1:
                blockers.append(
                    _make_blocker(
                        "mixed_size_formats",
                        f"Mixing sizes of different formats in one deal is not allowed (got {sorted(formats_seen)}).",
                    )
                )
            if len(resolved_sizes) > 15:
                blockers.append(_make_blocker("too_many_sizes", "No more than 15 sizes are allowed per deal."))
    resolved_entities["sizes"] = resolved_sizes

    if targeting:
        # Component-level overlay: caller-supplied raw components win over
        # the convenience-built ones (mirrors the API's PUT semantics).
        for component, component_value in targeting.items():
            built_targeting[component] = component_value

    # --- assemble ------------------------------------------------------------------
    payload: dict[str, Any] = {
        "name": deal_name,
        "type": "Curator",
        "marketplace": {"id": marketplace_id},
        "startDate": start_iso,
        "endDate": end_iso,
        "dsps": resolved_dsps,
        "curatorPricing": curator_pricing,
    }
    if resolved_sizes:
        payload["sizes"] = resolved_sizes
    if built_targeting:
        payload["targeting"] = built_targeting
    if extra:
        payload.update(copy.deepcopy(extra))

    blocking_issues = [blocker["message"] for blocker in blockers]
    prepared = {
        "prepared_deal_id": f"magnite-{uuid.uuid4().hex[:12]}",
        "source": resolved_source,
        "deal_intent": payload,
        "ready_to_create": not blockers,
        "blocking_issues": blocking_issues,
        "blockers": blockers,
        "warnings": warnings,
        "quality_flags": quality_flags,
        "resolved_entities": resolved_entities,
        "created": False,
    }
    _prepared_magnite_deals[prepared["prepared_deal_id"]] = prepared
    return prepared


@mcp.tool()
async def magnite_prepare_deal_from_prompt_inputs(
    deal_name: str,
    marketplace: Any,
    dsps: list[Any],
    publishers: list[Any] | None = None,
    channel: str | None = None,
    source: str | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    floor: float | None = None,
    rev_share_model: str | None = None,
    rev_share_value: float | None = None,
    price_type: str | None = None,
    price_behavior: str | None = None,
    currency: str = "USD",
    geo_countries_include: list[str] | None = None,
    geo_countries_exclude: list[str] | None = None,
    audience_segments: list[Any] | None = None,
    audience_segments_block: list[Any] | None = None,
    sizes: list[Any] | None = None,
    targeting: dict[str, Any] | None = None,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Resolve human-readable Magnite deal inputs into a server-side draft.

    All name -> ID resolution (marketplace, DSPs, buyers, publishers,
    countries, audience segments, DV+ ad sizes) happens server-side via the
    documented Demand Management reference endpoints. The returned
    prepared_deal_id is submitted via magnite_create_prepared_deal to
    actually create the deal. Inspect ready_to_create and blocking_issues
    before submitting.

    Key inputs:
      - channel: "ctv" routes to SpringServe; "display"/"olv" route to DV+.
        Alternatively pass source="SpringServe"|"DV+" directly.
      - marketplace: name or ID. Immutable after creation.
      - dsps: list of names/IDs, or {"dsp": <name_or_id>, "buyers":
        [<name_or_id>, ...]} entries. Magnite requires >=1 buyer per DSP; a
        bare DSP token auto-selects its buyer ONLY when exactly one exists.
      - publishers: names or IDs (required — deliberately NOT defaulted to
        all marketplace publishers).
      - floor: explicit CPM. Sets priceType=CPM with priceBehavior Auction
        (floor semantics) unless overridden.
      - rev share: defaults to Percent 0.30 (30% Elcano curator margin;
        Magnite's Percent scale is a fraction per the API guide examples) and
        emits magnite_default_curator_rev_share_applied.

    Source-specific constraints enforced as blockers: audience segments are
    SpringServe-only (DV+ arrives with API v3.0, ETA end of June 2026);
    sizes are DV+-only; 'Market Rate with Minimum' is DV+-only.
    """
    logger.info("magnite_prepare_deal_from_prompt_inputs called: %s", deal_name)
    try:
        prepared = await _build_prepared_magnite_deal(
            deal_name=deal_name,
            marketplace=marketplace,
            dsps=dsps,
            publishers=publishers,
            channel=channel,
            source=source,
            start_date=start_date,
            end_date=end_date,
            floor=floor,
            rev_share_model=rev_share_model,
            rev_share_value=rev_share_value,
            price_type=price_type,
            price_behavior=price_behavior,
            currency=currency,
            geo_countries_include=geo_countries_include,
            geo_countries_exclude=geo_countries_exclude,
            audience_segments=audience_segments,
            audience_segments_block=audience_segments_block,
            sizes=sizes,
            targeting=targeting,
            extra=extra,
        )
        return {
            "success": True,
            "prepared_deal_id": prepared["prepared_deal_id"],
            "source": prepared["source"],
            "ready_to_create": prepared["ready_to_create"],
            "blocking_issues": prepared["blocking_issues"],
            "blockers": prepared["blockers"],
            "warnings": prepared["warnings"],
            "quality_flags": prepared["quality_flags"],
            "resolved_entities": prepared["resolved_entities"],
            "deal_intent_preview": prepared["deal_intent"],
        }
    except Exception as exc:
        logger.error("magnite_prepare_deal_from_prompt_inputs failed: %s", exc)
        return {
            "success": False,
            "ready_to_create": False,
            "blocking_issues": [str(exc)],
            "blockers": [_make_blocker("preparation_error", str(exc))],
            "warnings": [],
            "quality_flags": [_make_quality_flag("magnite_preparation_error", str(exc))],
            "error": str(exc),
        }


@mcp.tool()
async def magnite_create_prepared_deal(prepared_deal_id: str) -> dict[str, Any]:
    """Submit a previously prepared Magnite deal artifact and verify it.

    This is a CRITICAL action that requires self-audit confirmation before
    execution. Refuses to submit if the prepared artifact has unresolved
    blocking_issues; replays the recorded outcome (instead of double-creating
    a live deal) if the artifact was already submitted. On success, calls
    magnite_get_deal to verify the created record.
    """
    logger.info("magnite_create_prepared_deal called: %s", prepared_deal_id)
    prepared = _prepared_magnite_deals.get(prepared_deal_id)
    if prepared is None:
        return {
            "success": False,
            "deal": None,
            "warnings": [],
            "quality_flags": [
                _make_quality_flag(
                    "magnite_prepared_deal_not_found",
                    f"Prepared Magnite deal not found: {prepared_deal_id}",
                )
            ],
            "error": f"Prepared Magnite deal not found: {prepared_deal_id}",
            "verification": None,
        }
    if not prepared["ready_to_create"]:
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": prepared["warnings"],
            "quality_flags": list(prepared.get("quality_flags", [])),
            "error": "Prepared Magnite deal is blocked and cannot be created.",
            "blocking_issues": prepared["blocking_issues"],
            "blockers": prepared["blockers"],
            "verification": None,
        }
    if prepared.get("created"):
        # The create POST for this artifact already succeeded — re-submitting
        # would create a duplicate live deal on Magnite.
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
                _make_quality_flag(
                    "magnite_deal_already_created",
                    "This prepared deal was already submitted and the deal exists on Magnite. "
                    "Do NOT prepare/submit it again; use magnite_get_deal with the recorded deal id.",
                )
            ],
            "error": "Prepared deal already submitted; refusing to create a duplicate.",
            "verification": None,
        }

    warnings = list(prepared["warnings"])
    quality_flags: list[dict[str, Any]] = list(prepared.get("quality_flags", []))
    verification: dict[str, Any] | None = None
    try:
        client = get_magnite_client()
        deal = await client.dmg_create_deal(prepared["source"], prepared["deal_intent"])
        # The deal now exists on Magnite — mark the artifact consumed so a
        # retry cannot double-create.
        prepared["created"] = True
        deal_id = deal.get("id")
        if deal_id:
            try:
                verification = await magnite_get_deal(str(deal_id), prepared["source"])
                if isinstance(verification, dict) and not verification.get("success"):
                    quality_flags.append(
                        _make_quality_flag(
                            "magnite_verification_failed",
                            "Magnite verification re-fetch failed.",
                            deal_id=deal_id,
                        )
                    )
            except Exception as ver_exc:
                verification = {"success": False, "error": f"Verification call failed: {ver_exc}"}
                quality_flags.append(
                    _make_quality_flag(
                        "magnite_verification_failed", f"Verification call failed: {ver_exc}", deal_id=deal_id
                    )
                )
        result = {
            "success": True,
            "prepared_deal_id": prepared_deal_id,
            "deal": deal,
            "deal_id": deal_id,
            # The Demand Management API guide documents no per-deal console
            # URL; surface deal_id prominently instead of fabricating a link.
            "deal_url": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": None,
            "verification": verification,
        }
        prepared["created_result"] = result
        return result
    except Exception as exc:
        error_payload = _dmg_exception_error("create_prepared_deal", exc)
        quality_flags.append(_make_quality_flag("magnite_create_call_failed", error_payload["message"]))
        return {
            "success": False,
            "prepared_deal_id": prepared_deal_id,
            "deal": None,
            "warnings": warnings,
            "quality_flags": quality_flags,
            "error": error_payload,
            "verification": None,
        }


@mcp.tool()
async def magnite_execute_deal_from_prompt_inputs(
    deal_name: str,
    marketplace: Any,
    dsps: list[Any],
    publishers: list[Any] | None = None,
    channel: str | None = None,
    source: str | None = None,
    start_date: str | None = None,
    end_date: str | None = None,
    floor: float | None = None,
    rev_share_model: str | None = None,
    rev_share_value: float | None = None,
    price_type: str | None = None,
    price_behavior: str | None = None,
    currency: str = "USD",
    geo_countries_include: list[str] | None = None,
    geo_countries_exclude: list[str] | None = None,
    audience_segments: list[Any] | None = None,
    audience_segments_block: list[Any] | None = None,
    sizes: list[Any] | None = None,
    targeting: dict[str, Any] | None = None,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Prepare, submit, and verify a Magnite ClearLine Curation deal in one call.

    This is a CRITICAL action that requires self-audit confirmation before
    execution. Thin wrapper over magnite_prepare_deal_from_prompt_inputs and
    magnite_create_prepared_deal — use the two-step pair when you want to
    inspect the resolved artifact before committing. See the prepare tool's
    docstring for input semantics, defaults, and per-source constraints.
    """
    logger.info("magnite_execute_deal_from_prompt_inputs called: %s", deal_name)
    preparation = await magnite_prepare_deal_from_prompt_inputs(
        deal_name=deal_name,
        marketplace=marketplace,
        dsps=dsps,
        publishers=publishers,
        channel=channel,
        source=source,
        start_date=start_date,
        end_date=end_date,
        floor=floor,
        rev_share_model=rev_share_model,
        rev_share_value=rev_share_value,
        price_type=price_type,
        price_behavior=price_behavior,
        currency=currency,
        geo_countries_include=geo_countries_include,
        geo_countries_exclude=geo_countries_exclude,
        audience_segments=audience_segments,
        audience_segments_block=audience_segments_block,
        sizes=sizes,
        targeting=targeting,
        extra=extra,
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
            "deal_id": None,
            "deal_url": None,
            "warnings": preparation.get("warnings", []),
            "quality_flags": list(preparation.get("quality_flags", [])),
            "error": error_message,
            "verification": None,
            "preparation": preparation,
        }
    creation = await magnite_create_prepared_deal(preparation["prepared_deal_id"])
    combined_warnings: list[str] = []
    seen_warnings: set[str] = set()
    for warning in list(preparation.get("warnings", [])) + list(creation.get("warnings", [])):
        if warning not in seen_warnings:
            combined_warnings.append(warning)
            seen_warnings.add(warning)
    # magnite_create_prepared_deal seeds its quality_flags from the prepared
    # artifact, so creation's list is the canonical merged result.
    return {
        "success": creation.get("success", False),
        "phase": "verify" if (creation.get("verification") or {}).get("success") else "create",
        "deal": creation.get("deal"),
        "deal_id": creation.get("deal_id"),
        "deal_url": creation.get("deal_url"),
        "warnings": combined_warnings,
        "quality_flags": list(creation.get("quality_flags", [])),
        "error": creation.get("error"),
        "verification": creation.get("verification"),
        "preparation": preparation,
        "creation": creation,
    }


# =============================================================================
# Main Entry Point
# =============================================================================

if __name__ == "__main__":
    logger.info("Starting Magnite MCP Server")

    # Check for Magnite credentials
    has_credentials = bool(os.environ.get("MAGNITE_ACCESS_KEY")) and bool(os.environ.get("MAGNITE_SECRET_KEY"))

    if not has_credentials:
        logger.warning(
            "Magnite not configured. Set MAGNITE_ACCESS_KEY and MAGNITE_SECRET_KEY to enable deal management."
        )

    try:
        # Use stdio transport (default for FastMCP)
        mcp.run(transport="stdio")
    except Exception as e:
        logger.error("Failed to start server: %s", e)
        sys.exit(1)
