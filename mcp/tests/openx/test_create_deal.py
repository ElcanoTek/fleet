"""
Tests for the create_deal MCP tool.

This is the HIGHEST PRIORITY test file. It validates:
- Exact GraphQL mutation payload structure
- Targeting, domain allowlists, fees, geo, device fields
- Returned MCP output structure
- GraphQL error handling
- Invalid input validation
"""

import json
import os
import sys
from unittest.mock import patch

import httpx
import pytest
import respx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from openx_mcp import ox_create_deal

from .conftest import OPENX_GRAPHQL_ENDPOINT
from .fixtures import (
    CREATE_DEAL_SUCCESS_RESPONSE,
    GRAPHQL_ERROR_RESPONSE,
    GRAPHQL_MULTIPLE_ERRORS,
    INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE,
)


def create_mock_handler(
    captured_requests: list | None = None,
    success_response: dict = CREATE_DEAL_SUCCESS_RESPONSE,
    introspection_response: dict = INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE,
):
    """
    Create a mock handler that responds to both introspection and mutation requests.

    Args:
        captured_requests: Optional list to capture mutation requests (introspection excluded)
        success_response: Response for mutation requests
        introspection_response: Response for introspection requests

    Returns:
        A function that handles httpx requests
    """

    def handler(request: httpx.Request) -> httpx.Response:
        payload = json.loads(request.content)
        query = payload.get("query", "")

        # Check if this is an introspection query
        if "__type" in query or "IntrospectType" in query:
            return httpx.Response(200, json=introspection_response)

        # Otherwise it's a mutation - capture it if requested
        if captured_requests is not None:
            captured_requests.append(request)

        return httpx.Response(200, json=success_response)

    return handler


class TestCreateDealSuccess:
    """Tests for successful deal creation scenarios."""

    @pytest.mark.asyncio
    async def test_create_deal_success(self, mock_openx_graphql: respx.MockRouter):
        """Test successful deal creation returns expected structure."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=create_mock_handler())

        result = await ox_create_deal(
            name="Elcano_OpenX_Test_Deal",
            currency="USD",
            deal_price=7.50,
            start_date="2024-03-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={"geographic": ["US"]},
        )

        assert result["success"] is True
        assert "deal" in result
        assert result["deal"]["deal_id"] == "ELC-2024-NEW-001"
        assert result["deal"]["name"] == "Elcano_OpenX_Test_Deal"
        assert "deal_url" in result
        assert result["deal_url"] == "https://select.openx.com/deals/deal-new-001/details"

    @pytest.mark.asyncio
    async def test_create_deal_returns_configured_deal_url(self, mock_openx_graphql: respx.MockRouter):
        """Create responses should expose a clickable deal URL when configured."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=create_mock_handler())

        with patch.dict(
            os.environ,
            {"OPENX_DEAL_URL_TEMPLATE": "https://ops.example.com/openx/deals/{id}?deal_id={deal_id}"},
            clear=False,
        ):
            result = await ox_create_deal(
                name="Elcano_OpenX_Test_Deal",
                currency="USD",
                deal_price=7.50,
                start_date="2024-03-01T00:00:00Z",
                deal_participants=[{"demand_partner_id": "TTD"}],
                package_name="Test_Package",
                targeting={"geographic": ["US"]},
            )

        assert result["success"] is True
        assert result["deal_url"] == "https://ops.example.com/openx/deals/deal-new-001?deal_id=ELC-2024-NEW-001"

    @pytest.mark.asyncio
    async def test_returned_mcp_output_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify the MCP tool output has correct structure."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=create_mock_handler())

        result = await ox_create_deal(
            name="Test_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "DV360"}],
            package_name="Package",
            targeting={},
        )

        # Verify top-level structure
        assert "success" in result
        assert result["success"] is True
        assert "deal" in result

        # Verify deal fields from response
        deal = result["deal"]
        assert "id" in deal
        assert "deal_id" in deal
        assert "name" in deal
        assert "status" in deal
        assert "currency" in deal
        assert "deal_price" in deal


class TestCreateDealPayloadStructure:
    """Tests verifying the exact GraphQL mutation payload."""

    @pytest.mark.asyncio
    async def test_mutation_payload_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify exact GraphQL mutation and DealCreateParams structure."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0",
            currency="USD",
            deal_price=5.50,
            start_date="2024-01-15T00:00:00Z",
            deal_participants=[{"demand_partner_id": "CRIMTAN", "buyer_ids": ["buyer-123"]}],
            package_name="US_CuratedDomains_Package",
            targeting={"geographic": ["US"], "device_type": ["DESKTOP", "MOBILE"]},
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)

        # Verify mutation query structure
        query = payload["query"]
        assert "mutation" in query
        assert "CreateDeal" in query or "dealCreate" in query
        assert "DealCreateParams" in query

        # Verify variables structure
        assert "variables" in payload
        assert "input" in payload["variables"]

        input_params = payload["variables"]["input"]

        # Core fields - deal_price must be a string with 2 decimal places
        assert input_params["name"] == "Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0"
        assert input_params["currency"] == "USD"
        assert input_params["deal_price"] == "5.50"
        assert isinstance(input_params["deal_price"], str)
        assert input_params["start_date"] == "2024-01-15T00:00:00Z"

        # Deal participants
        assert "deal_participants" in input_params
        assert len(input_params["deal_participants"]) == 1

        # Package with targeting
        assert "package" in input_params
        assert input_params["package"]["name"] == "US_CuratedDomains_Package"
        assert "targeting" in input_params["package"]

    @pytest.mark.asyncio
    async def test_targeting_fields(self, mock_openx_graphql: respx.MockRouter):
        """Verify geographic and device_type targeting in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        targeting = {
            "geographic": ["US", "CA", "UK"],
            "device_type": ["CTV", "DESKTOP", "MOBILE", "TABLET"],
        }

        await ox_create_deal(
            name="Targeting_Test_Deal",
            currency="USD",
            deal_price=10.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Targeting_Package",
            targeting=targeting,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        package_targeting = input_params["package"]["targeting"]

        # Verify inter_dimension_operator
        assert package_targeting["inter_dimension_operator"] == "AND"

        # Verify geographic targeting uses comma-delimited string format
        # Multiple countries: {"includes": {"country": "US,CA,UK"}}
        expected_geo = {
            "includes": {"country": "US,CA,UK"},
        }
        assert package_targeting["geographic"] == expected_geo

        # Verify device type targeting is normalized into required rendering_context V2 fields
        assert package_targeting["rendering_context"] == {
            "op": "AND",
            "ad_placement": {"op": "==", "val": "BANNER"},
            "distribution_channel": {"op": "INTERSECTS", "val": "WEB,APP"},
            "device_type": {
                "op": "INTERSECTS",
                "desktop_devices": "desktop",
                "mobile_devices": "phone,tablet",
                "tv_devices": "tv",
            },
        }
        assert package_targeting["metacategory"] == {
            "excludes": [],
            "includes": None,
            "keywords": None,
            "exclude_mfa": True,
            "inter_dimension_operator": "AND",
        }

    @pytest.mark.asyncio
    async def test_channel_based_rendering_context_defaults(self, mock_openx_graphql: respx.MockRouter):
        """Verify the tool auto-builds rendering_context defaults from channel."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="CTV_Rendering_Context_Test",
            currency="USD",
            deal_price=10.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="CTV_Package",
            targeting={"channel": "CTV", "device_type": ["CTV", "CONNECTED_DEVICE", "SET_TOP_BOX"]},
        )

        payload = json.loads(captured_requests[0].content)
        rendering_context = payload["variables"]["input"]["package"]["targeting"]["rendering_context"]

        assert rendering_context == {
            "op": "AND",
            "ad_placement": {"op": "==", "val": "CTV"},
            "distribution_channel": {"op": "INTERSECTS", "val": "APP"},
            "device_type": {
                "op": "INTERSECTS",
                "tv_devices": "tv,set-top-box",
            },
        }

    @pytest.mark.asyncio
    async def test_olv_channel_defaults_include_mobile_devices(self, mock_openx_graphql: respx.MockRouter):
        """An OLV deal with no explicit device_type must reach desktop AND mobile inventory."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="OLV_Default_Devices_Test",
            currency="USD",
            deal_price=0.10,
            start_date="2026-05-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="OLV_Default_Devices_Package",
            targeting={"channel": "OLV", "geographic": ["US"]},
        )

        payload = json.loads(captured_requests[0].content)
        rendering_context = payload["variables"]["input"]["package"]["targeting"]["rendering_context"]

        assert rendering_context == {
            "op": "AND",
            "ad_placement": {"op": "==", "val": "VIDEO"},
            "distribution_channel": {"op": "INTERSECTS", "val": "WEB,APP"},
            "device_type": {
                "op": "INTERSECTS",
                "desktop_devices": "desktop",
                "mobile_devices": "phone,tablet",
            },
        }

    @pytest.mark.asyncio
    async def test_display_channel_defaults_include_mobile_devices(self, mock_openx_graphql: respx.MockRouter):
        """A DISPLAY deal with no explicit device_type must reach desktop AND mobile inventory."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Display_Default_Devices_Test",
            currency="USD",
            deal_price=0.10,
            start_date="2026-05-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Display_Default_Devices_Package",
            targeting={"channel": "DISPLAY", "geographic": ["US"]},
        )

        payload = json.loads(captured_requests[0].content)
        device_type = payload["variables"]["input"]["package"]["targeting"]["rendering_context"]["device_type"]

        assert device_type == {
            "op": "INTERSECTS",
            "desktop_devices": "desktop",
            "mobile_devices": "phone,tablet",
        }

    @pytest.mark.asyncio
    async def test_ctv_channel_defaults_to_tv_devices(self, mock_openx_graphql: respx.MockRouter):
        """A CTV deal with no explicit device_type must default to TV devices
        (Connected TV + Set-Top Box), not desktop."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="CTV_Default_Devices_Test",
            currency="USD",
            deal_price=0.10,
            start_date="2026-05-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="CTV_Default_Devices_Package",
            targeting={"channel": "CTV", "geographic": ["US"]},
        )

        payload = json.loads(captured_requests[0].content)
        device_type = payload["variables"]["input"]["package"]["targeting"]["rendering_context"]["device_type"]

        assert device_type == {
            "op": "INTERSECTS",
            "tv_devices": "tv,set-top-box",
        }

    @pytest.mark.asyncio
    async def test_explicit_device_type_overrides_channel_default(self, mock_openx_graphql: respx.MockRouter):
        """Explicit device_type must override channel defaults — desktop-only stays desktop-only."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="OLV_Explicit_Desktop_Only_Test",
            currency="USD",
            deal_price=0.10,
            start_date="2026-05-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="OLV_Explicit_Desktop_Only_Package",
            targeting={"channel": "OLV", "device_type": ["DESKTOP"], "geographic": ["US"]},
        )

        payload = json.loads(captured_requests[0].content)
        device_type = payload["variables"]["input"]["package"]["targeting"]["rendering_context"]["device_type"]

        assert device_type == {
            "op": "INTERSECTS",
            "desktop_devices": "desktop",
        }

    @pytest.mark.asyncio
    async def test_single_country_geographic_targeting(self, mock_openx_graphql: respx.MockRouter):
        """Verify single country geographic targeting uses object format (not array)."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Single_Country_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="US_Package",
            targeting={"geographic": ["US"]},
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        package_targeting = input_params["package"]["targeting"]

        # Single country: {"includes": {"country": "US"}} (object, not array)
        expected_geo = {"includes": {"country": "US"}}
        assert package_targeting["geographic"] == expected_geo

    @pytest.mark.asyncio
    async def test_domain_allowlist(self, mock_openx_graphql: respx.MockRouter):
        """Verify url_targeting with domain allowlist in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        url_targeting = {"allowlist": ["premium-site.com", "quality-publisher.org", "trusted-media.net"]}

        await ox_create_deal(
            name="Domain_Allowlist_Deal",
            currency="USD",
            deal_price=8.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "DV360"}],
            package_name="Curated_Package",
            targeting={"geographic": ["US"]},
            url_targeting=url_targeting,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        # Verify url_targeting is in package with API format (type + urls)
        assert "url_targeting" in input_params["package"]
        assert input_params["package"]["url_targeting"]["type"] == "whitelist"
        assert input_params["package"]["url_targeting"]["urls"] == [
            "premium-site.com",
            "quality-publisher.org",
            "trusted-media.net",
        ]
        assert "allowlist" not in input_params["package"]["url_targeting"]

    @pytest.mark.asyncio
    async def test_domain_blocklist(self, mock_openx_graphql: respx.MockRouter):
        """Verify url_targeting with domain blocklist in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        url_targeting = {"blocklist": ["spam-site.com", "low-quality.net"]}

        await ox_create_deal(
            name="Domain_Blocklist_Deal",
            currency="USD",
            deal_price=6.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "XANDR"}],
            package_name="Blocklist_Package",
            targeting={},
            url_targeting=url_targeting,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        assert input_params["package"]["url_targeting"]["type"] == "blacklist"
        assert input_params["package"]["url_targeting"]["urls"] == [
            "spam-site.com",
            "low-quality.net",
        ]
        assert "blocklist" not in input_params["package"]["url_targeting"]

    @pytest.mark.asyncio
    async def test_third_party_fees_percent_of_media(self, mock_openx_graphql: respx.MockRouter):
        """Verify PERCENT_OF_MEDIA fee configuration in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        fees_config = {
            "partner_id": "partner-001",
            "revenue_method": "PoM",
            "gross_share": 30.0,
        }

        await ox_create_deal(
            name="Curated_Deal_With_Fees",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "CRIMTAN"}],
            package_name="Curated_Package",
            targeting={},
            third_party_fees_config=fees_config,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        assert "third_party_fees_config" in input_params
        # Must be wrapped in a list
        assert isinstance(input_params["third_party_fees_config"], list)
        assert len(input_params["third_party_fees_config"]) == 1
        fee = input_params["third_party_fees_config"][0]
        assert fee["partner_id"] == "partner-001"
        assert fee["revenue_method"] == "PoM"
        assert fee["gross_share"] == "30.0"

    @pytest.mark.asyncio
    async def test_third_party_fees_fixed_cpm(self, mock_openx_graphql: respx.MockRouter):
        """Verify FIXED_CPM fee configuration in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        fees_config = {
            "partner_id": "partner-002",
            "revenue_method": "CPM",
            "gross_cpm_cap": 1.50,
        }

        await ox_create_deal(
            name="Fixed_CPM_Deal",
            currency="USD",
            deal_price=10.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Standard_Package",
            targeting={},
            third_party_fees_config=fees_config,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        # Must be wrapped in a list
        assert isinstance(input_params["third_party_fees_config"], list)
        assert len(input_params["third_party_fees_config"]) == 1
        fee = input_params["third_party_fees_config"][0]
        assert fee["partner_id"] == "partner-002"
        assert fee["revenue_method"] == "CPM"
        assert fee["gross_cpm_cap"] == "1.5"

    @pytest.mark.asyncio
    async def test_third_party_platform_share_stripped_before_send(self, mock_openx_graphql: respx.MockRouter):
        """`platform_share` and `platform_partner_id` are response-only fields.

        They appear in OpenX's ``dealCreate`` *response* but are NOT valid
        members of ``ThirdPartyFeesConfigCreateParams`` on the input side.
        A caller who copies a fee block from a response and reuses it on
        a new create would otherwise hit:
            Field "platform_share" is not defined by type
            "ThirdPartyFeesConfigCreateParams".
        The normalizer strips both fields defensively so we never send
        them over the wire. The supported fields (``gross_share``,
        ``revenue_method``, ``partner_id``, ``gross_cpm_cap``) flow
        through unchanged and ``gross_share`` is still string-serialized.
        """
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        fees_config = {
            "partner_id": "partner-003",
            "revenue_method": "PoM",
            "gross_share": 40.0,
            # Response-only fields a naive caller might paste in. The
            # normalizer drops them before the request leaves cutlass.
            "platform_share": 12.5,
            "platform_partner_id": "540278980",
        }

        await ox_create_deal(
            name="Platform_Share_Deal",
            currency="USD",
            deal_price=10.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Standard_Package",
            targeting={},
            third_party_fees_config=fees_config,
        )

        payload = json.loads(captured_requests[0].content)
        fee = payload["variables"]["input"]["third_party_fees_config"][0]
        assert fee["gross_share"] == "40.0"
        assert "platform_share" not in fee
        assert "platform_partner_id" not in fee
        # Sanity: the rest of the fee block reaches OpenX intact.
        assert fee["partner_id"] == "partner-003"
        assert fee["revenue_method"] == "PoM"

    @pytest.mark.asyncio
    async def test_deal_participants_structure(self, mock_openx_graphql: respx.MockRouter):
        """Verify deal_participants structure in payload."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        participants = [
            {
                "demand_partner_id": "TTD",
                "buyer_ids": ["ttd-buyer-001", "ttd-buyer-002"],
                "brand_ids": ["brand-x", "brand-y"],
            },
            {
                "demand_partner_id": "DV360",
                "buyer_ids": ["dv360-buyer-001"],
            },
        ]

        await ox_create_deal(
            name="Multi_Partner_Deal",
            currency="USD",
            deal_price=12.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=participants,
            package_name="Multi_Partner_Package",
            targeting={},
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        assert len(input_params["deal_participants"]) == 2

        # First participant (demand_partner_id renamed to demand_partner)
        p1 = input_params["deal_participants"][0]
        assert p1["demand_partner"] == "TTD"
        assert "demand_partner_id" not in p1
        assert p1["buyer_ids"] == ["ttd-buyer-001", "ttd-buyer-002"]
        assert p1["brand_ids"] == ["brand-x", "brand-y"]

        # Second participant
        p2 = input_params["deal_participants"][1]
        assert p2["demand_partner"] == "DV360"
        assert "demand_partner_id" not in p2
        assert p2["buyer_ids"] == ["dv360-buyer-001"]

    @pytest.mark.asyncio
    async def test_end_date_when_provided(self, mock_openx_graphql: respx.MockRouter):
        """Verify end_date is included in payload when provided."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Dated_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            end_date="2024-12-31T23:59:59Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Dated_Package",
            targeting={},
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        assert input_params["end_date"] == "2024-12-31T23:59:59Z"

    @pytest.mark.asyncio
    async def test_request_headers(self, mock_openx_graphql: respx.MockRouter):
        """Verify correct headers are sent with the request."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Headers_Test_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        # Verify headers on the mutation request
        assert len(captured_requests) == 1
        request = captured_requests[0]
        assert request.headers.get("x-apikey") == "test-openx-api-key-12345"
        assert request.headers.get("Content-Type") == "application/json"
        assert "victoria-terminal" in request.headers.get("User-Agent", "")

    @pytest.mark.asyncio
    async def test_deal_price_is_string(self, mock_openx_graphql: respx.MockRouter):
        """Verify deal_price is sent as a string, not a float."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Price_Test_Deal",
            currency="USD",
            deal_price=7.50,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        # deal_price must be a string with 2 decimal places
        assert input_params["deal_price"] == "7.50"
        assert isinstance(input_params["deal_price"], str)


class TestCreateDealErrorHandling:
    """Tests for error handling in deal creation."""

    @pytest.mark.asyncio
    async def test_graphql_error_response(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of GraphQL errors in response."""

        # Create handler that returns introspection success but mutation error
        def error_handler(request: httpx.Request) -> httpx.Response:
            payload = json.loads(request.content)
            query = payload.get("query", "")
            if "__type" in query or "IntrospectType" in query:
                return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
            return httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=error_handler)

        result = await ox_create_deal(
            name="Error_Test_Deal",
            currency="USD",
            deal_price=-5.0,  # Invalid price
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        assert result["success"] is False
        assert "error" in result
        assert "GraphQL errors" in result["error"]
        assert "error_details" in result
        assert result["error_details"]["operation_name"] == "dealCreate"
        assert result["error_details"]["errors"][0]["extensions"]["code"] == "VALIDATION_ERROR"
        assert "create_payload_preview" in result
        assert "create_payload" in result
        assert result["create_payload_preview"]["name"] == "Error_Test_Deal"
        assert result["create_payload_preview"]["deal_price"] == "-5.00"
        assert result["create_payload_preview"]["package_name"] == "Test_Package"
        assert result["create_payload"]["name"] == "Error_Test_Deal"
        assert result["create_payload"]["package"]["name"] == "Test_Package"

    @pytest.mark.asyncio
    async def test_multiple_graphql_errors(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of multiple GraphQL errors - all messages should be included."""

        def error_handler(request: httpx.Request) -> httpx.Response:
            payload = json.loads(request.content)
            query = payload.get("query", "")
            if "__type" in query or "IntrospectType" in query:
                return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
            return httpx.Response(200, json=GRAPHQL_MULTIPLE_ERRORS)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=error_handler)

        result = await ox_create_deal(
            name="",
            currency="INVALID",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        assert result["success"] is False
        assert "error" in result
        # Both error messages should be joined with semicolons
        assert "required" in result["error"].lower()
        assert "currency" in result["error"].lower()
        # Verify both messages are present (joined by semicolon)
        assert ";" in result["error"]

    @pytest.mark.asyncio
    async def test_graphql_error_includes_payload_summary(self, mock_openx_graphql: respx.MockRouter):
        """Create failures should return a compact preview of the final dealCreate payload."""

        def error_handler(request: httpx.Request) -> httpx.Response:
            payload = json.loads(request.content)
            query = payload.get("query", "")
            if "__type" in query or "IntrospectType" in query:
                return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
            return httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=error_handler)

        result = await ox_create_deal(
            name="Preview_Test_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            end_date="2024-12-31T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD", "buyer_ids": ["buyer-123"]}],
            package_name="Preview_Package",
            targeting={"geographic": ["US"], "device_type": ["DESKTOP", "MOBILE"], "channel": "DISPLAY"},
            url_targeting={"allowlist": ["example.com", "example.org"], "domain_targeting_option": "ROOT"},
            third_party_fees_config={"partner_id": "560610563", "revenue_method": "PoM", "gross_share": "0.4"},
        )

        assert result["success"] is False
        preview = result["create_payload_preview"]
        assert preview["name"] == "Preview_Test_Deal"
        assert preview["deal_participants"] == [{"demand_partner": "TTD", "buyer_ids": ["buyer-123"]}]
        assert preview["targeting_keys"] == [
            "geographic",
            "inter_dimension_operator",
            "metacategory",
            "rendering_context",
        ]
        assert preview["has_rendering_context"] is True
        assert preview["url_targeting"] == {
            "type": "whitelist",
            "urls_count": 2,
            "domain_targeting_option": "ROOT",
        }
        full_payload = result["create_payload"]
        assert full_payload["name"] == "Preview_Test_Deal"
        assert full_payload["deal_participants"] == [{"demand_partner": "TTD", "buyer_ids": ["buyer-123"]}]
        assert full_payload["package"]["name"] == "Preview_Package"
        assert full_payload["package"]["url_targeting"] == {
            "type": "whitelist",
            "urls": ["example.com", "example.org"],
            "domain_targeting_option": "ROOT",
        }
        assert preview["third_party_fees_config"] == [
            {
                "partner_id": "560610563",
                "revenue_method": "PoM",
                "gross_share": "0.4",
                "gross_cpm_cap": None,
            }
        ]
        assert full_payload["third_party_fees_config"] == [
            {
                "partner_id": "560610563",
                "revenue_method": "PoM",
                "gross_share": "0.4",
            }
        ]

    @pytest.mark.asyncio
    async def test_create_payload_preserves_main_buyer_id(self, mock_openx_graphql: respx.MockRouter):
        """Low-level create payload should preserve main_buyer_id when explicitly supplied."""

        def error_handler(request: httpx.Request) -> httpx.Response:
            payload = json.loads(request.content)
            query = payload.get("query", "")
            if "__type" in query or "IntrospectType" in query:
                return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
            return httpx.Response(200, json=GRAPHQL_ERROR_RESPONSE)

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=error_handler)

        result = await ox_create_deal(
            name="Preview_Main_Buyer_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "537073292", "buyer_ids": ["109"], "main_buyer_id": "109"}],
            package_name="Preview_Package",
            targeting={"geographic": ["CA"], "channel": "DISPLAY"},
        )

        assert result["success"] is False
        preview = result["create_payload_preview"]
        assert preview["deal_participants"] == [
            {"demand_partner": "537073292", "buyer_ids": ["109"], "main_buyer_id": "109"}
        ]
        full_payload = result["create_payload"]
        assert full_payload["deal_participants"] == [
            {"demand_partner": "537073292", "buyer_ids": ["109"], "main_buyer_id": "109"}
        ]

    @pytest.mark.asyncio
    async def test_http_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of HTTP errors."""

        def error_handler(request: httpx.Request) -> httpx.Response:
            payload = json.loads(request.content)
            query = payload.get("query", "")
            if "__type" in query or "IntrospectType" in query:
                return httpx.Response(200, json=INTROSPECT_GEOGRAPHIC_ITEM_RESPONSE)
            return httpx.Response(500, text="Internal Server Error")

        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=error_handler)

        result = await ox_create_deal(
            name="HTTP_Error_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        assert result["success"] is False
        assert "error" in result

    @pytest.mark.asyncio
    async def test_network_error_handling(self, mock_openx_graphql: respx.MockRouter):
        """Test handling of network errors."""
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(side_effect=httpx.ConnectError("Connection refused"))

        result = await ox_create_deal(
            name="Network_Error_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
        )

        assert result["success"] is False
        assert "error" in result


class TestCreateDealApiKeyValidation:
    """Tests for API key validation - should fail before HTTP call."""

    @pytest.mark.asyncio
    async def test_missing_api_key_fails_before_http(self):
        """Test that missing API key fails before making an HTTP call."""
        # Import here to reset singleton
        import openx_mcp

        openx_mcp._openx_client = None

        # Remove the API key from environment and use respx to detect HTTP calls
        with (
            patch.dict(os.environ, {"OPENX_API_KEY": ""}, clear=False),
            respx.mock(assert_all_called=False) as mock,
        ):
            # Configure a route that should NOT be called
            route = mock.post(OPENX_GRAPHQL_ENDPOINT).mock(
                return_value=httpx.Response(200, json=CREATE_DEAL_SUCCESS_RESPONSE)
            )

            result = await ox_create_deal(
                name="No_Key_Test",
                currency="USD",
                deal_price=5.0,
                start_date="2024-01-01T00:00:00Z",
                deal_participants=[{"demand_partner_id": "TTD"}],
                package_name="Test_Package",
                targeting={},
            )

            # Verify it failed
            assert result["success"] is False
            assert "error" in result
            assert "API key" in result["error"] or "not configured" in result["error"].lower()

            # Verify NO HTTP call was made
            assert route.call_count == 0


class TestCreateDealFullWorkflow:
    """Integration-style tests for complete deal creation workflows."""

    @pytest.mark.asyncio
    async def test_complete_curated_deal(self, mock_openx_graphql: respx.MockRouter):
        """Test creating a complete curated deal with all fields."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        result = await ox_create_deal(
            name="Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0",
            currency="USD",
            deal_price=5.50,
            start_date="2024-01-15T00:00:00Z",
            end_date="2024-06-30T23:59:59Z",
            deal_participants=[
                {
                    "demand_partner_id": "CRIMTAN",
                    "buyer_ids": ["crimtan-buyer-001"],
                }
            ],
            package_name="US_CuratedDomains_Package",
            targeting={
                "geographic": ["US"],
                "device_type": ["CTV", "DESKTOP"],
            },
            url_targeting={
                "allowlist": [
                    "premium-news.com",
                    "quality-entertainment.net",
                    "trusted-sports.org",
                ]
            },
            third_party_fees_config={
                "partner_id": "partner-001",
                "revenue_method": "PoM",
                "gross_share": 30.0,
            },
        )

        # Verify success
        assert result["success"] is True
        assert "deal" in result

        # Verify complete payload structure
        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        # All fields should be present - deal_price must be a string with 2 decimal places
        assert input_params["name"] == "Elcano_OpenX_Crimtan_US_CuratedDomains_ELC00001_A0"
        assert input_params["currency"] == "USD"
        assert input_params["deal_price"] == "5.50"
        assert isinstance(input_params["deal_price"], str)
        assert input_params["start_date"] == "2024-01-15T00:00:00Z"
        assert input_params["end_date"] == "2024-06-30T23:59:59Z"
        assert len(input_params["deal_participants"]) == 1
        assert input_params["package"]["name"] == "US_CuratedDomains_Package"
        # Single country uses comma-delimited format: {"includes": {"country": "US"}}
        assert input_params["package"]["targeting"]["geographic"] == {"includes": {"country": "US"}}

    @pytest.mark.asyncio
    async def test_create_deal_forces_unique_targeting_settings(self, mock_openx_graphql: respx.MockRouter):
        """Verify caller-supplied targeting criteria cannot override unique targeting settings."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="Forced_Unique_Targeting_Deal",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Unique_Targeting_Package",
            targeting={"geographic": ["US"], "inter_dimension_operator": "OR"},
        )

        payload = json.loads(captured_requests[0].content)
        assert payload["variables"]["input"]["package"]["targeting"]["inter_dimension_operator"] == "AND"


class TestPmpDealTypeMapping:
    """Tests for pmp_deal_type human-readable to numeric code mapping."""

    @pytest.mark.asyncio
    async def test_preferred_deal_maps_to_3(self, mock_openx_graphql: respx.MockRouter):
        """Test that PREFERRED_DEAL is mapped to numeric code '3'."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="PMP_Type_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
            pmp_deal_type="PREFERRED_DEAL",
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        assert payload["variables"]["input"]["pmp_deal_type"] == "3"

    @pytest.mark.asyncio
    async def test_private_auction_maps_to_2(self, mock_openx_graphql: respx.MockRouter):
        """Test that PRIVATE_AUCTION is mapped to numeric code '2'."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="PMP_Type_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
            pmp_deal_type="PRIVATE_AUCTION",
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        assert payload["variables"]["input"]["pmp_deal_type"] == "2"

    @pytest.mark.asyncio
    async def test_numeric_passthrough(self, mock_openx_graphql: respx.MockRouter):
        """Test that numeric codes pass through unchanged."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="PMP_Type_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
            pmp_deal_type="3",
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        assert payload["variables"]["input"]["pmp_deal_type"] == "3"

    @pytest.mark.asyncio
    async def test_case_insensitive(self, mock_openx_graphql: respx.MockRouter):
        """Test that mapping is case-insensitive."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        await ox_create_deal(
            name="PMP_Type_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
            pmp_deal_type="preferred_deal",
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        assert payload["variables"]["input"]["pmp_deal_type"] == "3"


class TestUrlTargetingRawFormat:
    """Tests for url_targeting raw API format passthrough."""

    @pytest.mark.asyncio
    async def test_url_targeting_raw_format(self, mock_openx_graphql: respx.MockRouter):
        """Test that raw API format (type + urls) passes through unchanged."""
        captured_requests: list[httpx.Request] = []
        mock_openx_graphql.post(OPENX_GRAPHQL_ENDPOINT).mock(
            side_effect=create_mock_handler(captured_requests=captured_requests)
        )

        url_targeting = {"type": "whitelist", "urls": ["example.com"]}

        await ox_create_deal(
            name="Raw_URL_Test",
            currency="USD",
            deal_price=5.0,
            start_date="2024-01-01T00:00:00Z",
            deal_participants=[{"demand_partner_id": "TTD"}],
            package_name="Test_Package",
            targeting={},
            url_targeting=url_targeting,
        )

        assert len(captured_requests) == 1
        payload = json.loads(captured_requests[0].content)
        input_params = payload["variables"]["input"]

        assert input_params["package"]["url_targeting"]["type"] == "whitelist"
        assert input_params["package"]["url_targeting"]["urls"] == ["example.com"]
