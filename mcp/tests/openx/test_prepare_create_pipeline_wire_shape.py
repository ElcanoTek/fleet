"""Regression tests for the OpenX prepare → create pipeline wire shape.

Two bugs were caught by the trader's 9-deal smoke run on 2026-05-20:

1. ``third_party_fees_config`` injected response-only fields
   ``platform_share`` and ``platform_partner_id`` into the create
   payload. Those fields exist in OpenX's ``dealCreate`` *response* but
   ``ThirdPartyFeesConfigCreateParams`` (the input type, verified via
   GraphQL introspection) only accepts ``revenue_method``,
   ``gross_share``, ``gross_cpm_cap``, ``partner_id``. Every
   ``ox_create_prepared_deal`` rejected with
   ``Field "platform_share" is not defined by type ...``.

2. ``audience.openaudience_custom.includes: [<segment_name>]`` — the
   brief-shape audience dict — flowed through ``ox_prepare_deal_from_brief``
   unchanged. The wire type ``TargetingOpenaudienceCustomCreateParams``
   requires ``{op: INTERSECTS, val: <audience_id>}``. The audience
   resolver only ran when ``audience_segments`` came in as a top-level
   field; the dict-shape (which is the natural shape the multi-deal
   protocol emits) bypassed resolution entirely and OpenX rejected the
   create with two errors:
       Field "op" of required type "String!" was not provided.
       Field "includes" is not defined by type
         "TargetingOpenaudienceCustomCreateParams".

Both bugs forced the trader to abandon the prepare/create flow on the
live run and hand-construct deals via ``ox_create_deal``. The tests
below pin the fixes so the pipeline stays usable end-to-end.
"""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import _normalize_third_party_fees_config, ox_create_deal, ox_prepare_deal_from_brief

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import CREATE_DEAL_SUCCESS_RESPONSE, INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE


def _create_mock_handler(captured_requests: list | None = None):
    """Local copy of the handler factory in test_create_deal.py — keeps
    this test module standalone so it doesn't depend on cross-module imports
    that ruff/pyright would warn about."""

    def handler(request: httpx.Request) -> httpx.Response:
        payload = json.loads(request.content)
        query = payload.get("query", "")
        if "__type" in query or "IntrospectType" in query:
            return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
        if captured_requests is not None:
            captured_requests.append(request)
        return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

    return handler


# ---------------------------------------------------------------------------
# Bug 1 — platform_share / platform_partner_id stripped from wire payload
# ---------------------------------------------------------------------------


class TestPlatformShareNotInWirePayload:
    def test_normalizer_strips_response_only_fields(self):
        """Direct test of the normalizer — the input has the response-only
        fields, the output must not."""
        fee_input = {
            "partner_id": "560610563",
            "revenue_method": "PoM",
            "gross_share": "0.4",
            "platform_share": "0.100",
            "platform_partner_id": "540278980",
        }
        normalized = _normalize_third_party_fees_config(fee_input)
        assert normalized is not None
        assert len(normalized) == 1
        fee = normalized[0]
        assert "platform_share" not in fee
        assert "platform_partner_id" not in fee
        # Supported fields survive the normalization unchanged.
        assert fee == {
            "partner_id": "560610563",
            "revenue_method": "PoM",
            "gross_share": "0.4",
        }

    def test_normalizer_handles_list_input(self):
        """The normalizer accepts a list of fee configs; both shape and
        per-entry stripping must work."""
        fee_input = [
            {
                "partner_id": "p1",
                "revenue_method": "PoM",
                "gross_share": "0.4",
                "platform_share": "0.10",
            },
            {
                "partner_id": "p2",
                "revenue_method": "PoM",
                "gross_share": "0.3",
                "platform_partner_id": "540278980",
            },
        ]
        normalized = _normalize_third_party_fees_config(fee_input)
        assert normalized is not None
        assert all("platform_share" not in fee for fee in normalized)
        assert all("platform_partner_id" not in fee for fee in normalized)

    def test_normalizer_passthrough_when_clean(self):
        """A clean fee dict (no response-only fields) is unaffected."""
        fee_input = {
            "partner_id": "p1",
            "revenue_method": "PoM",
            "gross_share": "0.4",
        }
        normalized = _normalize_third_party_fees_config(fee_input)
        assert normalized == [fee_input]

    @pytest.mark.asyncio
    async def test_create_deal_strips_response_only_fields_from_wire(self, mock_openx_graphql: respx.MockRouter):
        """End-to-end through ``ox_create_deal``: response-only fields
        injected by the caller never reach OpenX."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=_create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Test_Deal",
            currency="USD",
            deal_price=0.10,
            start_date="2026-06-01T00:00:00Z",
            deal_participants=[{"demand_partner": "Beeswax - RTB"}],
            targeting={},
            third_party_fees_config={
                "partner_id": "560610563",
                "revenue_method": "PoM",
                "gross_share": 40.0,
                # Response-only — must be stripped before send.
                "platform_share": "0.100",
                "platform_partner_id": "540278980",
            },
        )

        payload = json.loads(captured_requests[0].content)
        fee = payload["variables"]["input"]["third_party_fees_config"][0]
        # Both response-only fields are gone.
        assert "platform_share" not in fee
        assert "platform_partner_id" not in fee
        # The supported fields are present and string-serialized.
        assert fee["partner_id"] == "560610563"
        assert fee["revenue_method"] == "PoM"
        assert fee["gross_share"] == "40.0"


# ---------------------------------------------------------------------------
# App-bundle targeting reaches the wire as its own app_inventory dimension
# ---------------------------------------------------------------------------


class TestAppInventoryReachesWirePayload:
    """App bundles must travel as ``package.targeting.app_inventory.app_bundle_id``
    (a distinct OpenX dimension), NOT folded into ``url_targeting``. Verified
    against a UI-created Reklaim GM Display deal. Pin the pass-through so a future
    ox_create_deal targeting refactor can't silently drop it."""

    @pytest.mark.asyncio
    async def test_app_inventory_survives_into_dealcreate_payload(self, mock_openx_graphql: respx.MockRouter):
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=_create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="App_Inventory_Deal",
            currency="USD",
            deal_price=0.10,
            start_date="2026-06-01T00:00:00Z",
            deal_participants=[{"demand_partner": "Beeswax - RTB"}],
            targeting={
                "channel": "DISPLAY",
                "app_inventory": {
                    "app_bundle_id": {
                        "op": "OR",
                        "val": [
                            {"op": "==", "val": "com.fubotv.vix"},
                            {"op": "==", "val": "B072QYQ43R"},
                        ],
                    },
                    "app_inventory_inter_dimension_operator": "AND",
                },
            },
        )

        payload = json.loads(captured_requests[0].content)
        wire_targeting = payload["variables"]["input"]["package"]["targeting"]
        assert wire_targeting["app_inventory"]["app_bundle_id"]["val"] == [
            {"op": "==", "val": "com.fubotv.vix"},
            {"op": "==", "val": "B072QYQ43R"},
        ]
        # Bundles must NOT leak into url_targeting.
        assert "url_targeting" not in payload["variables"]["input"]["package"]


# ---------------------------------------------------------------------------
# Bug 2 — audience brief-shape resolves and emits wire shape
# ---------------------------------------------------------------------------


class TestAudienceBriefShapeResolution:
    """The multi-deal protocol emits audience targeting as
    ``{"openaudience_custom": {"includes": [<segment_name>, ...]}}``. The
    prepare flow must extract the segment names, resolve them to OpenX
    audience IDs, and emit the wire shape
    ``{"openaudience_custom": {"op": "INTERSECTS", "val": <audience_id>}}``
    in the resulting ``create_args``.
    """

    @staticmethod
    def _make_resolver_mock(
        mock_openx_graphql: respx.MockRouter,
        audience_id: str = "openaudience-30fcdbfe-347a-4c24-a1bd-9884e993d432",
        audience_name: str = "Cars & Auto_Chrysler Enthusiasts",
        captured: list[httpx.Request] | None = None,
    ) -> respx.MockRouter:
        """Stub the OpenX GraphQL endpoint so:
        - audience optionsByPath returns one match for the segment name
        - demand-partner optionsByPath returns a match for Beeswax
        - fee-partner optionsByPath returns the Elcano partner record
        - introspection / IAB / etc. return enough to clear blockers
        - dealCreate returns a success envelope
        """

        def handler(request: httpx.Request) -> httpx.Response:
            if captured is not None:
                captured.append(request)
            body = json.loads(request.content)
            query = body.get("query", "")
            variables = body.get("variables", {})
            path = variables.get("path", "")
            if "optionsByPath" in query:
                if path == "deal.package.targeting.audience.openaudience_custom":
                    return httpx.Response(
                        200,
                        json={
                            "data": {
                                "optionsByPath": [
                                    {
                                        "id": audience_id,
                                        "name": audience_name,
                                        "extra": {"export_type": "PMP_US_ONLY"},
                                    }
                                ]
                            }
                        },
                    )
                if path == "deal.deal_participants.demand_partner":
                    return httpx.Response(
                        200,
                        json={"data": {"optionsByPath": [{"id": "537125689", "name": "Beeswax - RTB", "extra": {}}]}},
                    )
                if path == "deal.third_party_fees_config.partner_id":
                    return httpx.Response(
                        200,
                        json={
                            "data": {
                                "optionsByPath": [
                                    {
                                        "id": "560610563",
                                        "name": "Elcano",
                                        "extra": {"default_platform_share_partner_sourced": "0.100"},
                                    }
                                ]
                            }
                        },
                    )
                return httpx.Response(200, json={"data": {"optionsByPath": []}})
            return httpx.Response(200, json={"data": {}})

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=handler)
        return mock_openx_graphql

    @pytest.mark.asyncio
    async def test_includes_shape_extracted_and_resolved(self, mock_openx_graphql: respx.MockRouter):
        """The brief-shape ``audience.openaudience_custom.includes`` must
        be detected, the segment name resolved, and the create_args
        payload must hold the wire-shape ``{op, val}`` block."""
        self._make_resolver_mock(mock_openx_graphql)

        result = await ox_prepare_deal_from_brief(
            name="OpenX_Audience_BriefShape_Display",
            currency="USD",
            deal_price=0.10,
            start_date="2026-06-01",
            end_date="2026-12-31",
            demand_partner="Beeswax - RTB",
            pmp_deal_type="3",
            fee={"partner_name_or_id": "Elcano", "revenue_method": "PoM", "gross_share_percent": 40},
            targeting={
                "channel": "DISPLAY",
                "geographic": {"includes": {"country": "us"}},
                # The headline-bug shape — the brief naturally emits this
                # because it mirrors how a trader writes audience targeting
                # ("include these segments"). The MCP must convert.
                "audience": {"openaudience_custom": {"includes": ["Cars & Auto_Chrysler Enthusiasts"]}},
            },
        )

        assert result["success"] is True, result
        create_args = result["create_args_preview"]
        audience = create_args["targeting"]["audience"]["openaudience_custom"]
        # Wire-shape requirements from TargetingOpenaudienceCustomCreateParams.
        assert audience == {
            "op": "INTERSECTS",
            "val": "openaudience-30fcdbfe-347a-4c24-a1bd-9884e993d432",
        }
        # The brief-shape `includes` key must NOT survive — it's invalid
        # at the wire level and OpenX rejects it.
        assert "includes" not in audience

    @pytest.mark.asyncio
    async def test_create_args_wire_shape_omits_platform_share(self, mock_openx_graphql: respx.MockRouter):
        """Companion check that the create_args's third_party_fees_config
        no longer carries the response-only fields the prepare flow used
        to inject from the fee partner record."""
        self._make_resolver_mock(mock_openx_graphql)

        result = await ox_prepare_deal_from_brief(
            name="OpenX_Fee_Wire_Shape",
            currency="USD",
            deal_price=0.10,
            start_date="2026-06-01",
            end_date="2026-12-31",
            demand_partner="Beeswax - RTB",
            pmp_deal_type="3",
            fee={"partner_name_or_id": "Elcano", "revenue_method": "PoM", "gross_share_percent": 40},
            targeting={
                "channel": "DISPLAY",
                "geographic": {"includes": {"country": "us"}},
            },
        )

        assert result["success"] is True, result
        fee_config = result["create_args_preview"]["third_party_fees_config"]
        assert "platform_share" not in fee_config
        assert "platform_partner_id" not in fee_config
        # Supported fields stay.
        assert fee_config["partner_id"] == "560610563"
        assert fee_config["revenue_method"] == "PoM"
        assert fee_config["gross_share"] == "0.4"
