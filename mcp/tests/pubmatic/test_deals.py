import respx
from pubmatic_mcp import (
    pm_create_curated_deal,
    pm_discover_dsp_buyer_map,
    pm_discover_dsps,
    pm_get_curated_deal,
)


class TestPubMaticCuratedDeals:
    async def test_pm_create_curated_deal_success(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "access-123",
                "refreshToken": "refresh-123",
            },
        )
        mock_pubmatic_api.post("https://api.pubmatic.com/curateddeals/create").respond(
            200,
            json={"id": 111, "dealId": "TEST-ELC-CUR-001", "name": "TestDeal_Elcano_Curated_001"},
        )

        result = await pm_create_curated_deal(
            {
                "name": "TestDeal_Elcano_Curated_001",
                "dealId": "TEST-ELC-CUR-001",
                "auctionType": 1,
                "flooreCPM": 0.0,
                "startDate": "2026-04-01T00:00:00.000Z",
                "endDate": "2026-04-30T00:00:00.000Z",
                "timeZone": 1,
                "targeting": 999,
                "platforms": [1],
                "adFormats": [3],
                "hasMaxReach": 0,
                "priority": 10,
                "dealSource": 1,
                "loggedInOwnerId": 60067,
                "loggedInOwnerTypeId": 5,
                "dealDspBuyerMappings": [{"buyerId": 2, "dspId": 3, "seatId": "seat-1"}],
            }
        )

        assert result["success"] is True
        assert result["id"] == 111
        assert result["dealId"] == "TEST-ELC-CUR-001"

    async def test_pm_create_curated_deal_rejects_wrong_owner(self):
        result = await pm_create_curated_deal(
            {
                "name": "TestDeal_Elcano_Curated_001",
                "auctionType": 1,
                "startDate": "2026-04-01T00:00:00.000Z",
                "endDate": "2026-04-30T00:00:00.000Z",
                "loggedInOwnerId": 1,
                "loggedInOwnerTypeId": 5,
            }
        )
        assert result["success"] is False
        assert "SECURITY" in result["error"]

    async def test_pm_get_curated_deal_success(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "access-123",
                "refreshToken": "refresh-123",
            },
        )
        mock_pubmatic_api.get("https://api.pubmatic.com/curateddeals/111").respond(
            200,
            json={"id": 111, "name": "TestDeal_Elcano_Curated_001", "ownedById": 60067},
        )

        result = await pm_get_curated_deal(
            curated_id=111,
            logged_in_owner_id=60067,
            logged_in_owner_type_id=5,
            view="SUMMARY",
        )
        assert result["success"] is True
        assert result["result"]["id"] == 111

    async def test_pm_discover_dsps_success(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "access-123",
                "refreshToken": "refresh-123",
            },
        )
        mock_pubmatic_api.get("https://api.pubmatic.com/v1/common/advertisingEntity").respond(
            200,
            json={
                "items": [
                    {"id": 10, "name": "DSP One", "extra": "x"},
                    {"id": 20, "name": "DSP Two"},
                ]
            },
        )

        result = await pm_discover_dsps(logged_in_owner_type_id=7)
        assert result["success"] is True
        assert result["items"] == [{"id": 10, "name": "DSP One"}, {"id": 20, "name": "DSP Two"}]

    async def test_pm_discover_dsp_buyer_map_success(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "access-123",
                "refreshToken": "refresh-123",
            },
        )
        mock_pubmatic_api.get("https://api.pubmatic.com/v1/common/advertisingEntity/dspBuyerMap").respond(
            200,
            json={
                "items": [
                    {
                        "dspId": 99,
                        "dspName": "Test Demand Partner",
                        "buyerId": 77,
                        "buyerName": "Demo Test 2",
                        "seatId": "123rah",
                    }
                ]
            },
        )

        result = await pm_discover_dsp_buyer_map(dsp_id=99, query="123rah")
        assert result["success"] is True
        assert result["items"] == [
            {
                "dspId": 99,
                "dspName": "Test Demand Partner",
                "buyerId": 77,
                "buyerName": "Demo Test 2",
                "seatId": "123rah",
                "dspBuyerId": None,
            }
        ]
