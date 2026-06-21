#!/usr/bin/env python3
"""SendGrid MCP server implementation for Victoria Terminal.

HTML / subject / body validation lives in `email_lint` (shared with
chat's sendgrid_server + mailbux). This file is the SendGrid-specific
wiring: path security, attachment loading, the Mail() builder, and the
async SendGrid client call.
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import mimetypes
import os
import re
import sys
from collections.abc import Callable, Mapping, MutableMapping, Sequence
from typing import Any

from email_lint import (
    check_template_leakage_legacy as _check_template_leakage_pair,
)
from email_lint import (
    detect_html_content as _detect_html_content,
)
from email_lint import (
    extract_cid_references,
    format_findings,
    partition_findings,
    resolve_content_type,
    validate,
)
from email_lint import (
    extract_cid_references as _extract_cid_references,
)
from email_lint import (
    find_unresolved_template_tokens as _find_unresolved_template_tokens,
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
from python_http_client.exceptions import HTTPError as SendGridHTTPError
from sendgrid import SendGridAPIClient
from sendgrid.helpers.mail import (
    Attachment,
    Content,
    ContentId,
    Disposition,
    Email,
    FileContent,
    FileName,
    FileType,
    Mail,
    Personalization,
)

# Re-exported by name so the existing test files
# (`test_sendgrid_template_validation.py`, `test_mcp_integration.py`,
# `test_sendgrid_html_structure.py`) keep working. New code should
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

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger(__name__)

mcp = FastMCP("sendgrid")

_CLIENT: SendGridAPIClient | None = None


def _validate_path_security(file_path: str) -> str:
    """Validate that a file path is within allowed directories.

    Returns the absolute real path if valid.
    Raises ValueError if the path is outside allowed directories or invalid.
    """
    if not file_path:
        raise ValueError("File path cannot be empty")

    allowed_dirs = []
    cwd = os.getcwd()
    allowed_dirs.append(cwd)

    # Also allow reading from the temp directory if needed for tests
    import tempfile

    allowed_dirs.append(os.path.realpath(tempfile.gettempdir()))

    extra = os.environ.get("CUTLASS_ALLOWED_DIRS", "")
    if extra:
        for d in extra.split(":"):
            d = d.strip()
            if d:
                allowed_dirs.append(os.path.realpath(d))

    try:
        resolved_path = os.path.expanduser(file_path)
        if not os.path.isabs(resolved_path):
            resolved_path = os.path.abspath(resolved_path)

        # Need to check realpath to avoid symlink bypasses
        real_path = os.path.realpath(resolved_path)

        for allowed_dir in allowed_dirs:
            # ensure it ends with slash so /app doesn't match /apple
            allowed_prefix = allowed_dir if allowed_dir.endswith(os.sep) else allowed_dir + os.sep
            if real_path == allowed_dir or real_path.startswith(allowed_prefix):
                return resolved_path

        raise ValueError(f"SECURITY: Path is outside allowed directories: {file_path}")
    except Exception as e:
        if isinstance(e, ValueError):
            raise
        raise ValueError(f"SECURITY: Invalid path: {e}") from e


def _check_template_leakage(content: str) -> str | None:
    """Legacy single-string template leakage check.

    Re-exported (via ``__all__``) so the existing template-validation
    regression tests keep working without rewriting their import path.
    New code should call ``email_lint.check_template_leakage_legacy``
    directly.

    Returns a comma-joined sample of detected demo markers, or None.
    """
    errors, warnings = _check_template_leakage_pair(content)
    hits = errors + warnings
    if not hits:
        return None
    # Strip the rule-code prefix to preserve the old "Amazon US OLV, …" shape.
    cleaned: list[str] = []
    for line in hits:
        # "EL103 (error): … detected — Amazon US OLV, Amazon CA Display …"
        if " — " in line:
            cleaned.append(line.split(" — ", 1)[1])
        else:
            cleaned.append(line)
    return ", ".join(cleaned[:3])


def _read_file_content(path: str, encoding: str = "utf-8", errors: str = "strict") -> str:
    """Read a text file synchronously and return its contents.

    Intended to be wrapped in ``asyncio.to_thread`` from the async send
    paths so a slow disk read never blocks the event loop. Path security
    is enforced separately by ``_validate_path_security`` before this is
    called.
    """
    with open(path, encoding=encoding, errors=errors) as f:
        return f.read()


class SendGridConfigurationError(RuntimeError):
    """Raised when required SendGrid configuration is missing."""


def _get_api_key() -> str:
    api_key = os.environ.get("SENDGRID_API_KEY")
    if not api_key:
        message = "SENDGRID_API_KEY environment variable not set"
        logger.error(message)
        raise SendGridConfigurationError(message)
    return api_key


def _resolve_from_email(explicit_from: str | None = None) -> str:
    if explicit_from:
        return explicit_from

    env_from = os.environ.get("SENDGRID_FROM_EMAIL")
    if not env_from:
        message = "From email address not provided. Pass `from_email` or set SENDGRID_FROM_EMAIL in the environment."
        logger.error(message)
        raise SendGridConfigurationError(message)

    return env_from


def _get_sendgrid_client() -> SendGridAPIClient:
    """Return a cached SendGrid API client instance."""

    global _CLIENT
    if _CLIENT is None:
        api_key = _get_api_key()
        _CLIENT = SendGridAPIClient(api_key)
    return _CLIENT


def _decode_response_body(body: Any) -> str:
    if body in (None, b"", ""):
        return ""
    if isinstance(body, bytes):
        return body.decode("utf-8", errors="replace")
    return str(body)


def _read_and_encode_file(path: str) -> str:
    """Read a file synchronously and return its contents as a base64 string."""
    with open(path, "rb") as f:
        file_data = f.read()
    return base64.b64encode(file_data).decode("utf-8")


async def _sendgrid_request(
    operation: Callable[[SendGridAPIClient], Any],
    *,
    expected_status: Sequence[int] | None = None,
    parse_json: bool = True,
) -> MutableMapping[str, Any]:
    try:
        client = _get_sendgrid_client()
    except SendGridConfigurationError as exc:
        return {"error": str(exc)}

    expected = tuple(expected_status) if expected_status is not None else (200, 201, 202, 204)

    try:
        response = await asyncio.to_thread(operation, client)
    except SendGridHTTPError as exc:
        # python_http_client raises for every HTTP >= 400, so the non-2xx
        # branch below never sees those responses. Map the exception back
        # into the same structured handling: an expected status (e.g. the
        # 404 get_suppression_status relies on) becomes a normal payload,
        # and anything else surfaces SendGrid's actual error body (the
        # field-level validation messages live in exc.body) instead of the
        # bare "HTTP Error 400: Bad Request" string.
        status_code = getattr(exc, "status_code", None)
        body_text = _decode_response_body(getattr(exc, "body", "") or "")
        details: Any
        if parse_json and body_text:
            try:
                details = json.loads(body_text)
            except json.JSONDecodeError:
                details = body_text
        else:
            details = body_text
        if status_code in expected:
            payload = {"status_code": status_code}
            if isinstance(details, dict | list):
                payload["data"] = details
            elif details:
                payload["raw"] = details
            return payload
        message = f"SendGrid API error ({status_code}): {details}"
        logger.error(message)
        return {"error": message, "status_code": status_code, "details": details}
    except Exception as exc:  # pragma: no cover - network/SDK error
        message = f"SendGrid request error: {exc}"
        logger.error(message)
        return {"error": message}

    status_code = getattr(response, "status_code", None)
    logger.info("SendGrid response status: %s", status_code)

    if status_code not in expected:
        body_text = _decode_response_body(getattr(response, "body", ""))
        details: Any
        if parse_json and body_text:
            try:
                details = json.loads(body_text)
            except json.JSONDecodeError:
                details = body_text
        else:
            details = body_text
        message = f"SendGrid API error ({status_code}): {details}"
        logger.error(message)
        return {"error": message, "status_code": status_code, "details": details}

    payload: MutableMapping[str, Any] = {"status_code": status_code}

    headers = getattr(response, "headers", {}) or {}
    try:
        header_items = headers.items()
    except AttributeError:
        header_items = []
    header_map = {str(key).lower(): str(value) for key, value in header_items}
    message_id = header_map.get("x-message-id")
    if message_id:
        payload["message_id"] = message_id

    body_text = _decode_response_body(getattr(response, "body", ""))
    if parse_json and body_text:
        try:
            payload["data"] = json.loads(body_text)
        except json.JSONDecodeError:
            payload["raw"] = body_text
    elif body_text:
        payload["raw"] = body_text

    return payload


def _normalize_recipients(addresses: Sequence[str] | None) -> list[Email]:
    if not addresses:
        return []

    normalized: list[Email] = []
    for address in addresses:
        if not address:
            continue
        for candidate in re.split(r"[,;\n]", address):
            cleaned = candidate.strip()
            if cleaned:
                normalized.append(Email(cleaned))

    return normalized


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
) -> MutableMapping[str, Any]:
    """Send an email via SendGrid. ALWAYS use HTML content with proper styling.

    This is a CALLABLE TOOL - just call it directly, do NOT import it in Python.

    IMPORTANT: Always render email HTML from the canonical_template embedded in
    protocols/email-style.yaml. That file is the single source of truth: it
    mirrors flag/design-system/email-safe.html verbatim, lists the canonical
    palette and placeholders, and defines the rendering_procedure and
    pre_send_checklist. Do not hand-roll email HTML.

    To theme an email, load the selected theme from
    protocols/email_styles/themes/<theme_id>.yaml (default `victoria`) and
    apply its `color_overrides` as literal hex find-replace on canonical_template
    BEFORE substituting {{placeholders}}. Content type is auto-detected — HTML
    tags will be sent as text/html.

    RECIPIENT PRIVACY: The email body must NEVER mention internal implementation
    details such as SendGrid, theme names (e.g. "Victoria theme"), or any tooling
    used to build or send the email. Recipients should not see how the email was made.
    This also applies to internal quality flags about instruction conflict resolution
    or agent reasoning — these belong in logs, not in client-facing email content.

    HTML VALIDATION:
    Content is validated by the shared `email_lint` module. Each finding has a
    stable rule code (EH001, EC001, …) and severity. Errors block sending;
    warnings are surfaced in the response but do not prevent delivery.

    Common rules (see email_lint.RULES for the full catalog):
    - EH001 — element foster-parented out of a <table>/<tbody> (wrap in <tr><td>)
    - EH002 — loose text foster-parented out of a table
    - EH003 — unclosed HTML tags
    - EP001 — *-pad <td> nested inside another *-pad <td> (unclosed inner table)
    - EI001-004 — broken <img src> (empty, file://, data:, non-cid/http)
    - ET001/ET002 — unresolved `{{...}}` / `${...}` tokens in body / subject
    - EC001 — CSS rgba() (alpha stripped by most clients)
    - EL101-103 — canonical_template demo content survived in body

    HANDLING LARGE CONTENT (>50KB):
    For large HTML emails, save content to a file first using run_python, then use
    content_file parameter instead of content. This avoids JSON serialization limits.

    Example:
        1. In run_python: with open('/tmp/email.html', 'w') as f: f.write(html_content)
        2. Call send_email with content_file="/tmp/email.html"

    ATTACHMENTS:
    Pass a list of file paths to attach files to the email. Files are read and
    base64-encoded automatically. MIME types are auto-detected from file extensions.

    Example:
        send_email(..., attachments=["/tmp/report.pdf", "/tmp/data.csv"])

    Args:
        to_email: Recipient email address. Supports comma/semicolon/newline separated values.
        subject: Email subject line
        content: HTML content (must be styled, not plain text). Can be empty if content_file is provided.
        content_file: Path to file containing email content. Use this for large content (>50KB)
                      to avoid JSON serialization issues. Takes precedence over content if both provided.
        content_type: Usually omit - auto-detected from content
        cc_emails: Optional list of CC recipients
        bcc_emails: Optional list of BCC recipients
        attachments: Optional list of file paths to attach to the email. Files are read and
                     base64-encoded. MIME types are auto-detected.
        inline_attachments: Optional list of inline CID attachments. Each item must include
                            'path' and 'cid', and may include 'mime_type'.

    Returns:
        Dictionary with: status="queued", message_id, content_type, html_validated,
        attachments_count and/or inline_attachments_count (if provided)
    """

    try:
        sender = _resolve_from_email(from_email)
    except SendGridConfigurationError as exc:
        return {"error": str(exc)}

    # Handle content_file parameter - read content from file if provided
    actual_content = content
    content_source = "direct"
    if content_file:
        try:
            # Expand user home directory and resolve path securely
            file_path = _validate_path_security(content_file)

            if not os.path.exists(file_path):
                return {"error": f"Content file not found: {content_file}"}

            actual_content = await asyncio.to_thread(_read_file_content, file_path)
            content_source = f"file:{content_file}"
            logger.info("Read email content from file: %s (%d bytes)", content_file, len(actual_content))
        except ValueError as e:
            return {"error": str(e)}
        except PermissionError:
            return {"error": f"Permission denied reading content file: {content_file}"}
        except UnicodeDecodeError as e:
            return {"error": f"Failed to decode content file as UTF-8: {content_file} - {e}"}
        except Exception as e:
            return {"error": f"Failed to read content file: {content_file} - {e}"}

    # Ensure we have content from either source
    if not actual_content or not actual_content.strip():
        return {"error": "Email content is empty. Provide content directly or via content_file parameter."}

    # Auto-detect and resolve content type
    resolved_type, was_corrected = resolve_content_type(actual_content, content_type)

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

    inline_attachment_info: list[dict[str, Any]] = []
    inline_cid_map: dict[str, Mapping[str, Any]] = {}
    if inline_attachments:
        for item in inline_attachments:
            if not isinstance(item, Mapping):
                return {"error": "inline_attachments entries must be objects with keys: path, cid"}
            path_val = str(item.get("path", "")).strip()
            cid_val = str(item.get("cid", "")).strip()
            if not path_val or not cid_val:
                return {"error": "Each inline_attachments entry must include non-empty 'path' and 'cid'"}
            if cid_val in inline_cid_map:
                return {"error": f"Duplicate inline attachment cid: {cid_val}"}
            inline_cid_map[cid_val] = item

    if resolved_type == "text/html":
        cid_refs = extract_cid_references(actual_content)
        if cid_refs:
            if not inline_cid_map:
                missing = ", ".join(sorted(cid_refs))
                return {
                    "error": (
                        "HTML contains cid: references but no inline_attachments were provided. "
                        f"Missing inline cids: {missing}"
                    )
                }
            missing_refs = sorted(cid for cid in cid_refs if cid not in inline_cid_map)
            if missing_refs:
                return {
                    "error": (
                        "HTML contains cid: references that are not mapped in inline_attachments. "
                        f"Missing inline cids: {', '.join(missing_refs)}"
                    )
                }

    mail = Mail(from_email=sender, subject=subject)
    mail.reply_to = Email("brad@elcanotek.com")

    to_recipients = _normalize_recipients([to_email])
    if not to_recipients:
        return {"error": "No valid to_email recipients provided."}

    personalization = Personalization()
    for recipient in to_recipients:
        personalization.add_to(recipient)

    for cc in _normalize_recipients(cc_emails):
        personalization.add_cc(cc)

    for bcc in _normalize_recipients(bcc_emails):
        personalization.add_bcc(bcc)

    mail.add_personalization(personalization)
    mail.add_content(Content(resolved_type, actual_content))

    # Handle regular attachments
    attachment_info: list[dict[str, Any]] = []
    if attachments:
        for file_path in attachments:
            try:
                # Expand and resolve path securely
                resolved_path = _validate_path_security(file_path)

                if not os.path.exists(resolved_path):
                    return {"error": f"Attachment file not found: {file_path}"}

                # Get file info
                file_name = os.path.basename(resolved_path)
                file_size = os.path.getsize(resolved_path)

                # Check size limit (SendGrid has a 30MB total limit, but let's be safe with 25MB per file)
                max_attachment_size = 25 * 1024 * 1024  # 25MB
                if file_size > max_attachment_size:
                    return {
                        "error": f"Attachment too large: {file_name} ({file_size / 1024 / 1024:.1f}MB). "
                        f"Maximum size is 25MB per attachment."
                    }

                # Detect MIME type
                mime_type, _ = mimetypes.guess_type(resolved_path)
                if mime_type is None:
                    mime_type = "application/octet-stream"

                # Read and encode file without blocking the event loop
                encoded_data = await asyncio.to_thread(_read_and_encode_file, resolved_path)

                # Create attachment
                attachment = Attachment(
                    FileContent(encoded_data),
                    FileName(file_name),
                    FileType(mime_type),
                )
                mail.add_attachment(attachment)

                attachment_info.append(
                    {
                        "name": file_name,
                        "size": file_size,
                        "mime_type": mime_type,
                    }
                )
                logger.info("Added attachment: %s (%d bytes, %s)", file_name, file_size, mime_type)

            except ValueError as e:
                return {"error": str(e)}
            except PermissionError:
                return {"error": f"Permission denied reading attachment: {file_path}"}
            except Exception as e:
                return {"error": f"Failed to read attachment {file_path}: {e}"}

    # Handle inline CID attachments
    if inline_attachments:
        for item in inline_attachments:
            try:
                file_path = str(item.get("path", "")).strip()
                cid = str(item.get("cid", "")).strip()

                # Expand and resolve path securely
                resolved_path = _validate_path_security(file_path)

                if not os.path.exists(resolved_path):
                    return {"error": f"Inline attachment file not found: {file_path}"}

                file_name = os.path.basename(resolved_path)
                file_size = os.path.getsize(resolved_path)

                max_attachment_size = 25 * 1024 * 1024  # 25MB
                if file_size > max_attachment_size:
                    return {
                        "error": f"Inline attachment too large: {file_name} ({file_size / 1024 / 1024:.1f}MB). "
                        f"Maximum size is 25MB per attachment."
                    }

                mime_type = str(item.get("mime_type") or "").strip()
                if not mime_type:
                    guessed, _ = mimetypes.guess_type(resolved_path)
                    mime_type = guessed or "application/octet-stream"

                encoded_data = await asyncio.to_thread(_read_and_encode_file, resolved_path)

                attachment = Attachment(
                    FileContent(encoded_data),
                    FileName(file_name),
                    FileType(mime_type),
                )
                attachment.disposition = Disposition("inline")
                attachment.content_id = ContentId(cid)
                mail.add_attachment(attachment)

                inline_attachment_info.append(
                    {
                        "name": file_name,
                        "size": file_size,
                        "mime_type": mime_type,
                        "cid": cid,
                    }
                )
                logger.info("Added inline attachment: %s (%d bytes, %s, cid=%s)", file_name, file_size, mime_type, cid)

            except ValueError as e:
                return {"error": str(e)}
            except PermissionError:
                return {"error": f"Permission denied reading inline attachment: {item.get('path')}"}
            except Exception as e:
                return {"error": f"Failed to read inline attachment {item.get('path')}: {e}"}

    result = await _sendgrid_request(
        lambda client: client.send(mail),
        expected_status=(202,),
        parse_json=False,
    )
    if "error" in result:
        return result

    result["status"] = "queued"
    result["content_type"] = resolved_type
    result["content_source"] = content_source
    result["content_length"] = len(actual_content)
    result["html_validated"] = resolved_type == "text/html"
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
async def send_template_email(
    to_email: str,
    template_id: str,
    dynamic_template_data: Mapping[str, Any],
    *,
    from_email: str | None = None,
    subject: str | None = None,
) -> MutableMapping[str, Any]:
    """Send an email using a SendGrid dynamic template."""

    try:
        sender = _resolve_from_email(from_email)
    except SendGridConfigurationError as exc:
        return {"error": str(exc)}

    if subject is not None:
        # Subject-only check; SendGrid renders the body from the template.
        subject_findings, _ = partition_findings(validate("", subject=subject))
        if subject_findings:
            return {"error": f"Validation Error: {subject_findings[0].format()}"}

    mail = Mail(from_email=sender, to_emails=[to_email])
    mail.reply_to = Email("brad@elcanotek.com")
    mail.template_id = template_id
    mail.dynamic_template_data = dict(dynamic_template_data)

    if subject:
        mail.subject = subject

    result = await _sendgrid_request(
        lambda client: client.send(mail),
        expected_status=(202,),
        parse_json=False,
    )
    if "error" in result:
        return result

    result["status"] = "queued"
    result["template_id"] = template_id
    return result


@mcp.tool()
async def validate_email_content(
    content: str = "",
    content_file: str = "",
    subject: str = "",
) -> MutableMapping[str, Any]:
    """Validate HTML email content before sending. Use this to check for errors
    and fix them before calling send_email.

    Returns a structured result with:
    - valid: true if the content would pass send_email validation
    - findings: list of {rule, severity, message, hint, line} — full detail
    - errors: list of finding-format strings that would block sending
    - warnings: list of finding-format strings (non-blocking)
    - stats: content size, etc.

    Provide either content (HTML string) or content_file (path to HTML file).
    Pass the intended subject line to catch unresolved template tokens there
    as well; omit it to validate the body only.
    """
    actual_content = ""
    source = "content"

    if content_file:
        try:
            resolved_path = _validate_path_security(content_file.strip())
            if not os.path.exists(resolved_path):
                return {"valid": False, "errors": [f"File not found: {content_file}"], "warnings": []}
            actual_content = await asyncio.to_thread(_read_file_content, resolved_path, "utf-8", "replace")
            source = "content_file"
        except ValueError as e:
            return {"valid": False, "errors": [str(e)], "warnings": []}
        except Exception as e:
            return {"valid": False, "errors": [f"Failed to read content file: {e}"], "warnings": []}
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
async def validate_email_address(
    email_address: str,
    *,
    source: str = "Victoria Terminal",
) -> MutableMapping[str, Any]:
    """Validate an email address using the SendGrid email validation API."""

    payload = {
        "email": email_address,
        "source": source,
    }

    return await _sendgrid_request(
        lambda client: client.client.validations.email.post(request_body=payload),
        expected_status=(200, 202),
    )


@mcp.tool()
async def get_suppression_status(email_address: str) -> MutableMapping[str, Any]:
    """Check if an email address is on any SendGrid suppression lists."""

    result = await _sendgrid_request(
        lambda client: client.client.asm.suppressions._(email_address).get(),
        expected_status=(200, 404),
    )

    if "error" in result and result.get("status_code") != 404:
        return result

    status_code = result.get("status_code")
    if status_code == 404:
        return {
            "suppressed": False,
            "status_code": status_code,
        }

    return {
        "suppressed": True,
        "status_code": status_code,
        "data": result.get("data"),
    }


if __name__ == "__main__":
    mcp.run()
