"""Tests for Index Exchange targeting-related MCP tools."""

import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from indexexchange_mcp import (
    _normalize_targeting_for_create,
    ix_create_domain_targeting_values,
    ix_list_targeting_keys,
    ix_list_targeting_values,
)

from .conftest import IX_ACCOUNTS_ENDPOINT, IX_BASE_URL, IX_LOGIN_ENDPOINT
from .fixtures import LOGIN_SUCCESS_RESPONSE, TARGETING_KEYS_RESPONSE, TARGETING_VALUES_RESPONSE

IX_DOMAIN_TARGETING_VALUES_ENDPOINT = (
    f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups/targets/ext/domain/values"
)
IX_TARGETING_KEYS_ENDPOINT = f"{IX_BASE_URL}/api/supply-configuration/v1/inventory-groups/targets"


@pytest.fixture
def mock_targeting_api(mock_ix_api: respx.MockRouter) -> respx.MockRouter:
    mock_ix_api.post(IX_LOGIN_ENDPOINT).mock(return_value=httpx.Response(200, json=LOGIN_SUCCESS_RESPONSE))
    return mock_ix_api


class TestCreateDomainTargetingValues:
    @pytest.mark.asyncio
    async def test_batches_domains_into_200_and_aggregates_ids(self, mock_targeting_api: respx.MockRouter):
        domains = [f"site-{i}.example" for i in range(450)]
        seen_batches: list[list[str]] = []

        def capture_and_respond(request: httpx.Request) -> httpx.Response:
            body = json.loads(request.content.decode())
            batch_domains = body["domains"]
            seen_batches.append(batch_domains)
            ids = [{"valueID": f"id-{domain}"} for domain in batch_domains]
            return httpx.Response(200, json={"targetingKeyID": 120, "targetingValueIDs": ids})

        mock_targeting_api.put(IX_DOMAIN_TARGETING_VALUES_ENDPOINT).mock(side_effect=capture_and_respond)

        result = await ix_create_domain_targeting_values(domains=domains)

        assert result["success"] is True
        assert "partial_success" not in result
        assert len(seen_batches) == 3
        assert [len(batch) for batch in seen_batches] == [200, 200, 50]
        assert len(result["targeting_value_ids"]) == 450
        assert all(isinstance(value_id, str) for value_id in result["targeting_value_ids"])

    @pytest.mark.asyncio
    async def test_returns_403_1100_error_immediately(self, mock_targeting_api: respx.MockRouter):
        domains = [f"site-{i}.example" for i in range(401)]
        call_count = 0

        def respond_with_auth_failure_on_second_batch(request: httpx.Request) -> httpx.Response:
            nonlocal call_count
            call_count += 1
            if call_count == 2:
                return httpx.Response(403, json={"code": 1100, "message": "not authorized"})
            body = json.loads(request.content.decode())
            ids = [{"valueID": f"id-{domain}"} for domain in body["domains"]]
            return httpx.Response(200, json={"targetingKeyID": 120, "targetingValueIDs": ids})

        mock_targeting_api.put(IX_DOMAIN_TARGETING_VALUES_ENDPOINT).mock(
            side_effect=respond_with_auth_failure_on_second_batch
        )

        result = await ix_create_domain_targeting_values(domains=domains)

        assert result["success"] is False
        assert result["error"]["operation"] == "create_domain_targeting_values"
        assert "hint" in result
        assert call_count == 2

    @pytest.mark.asyncio
    async def test_continues_after_non_auth_failure_and_reports_partial_success(
        self, mock_targeting_api: respx.MockRouter
    ):
        domains = [f"site-{i}.example" for i in range(401)]
        call_count = 0

        def respond_with_server_error_on_second_batch(request: httpx.Request) -> httpx.Response:
            nonlocal call_count
            call_count += 1
            body = json.loads(request.content.decode())
            if call_count == 2:
                return httpx.Response(500, json={"message": "temporary failure"})
            ids = [{"valueID": f"id-{domain}"} for domain in body["domains"]]
            return httpx.Response(200, json={"targetingKeyID": 120, "targetingValueIDs": ids})

        mock_targeting_api.put(IX_DOMAIN_TARGETING_VALUES_ENDPOINT).mock(
            side_effect=respond_with_server_error_on_second_batch
        )

        result = await ix_create_domain_targeting_values(domains=domains)

        assert result["success"] is True
        assert result["partial_success"] is True
        assert call_count == 3
        assert len(result["targeting_value_ids"]) == 201
        assert len(result["failed_domains"]) == 200
        assert result["failed_domains"] == domains[200:400]

    @pytest.mark.asyncio
    async def test_domain_lookup_uses_legacy_marketplace_scope_when_account_id_provided(
        self, mock_targeting_api: respx.MockRouter
    ):
        account_request = None
        domain_request = None

        def capture_accounts(request: httpx.Request) -> httpx.Response:
            nonlocal account_request
            account_request = request
            return httpx.Response(
                200,
                json=[
                    {
                        "accountID": 1491166,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ],
            )

        def capture_domain_request(request: httpx.Request) -> httpx.Response:
            nonlocal domain_request
            domain_request = request
            return httpx.Response(
                200,
                json={"targetingKeyID": 120, "targetingValueIDs": [{"valueID": 999}]},
            )

        account_route = mock_targeting_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture_accounts)
        domain_route = mock_targeting_api.put(IX_DOMAIN_TARGETING_VALUES_ENDPOINT).mock(
            side_effect=capture_domain_request
        )

        result = await ix_create_domain_targeting_values(domains=["example.com"], account_id=1491166)

        assert result["success"] is True
        assert result["targeting_value_ids"] == ["999"]
        assert account_route.called
        assert domain_route.called
        assert account_request is not None
        assert domain_request is not None
        assert "accountIDs=1491166" in str(account_request.url)
        assert "publisherAccountID=209224" in str(domain_request.url)


class TestTargetingDiscoveryTools:
    @pytest.mark.asyncio
    async def test_list_targeting_keys_uses_legacy_marketplace_id(self, mock_targeting_api: respx.MockRouter):
        account_request = None
        targeting_request = None

        def capture_accounts(request: httpx.Request) -> httpx.Response:
            nonlocal account_request
            account_request = request
            return httpx.Response(
                200,
                json=[
                    {
                        "accountID": 12345,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ],
            )

        def capture_targeting(request: httpx.Request) -> httpx.Response:
            nonlocal targeting_request
            targeting_request = request
            return httpx.Response(200, json={"data": TARGETING_KEYS_RESPONSE})

        account_route = mock_targeting_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture_accounts)
        targeting_route = mock_targeting_api.get(IX_TARGETING_KEYS_ENDPOINT).mock(side_effect=capture_targeting)

        result = await ix_list_targeting_keys(account_id=12345)

        assert result["success"] is True
        assert result["targeting_keys"] == TARGETING_KEYS_RESPONSE
        assert account_route.called
        assert targeting_route.called
        assert account_request is not None
        assert targeting_request is not None
        assert "accountIDs=12345" in str(account_request.url)
        assert "publisherAccountID=209224" in str(targeting_request.url)


class _StubIndexExchangeClient:
    def __init__(self, responses: dict[str, object]):
        self._responses = responses
        self.calls: list[str] = []

    async def request(self, method: str, path: str, params: dict | None = None):
        del method, params
        self.calls.append(path)
        if path not in self._responses:
            raise AssertionError(f"Unexpected request path: {path}")
        return self._responses[path]


class TestNormalizeTargetingForCreate:
    @pytest.mark.asyncio
    async def test_accepts_numeric_app_bundle_ids_for_key_120(self):
        client = _StubIndexExchangeClient(
            {
                "/api/accounts/v2/accounts/": [
                    {
                        "accountID": 1491166,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ],
                "/api/supply-configuration/v1/inventory-groups/targets": {
                    "data": [{"targetingKeyID": 120, "key": "Domain and app bundle"}]
                },
            }
        )

        targeting = [
            {
                "targetingKeyID": 120,
                "keyName": "Domain and app bundle",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": "474301"}, {"value": "273862"}]}],
            }
        ]

        normalized_targeting, domain_stats = await _normalize_targeting_for_create(client, 1491166, targeting)

        assert normalized_targeting is not None
        values = normalized_targeting[0]["sets"][0]["values"]
        assert values == [{"value": "474301"}, {"value": "273862"}]
        assert domain_stats is not None
        assert domain_stats["invalid_count"] == 0
        assert "/api/supply-configuration/v1/inventory-groups/targets/120/values" not in client.calls

    @pytest.mark.asyncio
    async def test_skips_translation_for_numeric_segment_ids(self):
        client = _StubIndexExchangeClient(
            {
                "/api/accounts/v2/accounts/": [
                    {
                        "accountID": 1491166,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ],
                "/api/supply-configuration/v1/inventory-groups/targets": {
                    "data": [{"targetingKeyID": 777, "key": "Audience Segment"}]
                },
            }
        )

        targeting = [
            {
                "targetingKeyID": 777,
                "keyName": "Audience Segment",
                "targetingType": "standard",
                "sets": [{"operator": "ANY_OF", "values": [{"value": "308129"}]}],
            }
        ]

        normalized_targeting, domain_stats = await _normalize_targeting_for_create(client, 1491166, targeting)

        assert normalized_targeting is not None
        assert normalized_targeting[0]["sets"][0]["values"] == [{"value": "308129"}]
        assert domain_stats is not None
        assert "/api/supply-configuration/v1/inventory-groups/targets/777/values" not in client.calls


class TestTargetingDiscoveryToolsContinued:
    @pytest.mark.asyncio
    async def test_list_targeting_values_uses_values_query_parameter(self, mock_targeting_api: respx.MockRouter):
        key_id = 10
        endpoint = f"{IX_TARGETING_KEYS_ENDPOINT}/{key_id}/values"
        account_request = None
        targeting_request = None

        def capture_accounts(request: httpx.Request) -> httpx.Response:
            nonlocal account_request
            account_request = request
            return httpx.Response(
                200,
                json=[
                    {
                        "accountID": 777,
                        "accountType": "publisher",
                    }
                ],
            )

        def capture_targeting(request: httpx.Request) -> httpx.Response:
            nonlocal targeting_request
            targeting_request = request
            return httpx.Response(200, json={"data": TARGETING_VALUES_RESPONSE})

        account_route = mock_targeting_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture_accounts)
        targeting_route = mock_targeting_api.get(endpoint).mock(side_effect=capture_targeting)

        result = await ix_list_targeting_values(account_id=777, key_id=key_id, search="United")

        assert result["success"] is True
        assert result["targeting_values"] == TARGETING_VALUES_RESPONSE
        assert account_route.called
        assert targeting_route.called
        assert account_request is not None
        assert targeting_request is not None
        assert "accountIDs=777" in str(account_request.url)
        assert "publisherAccountID=777" in str(targeting_request.url)
        assert "values=United" in str(targeting_request.url)

    @pytest.mark.asyncio
    async def test_list_targeting_values_resolves_legacy_marketplace_id_from_accounts_envelope(
        self, mock_targeting_api: respx.MockRouter
    ):
        key_id = 8
        endpoint = f"{IX_TARGETING_KEYS_ENDPOINT}/{key_id}/values"
        account_request = None
        targeting_request = None

        def capture_accounts(request: httpx.Request) -> httpx.Response:
            nonlocal account_request
            account_request = request
            return httpx.Response(
                200,
                json={
                    "accounts": [
                        {
                            "accountID": 1491166,
                            "accountType": "marketplace",
                            "marketplace": {"legacyMarketplaceID": 209224},
                        }
                    ]
                },
            )

        def capture_targeting(request: httpx.Request) -> httpx.Response:
            nonlocal targeting_request
            targeting_request = request
            return httpx.Response(200, json={"data": TARGETING_VALUES_RESPONSE})

        account_route = mock_targeting_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture_accounts)
        targeting_route = mock_targeting_api.get(endpoint).mock(side_effect=capture_targeting)

        result = await ix_list_targeting_values(account_id=1491166, key_id=key_id, value="0.70")

        assert result["success"] is True
        assert result["targeting_values"] == TARGETING_VALUES_RESPONSE
        assert account_route.called
        assert targeting_route.called
        assert account_request is not None
        assert targeting_request is not None
        assert "accountIDs=1491166" in str(account_request.url)
        assert "publisherAccountID=209224" in str(targeting_request.url)
        assert "values=0.70" in str(targeting_request.url)

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        ("key_id", "lookup_value", "expected_value_id"),
        [
            (8, "0.70", 65),
            (8, "0.90", 67),
            (11, "Entertainment", 449),
            (9, "USA", 375),
        ],
    )
    async def test_marketplace_dynamic_lookup_examples(
        self,
        mock_targeting_api: respx.MockRouter,
        key_id: int,
        lookup_value: str,
        expected_value_id: int,
    ):
        endpoint = f"{IX_TARGETING_KEYS_ENDPOINT}/{key_id}/values"
        account_request = None
        targeting_request = None

        def capture_accounts(request: httpx.Request) -> httpx.Response:
            nonlocal account_request
            account_request = request
            return httpx.Response(
                200,
                json=[
                    {
                        "accountID": 1491166,
                        "accountType": "marketplace",
                        "marketplace": {"legacyMarketplaceID": 209224},
                    }
                ],
            )

        def capture_targeting(request: httpx.Request) -> httpx.Response:
            nonlocal targeting_request
            targeting_request = request
            return httpx.Response(
                200,
                json={"data": [{"valueID": expected_value_id, "value": lookup_value, "status": "active"}]},
            )

        account_route = mock_targeting_api.get(IX_ACCOUNTS_ENDPOINT).mock(side_effect=capture_accounts)
        targeting_route = mock_targeting_api.get(endpoint).mock(side_effect=capture_targeting)

        result = await ix_list_targeting_values(account_id=1491166, key_id=key_id, value=lookup_value)

        assert result["success"] is True
        assert result["targeting_values"][0]["valueID"] == expected_value_id
        assert account_route.called
        assert targeting_route.called
        assert account_request is not None
        assert targeting_request is not None
        assert "accountIDs=1491166" in str(account_request.url)
        assert "publisherAccountID=209224" in str(targeting_request.url)
        assert f"values={lookup_value}" in str(targeting_request.url)
