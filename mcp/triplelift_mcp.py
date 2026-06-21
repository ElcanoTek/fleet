#!/usr/bin/env python3
"""
TripleLift Curation MCP Server

A Model Context Protocol (MCP) server for programmatic deal management on the
TripleLift Curation platform. This is a dedicated MCP for the TripleLift
Curation REST API.

Runs within the Cutlass container environment.
"""

import copy
import hashlib
import logging
import os
import sys
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
mcp = FastMCP("triplelift_mcp")

# Constants
USER_AGENT = "cutlass/1.0"
DEFAULT_TIMEOUT = 60.0
DEFAULT_BASE_URL = "https://api.triplelift.net"
DEFAULT_TOKEN_URL = "https://auth.triplelift.net/oauth/token"
DEFAULT_REPORTING_BASE_URL = "https://reporting-api.triplelift.net/graphql"
DEFAULT_REPORT_DOWNLOAD_DIR = os.path.expanduser("~/Victoria/triplelift_reports")

SUPPORTED_DEAL_PRICE_TYPES = {"CEILING", "FIXED", "FLOOR"}
SUPPORTED_CHANNELS = {"WEB", "CTV"}
SUPPORTED_COMMERCIALIZED_FORMATS = {
    "BRANDED_VIDEO",
    "CANVAS_VIDEO",
    "CAROUSEL",
    "CINEMAGRAPH",
    "CLICK_TO_PLAY_VIDEO",
    "COLLECTION",
    "DISPLAY",
    "DYNAMIC_OVERLAY",
    "ENHANCED_SPOTS",
    "FLIPBOOK",
    "HIGH_IMPACT_DISPLAY",
    "IMAGE",
    "INSTREAM",
    "L_BAR",
    "MULTI_ACTION",
    "OUTSTREAM",
    "PAUSE_AD",
    "PHARMA",
    "RESPONSIVE_ECOMMERCE",
    "REVEAL",
    "SCROLL",
    "SKU_IMAGE",
    "SKU_VIDEO",
    "SPLIT_SCREEN",
    "SPOTS",
    "VERTICAL_VIDEO",
    "WINDOW",
}

# Regulatory Policy -> "Controlled" -> Include Political Ads Allowed.
#
# Confirmed empirically against the TripleLift UI (saving the toggle): the control
# is a targeting node carrying this UI-expression binding and content-category id.
# On the deal create/patch API the UI_EXPR_* binding is what you send; TripleLift
# maps it internally to the engine binding EB_SUPPLY_CONTENT_CATEGORY_ID / 23891.
# (The targeted-avails endpoint reports EB_SUPPLY_CONTENT_CATEGORY_ID under
# unsupportedBindings, but that is only an avails-forecast limitation — the deal
# still saves and serves with the policy applied.)
REGULATORY_POLICY_CONTROLLED_BINDING = "UI_EXPR_REGULATORY_POLICY_CONTROLLED"
POLITICAL_ADS_CATEGORY_ID = 23891

# Advertiser-side reporting enums for the advertiserDealsReport GraphQL query.
# Elcano is provisioned on TripleLift as memberType="advertiser" (a curator who
# sells curated packages to DSPs), so the advertiser-deals report is the one
# that returns per-deal performance data. The publisher-side queries are not
# valid for our role.
TRIPLELIFT_REPORT_DIMENSION_ALIASES: dict[str, str] = {
    "day": "YMD",
    "date": "YMD",
    "ymd": "YMD",
    "hour": "HOUR",
    "deal": "DEAL_NAME",
    "deal name": "DEAL_NAME",
    "deal id": "DEAL_ID",
    "deal status": "DEAL_STATUS",
    "dsp": "DSP_NAME",
    "dsp name": "DSP_NAME",
    "buyer": "DSP_NAME",
    "seat": "DSP_SEAT_NAME",
    "dsp seat": "DSP_SEAT_NAME",
    "seat id": "DSP_SEAT_ID",
    "format": "FORMAT",
    "primary goal": "PRIMARY_GOAL_NAME",
    "secondary goal": "SECONDARY_GOAL_NAME",
    "start date": "DEAL_START_DATE",
    "end date": "DEAL_END_DATE",
    "budget": "DEAL_BUDGET",
}

TRIPLELIFT_REPORT_METRIC_ALIASES: dict[str, str] = {
    "spend": "DEAL_SPEND",
    "deal spend": "DEAL_SPEND",
    "revenue": "DEAL_SPEND",
    "impressions": "IMPRESSIONS",
    "rendered": "RENDERED",
    "renders": "RENDERED",
    "clicks": "CLICKS",
    "ctr": "CTR",
    "cpc": "CPC",
    "ecpm": "AD_SPEND_ECPM",
    "cpm": "AD_SPEND_ECPM",
    "ad spend ecpm": "AD_SPEND_ECPM",
    "requests": "BID_REQUESTS",
    "bid requests": "BID_REQUESTS",
    "responses": "BID_RESPONSES",
    "bid responses": "BID_RESPONSES",
}


def _build_targeting_expression(
    country_ids: list[int] | None = None,
    device_types: list[str] | None = None,
    segment_ids: list[int] | None = None,
    operator: str = "AND",
) -> dict[str, Any]:
    """
    Build a simple TripleLift targetingExpression tree from common inputs.

    Args:
        country_ids: Country target IDs for EB_SUPPLY_GEO_COUNTRY_ID.
        device_types: Device type values for EB_DEVICE_TYPE.
        segment_ids: Segment IDs for EB_AUDIENCE_SEGMENT_ID.
        operator: Root boolean operator (AND/OR).

    Returns:
        Recursive targetingExpression dictionary.
    """
    root_type = operator.upper()
    if root_type not in {"AND", "OR"}:
        raise ValueError("operator must be AND or OR")

    children: list[dict[str, Any]] = []

    if country_ids:
        children.append(
            {
                "type": "ANY",
                "binding": "EB_SUPPLY_GEO_COUNTRY_ID",
                "integralTargets": [int(country_id) for country_id in country_ids],
            }
        )

    if device_types:
        # NOTE: device types are carried as string targets here; TripleLift's engine
        # binding for device type is EB_SUPPLY_DEVICE_TYPE and expects the numeric
        # device-type ids returned by the EB_SUPPLY_DEVICE_TYPE targets endpoint.
        children.append(
            {
                "type": "ANY",
                "binding": "EB_SUPPLY_DEVICE_TYPE",
                "stringTargets": [str(device_type) for device_type in device_types],
            }
        )

    if segment_ids:
        children.append(
            {
                "type": "ANY",
                "binding": "EB_SUPPLY_1P_SEGMENT_ID",
                "integralTargets": [int(segment_id) for segment_id in segment_ids],
            }
        )

    return {"type": root_type, "children": children}


def _expression_has_binding(expression: Any, binding: str) -> bool:
    """Recursively check whether a targetingExpression tree already uses `binding`."""
    if not isinstance(expression, dict):
        return False
    if expression.get("binding") == binding:
        return True
    return any(_expression_has_binding(child, binding) for child in expression.get("children", []) or [])


def _regulatory_policy_controlled_node() -> dict[str, Any]:
    """Build the 'Include Political Ads Allowed' regulatory-policy targeting node."""
    return {
        "type": "ANY",
        "excluded": False,
        "binding": REGULATORY_POLICY_CONTROLLED_BINDING,
        "integralTargets": [POLITICAL_ADS_CATEGORY_ID],
    }


def _apply_regulatory_policy_controlled(expression: dict[str, Any] | None) -> dict[str, Any]:
    """
    Merge the regulatory-policy ('Include Political Ads Allowed') node into a
    targetingExpression tree.

    - No existing expression -> a root AND wrapping just the policy node.
    - Existing AND/ALL root   -> append the policy node as another child.
    - Any other root          -> wrap the existing expression and the policy node
                                  together under a new AND.

    Idempotent: if the policy binding is already present, the expression is
    returned unchanged.
    """
    node = _regulatory_policy_controlled_node()
    if not expression:
        return {"type": "AND", "children": [node]}
    if _expression_has_binding(expression, REGULATORY_POLICY_CONTROLLED_BINDING):
        return expression
    merged = copy.deepcopy(expression)
    if merged.get("type") in ("AND", "ALL") and isinstance(merged.get("children"), list):
        merged["children"].append(node)
        return merged
    return {"type": "AND", "children": [merged, node]}


class TripleLiftClient:
    """
    Client for interacting with the TripleLift Curation API.

    Uses OAuth2 client_credentials authentication. Access token is obtained via
    POST to TRIPLELIFT_TOKEN_URL and included as Bearer token on API requests.
    """

    def __init__(self):
        self.base_url = os.environ.get("TRIPLELIFT_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.token_url = os.environ.get("TRIPLELIFT_TOKEN_URL", DEFAULT_TOKEN_URL)
        self.client_id = os.environ.get("TRIPLELIFT_CLIENT_ID", "")
        self.client_secret = os.environ.get("TRIPLELIFT_CLIENT_SECRET", "")
        self.audience = os.environ.get("TRIPLELIFT_AUDIENCE", "")
        self.organization = os.environ.get("TRIPLELIFT_ORGANIZATION", "")
        self.scope = os.environ.get("TRIPLELIFT_SCOPE", "")
        self.member_id = os.environ.get("TRIPLELIFT_MEMBER_ID", "")
        self._token = ""
        self._http_client: httpx.AsyncClient | None = None

    def _is_configured(self) -> bool:
        """Check if TripleLift credentials are configured."""
        return bool(self.client_id) and bool(self.client_secret)

    async def _get_http_client(self) -> httpx.AsyncClient:
        """Get or create the HTTP client."""
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _login(self) -> None:
        """Authenticate with TripleLift and obtain an OAuth2 access token."""
        if not self.client_id or not self.client_secret:
            raise ValueError("TRIPLELIFT_CLIENT_ID and TRIPLELIFT_CLIENT_SECRET are required for login.")

        logger.info("Logging in to TripleLift API")

        client = await self._get_http_client()

        try:
            payload: dict[str, str] = {
                "grant_type": "client_credentials",
                "client_id": self.client_id,
                "client_secret": self.client_secret,
            }
            if self.audience:
                payload["audience"] = self.audience
            if self.organization:
                payload["organization"] = self.organization
            if self.scope:
                payload["scope"] = self.scope

            response = await client.post(
                self.token_url,
                json=payload,
                headers={
                    "Content-Type": "application/json",
                    "User-Agent": USER_AGENT,
                },
            )
            response.raise_for_status()
            result = response.json()

            token = result.get("access_token")
            if not token:
                raise ValueError("Login successful but no access_token in response")

            self._token = token
            logger.info("Successfully obtained TripleLift access token")

        except httpx.HTTPStatusError as e:
            logger.error("Login failed with HTTP status %d", e.response.status_code)
            raise ValueError(f"TripleLift login failed: HTTP {e.response.status_code}: {e.response.text}") from e
        except Exception as e:
            if isinstance(e, ValueError):
                raise
            logger.error("Login failed: %s", type(e).__name__)
            raise ValueError(f"TripleLift login failed: {type(e).__name__}") from e

    async def _ensure_token(self) -> str:
        """Ensure we have a valid token, logging in if necessary."""
        if not self._is_configured():
            raise ValueError(
                "TripleLift not configured. Set TRIPLELIFT_CLIENT_ID and TRIPLELIFT_CLIENT_SECRET environment variables."
            )

        if not self._token:
            await self._login()

        return self._token

    def _get_headers(self) -> dict[str, str]:
        """Get request headers including authentication token."""
        return {
            "Authorization": f"Bearer {self._token}",
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
        """Execute an HTTP request against the TripleLift API."""
        await self._ensure_token()

        client = await self._get_http_client()
        url = f"{self.base_url}{endpoint}"
        headers = self._get_headers()

        logger.info("Executing %s request to TripleLift: %s", method, endpoint)

        try:
            if method.upper() == "GET":
                response = await client.get(url, headers=headers, params=params)
            elif method.upper() == "POST":
                response = await client.post(url, headers=headers, json=json_data, params=params)
            elif method.upper() == "PATCH":
                response = await client.patch(url, headers=headers, json=json_data, params=params)
            else:
                # DELETE is intentionally unsupported — see the policy note
                # above delete_deal's former location (irreversible actions
                # are kept off the agent surface).
                raise ValueError(f"Unsupported HTTP method: {method}")

            if response.status_code == 401 and retry_on_401:
                logger.warning("Received 401, attempting token refresh")
                self._token = ""
                return await self._request(method, endpoint, json_data, params, retry_on_401=False)

            response.raise_for_status()
            body = response.json() if response.content else {}
            # TripleLift wraps successful responses as {"status": "success", "data": ...}.
            # Unwrap so helpers see the payload directly. Unwrap is defensive — if the
            # envelope is absent (e.g. tests, future endpoint shapes) the body is returned as-is.
            if isinstance(body, dict) and body.get("status") == "success" and "data" in body:
                return body["data"]
            return body

        except httpx.HTTPStatusError:
            raise

    async def create_deal(self, member_id: int, payload: dict[str, Any]) -> dict[str, Any]:
        """POST /curation/v1/{member_id}/deal"""
        return await self._request("POST", f"/curation/v1/{member_id}/deal", json_data=payload)

    async def get_deal(self, member_id: int, deal_id: int) -> dict[str, Any]:
        """GET /curation/v1/{member_id}/deal/{deal_id}.

        Returns the FULL response envelope (deal + targeting + adQualityProfile +
        curationFee + dealTypeId + ...), NOT just the inner `deal` object. The
        `targeting` tree — where regulatory-policy / political-ads and all other
        targeting live — is a sibling of `deal`, so stripping to `deal` would hide it.
        """
        return await self._request("GET", f"/curation/v1/{member_id}/deal/{deal_id}")

    async def list_deals(
        self,
        member_id: int,
        query: str | None = None,
        order_by: str | None = None,
        sort_dir: str | None = None,
        deal_type_id: int | None = None,
    ) -> list[dict[str, Any]]:
        """GET /curation/v1/{member_id}/deals"""
        params: dict[str, Any] = {}
        if query:
            params["query"] = query
        if order_by:
            params["orderBy"] = order_by
        if sort_dir:
            params["sortDir"] = sort_dir
        if deal_type_id is not None:
            params["dealTypeId"] = deal_type_id
        result = await self._request("GET", f"/curation/v1/{member_id}/deals", params=params)
        return result.get("deals", [])

    async def update_deal(self, member_id: int, deal_id: int, payload: dict[str, Any]) -> dict[str, Any]:
        """PATCH /curation/v1/{member_id}/deal/{deal_id}"""
        return await self._request("PATCH", f"/curation/v1/{member_id}/deal/{deal_id}", json_data=payload)

    # NOTE: the TripleLift Curation API also exposes DELETE /curation/v1/
    # {member_id}/deal. It is DELIBERATELY not implemented here and MUST NOT
    # be exposed as an MCP tool: deletion is irreversible, and giving the
    # agent an irreversible destructive action is a security decision Elcano
    # made against (2026-06-11, Elyse — same policy as OpenX dealArchive).
    # To stop a deal's delivery, use toggle_deal_status(active=False).

    async def toggle_deal_status(self, member_id: int, deal_id: int, active: bool) -> dict[str, Any]:
        """PATCH /curation/v1/{member_id}/deal/status"""
        return await self._request(
            "PATCH",
            f"/curation/v1/{member_id}/deal/status",
            json_data={"id": deal_id, "active": active},
        )

    async def list_buyers(self, member_id: int, buyer_id: str | None = None) -> list[dict[str, Any]]:
        """GET /curation/v1/{member_id}/buyers"""
        params: dict[str, Any] = {}
        if buyer_id:
            params["buyerId"] = buyer_id
        result = await self._request("GET", f"/curation/v1/{member_id}/buyers", params=params)
        if isinstance(result, list):
            return result
        return result.get("buyers", [])

    async def list_countries(self, member_id: int) -> list[dict[str, Any]]:
        """GET /curation/v1/{member_id}/targets/EB_SUPPLY_GEO_COUNTRY_ID"""
        result = await self._request("GET", f"/curation/v1/{member_id}/targets/EB_SUPPLY_GEO_COUNTRY_ID")
        if isinstance(result, list):
            return result
        return result.get("countries", [])

    async def list_segments(self, member_id: int, with_description: bool = False) -> list[dict[str, Any]]:
        """GET /curation/v1/{member_id}/segments"""
        params: dict[str, Any] = {}
        if with_description:
            params["withDescription"] = "true"
        result = await self._request("GET", f"/curation/v1/{member_id}/segments", params=params)
        if isinstance(result, list):
            return result
        return result.get("segments", [])

    async def get_avails(self, member_id: int, payload: dict[str, Any]) -> dict[str, Any]:
        """POST /curation/v1/{member_id}/targeted-avails"""
        return await self._request("POST", f"/curation/v1/{member_id}/targeted-avails", json_data=payload)

    async def verify_token(self) -> bool:
        """Verify token by making a lightweight API call (list buyers)."""
        member_id = int(self.member_id) if self.member_id else 0
        await self._request("GET", f"/curation/v1/{member_id}/buyers")
        return True


class TripleLiftReportingClient:
    """
    Client for the TripleLift GraphQL Reporting API.

    Reuses the same OAuth2 client_credentials token issued for the curation API
    (audience: federated-api.prod.triplelift.net). The reporting host accepts
    that Bearer token directly; no separate API key, JWT, or console login is
    required.
    """

    def __init__(self):
        self.base_url = os.environ.get("TRIPLELIFT_REPORTING_BASE_URL", DEFAULT_REPORTING_BASE_URL)
        self.member_id = os.environ.get("TRIPLELIFT_MEMBER_ID", "")
        self._http_client: httpx.AsyncClient | None = None

    def _is_configured(self) -> bool:
        return get_triplelift_client()._is_configured()

    async def _get_http_client(self) -> httpx.AsyncClient:
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=DEFAULT_TIMEOUT)
        return self._http_client

    async def _ensure_auth(self) -> str:
        if not self._is_configured():
            raise ValueError(
                "TripleLift reporting is not configured. Set TRIPLELIFT_CLIENT_ID and TRIPLELIFT_CLIENT_SECRET "
                "(reporting reuses the curation OAuth credentials)."
            )
        return await get_triplelift_client()._ensure_token()

    def _build_headers(self, token: str) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        }

    async def graphql(self, query: str, variables: dict[str, Any] | None = None) -> dict[str, Any]:
        token = await self._ensure_auth()
        client = await self._get_http_client()
        payload = {"query": query, "variables": variables or {}}

        response = await client.post(self.base_url, json=payload, headers=self._build_headers(token))
        if response.status_code == 401:
            curation = get_triplelift_client()
            curation._token = ""
            token = await curation._ensure_token()
            response = await client.post(self.base_url, json=payload, headers=self._build_headers(token))
        response.raise_for_status()
        return response.json()


# Global client instance
_triplelift_client: TripleLiftClient | None = None
_triplelift_reporting_client: TripleLiftReportingClient | None = None


def get_triplelift_client() -> TripleLiftClient:
    """Get or create the TripleLift client singleton."""
    global _triplelift_client
    if _triplelift_client is None:
        _triplelift_client = TripleLiftClient()
    return _triplelift_client


def get_triplelift_reporting_client() -> TripleLiftReportingClient:
    global _triplelift_reporting_client
    if _triplelift_reporting_client is None:
        _triplelift_reporting_client = TripleLiftReportingClient()
    return _triplelift_reporting_client


def _resolve_triplelift_report_fields(requested_values: list[str] | None, alias_map: dict[str, str]) -> list[str]:
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


async def _fetch_triplelift_csv_presigned(presigned_url: str) -> str:
    """Fetch the CSV body from a TripleLift presigned report URL."""
    client = get_triplelift_reporting_client()
    http_client = await client._get_http_client()
    response = await http_client.get(presigned_url)
    response.raise_for_status()
    return response.text


def _write_triplelift_csv_download(*, filename_hint: str | None, csv_text: str) -> dict[str, Any]:
    download_dir = Path(os.environ.get("TRIPLELIFT_REPORT_DOWNLOAD_DIR", DEFAULT_REPORT_DOWNLOAD_DIR)).expanduser()
    download_dir.mkdir(parents=True, exist_ok=True)
    base_name = (filename_hint or "triplelift_report.csv").strip() or "triplelift_report.csv"
    if "." not in Path(base_name).name:
        base_name = f"{base_name}.csv"
    filepath = download_dir / Path(base_name).name
    filepath.write_text(csv_text, encoding="utf-8")
    file_bytes = filepath.read_bytes()
    return {
        "success": True,
        "path": str(filepath),
        "bytes": len(file_bytes),
        "sha256": hashlib.sha256(file_bytes).hexdigest(),
        "content_type": "text/csv",
    }


def _resolve_member_id(member_id: int | None) -> int:
    """Resolve optional member_id argument using client default when needed."""
    if member_id is not None:
        return int(member_id)

    client = get_triplelift_client()
    if client.member_id:
        return int(client.member_id)

    raise ValueError("member_id is required when TRIPLELIFT_MEMBER_ID is not configured")


@mcp.tool()
async def tl_auth_status() -> dict[str, Any]:
    """
    Check the authentication status of the TripleLift Curation API connection.

    Verifies that credentials are configured and that the current token is
    active by making a lightweight API call.
    """
    logger.info("tl_auth_status called")

    client = get_triplelift_client()

    if not client._is_configured():
        return {
            "configured": False,
            "authenticated": False,
            "error": "TripleLift credentials not configured. Set TRIPLELIFT_CLIENT_ID and TRIPLELIFT_CLIENT_SECRET.",
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
async def tl_reporting_auth_status() -> dict[str, Any]:
    client = get_triplelift_reporting_client()
    if not client._is_configured():
        return {
            "configured": False,
            "authenticated": False,
            "error": (
                "TripleLift reporting credentials not configured. Set TRIPLELIFT_CLIENT_ID and "
                "TRIPLELIFT_CLIENT_SECRET (reporting reuses the curation OAuth credentials)."
            ),
        }

    try:
        await client._ensure_auth()
        return {"configured": True, "authenticated": True}
    except httpx.HTTPStatusError as e:
        return {
            "configured": True,
            "authenticated": False,
            "error": f"HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {"configured": True, "authenticated": False, "error": str(e)}


@mcp.tool()
async def tl_advertiser_deals_report(
    deal_member_id: str,
    start_date: str,
    end_date: str,
    dimensions: list[str],
    metrics: list[str],
    filters: list[dict[str, Any]] | None = None,
    cursor: str | None = None,
    size: int | None = None,
) -> dict[str, Any]:
    """
    Run a synchronous advertiserDealsReport against the TripleLift reporting API.

    This is the advertiser-side report that returns per-deal performance data
    for accounts provisioned with memberType="advertiser" (curators selling
    packages to DSPs). Supports dimension breakdowns (deal/day/dsp/etc.) and
    paginates via the cursor field.

    Valid dimensions (AdvertiserDealsDimensionField): YMD, HOUR, DEAL_NAME,
    DEAL_ID, DEAL_STATUS, DSP_NAME, DSP_SEAT_NAME, DSP_SEAT_ID, FORMAT,
    PRIMARY_GOAL_NAME, SECONDARY_GOAL_NAME, SECONDARY_GOAL_VALUE,
    DEAL_START_DATE, DEAL_END_DATE, DEAL_BUDGET.

    Valid metrics (AdvertiserDealsMetricField): DEAL_SPEND, IMPRESSIONS,
    RENDERED, CLICKS, CTR, CPC, AD_SPEND_ECPM, BID_REQUESTS, BID_RESPONSES.
    """
    query = """
query AdvertiserDealsReport(
  $dealMemberId: String!,
  $startDate: Date!,
  $endDate: Date!,
  $dimensions: [AdvertiserDealsDimensionField!]!,
  $metrics: [AdvertiserDealsMetricField!]!,
  $filters: [AdvertiserDealsIdFilter!]!,
  $cursor: String,
  $size: Int
) {
  advertiserDealsReport(
    dealMemberId: $dealMemberId,
    startDate: $startDate,
    endDate: $endDate,
    dimensions: $dimensions,
    metrics: $metrics,
    filters: $filters,
    cursor: $cursor,
    size: $size
  ) {
    rows {
      dimensions {
        name
        value
      }
      metrics {
        __typename
        ... on MetricLong {
          name
          longValue: value
        }
        ... on MetricDecimal {
          name
          decimalValue: value
        }
      }
    }
    nextCursor
    totalRows
  }
}
"""

    try:
        client = get_triplelift_reporting_client()
        result = await client.graphql(
            query,
            {
                "dealMemberId": deal_member_id,
                "startDate": start_date,
                "endDate": end_date,
                "dimensions": dimensions,
                "metrics": metrics,
                "filters": filters or [],
                "cursor": cursor,
                "size": size,
            },
        )
        return {"success": True, "data": result.get("data"), "errors": result.get("errors")}
    except Exception as e:
        return {"success": False, "error": f"Failed TripleLift advertiserDealsReport: {e}"}


@mcp.tool()
async def tl_async_download_advertiser_report(
    buyer_member_id: str,
    start_date: str,
    end_date: str,
    metrics: list[str],
    filters: list[dict[str, Any]] | None = None,
    use_threshold: bool = False,
    sort_fields: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """
    Request an async CSV-download advertiserReport from the TripleLift reporting API.

    Returns a presigned S3 URL that the caller polls (via
    tl_async_download_report_status) until status == "READY", then downloads
    from. The async path returns AGGREGATE totals — TripleLift's
    AdvertiserDimensionField enum is empty, so per-deal breakdowns are not
    supported through this query. For per-deal data, use the sync
    tl_advertiser_deals_report tool.

    Valid metrics (AdvertiserMetricField): DEAL_SPEND, BILLABLE, CLICKS, CTR,
    CPC, AD_SPEND_ECPM, BID_REQUESTS, BID_RESPONSES, RENDERED,
    BID_RESPONSE_RATE, RENDER_RATE, WINS, WIN_RATE, VIDEO_STARTS,
    VIDEO_STARTS_RATE, VIDEO_COMPLETIONS, VIDEO_COMPLETION_RATE, CPCV.
    """
    query = """
query AsyncDownloadAdvertiserReport(
  $buyerMemberId: String!,
  $startDate: Date!,
  $endDate: Date!,
  $metrics: [AdvertiserMetricField!]!,
  $filters: [AdvertiserIdFilter!]!,
  $useThreshold: Boolean,
  $sortFields: [SortField!]!
) {
  asyncDownloadAdvertiserReport(
    buyerMemberId: $buyerMemberId,
    startDate: $startDate,
    endDate: $endDate,
    dimensions: [],
    metrics: $metrics,
    filters: $filters,
    useThreshold: $useThreshold,
    sortFields: $sortFields
  )
}
"""

    try:
        client = get_triplelift_reporting_client()
        result = await client.graphql(
            query,
            {
                "buyerMemberId": buyer_member_id,
                "startDate": start_date,
                "endDate": end_date,
                "metrics": metrics,
                "filters": filters or [],
                "useThreshold": use_threshold,
                "sortFields": sort_fields or [],
            },
        )
        return {"success": True, "data": result.get("data"), "errors": result.get("errors")}
    except Exception as e:
        return {"success": False, "error": f"Failed TripleLift async download report request: {e}"}


@mcp.tool()
async def tl_async_download_report_status(presigned_url: str) -> dict[str, Any]:
    query = """
query AsyncDownloadReportStatus($presignedUrl: String!) {
  asyncDownloadReportStatus(presignedUrl: $presignedUrl)
}
"""

    try:
        client = get_triplelift_reporting_client()
        result = await client.graphql(query, {"presignedUrl": presigned_url})
        return {"success": True, "data": result.get("data"), "errors": result.get("errors")}
    except Exception as e:
        return {"success": False, "error": f"Failed TripleLift async download report status: {e}"}


@mcp.tool()
async def tl_async_email_advertiser_report(
    buyer_member_id: str,
    start_date: str,
    end_date: str,
    metrics: list[str],
    emails: list[str],
    filters: list[dict[str, Any]] | None = None,
    use_threshold: bool = False,
    sort_fields: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """
    Request an async email-delivered advertiserReport.

    Same aggregate-only constraint as tl_async_download_advertiser_report:
    AdvertiserDimensionField is empty, so the email report cannot break down
    by deal. For per-deal data the caller should use tl_advertiser_deals_report.
    """
    query = """
query AsyncEmailAdvertiserReport(
  $buyerMemberId: String!,
  $startDate: Date!,
  $endDate: Date!,
  $metrics: [AdvertiserMetricField!]!,
  $filters: [AdvertiserIdFilter!]!,
  $emails: [String!]!,
  $useThreshold: Boolean,
  $sortFields: [SortField!]!
) {
  asyncEmailAdvertiserReport(
    buyerMemberId: $buyerMemberId,
    startDate: $startDate,
    endDate: $endDate,
    dimensions: [],
    metrics: $metrics,
    filters: $filters,
    emails: $emails,
    useThreshold: $useThreshold,
    sortFields: $sortFields
  )
}
"""

    try:
        client = get_triplelift_reporting_client()
        result = await client.graphql(
            query,
            {
                "buyerMemberId": buyer_member_id,
                "startDate": start_date,
                "endDate": end_date,
                "metrics": metrics,
                "filters": filters or [],
                "emails": emails,
                "useThreshold": use_threshold,
                "sortFields": sort_fields or [],
            },
        )
        return {"success": True, "data": result.get("data"), "errors": result.get("errors")}
    except Exception as e:
        return {"success": False, "error": f"Failed TripleLift async email report request: {e}"}


def _rows_to_csv(resolved_dimensions: list[str], resolved_metrics: list[str], rows: list[dict[str, Any]]) -> str:
    """Serialize advertiserDealsReport rows to a CSV string with dimensions + metrics columns."""
    header = [*[d.lower() for d in resolved_dimensions], *[m.lower() for m in resolved_metrics]]
    lines = [",".join(header)]
    for row in rows:
        dim_values = {item.get("name"): item.get("value", "") for item in (row.get("dimensions") or [])}
        metric_values: dict[str, Any] = {}
        for item in row.get("metrics") or []:
            name = item.get("name")
            value = item.get("longValue", item.get("decimalValue"))
            if name is not None:
                metric_values[name] = "" if value is None else value
        ordered: list[str] = []
        for dim in resolved_dimensions:
            ordered.append(str(dim_values.get(dim.lower(), "")))
        for metric in resolved_metrics:
            ordered.append(str(metric_values.get(metric.lower(), "")))
        # Escape commas / quotes per RFC 4180
        escaped = []
        for cell in ordered:
            if any(c in cell for c in (",", '"', "\n")):
                escaped.append('"' + cell.replace('"', '""') + '"')
            else:
                escaped.append(cell)
        lines.append(",".join(escaped))
    return "\n".join(lines) + "\n"


@mcp.tool()
async def tl_run_report_from_prompt_inputs(
    deal_member_id: str,
    start_date: str,
    end_date: str,
    breakdowns: list[str] | None = None,
    metrics: list[str] | None = None,
    filename_hint: str | None = None,
    page_size: int = 1000,
    max_pages: int = 25,
) -> dict[str, Any]:
    """
    Run an advertiserDealsReport with per-deal breakdowns, paginate via cursor,
    and write the assembled rows to a CSV on disk. This is the supported path
    for an advertiser-role member (curator) like Elcano, since TripleLift's
    async download endpoint does not allow per-deal dimensions.
    """
    resolved_dimensions = _resolve_triplelift_report_fields(breakdowns, TRIPLELIFT_REPORT_DIMENSION_ALIASES)
    resolved_metrics = _resolve_triplelift_report_fields(metrics, TRIPLELIFT_REPORT_METRIC_ALIASES)

    if not resolved_dimensions:
        resolved_dimensions = ["YMD", "DEAL_NAME", "DEAL_ID"]
    if not resolved_metrics:
        resolved_metrics = ["DEAL_SPEND", "IMPRESSIONS", "BID_REQUESTS", "BID_RESPONSES"]

    all_rows: list[dict[str, Any]] = []
    cursor: str | None = None
    total_rows: int | None = None
    pages_fetched = 0

    while pages_fetched < max_pages:
        page = await tl_advertiser_deals_report(
            deal_member_id=deal_member_id,
            start_date=start_date,
            end_date=end_date,
            dimensions=resolved_dimensions,
            metrics=resolved_metrics,
            cursor=cursor,
            size=page_size,
        )
        pages_fetched += 1
        if not page.get("success"):
            return page
        report = ((page.get("data") or {}).get("advertiserDealsReport")) or {}
        all_rows.extend(report.get("rows") or [])
        total_rows = report.get("totalRows", total_rows)
        cursor = report.get("nextCursor")
        if not cursor:
            break

    csv_text = _rows_to_csv(resolved_dimensions, resolved_metrics, all_rows)
    try:
        download = _write_triplelift_csv_download(filename_hint=filename_hint, csv_text=csv_text)
    except Exception as e:
        return {
            "success": False,
            "error": f"TripleLift report fetched but CSV write failed: {e}",
            "resolved_breakdowns": resolved_dimensions,
            "resolved_metrics": resolved_metrics,
            "row_count": len(all_rows),
        }

    return {
        "success": True,
        "resolved_breakdowns": resolved_dimensions,
        "resolved_metrics": resolved_metrics,
        "row_count": len(all_rows),
        "total_rows": total_rows,
        "pages_fetched": pages_fetched,
        "download": download,
    }


@mcp.tool()
async def tl_create_deal(member_id: int | None = None, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    """
    Create a new curated deal on TripleLift.

    This is a CRITICAL action that requires self-audit confirmation before execution.

    Required payload fields:
    - memberId
    - name
    - active
    - primaryGoalId
    - secondaryGoal
    - budget
    - dealPriceType
    - dealPriceValue
    - startDate
    - endDate
    - commercializedFormats
    - dsp
    - channel
    - isPublisher
    - creativeTags
    - dspFormatWorkflow
    - targetingExpression
    - dealTypeId

    Payload shape gotchas (create/POST differs from the read/GET response):
    - secondaryGoal.value MUST be a plain number on create, e.g.
      {"id": 2, "value": 250}. The GET response returns it as an object
      ({"type": "USD_CENTS", "value": 250}); sending that object back on create
      fails with HTTP 400 "expected number, received object".

    Auto-populated when omitted (TripleLift's validator requires these but the
    OpenAPI POST schema does not list them; missing values produce a 400 or an
    internal .filter() crash on their end):
    - brandAndCreativeControls: empty INCLUDE targeting for iab/brand/advertiser-domain
    - externalCreativeTypeItems: []
    - vendor: {"cintLucidCampaignStudyId": ""}
    - curationFee: 25% percent-model fee (placeholder; override with the
      caller-specific fee once TL confirms the right feeModel.id for our account).
      A wrong/placeholder fee surfaces as warning="FEE_CREATION_ERROR" on an
      otherwise-successful create (the deal is created without the fee), so pass an
      account-valid curationFee when the fee matters.

    Supported dealPriceType values: CEILING, FIXED, FLOOR.
    Supported channel values: WEB, CTV.
    Supported commercializedFormats values:
    BRANDED_VIDEO, CANVAS_VIDEO, CAROUSEL, CINEMAGRAPH, CLICK_TO_PLAY_VIDEO,
    COLLECTION, DISPLAY, DYNAMIC_OVERLAY, ENHANCED_SPOTS, FLIPBOOK,
    HIGH_IMPACT_DISPLAY, IMAGE, INSTREAM, L_BAR, MULTI_ACTION, OUTSTREAM,
    PAUSE_AD, PHARMA, RESPONSIVE_ECOMMERCE, REVEAL, SCROLL, SKU_IMAGE,
    SKU_VIDEO, SPLIT_SCREEN, SPOTS, VERTICAL_VIDEO, WINDOW.

    dsp object structure:
    {"id": <number>, "seat": {"id": <number>, "name": "<string>", "seatString": "<string>"}}

    targetingExpression structure:
    Recursive tree with type (AND/OR/NOT/EQ/ANY/ALL/etc.), binding
    (for example EB_SUPPLY_GEO_COUNTRY_ID), integralTargets/stringTargets,
    and/or children.

    Convenience payload keys are supported and converted into targetingExpression:
    - country_ids: list[int]
    - device_types: list[str]
    - segment_ids: list[int]
    - targeting_operator: "AND" or "OR"
    - allow_political_ads: bool — when true, sets Regulatory Policy ->
      Controlled -> "Include Political Ads Allowed" by folding the
      UI_EXPR_REGULATORY_POLICY_CONTROLLED node into targetingExpression.
      Composes with the other convenience keys / a hand-built targetingExpression,
      and satisfies the required targetingExpression field on its own.
    """
    logger.info("tl_create_deal called")

    try:
        if payload is None:
            return {
                "success": False,
                "error": "payload is required",
            }

        resolved_member_id = _resolve_member_id(member_id)

        # Deep copy to avoid mutating caller payload
        payload_copy = copy.deepcopy(payload)
        payload_copy.setdefault("memberId", resolved_member_id)

        if "targetingExpression" not in payload_copy and any(
            key in payload_copy for key in ("country_ids", "device_types", "segment_ids")
        ):
            payload_copy["targetingExpression"] = _build_targeting_expression(
                country_ids=payload_copy.pop("country_ids", None),
                device_types=payload_copy.pop("device_types", None),
                segment_ids=payload_copy.pop("segment_ids", None),
                operator=payload_copy.pop("targeting_operator", "AND"),
            )

        # Regulatory Policy -> Controlled -> Include Political Ads Allowed.
        # Pop the convenience flag (TripleLift rejects unknown top-level keys) and
        # fold the policy node into targetingExpression. Works whether or not the
        # caller supplied other targeting; it satisfies the required targetingExpression
        # field on its own when no other targeting is present.
        if payload_copy.pop("allow_political_ads", False):
            payload_copy["targetingExpression"] = _apply_regulatory_policy_controlled(
                payload_copy.get("targetingExpression")
            )

        # TripleLift's validator requires these fields but does not list them in the
        # OpenAPI POST schema. Without them the create returns either a 400 validation
        # error or an internal 500 "Cannot read properties of undefined (reading 'filter')".
        # Defaults can be overridden by the caller for deal-specific values.
        payload_copy.setdefault(
            "brandAndCreativeControls",
            {
                "iabCategoryTargeting": {"action": "INCLUDE", "items": []},
                "brandTargeting": {"action": "INCLUDE", "items": []},
                "advertiserDomainTargeting": {"action": "INCLUDE", "items": []},
            },
        )
        payload_copy.setdefault("externalCreativeTypeItems", [])
        payload_copy.setdefault("vendor", {"cintLucidCampaignStudyId": ""})
        payload_copy.setdefault(
            "curationFee",
            {
                "feeModel": {"id": 3, "type": "FEE_MODEL_TYPE_PERCENT"},
                "value": 25,
                "cap": None,
            },
        )

        required_fields = [
            "memberId",
            "name",
            "active",
            "primaryGoalId",
            "secondaryGoal",
            "budget",
            "dealPriceType",
            "dealPriceValue",
            "startDate",
            "endDate",
            "commercializedFormats",
            "dsp",
            "channel",
            "isPublisher",
            "creativeTags",
            "dspFormatWorkflow",
            "targetingExpression",
            "dealTypeId",
        ]

        missing_fields = [field for field in required_fields if field not in payload_copy]
        if missing_fields:
            return {
                "success": False,
                "error": f"Missing required fields: {', '.join(missing_fields)}",
            }

        if payload_copy.get("dealPriceType") not in SUPPORTED_DEAL_PRICE_TYPES:
            return {
                "success": False,
                "error": f"dealPriceType must be one of: {', '.join(sorted(SUPPORTED_DEAL_PRICE_TYPES))}",
            }

        if payload_copy.get("channel") not in SUPPORTED_CHANNELS:
            return {
                "success": False,
                "error": f"channel must be one of: {', '.join(sorted(SUPPORTED_CHANNELS))}",
            }

        commercialized_formats = payload_copy.get("commercializedFormats", [])
        invalid_formats = [value for value in commercialized_formats if value not in SUPPORTED_COMMERCIALIZED_FORMATS]
        if invalid_formats:
            return {
                "success": False,
                "error": (f"Invalid commercializedFormats values: {', '.join(invalid_formats)}"),
            }

        client = get_triplelift_client()
        data = await client.create_deal(member_id=resolved_member_id, payload=payload_copy)

        return {
            "success": True,
            "deal": data.get("deal", data),
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to create deal: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to create deal: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to create deal: {e}",
        }


@mcp.tool()
async def tl_get_deal(member_id: int | None = None, deal_id: int = 0) -> dict[str, Any]:
    """
    Get a single TripleLift deal by ID.

    Returns the full deal envelope: the inner `deal` object plus sibling keys
    `targeting` (the full targeting tree — inspect this to verify regulatory-policy /
    political-ads and all other targeting), `adQualityProfile`, `curationFee`, and
    `dealTypeId`.

    KNOWN TripleLift API quirk: the GET-by-id endpoint can ignore the requested
    deal_id and return a different deal (observed returning the member's
    most-recently-modified deal). When the returned deal id does not match the
    requested deal_id, a `warning` is included and `verified` is set to False —
    do NOT trust this response for verification in that case; reconcile via
    tl_list_deals instead.
    """
    logger.info("tl_get_deal called for deal_id=%d", deal_id)

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        envelope = await client.get_deal(member_id=resolved_member_id, deal_id=deal_id)

        result: dict[str, Any] = {"success": True}
        if isinstance(envelope, dict) and "deal" in envelope:
            result.update(envelope)
        else:
            # Defensive: some shapes may return the bare deal object.
            result["deal"] = envelope

        returned_id = result.get("deal", {}).get("id") if isinstance(result.get("deal"), dict) else None
        if deal_id and returned_id is not None and int(returned_id) != int(deal_id):
            result["verified"] = False
            result["warning"] = (
                f"TripleLift returned deal {returned_id} for requested id {deal_id}; "
                "the GET-by-id endpoint may ignore deal_id. Reconcile via tl_list_deals."
            )
        elif returned_id is not None:
            result["verified"] = True
        return result
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to get deal: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to get deal: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to get deal: {e}",
        }


@mcp.tool()
async def tl_list_deals(
    member_id: int | None = None,
    query: str | None = None,
    order_by: str | None = None,
    sort_dir: str | None = None,
    deal_type_id: int | None = None,
) -> dict[str, Any]:
    """List TripleLift deals with optional search/filter/sort."""
    logger.info("tl_list_deals called")

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        deals = await client.list_deals(
            member_id=resolved_member_id,
            query=query,
            order_by=order_by,
            sort_dir=sort_dir,
            deal_type_id=deal_type_id,
        )
        return {
            "success": True,
            "deals": deals,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to list deals: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to list deals: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to list deals: {e}",
        }


@mcp.tool()
async def tl_update_deal(
    member_id: int | None = None,
    deal_id: int = 0,
    payload: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """
    Update an existing TripleLift deal.

    Only changed fields need to be provided in the payload.
    """
    logger.info("tl_update_deal called for deal_id=%d", deal_id)

    try:
        if payload is None:
            return {
                "success": False,
                "error": "payload is required",
            }

        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        data = await client.update_deal(member_id=resolved_member_id, deal_id=deal_id, payload=copy.deepcopy(payload))
        return {
            "success": True,
            "deal": data.get("deal", data),
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to update deal: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to update deal: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to update deal: {e}",
        }


@mcp.tool()
async def tl_toggle_deal_status(
    member_id: int | None = None,
    deal_id: int = 0,
    active: bool = False,
) -> dict[str, Any]:
    """Activate/deactivate a TripleLift deal."""
    logger.info("tl_toggle_deal_status called for deal_id=%d active=%s", deal_id, active)

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        data = await client.toggle_deal_status(member_id=resolved_member_id, deal_id=deal_id, active=active)
        return {
            "success": True,
            "data": data,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to toggle deal status: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to toggle deal status: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to toggle deal status: {e}",
        }


@mcp.tool()
async def tl_list_buyers(member_id: int | None = None, buyer_id: str | None = None) -> dict[str, Any]:
    """List available buyers/DSPs."""
    logger.info("tl_list_buyers called")

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        buyers = await client.list_buyers(member_id=resolved_member_id, buyer_id=buyer_id)
        return {
            "success": True,
            "buyers": buyers,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to list buyers: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to list buyers: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to list buyers: {e}",
        }


@mcp.tool()
async def tl_list_countries(member_id: int | None = None) -> dict[str, Any]:
    """List geo country targeting values."""
    logger.info("tl_list_countries called")

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        countries = await client.list_countries(member_id=resolved_member_id)
        return {
            "success": True,
            "countries": countries,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to list countries: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to list countries: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to list countries: {e}",
        }


@mcp.tool()
async def tl_list_segments(member_id: int | None = None, with_description: bool = False) -> dict[str, Any]:
    """List audience segments."""
    logger.info("tl_list_segments called")

    try:
        resolved_member_id = _resolve_member_id(member_id)
        client = get_triplelift_client()
        segments = await client.list_segments(member_id=resolved_member_id, with_description=with_description)
        return {
            "success": True,
            "segments": segments,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to list segments: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to list segments: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to list segments: {e}",
        }


@mcp.tool()
async def tl_get_avails(
    member_id: int | None = None,
    channel: str = "WEB",
    targeting_expression: dict[str, Any] | None = None,
    commercialized_formats: list[str] | None = None,
) -> dict[str, Any]:
    """Estimate inventory availability for the provided targeting criteria."""
    logger.info("tl_get_avails called for channel=%s", channel)

    try:
        resolved_member_id = _resolve_member_id(member_id)

        if channel not in SUPPORTED_CHANNELS:
            return {
                "success": False,
                "error": f"channel must be one of: {', '.join(sorted(SUPPORTED_CHANNELS))}",
            }

        payload: dict[str, Any] = {
            "channel": channel,
            "targetingExpression": targeting_expression or _build_targeting_expression(),
        }
        if commercialized_formats is not None:
            payload["commercializedFormats"] = commercialized_formats

        client = get_triplelift_client()
        data = await client.get_avails(member_id=resolved_member_id, payload=payload)

        avails_count = data.get("availsCount")
        if avails_count is None:
            avails_count = data.get("targetedAvailsCount", data.get("count", 0))

        unsupported_bindings = data.get("unsupportedBindings", [])

        return {
            "success": True,
            "avails_count": int(avails_count),
            "unsupported_bindings": unsupported_bindings,
        }
    except ValueError as e:
        return {
            "success": False,
            "error": f"Failed to get avails: {e}",
        }
    except httpx.HTTPStatusError as e:
        return {
            "success": False,
            "error": f"Failed to get avails: HTTP {e.response.status_code}: {e.response.text}",
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"Failed to get avails: {e}",
        }


if __name__ == "__main__":
    logger.info("Starting TripleLift Curation MCP Server")
    has_credentials = bool(os.environ.get("TRIPLELIFT_CLIENT_ID")) and bool(os.environ.get("TRIPLELIFT_CLIENT_SECRET"))
    if not has_credentials:
        logger.warning(
            "TripleLift not configured. Set TRIPLELIFT_CLIENT_ID and TRIPLELIFT_CLIENT_SECRET to enable deal management."
        )
    try:
        mcp.run(transport="stdio")
    except Exception as e:
        logger.error("Failed to start server: %s", e)
        sys.exit(1)
