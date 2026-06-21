#!/usr/bin/env python3
"""
Gamma MCP Server

A simple MCP server for Gamma AI presentation generation.

Optional in the chat server: registered via MCPServerSpec with
Optional=true so it only participates in a turn when the conversation
has opted in. Ported from cutlass/mcp/gamma.py; keep the two in sync
when upstream Gamma API changes require edits.
"""

import asyncio
import logging
import os
import re
import sys
from datetime import datetime
from enum import Enum
from pathlib import Path
from typing import Any

import httpx
from mcp.server.fastmcp import FastMCP

# Configure logging to stderr (not stdout for STDIO transport)
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s", stream=sys.stderr
)
logger = logging.getLogger(__name__)

# Initialize FastMCP server
mcp = FastMCP("gamma")

# Constants
GAMMA_API_BASE = "https://public-api.gamma.app/v0.2"
GAMMA_API_V1_BASE = "https://public-api.gamma.app/v1.0"
USER_AGENT = "elcanotek-chat/1.0"
DEFAULT_TIMEOUT = 60.0  # Increased timeout for presentation generation

# Polling configuration
INITIAL_POLLING_INTERVAL = 2  # seconds
POLLING_BACKOFF_FACTOR = 1.5
MAX_POLLING_INTERVAL = 30  # seconds
POLLING_TIMEOUT_SECONDS = 300  # 5 minutes total

# Template IDs
WRAP_UP_PROTOCOL_TEMPLATE_ID = "g_vzunwtnstnq4oag"


DEFAULT_REPORT_DOWNLOAD_DIR = os.path.expanduser("~/Victoria/gamma_decks")


def _pick_api_key(account: str | None) -> tuple[str | None, str | None]:
    """Resolve the API key to use for a generation call.

    Returns (api_key, error_message). On success error_message is None.
    Resolution order:
      - account="brad" or account="brad@elcanotek.com" → GAMMA_API_KEY_BRAD
      - account=None or "" → GAMMA_API_KEY (shared/default)
      - first match wins; missing both is the error case.

    The chat user's email (brad@elcanotek.com) and the bare first name
    ("brad") both resolve to the same key — that's the natural form
    callers will pass when threading user identity through the agent.
    """
    if account:
        token = account.split("@", 1)[0].strip()
        if token:
            var = f"GAMMA_API_KEY_{token.upper()}"
            value = os.environ.get(var)
            if value:
                return value, None
            return None, (
                f"No Gamma API key configured for account '{account}'. "
                f"Operator should set {var} in /opt/chat/.env.local. "
                "Configured accounts: " + ", ".join(_list_account_names()) + "."
            )
    shared = os.environ.get("GAMMA_API_KEY")
    if shared:
        return shared, None
    accounts = _list_account_names()
    if accounts:
        return None, (
            "GAMMA_API_KEY (shared) is not set; pass an account name instead. "
            "Configured accounts: " + ", ".join(accounts) + "."
        )
    return None, "Gamma not configured (no GAMMA_API_KEY or GAMMA_API_KEY_<NAME> set)."


def _list_account_names() -> list[str]:
    """Return the lowercase account names derived from GAMMA_API_KEY_<NAME> env vars."""
    out: list[str] = []
    for key in os.environ:
        upper = key.upper()
        if upper.startswith("GAMMA_API_KEY_") and os.environ.get(key):
            name = upper[len("GAMMA_API_KEY_") :].lower()
            if name:
                out.append(name)
    return sorted(set(out))


async def make_gamma_request(
    method: str,
    url: str,
    json: dict[str, Any] | None = None,
    account: str | None = None,
) -> dict[str, Any]:
    """Make a request to the Gamma API with proper error handling.

    account picks which Gamma user's API key to use (and therefore whose
    credit balance to spend). Pass None for the shared GAMMA_API_KEY.
    """
    api_key, err = _pick_api_key(account)
    if err or not api_key:
        logger.error(err)
        return {"error": err or "no API key"}

    headers = {
        "User-Agent": USER_AGENT,
        "Content-Type": "application/json",
        "X-API-KEY": api_key,
        "accept": "application/json",
    }

    logger.info(f"Making {method} request to {url}")

    try:
        async with httpx.AsyncClient(timeout=DEFAULT_TIMEOUT) as client:
            if method.upper() == "POST":
                response = await client.post(url, headers=headers, json=json)
            else:
                response = await client.get(url, headers=headers)

            logger.info(f"Response status: {response.status_code}")
            response.raise_for_status()
            result = response.json()
            logger.info("Request completed successfully")
            return result

    except httpx.TimeoutException as e:
        error_msg = f"Request timeout after {DEFAULT_TIMEOUT}s: {str(e)}"
        logger.error(error_msg)
        return {"error": error_msg}
    except httpx.HTTPStatusError as e:
        error_msg = f"HTTP error: {e.response.status_code} - {e.response.text}"
        logger.error(error_msg)
        return {"error": error_msg}
    except httpx.RequestError as e:
        error_msg = f"Request error: {str(e)}"
        logger.error(error_msg)
        return {"error": error_msg}
    except Exception as e:
        error_msg = f"Unexpected error: {str(e)}"
        logger.error(error_msg)
        return {"error": error_msg}


async def _save_export_to_disk(
    *,
    export_url: str,
    gamma_id: str,
    title_hint: str,
    api_key: str,
    output_dir: str | None,
    export_format: str,
) -> dict[str, Any]:
    """Download a completed presentation's exportUrl to local disk.

    Returns a dict with `path`, `bytes`, and `error` (when applicable).
    Saves into output_dir if given, else DEFAULT_REPORT_DOWNLOAD_DIR.
    The Gamma export URL requires the same X-API-KEY header that
    generated the presentation — anonymous fetches 502 at the CDN.

    Letting the agent specify output_dir lets it pass the per-conv
    workspace path (e.g. /opt/chat/workspace/<convID>/) so the chat UI
    can render the file via the workspace API. Default fallback is
    ~/Victoria/gamma_decks/, mirroring the SSP MCPs.
    """
    target_dir = Path(output_dir).expanduser() if output_dir else Path(DEFAULT_REPORT_DOWNLOAD_DIR).expanduser()
    try:
        target_dir.mkdir(parents=True, exist_ok=True)
    except OSError as e:
        return {"error": f"could not create output dir {target_dir}: {e}"}

    safe_title = re.sub(r"[^A-Za-z0-9._-]+", "_", (title_hint or "deck")).strip("_")[:60] or "deck"
    ext = export_format.lower().lstrip(".") or "pptx"
    filename = f"{safe_title}_{gamma_id}.{ext}"
    target = target_dir / filename

    headers = {"X-API-KEY": api_key, "accept": "*/*"}
    try:
        async with httpx.AsyncClient(timeout=DEFAULT_TIMEOUT) as client:
            response = await client.get(export_url, headers=headers)
            response.raise_for_status()
            content = response.content
    except httpx.HTTPStatusError as e:
        return {"error": f"export download failed: HTTP {e.response.status_code}"}
    except Exception as e:
        return {"error": f"export download failed: {type(e).__name__}: {e}"}

    target.write_bytes(content)
    return {"path": str(target), "bytes": len(content)}


# Agent-facing warning attached to any result that still carries an
# exportUrl. The URL authenticates with this server's X-API-KEY header;
# an anonymous browser GET 502s at Gamma's CDN, so it must NEVER be
# pasted into a chat reply as a download link (bug report, conv
# 79af2721: the model offered "Download from Gamma CDN" and the user got
# a dead link — "i cant download this").
EXPORT_URL_WARNING = (
    "exportUrl is backend-only: it authenticates with this server's X-API-KEY header and "
    "anonymous browser fetches fail with HTTP 502. NEVER give it to the user as a download "
    "link. To deliver the deck, save it locally via wait_for_presentation_completion with "
    "output_dir set to the absolute per-conversation workspace path, then link the saved "
    "file by bare relative filename."
)


def _present_completion_for_chat(result: dict[str, Any]) -> dict[str, Any]:
    """Shape a completed-generation result so a chat model can't hand the
    user a link that doesn't work in a browser.

    The Gamma exportUrl requires the generating account's X-API-KEY header —
    an anonymous browser GET 502s at the CDN. Models that see the field in a
    tool result naturally paste it into replies as "Download from Gamma CDN",
    and the user gets a dead link (bug report, conv 79af2721). The export is
    ALREADY saved to local disk precisely so the chat UI can serve it, so
    once that save succeeded the export URL has done its only job: drop it
    and replace it with a ready-to-paste markdown link (poka-yoke — the
    un-shareable URL is no longer there to share).

    When the local save failed (or never ran), the exportUrl is kept so the
    agent can retry the save with a corrected output_dir — annotated with
    EXPORT_URL_WARNING so it still never reaches the user.
    """
    download = result.get("download")
    saved_path = download.get("path") if isinstance(download, dict) else None
    if saved_path:
        result.pop("exportUrl", None)
        result.pop("exportUrlWarning", None)
        filename = Path(saved_path).name
        result["user_delivery"] = {
            "markdown_link": f"[{filename}]({filename})",
            "instructions": (
                "To hand the user this deck, paste markdown_link verbatim in your reply — the "
                "chat UI rewrites a bare relative filename to an authenticated workspace download "
                "URL. This works when the file was saved into this conversation's workspace dir "
                f"(saved to: {saved_path}); if it was saved elsewhere, re-run "
                "wait_for_presentation_completion with output_dir set to the absolute workspace "
                "path first. Do NOT present gammaUrl as the download (it needs a Gamma seat; offer "
                "it only as an optional edit/share link) and NEVER link assets.api.gamma.app "
                "export URLs — they are backend-only and fail in a browser."
            ),
        }
    elif "exportUrl" in result:
        result["exportUrlWarning"] = EXPORT_URL_WARNING
    return result


DEFAULT_THEME = "Elcano"
AVAILABLE_THEMES = {"Elcano"}

# Default presentation instructions for Campaign Wrap-Up Protocol
DEFAULT_ADDITIONAL_INSTRUCTIONS = (
    "Use the Elcano theme with standard title and thank you slides. "
    "For Campaign Wrap-Up Protocol presentations, follow this 10-slide structure: "
    "1) Title: Client name/logo left, Elcano logo right, campaign year bottom-center; full-width card with Elcano Gamma accent image. "
    "2) Executive Summary: Four metrics at top (Investment, Conversions, Rate, CPA); Highlights bottom-left; Recommendations bottom-right. "
    "3) Platform Analysis: Table of metrics top; donut chart conversions bottom-left; key insights bottom-right. "
    "4) Lifecycle Optimization: Line chart CPA left; optimization actions right. "
    "5) Conversion Spikes: Line chart left; event details right; Immediate Actions bottom. "
    "6) Winning Inventory: Bar chart conversions by theme left; Content Theme Analysis right; Recommendation bottom. "
    "7) Geographic Insights: Heat map top 3 DMAs left; DMA Analysis right; table of DMA, CTR, Spend Share. "
    "8) Day of Week: Bar chart conversions by day top; 4 highlights in cards below. "
    "9) Key Learnings & Next Steps: Two sections. "
    "10) Thank You: Simple message with Elcano logo. "
    "Charts & visuals: "
    "1) Use the right type (bar for comparisons, line for trends, pie/donut for proportions, maps for geo). "
    "2) Add clear titles and axis labels. "
    "3) Use Elcano brand colors. "
    "4) Sort data logically. "
    "5) Add labels if readability improves. "
    "6) Highlight only key insights. "
    "7) Keep formatting clean with spacing, headings, readable fonts. "
    "8) Use styling (borders, shadows, padding) to group important insights, summaries, or recs; apply sparingly for emphasis."
)


def _resolve_theme_name(theme_name: str) -> str:
    """Normalize a requested theme to one of the supported Gamma themes."""

    requested = theme_name.strip()
    if requested in AVAILABLE_THEMES:
        return requested

    normalized = requested.replace("-", "_").replace(" ", "_").casefold()
    for theme in AVAILABLE_THEMES:
        if normalized == theme.casefold():
            return theme

    logger.warning(
        "Unsupported theme '%s' requested. Falling back to default theme '%s'.",
        theme_name,
        DEFAULT_THEME,
    )
    return DEFAULT_THEME


@mcp.tool()
async def generate_presentation(
    input_text: str,
    layout_format: str = "16x9",
    additional_instructions: str = DEFAULT_ADDITIONAL_INSTRUCTIONS,
    export_as: str = "pptx",
    text_mode: str = "generate",
    theme_id: str | None = None,
    account: str | None = None,
) -> dict[str, Any]:
    """Generate a presentation using the Gamma v1.0 API.

    Use this for ad-hoc decks built from a markdown brief. Use
    generate_wrap_up_presentation when you specifically want Elcano's
    Campaign Wrap-Up template.

    Args:
        input_text: Markdown content for the presentation. The model controls
          structure (use `---` between slides, `# H1`, `## H2`, bullets, etc).
        layout_format: One of "16x9" (default), "4x3", or "fluid".
        additional_instructions: Free-form guidance to the Gamma generator
          (tone, layout preferences, image policy).
        export_as: "pptx" (default) or "pdf". Both produce a downloadable file
          on completion via wait_for_presentation_completion.
        text_mode: How Gamma treats input_text. Default "generate" lets Gamma
          expand and rewrite; "preserve" keeps your text verbatim;
          "condense" tightens it.
        theme_id: Optional Gamma theme id. Omit for the user's default theme.
          The legacy `theme_name` argument was sunset with v0.2; Gamma's v1.0
          API requires the opaque theme id from the operator's Gamma workspace.
        account: Whose Gamma account (and credit balance) to use. Pass a name
          like "brad" or an email like "brad@elcanotek.com". Resolves to the
          GAMMA_API_KEY_<UPPER> env var. Omit to use the shared GAMMA_API_KEY.
          When the user is fuzzy about whose account ("make me a deck"), check
          list_gamma_accounts and ask if there's more than one configured.

    Returns:
        {generationId, ...} on success — pass to wait_for_presentation_completion
        (with output_dir set to the workspace path) to download the file and get
        the user_delivery markdown for handing it to the user.
    """
    logger.info("Generating presentation: format=%s, account=%s", export_as, account or "<shared>")

    url = f"{GAMMA_API_V1_BASE}/generations"
    payload: dict[str, Any] = {
        "inputText": input_text,
        "format": "presentation",
        "textMode": text_mode,
        "cardOptions": {"dimensions": layout_format},
        "additionalInstructions": additional_instructions,
        "imageOptions": {"source": "noImages"},
        "exportAs": export_as,
    }
    if theme_id:
        payload["themeId"] = theme_id

    result = await make_gamma_request("POST", url, json=payload, account=account)

    if "error" not in result:
        logger.info("Presentation generation started successfully")
        # Stash the account on the result so the polling step can use the
        # SAME API key when the export URL is downloaded — anonymous fetches
        # of the export URL 502 at Gamma's CDN.
        if isinstance(result, dict):
            result.setdefault("_account", account)

    return result


@mcp.tool()
async def generate_wrap_up_presentation(
    client_name: str,
    campaign_data: str,
    client_logo_url: str | None = None,
    campaign_year: int | None = None,
    theme_id: str | None = None,
    folder_ids: list[str] | None = None,
    export_as: str = "pptx",
    image_model: str | None = None,
    image_style: str | None = None,
    account: str | None = None,
) -> dict[str, Any]:
    """
    Generate a Campaign Wrap-Up presentation using the predefined Gamma template.

    This function uses Gamma's template API (v1.0) to create presentations following the
    Campaign Wrap-Up Protocol structure. The template (g_vzunwtnstnq4oag) includes
    predefined pages and layouts optimized for campaign analysis reporting.

    IMPORTANT: The template includes static slides that will be preserved as-is:
    - "How We Did It" slide (methodology overview)
    - "Meet Victoria" slide (platform introduction)
    - "Thank You" slide (closing slide)

    These slides do not need any data input and will be copied directly from the template.

    Args:
        client_name: Name of the client for the wrap-up (e.g., "Acme Corp").
                    Used for the presentation title and title slide.
        campaign_data: Campaign metrics, insights, and analysis data to populate the slides.
                      Should include: Executive Summary metrics, Platform Performance,
                      Campaign Lifecycle, Geographic Insights, Temporal Analysis,
                      Key Learnings, and Strategic Recommendations.
        client_logo_url: Optional URL to the client's logo image for the title slide
        campaign_year: Optional campaign year (defaults to current year if not provided)
        theme_id: Optional theme ID to override the template's default theme
        folder_ids: Optional list of folder IDs where the gamma should be stored
        export_as: Export format - "pdf" or "pptx" (default: pptx)
        image_model: Optional AI image model to use (e.g., "flux-1-pro", "imagen-4-pro")
        image_style: Optional style description for AI-generated images (e.g., "photorealistic")

    Returns:
        Dictionary containing the generation ID or error information
    """
    logger.info(f"Generating Campaign Wrap-Up presentation for {client_name}")

    # Use current year if not specified
    if campaign_year is None:
        campaign_year = datetime.now().year

    # Construct the presentation title for logging
    presentation_title = f"{client_name} Wrap Up"

    # Build a simple prompt focused only on what needs to change
    # Do NOT mention slides that should remain unchanged - they'll be preserved automatically
    structured_prompt = f"""Update this campaign wrap-up presentation for {client_name}.

Title Slide:
- Client: {client_name}
- Year: {campaign_year}"""

    if client_logo_url:
        structured_prompt += f"\n- Client logo: {client_logo_url}"

    structured_prompt += f"""

Campaign Data for Executive Summary, Platform Performance, Campaign Lifecycle, Geographic Insights, Temporal Analysis, and Key Learnings slides:

{campaign_data}"""

    url = f"{GAMMA_API_V1_BASE}/generations/from-template"
    payload = {
        "gammaId": WRAP_UP_PROTOCOL_TEMPLATE_ID,
        "prompt": structured_prompt,
        "exportAs": export_as,
    }

    # Add optional parameters
    if theme_id:
        payload["themeId"] = theme_id
    if folder_ids:
        payload["folderIds"] = folder_ids

    # Add image options if specified
    if image_model or image_style:
        image_options = {}
        if image_model:
            image_options["model"] = image_model
        if image_style:
            image_options["style"] = image_style
        payload["imageOptions"] = image_options

    result = await make_gamma_request("POST", url, json=payload, account=account)

    if "error" not in result:
        logger.info(f"Wrap-Up presentation '{presentation_title}' generation started successfully")
        if isinstance(result, dict):
            result.setdefault("_account", account)

    return result


@mcp.tool()
async def generate_standard_presentation(
    input_text: str,
    layout_format: str = "16x9",
    export_as: str = "pptx",
    text_mode: str = "generate",
    theme_id: str | None = None,
    account: str | None = None,
) -> dict[str, Any]:
    """Generate a standard ad-hoc presentation via the Gamma v1.0 API.

    Lightweight wrapper for the common case where the agent has a markdown
    deck draft and wants Gamma to render it. For Campaign Wrap-Ups use
    generate_wrap_up_presentation; for full control use generate_presentation.

    Args:
        input_text: Markdown content. Use `---` to separate slides.
        layout_format: "16x9" (default), "4x3", or "fluid".
        export_as: "pptx" (default) or "pdf".
        text_mode: "generate" (default; Gamma rewrites/expands), "preserve"
          (verbatim), or "condense".
        theme_id: Optional Gamma theme id. Omit for the user's default theme.
        account: Whose Gamma account to use. See generate_presentation for
          the resolution rules.

    Returns:
        {generationId, ...} on success.
    """
    logger.info("Generating standard presentation (account=%s)", account or "<shared>")

    url = f"{GAMMA_API_V1_BASE}/generations"
    payload: dict[str, Any] = {
        "inputText": input_text,
        "format": "presentation",
        "textMode": text_mode,
        "cardOptions": {"dimensions": layout_format},
        "additionalInstructions": "Clean, professional styling. Keep formatting simple and readable.",
        "imageOptions": {"source": "noImages"},
        "exportAs": export_as,
    }
    if theme_id:
        payload["themeId"] = theme_id

    result = await make_gamma_request("POST", url, json=payload, account=account)

    if "error" not in result:
        logger.info("Standard presentation generation started successfully")
        if isinstance(result, dict):
            result.setdefault("_account", account)

    return result


@mcp.tool()
async def list_gamma_accounts() -> dict[str, Any]:
    """List the Gamma user accounts the operator has configured.

    Each entry corresponds to a `GAMMA_API_KEY_<NAME>` env var on the chat
    server. The lower-cased <NAME> is what you pass as the `account`
    argument to the generation tools (e.g. account="brad").

    The shared `GAMMA_API_KEY` (no suffix) is the fallback used when no
    account is specified — present in the response as `shared_configured`.
    """
    return {
        "success": True,
        "accounts": _list_account_names(),
        "shared_configured": bool(os.environ.get("GAMMA_API_KEY")),
    }


@mcp.tool()
async def check_presentation_status(generation_id: str, account: str | None = None) -> dict[str, Any]:
    """Check the status of a presentation generation.

    A completed status response carries the raw Gamma exportUrl. It is
    backend-only (X-API-KEY auth; browsers get HTTP 502) — NEVER give it
    to the user as a download link. To deliver the file, call
    wait_for_presentation_completion with output_dir set to the workspace
    path and use the user_delivery markdown it returns.

    Args:
        generation_id: The ID of the generation to check.
        account: The account whose API key generated it. Pass the same
          value used in the matching generate_* call so the status query
          authenticates with the right key.
    """
    logger.info("Checking status for generation ID: %s (account=%s)", generation_id, account or "<shared>")

    url = f"{GAMMA_API_V1_BASE}/generations/{generation_id}"
    result = await make_gamma_request("GET", url, account=account)

    if "error" not in result:
        status = result.get("status", "unknown")
        logger.info(f"Generation status: {status}")
        # Annotate (don't drop — wait_for_presentation_completion reads it
        # to run the local save) so a model that surfaces this raw result
        # knows the URL is not user-shareable.
        if "exportUrl" in result:
            result["exportUrlWarning"] = EXPORT_URL_WARNING

    return result


async def poll_with_backoff(generation_id: str, account: str | None = None) -> dict[str, Any]:
    """Poll for presentation status with exponential backoff."""
    start_time = datetime.now()
    attempt = 0
    polling_interval = INITIAL_POLLING_INTERVAL

    while (datetime.now() - start_time).total_seconds() < POLLING_TIMEOUT_SECONDS:
        attempt += 1
        logger.info(f"Polling attempt {attempt} for generation ID: {generation_id}")

        result = await check_presentation_status(generation_id, account=account)
        if "error" in result:
            return result

        status = result.get("status", "unknown")
        if status in ["completed", "failed"]:
            return result

        logger.info(f"Status is '{status}', waiting {polling_interval:.1f} seconds...")
        await asyncio.sleep(polling_interval)

        # Increase polling interval for next attempt
        polling_interval = min(polling_interval * POLLING_BACKOFF_FACTOR, MAX_POLLING_INTERVAL)

    return {
        "error": f"Polling timed out after {POLLING_TIMEOUT_SECONDS} seconds",
        "generationId": generation_id,
        "status": "timed_out",
    }


@mcp.tool()
async def wait_for_presentation_completion(
    generation_id: str,
    account: str | None = None,
    output_dir: str | None = None,
    title_hint: str | None = None,
) -> dict[str, Any]:
    """Poll until the presentation is ready, then download the export to disk.

    This is the second half of the standard generate-then-wait flow. The
    response from generate_presentation includes a `_account` field that
    you can pass straight back as `account` here so the polling + download
    use the same API key the generation used.

    On completion the response includes:
      - gammaUrl      — link to edit/share in the Gamma UI. Requires a Gamma
        seat; offer it as an optional edit link, never as THE download.
      - download      — { path, bytes } — local file saved for you.
      - user_delivery — { markdown_link, instructions } — the exact markdown
        to paste in your reply so the user gets a working download link.
        This is the ONLY supported way to deliver the file to the user.

    The raw Gamma exportUrl is intentionally absent after a successful local
    save: it authenticates with this server's API key and anonymous browser
    fetches fail with HTTP 502, so it must never be shared with the user. It
    only appears (with exportUrlWarning) when the local save failed, so you
    can fix output_dir and retry.

    Args:
        generation_id: The ID returned by a generate_* call.
        account: Whose API key to authenticate with. MUST match the account
          used at generation time. If you saved the original result, pass
          its `_account` field.
        output_dir: Where to save the downloaded file. Pass the per-conv
          workspace dir (e.g. /opt/chat/workspace/<convID>/) so the chat
          UI can render the file. Falls back to ~/Victoria/gamma_decks/.
        title_hint: Used as the filename stem when present (sanitized).
          Defaults to "deck".
    """
    logger.info(f"Starting automatic polling for generation ID: {generation_id}")

    result = await poll_with_backoff(generation_id, account=account)

    status = result.get("status", "unknown")
    if status == "completed":
        logger.info("Presentation generation completed successfully!")
        export_url = result.get("exportUrl")
        gamma_id = result.get("gammaId") or generation_id
        if export_url:
            api_key, err = _pick_api_key(account)
            if api_key:
                fmt = "pdf" if "/pdf/" in export_url or export_url.endswith(".pdf") else "pptx"
                download = await _save_export_to_disk(
                    export_url=export_url,
                    gamma_id=gamma_id,
                    title_hint=title_hint or "deck",
                    api_key=api_key,
                    output_dir=output_dir,
                    export_format=fmt,
                )
                result["download"] = download
            else:
                result["download"] = {"error": err or "no API key for download"}
        result = _present_completion_for_chat(result)
    elif status == "failed":
        logger.error("Presentation generation failed")
    elif status == "timed_out":
        logger.warning(f"Polling timed out for generation ID: {generation_id}")

    return result


@mcp.tool()
async def generate_and_wait_for_presentation(
    input_text: str,
    theme_name: str = DEFAULT_THEME,
    layout_format: str = "16x9",
    additional_instructions: str = DEFAULT_ADDITIONAL_INSTRUCTIONS,
    export_as: str = "pptx",
    output_dir: str | None = None,
    title_hint: str | None = None,
) -> dict[str, Any]:
    """
    Generate a presentation and automatically wait for completion.

    This is a convenience function that combines generate_presentation and
    wait_for_presentation_completion into a single call. It will start the
    generation and then automatically poll with adaptive backoff until the
    presentation is ready.

    Args:
        input_text: The markdown content for the presentation
        theme_name: The name of the theme to use (default: Elcano)
        layout_format: Layout format for the presentation. Options: "16x9" (Traditional), "4x3" (Tall), "fluid" (Default/Fluid). Default: "16x9"
        additional_instructions: Additional instructions for generation
        export_as: Export format (default: pptx)
        output_dir: Where to save the exported file. Pass the absolute
          per-conversation workspace path so the chat UI can serve the
          file to the user (see user_delivery in the result). Falls back
          to ~/Victoria/gamma_decks/, which the chat UI canNOT serve.
        title_hint: Filename stem for the saved file (sanitized).

    Returns:
        Dictionary containing the final generation result or error information
    """
    logger.info("Starting presentation generation with automatic completion waiting")

    # Start the generation
    generation_result = await generate_presentation(
        input_text=input_text,
        theme_name=theme_name,
        layout_format=layout_format,
        additional_instructions=additional_instructions,
        export_as=export_as,
    )

    # Check if generation started successfully
    if "error" in generation_result:
        logger.error("Failed to start presentation generation")
        return generation_result

    generation_id = generation_result.get("generationId")
    if not generation_id:
        logger.error("No generation ID returned from generation request")
        return {"error": "No generation ID returned from generation request"}

    logger.info(f"Generation started successfully with ID: {generation_id}")
    logger.info("Now waiting for completion...")

    # Wait for completion
    completion_result = await wait_for_presentation_completion(
        generation_id=generation_id, output_dir=output_dir, title_hint=title_hint
    )

    return completion_result


@mcp.tool()
async def generate_and_wait_for_wrap_up_presentation(
    client_name: str,
    campaign_data: str,
    client_logo_url: str | None = None,
    campaign_year: int | None = None,
    theme_id: str | None = None,
    folder_ids: list[str] | None = None,
    export_as: str = "pptx",
    image_model: str | None = None,
    image_style: str | None = None,
    output_dir: str | None = None,
    title_hint: str | None = None,
) -> dict[str, Any]:
    """
    Generate a Campaign Wrap-Up presentation from template and automatically wait for completion.

    This is a convenience function that combines generate_wrap_up_presentation and
    wait_for_presentation_completion into a single call. It will start the generation
    using the predefined Campaign Wrap-Up Protocol template and then automatically
    poll with adaptive backoff until the presentation is ready.

    Args:
        client_name: Name of the client for the wrap-up (e.g., "Acme Corp")
        campaign_data: Campaign metrics, insights, and analysis data to populate the slides
        client_logo_url: Optional URL to the client's logo image for the title slide
        campaign_year: Optional campaign year (defaults to current year if not provided)
        theme_id: Optional theme ID to override the template's default theme
        folder_ids: Optional list of folder IDs where the gamma should be stored
        export_as: Export format - "pdf" or "pptx" (default: pptx)
        image_model: Optional AI image model to use (e.g., "flux-1-pro", "imagen-4-pro")
        image_style: Optional style description for AI-generated images
        output_dir: Where to save the exported file. Pass the absolute
          per-conversation workspace path so the chat UI can serve the
          file to the user (see user_delivery in the result). Falls back
          to ~/Victoria/gamma_decks/, which the chat UI canNOT serve.
        title_hint: Filename stem for the saved file (sanitized).
          Defaults to the client name.

    Returns:
        Dictionary containing the final generation result or error information
    """
    logger.info(
        f"Starting Campaign Wrap-Up presentation generation for {client_name} with automatic completion waiting"
    )

    # Start the generation
    generation_result = await generate_wrap_up_presentation(
        client_name=client_name,
        campaign_data=campaign_data,
        client_logo_url=client_logo_url,
        campaign_year=campaign_year,
        theme_id=theme_id,
        folder_ids=folder_ids,
        export_as=export_as,
        image_model=image_model,
        image_style=image_style,
    )

    # Check if generation started successfully
    if "error" in generation_result:
        logger.error("Failed to start wrap-up presentation generation")
        return generation_result

    generation_id = generation_result.get("generationId")
    if not generation_id:
        logger.error("No generation ID returned from generation request")
        return {"error": "No generation ID returned from generation request"}

    logger.info(f"Wrap-Up generation started successfully with ID: {generation_id}")
    logger.info("Now waiting for completion...")

    # Wait for completion
    completion_result = await wait_for_presentation_completion(
        generation_id=generation_id, output_dir=output_dir, title_hint=title_hint or client_name
    )

    return completion_result


@mcp.tool()
async def generate_and_wait_for_standard_presentation(
    input_text: str,
    layout_format: str = "16x9",
    export_as: str = "pptx",
    output_dir: str | None = None,
    title_hint: str | None = None,
) -> dict[str, Any]:
    """
    Generate a standard presentation with Elcano theme and automatically wait for completion.

    This is a convenience function that combines generate_standard_presentation and
    wait_for_presentation_completion into a single call. It will start the generation
    with simple Elcano theme styling and then automatically poll with adaptive backoff
    until the presentation is ready.

    Args:
        input_text: The markdown content for the presentation
        layout_format: Layout format for the presentation. Options: "16x9" (Traditional), "4x3" (Tall), "fluid" (Default/Fluid). Default: "16x9"
        export_as: Export format (default: pptx)
        output_dir: Where to save the exported file. Pass the absolute
          per-conversation workspace path so the chat UI can serve the
          file to the user (see user_delivery in the result). Falls back
          to ~/Victoria/gamma_decks/, which the chat UI canNOT serve.
        title_hint: Filename stem for the saved file (sanitized).

    Returns:
        Dictionary containing the final generation result or error information
    """
    logger.info("Starting standard presentation generation with automatic completion waiting")

    # Start the generation
    generation_result = await generate_standard_presentation(
        input_text=input_text, layout_format=layout_format, export_as=export_as
    )

    # Check if generation started successfully
    if "error" in generation_result:
        logger.error("Failed to start standard presentation generation")
        return generation_result

    generation_id = generation_result.get("generationId")
    if not generation_id:
        logger.error("No generation ID returned from generation request")
        return {"error": "No generation ID returned from generation request"}

    logger.info(f"Standard generation started successfully with ID: {generation_id}")
    logger.info("Now waiting for completion...")

    # Wait for completion
    completion_result = await wait_for_presentation_completion(
        generation_id=generation_id, output_dir=output_dir, title_hint=title_hint
    )

    return completion_result


# Chart Brief Generation Functionality


class ChartType(Enum):
    """Supported chart types for Gamma presentations."""

    BAR = "bar"
    COLUMN = "column"
    LINE = "line"
    PIE = "pie"
    DONUT = "donut"
    SCATTER = "scatter"
    BUBBLE = "bubble"
    HEATMAP = "heatmap"
    AREA = "area"


@mcp.tool()
async def generate_chart_brief(
    chart_type: str,
    title: str,
    data: list[dict[str, Any]],
    x_axis_title: str = "",
    y_axis_title: str = "",
    key_insight: str = "",
    color_palette: str = "Elcano brand colors",
    sort_order: str = "",
) -> dict[str, Any]:
    """
    Generate a structured chart brief for Gamma AI presentations.

    This tool creates professional chart briefs that follow the Chart Brief Template
    documented in VICTORIA.md, ensuring high-quality visualizations in presentations.

    Args:
        chart_type: Type of chart (bar, column, line, pie, donut, scatter, bubble, heatmap, area)
        title: Clear, descriptive title for the chart
        data: List of data dictionaries containing the chart data
        x_axis_title: Title for X-axis (optional)
        y_axis_title: Title for Y-axis (optional)
        key_insight: Main takeaway to highlight (optional)
        color_palette: Color palette to use (default: Elcano brand colors)
        sort_order: How to organize the data (optional)

    Returns:
        Dictionary containing the formatted chart brief and metadata
    """
    try:
        # Validate chart type
        try:
            chart_enum = ChartType(chart_type.lower())
        except ValueError:
            return {"error": f"Invalid chart type '{chart_type}'. Supported types: {[t.value for t in ChartType]}"}

        # Get chart-specific instructions
        chart_instructions = _get_chart_instructions(chart_enum)

        # Build the chart brief
        brief = f"""**Chart Brief:**
- **Chart Type**: {chart_instructions["type_name"]}
- **Title**: "{title}"
"""

        if x_axis_title:
            brief += f'- **X-Axis Title**: "{x_axis_title}"\n'
        if y_axis_title:
            brief += f'- **Y-Axis Title**: "{y_axis_title}"\n'

        brief += f"- **Data Labels**: {chart_instructions['data_labels']}\n"

        if sort_order:
            brief += f"- **Sorting**: {sort_order}\n"
        elif chart_instructions["default_sort"]:
            brief += f"- **Sorting**: {chart_instructions['default_sort']}\n"

        brief += f"- **Color Palette**: Use {color_palette}\n"
        brief += f"- **Purpose**: {chart_instructions['purpose']}\n"

        if key_insight:
            brief += f"- **Key Insight**: {key_insight}\n"

        # Add data table
        brief += "\n**Data:**\n"
        brief += _format_data_table(data)

        return {
            "chart_brief": brief,
            "chart_type": chart_instructions["type_name"],
            "data_points": len(data),
            "success": True,
        }

    except Exception as e:
        logger.error(f"Error generating chart brief: {str(e)}")
        return {"error": f"Failed to generate chart brief: {str(e)}"}


def _get_chart_instructions(chart_type: ChartType) -> dict[str, str]:
    """Get chart-specific instructions based on chart type."""

    instructions = {
        ChartType.BAR: {
            "type_name": "Horizontal Bar Chart",
            "purpose": "Compare values across different categories",
            "data_labels": "Show values on bars, formatted appropriately",
            "default_sort": "Sort bars from highest to lowest for easy comparison",
        },
        ChartType.COLUMN: {
            "type_name": "Vertical Column Chart",
            "purpose": "Compare values across different categories",
            "data_labels": "Show values on top of columns, formatted appropriately",
            "default_sort": "Sort columns from highest to lowest for easy comparison",
        },
        ChartType.LINE: {
            "type_name": "Line Chart",
            "purpose": "Show trends and changes over time",
            "data_labels": "Add markers for each data point to improve readability",
            "default_sort": "Sort by time/sequence in chronological order",
        },
        ChartType.PIE: {
            "type_name": "Pie Chart",
            "purpose": "Show proportions of each category as part of the whole",
            "data_labels": "Label each slice with category name and percentage",
            "default_sort": "Sort slices from largest to smallest, limit to 5 categories max",
        },
        ChartType.DONUT: {
            "type_name": "Donut Chart",
            "purpose": "Show proportions with emphasis on the total in the center",
            "data_labels": "Label each segment with category name and percentage",
            "default_sort": "Sort segments from largest to smallest, limit to 5 categories max",
        },
        ChartType.SCATTER: {
            "type_name": "Scatter Plot",
            "purpose": "Show relationships and correlations between two variables",
            "data_labels": "Label key data points, include trendline if correlation exists",
            "default_sort": None,
        },
        ChartType.BUBBLE: {
            "type_name": "Bubble Chart",
            "purpose": "Show relationships between three variables using position and size",
            "data_labels": "Label significant bubbles, use size to represent third variable",
            "default_sort": None,
        },
        ChartType.HEATMAP: {
            "type_name": "Heatmap",
            "purpose": "Show patterns and intensity across two categorical dimensions",
            "data_labels": "Use color intensity to represent values, include legend",
            "default_sort": "Arrange categories logically (e.g., time, alphabetical)",
        },
        ChartType.AREA: {
            "type_name": "Area Chart",
            "purpose": "Show cumulative totals and trends over time",
            "data_labels": "Label key points and show total values",
            "default_sort": "Sort by time/sequence in chronological order",
        },
    }

    return instructions.get(
        chart_type,
        {
            "type_name": "Chart",
            "purpose": "Visualize the data effectively",
            "data_labels": "Include appropriate labels",
            "default_sort": None,
        },
    )


def _format_data_table(data: list[dict[str, Any]]) -> str:
    """Format data as a markdown table."""
    if not data:
        return "| No data provided |\n|---|\n"

    # Get headers from first row
    headers = list(data[0].keys())

    # Create table header
    table = "| " + " | ".join(headers) + " |\n"
    table += "|" + "|".join(["---"] * len(headers)) + "|\n"

    # Add data rows
    for row in data:
        values = [str(row.get(header, "")) for header in headers]
        table += "| " + " | ".join(values) + " |\n"

    return table


if __name__ == "__main__":
    logger.info("Starting Gamma MCP Server")

    # Accept either the shared GAMMA_API_KEY or any per-user
    # GAMMA_API_KEY_<NAME>. The runtime resolves per-call via
    # _pick_api_key — operators on per-user-only setups would
    # otherwise crash here even though the call path is fine.
    if not os.environ.get("GAMMA_API_KEY") and not _list_account_names():
        logger.error("No GAMMA_API_KEY or GAMMA_API_KEY_<NAME> env var set")
        sys.exit(1)

    try:
        # Use stdio transport (default for FastMCP)
        mcp.run(transport="stdio")
    except Exception as e:
        logger.error(f"Failed to start server: {e}")
        sys.exit(1)
