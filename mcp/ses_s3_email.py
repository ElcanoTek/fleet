#!/usr/bin/env python3
"""AWS SES + S3 Email Polling MCP Server for Cutlass

This MCP server provides tools for the agent to poll an S3 bucket for emails
that have been deposited by AWS SES. It allows the agent to:
- Check for new emails since the last check
- Get email details (subject, from, to, date, attachments)
- Download attachments (especially CSV files) for data analysis

Setup Requirements:
1. AWS SES configured to receive emails and save to S3
2. S3 bucket with emails stored in a prefix (e.g., emails/)
3. AWS credentials with S3 read access

Environment Variables:
- AWS_ACCESS_KEY_ID: AWS access key
- AWS_SECRET_ACCESS_KEY: AWS secret key
- AWS_REGION: AWS region (e.g., us-east-2)
- EMAIL_S3_BUCKET: S3 bucket name (e.g., victoria-email-inbox)
- EMAIL_S3_PREFIX: S3 prefix for emails (default: emails/)
- EMAIL_S3_ARCHIVE_PREFIX: S3 prefix of the link archive written by the
  ses-email-date-partitioner lambda (default: emails/attachments/). Used as
  a fallback when a live link download fails because the URL has expired.
- EMAIL_ATTACHMENT_DIR: Local directory for downloaded attachments
- EMAIL_LAST_CHECK_FILE: File to store last check timestamp
"""

import asyncio
import email
import hashlib
import json
import logging
import os
import re
from datetime import UTC, datetime, timedelta
from email.header import decode_header
from email.message import Message
from email.parser import BytesHeaderParser
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

import aioboto3
import aiofiles
import httpx
from mcp.server.fastmcp import FastMCP

_LINE_CONT_RE = re.compile(r"=[\r\n]+")
_SPACE_RE = re.compile(r"\s+")
_ALNUM_RE = re.compile(r"[A-Za-z0-9]")
_FILENAME_RE = re.compile(r'filename[*]?=["\']?(?:UTF-8\'\')?([^"\';\n]+)', re.IGNORECASE)


logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
)
logger = logging.getLogger(__name__)

DEBUG_LOG_FILE = os.environ.get("EMAIL_DEBUG_LOG_FILE", "/tmp/ses_s3_email_debug.log")
file_handler = logging.FileHandler(DEBUG_LOG_FILE)
file_handler.setLevel(logging.INFO)
file_handler.setFormatter(logging.Formatter("%(asctime)s - %(name)s - %(levelname)s - %(message)s"))
logger.addHandler(file_handler)

# --- Configuration ---
S3_BUCKET = os.environ.get("EMAIL_S3_BUCKET", "victoria-email-inbox")
S3_PREFIX = os.environ.get("EMAIL_S3_PREFIX", "emails/")
# Production always sets these env vars (Containerfile / .env); the /tmp
# fallback only fires in tests and ad-hoc dev runs, where ephemeral storage
# is fine and leaves no clutter in $HOME.
ATTACHMENT_DOWNLOAD_DIR = os.environ.get("EMAIL_ATTACHMENT_DIR", "/tmp/cutlass/email_attachments")
LAST_CHECK_FILE = os.environ.get("EMAIL_LAST_CHECK_FILE", "/tmp/cutlass/email_last_checked.txt")
AWS_REGION = os.environ.get("AWS_REGION", "us-east-2")
# Where the ses-email-date-partitioner lambda archives email body links
# (see ElcanoTek/ses-s3-setup). Layout:
#   <prefix>by-url/<sha256(url)>/<filename>          archived payloads
#   <prefix>manifests/YYYY/MM/DD/<messageId>.json    per-email link outcomes
ARCHIVE_PREFIX = os.environ.get("EMAIL_S3_ARCHIVE_PREFIX", "emails/attachments/")

# Ensure directories exist
Path(ATTACHMENT_DOWNLOAD_DIR).mkdir(parents=True, exist_ok=True)
Path(LAST_CHECK_FILE).parent.mkdir(parents=True, exist_ok=True)

# Initialize FastMCP
mcp = FastMCP("ses_s3_email")

# Initialize aioboto3 session
session = aioboto3.Session()

# Size of initial S3 byte-range request for header-only filtering
HEADER_FETCH_BYTES = int(os.environ.get("EMAIL_HEADER_FETCH_BYTES", "65536"))

# Emit progress logs every N scanned emails (0 disables periodic progress logs)
SEARCH_PROGRESS_EVERY = int(os.environ.get("EMAIL_SEARCH_PROGRESS_EVERY", "0"))

# Emit current object key logs every N scanned emails (0 disables)
SEARCH_KEY_LOG_EVERY = int(os.environ.get("EMAIL_SEARCH_KEY_LOG_EVERY", "0"))

# Max concurrent header range requests during search listing scans.
SEARCH_HEADER_FETCH_CONCURRENCY = int(os.environ.get("EMAIL_SEARCH_HEADER_FETCH_CONCURRENCY", "4"))

# Optional date-partitioned listing mode. Example format: emails/%Y/%m/%d/
DATE_PREFIX_FORMAT = os.environ.get("EMAIL_S3_DATE_PREFIX_FORMAT", "")
# Safety cap for number of daily prefixes to enumerate before falling back.
MAX_DATE_PREFIX_DAYS = int(os.environ.get("EMAIL_S3_MAX_DATE_PREFIX_DAYS", "62"))


def sanitize_download_filename(filename: str) -> str:
    """Sanitize an email-provided filename without losing meaningful path-like segments."""
    safe_name = (filename or "").strip()
    safe_name = safe_name.replace("\\", "_").replace("/", "_")
    safe_name = _SPACE_RE.sub(" ", safe_name).strip(" .")
    if not safe_name:
        safe_name = f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}"
    return safe_name


def clean_reported_filename(filename: str | None) -> str:
    """Normalize a MIME-decoded attachment filename for return to agents.

    Why: some vendors fold long filenames across multiple header lines, which
    leaves raw CR/LF bytes embedded in the decoded filename. Agents that copy
    the returned string verbatim back into download_attachment then fail the
    lookup because the part's filename on disk has different whitespace.
    Collapsing whitespace here makes the round-trip robust.
    """
    if not filename:
        return ""
    return _SPACE_RE.sub(" ", filename).strip(" .")


def sniff_file_metadata(path: Path | str, max_bytes: int = 4096) -> dict[str, Any]:
    """Inspect a downloaded file and return compact metadata for the agent.

    Handles plain CSV/TSV, UTF-8 BOM, gzip, and zip-of-CSVs. The goal is to
    answer "what's actually in this file and how do I pandas.read_csv it"
    without the agent having to hexdump or re-open the file. Best-effort: any
    failure collapses to a minimal record — the caller should treat missing
    fields as "unknown", not as "definitely absent".
    """
    import csv
    import gzip
    import io
    import zipfile

    p = Path(path)
    meta: dict[str, Any] = {"path": str(p), "size_bytes": None}
    try:
        meta["size_bytes"] = p.stat().st_size
    except OSError:
        return meta

    suffixes = [s.lower() for s in p.suffixes]
    suffix = suffixes[-1] if suffixes else ""
    kind = "other"
    inner_name: str | None = None

    try:
        if suffix == ".zip":
            kind = "zip"
            with zipfile.ZipFile(p) as zf:
                names = zf.namelist()
                meta["zip_members"] = names[:10]
                csv_members = [n for n in names if n.lower().endswith((".csv", ".tsv", ".txt"))]
                if csv_members:
                    inner_name = csv_members[0]
                    with zf.open(inner_name) as zfh:
                        head = zfh.read(max_bytes)
                    kind = "zip_csv"
                else:
                    return {**meta, "kind": "zip", "inner_member_count": len(names)}
        elif suffix == ".gz" or (len(suffixes) >= 2 and suffixes[-2] == ".csv" and suffix == ".gz"):
            kind = "gzip_csv"
            with gzip.open(p, "rb") as gfh:
                head = gfh.read(max_bytes)
        else:
            with open(p, "rb") as fh:
                head = fh.read(max_bytes)
            if suffix in {".csv", ".tsv", ".txt"}:
                kind = "csv" if suffix != ".tsv" else "tsv"
            elif head.lstrip().startswith(b"<"):
                kind = "html_or_xml"
            else:
                kind = "other"

        # BOM detection + encoding guess
        bom = None
        if head.startswith(b"\xef\xbb\xbf"):
            bom = "utf-8-bom"
            head = head[3:]
        elif head.startswith(b"\xff\xfe"):
            bom = "utf-16-le"
        elif head.startswith(b"\xfe\xff"):
            bom = "utf-16-be"
        meta["byte_order_mark"] = bom

        if kind not in {"csv", "tsv", "zip_csv", "gzip_csv"}:
            return {**meta, "kind": kind}

        try:
            text = head.decode("utf-8", errors="replace")
        except UnicodeDecodeError:
            text = head.decode("latin-1", errors="replace")

        # First non-empty line looks like a header row if it has commas/tabs/semicolons
        first_line = ""
        for line in text.splitlines():
            if line.strip():
                first_line = line
                break

        delimiter = ","
        try:
            dialect = csv.Sniffer().sniff(text[:2048], delimiters=",;\t|")
            delimiter = dialect.delimiter
        except csv.Error:
            # Fallback: pick the most common candidate in the first line
            counts = {d: first_line.count(d) for d in [",", "\t", ";", "|"]}
            delimiter = max(counts, key=counts.get) if any(counts.values()) else ","

        # Column headers (best effort)
        header_fields: list[str] = []
        try:
            reader = csv.reader(io.StringIO(text), delimiter=delimiter)
            header_fields = [c.strip().strip('"').strip() for c in next(reader, [])]
        except csv.Error:
            pass

        # Quick row-count estimate from the sample
        newline_count = head.count(b"\n")

        return {
            **meta,
            "kind": kind,
            "inner_member": inner_name,
            "delimiter": delimiter,
            "header_sample": header_fields[:20],
            "sample_newline_count": newline_count,
            "sample_bytes": len(head),
        }
    except Exception as exc:
        logger.warning(f"sniff_file_metadata failed for {p}: {exc}")
        return {**meta, "kind": kind, "sniff_error": str(exc)}


def build_collision_safe_output_path(download_dir: str | Path, filename: str, source_identifier: str) -> Path:
    """Build a deterministic, collision-safe output path for downloads."""
    safe_name = sanitize_download_filename(filename)

    path = Path(safe_name)
    suffix = "".join(path.suffixes)
    stem = path.name[: -len(suffix)] if suffix else path.name
    token = hashlib.sha1(source_identifier.encode("utf-8")).hexdigest()[:8]
    candidate = Path(download_dir) / f"{stem}__{token}{suffix}"

    counter = 1
    while candidate.exists():
        candidate = Path(download_dir) / f"{stem}__{token}_{counter}{suffix}"
        counter += 1

    return candidate


def attachment_name_matches(part_filename: str, requested_filename: str) -> bool:
    """Match attachment names even when the caller passes a saved path or sanitized variant."""
    if part_filename == requested_filename:
        return True

    requested_basename = Path(requested_filename).name
    if part_filename == requested_basename:
        return True

    sanitized_part = sanitize_download_filename(part_filename)
    sanitized_requested = sanitize_download_filename(requested_filename)
    sanitized_basename = sanitize_download_filename(requested_basename)
    return sanitized_part == sanitized_requested or sanitized_part.endswith(sanitized_basename)


def _is_meaningful_text_filter(value: str | None) -> bool:
    """Return True when a text filter has at least one alphanumeric character."""
    if value is None:
        return False
    return bool(_ALNUM_RE.search(value))


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
    """Generate query-quality warnings for LLM agents."""
    warnings: list[dict[str, str]] = []

    if sender_contains is not None and not _is_meaningful_text_filter(sender_contains):
        warnings.append(
            {
                "code": "sender_filter_not_meaningful",
                "severity": "high",
                "message": "sender_contains has no letters or digits and will not narrow results reliably.",
                "suggestion": "Use a sender domain/name fragment like 'magnite.com' or remove sender_contains.",
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
    has_structure_filter = has_payload is not None
    if not has_text_filter and not has_structure_filter:
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


def build_search_prefixes(date_from_dt: datetime | None, date_to_dt: datetime | None) -> list[str]:
    """Build S3 prefixes to scan. Uses date partitions when configured and bounded."""
    if not DATE_PREFIX_FORMAT:
        return [S3_PREFIX]

    if date_from_dt is None or date_to_dt is None:
        return [S3_PREFIX]

    start_date = date_from_dt.date()
    end_date = date_to_dt.date()
    if end_date < start_date:
        start_date, end_date = end_date, start_date

    total_days = (end_date - start_date).days + 1
    if total_days <= 0 or total_days > MAX_DATE_PREFIX_DAYS:
        logger.info(
            "Date prefix mode fallback: total_days=%s exceeds bounds (1..%s), using base prefix",
            total_days,
            MAX_DATE_PREFIX_DAYS,
        )
        return [S3_PREFIX]

    prefixes: list[str] = []
    current = end_date
    while current >= start_date:
        day_dt = datetime(current.year, current.month, current.day, tzinfo=UTC)
        prefix = day_dt.strftime(DATE_PREFIX_FORMAT)
        prefixes.append(prefix)
        current = current - timedelta(days=1)

    return prefixes


def parse_date_bound(date_str: str | None, is_end: bool) -> datetime | None:
    """Parse ISO date/datetime bounds with sensible date-only behavior.

    - Date-only start bound (YYYY-MM-DD): 00:00:00 UTC
    - Date-only end bound (YYYY-MM-DD):   23:59:59.999999 UTC
    - Datetime bounds preserve provided time; naive datetimes assume UTC.
    """
    if not date_str:
        return None

    if "T" in date_str:
        dt = datetime.fromisoformat(date_str)
        if dt.tzinfo is None:
            return dt.replace(tzinfo=UTC)
        return dt.astimezone(UTC)

    dt = datetime.fromisoformat(date_str)
    if is_end:
        return datetime(dt.year, dt.month, dt.day, 23, 59, 59, 999999, tzinfo=UTC)
    return datetime(dt.year, dt.month, dt.day, 0, 0, 0, 0, tzinfo=UTC)


def resolve_search_bounds(
    date_from_dt: datetime | None, date_to_dt: datetime | None
) -> tuple[datetime | None, datetime | None]:
    """Resolve effective search bounds for date-partitioned scans.

    When date partitions are configured and only date_from is provided,
    use current time as date_to to keep prefix listing bounded.
    """
    if not DATE_PREFIX_FORMAT:
        return date_from_dt, date_to_dt

    if date_from_dt is not None and date_to_dt is None:
        return date_from_dt, datetime.now(UTC)

    return date_from_dt, date_to_dt


def decode_email_header(header_value: str) -> str:
    """Decode email header that may contain encoded words."""
    if not header_value:
        return ""
    decoded_parts = decode_header(header_value)
    result = []
    for part, charset in decoded_parts:
        if isinstance(part, bytes):
            result.append(part.decode(charset or "utf-8", errors="replace"))
        else:
            result.append(part)
    return "".join(result)


async def fetch_email_headers(s3_client, s3_key: str) -> dict[str, str]:
    """Fetch and parse email headers only using an S3 range request."""
    response = await s3_client.get_object(
        Bucket=S3_BUCKET,
        Key=s3_key,
        Range=f"bytes=0-{HEADER_FETCH_BYTES - 1}",
    )
    raw_chunk = await response["Body"].read()

    parser = BytesHeaderParser()
    msg = parser.parsebytes(raw_chunk)

    return {
        "subject": decode_email_header(msg.get("Subject", "")),
        "from": decode_email_header(msg.get("From", "")),
        "to": decode_email_header(msg.get("To", "")),
        "date": msg.get("Date", ""),
    }


async def fetch_email_headers_batch(
    s3_client,
    objects: list[dict[str, Any]],
    max_concurrency: int,
) -> list[tuple[dict[str, Any], dict[str, str] | None, Exception | None]]:
    """Fetch headers for a batch of S3 objects with bounded concurrency."""
    if not objects:
        return []

    semaphore = asyncio.Semaphore(max(1, max_concurrency))

    async def fetch_one(obj: dict[str, Any]) -> tuple[dict[str, Any], dict[str, str] | None, Exception | None]:
        async with semaphore:
            try:
                headers = await fetch_email_headers(s3_client, obj["Key"])
                return obj, headers, None
            except Exception as exc:  # noqa: BLE001
                return obj, None, exc

    tasks = [fetch_one(obj) for obj in objects]
    return await asyncio.gather(*tasks)


async def get_last_checked_timestamp() -> datetime:
    """Reads the timestamp of the last successful email check."""
    if not os.path.exists(LAST_CHECK_FILE):
        # Default to a very old date if never checked
        return datetime(2000, 1, 1, tzinfo=UTC)
    async with aiofiles.open(LAST_CHECK_FILE) as f:
        iso_ts = (await f.read()).strip()
        if not iso_ts:
            return datetime(2000, 1, 1, tzinfo=UTC)
        return datetime.fromisoformat(iso_ts)


async def update_last_checked_timestamp(ts: datetime | None = None):
    """Updates the timestamp to the current time or a specified time."""
    if ts is None:
        ts = datetime.now(UTC)
    async with aiofiles.open(LAST_CHECK_FILE, "w") as f:
        await f.write(ts.isoformat())
    logger.info(f"Updated last checked timestamp to {ts.isoformat()}")


# Common file extensions that indicate downloadable files
DOWNLOAD_EXTENSIONS = {
    # Documents
    ".pdf",
    ".doc",
    ".docx",
    ".xls",
    ".xlsx",
    ".ppt",
    ".pptx",
    ".odt",
    ".ods",
    ".odp",
    ".rtf",
    ".txt",
    # Data files
    ".csv",
    ".json",
    ".xml",
    ".yaml",
    ".yml",
    # Archives
    ".zip",
    ".tar",
    ".gz",
    ".rar",
    ".7z",
    ".bz2",
    # Images
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".bmp",
    ".svg",
    ".webp",
    # Media
    ".mp3",
    ".mp4",
    ".wav",
    ".avi",
    ".mov",
    ".mkv",
    # Code/Dev
    ".py",
    ".js",
    ".ts",
    ".html",
    ".css",
    ".sql",
    # Other
    ".exe",
    ".dmg",
    ".pkg",
    ".deb",
    ".rpm",
    ".apk",
}


# Image extensions that should NOT be treated as report payloads.
# Email banners, logos, and tracking pixels almost always have these
# extensions and are never the actual report file. They were previously
# matched by both DOWNLOAD_EXTENSIONS and the keyword heuristic in
# classify_url, which caused download_all_link_attachments to "succeed"
# by pulling the brand banner instead of the report URL.
IMAGE_EXTENSIONS = {
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".bmp",
    ".svg",
    ".webp",
    ".ico",
}


# Click-tracking redirect URLs. These don't have file extensions and
# don't contain "report"/"download" in the encoded payload, but they
# ARE the path to the actual report (the email-marketing platform
# rewrites the original link into a tracker before delivery). We need
# to recognize these as download candidates so:
#   - has_payload=True searches return the email
#   - download_all_link_attachments tries the tracker (not the banner)
# Each entry is (host_suffix, path_substring). Host_suffix matches the
# end of the netloc (case-insensitive); path_substring matches anywhere
# in the path. Either an exact host_suffix match OR a path_substring
# match is enough to flag — many of these platforms split traffic
# across many subdomains.
CLICK_TRACKER_PATTERNS: tuple[tuple[str, str], ...] = (
    # SendGrid (Viant DSP, many SaaS senders)
    ("sendgrid.net", "/ls/click"),
    ("sendgrid.net", "/wf/click"),
    # Mailchimp
    ("list-manage.com", "/track/click"),
    ("mailchimp.com", "/track/click"),
    # HubSpot
    ("hubspotlinks.com", ""),
    ("hs-sites.com", "/e1t/c/"),
    # Iterable
    ("iterable.com", "/api/email/click"),
    ("links.iterable.com", ""),
    # Marketo / Eloqua / Pardot / Salesforce Marketing Cloud
    ("mkt.com", "/trk"),
    ("eloqua.com", "/e/er"),
    ("pardot.com", "/e/"),
    ("exct.net", ""),
    ("exacttarget.com", ""),
    # Postmark
    ("pstmrk.it", ""),
    # Generic patterns the rewriters share
    ("", "/ls/click"),
    ("", "/wf/click"),
    ("", "/track/click"),
)


def is_click_tracker_url(url: str) -> bool:
    """Return True if the URL looks like an email-marketing click-tracking
    redirector. These URLs don't expose the underlying file extension so
    extension/keyword classification misses them, but they are usually
    the only path to the actual payload (e.g. Viant DSP scheduled
    reports route through SendGrid's /ls/click)."""
    try:
        parsed = urlparse(url)
    except Exception:
        return False
    host = (parsed.netloc or "").lower()
    path = (parsed.path or "").lower()
    for host_suffix, path_substring in CLICK_TRACKER_PATTERNS:
        host_ok = (not host_suffix) or host == host_suffix or host.endswith("." + host_suffix)
        path_ok = (not path_substring) or path_substring in path
        # Require a non-empty match on at least one of the two so a
        # blank/blank entry never matches everything.
        if (host_suffix or path_substring) and host_ok and path_ok:
            return True
    return False


def clean_url(url: str) -> str:
    """Clean and normalize a URL extracted from email text.

    Handles common email formatting issues:
    - Removes embedded newlines and line-wrapping artifacts
    - Strips trailing punctuation
    - Fixes quoted-printable encoding artifacts
    - Normalizes whitespace
    """
    if not url:
        return ""

    # Remove soft line breaks (= followed by newline in quoted-printable)
    url = _LINE_CONT_RE.sub("", url)

    # Remove embedded newlines and carriage returns (common in quoted-printable)
    url = url.replace("\n", "").replace("\r", "")

    # Strip leading/trailing whitespace
    url = url.strip()

    # Clean up trailing punctuation that might be captured
    url = url.rstrip(".,;:!?")

    return url


def extract_urls_from_text(text: str) -> list[str]:
    """Extract URLs from plain text using regex."""
    # URL pattern that captures most common URL formats
    # Updated to handle line-wrapped URLs by including = and newlines in the capture
    # then cleaning them up afterward
    url_pattern = r'https?://[^\s<>"\')\]}>]*(?:=[\r\n]+[^\s<>"\')\]}>]*)*'
    urls = re.findall(url_pattern, text)

    # Clean up each URL
    cleaned = []
    for url in urls:
        url = clean_url(url)
        if url:
            cleaned.append(url)

    return cleaned


def extract_urls_from_html(html: str) -> list[dict[str, str]]:
    """Extract URLs from HTML anchor tags with their link text."""
    # Pattern to match <a> tags and extract href and text
    link_pattern = r'<a[^>]+href=["\']([^"\']+)["\'][^>]*>([^<]*)</a>'
    matches = re.findall(link_pattern, html, re.IGNORECASE)

    links = []
    for href, text in matches:
        if href.startswith(("http://", "https://")):
            # Clean and decode the URL
            cleaned_url = clean_url(href)
            links.append(
                {
                    "url": cleaned_url,
                    "text": text.strip() if text else "",
                }
            )
    return links


# Redirector/security wrappers that hide the real target URL. Proofpoint
# URL Defense rewrites every link in mail from senders behind it (Omnicom
# et al.), Amazon Ads routes report links through na.r.ads.amazon.com/CL0/,
# and AWS SES click tracking uses awstrack.me/L0/. Decoding the inner URL
# lets classification see the real target (e.g. a presigned S3 report vs.
# an advertising.amazon.com sign-in page). Fetching still uses the OUTER
# URL so redirect chains and one-shot tokens behave normally.


def _decode_proofpoint_v2(url: str) -> str | None:
    parsed = urlparse(url)
    if "urldefense.proofpoint.com" not in parsed.netloc.lower() or "/v2/url" not in parsed.path:
        return None
    encoded = parse_qs(parsed.query).get("u", [""])[0]
    if not encoded:
        return None
    try:
        return unquote(encoded.replace("_", "/").replace("-", "%"))
    except Exception:
        return None


def _decode_proofpoint_v3(url: str) -> str | None:
    host = urlparse(url).netloc.lower()
    if "urldefense.com" not in host and "urldefense.proofpoint.com" not in host:
        return None
    match = re.search(r"/v3/__(.+?)__;", url)
    if not match:
        return None
    # v3 replaces runs of special chars with '*' (originals live in the
    # base64 tail). Domain and path survive intact, which is all that
    # classification needs.
    return match.group(1)


def _decode_path_embedded(url: str) -> str | None:
    """Amazon Ads na.r.ads.amazon.com/CL0/<url> and awstrack.me/L0/<url>.

    The embedded URL is one percent-encoded path segment (its slashes are
    %2F), and trackers append trailing segments after it (/1/<msgid>/<sig>),
    so split on the first literal '/' before unquoting."""
    host = urlparse(url).netloc.lower()
    if host.endswith("r.ads.amazon.com"):
        marker = "/CL0/"
    elif host.endswith("awstrack.me"):
        marker = "/L0/"
    else:
        return None
    idx = url.find(marker)
    if idx < 0:
        return None
    inner = unquote(url[idx + len(marker) :].split("/", 1)[0])
    return inner if inner.startswith(("http://", "https://")) else None


def unwrap_url(url: str, max_depth: int = 3) -> str:
    """Peel known redirector/security wrappers to reach the inner URL.

    Returns the innermost decodable URL (possibly the input itself).
    Wrappers nest in practice (Proofpoint around awstrack around the real
    link), hence the loop.
    """
    current = url
    for _ in range(max_depth):
        inner = _decode_proofpoint_v2(current) or _decode_proofpoint_v3(current) or _decode_path_embedded(current)
        if not inner or not inner.startswith(("http://", "https://")):
            return current
        current = clean_url(inner)
    return current


def is_presigned_url(url: str) -> bool:
    """Time-limited signed storage URLs (S3/GCS/Azure) — always payloads."""
    parsed = urlparse(url)
    query = parsed.query.lower()
    if "x-amz-signature" in query or "x-amz-credential" in query or "x-goog-signature" in query:
        return True
    return "blob.core.windows.net" in parsed.netloc.lower() and "sig=" in query and "sv=" in query


# Inner-URL hosts that are never report payloads even when surrounding
# keywords look download-ish (sign-in pages, social, scheduling). Derived
# from a 14-day inbox inventory: advertising.amazon.com sign-in links alone
# were ~25% of all "download-likely" URLs before this filter.
JUNK_HOST_SUFFIXES = (
    "advertising.amazon.com",
    "facebook.com",
    "twitter.com",
    "x.com",
    "instagram.com",
    "linkedin.com",
    "calendly.com",
    "calendar.google.com",
    "zoom.us",
    "youtube.com",
    "vimeo.com",
)


def _is_junk_host(host: str) -> bool:
    host = host.lower()
    return any(host == s or host.endswith("." + s) for s in JUNK_HOST_SUFFIXES)


# Never fetch list-management links: some providers process unsubscribes on
# a plain GET, so "downloading" one (e.g. reports.yahooinc.com/unsubscribe,
# which matches the "report" keyword) could silently cancel the scheduled
# report emails these tools exist to fetch.
UNSUBSCRIBE_RE = re.compile(r"unsubscribe|opt[-_]?out|email[-_]?preferences|manage[-_]?preferences", re.IGNORECASE)


def classify_url(url: str) -> dict[str, Any]:
    """Classify a URL and determine if it's likely a download link.

    Known redirector/security wrappers (Proofpoint, Amazon Ads CL0,
    awstrack) are decoded first so extension/keyword/presigned checks see
    the real target instead of the wrapper encoding."""
    cleaned_url = clean_url(url)
    inner_url = unwrap_url(cleaned_url)
    target = inner_url

    parsed = urlparse(target)
    path = unquote(parsed.path)

    # Extract filename from URL path
    filename = Path(path).name if path else ""
    extension = Path(path).suffix.lower() if path else ""

    # Image extensions are almost always email banners, logos, or
    # tracking pixels — never the actual report payload. Demote them
    # so download_all_link_attachments doesn't grab the brand banner
    # and call the email "downloaded" while ignoring the real link.
    is_image_asset = extension in IMAGE_EXTENSIONS

    # Check if extension indicates a downloadable file (excluding images,
    # handled separately as is_image_asset).
    is_download_extension = extension in DOWNLOAD_EXTENSIONS and not is_image_asset

    # Check for common download indicators in URL (includes API patterns)
    download_keywords = [
        "download",
        "attachment",
        "file",
        "export",
        "get",
        "report",
        "generate",
        "csv",
        "xlsx",
        "pdf",
        "api/download",
        "api/export",
        "api/report",
    ]
    has_download_keyword = any(kw in target.lower() for kw in download_keywords)

    # Check query parameters for download-related params
    query_lower = parsed.query.lower()
    download_params = [
        "format=csv",
        "format=xlsx",
        "format=pdf",
        "type=csv",
        # Magnite/Telaria scheduled reports use fmt= (?fmt=xls)
        "fmt=csv",
        "fmt=xls",
        "fmt=xlsx",
        "fmt=pdf",
        "export=",
        "download=",
        "action=download",
        "action=export",
    ]
    has_download_param = any(p in query_lower for p in download_params)

    # Click-tracking redirectors (SendGrid /ls/click, Mailchimp, etc.).
    # These don't have file extensions or readable keywords because the
    # original URL is encoded in a query parameter, but they ARE the
    # only path to the actual payload for many DSP / SaaS senders.
    is_click_tracker = is_click_tracker_url(cleaned_url) or is_click_tracker_url(target)

    is_presigned = is_presigned_url(target)
    is_junk_host = _is_junk_host(parsed.netloc)
    # A presigned URL or an explicit file extension is a real payload even
    # if the name mentions opt-outs (e.g. a CCPA suppression-list export
    # named ccpa_optout_report.csv) — only suppress bare list-management
    # pages, where a GET could actually unsubscribe.
    is_unsubscribe = (
        bool(UNSUBSCRIBE_RE.search(cleaned_url) or UNSUBSCRIBE_RE.search(target))
        and not is_download_extension
        and not is_presigned
    )

    is_download_likely = (
        (is_download_extension or has_download_keyword or has_download_param or is_click_tracker or is_presigned)
        and not is_image_asset
        and not is_junk_host
        and not is_unsubscribe
    )

    return {
        "url": cleaned_url,
        "original_url": url,  # Keep original for reference
        "inner_url": inner_url if inner_url != cleaned_url else None,
        "domain": parsed.netloc,
        "filename": filename if filename else None,
        "extension": extension if extension else None,
        "is_download_likely": is_download_likely,
        "is_image_asset": is_image_asset,
        "is_click_tracker": is_click_tracker,
        "is_presigned": is_presigned,
        "is_junk_host": is_junk_host,
        "is_unsubscribe": is_unsubscribe,
        "has_download_keyword": has_download_keyword,
        "has_download_param": has_download_param,
    }


def _extract_csvs_from_email(raw_email: bytes) -> tuple[str, str, list[dict[str, Any]]]:
    """Helper function to parse email content in a separate thread.

    Extracts CSV attachments from raw email bytes.
    """
    msg: Message = email.message_from_bytes(raw_email)
    email_subject = decode_email_header(msg.get("Subject", "unknown"))
    email_from = decode_email_header(msg.get("From", "unknown"))
    attachments = []

    for part in msg.walk():
        if part.get_content_maintype() == "multipart":
            continue

        filename = part.get_filename()
        if filename:
            filename = decode_email_header(filename)
            content_type = part.get_content_type()

            # Check if it's a CSV
            is_csv = content_type == "text/csv" or filename.lower().endswith(".csv")

            if is_csv:
                payload = part.get_payload(decode=True)
                if isinstance(payload, bytes):
                    attachments.append({"filename": filename, "payload": payload, "content_type": content_type})
    return email_subject, email_from, attachments


def get_filename_from_response(response: httpx.Response, url: str) -> str:
    """Extract filename from HTTP response headers or URL."""
    # Try Content-Disposition header first
    content_disposition = response.headers.get("content-disposition", "")
    if content_disposition:
        # Try to extract filename from Content-Disposition
        filename_match = _FILENAME_RE.search(content_disposition)
        if filename_match:
            return unquote(filename_match.group(1).strip())

    # Fall back to URL path
    parsed = urlparse(url)
    path = unquote(parsed.path)
    filename = Path(path).name

    if filename and "." in filename:
        return filename

    # Generate a filename based on content type
    content_type = response.headers.get("content-type", "").lower()
    extension_map = {
        # Document types
        "application/pdf": ".pdf",
        "text/csv": ".csv",
        "application/csv": ".csv",
        "text/comma-separated-values": ".csv",
        "application/json": ".json",
        "application/xml": ".xml",
        "text/xml": ".xml",
        "text/plain": ".txt",
        # Microsoft Office
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
        "application/vnd.ms-excel": ".xls",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
        "application/msword": ".doc",
        "application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
        "application/vnd.ms-powerpoint": ".ppt",
        # Archives
        "application/zip": ".zip",
        "application/x-gzip": ".gz",
        "application/gzip": ".gz",
        "application/x-tar": ".tar",
        "application/x-rar-compressed": ".rar",
        # Images
        "image/png": ".png",
        "image/jpeg": ".jpg",
        "image/gif": ".gif",
        "image/svg+xml": ".svg",
        # Octet-stream often used for generic binary downloads
        "application/octet-stream": "",  # Will need better handling
    }

    for mime, ext in extension_map.items():
        if mime in content_type:
            if ext:
                return f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}{ext}"
            else:
                # For octet-stream, try to guess from URL or just use .bin
                parsed = urlparse(url)
                url_ext = Path(unquote(parsed.path)).suffix.lower()
                if url_ext and url_ext in DOWNLOAD_EXTENSIONS:
                    return f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}{url_ext}"
                return f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}.bin"

    return f"download_{datetime.now().strftime('%Y%m%d_%H%M%S')}"


# --- MCP Tools ---


def _extract_email_content(msg: Message, extract_body: bool = False) -> dict[str, Any]:
    """Helper to extract attachments and body/links from a message.

    Running this in an executor prevents blocking the event loop during
    CPU-intensive parsing and decoding.

    When extract_body is True, returns:
      - download_links: URLs the classifier flags as `is_download_likely`
        (kept for backward-compat with the search/download tools).
      - body_urls: ALL distinct URLs found in the body, each with
        classification flags (is_download_likely, is_click_tracker,
        is_image_asset, domain, link_text). This is the durable
        triage surface — agents can scan this list when our
        is_download_likely heuristic misses a new wrapper format.
    """
    attachments = []
    body_plain = ""
    body_html = ""
    download_links = []
    body_urls: list[dict[str, Any]] = []

    # Single pass to extract everything
    for part in msg.walk():
        if part.get_content_maintype() == "multipart":
            continue

        content_disposition = part.get("Content-Disposition", "")
        filename = part.get_filename()

        # Check for attachment
        if "attachment" in content_disposition or filename:
            if filename:
                filename = clean_reported_filename(decode_email_header(filename))
                payload = part.get_payload(decode=True)
                attachments.append(
                    {
                        "filename": filename,
                        "content_type": part.get_content_type(),
                        "size_bytes": len(payload) if payload else 0,
                    }
                )

        # Check for body content if requested
        elif extract_body:
            content_type = part.get_content_type()
            if "attachment" not in content_disposition:
                if content_type == "text/plain":
                    payload = part.get_payload(decode=True)
                    if isinstance(payload, bytes):
                        body_plain = payload.decode("utf-8", errors="replace")
                elif content_type == "text/html":
                    payload = part.get_payload(decode=True)
                    if isinstance(payload, bytes):
                        body_html = payload.decode("utf-8", errors="replace")

    # Extract links if body was extracted
    if extract_body:
        seen_urls: set[str] = set()

        def _record(url: str, link_text: str | None, source: str) -> None:
            classification = classify_url(url)
            cleaned = classification["url"]
            if cleaned in seen_urls:
                return
            seen_urls.add(cleaned)
            entry = {
                "url": cleaned,
                "domain": classification["domain"],
                "filename": classification["filename"],
                "extension": classification["extension"],
                "link_text": link_text,
                "source": source,
                "is_download_likely": classification["is_download_likely"],
                "is_click_tracker": classification["is_click_tracker"],
                "is_image_asset": classification["is_image_asset"],
                "url_length": len(cleaned),
            }
            body_urls.append(entry)
            if classification["is_download_likely"]:
                download_links.append(
                    {
                        "url": cleaned,
                        "filename": classification["filename"],
                        "link_text": link_text,
                        "is_click_tracker": classification["is_click_tracker"],
                    }
                )

        if body_html:
            for link in extract_urls_from_html(body_html):
                _record(link["url"], link["text"] or None, "html")

        if body_plain:
            for url in extract_urls_from_text(body_plain):
                _record(url, None, "plain_text")

        # Catch URLs embedded in HTML but outside <a> tags (e.g. inside
        # <img src=> or raw text in the html body) — useful when the
        # html anchor parser misses something the plain-text extractor
        # also missed (alt-text-only, JS-rewritten, etc.).
        if body_html:
            for url in extract_urls_from_text(body_html):
                _record(url, None, "html_text")

    return {
        "attachments": attachments,
        "download_links": download_links,
        "body_urls": body_urls,
        "body_plain": body_plain,
        "body_html": body_html,
    }


@mcp.tool()
async def get_email(s3_key: str) -> dict[str, Any]:
    """Get full details of a specific email. Requires s3_key from search_emails().

    Use search_emails() FIRST to find emails, then use the s3_key from results here.
    Returns full email content including body preview and attachment list.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Args:
        s3_key: The S3 object key from search_emails() results.

    Returns:
        Dictionary with: subject, from, to, date, body_preview,
        attachments[], download_links[] (URLs the classifier marks as
        likely report payloads — includes click-tracking redirectors
        such as SendGrid /ls/click), and body_urls[] (every URL found
        in the body with classification flags, so the agent can triage
        unknown wrapper formats).
    """
    try:
        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=s3_key)
            raw_email = await response["Body"].read()
        msg: Message = email.message_from_bytes(raw_email)

        content = _extract_email_content(msg, extract_body=True)
        attachments = content["attachments"]
        body_plain = content["body_plain"]
        body_html = content["body_html"]
        download_links = content["download_links"]
        body_urls = content["body_urls"]

        details = {
            "s3_key": s3_key,
            "subject": decode_email_header(msg.get("Subject", "")),
            "from": decode_email_header(msg.get("From", "")),
            "to": decode_email_header(msg.get("To", "")),
            "date": msg.get("Date", ""),
            "message_id": msg.get("Message-ID", ""),
            "body_preview": body_plain[:500] if body_plain else body_html[:500],
            "attachment_count": len(attachments),
            "attachments": attachments,
            "download_link_count": len(download_links),
            "download_links": download_links,
            "body_url_count": len(body_urls),
            "body_urls": body_urls,
        }

        return {"status": "success", "email": details}

    except Exception as e:
        logger.error(f"Failed to get email {s3_key}: {e}")
        return {"status": "error", "s3_key": s3_key, "error": str(e)}


@mcp.tool()
async def download_attachment(
    s3_key: str,
    filename: str,
    output_dir: str | None = None,
) -> dict[str, Any]:
    """Download an attachment from an email. Use after search_emails() and get_email().

    Workflow: search_emails() → get_email() → download_attachment()

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Args:
        s3_key: The S3 object key from search_emails() results.
        filename: The attachment filename from get_email() results.
        output_dir: Optional save directory. Defaults to EMAIL_ATTACHMENT_DIR.

    Returns:
        Dictionary with: saved_to (local file path), size_bytes, content_type.
    """
    try:
        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=s3_key)
            raw_email = await response["Body"].read()
        msg: Message = email.message_from_bytes(raw_email)

        download_dir = output_dir or ATTACHMENT_DOWNLOAD_DIR
        Path(download_dir).mkdir(parents=True, exist_ok=True)

        for part in msg.walk():
            if part.get_content_maintype() == "multipart":
                continue

            part_filename = part.get_filename()
            if part_filename:
                part_filename = clean_reported_filename(decode_email_header(part_filename))

                if attachment_name_matches(part_filename, filename):
                    # Found the attachment
                    payload = part.get_payload(decode=True)

                    # get_payload(decode=True) returns None for parts that
                    # don't decode to bytes (e.g. message/rfc822 attachments
                    # or a malformed Content-Transfer-Encoding). Referencing
                    # output_path outside this guard used to raise
                    # UnboundLocalError, surfacing the useless "cannot access
                    # local variable 'output_path'" instead of this error.
                    if not isinstance(payload, bytes):
                        return {
                            "status": "error",
                            "error": (
                                f"Attachment '{part_filename}' was found but its payload could not be "
                                f"decoded to bytes (content type {part.get_content_type()}, "
                                f"transfer encoding {part.get('Content-Transfer-Encoding', 'unknown')})."
                            ),
                            "filename": part_filename,
                            "requested_filename": filename,
                            "content_type": part.get_content_type(),
                        }

                    output_path = build_collision_safe_output_path(
                        download_dir,
                        part_filename,
                        f"{s3_key}:{part_filename}",
                    )

                    # Write the file
                    async with aiofiles.open(output_path, "wb") as f:
                        await f.write(payload)

                    logger.info(f"Downloaded attachment: {output_path}")

                    return {
                        "status": "success",
                        "filename": part_filename,
                        "requested_filename": filename,
                        "saved_to": str(output_path),
                        "size_bytes": len(payload),
                        "content_type": part.get_content_type(),
                        "file_metadata": sniff_file_metadata(output_path),
                    }

        return {
            "status": "error",
            "error": f"Attachment '{filename}' not found in email",
        }

    except Exception as e:
        logger.error(f"Failed to download attachment: {e}")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def get_inbox_stats() -> dict[str, Any]:
    """Get statistics about the email inbox.

    Returns:
        Dictionary with inbox statistics.
    """
    try:
        total_emails = 0
        total_size = 0
        oldest_email = None
        newest_email = None

        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            paginator = s3_client.get_paginator("list_objects_v2")
            async for page in paginator.paginate(Bucket=S3_BUCKET, Prefix=S3_PREFIX):
                if "Contents" not in page:
                    continue
                for obj in page["Contents"]:
                    if obj["Key"] == S3_PREFIX:
                        continue
                    # The link archive lives under the email prefix
                    # (emails/attachments/) — its payloads/manifests are
                    # not emails.
                    if obj["Key"].startswith(ARCHIVE_PREFIX):
                        continue
                    total_emails += 1
                    total_size += obj["Size"]

                    if oldest_email is None or obj["LastModified"] < oldest_email:
                        oldest_email = obj["LastModified"]
                    if newest_email is None or obj["LastModified"] > newest_email:
                        newest_email = obj["LastModified"]

        last_checked = await get_last_checked_timestamp()

        return {
            "status": "success",
            "stats": {
                "total_emails": total_emails,
                "total_size_bytes": total_size,
                "total_size_mb": round(total_size / (1024 * 1024), 2),
                "oldest_email": oldest_email.isoformat() if oldest_email else None,
                "newest_email": newest_email.isoformat() if newest_email else None,
                "last_checked": last_checked.isoformat(),
                "s3_bucket": S3_BUCKET,
                "s3_prefix": S3_PREFIX,
            },
        }

    except Exception as e:
        logger.error(f"Failed to get inbox stats: {e}")
        return {"status": "error", "error": str(e)}


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
    """RECOMMENDED: Search emails by subject, sender, date, or payload presence.

    START HERE when you need to find emails. This is the most efficient way to
    locate specific emails. Returns matching emails with metadata including s3_key
    (needed for get_email and download_attachment).

    PERFORMANCE GUIDANCE (important for large inboxes):
    - date_from and date_to are REQUIRED for every search.
    - Searches spanning more than 3 calendar days are rejected.
    - Prefer sender_contains + small max_results for source-specific queries.
    - Avoid broad multi-day windows during initial discovery.
    - Expand windows only when required evidence is missing.
    - Avoid body previews unless needed (`include_body_preview=False` is faster).

    DAY-BOUNDED RETRIEVAL PATTERNS (recommended):
    - Weekly WoW selection: run separate 1-day queries in sequence.
      Example order per source:
      1) current day (today),
      2) fallback current day-1,
      3) prior target day (selected current end date - 7),
      4) fallback prior target day-1.
    - "Most recent report" retrieval: start with a 1-day query for today,
      then step back one day at a time until a usable report is found.

    ANTI-PATTERN (discouraged, but still allowed when explicitly needed):
    - Do not start with wide ranges (for example 7-14 day windows) when selecting
      current/prior report candidates. Wide ranges increase scan volume and can
      produce overlapping-week selections.

    Date-partition behavior:
    - If EMAIL_S3_DATE_PREFIX_FORMAT is configured and only date_from is provided,
      date_to is auto-resolved to now to keep searches bounded.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Args:
        subject_contains: Case-insensitive substring match in subject line
        subject_keywords: Case-insensitive list of keywords to find in subject (matches any by default)
        sender_contains: Case-insensitive substring match in sender email/name
        recipient_contains: Case-insensitive substring match in To recipients
        date_from: Only emails received on or after this date (ISO format: "2026-01-20")
        date_to: Only emails received on or before this date (ISO format: "2026-01-27")
        has_payload: Filter by presence of a fetchable payload — either a MIME
            attachment OR an in-body download link. True requires at least one;
            False requires neither; None (default) does not filter. Vendors split
            roughly evenly between attaching report files and shipping a download
            link in the body, and this flag abstracts over both delivery modes so
            you do not need per-vendor knowledge to find report emails.
        max_results: Maximum number of results to return (default 25)
        include_body_preview: Include first 500 chars of body (default False)

    Returns:
        Dictionary with matching emails and their metadata. Each match reports
        attachment_names, has_download_links, and (when present) the resolved
        download_links list, so you can see what was actually delivered before
        calling get_email or download_attachment.

    Examples:
        # Day-bounded query for today's candidates (recommended)
        search_emails(
            sender_contains="indexexchange.com",
            subject_keywords=["Master Chief", "Daily Snowflake"],
            date_from="2026-03-23",
            date_to="2026-03-23",
            has_payload=True,
            max_results=3,
        )

        # Prior-week target day query (run separately from current-day query)
        search_emails(
            sender_contains="indexexchange",
            has_payload=True,
            date_from="2026-03-16",
            date_to="2026-03-16",
            max_results=3,
        )
    """
    try:
        started_at = asyncio.get_running_loop().time()

        # Remove empty keywords early (helps avoid accidental broad scans).
        if subject_keywords is not None:
            subject_keywords = [kw for kw in subject_keywords if kw and kw.strip()]
            if len(subject_keywords) == 0:
                subject_keywords = None

        search_warnings = _build_search_warnings(
            subject_contains=subject_contains,
            subject_keywords=subject_keywords,
            sender_contains=sender_contains,
            recipient_contains=recipient_contains,
            date_from=date_from,
            date_to=date_to,
            has_payload=has_payload,
            max_results=max_results,
        )

        # Hard-stop obviously invalid text filters that often cause expensive, irrelevant scans.
        high_invalid_filter_codes = {
            "sender_filter_not_meaningful",
            "recipient_filter_not_meaningful",
        }
        invalid_filter_warnings = [
            warning for warning in search_warnings if warning["code"] in high_invalid_filter_codes
        ]
        if invalid_filter_warnings:
            return {
                "status": "error",
                "error": "Query validation failed: non-meaningful sender/recipient filter.",
                "search_warnings": invalid_filter_warnings,
                "search_criteria": {
                    "subject_contains": subject_contains,
                    "subject_keywords": subject_keywords,
                    "sender_contains": sender_contains,
                    "recipient_contains": recipient_contains,
                    "date_from": date_from,
                    "date_to": date_to,
                    "has_payload": has_payload,
                },
            }

        # Parse and resolve date filters
        date_from_dt = parse_date_bound(date_from, is_end=False)
        date_to_dt = parse_date_bound(date_to, is_end=True)
        if date_from_dt is None or date_to_dt is None:
            return {
                "status": "error",
                "error": "Query validation failed: search_emails requires both date_from and date_to.",
                "search_warnings": [
                    {
                        "code": "bounded_date_window_required",
                        "severity": "high",
                        "message": "search_emails requires both date_from and date_to.",
                        "suggestion": "Use deterministic 1-day searches in sequence. At most 3 calendar days are allowed per search.",
                    }
                ],
                "search_criteria": {
                    "subject_contains": subject_contains,
                    "subject_keywords": subject_keywords,
                    "sender_contains": sender_contains,
                    "recipient_contains": recipient_contains,
                    "date_from": date_from,
                    "date_to": date_to,
                    "has_payload": has_payload,
                    "max_results": max_results,
                },
            }

        if date_to_dt < date_from_dt:
            date_from_dt, date_to_dt = date_to_dt, date_from_dt

        window_days = (date_to_dt.date() - date_from_dt.date()).days + 1
        if window_days > 3:
            # Accept the wide window but scan it day-by-day under the hood.
            # build_search_prefixes already emits one S3 prefix per day, so the
            # work is the same as N independent 1-day calls — and the agent
            # avoids the error/retry loop that used to cost round trips when
            # prompts supplied lookback_days > 3. Hard cap stays at
            # MAX_DATE_PREFIX_DAYS (62); anything wider falls back to the
            # unpartitioned prefix scan, which we still want to warn about.
            search_warnings.append(
                {
                    "code": "date_window_auto_chunked",
                    "severity": "medium" if window_days <= MAX_DATE_PREFIX_DAYS else "high",
                    "message": (
                        f"Window spans {window_days} days; scanning one S3 prefix per day."
                        if window_days <= MAX_DATE_PREFIX_DAYS
                        else f"Window spans {window_days} days; exceeds {MAX_DATE_PREFIX_DAYS}-day partitioned budget — falling back to a broad scan."
                    ),
                    "suggestion": "Prefer targeted 1-day searches for recurring report selection; wide windows are allowed but cost more scan budget.",
                }
            )

        date_from_dt, date_to_dt = resolve_search_bounds(date_from_dt, date_to_dt)
        auto_bounded_date_to_now = bool(DATE_PREFIX_FORMAT and date_from and not date_to and date_to_dt is not None)

        # Normalize search strings
        subject_lower = subject_contains.lower() if subject_contains else None
        sender_lower = sender_contains.lower() if sender_contains else None
        recipient_lower = recipient_contains.lower() if recipient_contains else None
        keywords_lower = [k.lower() for k in subject_keywords] if subject_keywords else None

        matching_emails = []
        emails_scanned = 0
        header_fetches = 0
        full_fetches = 0
        parse_errors = 0

        logger.info(
            "search_emails start: recipient=%s sender=%s subject=%s keywords=%s date_from=%s date_to=%s max_results=%s",
            recipient_contains,
            sender_contains,
            subject_contains,
            subject_keywords,
            date_from,
            date_to,
            max_results,
        )

        search_prefixes = build_search_prefixes(date_from_dt, date_to_dt)
        using_partition_prefixes = not (len(search_prefixes) == 1 and search_prefixes[0] == S3_PREFIX)
        logger.info("search_emails prefixes: count=%s sample=%s", len(search_prefixes), search_prefixes[:3])

        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            paginator = s3_client.get_paginator("list_objects_v2")

            stop_search = False
            for prefix in search_prefixes:
                async for page in paginator.paginate(Bucket=S3_BUCKET, Prefix=prefix):
                    if "Contents" not in page:
                        continue

                    page_objects: list[dict[str, Any]] = []
                    for obj in page["Contents"]:
                        if obj["Key"] == prefix:
                            continue
                        # Skip link-archive payloads/manifests, which live
                        # under the email prefix but are not emails.
                        if obj["Key"].startswith(ARCHIVE_PREFIX):
                            continue

                        last_modified = obj["LastModified"]
                        if not using_partition_prefixes:
                            if date_from_dt and last_modified < date_from_dt:
                                continue
                            if date_to_dt and last_modified > date_to_dt:
                                continue

                        page_objects.append(obj)

                    header_results = await fetch_email_headers_batch(
                        s3_client,
                        page_objects,
                        SEARCH_HEADER_FETCH_CONCURRENCY,
                    )

                    for obj, headers, header_error in header_results:
                        emails_scanned += 1

                        if header_error is not None or headers is None:
                            parse_errors += 1
                            logger.warning(f"Failed to parse email {obj['Key']}: {header_error}")
                            continue

                        header_fetches += 1
                        subject = headers["subject"]
                        sender = headers["from"]
                        recipients = headers["to"]
                        message_date = headers["date"]
                        last_modified = obj["LastModified"]

                        if SEARCH_PROGRESS_EVERY > 0 and emails_scanned % SEARCH_PROGRESS_EVERY == 0:
                            logger.info(
                                "search_emails progress: scanned=%s matched=%s header_fetches=%s full_fetches=%s",
                                emails_scanned,
                                len(matching_emails),
                                header_fetches,
                                full_fetches,
                            )

                        if SEARCH_KEY_LOG_EVERY > 0 and emails_scanned % SEARCH_KEY_LOG_EVERY == 0:
                            logger.info(
                                "search_emails key: scanned=%s key=%s size_bytes=%s received_at=%s",
                                emails_scanned,
                                obj["Key"],
                                obj.get("Size", 0),
                                last_modified.isoformat(),
                            )

                        if subject_lower and subject_lower not in subject.lower():
                            continue

                        if keywords_lower:
                            subject_check = subject.lower()
                            if not any(kw in subject_check for kw in keywords_lower):
                                continue

                        if sender_lower and sender_lower not in sender.lower():
                            continue

                        if recipient_lower and recipient_lower not in recipients.lower():
                            continue

                        need_full = has_payload is not None or include_body_preview

                        attachments: list[dict[str, Any]] = []
                        download_links: list[dict[str, Any]] = []
                        body_urls: list[dict[str, Any]] = []
                        body_plain = ""
                        body_html = ""

                        if need_full:
                            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=obj["Key"])
                            raw_email = await response["Body"].read()
                            full_fetches += 1
                            msg: Message = await asyncio.get_running_loop().run_in_executor(
                                None, email.message_from_bytes, raw_email
                            )

                            content = await asyncio.get_running_loop().run_in_executor(
                                None, _extract_email_content, msg, True
                            )

                            attachments = content["attachments"]
                            download_links = content["download_links"]
                            body_urls = content["body_urls"]
                            body_plain = content["body_plain"]
                            body_html = content["body_html"]

                        if has_payload is not None:
                            payload_count = len(attachments) + len(download_links)
                            if has_payload and payload_count == 0:
                                continue
                            if not has_payload and payload_count > 0:
                                continue

                        email_result = {
                            "s3_key": obj["Key"],
                            "subject": subject,
                            "from": sender,
                            "to": recipients,
                            "date": message_date,
                            "received_at": last_modified.isoformat(),
                            "size_bytes": obj["Size"],
                            "has_attachments": len(attachments) > 0,
                            "attachment_count": len(attachments),
                            "attachment_names": [att["filename"] for att in attachments],
                            "has_download_links": len(download_links) > 0,
                            "download_link_count": len(download_links),
                            "body_url_count": len(body_urls),
                        }

                        if download_links:
                            email_result["download_links"] = download_links

                        if include_body_preview:
                            preview = body_plain[:500] if body_plain else body_html[:500]
                            email_result["body_preview"] = preview
                            # Surface the structured URL list alongside
                            # the preview, so an agent can triage
                            # wrapper formats our classifier missed
                            # without re-fetching the email.
                            if body_urls:
                                email_result["body_urls"] = body_urls

                        matching_emails.append(email_result)

                        if len(matching_emails) >= max_results:
                            stop_search = True
                            break

                    if stop_search:
                        break
                if stop_search:
                    break

        matching_emails.sort(key=lambda x: x["received_at"], reverse=True)

        elapsed_seconds = round(asyncio.get_running_loop().time() - started_at, 3)
        logger.info(
            "search_emails done: scanned=%s matched=%s header_fetches=%s full_fetches=%s parse_errors=%s elapsed_s=%s",
            emails_scanned,
            len(matching_emails),
            header_fetches,
            full_fetches,
            parse_errors,
            elapsed_seconds,
        )

        if emails_scanned >= 2000 and len(matching_emails) == 0:
            search_warnings.append(
                {
                    "code": "high_scan_zero_matches",
                    "severity": "high",
                    "message": "Scanned a large volume with zero matches; query is likely too broad or misconfigured.",
                    "suggestion": "Tighten sender/subject filters and confirm date bounds before retrying.",
                }
            )

        return {
            "status": "success",
            "matches_found": len(matching_emails),
            "emails_scanned": emails_scanned,
            "search_stats": {
                "header_fetches": header_fetches,
                "full_fetches": full_fetches,
                "parse_errors": parse_errors,
                "elapsed_seconds": elapsed_seconds,
            },
            "search_criteria": {
                "subject_contains": subject_contains,
                "subject_keywords": subject_keywords,
                "sender_contains": sender_contains,
                "recipient_contains": recipient_contains,
                "date_from": date_from,
                "date_to": date_to,
                "effective_date_from": date_from_dt.isoformat() if date_from_dt else None,
                "effective_date_to": date_to_dt.isoformat() if date_to_dt else None,
                "auto_bounded_date_to_now": auto_bounded_date_to_now,
                "has_payload": has_payload,
            },
            "search_warnings": search_warnings,
            "emails": matching_emails,
        }

    except Exception as e:
        logger.error(f"Failed to search emails: {e}")
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
    """Keyword search when you don't know exact subject. Use search_emails() if you know subject.

    Use this when you have keywords but don't know the exact email subject line.
    Searches across subject, sender, and optionally body text. Keyword matches are
    case-insensitive.

    PERFORMANCE GUIDANCE:
    - Use this as a fallback after exact `search_emails` attempts.
    - Keep date windows bounded with BOTH date_from and date_to.
    - Prefer 1-day window probes in sequence over broad multi-day scans.
    - Avoid `search_fields=["body"]` unless necessary (full reads are slower).
    - Start with small max_results and widen only if required evidence is missing.

    ANTI-PATTERN (discouraged):
    - Avoid starting fuzzy discovery with large date ranges when finding weekly
      current/prior candidates. Try day-bounded exact search first.

    Date-partition behavior:
    - If EMAIL_S3_DATE_PREFIX_FORMAT is configured and only date_from is provided,
      date_to is auto-resolved to now to keep searches bounded.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Args:
        keywords: List of keywords to search for
        search_fields: Which fields to search in (default: ["subject", "sender"])
                      Options: "subject", "sender", "body"
        match_all: If True, all keywords must match (AND). If False, any keyword (OR)
        date_from: Only emails received on or after this date (ISO format)
        date_to: Only emails received on or before this date (ISO format)
        max_results: Maximum results to return (default 50)

    Returns:
        Dictionary with matching emails sorted by relevance score.

    Examples:
        # Day-bounded fuzzy query (fallback only)
        search_emails_fuzzy(
            keywords=["Master Chief", "Streaming"],
            search_fields=["subject", "sender"],
            match_all=True,
            date_from="2026-03-23",
            date_to="2026-03-23",
            max_results=5,
        )

        # Prior target day fuzzy fallback
        search_emails_fuzzy(
            keywords=["Magnite", "Index Exchange"],
            search_fields=["sender"],
            match_all=False,
            date_from="2026-03-16",
            date_to="2026-03-16",
            max_results=5,
        )
    """
    try:
        started_at = asyncio.get_running_loop().time()

        # Validate keywords early.
        keywords = [kw for kw in keywords if kw and kw.strip()]
        if len(keywords) == 0:
            return {
                "status": "error",
                "error": "keywords must contain at least one non-empty value",
                "search_warnings": [
                    {
                        "code": "empty_keywords",
                        "severity": "high",
                        "message": "Fuzzy search invoked without usable keywords.",
                        "suggestion": "Provide 1-3 concrete keywords or use exact search_emails with sender/subject filters.",
                    }
                ],
            }

        if search_fields is None:
            search_fields = ["subject", "sender"]

        search_warnings = _build_search_warnings(
            subject_contains=None,
            subject_keywords=keywords,
            sender_contains=None,
            recipient_contains=None,
            date_from=date_from,
            date_to=date_to,
            has_payload=None,
            max_results=max_results,
        )

        # Validate search fields
        valid_fields = {"subject", "sender", "body"}
        if not all(field in valid_fields for field in search_fields):
            return {
                "status": "error",
                "error": f"Invalid search_fields. Must be subset of {valid_fields}",
            }

        # Parse and resolve date filters
        date_from_dt = parse_date_bound(date_from, is_end=False)
        date_to_dt = parse_date_bound(date_to, is_end=True)
        date_from_dt, date_to_dt = resolve_search_bounds(date_from_dt, date_to_dt)
        auto_bounded_date_to_now = bool(DATE_PREFIX_FORMAT and date_from and not date_to and date_to_dt is not None)

        # Normalize keywords (case-insensitive)
        keyword_pairs = [(keyword, keyword.lower()) for keyword in keywords]
        search_keywords = [normalized for _, normalized in keyword_pairs]

        matching_emails = []
        emails_scanned = 0
        header_fetches = 0
        full_fetches = 0
        parse_errors = 0

        logger.info(
            "search_emails_fuzzy start: keywords=%s fields=%s match_all=%s date_from=%s date_to=%s max_results=%s",
            keywords,
            search_fields,
            match_all,
            date_from,
            date_to,
            max_results,
        )

        search_prefixes = build_search_prefixes(date_from_dt, date_to_dt)
        using_partition_prefixes = not (len(search_prefixes) == 1 and search_prefixes[0] == S3_PREFIX)
        logger.info("search_emails_fuzzy prefixes: count=%s sample=%s", len(search_prefixes), search_prefixes[:3])

        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            paginator = s3_client.get_paginator("list_objects_v2")

            stop_search = False
            for prefix in search_prefixes:
                async for page in paginator.paginate(Bucket=S3_BUCKET, Prefix=prefix):
                    if "Contents" not in page:
                        continue

                    for obj in page["Contents"]:
                        if obj["Key"] == prefix:
                            continue
                        # Skip link-archive payloads/manifests, which live
                        # under the email prefix but are not emails.
                        if obj["Key"].startswith(ARCHIVE_PREFIX):
                            continue

                        # Apply date filters when not using date-partition prefixes
                        last_modified = obj["LastModified"]
                        if not using_partition_prefixes:
                            if date_from_dt and last_modified < date_from_dt:
                                continue
                            if date_to_dt and last_modified > date_to_dt:
                                continue

                        emails_scanned += 1

                        try:
                            # Fast path: subject/sender-only fuzzy search can be done with header-range read.
                            if "body" not in search_fields:
                                headers = await fetch_email_headers(s3_client, obj["Key"])
                                header_fetches += 1
                                subject = headers["subject"]
                                sender = headers["from"]
                                recipients = headers["to"]
                                message_date = headers["date"]
                                body = ""
                                attachments = []
                            else:
                                response = await s3_client.get_object(Bucket=S3_BUCKET, Key=obj["Key"])
                                raw_email = await response["Body"].read()
                                full_fetches += 1
                                msg: Message = email.message_from_bytes(raw_email)

                                # Extract fields to search
                                subject = decode_email_header(msg.get("Subject", ""))
                                sender = decode_email_header(msg.get("From", ""))
                                recipients = decode_email_header(msg.get("To", ""))
                                message_date = msg.get("Date", "")
                                body = ""

                                for part in msg.walk():
                                    content_type = part.get_content_type()
                                    content_disposition = part.get("Content-Disposition", "")
                                    if "attachment" not in content_disposition and content_type == "text/plain":
                                        payload = part.get_payload(decode=True)
                                        if isinstance(payload, bytes):
                                            body = payload.decode("utf-8", errors="replace")
                                            break

                                # Parse attachments for metadata
                                attachments = []
                                for part in msg.walk():
                                    if part.get_content_maintype() == "multipart":
                                        continue
                                    content_disposition = part.get("Content-Disposition", "")
                                    if "attachment" in content_disposition or part.get_filename():
                                        filename = part.get_filename()
                                        if filename:
                                            filename = decode_email_header(filename)
                                            attachments.append(filename)

                            if SEARCH_PROGRESS_EVERY > 0 and emails_scanned % SEARCH_PROGRESS_EVERY == 0:
                                logger.info(
                                    "search_emails_fuzzy progress: scanned=%s matched=%s header_fetches=%s full_fetches=%s",
                                    emails_scanned,
                                    len(matching_emails),
                                    header_fetches,
                                    full_fetches,
                                )

                            if SEARCH_KEY_LOG_EVERY > 0 and emails_scanned % SEARCH_KEY_LOG_EVERY == 0:
                                logger.info(
                                    "search_emails_fuzzy key: scanned=%s key=%s size_bytes=%s received_at=%s",
                                    emails_scanned,
                                    obj["Key"],
                                    obj.get("Size", 0),
                                    last_modified.isoformat(),
                                )

                            # Build searchable text
                            searchable_parts = []
                            if "subject" in search_fields:
                                searchable_parts.append(subject)
                            if "sender" in search_fields:
                                searchable_parts.append(sender)
                            if "body" in search_fields:
                                searchable_parts.append(body)

                            searchable_text = " ".join(searchable_parts)
                            searchable_text = searchable_text.lower()

                            # Check keyword matches
                            matches = [
                                original for original, normalized in keyword_pairs if normalized in searchable_text
                            ]

                            if match_all:
                                # All keywords must match
                                if len(matches) != len(search_keywords):
                                    continue
                            else:
                                # At least one keyword must match
                                if len(matches) == 0:
                                    continue

                            # Calculate match score (percentage of keywords matched)
                            match_score = len(matches) / len(search_keywords)

                            matching_emails.append(
                                {
                                    "s3_key": obj["Key"],
                                    "subject": subject,
                                    "from": sender,
                                    "to": recipients,
                                    "date": message_date,
                                    "received_at": last_modified.isoformat(),
                                    "size_bytes": obj["Size"],
                                    "has_attachments": len(attachments) > 0,
                                    "attachment_count": len(attachments),
                                    "attachment_names": attachments,
                                    "match_score": match_score,
                                    "matched_keywords": matches,
                                }
                            )

                            if len(matching_emails) >= max_results:
                                stop_search = True
                                break

                        except Exception as e:
                            parse_errors += 1
                            logger.warning(f"Failed to parse email {obj['Key']}: {e}")
                            continue

                    if stop_search:
                        break
                if stop_search:
                    break

        # Sort by match score (highest first), then by date (newest first)
        matching_emails.sort(key=lambda x: (x["match_score"], x["received_at"]), reverse=True)

        elapsed_seconds = round(asyncio.get_running_loop().time() - started_at, 3)
        logger.info(
            "search_emails_fuzzy done: scanned=%s matched=%s header_fetches=%s full_fetches=%s parse_errors=%s elapsed_s=%s",
            emails_scanned,
            len(matching_emails),
            header_fetches,
            full_fetches,
            parse_errors,
            elapsed_seconds,
        )

        if emails_scanned >= 2000 and len(matching_emails) == 0:
            search_warnings.append(
                {
                    "code": "high_scan_zero_matches",
                    "severity": "high",
                    "message": "Scanned a large volume with zero fuzzy matches.",
                    "suggestion": "Use sender-refined exact search_emails first, then retry fuzzy with tighter keywords.",
                }
            )

        return {
            "status": "success",
            "matches_found": len(matching_emails),
            "emails_scanned": emails_scanned,
            "search_stats": {
                "header_fetches": header_fetches,
                "full_fetches": full_fetches,
                "parse_errors": parse_errors,
                "elapsed_seconds": elapsed_seconds,
            },
            "search_criteria": {
                "keywords": keywords,
                "search_fields": search_fields,
                "match_all": match_all,
                "date_from": date_from,
                "date_to": date_to,
                "effective_date_from": date_from_dt.isoformat() if date_from_dt else None,
                "effective_date_to": date_to_dt.isoformat() if date_to_dt else None,
                "auto_bounded_date_to_now": auto_bounded_date_to_now,
            },
            "search_warnings": search_warnings,
            "emails": matching_emails,
        }

    except Exception as e:
        logger.error(f"Failed to fuzzy search emails: {e}")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def extract_download_links(
    s3_key: str,
    download_likely_only: bool = False,
) -> dict[str, Any]:
    """Extract potential download links from an email body.

    This tool parses the email body (both plain text and HTML) to find URLs
    that may be download links. It classifies each URL based on file extension
    and download-related keywords.

    Args:
        s3_key: The S3 object key of the email file.
        download_likely_only: If True, only return links that appear to be downloads
                             (have file extensions or download keywords).

    Returns:
        Dictionary with list of found links and their classifications.
    """
    try:
        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=s3_key)
            raw_email = await response["Body"].read()
        msg: Message = email.message_from_bytes(raw_email)

        # Get email body
        body_plain = ""
        body_html = ""
        for part in msg.walk():
            content_type = part.get_content_type()
            content_disposition = part.get("Content-Disposition", "")

            if "attachment" not in content_disposition:
                if content_type == "text/plain":
                    payload = part.get_payload(decode=True)
                    if isinstance(payload, bytes):
                        body_plain = payload.decode("utf-8", errors="replace")
                elif content_type == "text/html":
                    payload = part.get_payload(decode=True)
                    if isinstance(payload, bytes):
                        body_html = payload.decode("utf-8", errors="replace")

        all_links = []
        seen_urls = set()

        # Extract from HTML (includes link text)
        if body_html:
            html_links = extract_urls_from_html(body_html)
            for link in html_links:
                if link["url"] not in seen_urls:
                    seen_urls.add(link["url"])
                    classification = classify_url(link["url"])
                    classification["link_text"] = link["text"]
                    classification["source"] = "html"
                    all_links.append(classification)

        # Extract from plain text
        if body_plain:
            text_urls = extract_urls_from_text(body_plain)
            for url in text_urls:
                if url not in seen_urls:
                    seen_urls.add(url)
                    classification = classify_url(url)
                    classification["link_text"] = None
                    classification["source"] = "plain_text"
                    all_links.append(classification)

        # Also extract any URLs from HTML that weren't in anchor tags
        if body_html:
            text_urls = extract_urls_from_text(body_html)
            for url in text_urls:
                if url not in seen_urls:
                    seen_urls.add(url)
                    classification = classify_url(url)
                    classification["link_text"] = None
                    classification["source"] = "html_text"
                    all_links.append(classification)

        # Filter if requested
        if download_likely_only:
            all_links = [link for link in all_links if link["is_download_likely"]]

        # Sort by download likelihood
        all_links.sort(key=lambda x: x["is_download_likely"], reverse=True)

        return {
            "status": "success",
            "s3_key": s3_key,
            "subject": decode_email_header(msg.get("Subject", "")),
            "total_links_found": len(all_links),
            "download_likely_count": sum(1 for link in all_links if link["is_download_likely"]),
            "links": all_links,
        }

    except Exception as e:
        logger.error(f"Failed to extract links from email {s3_key}: {e}")
        return {"status": "error", "s3_key": s3_key, "error": str(e)}


# Browser-style headers for fetching email body links. Many email-marketing
# click-tracking redirectors (SendGrid, Mailchimp, etc.) and DSP report-host
# CDNs return 400/403/Bad-Bot pages to default httpx UA strings, so we send
# a real Chrome UA + the headers a browser would send when following an
# email link. This is a server-side fetch on behalf of an authenticated
# user who already received the email — same effective trust as if they
# clicked the link in their mail client.
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
    """Detect whether an httpx response is an HTML landing page rather
    than a binary file. Click-trackers usually 302 to the file directly,
    but some sender platforms first land on an interstitial HTML page
    that contains the real download link as an anchor."""
    content_type = (response.headers.get("content-type") or "").lower()
    if "text/html" in content_type or "application/xhtml" in content_type:
        return True
    # Some servers return the file with a generic content-type; sniff the
    # first bytes for an HTML signature as a fallback.
    head = response.content[:512].lstrip().lower()
    return head.startswith(b"<!doctype html") or head.startswith(b"<html")


def _pick_chase_target(html: str, original_url: str) -> str | None:
    """Given the HTML body of a click-tracker landing page, pick the most
    likely real download URL. Strategy: classify every URL in the HTML
    and prefer download-likely / non-image / non-tracker candidates over
    branding/footer links. Returns None if nothing plausible is found."""
    candidates: list[tuple[int, str]] = []
    seen: set[str] = set()
    for link in extract_urls_from_html(html):
        url = link["url"]
        if url in seen or url == original_url:
            continue
        seen.add(url)
        c = classify_url(url)
        if not c["is_download_likely"] or c["is_image_asset"]:
            continue
        # Score: prefer non-tracker (real direct download) > tracker;
        # prefer URLs with a recognized extension; deprioritize same-host
        # CMS links.
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


def archive_hash_for_url(url: str) -> str:
    """SHA-256 of the cleaned URL string (post clean_url, as both
    extract_download_links and the archiver lambda produce it) — the key
    contract shared with the ses-email-date-partitioner lambda's link
    archiver (see ElcanoTek/ses-s3-setup). Cleaning here means an agent
    that hand-copied a URL with quoted-printable line breaks or trailing
    punctuation still hits the same archive key."""
    return hashlib.sha256(clean_url(url).encode("utf-8")).hexdigest()


async def _fetch_archived_link(
    url: str, download_dir: str | Path, filename: str | None = None
) -> dict[str, Any] | None:
    """Try to serve a link from the S3 archive written at email-arrival time.

    Returns a success result dict if the archive has the URL, else None.
    Never raises — archive lookup is a best-effort fallback."""
    try:
        url_hash = archive_hash_for_url(url)
        prefix = f"{ARCHIVE_PREFIX}by-url/{url_hash}/"
        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            listing = await s3_client.list_objects_v2(Bucket=S3_BUCKET, Prefix=prefix, MaxKeys=1)
            contents = listing.get("Contents", [])
            if not contents:
                return None
            archive_key = contents[0]["Key"]
            archived_name = archive_key.rsplit("/", 1)[-1]
            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=archive_key)
            content = await response["Body"].read()
            metadata = response.get("Metadata", {})

        output_path = build_collision_safe_output_path(download_dir, filename or archived_name, f"archive:{url}")
        async with aiofiles.open(output_path, "wb") as f:
            await f.write(content)

        logger.info(f"Served expired link from S3 archive: {archive_key}")
        return {
            "status": "success",
            "source": "s3_archive",
            "url": url,
            "filename": output_path.name,
            "saved_to": str(output_path),
            "size_bytes": len(content),
            "content_type": response.get("ContentType", "unknown"),
            "archive_key": archive_key,
            "archived_at": metadata.get("archived-at"),
            "file_metadata": sniff_file_metadata(output_path),
            "note": (
                "Live download failed (link likely expired), so this file was "
                "served from the S3 link archive captured when the email "
                "arrived. Contents are identical to what the link returned "
                "at archive time."
            ),
        }
    except Exception as e:
        logger.warning(f"Archive fallback lookup failed for {url}: {e}")
        return None


@mcp.tool()
async def download_link_attachment(
    url: str,
    output_dir: str | None = None,
    filename: str | None = None,
    timeout_seconds: int = 120,
    chase_html: bool = True,
) -> dict[str, Any]:
    """Download a file from a URL found in an email body (not a direct attachment).

    Use this for download links IN the email body, not for actual attachments.
    For actual attachments, use download_attachment() instead.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Browser-emulating fetch: we send a Chrome User-Agent and Accept headers
    so click-tracking redirectors (SendGrid /ls/click, Mailchimp, etc.)
    that block default Python UAs return the real 302 instead of a 400.
    Redirects are followed automatically; the chain is reported back as
    `redirect_chain` so the agent can see where the URL landed.

    HTML-chase fallback: if the URL is a click-tracker (or the redirect
    lands on a text/html page) and `chase_html` is True, we parse the
    landing page once for the most plausible download link and try it.
    This handles sender platforms that interpose an interstitial page
    between the click-tracker and the real file. We do NOT recurse — at
    most one chase per call.

    S3-archive fallback: every download-likely link is archived to S3 by
    a lambda minutes after the email arrives (ElcanoTek/ses-s3-setup). If
    the live fetch fails (expired presigned URL, dead one-shot token) or
    returns an HTML login page, the archived copy is served instead —
    look for `source: "s3_archive"` in the result. This makes historical
    report links downloadable long after the original URL died.

    Args:
        url: The URL to download from.
        output_dir: Optional directory to save the file. Defaults to EMAIL_ATTACHMENT_DIR.
        filename: Optional filename to use. If not provided, extracted from response.
        timeout_seconds: Download timeout in seconds (default 120).
        chase_html: If True (default), follow one HTML interstitial when
            the click-tracker doesn't land on a binary file directly.
            Set False to disable for debugging.

    Returns:
        Dictionary with the local file path and download details.
        On HTML-chase, includes `chased_from` (the URL that returned HTML)
        and `chased_to` (the URL we followed it to).
    """
    download_dir = output_dir or ATTACHMENT_DOWNLOAD_DIR
    Path(download_dir).mkdir(parents=True, exist_ok=True)

    # Validate URL up front
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https"):
        return {
            "status": "error",
            "error": f"Invalid URL scheme: {parsed.scheme}. Only http/https supported.",
        }

    classification = classify_url(url)

    async def _fetch(target_url: str) -> httpx.Response:
        async with httpx.AsyncClient(
            follow_redirects=True,
            timeout=httpx.Timeout(timeout_seconds),
            headers=_BROWSER_HEADERS,
        ) as client:
            return await client.get(target_url)

    def _redirect_chain(response: httpx.Response) -> list[str]:
        chain = [str(h.url) for h in response.history]
        chain.append(str(response.url))
        return chain

    try:
        logger.info(f"Downloading from URL: {url}")
        response = await _fetch(url)

        # If we got HTML back from a click-tracker and chase is allowed,
        # try one more hop to the real download link.
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
                    logger.info(f"Click-tracker landed on HTML; chasing to {next_url}")
                    chased_from = str(response.url)
                    chased_to = next_url
                    response = await _fetch(next_url)

        response.raise_for_status()

        # Determine filename
        final_filename = filename or get_filename_from_response(response, str(response.url))
        safe_filename = Path(final_filename).name
        output_path = build_collision_safe_output_path(download_dir, safe_filename, url)

        content = response.content
        async with aiofiles.open(output_path, "wb") as f:
            await f.write(content)

        logger.info(f"Downloaded link attachment: {output_path}")

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
            "file_metadata": sniff_file_metadata(output_path),
        }
        if chased_from:
            result["chased_from"] = chased_from
            result["chased_to"] = chased_to
        # If the saved file is suspiciously HTML it may be a "log in to
        # download" interstitial we couldn't bypass. Prefer the archived
        # binary captured at email-arrival time when one exists.
        if _looks_like_html_response(response):
            archived = await _fetch_archived_link(url, download_dir, filename)
            if archived is not None:
                # Don't leave the live login-page HTML in the workspace
                # next to the real file we're returning.
                output_path.unlink(missing_ok=True)
                archived["live_result_was"] = "html_page"
                return archived
            result["warning"] = (
                "Saved response appears to be HTML, not a binary file. "
                "The click-tracker may require user-side authentication "
                "or a session cookie. Inspect the file or ask the user "
                "to forward the actual report."
            )
        return result

    except httpx.TimeoutException:
        logger.error(f"Download timeout for URL: {url}")
        archived = await _fetch_archived_link(url, download_dir, filename)
        if archived is not None:
            archived["live_result_was"] = "timeout"
            return archived
        return {
            "status": "error",
            "url": url,
            "error": f"Download timed out after {timeout_seconds} seconds",
        }
    except httpx.HTTPStatusError as e:
        logger.error(f"HTTP error downloading {url}: {e}")
        archived = await _fetch_archived_link(url, download_dir, filename)
        if archived is not None:
            archived["live_result_was"] = f"http_{e.response.status_code}"
            return archived
        # Click-tracker URLs that 400 are usually either truncated, or
        # one-shot tokens that have already been clicked. Either way
        # retrying via a different HTTP fetcher won't help — surface a
        # structured `retryable: false` so the agent knows to escalate.
        retryable = not (classification["is_click_tracker"] and e.response.status_code in (400, 410, 404))
        return {
            "status": "error",
            "url": url,
            "error": f"HTTP error {e.response.status_code}: {e.response.reason_phrase}",
            "http_status": e.response.status_code,
            "is_click_tracker_source": classification["is_click_tracker"],
            "retryable": retryable,
            "hint": (
                "Click-tracker URL rejected by the redirector. The link is "
                "likely truncated, expired, or browser-session-bound. No "
                "archived copy was found for this URL either (the archive "
                "covers emails received after 2026-05-28). Do not retry "
                "through other HTTP fetchers — ask the user to forward the "
                "actual file."
            )
            if not retryable
            else None,
        }
    except Exception as e:
        logger.error(f"Failed to download from URL {url}: {e}")
        archived = await _fetch_archived_link(url, download_dir, filename)
        if archived is not None:
            archived["live_result_was"] = f"exception: {e!s:.120}"
            return archived
        return {"status": "error", "url": url, "error": str(e)}


@mcp.tool()
async def download_all_link_attachments(
    s3_key: str,
    output_dir: str | None = None,
    timeout_seconds: int = 120,
) -> dict[str, Any]:
    """Download all likely download links from an email.

    This tool extracts all URLs from an email that appear to be download links
    (based on file extension or download keywords) and downloads them all.

    Args:
        s3_key: The S3 object key of the email file.
        output_dir: Optional directory to save files. Defaults to EMAIL_ATTACHMENT_DIR.
        timeout_seconds: Download timeout per file in seconds (default 120).

    Returns:
        Dictionary with list of downloaded files and any errors.
    """
    try:
        # First extract the download links
        links_result = await extract_download_links(s3_key, download_likely_only=True)

        if links_result["status"] != "success":
            return links_result

        if not links_result["links"]:
            return {
                "status": "success",
                "s3_key": s3_key,
                "message": "No download links found in email",
                "downloaded": [],
                "errors": [],
            }

        downloaded = []
        errors = []

        # Create tasks for all downloads
        download_tasks = []
        for link in links_result["links"]:
            download_tasks.append(
                download_link_attachment(
                    url=link["url"],
                    output_dir=output_dir,
                    timeout_seconds=timeout_seconds,
                )
            )

        # Execute downloads concurrently
        results = await asyncio.gather(*download_tasks)

        # Process results
        for link, result in zip(links_result["links"], results, strict=True):
            if result["status"] == "success":
                downloaded.append(
                    {
                        "url": link["url"],
                        "link_text": link.get("link_text"),
                        "saved_to": result["saved_to"],
                        "size_bytes": result["size_bytes"],
                        "content_type": result["content_type"],
                        # "live" or "s3_archive" (served from the link
                        # archive because the live URL had expired)
                        "source": result.get("source", "live"),
                    }
                )
            else:
                errors.append(
                    {
                        "url": link["url"],
                        "link_text": link.get("link_text"),
                        "error": result["error"],
                    }
                )

        return {
            "status": "success",
            "s3_key": s3_key,
            "subject": links_result["subject"],
            "total_links_found": links_result["total_links_found"],
            "downloaded_count": len(downloaded),
            "error_count": len(errors),
            "downloaded": downloaded,
            "errors": errors,
            "download_directory": output_dir or ATTACHMENT_DOWNLOAD_DIR,
        }

    except Exception as e:
        logger.error(f"Failed to download link attachments from {s3_key}: {e}")
        return {"status": "error", "s3_key": s3_key, "error": str(e)}


@mcp.tool()
async def find_latest_report(
    sender_contains: str | None = None,
    subject_contains: str | None = None,
    subject_keywords: list[str] | None = None,
    recipient_contains: str | None = None,
    on_or_before: str | None = None,
    lookback_days: int = 14,
    require_payload: bool = True,
    max_candidates: int = 1,
) -> dict[str, Any]:
    """Walk back day-by-day from `on_or_before` until a qualifying email is found.

    Use this instead of looping search_emails() yourself when you need "the most
    recent report from vendor X on or before date Y". Stops after `lookback_days`
    or after collecting `max_candidates` matches, whichever comes first.

    Day-by-day stepping keeps each underlying S3 scan narrow, and the total
    number of scanned days is capped at `lookback_days` (hard max 60).

    Args:
        sender_contains: Case-insensitive substring match on sender.
        subject_contains: Case-insensitive substring match on subject.
        subject_keywords: Case-insensitive list of keywords to find in subject.
        recipient_contains: Case-insensitive substring match on recipient.
        on_or_before: ISO date (YYYY-MM-DD) to start walking back from. Defaults
            to today in UTC.
        lookback_days: Maximum number of days to walk back (default 14, hard cap 60).
        require_payload: Require a fetchable payload (attachment or download link).
            Default True matches the typical "find the report" use case.
        max_candidates: Stop after collecting this many matches (default 1).

    Returns:
        status, days_searched, first_match_date (or None), emails (list of
        matches with s3_key, subject, from, received_at, attachment_names,
        and download_links when present).
    """
    try:
        # Resolve on_or_before to a UTC date
        if on_or_before:
            try:
                anchor_dt = datetime.fromisoformat(on_or_before)
                if anchor_dt.tzinfo is None:
                    anchor_dt = anchor_dt.replace(tzinfo=UTC)
            except ValueError:
                return {
                    "status": "error",
                    "error": f"Invalid on_or_before date: {on_or_before!r}. Use ISO format YYYY-MM-DD.",
                }
            anchor_date = anchor_dt.date()
        else:
            anchor_date = datetime.now(UTC).date()

        lookback_days = max(1, min(int(lookback_days), 60))
        max_candidates = max(1, int(max_candidates))

        collected: list[dict[str, Any]] = []
        days_searched = 0
        first_match_date: str | None = None
        payload_flag = True if require_payload else None

        for offset in range(lookback_days):
            day = anchor_date - timedelta(days=offset)
            day_iso = day.isoformat()
            days_searched += 1

            result = await search_emails(
                subject_contains=subject_contains,
                subject_keywords=subject_keywords,
                sender_contains=sender_contains,
                recipient_contains=recipient_contains,
                date_from=day_iso,
                date_to=day_iso,
                has_payload=payload_flag,
                max_results=max(max_candidates - len(collected), 1),
                include_body_preview=False,
            )

            if result.get("status") != "success":
                continue

            for match in result.get("emails", []):
                collected.append(match)
                if first_match_date is None:
                    first_match_date = day_iso
                if len(collected) >= max_candidates:
                    break

            if len(collected) >= max_candidates:
                break

        return {
            "status": "success",
            "days_searched": days_searched,
            "anchor_date": anchor_date.isoformat(),
            "first_match_date": first_match_date,
            "match_count": len(collected),
            "emails": collected,
            "search_criteria": {
                "sender_contains": sender_contains,
                "subject_contains": subject_contains,
                "subject_keywords": subject_keywords,
                "recipient_contains": recipient_contains,
                "on_or_before": anchor_date.isoformat(),
                "lookback_days": lookback_days,
                "require_payload": require_payload,
                "max_candidates": max_candidates,
            },
        }

    except Exception as e:
        logger.error(f"find_latest_report failed: {e}")
        return {"status": "error", "error": str(e)}


@mcp.tool()
async def list_archived_attachments(s3_key: str) -> dict[str, Any]:
    """List the link-archive manifest for an email: which body links were
    archived to S3 at arrival time, which were skipped, and which failed.

    A lambda archives every download-likely body link minutes after an
    email arrives (so files are retrievable after the original URLs
    expire). Use this to see what is available for a historical email
    before downloading: entries with status "archived" can be fetched via
    download_link_attachment(url=<entry url>) — it serves the archived
    copy automatically when the live URL is dead.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    Args:
        s3_key: The S3 object key of the email file
            (e.g. emails/2026/06/11/<messageId>).

    Returns:
        Dictionary with the manifest contents: per-link status
        ("archived", "already_archived", "skipped_not_download_likely",
        "failed_http_404", ...), archive keys, and file metadata. Emails
        received before the archiver was deployed (2026-05-28) have no
        manifest.
    """
    match = re.match(r"^.*?(\d{4}/\d{2}/\d{2})/([^/]+)$", s3_key)
    if not match:
        return {
            "status": "error",
            "s3_key": s3_key,
            "error": (
                "Could not derive a date partition from this key; expected a key like emails/YYYY/MM/DD/<messageId>."
            ),
        }
    manifest_key = f"{ARCHIVE_PREFIX}manifests/{match.group(1)}/{match.group(2)}.json"
    try:
        async with session.client("s3", region_name=AWS_REGION) as s3_client:
            response = await s3_client.get_object(Bucket=S3_BUCKET, Key=manifest_key)
            manifest = json.loads(await response["Body"].read())
        return {"status": "success", "manifest_key": manifest_key, **manifest}
    except Exception as e:
        # Only a genuinely missing key means "no manifest". Other S3
        # ClientErrors (AccessDenied, throttling, expired creds) must
        # surface as errors, not as a benign absence.
        error_code = getattr(e, "response", {}).get("Error", {}).get("Code", "")
        if error_code in ("NoSuchKey", "404", "NotFound"):
            return {
                "status": "not_found",
                "s3_key": s3_key,
                "manifest_key": manifest_key,
                "message": (
                    "No archive manifest for this email. Either it arrived "
                    "before the link archiver was deployed (2026-05-28) or "
                    "archiving failed; try the live URLs via "
                    "download_link_attachment."
                ),
            }
        logger.error(f"Failed to read archive manifest {manifest_key}: {e}")
        return {"status": "error", "s3_key": s3_key, "error": str(e)}


if __name__ == "__main__":
    logger.info("Starting SES S3 Email MCP Server")
    logger.info(f"  Bucket: {S3_BUCKET}")
    logger.info(f"  Prefix: {S3_PREFIX}")
    logger.info(f"  Region: {AWS_REGION}")
    mcp.run()
