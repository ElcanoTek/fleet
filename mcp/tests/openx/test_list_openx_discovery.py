"""Tests for agent-friendly OpenX discovery tools."""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_list_audience_segments, ox_list_fee_partners, ox_list_states

from .conftest import OPENX_GRAPHQL_ENDPOINT


class TestListFeePartners:
    @pytest.mark.asyncio
    async def test_lists_fee_partners(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "560610563",
                                "name": "Elcano (fka Hyphatec)",
                                "path": "deal.third_party_fees_config.partner_id",
                                "extra": {},
                            },
                            {
                                "id": "1",
                                "name": "Another Partner",
                                "path": "deal.third_party_fees_config.partner_id",
                                "extra": {},
                            },
                        ]
                    }
                },
            )
        )

        result = await ox_list_fee_partners("elcano")

        assert result["success"] is True
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "560610563"


class TestListAudienceSegments:
    @pytest.mark.asyncio
    async def test_lists_audience_segments(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "openaudience-123",
                                "name": "Cars & Auto_Chrysler Enthusiasts",
                                "path": "deal.package.targeting.audience.openaudience_custom",
                                "extra": {},
                            }
                        ]
                    }
                },
            )
        )

        result = await ox_list_audience_segments("chrysler")

        assert result["success"] is True
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "openaudience-123"


class TestListStates:
    @pytest.mark.asyncio
    async def test_lists_states_from_openx(self, openx_api_key: str):  # noqa: ARG002
        result = await ox_list_states("HI")

        assert result["success"] is True
        assert result["source"] == "verified_table"
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "3595"
        assert result["options"][0]["extra"]["country"] == "united states"

    @pytest.mark.asyncio
    async def test_lists_states_via_optionsbypath_when_not_verified(self, mock_openx_graphql: respx.MockRouter):
        def handler(request: httpx.Request) -> httpx.Response:
            import json

            payload = json.loads(request.content)
            assert payload.get("variables", {}).get("path") == "deal.package.targeting.geographic.state"
            assert payload.get("variables", {}).get("filter") == {"state": "utah*"}
            return httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "3628",
                                "name": "utah",
                                "path": "deal.package.targeting.geographic.state",
                                "extra": {
                                    "country": "united states",
                                    "state": "utah",
                                    "type": "state",
                                    "type-id": "state-3628",
                                    "type_id": "state-3628",
                                },
                            }
                        ]
                    }
                },
            )

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=handler)

        result = await ox_list_states("UT")

        assert result["success"] is True
        assert result["source"] == "optionsByPath"
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "3628"

    @pytest.mark.asyncio
    async def test_lists_canadian_province_via_optionsbypath(self, mock_openx_graphql: respx.MockRouter):
        def handler(request: httpx.Request) -> httpx.Response:
            import json

            payload = json.loads(request.content)
            assert payload.get("variables", {}).get("path") == "deal.package.targeting.geographic.state"
            assert payload.get("variables", {}).get("filter") == {"state": "alberta*", "country": "canada*"}
            return httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "550",
                                "name": "alberta",
                                "path": "deal.package.targeting.geographic.state",
                                "extra": {
                                    "country": "canada",
                                    "state": "alberta",
                                    "type": "state",
                                    "type-id": "state-550",
                                    "type_id": "state-550",
                                },
                            }
                        ]
                    }
                },
            )

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=handler)

        result = await ox_list_states("Alberta", country="ca")

        assert result["success"] is True
        assert result["source"] == "optionsByPath"
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "550"
        assert result["options"][0]["extra"]["country"] == "canada"

    @pytest.mark.asyncio
    async def test_lists_states_filters_to_us_only(self, openx_api_key: str):  # noqa: ARG002
        result = await ox_list_states("CA")

        assert result["success"] is True
        assert result["source"] == "verified_table"
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "3588"

    @pytest.mark.asyncio
    async def test_list_states_requires_query(self):
        result = await ox_list_states()

        assert result["success"] is False
        assert "requires a non-empty query" in result["error"]
