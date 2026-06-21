"""Tests for generic OpenX IAB category discovery."""

import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_list_iab_categories

from .conftest import OPENX_GRAPHQL_ENDPOINT


class TestListIabCategories:
    @pytest.mark.asyncio
    async def test_lists_all_iab_categories(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "18",
                                "name": "Certified Pre-Owned Cars",
                                "path": "deal.package.targeting.domain.categories_iab_v2",
                                "extra": {},
                            },
                            {
                                "id": "25",
                                "name": "Car Culture",
                                "path": "deal.package.targeting.domain.categories_iab_v2",
                                "extra": {},
                            },
                        ]
                    }
                },
            )
        )

        result = await ox_list_iab_categories()

        assert result["success"] is True
        assert result["total_options"] == 2
        assert result["options"][0]["name"] == "Certified Pre-Owned Cars"

    @pytest.mark.asyncio
    async def test_filters_iab_categories_by_query(self, mock_openx_graphql: respx.MockRouter):
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            return_value=httpx.Response(
                200,
                json={
                    "data": {
                        "optionsByPath": [
                            {
                                "id": "18",
                                "name": "Certified Pre-Owned Cars",
                                "path": "deal.package.targeting.domain.categories_iab_v2",
                                "extra": {},
                            },
                            {
                                "id": "25",
                                "name": "Car Culture",
                                "path": "deal.package.targeting.domain.categories_iab_v2",
                                "extra": {},
                            },
                        ]
                    }
                },
            )
        )

        result = await ox_list_iab_categories("certified")

        assert result["success"] is True
        assert result["total_options"] == 1
        assert result["options"][0]["id"] == "18"
