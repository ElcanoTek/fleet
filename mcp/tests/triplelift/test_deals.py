import json
import os
import sys

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from triplelift_mcp import (
    POLITICAL_ADS_CATEGORY_ID,
    REGULATORY_POLICY_CONTROLLED_BINDING,
    _build_targeting_expression,
    _expression_has_binding,
    tl_create_deal,
    tl_get_avails,
    tl_get_deal,
    tl_list_buyers,
    tl_list_countries,
    tl_list_deals,
    tl_list_segments,
    tl_toggle_deal_status,
    tl_update_deal,
)

from .conftest import (
    TRIPLELIFT_AVAILS_ENDPOINT,
    TRIPLELIFT_BUYERS_ENDPOINT,
    TRIPLELIFT_COUNTRIES_ENDPOINT,
    TRIPLELIFT_DEAL_ENDPOINT,
    TRIPLELIFT_DEALS_ENDPOINT,
    TRIPLELIFT_SEGMENTS_ENDPOINT,
    TRIPLELIFT_STATUS_ENDPOINT,
    TRIPLELIFT_TOKEN_URL,
)
from .fixtures import (
    AUTH_SUCCESS_RESPONSE,
    AVAILS_RESPONSE,
    BUYERS_RESPONSE,
    COUNTRIES_RESPONSE,
    CREATE_DEAL_SUCCESS_RESPONSE,
    GET_DEAL_RESPONSE,
    LIST_DEALS_RESPONSE,
    SAMPLE_CREATE_DEAL_PAYLOAD,
    SEGMENTS_RESPONSE,
)


class TestDealTools:
    @pytest.mark.asyncio
    async def test_create_deal_success(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(
            return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
        )

        result = await tl_create_deal(member_id=12345, payload=SAMPLE_CREATE_DEAL_PAYLOAD)

        assert result["success"] is True
        assert result["deal"]["id"] == 1010

    @pytest.mark.asyncio
    async def test_create_deal_validation_error(self):
        result = await tl_create_deal(member_id=12345, payload={"name": "incomplete"})
        assert result["success"] is False
        assert "Missing required fields" in result["error"]

    @pytest.mark.asyncio
    async def test_create_deal_payload_not_mutated(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(side_effect=capture)

        payload = dict(SAMPLE_CREATE_DEAL_PAYLOAD)
        payload.pop("targetingExpression")
        payload["country_ids"] = [233]

        await tl_create_deal(member_id=12345, payload=payload)

        assert "targetingExpression" not in payload
        assert captured_request is not None
        sent_payload = json.loads(captured_request.content)
        assert sent_payload["targetingExpression"]["type"] == "AND"

    @pytest.mark.asyncio
    async def test_create_deal_requires_deal_type_id(self):
        payload = dict(SAMPLE_CREATE_DEAL_PAYLOAD)
        payload.pop("dealTypeId")
        result = await tl_create_deal(member_id=12345, payload=payload)
        assert result["success"] is False
        assert "dealTypeId" in result["error"]

    @pytest.mark.asyncio
    async def test_create_deal_auto_populates_validator_required_fields(self, mock_triplelift_api: respx.MockRouter):
        """Caller omits brandAndCreativeControls/externalCreativeTypeItems/vendor/curationFee;
        the tool should default them so TripleLift's validator accepts the payload."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(side_effect=capture)

        # SAMPLE_CREATE_DEAL_PAYLOAD intentionally omits the four auto-populated fields.
        await tl_create_deal(member_id=12345, payload=dict(SAMPLE_CREATE_DEAL_PAYLOAD))

        assert captured_request is not None
        sent = json.loads(captured_request.content)

        # brandAndCreativeControls — three INCLUDE blocks with empty items arrays
        bcc = sent["brandAndCreativeControls"]
        for key in ("iabCategoryTargeting", "brandTargeting", "advertiserDomainTargeting"):
            assert bcc[key] == {"action": "INCLUDE", "items": []}

        assert sent["externalCreativeTypeItems"] == []
        assert sent["vendor"] == {"cintLucidCampaignStudyId": ""}
        assert sent["curationFee"]["feeModel"] == {"id": 3, "type": "FEE_MODEL_TYPE_PERCENT"}
        assert sent["curationFee"]["value"] == 25
        assert sent["curationFee"]["cap"] is None

    @pytest.mark.asyncio
    async def test_create_deal_respects_caller_overrides(self, mock_triplelift_api: respx.MockRouter):
        """Caller-supplied values for the auto-populated fields must not be overwritten."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(side_effect=capture)

        payload = dict(SAMPLE_CREATE_DEAL_PAYLOAD)
        payload["externalCreativeTypeItems"] = [4]
        payload["vendor"] = {"cintLucidCampaignStudyId": "study-abc"}
        payload["curationFee"] = {
            "feeModel": {"id": 3, "type": "FEE_MODEL_TYPE_PERCENT"},
            "value": 12,
            "cap": 5,
        }

        await tl_create_deal(member_id=12345, payload=payload)

        assert captured_request is not None
        sent = json.loads(captured_request.content)
        assert sent["externalCreativeTypeItems"] == [4]
        assert sent["vendor"] == {"cintLucidCampaignStudyId": "study-abc"}
        assert sent["curationFee"]["value"] == 12
        assert sent["curationFee"]["cap"] == 5

    @pytest.mark.asyncio
    async def test_create_deal_allow_political_ads_only(self, mock_triplelift_api: respx.MockRouter):
        """allow_political_ads=True with no other targeting builds a targetingExpression
        containing only the regulatory-policy node, and the convenience flag is not sent."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(side_effect=capture)

        payload = dict(SAMPLE_CREATE_DEAL_PAYLOAD)
        payload.pop("targetingExpression")
        payload["allow_political_ads"] = True

        await tl_create_deal(member_id=12345, payload=payload)

        assert captured_request is not None
        sent = json.loads(captured_request.content)
        assert "allow_political_ads" not in sent
        expr = sent["targetingExpression"]
        assert expr["type"] == "AND"
        node = expr["children"][0]
        assert node["binding"] == REGULATORY_POLICY_CONTROLLED_BINDING
        assert node["integralTargets"] == [POLITICAL_ADS_CATEGORY_ID]
        assert node["excluded"] is False

    @pytest.mark.asyncio
    async def test_create_deal_allow_political_ads_composes_with_targeting(self, mock_triplelift_api: respx.MockRouter):
        """allow_political_ads appends the policy node to an existing AND-rooted tree
        (here built from the country_ids convenience key) without dropping it."""
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_DEAL_ENDPOINT).mock(side_effect=capture)

        payload = dict(SAMPLE_CREATE_DEAL_PAYLOAD)
        payload.pop("targetingExpression")
        payload["country_ids"] = [233]
        payload["allow_political_ads"] = True

        await tl_create_deal(member_id=12345, payload=payload)

        assert captured_request is not None
        sent = json.loads(captured_request.content)
        bindings = {child.get("binding") for child in sent["targetingExpression"]["children"]}
        assert "EB_SUPPLY_GEO_COUNTRY_ID" in bindings
        assert REGULATORY_POLICY_CONTROLLED_BINDING in bindings

    @pytest.mark.asyncio
    async def test_get_deal(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(f"{TRIPLELIFT_DEAL_ENDPOINT}/1001").mock(
            return_value=httpx.Response(200, json=GET_DEAL_RESPONSE)
        )

        result = await tl_get_deal(member_id=12345, deal_id=1001)
        assert result["success"] is True
        assert result["deal"]["id"] == 1001

    @pytest.mark.asyncio
    async def test_get_deal_exposes_targeting(self, mock_triplelift_api: respx.MockRouter):
        """tl_get_deal must surface the sibling `targeting` tree so the
        regulatory-policy / political-ads node is verifiable."""
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(f"{TRIPLELIFT_DEAL_ENDPOINT}/1001").mock(
            return_value=httpx.Response(200, json=GET_DEAL_RESPONSE)
        )

        result = await tl_get_deal(member_id=12345, deal_id=1001)
        assert result["success"] is True
        assert result["verified"] is True
        assert "targeting" in result
        assert _expression_has_binding(result["targeting"], REGULATORY_POLICY_CONTROLLED_BINDING)

    @pytest.mark.asyncio
    async def test_get_deal_warns_on_id_mismatch(self, mock_triplelift_api: respx.MockRouter):
        """TripleLift's GET-by-id can return a different deal; tl_get_deal must flag it."""
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        # Requested 9999 but the API returns the deal with id 1001.
        mock_triplelift_api.get(f"{TRIPLELIFT_DEAL_ENDPOINT}/9999").mock(
            return_value=httpx.Response(200, json=GET_DEAL_RESPONSE)
        )

        result = await tl_get_deal(member_id=12345, deal_id=9999)
        assert result["success"] is True
        assert result["verified"] is False
        assert "warning" in result
        assert "9999" in result["warning"]

    @pytest.mark.asyncio
    async def test_list_deals(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json=LIST_DEALS_RESPONSE)

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_DEALS_ENDPOINT).mock(side_effect=capture)

        result = await tl_list_deals(member_id=12345, query="Elcano", order_by="name", sort_dir="asc", deal_type_id=2)

        assert result["success"] is True
        assert len(result["deals"]) == 2
        assert captured_request is not None
        assert "query=Elcano" in str(captured_request.url)
        assert "orderBy=name" in str(captured_request.url)
        assert "sortDir=asc" in str(captured_request.url)
        assert "dealTypeId=2" in str(captured_request.url)

    @pytest.mark.asyncio
    async def test_update_deal(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.patch(f"{TRIPLELIFT_DEAL_ENDPOINT}/1001").mock(
            return_value=httpx.Response(200, json=GET_DEAL_RESPONSE)
        )

        result = await tl_update_deal(member_id=12345, deal_id=1001, payload={"active": False})
        assert result["success"] is True
        assert result["deal"]["id"] == 1001

    def test_delete_deal_not_exposed(self):
        """Deal deletion must NOT be reachable by the agent.

        Deletion is irreversible, so exposing it to an LLM agent was vetoed
        as a security decision (2026-06-11, Elyse — same policy as OpenX
        dealArchive). Pausing via tl_toggle_deal_status(active=False) is the
        supported way to stop delivery. This test pins the decision so a
        future change re-introducing a delete surface fails loudly.
        """
        import triplelift_mcp

        assert not hasattr(triplelift_mcp, "tl_delete_deal")
        assert not hasattr(triplelift_mcp.TripleLiftClient, "delete_deal")
        with open(triplelift_mcp.__file__) as f:
            source = f.read()
        # No DELETE call against the deal endpoint may exist in the module.
        assert '"DELETE"' not in source

    @pytest.mark.asyncio
    async def test_toggle_deal_status(self, mock_triplelift_api: respx.MockRouter):
        captured_request = None

        def capture(request: httpx.Request) -> httpx.Response:
            nonlocal captured_request
            captured_request = request
            return httpx.Response(200, json={"message": "updated"})

        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.patch(TRIPLELIFT_STATUS_ENDPOINT).mock(side_effect=capture)

        result = await tl_toggle_deal_status(member_id=12345, deal_id=1001, active=False)
        assert result["success"] is True
        assert captured_request is not None
        payload = json.loads(captured_request.content)
        assert payload == {"id": 1001, "active": False}


class TestDiscoveryTools:
    @pytest.mark.asyncio
    async def test_list_buyers(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_BUYERS_ENDPOINT).mock(return_value=httpx.Response(200, json=BUYERS_RESPONSE))

        result = await tl_list_buyers(member_id=12345)
        assert result["success"] is True
        assert len(result["buyers"]) == 2

    @pytest.mark.asyncio
    async def test_list_countries(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_COUNTRIES_ENDPOINT).mock(
            return_value=httpx.Response(200, json=COUNTRIES_RESPONSE)
        )

        result = await tl_list_countries(member_id=12345)
        assert result["success"] is True
        assert result["countries"][0]["name"] == "United States"

    @pytest.mark.asyncio
    async def test_list_segments(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.get(TRIPLELIFT_SEGMENTS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=SEGMENTS_RESPONSE)
        )

        result = await tl_list_segments(member_id=12345, with_description=True)
        assert result["success"] is True
        assert len(result["segments"]) == 2

    @pytest.mark.asyncio
    async def test_get_avails(self, mock_triplelift_api: respx.MockRouter):
        mock_triplelift_api.post(TRIPLELIFT_TOKEN_URL).mock(
            return_value=httpx.Response(200, json=AUTH_SUCCESS_RESPONSE)
        )
        mock_triplelift_api.post(TRIPLELIFT_AVAILS_ENDPOINT).mock(
            return_value=httpx.Response(200, json=AVAILS_RESPONSE)
        )

        result = await tl_get_avails(member_id=12345, channel="WEB")
        assert result["success"] is True
        assert result["avails_count"] == 123456


class TestTargetingExpressionBuilder:
    def test_builder_uses_engine_bindings(self):
        """Segment/device convenience keys must emit TripleLift's real engine bindings
        (EB_SUPPLY_1P_SEGMENT_ID / EB_SUPPLY_DEVICE_TYPE), not the older invalid names."""
        expr = _build_targeting_expression(
            country_ids=[233],
            device_types=["DESKTOP"],
            segment_ids=[991],
        )
        bindings = {child["binding"] for child in expr["children"]}
        assert bindings == {
            "EB_SUPPLY_GEO_COUNTRY_ID",
            "EB_SUPPLY_DEVICE_TYPE",
            "EB_SUPPLY_1P_SEGMENT_ID",
        }
        # Guard against regressing to the previously-wrong binding names.
        assert "EB_AUDIENCE_SEGMENT_ID" not in bindings
        assert "EB_DEVICE_TYPE" not in bindings
