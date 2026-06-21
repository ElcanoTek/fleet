import respx
from pubmatic_mcp import pm_auth_status


class TestPubMaticAuth:
    async def test_pm_auth_status_success(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            200,
            json={
                "userEmail": "user@example.com",
                "tokenType": "Bearer",
                "accessToken": "access-123",
                "refreshToken": "refresh-123",
            },
        )

        result = await pm_auth_status()

        assert result["configured"] is True
        assert result["authenticated"] is True
        assert result["user_email"] == "user@example.com"

    async def test_pm_auth_status_failure(self, mock_pubmatic_api: respx.MockRouter):
        mock_pubmatic_api.post("https://api.pubmatic.com/v1/developer-integrations/developer/token").respond(
            401,
            json=[{"errorCode": "AUTH_FAILED", "errorMessage": "Invalid Token Provided"}],
        )

        result = await pm_auth_status()

        assert result["configured"] is True
        assert result["authenticated"] is False
        assert "Invalid Token Provided" in result["error"]
