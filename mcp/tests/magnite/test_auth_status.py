"""Tests for the simplified Magnite auth-status tool.

The DV+ reporting and ClearLine Demand Management surfaces share one set
of Basic-auth credentials, so `magnite_auth_status` reports whether
ACCESS_KEY/SECRET_KEY are present, whether the mp-vendor ACCOUNT_ID is
configured, and the two API hosts. There is no separate session/cookie
state to surface — every request authenticates via HTTP Basic Auth.
"""

import os
import sys
from unittest.mock import patch

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from magnite_mcp import magnite_auth_status


@pytest.mark.asyncio
async def test_reports_configured_when_keys_and_account_present(
    mock_magnite_dv_api,  # noqa: ARG001
):
    """When ACCESS_KEY, SECRET_KEY, and ACCOUNT_ID are all set, both flags
    come back True. (mock_magnite_dv_api fixture seeds the env with the
    Elcano vendor ID, so the resolved label is "Elcano".)"""
    result = await magnite_auth_status()
    assert result == {
        "configured": True,
        "has_account_id": True,
        "account_id": "102",
        "vendor_label": "Elcano",
        "dv_base_url": "https://api.rubiconproject.com",
        "dmg_base_url": "https://dmg.rubiconproject.com",
    }


@pytest.mark.asyncio
async def test_reports_not_configured_when_keys_missing(reset_magnite_client):  # noqa: ARG001
    """Stripping the credentials flips configured to False and clears the
    account/vendor diagnostics."""
    with patch.dict(os.environ, {}, clear=True):
        result = await magnite_auth_status()
    assert result["configured"] is False
    assert result["has_account_id"] is False
    assert result["account_id"] == ""
    assert result["vendor_label"] is None


@pytest.mark.asyncio
async def test_account_id_missing_but_keys_present(reset_magnite_client):  # noqa: ARG001
    """Keys-only configuration is reported as configured=True but
    has_account_id=False — calls to DV+ reporting will fail until the
    trader sets MAGNITE_ACCOUNT_ID."""
    with patch.dict(
        os.environ,
        {
            "MAGNITE_ACCESS_KEY": "k",
            "MAGNITE_SECRET_KEY": "s",
            "MAGNITE_ACCOUNT_ID": "",
        },
        clear=True,
    ):
        result = await magnite_auth_status()
    assert result == {
        "configured": True,
        "has_account_id": False,
        "account_id": "",
        "vendor_label": None,
        "dv_base_url": "https://api.rubiconproject.com",
        "dmg_base_url": "https://dmg.rubiconproject.com",
    }


@pytest.mark.asyncio
async def test_vendor_label_resolves_for_known_vendor_ids(reset_magnite_client):  # noqa: ARG001
    """The three known mp-vendor IDs (TWC=27, Elcano=102, Raptive=216)
    should each resolve to the right human-readable label."""
    cases = [
        ("27", "The Weather Company"),
        ("102", "Elcano"),
        ("216", "Raptive"),
    ]
    for account_id, expected_label in cases:
        with patch.dict(
            os.environ,
            {
                "MAGNITE_ACCESS_KEY": "k",
                "MAGNITE_SECRET_KEY": "s",
                "MAGNITE_ACCOUNT_ID": account_id,
            },
            clear=True,
        ):
            # Reset the client between cases so it re-reads the env.
            import magnite_mcp

            magnite_mcp._magnite_client = None
            result = await magnite_auth_status()
        assert result["account_id"] == account_id, account_id
        assert result["vendor_label"] == expected_label, account_id


@pytest.mark.asyncio
async def test_vendor_label_is_null_for_unknown_vendor_id(reset_magnite_client):  # noqa: ARG001
    """An MAGNITE_ACCOUNT_ID that isn't in MAGNITE_VENDOR_LABELS reports
    has_account_id=True but vendor_label=None — the diagnostic is purely
    informational, never gates behaviour."""
    with patch.dict(
        os.environ,
        {
            "MAGNITE_ACCESS_KEY": "k",
            "MAGNITE_SECRET_KEY": "s",
            "MAGNITE_ACCOUNT_ID": "99999",
        },
        clear=True,
    ):
        result = await magnite_auth_status()
    assert result["has_account_id"] is True
    assert result["account_id"] == "99999"
    assert result["vendor_label"] is None


def test_magnite_vendor_labels_dict_contents():
    """Sanity check — the labels dict carries every vendor the operator
    expects to see from the Magnite /me endpoint."""
    from magnite_mcp import MAGNITE_VENDOR_LABELS

    assert MAGNITE_VENDOR_LABELS["27"] == "The Weather Company"
    assert MAGNITE_VENDOR_LABELS["102"] == "Elcano"
    assert MAGNITE_VENDOR_LABELS["216"] == "Raptive"
