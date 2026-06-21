#!/usr/bin/env python3
"""Mailbux MCP server for Victoria Terminal.

Mailbux runs the Stalwart mail server with JMAP and SMTP exposed. This MCP
mirrors the surface the agent already knows from ses_s3_email + sendgrid_server
so prompts and protocols can target whichever provider an operator has
configured (or both side by side — FastMCP prefixes every tool with the
server name, so `mcp_email_search_emails` and `mcp_mailbux_search_emails`
never collide).

Transports:
- Search / get / download: JMAP at MAILBUX_JMAP_BASE_URL (default
  https://my.mailbux.com), basic-auth with MAILBUX_USERNAME / MAILBUX_PASSWORD.
- Send: SMTP submission at MAILBUX_SMTP_HOST:MAILBUX_SMTP_PORT (default
  my.mailbux.com:587 STARTTLS), same credentials.

Environment Variables (all optional except the credentials):
- MAILBUX_USERNAME       — JMAP/SMTP user (e.g. victoria@example.com). Required.
- MAILBUX_PASSWORD       — JMAP/SMTP password. Required.
- MAILBUX_FROM_EMAIL     — default From address for outgoing mail (falls back
                           to MAILBUX_USERNAME, then victoria@elcanotek.com).
- MAILBUX_JMAP_BASE_URL  — JMAP server base (default https://my.mailbux.com).
- MAILBUX_SMTP_HOST      — SMTP submission host (default my.mailbux.com).
- MAILBUX_SMTP_PORT      — SMTP submission port (default 587 = STARTTLS;
                           465 selects implicit TLS automatically).
- MAILBUX_DOWNLOAD_DIR   — attachment download fallback dir (default
                           ~/Victoria/email_attachments, matching ses_s3_email).
"""

from __future__ import annotations

import asyncio
import base64
import contextlib
import logging
import mimetypes
import os
import re
import smtplib
import ssl
import sys
from collections.abc import Mapping, MutableMapping, Sequence
from datetime import UTC, datetime
from email.message import EmailMessage
from email.utils import formataddr, parseaddr
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlparse

import aiofiles
import httpx
from email_lint import (
    check_template_leakage_legacy as _check_template_leakage_pair,
)
from email_lint import (
    detect_html_content as _detect_html_content,
)
from email_lint import (
    extract_cid_references as _extract_cid_references,
)
from email_lint import (
    find_unresolved_template_tokens as _find_unresolved_template_tokens,
)
from email_lint import (
    format_findings,
    partition_findings,
    validate,
)
from email_lint import (
    resolve_content_type as _resolve_content_type,
)
from email_lint import (
    validate_email_body_legacy as _validate_email_body,
)
from email_lint import (
    validate_email_subject_legacy as _validate_email_subject,
)
from email_lint import (
    validate_html_legacy as _validate_html,
)
from mcp.server.fastmcp import FastMCP

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger(__name__)

# --- Configuration ---
MAILBUX_USERNAME = os.environ.get("MAILBUX_USERNAME", "")
MAILBUX_PASSWORD = os.environ.get("MAILBUX_PASSWORD", "")
DEFAULT_FROM_EMAIL = "victoria@elcanotek.com"
MAILBUX_FROM_EMAIL = (
    os.environ.get("MAILBUX_FROM_EMAIL", "") or MAILBUX_USERNAME or DEFAULT_FROM_EMAIL
)
MAILBUX_JMAP_BASE_URL = os.environ.get("MAILBUX_JMAP_BASE_URL", "https://my.mailbux.com").rstrip("/")
MAILBUX_SMTP_HOST = os.environ.get("MAILBUX_SMTP_HOST", "my.mailbux.com")
MAILBUX_SMTP_PORT = int(os.environ.get("MAILBUX_SMTP_PORT", "587"))
ATTACHMENT_DOWNLOAD_DIR = os.environ.get(
    "MAILBUX_DOWNLOAD_DIR",
    os.path.expanduser("~/Victoria/email_attachments"),
)

# JMAP server timeout. Stalwart's Email/get on 50+ ids stays well under 5s; pick
# a generous upper bound so a slow page doesn't kill an otherwise fine search.
JMAP_TIMEOUT_SECONDS = float(os.environ.get("MAILBUX_JMAP_TIMEOUT_SECONDS", "30"))

# Page size for paginated Email/query scans (used by has_payload filtering
# where we may need to keep walking past `max_results` to find enough bodies
# that satisfy the post-fetch filter). 200 is a sweet spot: large enough that
# we typically finish in 1–2 round trips on a 3-day window even for busy
# clients, small enough that the Email/get body fetch stays bounded.
EMAIL_QUERY_PAGE_LIMIT = int(os.environ.get("MAILBUX_QUERY_PAGE_LIMIT", "200"))

# Hard cap on emails we'll scan client-side per search call. The date window
# narrows things server-side first; this cap protects against a misconfigured
# query that would otherwise stream the entire 3-day window into memory.
# Operators can raise this for clients with high inbound volume.
SEARCH_MAX_SCAN_DEFAULT = int(os.environ.get("MAILBUX_SEARCH_MAX_SCAN", "2000"))

# Ensure the default attachment dir is writable on startup. Matches the
# ses_s3_email behavior so the two servers behave consistently when the
# agent omits output_dir.
Path(ATTACHMENT_DOWNLOAD_DIR).mkdir(parents=True, exist_ok=True)

mcp = FastMCP("mailbux")


# ────────────────────────────────────────────────────────────────────────────
# HTML validation
#
# Delegated to the shared `email_lint` module — same validator as
# sendgrid_server.py (this repo) and cutlass/mcp/sendgrid_server.py
# (cross-repo). The `_validate_html`, `_validate_email_body`,
# `_validate_email_subject`, `_find_unresolved_template_tokens` names
# are re-exported above so existing tests (`test_template_validation.py`)
# keep exercising the validator through this module.
# ────────────────────────────────────────────────────────────────────────────


def _read_text_file(path: str, errors: str = "strict") -> str:
    """Read a text file with a context manager (closes the handle).

    Intended to be wrapped in ``asyncio.to_thread`` from the async paths
    so a slow disk read never blocks the event loop while still releasing
    the file descriptor promptly.
    """
    with open(path, encoding="utf-8", errors=errors) as f:
        return f.read()


def _check_template_leakage(content: str) -> str | None:
    """Legacy single-string template leakage check.

    Returns a comma-joined sample of detected demo markers, or None.
    Used by callers that want a single-line warning string rather than
    structured findings.
    """
    errors, warnings = _check_template_leakage_pair(content)
    hits = errors + warnings
    if not hits:
        return None
    cleaned: list[str] = []
    for line in hits:
        # "EL103 (error): … detected — Amazon US OLV, Amazon CA Display …"
        if " — " in line:
            cleaned.append(line.split(" — ", 1)[1])
        else:
            cleaned.append(line)
    return ", ".join(cleaned[:3])


# Re-exported by name so the existing test_template_validation.py keeps
# importing these via `mailbux._validate_html`, etc. New code should
# import from `email_lint` directly.
__all__ = [
    "_check_template_leakage",
    "_detect_html_content",
    "_extract_cid_references",
    "_find_unresolved_template_tokens",
    "_resolve_content_type",
    "_validate_email_body",
    "_validate_email_subject",
    "_validate_html",
]


# ────────────────────────────────────────────────────────────────────────────
# URL / link classification (mirrors ses_s3_email.py; same heuristics so the
# `body_urls[]` / `download_links[]` semantics match across both inbox MCPs).
# ────────────────────────────────────────────────────────────────────────────

DOWNLOAD_EXTENSIONS = {
    ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".odt", ".ods", ".odp", ".rtf", ".txt",
    ".csv", ".json", ".xml", ".yaml", ".yml",
    ".zip", ".tar", ".gz", ".rar", ".7z", ".bz2",
    ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".svg", ".webp",
    ".mp3", ".mp4", ".wav", ".avi", ".mov", ".mkv",
    ".py", ".js", ".ts", ".html", ".css", ".sql",
    ".exe", ".dmg", ".pkg", ".deb", ".rpm", ".apk",
}

IMAGE_EXTENSIONS = {".png", ".jpg", ".jpeg", ".gif", ".bmp", ".svg", ".webp", ".ico"}

CLICK_TRACKER_PATTERNS: tuple[tuple[str, str], ...] = (
    ("sendgrid.net", "/ls/click"),
    ("sendgrid.net", "/wf/click"),
    ("list-manage.com", "/track/click"),
    ("mailchimp.com", "/track/click"),
    ("hubspotlinks.com", ""),
    ("hs-sites.com", "/e1t/c/"),
    ("iterable.com", "/api/email/click"),
    ("links.iterable.com", ""),
    ("mkt.com", "/trk"),
    ("eloqua.com", "/e/er"),
    ("pardot.com", "/e/"),
    ("exct.net", ""),
    ("exacttarget.com", ""),
    ("pstmrk.it", ""),
    ("", "/ls/click"),
    ("", "/wf/click"),
    ("", "/track/click"),
)


def _is_click_tracker_url(url: str) -> bool:
    try:
        parsed = urlparse(url)
    except Exception:
        return False
    host = (parsed.netloc or "").lower()
    path = (parsed.path or "").lower()
    for host_suffix, path_substring in CLICK_TRACKER_PATTERNS:
        host_ok = (not host_suffix) or host == host_suffix or host.endswith("." + host_suffix)
        path_ok = (not path_substring) or path_substring in path
        if (host_suffix or path_substring) and host_ok and path_ok:
            return True
    return False


def _clean_url(url: str) -> str:
    if not url:
        return ""
    url = re.sub(r"=[\r\n]+", "", url)
    url = url.replace("\n", "").replace("\r", "").strip().rstrip(".,;:!?")
    return url


def _extract_urls_from_text(text: str) -> list[str]:
    pattern = r'https?://[^\s<>"\')\]}>]*(?:=[\r\n]+[^\s<>"\')\]}>]*)*'
    return [u for u in (_clean_url(m) for m in re.findall(pattern, text)) if u]


def _extract_urls_from_html(html: str) -> list[dict[str, str]]:
    pattern = r'<a[^>]+href=["\']([^"\']+)["\'][^>]*>([^<]*)</a>'
    links = []
    for href, text in re.findall(pattern, html, re.IGNORECASE):
        if href.startswith(("http://", "https://")):
            links.append({"url": _clean_url(href), "text": (text or "").strip()})
    return links


def _classify_url(url: str) -> dict[str, Any]:
    cleaned = _clean_url(url)
    parsed = urlparse(cleaned)
    path = unquote(parsed.path)
    filename = Path(path).name if path else ""
    extension = Path(path).suffix.lower() if path else ""
    is_image_asset = extension in IMAGE_EXTENSIONS
    is_download_extension = extension in DOWNLOAD_EXTENSIONS and not is_image_asset
    download_keywords = [
        "download", "attachment", "file", "export", "get", "report", "generate",
        "csv", "xlsx", "pdf", "api/download", "api/export", "api/report",
    ]
    has_download_keyword = any(kw in cleaned.lower() for kw in download_keywords)
    query_lower = parsed.query.lower()
    download_params = [
        "format=csv", "format=xlsx", "format=pdf", "type=csv", "export=",
        "download=", "action=download", "action=export",
    ]
    has_download_param = any(p in query_lower for p in download_params)
    is_click_tracker = _is_click_tracker_url(cleaned)
    is_download_likely = (
        is_download_extension or has_download_keyword or has_download_param or is_click_tracker
    ) and not is_image_asset
    return {
        "url": cleaned,
        "original_url": url,
        "domain": parsed.netloc,
        "filename": filename if filename else None,
        "extension": extension if extension else None,
        "is_download_likely": is_download_likely,
        "is_image_asset": is_image_asset,
        "is_click_tracker": is_click_tracker,
        "has_download_keyword": has_download_keyword,
        "has_download_param": has_download_param,
    }


# ────────────────────────────────────────────────────────────────────────────
# Filename / output-path helpers (lifted from ses_s3_email.py so download
# tools behave identically across both providers).
# ────────────────────────────────────────────────────────────────────────────

def _sanitize_download_filename(filename: str) -> str:
    safe = (filename or "").strip().replace("\\", "_").replace("/", "_")
    safe = re.sub(r"\s+", " ", safe).strip(" .")
    if not safe:
        safe = f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}"
    return safe


def _strip_html_to_text(html: str) -> str:
    """Render a readable plain-text alternative from HTML body content.

    Used by the SMTP send path so the `text/plain` alternative carries the
    actual message instead of a "view in HTML client" placeholder. The agent
    later reads this fallback when previewing or searching; a placeholder
    here breaks body_preview-based UX.
    """
    if not html:
        return ""
    # Drop script/style blocks entirely.
    text = re.sub(r"<(script|style)\b[^>]*>.*?</\1>", "", html, flags=re.IGNORECASE | re.DOTALL)
    # Block elements become newlines so paragraphs/list items don't collapse.
    text = re.sub(r"<br\s*/?>", "\n", text, flags=re.IGNORECASE)
    text = re.sub(
        r"</(p|div|li|h[1-6]|tr|table|section|article|header|footer)\s*>",
        "\n",
        text,
        flags=re.IGNORECASE,
    )
    # Strip remaining tags.
    text = re.sub(r"<[^>]+>", "", text)
    # Common entities — handle the cases that come up in our email
    # templates. html.unescape would cover all named refs, but the
    # output is fine for the plain alternative.
    text = (
        text.replace("&nbsp;", " ")
        .replace("&amp;", "&")
        .replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", '"')
        .replace("&apos;", "'")
    )
    # Collapse runs of whitespace per-line, then drop extra blank lines.
    lines = [re.sub(r"[ \t]+", " ", ln).strip() for ln in text.split("\n")]
    out: list[str] = []
    last_blank = False
    for ln in lines:
        if ln:
            out.append(ln)
            last_blank = False
        elif not last_blank:
            out.append("")
            last_blank = True
    return "\n".join(out).strip()


def _build_collision_safe_output_path(download_dir: str | Path, filename: str, source_id: str) -> Path:
    import hashlib

    safe = _sanitize_download_filename(filename)
    path = Path(safe)
    suffix = "".join(path.suffixes)
    stem = path.name[: -len(suffix)] if suffix else path.name
    token = hashlib.sha1(source_id.encode("utf-8")).hexdigest()[:8]
    candidate = Path(download_dir) / f"{stem}__{token}{suffix}"
    counter = 1
    while candidate.exists():
        candidate = Path(download_dir) / f"{stem}__{token}_{counter}{suffix}"
        counter += 1
    return candidate


# ────────────────────────────────────────────────────────────────────────────
# JMAP client
# ────────────────────────────────────────────────────────────────────────────


class MailbuxConfigurationError(RuntimeError):
    """Raised when required Mailbux credentials are missing."""


_SESSION_CACHE: dict[str, Any] | None = None
_SESSION_LOCK = asyncio.Lock()


def _is_fulltext_indexable(substring: str | None) -> bool:
    """Heuristic for whether Stalwart's `text` filter will narrow on this fragment.

    Stalwart tokenizes addresses on `@` and `.` and indexes both halves —
    `text="elcanotek.com"` hits messages from `brad@elcanotek.com`, but
    `text="elcanotek"` (no dot) returns 0. We only push the caller's
    sender/recipient substring into the JMAP `text` filter when it looks
    like a token Stalwart actually indexes: contains an `@`, contains a
    `.`, or is at least 6 chars long (so name fragments like "victoria"
    and "company" still benefit on big mailboxes).

    Falsey or short fragments fall back to the strict Python post-filter
    only — slower on a huge mailbox but always correct.
    """
    if not substring:
        return False
    s = substring.strip()
    if not s:
        return False
    if "@" in s or "." in s:
        return True
    return len(s) >= 6


def _make_token_field_filter(field: str, value: str | None) -> dict[str, Any] | None:
    """Build a single JMAP FilterCondition that narrows `field` to
    "all whitespace-separated tokens present".

    Background: Stalwart implements `subject`, `from`, `to`, and `text`
    filters as a tokenized index lookup with OR across tokens — passing
    `{subject: "Mailbux MCP inline test"}` returns ANY email containing
    ANY of those four words (5+ false positives on a normal mailbox).
    Splitting into AND-of-tokens (`{operator:AND, conditions:[{subject:tok}
    for tok in tokens]}`) narrows server-side to emails containing every
    token — verified to drop "Mailbux MCP inline test" from 5 hits to 1.

    Quoted phrases ("foo bar") are NOT respected by Stalwart, so this is
    the strictest server-side push-down available. The caller still applies
    a Python substring check after fetch for exact-phrase correctness, but
    the candidate set is now bounded.
    """
    if not value:
        return None
    tokens = [t for t in value.split() if t]
    if not tokens:
        return None
    if len(tokens) == 1:
        return {field: tokens[0]}
    return {"operator": "AND", "conditions": [{field: t} for t in tokens]}


def _basic_auth_header() -> str:
    if not MAILBUX_USERNAME or not MAILBUX_PASSWORD:
        raise MailbuxConfigurationError(
            "MAILBUX_USERNAME and MAILBUX_PASSWORD environment variables must be set."
        )
    token = base64.b64encode(f"{MAILBUX_USERNAME}:{MAILBUX_PASSWORD}".encode()).decode("ascii")
    return f"Basic {token}"


async def _jmap_session() -> dict[str, Any]:
    """Fetch and cache the JMAP session descriptor (.well-known/jmap)."""
    global _SESSION_CACHE
    if _SESSION_CACHE is not None:
        return _SESSION_CACHE
    async with _SESSION_LOCK:
        if _SESSION_CACHE is not None:
            return _SESSION_CACHE
        url = f"{MAILBUX_JMAP_BASE_URL}/.well-known/jmap/"
        async with httpx.AsyncClient(
            timeout=JMAP_TIMEOUT_SECONDS, follow_redirects=True
        ) as client:
            r = await client.get(url, headers={"Authorization": _basic_auth_header()})
        if r.status_code == 401:
            raise MailbuxConfigurationError("Mailbux JMAP auth failed (401). Check credentials.")
        r.raise_for_status()
        session = r.json()
        _SESSION_CACHE = session
        return session


async def _account_id() -> str:
    session = await _jmap_session()
    primary = session.get("primaryAccounts") or {}
    acct = primary.get("urn:ietf:params:jmap:mail")
    if not acct:
        accounts = session.get("accounts") or {}
        if accounts:
            acct = next(iter(accounts.keys()))
    if not acct:
        raise MailbuxConfigurationError("JMAP session has no primary mail account.")
    return acct


async def _jmap_call(method_calls: list[list[Any]]) -> list[list[Any]]:
    """POST a single JMAP request and return its methodResponses list."""
    session = await _jmap_session()
    api_url = session.get("apiUrl") or f"{MAILBUX_JMAP_BASE_URL}/jmap/"
    body = {
        "using": [
            "urn:ietf:params:jmap:core",
            "urn:ietf:params:jmap:mail",
            "urn:ietf:params:jmap:submission",
        ],
        "methodCalls": method_calls,
    }
    async with httpx.AsyncClient(timeout=JMAP_TIMEOUT_SECONDS) as client:
        r = await client.post(
            api_url,
            json=body,
            headers={
                "Authorization": _basic_auth_header(),
                "Content-Type": "application/json",
            },
        )
    if r.status_code == 401:
        raise MailbuxConfigurationError("Mailbux JMAP auth failed (401). Check credentials.")
    r.raise_for_status()
    payload = r.json()
    responses = payload.get("methodResponses") or []
    # Surface JMAP-level errors as exceptions so the caller never has to
    # inspect each response tuple individually.
    for resp in responses:
        if resp and resp[0] == "error":
            raise RuntimeError(f"JMAP error: {resp[1]}")
    return responses


# ────────────────────────────────────────────────────────────────────────────
# JMAP search helpers — translation between caller-friendly filters and the
# JMAP `FilterCondition` object.
# ────────────────────────────────────────────────────────────────────────────


def _parse_date_bound(date_str: str | None, is_end: bool) -> datetime | None:
    """Match ses_s3_email.parse_date_bound semantics for cross-MCP consistency."""
    if not date_str:
        return None
    if "T" in date_str:
        dt = datetime.fromisoformat(date_str)
        return dt.replace(tzinfo=UTC) if dt.tzinfo is None else dt.astimezone(UTC)
    dt = datetime.fromisoformat(date_str)
    if is_end:
        return datetime(dt.year, dt.month, dt.day, 23, 59, 59, 999999, tzinfo=UTC)
    return datetime(dt.year, dt.month, dt.day, 0, 0, 0, 0, tzinfo=UTC)


def _to_jmap_utc(dt: datetime | None) -> str | None:
    """JMAP UTCDate format: YYYY-MM-DDTHH:MM:SSZ (no fractional seconds)."""
    if dt is None:
        return None
    return dt.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _build_query_filter(
    *,
    subject_contains: str | None,
    subject_keywords: list[str] | None,
    sender_contains: str | None,
    recipient_contains: str | None,
    date_from: datetime | None,
    date_to: datetime | None,
    text: str | None = None,
    has_attachment: bool | None = None,
    use_address_filters: bool = False,
) -> dict[str, Any] | None:
    """Compose a JMAP Email/query FilterCondition (or composite Filter).

    JMAP filters can be either single FilterCondition dicts or compound
    Filter operators (`{"operator": "AND"/"OR"/"NOT", "conditions": [...]}`).
    Multi-token caller values (e.g. subject_contains="Master Chief Daily")
    are split into AND-of-tokens server-side so Stalwart's tokenized-OR
    semantics don't produce false positives.

    Address filters caveat: Stalwart's `from`/`to` JMAP filters match name
    *tokens*, not the address substring — `from="elcanotek"` returns 0 hits
    even on mail from `brad@elcanotek.com`. The default here is to NOT
    push sender/recipient into JMAP so the agent's substring expectation
    is honored; instead the caller post-filters in Python after fetching
    headers. Pass `use_address_filters=True` to override (e.g. when the
    caller has confirmed the substring is a person-name token).
    """
    conds: list[dict[str, Any]] = []
    if subject_contains:
        sf = _make_token_field_filter("subject", subject_contains)
        if sf is not None:
            conds.append(sf)
    if subject_keywords:
        # Each keyword may itself be multi-word (e.g. "Master Chief").
        # AND-of-tokens within a keyword, OR across keywords — matches the
        # original "any keyword present" semantic without false positives.
        kw_filters = [_make_token_field_filter("subject", kw) for kw in subject_keywords]
        kw_filters = [f for f in kw_filters if f is not None]
        if len(kw_filters) == 1:
            conds.append(kw_filters[0])
        elif kw_filters:
            conds.append({"operator": "OR", "conditions": kw_filters})
    if use_address_filters and sender_contains:
        af = _make_token_field_filter("from", sender_contains)
        if af is not None:
            conds.append(af)
    if use_address_filters and recipient_contains:
        af = _make_token_field_filter("to", recipient_contains)
        if af is not None:
            conds.append(af)
    if date_from is not None:
        # `after` is inclusive in JMAP.
        conds.append({"after": _to_jmap_utc(date_from)})
    if date_to is not None:
        # `before` is exclusive in JMAP; bump to next-second to make end-of-day
        # inclusive (matches the SES MCP, which treats date_to as inclusive).
        from datetime import timedelta

        conds.append({"before": _to_jmap_utc(date_to + timedelta(seconds=1))})
    if text:
        conds.append({"text": text})
    if has_attachment is True:
        conds.append({"hasAttachment": True})
    elif has_attachment is False:
        conds.append({"hasAttachment": False})
    if not conds:
        return None
    if len(conds) == 1:
        return conds[0]
    return {"operator": "AND", "conditions": conds}


# Header / body properties we always pull. Kept minimal — Stalwart is fast at
# Email/get but each property still costs round-trip serialization.
_HEADER_PROPS = [
    "id",
    "blobId",
    "threadId",
    "mailboxIds",
    "from",
    "to",
    "cc",
    "subject",
    "receivedAt",
    "sentAt",
    "size",
    "hasAttachment",
    "preview",
]
_BODY_PROPS = _HEADER_PROPS + ["bodyValues", "textBody", "htmlBody", "attachments"]


def _address_list_to_str(addresses: list[dict[str, Any]] | None) -> str:
    if not addresses:
        return ""
    out = []
    for a in addresses:
        name = (a.get("name") or "").strip()
        email = (a.get("email") or "").strip()
        if name and email:
            out.append(formataddr((name, email)))
        elif email:
            out.append(email)
        elif name:
            out.append(name)
    return ", ".join(out)


def _walk_body_parts(parts: list[dict[str, Any]] | None) -> list[dict[str, Any]]:
    """Flatten a JMAP body-parts tree (htmlBody/textBody) to a flat list."""
    if not parts:
        return []
    out: list[dict[str, Any]] = []
    for p in parts:
        if not p:
            continue
        out.append(p)
        out.extend(_walk_body_parts(p.get("subParts")))
    return out


def _body_text_html(email_obj: dict[str, Any]) -> tuple[str, str]:
    """Return (plain_text, html) for a JMAP Email object. Empty strings if absent."""
    body_values = email_obj.get("bodyValues") or {}
    text_parts = _walk_body_parts(email_obj.get("textBody"))
    html_parts = _walk_body_parts(email_obj.get("htmlBody"))
    plain = ""
    for p in text_parts:
        v = body_values.get(p.get("partId") or "")
        if v and v.get("value"):
            plain = v["value"]
            break
    html = ""
    for p in html_parts:
        v = body_values.get(p.get("partId") or "")
        if v and v.get("value"):
            html = v["value"]
            break
    return plain, html


def _attachment_list(email_obj: dict[str, Any]) -> list[dict[str, Any]]:
    """Return a list of {filename, blobId, content_type, size_bytes} entries."""
    out: list[dict[str, Any]] = []
    for att in email_obj.get("attachments") or []:
        name = att.get("name") or att.get("filename") or ""
        if not name and att.get("type"):
            # Fall back to a synthetic name keyed off the blob so collision-safe
            # paths still work for unnamed attachments.
            name = f"attachment-{att.get('blobId', 'unknown')}"
        out.append(
            {
                "filename": name,
                "blobId": att.get("blobId"),
                "content_type": att.get("type") or "application/octet-stream",
                "size_bytes": int(att.get("size") or 0),
                "disposition": att.get("disposition"),
                "cid": att.get("cid"),
            }
        )
    return out


def _extract_body_urls(plain: str, html: str) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Walk plain + html body for URLs and classify each. Returns
    (body_urls_all, download_links_subset) mirroring ses_s3_email."""
    body_urls: list[dict[str, Any]] = []
    download_links: list[dict[str, Any]] = []
    seen: set[str] = set()

    def _record(url: str, link_text: str | None, source: str) -> None:
        c = _classify_url(url)
        cleaned = c["url"]
        if cleaned in seen:
            return
        seen.add(cleaned)
        entry = {
            "url": cleaned,
            "domain": c["domain"],
            "filename": c["filename"],
            "extension": c["extension"],
            "link_text": link_text,
            "source": source,
            "is_download_likely": c["is_download_likely"],
            "is_click_tracker": c["is_click_tracker"],
            "is_image_asset": c["is_image_asset"],
            "url_length": len(cleaned),
        }
        body_urls.append(entry)
        if c["is_download_likely"]:
            download_links.append(
                {
                    "url": cleaned,
                    "filename": c["filename"],
                    "link_text": link_text,
                    "is_click_tracker": c["is_click_tracker"],
                }
            )

    if html:
        for link in _extract_urls_from_html(html):
            _record(link["url"], link["text"] or None, "html")
    if plain:
        for url in _extract_urls_from_text(plain):
            _record(url, None, "plain_text")
    if html:
        for url in _extract_urls_from_text(html):
            _record(url, None, "html_text")
    return body_urls, download_links


# ────────────────────────────────────────────────────────────────────────────
# JMAP blob download
# ────────────────────────────────────────────────────────────────────────────


async def _download_blob(blob_id: str, filename: str = "blob") -> tuple[bytes, str | None]:
    """Fetch a blob from the JMAP downloadUrl. Returns (content, content-type)."""
    session = await _jmap_session()
    download_url = session.get("downloadUrl")
    if not download_url:
        raise RuntimeError("JMAP session missing downloadUrl.")
    account = await _account_id()
    safe_name = _sanitize_download_filename(filename)
    url = (
        download_url.replace("{accountId}", account)
        .replace("{blobId}", blob_id)
        .replace("{name}", safe_name)
        .replace("{type}", "application/octet-stream")
    )
    async with httpx.AsyncClient(timeout=JMAP_TIMEOUT_SECONDS) as client:
        r = await client.get(url, headers={"Authorization": _basic_auth_header()})
    if r.status_code == 401:
        raise MailbuxConfigurationError("Mailbux JMAP auth failed during blob download.")
    r.raise_for_status()
    return r.content, r.headers.get("content-type")


# ────────────────────────────────────────────────────────────────────────────
# SMTP send (threaded blocking smtplib via asyncio.to_thread — same pattern
# the sendgrid MCP uses for its SDK, no extra deps).
# ────────────────────────────────────────────────────────────────────────────


def _resolve_from_email(explicit_from: str | None = None) -> str:
    if explicit_from:
        return explicit_from
    return MAILBUX_FROM_EMAIL


def _normalize_recipients(addresses: Sequence[str] | None) -> list[str]:
    if not addresses:
        return []
    out: list[str] = []
    for entry in addresses:
        if not entry:
            continue
        # Split on comma/semicolon/newline so the agent can pass a single
        # delimited string instead of a list — same shape sendgrid_server
        # accepts so prompts stay portable.
        for piece in re.split(r"[,;\n]", entry):
            piece = piece.strip()
            if not piece:
                continue
            _, addr = parseaddr(piece)
            if addr:
                out.append(piece)
    return out


def _build_mime_message(
    *,
    sender: str,
    to_list: list[str],
    cc_list: list[str],
    bcc_list: list[str],
    subject: str,
    content: str,
    content_type: str,
    attachments: Sequence[str] | None,
    inline_attachments: Sequence[Mapping[str, Any]] | None,
    reply_to: str | None,
) -> tuple[EmailMessage, list[dict[str, Any]], list[dict[str, Any]]]:
    """Build a multipart MIME message. Returns (message, attachment_info, inline_attachment_info)."""
    msg = EmailMessage()
    msg["From"] = sender
    msg["To"] = ", ".join(to_list)
    if cc_list:
        msg["Cc"] = ", ".join(cc_list)
    if reply_to:
        msg["Reply-To"] = reply_to
    msg["Subject"] = subject

    is_html = content_type.lower() == "text/html"
    if is_html:
        # Strip HTML to a readable plain-text alternative for non-HTML
        # clients (and for search/preview tools that read the text part
        # first). A hand-rolled stripper is fine here — no external dep,
        # and we already validated the HTML upstream.
        plain_fallback = _strip_html_to_text(content) or "(HTML email — view in an HTML-capable client.)"
        msg.set_content(plain_fallback)
        msg.add_alternative(content, subtype="html")
    else:
        msg.set_content(content)

    attachment_info: list[dict[str, Any]] = []
    if attachments:
        for path_str in attachments:
            resolved = os.path.expanduser(path_str)
            if not os.path.isabs(resolved):
                resolved = os.path.abspath(resolved)
            if not os.path.exists(resolved):
                raise FileNotFoundError(f"Attachment file not found: {path_str}")
            size = os.path.getsize(resolved)
            if size > 25 * 1024 * 1024:
                raise ValueError(
                    f"Attachment too large: {os.path.basename(resolved)} ({size / 1024 / 1024:.1f}MB). "
                    "Maximum size is 25MB per attachment."
                )
            mime_type, _ = mimetypes.guess_type(resolved)
            mime_type = mime_type or "application/octet-stream"
            maintype, subtype = mime_type.split("/", 1) if "/" in mime_type else ("application", "octet-stream")
            with open(resolved, "rb") as f:
                data = f.read()
            msg.add_attachment(
                data,
                maintype=maintype,
                subtype=subtype,
                filename=os.path.basename(resolved),
            )
            attachment_info.append({"name": os.path.basename(resolved), "size": size, "mime_type": mime_type})

    inline_attachment_info: list[dict[str, Any]] = []
    if inline_attachments:
        for item in inline_attachments:
            if not isinstance(item, Mapping):
                raise ValueError("inline_attachments entries must be objects with keys: path, cid")
            path_val = str(item.get("path", "")).strip()
            cid_val = str(item.get("cid", "")).strip()
            if not path_val or not cid_val:
                raise ValueError("Each inline_attachments entry must include non-empty 'path' and 'cid'")
            resolved = os.path.expanduser(path_val)
            if not os.path.isabs(resolved):
                resolved = os.path.abspath(resolved)
            if not os.path.exists(resolved):
                raise FileNotFoundError(f"Inline attachment file not found: {path_val}")
            size = os.path.getsize(resolved)
            if size > 25 * 1024 * 1024:
                raise ValueError(
                    f"Inline attachment too large: {os.path.basename(resolved)} ({size / 1024 / 1024:.1f}MB)."
                )
            explicit = str(item.get("mime_type") or "").strip()
            if explicit:
                mime_type = explicit
            else:
                guessed, _ = mimetypes.guess_type(resolved)
                mime_type = guessed or "application/octet-stream"
            maintype, subtype = mime_type.split("/", 1) if "/" in mime_type else ("application", "octet-stream")
            with open(resolved, "rb") as f:
                data = f.read()
            msg.add_attachment(
                data,
                maintype=maintype,
                subtype=subtype,
                filename=os.path.basename(resolved),
                disposition="inline",
                cid=f"<{cid_val}>",
            )
            inline_attachment_info.append(
                {"name": os.path.basename(resolved), "size": size, "mime_type": mime_type, "cid": cid_val}
            )

    # BCC stays out of the message headers — pass via SMTP envelope only.
    return msg, attachment_info, inline_attachment_info


def _smtp_send_blocking(
    msg: EmailMessage,
    *,
    envelope_from: str,
    envelope_to: list[str],
) -> dict[str, Any]:
    """Synchronous smtplib send. Run via asyncio.to_thread from the tool."""
    host = MAILBUX_SMTP_HOST
    port = MAILBUX_SMTP_PORT
    ctx = ssl.create_default_context()
    if port == 465:
        client = smtplib.SMTP_SSL(host, port, context=ctx, timeout=30)
    else:
        client = smtplib.SMTP(host, port, timeout=30)
        client.ehlo()
        client.starttls(context=ctx)
        client.ehlo()
    try:
        client.login(MAILBUX_USERNAME, MAILBUX_PASSWORD)
        refused = client.send_message(msg, from_addr=envelope_from, to_addrs=envelope_to)
    finally:
        with contextlib.suppress(Exception):
            client.quit()
    return {"refused": refused, "message_id": msg.get("Message-ID", "")}


# ────────────────────────────────────────────────────────────────────────────
# Search-quality warnings (mirrors ses_s3_email so the agent gets the same
# guardrails when it issues sloppy queries against either backend).
# ────────────────────────────────────────────────────────────────────────────


def _is_meaningful_text_filter(value: str | None) -> bool:
    if value is None:
        return False
    return bool(re.search(r"[A-Za-z0-9]", value))


def _build_search_warnings(
    *,
    subject_contains: str | None,
    subject_keywords: list[str] | None,
    sender_contains: str | None,
    recipient_contains: str | None,
    date_from: str | None,
    date_to: str | None,
    has_payload: bool | None,
    max_results: int,
) -> list[dict[str, str]]:
    warnings: list[dict[str, str]] = []
    if sender_contains is not None and not _is_meaningful_text_filter(sender_contains):
        warnings.append(
            {
                "code": "sender_filter_not_meaningful",
                "severity": "high",
                "message": "sender_contains has no letters or digits and will not narrow results reliably.",
                "suggestion": "Use a sender domain/name fragment or remove sender_contains.",
            }
        )
    if recipient_contains is not None and not _is_meaningful_text_filter(recipient_contains):
        warnings.append(
            {
                "code": "recipient_filter_not_meaningful",
                "severity": "high",
                "message": "recipient_contains has no letters or digits and will not narrow results reliably.",
                "suggestion": "Use a recipient email/domain fragment or remove recipient_contains.",
            }
        )
    if date_from is None and date_to is None:
        warnings.append(
            {
                "code": "unbounded_date_window",
                "severity": "medium",
                "message": "No explicit date_from/date_to was provided.",
                "suggestion": "Use both date_from and date_to to keep searches bounded.",
            }
        )
    elif date_from is None or date_to is None:
        warnings.append(
            {
                "code": "partially_bounded_date_window",
                "severity": "medium",
                "message": "Only one date bound was provided.",
                "suggestion": "Provide both date_from and date_to for deterministic retrieval.",
            }
        )
    has_text_filter = (
        bool(subject_contains) or bool(subject_keywords) or bool(sender_contains) or bool(recipient_contains)
    )
    if not has_text_filter and has_payload is None:
        warnings.append(
            {
                "code": "broad_query_low_selectivity",
                "severity": "medium",
                "message": "No subject/sender/recipient/payload filters were provided.",
                "suggestion": "Add sender_contains and/or subject filters before expanding the date window.",
            }
        )
    if max_results > 250:
        warnings.append(
            {
                "code": "large_max_results",
                "severity": "low",
                "message": "max_results is very high and may increase response size and latency.",
                "suggestion": "Start with max_results <= 25, then widen only if needed.",
            }
        )
    return warnings


# ────────────────────────────────────────────────────────────────────────────
# Tools
# ────────────────────────────────────────────────────────────────────────────


@mcp.tool()
async def send_email(
    to_email: str,
    subject: str,
    content: str = "",
    *,
    content_file: str | None = None,
    content_type: str | None = None,
    from_email: str | None = None,
    cc_emails: Sequence[str] | None = None,
    bcc_emails: Sequence[str] | None = None,
    attachments: Sequence[str] | None = None,
    inline_attachments: Sequence[Mapping[str, Any]] | None = None,
    reply_to: str | None = None,
) -> MutableMapping[str, Any]:
    """Send an email through the configured Mailbux SMTP submission server.

    This is a CALLABLE TOOL — call it directly, do NOT import it in Python.

    Argument semantics mirror sendgrid_server.send_email so prompts and
    protocols can target either backend without conditional branching:
    auto-detected HTML, file-based content for large bodies, regular and
    inline (cid:) attachments, and the same HTML / subject / template-leak
    validation pipeline (including the table-foster-parent and rgba()
    blocks). The only Mailbux-specific knob is `reply_to` (optional
    explicit Reply-To address; defaults to the From address).

    RECIPIENT PRIVACY: The email body must NEVER mention internal
    implementation details such as Mailbux, theme names, or any tooling
    used to build or send the email. Recipients should not see how the
    email was made.

    HANDLING LARGE CONTENT (>50KB):
    For large HTML emails, save content to a file first using run_python,
    then pass `content_file` instead of `content`.

    Args:
        to_email: Recipient email address. Supports comma/semicolon/newline-separated lists.
        subject: Email subject line.
        content: HTML or plain-text body. May be empty if content_file is provided.
        content_file: Optional path to a file containing the body — used to avoid JSON
            serialization limits on >50KB emails. Takes precedence over `content`.
        content_type: Usually omit — auto-detected from content. Set to "text/plain"
            to force plain-text delivery.
        from_email: From address. Defaults to MAILBUX_FROM_EMAIL / MAILBUX_USERNAME,
            then victoria@elcanotek.com.
        cc_emails / bcc_emails: Optional CC / BCC recipients.
        attachments: Optional list of file paths to attach. MIME types are auto-detected.
        inline_attachments: Optional list of inline CID attachments. Each entry must include
            `path` and `cid`, and may include `mime_type`.
        reply_to: Optional explicit Reply-To header (defaults to the From address).

    Returns:
        Dictionary with status, message_id (if any), content_type, attachment counts.
    """
    try:
        sender = _resolve_from_email(from_email)
    except MailbuxConfigurationError as exc:
        return {"error": str(exc)}

    actual_content = content
    content_source = "direct"
    if content_file:
        try:
            file_path = os.path.expanduser(content_file)
            if not os.path.isabs(file_path):
                file_path = os.path.abspath(file_path)
            if not os.path.exists(file_path):
                return {"error": f"Content file not found: {content_file}"}
            actual_content = await asyncio.to_thread(_read_text_file, file_path)
            content_source = f"file:{content_file}"
            logger.info("Read email content from file: %s (%d bytes)", content_file, len(actual_content))
        except PermissionError:
            return {"error": f"Permission denied reading content file: {content_file}"}
        except UnicodeDecodeError as e:
            return {"error": f"Failed to decode content file as UTF-8: {content_file} - {e}"}
        except Exception as e:
            return {"error": f"Failed to read content file: {content_file} - {e}"}

    if not actual_content or not actual_content.strip():
        return {"error": "Email content is empty. Provide content directly or via content_file."}

    resolved_type, was_corrected = _resolve_content_type(actual_content, content_type)

    # Run the shared validator (subject + body + structure + leakage).
    findings = validate(actual_content, subject=subject, content_type=resolved_type)
    errors, warnings = partition_findings(findings)

    for finding in errors:
        logger.error("validation error: %s", finding.format())
    for finding in warnings:
        logger.warning("validation warning: %s", finding.format())

    if errors:
        msg = f"Validation Failed. Fix these errors before sending:\n\n  - {format_findings(errors)}"
        if warnings:
            msg += f"\n\nWarnings (non-blocking, but worth fixing):\n  - {format_findings(warnings)}"
        return {"error": msg}

    inline_cid_map: dict[str, Mapping[str, Any]] = {}
    if inline_attachments:
        for item in inline_attachments:
            if not isinstance(item, Mapping):
                return {"error": "inline_attachments entries must be objects with keys: path, cid"}
            cid_val = str(item.get("cid", "")).strip()
            path_val = str(item.get("path", "")).strip()
            if not cid_val or not path_val:
                return {"error": "Each inline_attachments entry must include non-empty 'path' and 'cid'"}
            if cid_val in inline_cid_map:
                return {"error": f"Duplicate inline attachment cid: {cid_val}"}
            inline_cid_map[cid_val] = item

    if resolved_type == "text/html":
        cid_refs = _extract_cid_references(actual_content)
        if cid_refs:
            if not inline_cid_map:
                return {
                    "error": (
                        "HTML contains cid: references but no inline_attachments were provided. "
                        f"Missing inline cids: {', '.join(sorted(cid_refs))}"
                    )
                }
            missing = sorted(cid for cid in cid_refs if cid not in inline_cid_map)
            if missing:
                return {
                    "error": (
                        "HTML contains cid: references that are not mapped in inline_attachments. "
                        f"Missing inline cids: {', '.join(missing)}"
                    )
                }

    to_list = _normalize_recipients([to_email])
    if not to_list:
        return {"error": "No valid to_email recipients provided."}
    cc_list = _normalize_recipients(cc_emails)
    bcc_list = _normalize_recipients(bcc_emails)

    # Envelope recipients: only the email portion, parseaddr'd. Headers carry
    # the human-readable forms.
    envelope_to = [parseaddr(addr)[1] for addr in to_list + cc_list + bcc_list if parseaddr(addr)[1]]
    envelope_from = parseaddr(sender)[1] or sender

    try:
        msg, attachment_info, inline_attachment_info = _build_mime_message(
            sender=sender,
            to_list=to_list,
            cc_list=cc_list,
            bcc_list=bcc_list,
            subject=subject,
            content=actual_content,
            content_type=resolved_type,
            attachments=attachments,
            inline_attachments=inline_attachments,
            reply_to=reply_to,
        )
    except FileNotFoundError as e:
        return {"error": str(e)}
    except ValueError as e:
        return {"error": str(e)}
    except Exception as e:
        return {"error": f"Failed to build MIME message: {e}"}

    try:
        send_result = await asyncio.to_thread(
            _smtp_send_blocking, msg, envelope_from=envelope_from, envelope_to=envelope_to
        )
    except smtplib.SMTPAuthenticationError as e:
        return {"error": f"SMTP authentication failed: {e}"}
    except smtplib.SMTPRecipientsRefused as e:
        return {"error": f"All recipients refused: {e.recipients}"}
    except smtplib.SMTPException as e:
        return {"error": f"SMTP error: {e}"}
    except Exception as e:
        return {"error": f"Failed to send: {e}"}

    result: MutableMapping[str, Any] = {
        "status": "sent",
        "content_type": resolved_type,
        "content_source": content_source,
        "content_length": len(actual_content),
        "html_validated": resolved_type == "text/html",
    }
    if msg_id := send_result.get("message_id"):
        result["message_id"] = msg_id
    if send_result.get("refused"):
        result["refused_recipients"] = send_result["refused"]
        result["status"] = "partial"
    if warnings:
        result["validation_warnings"] = [
            {"rule": f.rule, "severity": f.severity, "message": f.message, "hint": f.hint} for f in warnings
        ]
    if attachment_info:
        result["attachments_count"] = len(attachment_info)
        result["attachments"] = attachment_info
    if inline_attachment_info:
        result["inline_attachments_count"] = len(inline_attachment_info)
        result["inline_attachments"] = inline_attachment_info
    if was_corrected:
        result["content_type_auto_corrected"] = True
        result["original_content_type"] = content_type
    return result


@mcp.tool()
async def validate_email_content(
    content: str = "",
    content_file: str = "",
    subject: str = "",
) -> MutableMapping[str, Any]:
    """Validate HTML email content before sending — identical semantics to the
    sendgrid MCP's validator, so a workflow that drafts against either send tool
    gets the same red lines. Use this to catch unclosed tags, unresolved
    template tokens, and example-data leakage before calling `send_email`.
    """
    actual_content = ""
    source = "content"
    if content_file:
        resolved = os.path.expanduser(content_file.strip())
        if not os.path.isabs(resolved):
            resolved = os.path.abspath(resolved)
        if not os.path.exists(resolved):
            return {"valid": False, "errors": [f"File not found: {content_file}"], "warnings": []}
        actual_content = await asyncio.to_thread(_read_text_file, resolved, "replace")
        source = "content_file"
    elif content:
        actual_content = content
    else:
        return {"valid": False, "errors": ["Provide either content or content_file"], "warnings": []}

    findings = validate(actual_content, subject=subject or None, content_type="text/html")
    errors, warnings = partition_findings(findings)
    return {
        "valid": len(errors) == 0,
        "findings": [
            {"rule": f.rule, "severity": f.severity, "message": f.message, "hint": f.hint, "line": f.line}
            for f in findings
        ],
        "errors": [f.format() for f in errors],
        "warnings": [f.format() for f in warnings],
        "source": source,
        "stats": {
            "content_length": len(actual_content),
            "content_length_kb": round(len(actual_content) / 1024, 1),
        },
    }


@mcp.tool()
async def validate_email_address(email_address: str) -> MutableMapping[str, Any]:
    """Run a basic syntactic check on an email address.

    Mailbux does not expose a deliverability-verification endpoint, so this
    only confirms the address is parseable and follows the local@domain
    shape — it does NOT confirm that the inbox exists or accepts mail. Use
    this when you want a cheap pre-send sanity check; for actual
    deliverability you have to send and inspect bounces.
    """
    name, addr = parseaddr(email_address or "")
    if not addr or "@" not in addr:
        return {
            "email": email_address,
            "valid": False,
            "verdict": "syntax_invalid",
            "note": "Address is missing or does not contain '@'.",
        }
    local, _, domain = addr.rpartition("@")
    if not local or not domain or "." not in domain:
        return {
            "email": email_address,
            "valid": False,
            "verdict": "syntax_invalid",
            "note": "Address is missing a usable local part or domain.",
        }
    return {
        "email": email_address,
        "valid": True,
        "verdict": "syntax_ok",
        "parsed": {"name": name, "address": addr, "local": local, "domain": domain},
        "note": (
            "Syntactic check only — Mailbux does not provide a delivery-confirmation "
            "API. Send and watch for bounces to confirm deliverability."
        ),
    }


@mcp.tool()
async def get_suppression_status(email_address: str) -> MutableMapping[str, Any]:
    """Stub for the SendGrid suppression-list API.

    Mailbux is a regular SMTP/JMAP host with no provider-managed suppression
    list, so we always return `suppressed: False` plus an explanatory note.
    Kept on the tool surface so prompts that probe both backends do not
    error out on Mailbux.
    """
    return {
        "email": email_address,
        "suppressed": False,
        "verdict": "not_supported",
        "note": (
            "Mailbux does not expose a suppression-list API. If you need to enforce a "
            "block-list, maintain it in your own application logic before calling send_email."
        ),
    }


@mcp.tool()
async def get_inbox_stats() -> MutableMapping[str, Any]:
    """Return Mailbux mailbox-level statistics across all folders.

    Returns:
        Dictionary with per-mailbox counts (Inbox / Sent / Drafts / etc.) and
        an aggregate total. Mirrors the ses_s3_email get_inbox_stats shape so
        the agent can reuse the same surface area across providers.
    """
    try:
        account = await _account_id()
        responses = await _jmap_call(
            [["Mailbox/get", {"accountId": account}, "0"]]
        )
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.error("get_inbox_stats failed: %s", e)
        return {"status": "error", "error": str(e)}

    mailboxes = responses[0][1].get("list") or []
    out_mailboxes = []
    total_emails = 0
    total_unread = 0
    for mb in mailboxes:
        total_emails += int(mb.get("totalEmails") or 0)
        total_unread += int(mb.get("unreadEmails") or 0)
        out_mailboxes.append(
            {
                "id": mb.get("id"),
                "name": mb.get("name"),
                "role": mb.get("role"),
                "total": int(mb.get("totalEmails") or 0),
                "unread": int(mb.get("unreadEmails") or 0),
            }
        )
    return {
        "status": "success",
        "stats": {
            "account_id": account,
            "username": MAILBUX_USERNAME,
            "jmap_base_url": MAILBUX_JMAP_BASE_URL,
            "total_emails": total_emails,
            "total_unread": total_unread,
            "mailboxes": out_mailboxes,
        },
    }


def _format_email_match(
    email_obj: dict[str, Any],
    *,
    include_body_preview: bool,
) -> dict[str, Any]:
    """Common projection used by search_emails / search_emails_fuzzy."""
    attachments = _attachment_list(email_obj)
    body_plain, body_html = ("", "")
    body_urls: list[dict[str, Any]] = []
    download_links: list[dict[str, Any]] = []
    if email_obj.get("bodyValues") is not None or email_obj.get("textBody") or email_obj.get("htmlBody"):
        body_plain, body_html = _body_text_html(email_obj)
        body_urls, download_links = _extract_body_urls(body_plain, body_html)
    out: dict[str, Any] = {
        "email_id": email_obj.get("id"),
        "blob_id": email_obj.get("blobId"),
        "thread_id": email_obj.get("threadId"),
        "mailbox_ids": list((email_obj.get("mailboxIds") or {}).keys()),
        "subject": email_obj.get("subject") or "",
        "from": _address_list_to_str(email_obj.get("from")),
        "to": _address_list_to_str(email_obj.get("to")),
        "date": email_obj.get("sentAt") or email_obj.get("receivedAt") or "",
        "received_at": email_obj.get("receivedAt") or "",
        "size_bytes": int(email_obj.get("size") or 0),
        "has_attachments": bool(email_obj.get("hasAttachment") or attachments),
        "attachment_count": len(attachments),
        "attachment_names": [a["filename"] for a in attachments],
        "has_download_links": len(download_links) > 0,
        "download_link_count": len(download_links),
        "body_url_count": len(body_urls),
    }
    if download_links:
        out["download_links"] = download_links
    if include_body_preview:
        preview = body_plain[:500] if body_plain else body_html[:500]
        out["body_preview"] = preview
        if body_urls:
            out["body_urls"] = body_urls
    return out


@mcp.tool()
async def search_emails(
    subject_contains: str | None = None,
    subject_keywords: list[str] | None = None,
    sender_contains: str | None = None,
    recipient_contains: str | None = None,
    date_from: str | None = None,
    date_to: str | None = None,
    has_payload: bool | None = None,
    max_results: int = 25,
    include_body_preview: bool = False,
) -> dict[str, Any]:
    """Search the Mailbux account by subject, sender, recipient, date, and
    payload presence. Mirrors the ses_s3_email `search_emails` surface so the
    same prompt can drive either backend — `s3_key` in the result is aliased
    to the JMAP email id for compatibility with older prompts.

    START HERE when you need to find emails. Returns matching emails with
    metadata plus the email_id needed for get_email / download_attachment.

    PERFORMANCE GUIDANCE (matches the SES MCP for prompt portability):
    - date_from and date_to are REQUIRED for every search.
    - Searches spanning more than 3 calendar days are rejected.
    - Prefer sender_contains + small max_results for source-specific queries.
    - Avoid body previews unless needed (`include_body_preview=False` is faster).

    FAST-PATH ON LARGE MAILBOXES:
    - The date window is enforced server-side, so a 1-day window scans only
      messages from that day regardless of mailbox size.
    - When sender_contains or recipient_contains is a domain or full email
      (`indexexchange.com`, `victoria@example.com`), it's also pushed to the
      JMAP full-text index for server-side pre-filtering. Bare name fragments
      ("brad") fall back to a client-side post-filter and may scan more
      candidates — prefer domains when you have them.
    - We cap client-side scanning at a tunable ceiling (default 2000 emails
      per call, see MAILBUX_SEARCH_MAX_SCAN). If a query hits the cap before
      filling max_results, the response surfaces a `scan_cap_hit` warning and
      `search_stats.scan_cap_hit=True`. When that fires, shrink the window or
      tighten filters and retry.

    Args:
        subject_contains: Case-insensitive substring match in subject.
        subject_keywords: Case-insensitive list of subject keywords (any-match).
        sender_contains: Substring match against the From address.
        recipient_contains: Substring match against the To address.
        date_from: ISO date or datetime (inclusive).
        date_to: ISO date or datetime (inclusive).
        has_payload: True requires a MIME attachment OR an in-body download link;
            False requires neither; None (default) does not filter.
        max_results: Maximum number of results to return (default 25).
        include_body_preview: Include first 500 chars of body and body_urls[] (default False).

    Returns:
        Dictionary with matching emails and metadata. Each match reports
        attachment_names, has_download_links, and (when present) download_links[],
        so you can see what was actually delivered before calling get_email.
    """
    try:
        started = asyncio.get_running_loop().time()

        if subject_keywords is not None:
            subject_keywords = [k for k in subject_keywords if k and k.strip()]
            if len(subject_keywords) == 0:
                subject_keywords = None

        warnings = _build_search_warnings(
            subject_contains=subject_contains,
            subject_keywords=subject_keywords,
            sender_contains=sender_contains,
            recipient_contains=recipient_contains,
            date_from=date_from,
            date_to=date_to,
            has_payload=has_payload,
            max_results=max_results,
        )
        invalid_codes = {"sender_filter_not_meaningful", "recipient_filter_not_meaningful"}
        invalid = [w for w in warnings if w["code"] in invalid_codes]
        if invalid:
            return {
                "status": "error",
                "error": "Query validation failed: non-meaningful sender/recipient filter.",
                "search_warnings": invalid,
            }

        date_from_dt = _parse_date_bound(date_from, is_end=False)
        date_to_dt = _parse_date_bound(date_to, is_end=True)
        if date_from_dt is None or date_to_dt is None:
            return {
                "status": "error",
                "error": "Query validation failed: search_emails requires both date_from and date_to.",
                "search_warnings": [
                    {
                        "code": "bounded_date_window_required",
                        "severity": "high",
                        "message": "search_emails requires both date_from and date_to.",
                        "suggestion": "Use 1-day searches in sequence. At most 3 calendar days per search.",
                    }
                ],
            }
        if date_to_dt < date_from_dt:
            date_from_dt, date_to_dt = date_to_dt, date_from_dt
        if (date_to_dt.date() - date_from_dt.date()).days > 2:
            return {
                "status": "error",
                "error": "Query validation failed: search_emails allows a maximum 3-day date window.",
                "search_warnings": [
                    {
                        "code": "date_window_too_wide",
                        "severity": "high",
                        "message": "search_emails allows a maximum 3-day date window.",
                        "suggestion": "Break the search into 1-day queries; at most 3 calendar days per search.",
                    }
                ],
            }

        account = await _account_id()

        # has_payload semantics:
        #   True  -> JMAP `hasAttachment:true` would miss body-link emails, so
        #            we query without it and then post-filter by attachment or
        #            download_links (mirrors ses_s3_email exactly).
        #   False -> require no attachment AND no download links — same post-filter.
        #   None  -> no payload filter at all.
        need_body = bool(include_body_preview) or has_payload is not None

        # Performance pushdown: when the caller's sender/recipient fragment
        # looks like something Stalwart's `text` index will tokenize (domain
        # or full email), pre-narrow server-side. This is critical on busy
        # mailboxes where the date window alone leaves thousands of candidates
        # but only a handful match the sender. Stalwart's `text` filter
        # searches subject + body + addresses, so it's a strict superset of
        # what the Python post-filter checks — pushing here is always a safe
        # narrowing optimization.
        text_pushdown_terms: list[str] = []
        if _is_fulltext_indexable(sender_contains):
            text_pushdown_terms.append(sender_contains.strip())
        if _is_fulltext_indexable(recipient_contains) and recipient_contains.strip() not in text_pushdown_terms:
            text_pushdown_terms.append(recipient_contains.strip())

        jmap_filter = _build_query_filter(
            subject_contains=subject_contains,
            subject_keywords=subject_keywords,
            sender_contains=sender_contains,
            recipient_contains=recipient_contains,
            date_from=date_from_dt,
            date_to=date_to_dt,
        )
        # AND the text-pushdown terms onto whatever filter we already built.
        # Each term gets AND-tokenized too, so a multi-word value like
        # "John Smith" doesn't degrade into the tokenized OR Stalwart does
        # by default. Single-domain terms ("elcanotek.com") stay as one token.
        if text_pushdown_terms:
            text_conds = [_make_token_field_filter("text", t) for t in text_pushdown_terms]
            text_conds = [c for c in text_conds if c is not None]
            if text_conds:
                pushdown_filter: dict[str, Any]
                if len(text_conds) == 1:
                    pushdown_filter = text_conds[0]
                else:
                    pushdown_filter = {"operator": "AND", "conditions": text_conds}
                if jmap_filter is None:
                    jmap_filter = pushdown_filter
                else:
                    jmap_filter = {"operator": "AND", "conditions": [jmap_filter, pushdown_filter]}

        emails_scanned = 0
        full_fetches = 0
        matches: list[dict[str, Any]] = []
        page_position = 0

        # JMAP-paginated outer loop. Date filter already narrows server-side;
        # this ceiling protects against a misconfigured filter that would
        # otherwise stream the whole window into memory.
        max_scan = max(max_results * 20, SEARCH_MAX_SCAN_DEFAULT)
        hit_scan_cap = False

        while len(matches) < max_results and emails_scanned < max_scan:
            page_limit = min(EMAIL_QUERY_PAGE_LIMIT, max_scan - emails_scanned)
            query_args = {
                "accountId": account,
                "sort": [{"property": "receivedAt", "isAscending": False}],
                "limit": page_limit,
                "position": page_position,
            }
            if jmap_filter is not None:
                query_args["filter"] = jmap_filter

            props = _BODY_PROPS if need_body else _HEADER_PROPS
            fetch_args = {
                "accountId": account,
                "#ids": {"resultOf": "q", "name": "Email/query", "path": "/ids"},
                "properties": props,
            }
            if need_body:
                fetch_args["fetchAllBodyValues"] = True

            responses = await _jmap_call(
                [
                    ["Email/query", query_args, "q"],
                    ["Email/get", fetch_args, "g"],
                ]
            )
            q_resp = next(r for r in responses if r[2] == "q")
            g_resp = next(r for r in responses if r[2] == "g")
            ids = q_resp[1].get("ids") or []
            emails = g_resp[1].get("list") or []
            page_position += len(ids)
            if not ids:
                break
            emails_scanned += len(ids)
            if need_body:
                full_fetches += len(emails)

            # Preserve receivedAt ordering — Email/get returns in undefined order.
            id_index = {eid: i for i, eid in enumerate(ids)}
            emails.sort(key=lambda e: id_index.get(e.get("id"), 0))

            sender_lower = sender_contains.lower() if sender_contains else None
            recipient_lower = recipient_contains.lower() if recipient_contains else None
            subject_lower = subject_contains.lower() if subject_contains else None
            keywords_lower = (
                [kw.lower() for kw in subject_keywords] if subject_keywords else None
            )

            for email_obj in emails:
                match = _format_email_match(email_obj, include_body_preview=include_body_preview)
                # Server-side filters above use AND-of-tokens push-down so the
                # candidate set is already narrow. These Python checks are the
                # final correctness layer for cases the server-side filter
                # can't express (exact phrase order; substring inside a token
                # like "elcanotek" without a dot; from/to which Stalwart
                # tokenizes by name regardless of input).
                if sender_lower and sender_lower not in (match["from"] or "").lower():
                    continue
                if recipient_lower and recipient_lower not in (match["to"] or "").lower():
                    continue
                if subject_lower or keywords_lower:
                    subj_haystack = (match["subject"] or "").lower()
                    if subject_lower and subject_lower not in subj_haystack:
                        continue
                    if keywords_lower and not any(kw in subj_haystack for kw in keywords_lower):
                        continue
                if has_payload is not None:
                    payload_present = match["has_attachments"] or match["has_download_links"]
                    if has_payload and not payload_present:
                        continue
                    if not has_payload and payload_present:
                        continue
                matches.append(match)
                if len(matches) >= max_results:
                    break

            if len(ids) < page_limit:
                break  # no more pages

        if emails_scanned >= max_scan and len(matches) < max_results:
            hit_scan_cap = True

        elapsed = round(asyncio.get_running_loop().time() - started, 3)
        logger.info(
            "search_emails done: scanned=%s matched=%s full_fetches=%s pushdown=%s elapsed_s=%s",
            emails_scanned, len(matches), full_fetches, text_pushdown_terms, elapsed,
        )
        if hit_scan_cap:
            warnings.append(
                {
                    "code": "scan_cap_hit",
                    "severity": "high",
                    "message": (
                        f"Scanned the maximum of {max_scan} emails without filling max_results; "
                        "results may be incomplete."
                    ),
                    "suggestion": (
                        "Narrow the window (try a 1-day search), tighten sender_contains / "
                        "recipient_contains to a domain like 'indexexchange.com', or add a subject filter."
                    ),
                }
            )
        elif emails_scanned >= max_scan and len(matches) == 0:
            warnings.append(
                {
                    "code": "high_scan_zero_matches",
                    "severity": "high",
                    "message": "Scanned a large volume with zero matches; query is likely too broad.",
                    "suggestion": "Tighten sender/subject filters and confirm date bounds before retrying.",
                }
            )

        return {
            "status": "success",
            "matches_found": len(matches),
            "emails_scanned": emails_scanned,
            "search_stats": {
                "full_fetches": full_fetches,
                "elapsed_seconds": elapsed,
                "text_pushdown_terms": text_pushdown_terms,
                "scan_cap": max_scan,
                "scan_cap_hit": hit_scan_cap,
            },
            "search_criteria": {
                "subject_contains": subject_contains,
                "subject_keywords": subject_keywords,
                "sender_contains": sender_contains,
                "recipient_contains": recipient_contains,
                "date_from": date_from,
                "date_to": date_to,
                "effective_date_from": date_from_dt.isoformat(),
                "effective_date_to": date_to_dt.isoformat(),
                "has_payload": has_payload,
            },
            "search_warnings": warnings,
            "emails": matches,
        }
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.exception("search_emails failed")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def search_emails_fuzzy(
    keywords: list[str],
    search_fields: list[str] | None = None,
    match_all: bool = False,
    date_from: str | None = None,
    date_to: str | None = None,
    max_results: int = 50,
) -> dict[str, Any]:
    """Keyword search across subject, sender, and (optionally) body text.

    Uses Stalwart's full-text index when `body` is in `search_fields` —
    JMAP's `text` filter searches subject + headers + body in one shot.
    For `subject`/`sender`-only queries we compose multiple FilterConditions
    so the search stays fast.

    Args:
        keywords: 1-3 keywords (case-insensitive). Empty list rejected.
        search_fields: Subset of {"subject","sender","body"}; default is subject+sender.
        match_all: If True, all keywords must match (AND). Otherwise any (OR).
        date_from / date_to: ISO bounds (inclusive). Bounded windows recommended.
        max_results: Default 50.
    """
    try:
        started = asyncio.get_running_loop().time()
        keywords = [k.strip() for k in (keywords or []) if k and k.strip()]
        if not keywords:
            return {
                "status": "error",
                "error": "keywords must contain at least one non-empty value",
                "search_warnings": [
                    {
                        "code": "empty_keywords",
                        "severity": "high",
                        "message": "Fuzzy search invoked without usable keywords.",
                        "suggestion": "Provide 1-3 concrete keywords or use search_emails.",
                    }
                ],
            }
        if search_fields is None:
            search_fields = ["subject", "sender"]
        valid_fields = {"subject", "sender", "body"}
        if not all(f in valid_fields for f in search_fields):
            return {"status": "error", "error": f"Invalid search_fields. Must be subset of {valid_fields}"}

        warnings = _build_search_warnings(
            subject_contains=None,
            subject_keywords=keywords,
            sender_contains=None,
            recipient_contains=None,
            date_from=date_from,
            date_to=date_to,
            has_payload=None,
            max_results=max_results,
        )

        date_from_dt = _parse_date_bound(date_from, is_end=False)
        date_to_dt = _parse_date_bound(date_to, is_end=True)

        # Build per-keyword filters that match across the requested fields.
        # Each keyword is AND-tokenized within a field (so a multi-word
        # keyword like "John Deere" requires both tokens present, not
        # either) and OR'd across fields; combination across keywords is
        # AND (match_all) or OR (default). Without the per-keyword AND
        # tokenization Stalwart's index defaults to OR-of-tokens and
        # multi-word keywords degrade into "any token matches" — a real
        # correctness issue, not a performance one.
        def keyword_to_filter(kw: str) -> dict[str, Any] | None:
            per_field: list[dict[str, Any]] = []
            for fld_name, jmap_field in (
                ("subject", "subject"),
                ("sender", "from"),
                ("body", "text"),
            ):
                if fld_name in search_fields:
                    f = _make_token_field_filter(jmap_field, kw)
                    if f is not None:
                        per_field.append(f)
            if not per_field:
                return None
            if len(per_field) == 1:
                return per_field[0]
            return {"operator": "OR", "conditions": per_field}

        kw_filters = [keyword_to_filter(kw) for kw in keywords]
        kw_filters = [f for f in kw_filters if f is not None]
        if not kw_filters:
            return {
                "status": "error",
                "error": "All keywords were empty after tokenization.",
            }
        if len(kw_filters) == 1:
            combined = kw_filters[0]
        else:
            combined = {
                "operator": "AND" if match_all else "OR",
                "conditions": kw_filters,
            }

        # Compose with the date bounds.
        date_conds: list[dict[str, Any]] = []
        if date_from_dt is not None:
            date_conds.append({"after": _to_jmap_utc(date_from_dt)})
        if date_to_dt is not None:
            from datetime import timedelta

            date_conds.append({"before": _to_jmap_utc(date_to_dt + timedelta(seconds=1))})
        if date_conds:
            combined = {"operator": "AND", "conditions": [combined, *date_conds]}

        account = await _account_id()

        # Pull header props plus body only when the user explicitly asked for
        # body search (we already have a fast subject-only path via JMAP filters).
        need_body = "body" in search_fields
        props = _BODY_PROPS if need_body else _HEADER_PROPS

        responses = await _jmap_call(
            [
                [
                    "Email/query",
                    {
                        "accountId": account,
                        "filter": combined,
                        "sort": [{"property": "receivedAt", "isAscending": False}],
                        "limit": max_results,
                    },
                    "q",
                ],
                [
                    "Email/get",
                    {
                        "accountId": account,
                        "#ids": {"resultOf": "q", "name": "Email/query", "path": "/ids"},
                        "properties": props,
                        "fetchAllBodyValues": need_body,
                    },
                    "g",
                ],
            ]
        )
        q_resp = next(r for r in responses if r[2] == "q")
        g_resp = next(r for r in responses if r[2] == "g")
        emails = g_resp[1].get("list") or []
        ids = q_resp[1].get("ids") or []
        id_index = {eid: i for i, eid in enumerate(ids)}
        emails.sort(key=lambda e: id_index.get(e.get("id"), 0))

        # Compute a match_score for parity with the SES fuzzy tool: fraction
        # of keywords that appear in the searchable text. Server-side
        # narrowing already AND-tokenized the words within a keyword, but
        # token order isn't enforced — a keyword like "John Deere" would
        # match an email containing "Deere John" or where the tokens fall
        # in different fields. We re-check the literal phrase here, drop
        # any candidate that doesn't match at least one keyword (or all,
        # when match_all=True), and use the literal-match count as the score.
        lower_keywords = [(kw, kw.lower()) for kw in keywords]
        results: list[dict[str, Any]] = []
        for email_obj in emails:
            base = _format_email_match(email_obj, include_body_preview=False)
            haystack_parts = []
            if "subject" in search_fields:
                haystack_parts.append(base["subject"])
            if "sender" in search_fields:
                haystack_parts.append(base["from"])
            if "body" in search_fields and need_body:
                p, h = _body_text_html(email_obj)
                haystack_parts.append(p)
                haystack_parts.append(h)
            hay = " ".join(haystack_parts).lower()
            matched = [orig for orig, low in lower_keywords if low in hay]
            # Strict correctness layer: enforce the caller's match_all
            # contract and discard residual false positives (server-side
            # match_score=0 hits that only landed in results because
            # Stalwart's loose tokenization).
            if match_all and len(matched) != len(keywords):
                continue
            if not match_all and not matched:
                continue
            base["match_score"] = len(matched) / len(keywords) if keywords else 0
            base["matched_keywords"] = matched
            results.append(base)

        results.sort(key=lambda r: (r["match_score"], r["received_at"]), reverse=True)
        elapsed = round(asyncio.get_running_loop().time() - started, 3)
        return {
            "status": "success",
            "matches_found": len(results),
            "emails_scanned": len(ids),
            "search_stats": {"elapsed_seconds": elapsed},
            "search_criteria": {
                "keywords": keywords,
                "search_fields": search_fields,
                "match_all": match_all,
                "date_from": date_from,
                "date_to": date_to,
                "effective_date_from": date_from_dt.isoformat() if date_from_dt else None,
                "effective_date_to": date_to_dt.isoformat() if date_to_dt else None,
            },
            "search_warnings": warnings,
            "emails": results,
        }
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.exception("search_emails_fuzzy failed")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def get_email(email_id: str) -> dict[str, Any]:
    """Fetch a single email by its JMAP id.

    Returns full details — subject, from, to, date, body preview, attachments[],
    download_links[] (URLs the classifier flagged as likely report payloads,
    including click-tracking redirectors), and body_urls[] (every URL found
    in the body with classification flags).
    """
    target = (email_id or "").strip()
    if not target:
        return {"status": "error", "error": "email_id is required."}
    try:
        account = await _account_id()
        responses = await _jmap_call(
            [
                [
                    "Email/get",
                    {
                        "accountId": account,
                        "ids": [target],
                        "properties": _BODY_PROPS,
                        "fetchAllBodyValues": True,
                    },
                    "g",
                ]
            ]
        )
        emails = responses[0][1].get("list") or []
        if not emails:
            return {"status": "error", "email_id": target, "error": "Email not found."}
        email_obj = emails[0]
        plain, html = _body_text_html(email_obj)
        attachments = _attachment_list(email_obj)
        body_urls, download_links = _extract_body_urls(plain, html)

        details = {
            "email_id": email_obj.get("id"),
            "blob_id": email_obj.get("blobId"),
            "subject": email_obj.get("subject") or "",
            "from": _address_list_to_str(email_obj.get("from")),
            "to": _address_list_to_str(email_obj.get("to")),
            "cc": _address_list_to_str(email_obj.get("cc")),
            "date": email_obj.get("sentAt") or email_obj.get("receivedAt") or "",
            "received_at": email_obj.get("receivedAt") or "",
            "message_id": "",  # Stalwart exposes via headers if requested; omit for now.
            "size_bytes": int(email_obj.get("size") or 0),
            "body_preview": (plain[:500] if plain else html[:500]),
            "attachment_count": len(attachments),
            "attachments": attachments,
            "download_link_count": len(download_links),
            "download_links": download_links,
            "body_url_count": len(body_urls),
            "body_urls": body_urls,
        }
        return {"status": "success", "email": details}
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.exception("get_email failed")
        return {"status": "error", "email_id": target, "error": str(e)}


@mcp.tool()
async def download_attachment(
    email_id: str,
    filename: str,
    output_dir: str | None = None,
) -> dict[str, Any]:
    """Download a named MIME attachment from a Mailbux email.

    Workflow: search_emails() → get_email() → download_attachment().

    Args:
        email_id: JMAP email id from search_emails() results.
        filename: The attachment filename from get_email().attachments[].
        output_dir: REQUIRED in practice — pass the absolute per-conversation
            workspace path from the "Working directory" section of the system
            prompt. Falls back to MAILBUX_DOWNLOAD_DIR (a shared dir that may
            be read-only in prod) only when neither side supplies a value.

    Returns:
        Dictionary with: saved_to (local file path), size_bytes, content_type.
    """
    target = (email_id or "").strip()
    if not target:
        return {"status": "error", "error": "email_id is required."}
    if not filename:
        return {"status": "error", "error": "filename is required."}

    download_dir = output_dir or ATTACHMENT_DOWNLOAD_DIR
    Path(download_dir).mkdir(parents=True, exist_ok=True)

    try:
        account = await _account_id()
        responses = await _jmap_call(
            [
                [
                    "Email/get",
                    {
                        "accountId": account,
                        "ids": [target],
                        "properties": ["id", "attachments"],
                    },
                    "g",
                ]
            ]
        )
        emails = responses[0][1].get("list") or []
        if not emails:
            return {"status": "error", "email_id": target, "error": "Email not found."}
        attachments = _attachment_list(emails[0])
        # Try exact, then basename, then sanitized-equality (mirrors SES MCP).
        sanitized_target = _sanitize_download_filename(filename)
        basename_target = os.path.basename(filename)
        match = next(
            (a for a in attachments if a["filename"] == filename),
            None,
        ) or next(
            (a for a in attachments if a["filename"] == basename_target),
            None,
        ) or next(
            (
                a
                for a in attachments
                if _sanitize_download_filename(a["filename"]) == sanitized_target
            ),
            None,
        )
        if not match:
            return {
                "status": "error",
                "error": f"Attachment '{filename}' not found in email",
                "available": [a["filename"] for a in attachments],
            }
        if not match.get("blobId"):
            return {"status": "error", "error": "Attachment is missing a JMAP blobId."}
        data, content_type = await _download_blob(match["blobId"], match["filename"])
        output_path = _build_collision_safe_output_path(
            download_dir, match["filename"], f"{target}:{match['filename']}"
        )
        async with aiofiles.open(output_path, "wb") as f:
            await f.write(data)
        logger.info("Downloaded attachment: %s", output_path)
        return {
            "status": "success",
            "filename": match["filename"],
            "requested_filename": filename,
            "saved_to": str(output_path),
            "size_bytes": len(data),
            "content_type": content_type or match["content_type"],
        }
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.exception("download_attachment failed")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def extract_download_links(
    email_id: str,
    download_likely_only: bool = False,
) -> dict[str, Any]:
    """Extract URLs from an email body and classify each as download-likely or not."""
    target = (email_id or "").strip()
    if not target:
        return {"status": "error", "error": "email_id is required."}
    try:
        account = await _account_id()
        responses = await _jmap_call(
            [
                [
                    "Email/get",
                    {
                        "accountId": account,
                        "ids": [target],
                        "properties": _BODY_PROPS,
                        "fetchAllBodyValues": True,
                    },
                    "g",
                ]
            ]
        )
        emails = responses[0][1].get("list") or []
        if not emails:
            return {"status": "error", "email_id": target, "error": "Email not found."}
        plain, html = _body_text_html(emails[0])
        body_urls, _ = _extract_body_urls(plain, html)
        if download_likely_only:
            body_urls = [u for u in body_urls if u.get("is_download_likely")]
        body_urls.sort(key=lambda x: x.get("is_download_likely", False), reverse=True)
        return {
            "status": "success",
            "email_id": target,
            "subject": emails[0].get("subject") or "",
            "total_links_found": len(body_urls),
            "download_likely_count": sum(1 for u in body_urls if u.get("is_download_likely")),
            "links": body_urls,
        }
    except MailbuxConfigurationError as exc:
        return {"status": "error", "error": str(exc)}
    except Exception as e:
        logger.exception("extract_download_links failed")
        return {"status": "error", "email_id": target, "error": str(e)}


_BROWSER_HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/124.0.0.0 Safari/537.36"
    ),
    "Accept": (
        "text/html,application/xhtml+xml,application/xml;q=0.9,"
        "image/avif,image/webp,application/octet-stream;q=0.8,*/*;q=0.7"
    ),
    "Accept-Language": "en-US,en;q=0.9",
    "Accept-Encoding": "gzip, deflate, br",
    "Upgrade-Insecure-Requests": "1",
}


def _looks_like_html_response(response: httpx.Response) -> bool:
    content_type = (response.headers.get("content-type") or "").lower()
    if "text/html" in content_type or "application/xhtml" in content_type:
        return True
    head = response.content[:512].lstrip().lower()
    return head.startswith(b"<!doctype html") or head.startswith(b"<html")


def _pick_chase_target(html: str, original_url: str) -> str | None:
    candidates: list[tuple[int, str]] = []
    seen: set[str] = set()
    for link in _extract_urls_from_html(html):
        url = link["url"]
        if url in seen or url == original_url:
            continue
        seen.add(url)
        c = _classify_url(url)
        if not c["is_download_likely"] or c["is_image_asset"]:
            continue
        score = 0
        if not c["is_click_tracker"]:
            score += 10
        if c["extension"] in DOWNLOAD_EXTENSIONS and not c["is_image_asset"]:
            score += 5
        if c["has_download_keyword"]:
            score += 2
        candidates.append((score, c["url"]))
    if not candidates:
        return None
    candidates.sort(reverse=True)
    return candidates[0][1]


def _filename_from_response(response: httpx.Response, url: str) -> str:
    content_disposition = response.headers.get("content-disposition", "")
    if content_disposition:
        m = re.search(r'filename[*]?=["\']?(?:UTF-8\'\')?([^"\';\n]+)', content_disposition, re.IGNORECASE)
        if m:
            return unquote(m.group(1).strip())
    parsed = urlparse(url)
    path = unquote(parsed.path)
    filename = Path(path).name
    if filename and "." in filename:
        return filename
    return f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}"


@mcp.tool()
async def download_link_attachment(
    url: str,
    output_dir: str | None = None,
    filename: str | None = None,
    timeout_seconds: int = 120,
    chase_html: bool = True,
) -> dict[str, Any]:
    """Download a file from a URL found in an email body. Mirrors the SES
    MCP's behavior: browser headers, follow redirects, one HTML-chase hop
    when the click-tracker lands on an interstitial page.
    """
    download_dir = output_dir or ATTACHMENT_DOWNLOAD_DIR
    Path(download_dir).mkdir(parents=True, exist_ok=True)
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https"):
        return {"status": "error", "error": f"Invalid URL scheme: {parsed.scheme}. Only http/https supported."}

    classification = _classify_url(url)

    async def _fetch(target_url: str) -> httpx.Response:
        async with httpx.AsyncClient(
            follow_redirects=True,
            timeout=httpx.Timeout(timeout_seconds),
            headers=_BROWSER_HEADERS,
        ) as client:
            return await client.get(target_url)

    def _redirect_chain(response: httpx.Response) -> list[str]:
        return [str(h.url) for h in response.history] + [str(response.url)]

    try:
        response = await _fetch(url)
        chased_from: str | None = None
        chased_to: str | None = None
        if (
            chase_html
            and response.status_code < 400
            and _looks_like_html_response(response)
            and (classification["is_click_tracker"] or len(response.history) > 0)
        ):
            try:
                html = response.text
            except Exception:
                html = ""
            if html:
                next_url = _pick_chase_target(html, url)
                if next_url:
                    chased_from = str(response.url)
                    chased_to = next_url
                    response = await _fetch(next_url)
        response.raise_for_status()
        final_filename = filename or _filename_from_response(response, str(response.url))
        safe_filename = Path(final_filename).name
        output_path = _build_collision_safe_output_path(download_dir, safe_filename, url)
        content = response.content
        async with aiofiles.open(output_path, "wb") as f:
            await f.write(content)
        result: dict[str, Any] = {
            "status": "success",
            "url": url,
            "final_url": str(response.url),
            "filename": safe_filename,
            "saved_to": str(output_path),
            "size_bytes": len(content),
            "content_type": response.headers.get("content-type", "unknown"),
            "http_status": response.status_code,
            "redirect_chain": _redirect_chain(response),
            "is_click_tracker_source": classification["is_click_tracker"],
        }
        if chased_from:
            result["chased_from"] = chased_from
            result["chased_to"] = chased_to
        if _looks_like_html_response(response):
            result["warning"] = (
                "Saved response appears to be HTML, not a binary file. "
                "The click-tracker may require user-side authentication."
            )
        return result
    except httpx.TimeoutException:
        return {"status": "error", "url": url, "error": f"Download timed out after {timeout_seconds} seconds"}
    except httpx.HTTPStatusError as e:
        retryable = not (classification["is_click_tracker"] and e.response.status_code in (400, 410, 404))
        return {
            "status": "error",
            "url": url,
            "error": f"HTTP error {e.response.status_code}: {e.response.reason_phrase}",
            "http_status": e.response.status_code,
            "is_click_tracker_source": classification["is_click_tracker"],
            "retryable": retryable,
        }
    except Exception as e:
        return {"status": "error", "url": url, "error": str(e)}


@mcp.tool()
async def download_all_link_attachments(
    email_id: str,
    output_dir: str | None = None,
    timeout_seconds: int = 120,
) -> dict[str, Any]:
    """Download every download-likely URL found in an email body."""
    target = (email_id or "").strip()
    if not target:
        return {"status": "error", "error": "email_id is required."}
    try:
        links_result = await extract_download_links(email_id=target, download_likely_only=True)
        if links_result["status"] != "success":
            return links_result
        links = links_result["links"]
        if not links:
            return {
                "status": "success",
                "email_id": target,
                "message": "No download links found in email",
                "downloaded": [],
                "errors": [],
            }
        tasks = [
            download_link_attachment(url=link["url"], output_dir=output_dir, timeout_seconds=timeout_seconds)
            for link in links
        ]
        results = await asyncio.gather(*tasks)
        downloaded: list[dict[str, Any]] = []
        errors: list[dict[str, Any]] = []
        for link, result in zip(links, results, strict=True):
            if result.get("status") == "success":
                downloaded.append(
                    {
                        "url": link["url"],
                        "link_text": link.get("link_text"),
                        "saved_to": result["saved_to"],
                        "size_bytes": result["size_bytes"],
                        "content_type": result["content_type"],
                    }
                )
            else:
                errors.append({"url": link["url"], "link_text": link.get("link_text"), "error": result.get("error")})
        return {
            "status": "success",
            "email_id": target,
            "subject": links_result.get("subject", ""),
            "total_links_found": links_result["total_links_found"],
            "downloaded_count": len(downloaded),
            "error_count": len(errors),
            "downloaded": downloaded,
            "errors": errors,
            "download_directory": output_dir or ATTACHMENT_DOWNLOAD_DIR,
        }
    except Exception as e:
        logger.exception("download_all_link_attachments failed")
        return {"status": "error", "email_id": target, "error": str(e)}


if __name__ == "__main__":
    if not MAILBUX_USERNAME or not MAILBUX_PASSWORD:
        logger.error("MAILBUX_USERNAME and MAILBUX_PASSWORD are required to run the Mailbux MCP")
        sys.exit(2)
    logger.info("Starting Mailbux MCP Server")
    logger.info("  JMAP base: %s", MAILBUX_JMAP_BASE_URL)
    logger.info("  SMTP submission: %s:%s", MAILBUX_SMTP_HOST, MAILBUX_SMTP_PORT)
    logger.info("  From address: %s", MAILBUX_FROM_EMAIL)
    mcp.run()
