"""Helpers for generating compliant external deal IDs."""

from __future__ import annotations

import re
import secrets
import time
from threading import Lock

_ALLOWED_PREFIX_CHARS = re.compile(r"[^A-Za-z0-9._-]+")
_MAX_EXTERNAL_DEAL_ID_LENGTH = 64
_TIMESTAMP_MS_LENGTH = 13
# 5-digit suffix (was 4) so the total numeric component is 18 digits, matching
# the format the Index Exchange UI generates (e.g. IX777567008013364251 — 20
# total chars: IX + 18 digits). Trader-confirmed: the 17-digit form the MCP
# previously produced reads as "one character short" next to UI-created deals
# in trader spot-checks.
_NUMERIC_SUFFIX_LENGTH = 5
_NUMERIC_COMPONENT_LENGTH = _TIMESTAMP_MS_LENGTH + _NUMERIC_SUFFIX_LENGTH
_MAX_PREFIX_LENGTH = _MAX_EXTERNAL_DEAL_ID_LENGTH - _NUMERIC_COMPONENT_LENGTH

_suffix_lock = Lock()
_last_timestamp_ms = -1
_suffix_counter = 0


def _normalize_prefix(prefix: str) -> str:
    normalized = _ALLOWED_PREFIX_CHARS.sub("", prefix)
    if not normalized:
        normalized = "IX"

    if normalized.startswith("0"):
        normalized = f"X{normalized}"

    return normalized[:_MAX_PREFIX_LENGTH]


def _next_numeric_component() -> str:
    global _last_timestamp_ms, _suffix_counter

    with _suffix_lock:
        timestamp_ms = int(time.time() * 1000)
        if timestamp_ms != _last_timestamp_ms:
            _last_timestamp_ms = timestamp_ms
            _suffix_counter = secrets.randbelow(10**_NUMERIC_SUFFIX_LENGTH)
        else:
            _suffix_counter = (_suffix_counter + 1) % (10**_NUMERIC_SUFFIX_LENGTH)

        return f"{timestamp_ms:0{_TIMESTAMP_MS_LENGTH}d}{_suffix_counter:0{_NUMERIC_SUFFIX_LENGTH}d}"


def generate_external_deal_id(prefix: str = "IX") -> str:
    """Generate a globally unique, IX-compliant external deal ID.

    Format: ``{prefix}{numeric_component}``
    Numeric component is 18 digits: 13-digit ms epoch + 5-digit suffix.
    Example: ``IX177756700801336425``
    """
    normalized_prefix = _normalize_prefix(prefix)
    deal_id = f"{normalized_prefix}{_next_numeric_component()}"

    if deal_id.startswith("0"):
        deal_id = f"X{deal_id[1:]}"

    return deal_id
