"""Regression tests for the IX execute-tool fixes shipped in
feat/ix-execute-tool-segment-exclude-and-dma.

Each test covers one of the six items called out by the audit of the
Brookfield Zoo deal (IX 506656). The deal landed but only after the
agent burned ~$2 iterating around bugs that should have been one-shots:

  1. ix_list_segments returned the entire 92,995-entry catalog with no
     server- or client-side filter.
  2. ix_execute_deal_from_prompt_inputs rejected numeric seat IDs ("2968").
  3. ix_create_marketplace_deal validation required targetingKeyID for
     segment targeting, but the IX API stores segments without one.
  4. Segment names with publisher prefixes (e.g. "The Weather Company > X")
     didn't resolve against catalog entries stored as bare "X".
  5. The high-level tool had no excluded_segment_names parameter so
     callers had to drop down to ix_create_marketplace_deal.
  6. The high-level tool had no dma_codes parameter — DMA targeting
     requires the special `ZipCode` key #781.
"""

import indexexchange_mcp
import pytest
from indexexchange_mcp import (
    _build_marketplace_deal_payload,
    _resolve_segment_with_prefix_tolerance,
    ix_list_segments,
)

# ---------------------------------------------------------------------------
# Fix 1 — ix_list_segments accepts a name_like filter
# ---------------------------------------------------------------------------


class TestListSegmentsNameLike:
    @pytest.mark.asyncio
    async def test_filters_client_side_when_name_like_passed(self, monkeypatch):
        catalog = [
            {"segmentID": 1, "externalSegmentName": "Weather Targeting > Sunny"},
            {"segmentID": 2, "externalSegmentName": "Weather Targeting > Light Rain"},
            {"segmentID": 3, "externalSegmentName": "Auto Intenders"},
            {"segmentID": 4, "externalSegmentName": "Sports Fans"},
        ]

        class FakeClient:
            async def request(self, _method, path, **_kwargs):
                assert path == "/api/segments/v2/segments"
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await ix_list_segments(account_id=123, name_like="Weather")
        assert result["success"] is True
        names = [s["externalSegmentName"] for s in result["segments"]]
        assert names == [
            "Weather Targeting > Sunny",
            "Weather Targeting > Light Rain",
        ]

    @pytest.mark.asyncio
    async def test_returns_full_catalog_when_name_like_omitted(self, monkeypatch):
        catalog = [{"segmentID": i, "externalSegmentName": f"S{i}"} for i in range(50)]

        class FakeClient:
            async def request(self, *_, **__):
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await ix_list_segments(account_id=123)
        assert len(result["segments"]) == 50


# ---------------------------------------------------------------------------
# Server-side filtering on ix_list_marketplace_publishers and ix_list_dsp_seats
# Production catalogs return ~1MB of JSON; filtering prevents truncation and
# the manual-grep workflow that followed it in the TWC regression.
# ---------------------------------------------------------------------------


class TestListMarketplacePublishersNameLike:
    @pytest.mark.asyncio
    async def test_filters_by_substring_when_name_like_passed(self, monkeypatch):
        catalog = [
            {"accountID": 2722, "name": "The Weather Company via Prebid"},
            {"accountID": 2725, "name": "The Weather Company via OB"},
            {"accountID": 5398, "name": "WeatherZone via S2S OB"},
            {"accountID": 1100, "name": "Yahoo North America"},
        ]

        class FakeClient:
            async def request(self, _method, path, **_kwargs):
                assert "publishers" in path
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await indexexchange_mcp.ix_list_marketplace_publishers(
            marketplace_account_id=1499155, name_like="weather company"
        )
        assert result["success"] is True
        names = [p["name"] for p in result["publishers"]]
        assert names == [
            "The Weather Company via Prebid",
            "The Weather Company via OB",
        ]

    @pytest.mark.asyncio
    async def test_returns_full_catalog_when_name_like_omitted(self, monkeypatch):
        catalog = [{"accountID": i, "name": f"Pub{i}"} for i in range(30)]

        class FakeClient:
            async def request(self, *_, **__):
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await indexexchange_mcp.ix_list_marketplace_publishers(marketplace_account_id=1499155)
        assert len(result["publishers"]) == 30


class TestListDspSeatsFilters:
    @pytest.mark.asyncio
    async def test_filters_by_seat_id_like(self, monkeypatch):
        catalog = [
            {"seatID": 100, "extendedSeatID": "100", "name": "Amazon", "dspID": 198},
            {"seatID": 5030037, "extendedSeatID": "AMZATKFYXZ39AR77", "dspID": 198},
            {"seatID": 469, "extendedSeatID": "200_469", "name": "GroupM - Xaxis", "dspID": 198},
        ]

        class FakeClient:
            async def request(self, _method, path, **_kwargs):
                assert path.endswith("/seats")
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await indexexchange_mcp.ix_list_dsp_seats(dsp_id=198, seat_id_like="AMZATKFYXZ39AR77")
        assert result["success"] is True
        assert len(result["seats"]) == 1
        assert result["seats"][0]["seatID"] == 5030037

    @pytest.mark.asyncio
    async def test_filters_by_name_like(self, monkeypatch):
        catalog = [
            {"seatID": 100, "name": "Amazon", "dspID": 198},
            {"seatID": 469, "name": "GroupM - Xaxis", "dspID": 198},
            {"seatID": 487, "name": "GroupM - Xaxis", "dspID": 198},
            {"seatID": 595, "name": "GroupM - Mediacom", "dspID": 198},
        ]

        class FakeClient:
            async def request(self, *_, **__):
                return catalog

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await indexexchange_mcp.ix_list_dsp_seats(dsp_id=198, name_like="xaxis")
        seats = result["seats"]
        assert len(seats) == 2
        assert all("Xaxis" in s["name"] for s in seats)

    @pytest.mark.asyncio
    async def test_unwraps_nested_seats_envelope(self, monkeypatch):
        """Some IX accounts return {"seats": [...]} instead of a bare list.
        Filter result should always be a flat list under `seats`."""
        nested_payload = {"seats": [{"seatID": 1, "extendedSeatID": "1", "dspID": 1}]}

        class FakeClient:
            async def request(self, *_, **__):
                return nested_payload

        monkeypatch.setattr(indexexchange_mcp, "get_ix_client", lambda: FakeClient())
        result = await indexexchange_mcp.ix_list_dsp_seats(dsp_id=1)
        assert isinstance(result["seats"], list)
        assert result["seats"][0]["seatID"] == 1


# ---------------------------------------------------------------------------
# Fix 3 — targetingKeyID is optional for segment targeting
# ---------------------------------------------------------------------------


class TestSegmentTargetingValidationRelaxed:
    """The IX API stores `keyName: "segmentid"` without a targetingKeyID
    — the validator must allow it (and the create payload must omit
    targetingKeyID for segment objects)."""

    def test_accepts_segment_targeting_without_targeting_key_id(self):
        payload = _build_marketplace_deal_payload(
            account_id=123,
            name="Test Deal",
            external_deal_id="IXTEST1",
            start_date="2026-05-01",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=180,
            seat_ids=["2968"],
            targeting=[
                {
                    "keyName": "segmentid",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "238564"}]}],
                }
            ],
        )
        segment_targeting = next(t for t in payload["targeting"] if t.get("keyName") == "segmentid")
        assert "targetingKeyID" not in segment_targeting

    def test_strips_targeting_key_id_from_segment_payload(self):
        """A caller that DOES pass targetingKeyID for segments (e.g. older
        prompts that defaulted to a placeholder) gets it removed before the
        request goes out."""
        payload = _build_marketplace_deal_payload(
            account_id=123,
            name="Test Deal",
            external_deal_id="IXTEST2",
            start_date="2026-05-01",
            end_date="2026-12-31",
            floor=0.10,
            dsp_id=180,
            seat_ids=["2968"],
            targeting=[
                {
                    "targetingKeyID": 1,  # placeholder — should be stripped
                    "keyName": "segmentid",
                    "targetingType": "standard",
                    "sets": [{"operator": "ANY_OF", "values": [{"value": "238564"}]}],
                }
            ],
        )
        segment_targeting = next(t for t in payload["targeting"] if t.get("keyName") == "segmentid")
        assert "targetingKeyID" not in segment_targeting

    def test_still_rejects_missing_targeting_key_id_for_other_keys(self):
        with pytest.raises(ValueError, match="targetingKeyID"):
            _build_marketplace_deal_payload(
                account_id=123,
                name="Test Deal",
                external_deal_id="IXTEST3",
                start_date="2026-05-01",
                end_date="2026-12-31",
                floor=0.10,
                dsp_id=180,
                targeting=[
                    {
                        "keyName": "country",  # NOT segment, must have targetingKeyID
                        "targetingType": "standard",
                        "sets": [{"operator": "ANY_OF", "values": [{"value": "USA"}]}],
                    }
                ],
            )


# ---------------------------------------------------------------------------
# Fix 4 — segment-name prefix tolerance
# ---------------------------------------------------------------------------


class TestSegmentPrefixTolerance:
    """Trader prompts namespace segments under the data partner ('The Weather
    Company > Weather Targeting > X'); the catalog stores bare 'Weather
    Targeting > X'. Resolver must try the full name first then progressively
    strip leading components."""

    def test_strips_publisher_prefix(self):
        catalog = [
            {"segmentID": 238564, "externalSegmentName": "Weather Targeting > Absolute > Current > Sunny"},
            {"segmentID": 238648, "externalSegmentName": "Weather Targeting > Relative > Forecast > Sunny"},
        ]
        result = _resolve_segment_with_prefix_tolerance(
            catalog,
            "The Weather Company > Weather Targeting > Absolute > Current > Sunny",
        )
        assert result["segmentID"] == 238564

    def test_strips_multiple_prefix_levels(self):
        catalog = [{"segmentID": 99, "externalSegmentName": "Sunny"}]
        result = _resolve_segment_with_prefix_tolerance(
            catalog,
            "Foo > Bar > Baz > Sunny",
        )
        assert result["segmentID"] == 99

    def test_full_name_wins_when_present(self):
        catalog = [
            {"segmentID": 1, "externalSegmentName": "The Weather Company > X"},
            {"segmentID": 2, "externalSegmentName": "X"},
        ]
        result = _resolve_segment_with_prefix_tolerance(catalog, "The Weather Company > X")
        assert result["segmentID"] == 1

    def test_raises_when_no_match_at_any_prefix_level(self):
        catalog = [{"segmentID": 1, "externalSegmentName": "Different Thing"}]
        with pytest.raises((LookupError, ValueError)):
            _resolve_segment_with_prefix_tolerance(catalog, "Foo > Bar > Atlantis")


# ---------------------------------------------------------------------------
# Fixes 2, 5, 6 — execute-tool: numeric seat passthrough, excluded segments,
# DMA codes. These are end-to-end against the high-level tool with stubbed
# resolution helpers.
# ---------------------------------------------------------------------------


@pytest.fixture
def stub_dependencies(monkeypatch):
    """Stub the network-touching helpers so we can exercise the high-level
    tool's parameter wiring without spinning up full HTTP mocks."""

    async def fake_list_dsps(**_):
        return {
            "success": True,
            "dsps": [{"dspID": 180, "name": "Viant", "classID": 4}],
        }

    async def fake_list_publishers(**_):
        return {"success": True, "publishers": []}

    async def fake_list_dsp_seats(**_):
        return {
            "success": True,
            "seats": [
                {"seatID": 2968, "extendedSeatID": "2968", "dspID": 180},
                {"seatID": 5030037, "extendedSeatID": "AMZATKFYXZ39AR77", "dspID": 198},
            ],
        }

    async def fake_list_targeting_keys(**_):
        return {
            "success": True,
            "targeting_keys": [
                {"targetingKeyID": 9, "key": "country", "keyName": "Country"},
                {"targetingKeyID": 1067, "key": "regionCode", "keyName": "regionCode"},
                {"targetingKeyID": 781, "key": "ZipCode", "keyName": "ZipCode"},
            ],
        }

    async def fake_list_segments(**_):
        return {
            "success": True,
            "segments": [
                {
                    "segmentID": 238564,
                    "externalSegmentName": "Weather Targeting > Absolute > Current > Sunny",
                },
                {"segmentID": 308129, "externalSegmentName": "Weather Block List"},
            ],
        }

    captured: dict = {}

    async def fake_create_marketplace_deal(**kwargs):
        captured.update(kwargs)
        return {
            "success": True,
            "deal_url": "https://app.indexexchange.com/deals/506656/show?account_id=1499155",
            "deal": {"internalDealID": 506656, "externalDealID": "IX9001"},
            "warnings": [],
            "quality_flags": [],
        }

    monkeypatch.setattr(indexexchange_mcp, "ix_list_dsps", fake_list_dsps)
    monkeypatch.setattr(indexexchange_mcp, "ix_list_marketplace_publishers", fake_list_publishers)
    monkeypatch.setattr(indexexchange_mcp, "ix_list_dsp_seats", fake_list_dsp_seats)
    monkeypatch.setattr(indexexchange_mcp, "ix_list_targeting_keys", fake_list_targeting_keys)
    monkeypatch.setattr(indexexchange_mcp, "ix_list_segments", fake_list_segments)
    monkeypatch.setattr(indexexchange_mcp, "ix_create_marketplace_deal", fake_create_marketplace_deal)
    return captured


class TestSeatResolution:
    """The IX create API requires the seat's `extendedSeatID` string —
    not the numeric `seatID`. The resolver must accept either form (or
    the human-readable name) and always forward the extendedSeatID.

    Regression context: the original implementation treated pure-digit
    input as a fast-path that bypassed `ix_list_dsp_seats` entirely and
    forwarded the bare seatID. That broke TWC's Amazon seat
    (seatID 5030037 / extendedSeatID AMZATKFYXZ39AR77) because the create
    endpoint rejected the bare seatID with HTTP 400 'Specified 1 seat
    IDs but found 0'. See conversation history on cutlass#TBD."""

    @pytest.mark.asyncio
    async def test_numeric_seat_resolves_to_extended_seat_id(self, stub_dependencies):
        """Pure-digit input must look up the seat and forward the extendedSeatID.

        This is the TWC regression case: seatID 5030037 must round-trip to
        extendedSeatID 'AMZATKFYXZ39AR77'."""
        result = await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="5030037",
            end_date="2026-12-31",
        )
        assert result["success"] is True
        assert stub_dependencies["seat_ids"] == ["AMZATKFYXZ39AR77"]

    @pytest.mark.asyncio
    async def test_numeric_seat_equal_to_extended_id_round_trips(self, stub_dependencies):
        """For older seats where seatID == extendedSeatID (e.g. seatID 2968
        whose extendedSeatID is also '2968'), the result is the same string
        but still routed through the lookup."""
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
        )
        assert stub_dependencies["seat_ids"] == ["2968"]

    @pytest.mark.asyncio
    async def test_extended_seat_id_input_resolves_directly(self, stub_dependencies):
        """Caller may pass the extendedSeatID directly (alphanumeric
        identifier with no human-readable name)."""
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="AMZATKFYXZ39AR77",
            end_date="2026-12-31",
        )
        assert stub_dependencies["seat_ids"] == ["AMZATKFYXZ39AR77"]

    @pytest.mark.asyncio
    async def test_dsp_slash_form_keeps_right_side(self, stub_dependencies):
        """`bidswitch/2968` keeps `2968` and resolves it through the lookup."""
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="bidswitch/2968",
            end_date="2026-12-31",
        )
        assert stub_dependencies["seat_ids"] == ["2968"]


class TestPublisherAmbiguity:
    """When publisher_names resolves to >1 candidate (e.g. TWC's 'via
    Prebid' / 'via OB' feed variants), the tool must refuse to silently
    pick — emit a structured quality flag and require the caller to
    re-issue with explicit publisher_ids."""

    @pytest.fixture
    def stub_with_twc_publishers(self, monkeypatch):
        async def fake_list_dsps(**_):
            return {"success": True, "dsps": [{"dspID": 198, "name": "Amazon", "classID": 4}]}

        async def fake_list_publishers(**_):
            return {
                "success": True,
                "publishers": [
                    {
                        "accountID": 2722,
                        "legacyAccountID": 182970,
                        "name": "The Weather Company via Prebid",
                        "accountStatus": "A",
                        "currency": "USD",
                    },
                    {
                        "accountID": 2725,
                        "legacyAccountID": 184272,
                        "name": "The Weather Company via OB",
                        "accountStatus": "A",
                        "currency": "USD",
                    },
                ],
            }

        async def fake_list_dsp_seats(**_):
            return {"success": True, "seats": [{"seatID": 1, "extendedSeatID": "1", "dspID": 198}]}

        async def fake_list_targeting_keys(**_):
            return {"success": True, "targeting_keys": []}

        captured: dict = {}

        async def fake_create_marketplace_deal(**kwargs):
            captured.update(kwargs)
            return {
                "success": True,
                "deal_url": "https://app.indexexchange.com/deals/519100/show?account_id=1499155",
                "deal": {"internalDealID": 519100, "externalDealID": "IX9000"},
                "warnings": [],
                "quality_flags": [],
            }

        monkeypatch.setattr(indexexchange_mcp, "ix_list_dsps", fake_list_dsps)
        monkeypatch.setattr(indexexchange_mcp, "ix_list_marketplace_publishers", fake_list_publishers)
        monkeypatch.setattr(indexexchange_mcp, "ix_list_dsp_seats", fake_list_dsp_seats)
        monkeypatch.setattr(indexexchange_mcp, "ix_list_targeting_keys", fake_list_targeting_keys)
        monkeypatch.setattr(indexexchange_mcp, "ix_create_marketplace_deal", fake_create_marketplace_deal)
        return captured

    @pytest.mark.asyncio
    async def test_ambiguous_publisher_name_emits_quality_flag(self, stub_with_twc_publishers):
        """'The Weather Company' matches 2 feeds — must NOT silently attach either."""
        result = await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-06-15",
            floor=0.10,
            dsp_name="Amazon",
            seat_name="1",
            end_date="2026-12-31",
            publisher_names=["The Weather Company"],
        )
        # No publisher_ids should have been forwarded — caller must re-issue.
        # (resolved_publisher_ids was [], which translates to publisher_ids=None
        # on the create call.)
        assert stub_with_twc_publishers["publisher_ids"] is None
        # The quality flag must include both candidates so the operator can
        # update the brief.
        flags = result["quality_flags"]
        ambiguity_flags = [f for f in flags if f.get("flag") == "ix_publisher_resolution_ambiguous"]
        assert len(ambiguity_flags) == 1
        candidates = ambiguity_flags[0]["candidates"]
        legacy_ids = sorted(c["legacyAccountID"] for c in candidates)
        assert legacy_ids == [182970, 184272]

    @pytest.mark.asyncio
    async def test_explicit_publisher_ids_bypass_ambiguity(self, stub_with_twc_publishers):
        """The brief-driven path: pass publisher_ids=[...] explicitly and
        no ambiguity flag fires."""
        result = await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-06-15",
            floor=0.10,
            dsp_name="Amazon",
            seat_name="1",
            end_date="2026-12-31",
            publisher_ids=[182970, 184272],
        )
        assert stub_with_twc_publishers["publisher_ids"] == [182970, 184272]
        flags = result["quality_flags"]
        assert not any(f.get("flag", "").startswith("ix_publisher_resolution") for f in flags)


@pytest.mark.usefixtures("stub_publishers")
class TestFindPublisherByName:
    """The find_publisher_by_name convenience tool returns 0/1/N matches
    with the trade-off context (status, currency, isOptedOutOfMarketplaces)
    so callers can decide which legacyAccountIDs to use."""

    @pytest.fixture
    def stub_publishers(self, monkeypatch):
        async def fake_list_marketplace_publishers(marketplace_account_id, name_like=None):
            full = [
                {
                    "accountID": 2722,
                    "legacyAccountID": 182970,
                    "name": "The Weather Company via Prebid",
                    "accountStatus": "A",
                    "currency": "USD",
                    "isOptedOutOfMarketplaces": False,
                },
                {
                    "accountID": 2725,
                    "legacyAccountID": 184272,
                    "name": "The Weather Company via OB",
                    "accountStatus": "A",
                    "currency": "USD",
                },
                {
                    "accountID": 5398,
                    "legacyAccountID": 189342,
                    "name": "WeatherZone via S2S OB",
                    "accountStatus": "A",
                    "currency": "AUD",
                },
                {
                    "accountID": 7000,
                    "legacyAccountID": 200000,
                    "name": "Unrelated Publisher",
                    "accountStatus": "A",
                    "currency": "USD",
                },
            ]
            if name_like:
                needle = name_like.lower()
                full = [p for p in full if needle in p["name"].lower()]
            return {"success": True, "publishers": full}

        monkeypatch.setattr(
            indexexchange_mcp,
            "ix_list_marketplace_publishers",
            fake_list_marketplace_publishers,
        )

    @pytest.mark.asyncio
    async def test_returns_all_substring_matches_with_metadata(self):
        result = await indexexchange_mcp.ix_find_publisher_by_name(marketplace_account_id=1499155, name="Weather")
        assert result["success"] is True
        assert result["match_count"] == 3
        assert result["strategy"] == "substring_name"
        legacy_ids = sorted(c["legacyAccountID"] for c in result["candidates"])
        assert legacy_ids == [182970, 184272, 189342]
        # currency trade-off context is carried through
        currencies = {c["legacyAccountID"]: c["currency"] for c in result["candidates"]}
        assert currencies[189342] == "AUD"

    @pytest.mark.asyncio
    async def test_no_match_returns_empty(self):
        result = await indexexchange_mcp.ix_find_publisher_by_name(marketplace_account_id=1499155, name="Nonexistent")
        assert result["success"] is True
        assert result["match_count"] == 0
        assert result["candidates"] == []

    @pytest.mark.asyncio
    async def test_exact_match_beats_substring(self, monkeypatch):
        async def fake_list(*_, **__):
            return {
                "success": True,
                "publishers": [
                    {"accountID": 1, "legacyAccountID": 100, "name": "ACME", "accountStatus": "A"},
                    {"accountID": 2, "legacyAccountID": 101, "name": "ACME Holdings", "accountStatus": "A"},
                ],
            }

        monkeypatch.setattr(indexexchange_mcp, "ix_list_marketplace_publishers", fake_list)
        result = await indexexchange_mcp.ix_find_publisher_by_name(marketplace_account_id=1, name="ACME")
        assert result["strategy"] == "exact_name_or_id"
        assert result["match_count"] == 1
        assert result["candidates"][0]["legacyAccountID"] == 100


class TestExternalDealIdPassthrough:
    """`external_deal_id` flows through the high-level tool to
    ix_create_marketplace_deal so callers don't have to drop to the
    lower-level tool just to set the upstream order ID."""

    @pytest.mark.asyncio
    async def test_external_deal_id_forwarded_to_create(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
            external_deal_id="TWC-IX-Kenvue-Inc-1443746",
        )
        assert stub_dependencies["external_deal_id"] == "TWC-IX-Kenvue-Inc-1443746"

    @pytest.mark.asyncio
    async def test_external_deal_id_omitted_when_caller_passes_none(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
        )
        # The high-level tool forwards None; ix_create_marketplace_deal then
        # auto-generates the externalDealID.
        assert stub_dependencies["external_deal_id"] is None


class TestExcludedSegmentNames:
    @pytest.mark.asyncio
    async def test_excluded_segments_become_none_of_set(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
            segment_names=["Weather Targeting > Absolute > Current > Sunny"],
            excluded_segment_names=["Weather Block List"],
        )
        targeting = stub_dependencies["targeting"]
        segment_object = next(t for t in targeting if t["keyName"] == "segmentid")
        operators = [s["operator"] for s in segment_object["sets"]]
        assert "ANY_OF" in operators and "NONE_OF" in operators
        none_of = next(s for s in segment_object["sets"] if s["operator"] == "NONE_OF")
        assert none_of["values"] == [{"value": "308129"}]
        any_of = next(s for s in segment_object["sets"] if s["operator"] == "ANY_OF")
        assert any_of["values"] == [{"value": "238564"}]

    @pytest.mark.asyncio
    async def test_segment_targeting_uses_segmentid_keyname_not_audience_segment(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
            segment_names=["Weather Targeting > Absolute > Current > Sunny"],
        )
        targeting = stub_dependencies["targeting"]
        segment_objects = [t for t in targeting if "segment" in t.get("keyName", "").lower()]
        assert len(segment_objects) == 1
        # Must be the API-accepted "segmentid" name, not "Audience Segment"
        # (which the resolver would otherwise return for the `im_segments`
        # key — which rejects exclusion at the API level).
        assert segment_objects[0]["keyName"] == "segmentid"


class TestDmaCodes:
    @pytest.mark.asyncio
    async def test_dma_codes_build_zipcode_targeting_object(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
            dma_codes=["602"],  # Chicago
        )
        targeting = stub_dependencies["targeting"]
        zipcode_object = next(t for t in targeting if t["keyName"] == "ZipCode")
        assert zipcode_object["targetingKeyID"] == 781
        assert zipcode_object["sets"][0]["values"] == [{"value": "602"}]
        assert zipcode_object["sets"][0]["operator"] == "ANY_OF"

    @pytest.mark.asyncio
    async def test_multiple_dma_codes_dedupe_and_preserve_order(self, stub_dependencies):
        await indexexchange_mcp.ix_execute_deal_from_prompt_inputs(
            account_id=1499155,
            name="Test Deal",
            start_date="2026-05-04",
            floor=0.10,
            dsp_name="Viant",
            seat_name="2968",
            end_date="2026-12-31",
            dma_codes=["602", "501", "602"],  # Chicago, NYC, Chicago dupe
        )
        targeting = stub_dependencies["targeting"]
        zipcode_object = next(t for t in targeting if t["keyName"] == "ZipCode")
        values = [v["value"] for v in zipcode_object["sets"][0]["values"]]
        assert values == ["602", "501"]
