"""Prepared-deal artifacts must be consume-once.

Regression tests for the duplicate-deal-creation hazard: after a successful
create POST, the prepared artifact used to stay in the server-side store with
``ready_to_create: True``, so an agent retrying the same ``prepared_deal_id``
(e.g. after a post-create verification error returned ``success: False``)
would re-run the create POST and stand up a duplicate live deal.

Covers pm_create_prepared_deal, ox_create_prepared_deal, and
mn_create_prepared_deal.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import medianet_mcp
import openx_mcp
import pubmatic_mcp


class TestPubMaticConsumeOnce:
    def _seed_artifact(self, prepared_deal_id: str) -> None:
        pubmatic_mcp._prepared_pubmatic_deals[prepared_deal_id] = {
            "prepared_deal_id": prepared_deal_id,
            "ready_to_create": True,
            "blocking_issues": [],
            "blockers": [],
            "warnings": [],
            "quality_flags": [],
            "resolved_entities": {},
            "targeting_intent": None,
            "deal_intent": {"name": "Consume Once Deal", "dealId": "PM-CONSUME-1"},
            "logged_in_owner_type_id": 5,
        }

    @pytest.mark.asyncio
    async def test_second_submit_replays_recorded_result(self, monkeypatch: pytest.MonkeyPatch):
        create_calls = []

        async def fake_create_curated_deal(payload):
            create_calls.append(payload)
            return {"success": True, "result": {"id": 555, "name": "Consume Once Deal"}}

        async def fake_get_curated_deal(**kwargs):
            return {"success": True, "result": {"id": 555}}

        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", fake_create_curated_deal)
        monkeypatch.setattr(pubmatic_mcp, "pm_get_curated_deal", fake_get_curated_deal)

        prepared_deal_id = "pubmatic-prepared-consume-once"
        self._seed_artifact(prepared_deal_id)
        try:
            first = await pubmatic_mcp.pm_create_prepared_deal(prepared_deal_id)
            second = await pubmatic_mcp.pm_create_prepared_deal(prepared_deal_id)
        finally:
            pubmatic_mcp._prepared_pubmatic_deals.pop(prepared_deal_id, None)

        assert first["success"] is True
        assert len(create_calls) == 1, "create POST must run exactly once"
        assert second.get("replayed") is True
        assert second["success"] is True

    @pytest.mark.asyncio
    async def test_verification_failure_after_create_does_not_allow_resubmit(self, monkeypatch: pytest.MonkeyPatch):
        """The exact failure shape from the audit: the deal is created, then the
        verification re-fetch raises — the response says success=False, which is
        precisely what prompts an agent to retry. The retry must NOT re-create."""
        create_calls = []

        async def fake_create_curated_deal(payload):
            create_calls.append(payload)
            return {"success": True, "result": {"id": 556, "name": "Consume Once Deal"}}

        async def exploding_get_curated_deal(**kwargs):
            raise ValueError("verification blew up after the deal was created")

        monkeypatch.setattr(pubmatic_mcp, "pm_create_curated_deal", fake_create_curated_deal)
        monkeypatch.setattr(pubmatic_mcp, "pm_get_curated_deal", exploding_get_curated_deal)

        prepared_deal_id = "pubmatic-prepared-verify-explodes"
        self._seed_artifact(prepared_deal_id)
        try:
            first = await pubmatic_mcp.pm_create_prepared_deal(prepared_deal_id)
            second = await pubmatic_mcp.pm_create_prepared_deal(prepared_deal_id)
        finally:
            pubmatic_mcp._prepared_pubmatic_deals.pop(prepared_deal_id, None)

        assert first["success"] is False
        flag_codes = [flag["flag"] for flag in first["quality_flags"]]
        assert "pm_deal_already_created" in flag_codes, "a post-create failure must tell the agent the deal exists"
        assert len(create_calls) == 1, "retry after post-create failure must not re-run the create POST"
        assert second.get("replayed") is True


class TestOpenXConsumeOnce:
    @pytest.mark.asyncio
    async def test_second_submit_replays_recorded_result(self, monkeypatch: pytest.MonkeyPatch):
        create_calls = []

        async def fake_ox_create_deal(**kwargs):
            create_calls.append(kwargs)
            return {"success": True, "deal": {"deal_id": "OX-1", "name": kwargs.get("name")}}

        monkeypatch.setattr(openx_mcp, "ox_create_deal", fake_ox_create_deal)

        prepared_deal_id = "openx-prepared-consume-once"
        openx_mcp._prepared_openx_deals[prepared_deal_id] = {
            "prepared_deal_id": prepared_deal_id,
            "ready_to_create": True,
            "blocking_issues": [],
            "blockers": [],
            "warnings": [],
            "quality_flags": [],
            "create_args": {"name": "Consume Once Deal", "currency": "USD"},
        }
        try:
            first = await openx_mcp.ox_create_prepared_deal(prepared_deal_id)
            second = await openx_mcp.ox_create_prepared_deal(prepared_deal_id)
        finally:
            openx_mcp._prepared_openx_deals.pop(prepared_deal_id, None)

        assert first["success"] is True
        assert len(create_calls) == 1, "create call must run exactly once"
        assert second.get("replayed") is True
        assert second["success"] is True

    @pytest.mark.asyncio
    async def test_failed_create_can_be_retried(self, monkeypatch: pytest.MonkeyPatch):
        """A create that never succeeded is NOT consumed — the agent may fix
        transient failures and retry the same artifact."""
        attempts = []

        async def flaky_ox_create_deal(**kwargs):
            attempts.append(kwargs)
            if len(attempts) == 1:
                return {"success": False, "error": "GraphQL HTTP 503: upstream flake"}
            return {"success": True, "deal": {"deal_id": "OX-2"}}

        monkeypatch.setattr(openx_mcp, "ox_create_deal", flaky_ox_create_deal)

        prepared_deal_id = "openx-prepared-retryable"
        openx_mcp._prepared_openx_deals[prepared_deal_id] = {
            "prepared_deal_id": prepared_deal_id,
            "ready_to_create": True,
            "blocking_issues": [],
            "blockers": [],
            "warnings": [],
            "quality_flags": [],
            "create_args": {"name": "Retryable Deal"},
        }
        try:
            first = await openx_mcp.ox_create_prepared_deal(prepared_deal_id)
            second = await openx_mcp.ox_create_prepared_deal(prepared_deal_id)
        finally:
            openx_mcp._prepared_openx_deals.pop(prepared_deal_id, None)

        assert first["success"] is False
        assert second["success"] is True
        assert len(attempts) == 2


class TestMediaNetConsumeOnce:
    @pytest.mark.asyncio
    async def test_second_submit_replays_recorded_result(self, monkeypatch: pytest.MonkeyPatch):
        create_calls = []

        class FakeClient:
            async def create_deal(self, payload):
                create_calls.append(payload)
                return {"deal_id": "MN-1", "display_name": "Consume Once Deal"}

        async def fake_get_deal(deal_id):
            return {"success": True, "deal": {"deal_id": deal_id}}

        monkeypatch.setattr(medianet_mcp, "get_medianet_client", lambda: FakeClient())
        monkeypatch.setattr(medianet_mcp, "mn_get_deal", fake_get_deal)

        prepared_deal_id = "medianet-prepared-consume-once"
        medianet_mcp._prepared_medianet_deals[prepared_deal_id] = {
            "prepared_deal_id": prepared_deal_id,
            "ready_to_create": True,
            "blocking_issues": [],
            "blockers": [],
            "warnings": [],
            "quality_flags": [],
            "deal_intent": {"deal_id": "MN-1", "display_name": "Consume Once Deal"},
        }
        try:
            first = await medianet_mcp.mn_create_prepared_deal(prepared_deal_id)
            second = await medianet_mcp.mn_create_prepared_deal(prepared_deal_id)
        finally:
            medianet_mcp._prepared_medianet_deals.pop(prepared_deal_id, None)

        assert first["success"] is True
        assert len(create_calls) == 1, "create POST must run exactly once"
        assert second.get("replayed") is True
        assert second["success"] is True
