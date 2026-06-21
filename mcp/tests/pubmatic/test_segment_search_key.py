"""Tests for the PubMatic audience search-key candidate logic.

Background: PubMatic's `/v1/audience/buyerInsights/audiences` searchKey
behaves like a substring match capped at 100 results per page. The
earlier "single longest token" strategy picked generic nouns like
"Enthusiasts" or "Shoppers" that saturate the page with unrelated
audiences and crowd out the actual target. The candidate logic now
returns distinctive tokens first and falls back to generic nouns only
if nothing more specific is available.
"""

import pytest
from pubmatic_mcp import (
    _audience_search_key,
    _audience_search_key_candidates,
    _resolve_audience_segments,
)


@pytest.fixture(autouse=True)
def _isolate_pubmatic_disk_cache(monkeypatch: pytest.MonkeyPatch, tmp_path_factory):
    """Each test gets a fresh disk-cache directory so we never pick up live-run
    artifacts. The audience-search helper is disk-cached on the searchKey,
    which would otherwise short-circuit the FakeClient stubs below."""
    cache_root = tmp_path_factory.mktemp("pmcache_segment")
    monkeypatch.setenv("XDG_CACHE_HOME", str(cache_root))
    yield


class TestAudienceSearchKeyCandidates:
    def test_picks_distinctive_token_over_generic_noun(self):
        # "Cars & Auto_Chrysler Enthusiasts" used to pick "Enthusiasts"
        # (longest), saturating the page. Now "Chrysler" leads.
        candidates = _audience_search_key_candidates("Cars & Auto_Chrysler Enthusiasts")
        assert candidates[0] == "Chrysler"
        # Generic noun "Enthusiasts" only ranks after every distinctive token.
        full = _audience_search_key_candidates("Cars & Auto_Chrysler Enthusiasts", max_candidates=10)
        assert full.index("Chrysler") < full.index("Enthusiasts")
        assert full.index("Cars") < full.index("Enthusiasts")
        assert full.index("Auto") < full.index("Enthusiasts")

    def test_distinctive_tokens_sorted_by_length(self):
        candidates = _audience_search_key_candidates("In-Market: Vehicle Purchase Intent")
        # Stop words "In", "Market", "Intent" drop. "Purchase" (8) > "Vehicle" (7).
        assert candidates == ["Purchase", "Vehicle"]

    def test_generic_nouns_appear_after_distinctive(self):
        candidates = _audience_search_key_candidates("Auto Service Maintenance Shoppers", max_candidates=10)
        # "Shoppers" is generic and trails the distinctive tokens even
        # though it is longer than "Auto".
        assert candidates.index("Shoppers") > candidates.index("Maintenance")
        assert candidates.index("Shoppers") > candidates.index("Service")
        assert candidates.index("Shoppers") > candidates.index("Auto")

    def test_falls_back_to_generic_when_no_distinctive_token(self):
        candidates = _audience_search_key_candidates("Shoppers Buyers")
        # Both generic; ordering is by length within the generic group.
        assert candidates == ["Shoppers", "Buyers"]

    def test_max_candidates_caps_list(self):
        candidates = _audience_search_key_candidates("Alpha Bravo Charlie Delta Echo Foxtrot", max_candidates=2)
        assert len(candidates) == 2

    def test_dedupes_repeated_tokens(self):
        candidates = _audience_search_key_candidates("Chrysler Chrysler Enthusiasts")
        assert candidates.count("Chrysler") == 1

    def test_empty_input_returns_safe_fallback(self):
        candidates = _audience_search_key_candidates("")
        assert candidates == [] or candidates == [""]

    def test_backward_compat_shim_returns_first_candidate(self):
        # _audience_search_key still returns a single string; tests that
        # depend on the old surface keep working.
        assert _audience_search_key("In-Market: Vehicle Purchase Intent") == "Purchase"


class TestResolveAudienceSegmentsRetriesOnSaturation:
    @pytest.mark.asyncio
    async def test_retries_with_next_candidate_when_first_fails_to_match(self, monkeypatch: pytest.MonkeyPatch):
        """Real-world bug: 'Cars & Auto_Chrysler Enthusiasts' returned 100 generic
        Enthusiasts and the actual Chrysler audience was missed. With the
        retry, "Chrysler" is tried first and finds the match."""

        captured_search_keys: list[str] = []

        class FakeClient:
            async def list_buyer_audiences(self, *, search_key, **_kwargs):
                captured_search_keys.append(search_key)
                if search_key.lower() == "chrysler":
                    return {
                        "items": [
                            {
                                "audienceId": 5500,
                                "audienceName": "Cars & Auto_Chrysler Enthusiasts",
                            },
                        ]
                    }
                if search_key.lower() == "enthusiasts":
                    # Saturated page of unrelated audiences.
                    return {
                        "items": [{"audienceId": i, "audienceName": f"Generic Enthusiasts {i}"} for i in range(100)]
                    }
                return {"items": []}

        import pubmatic_mcp

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        ids, warnings = await _resolve_audience_segments(
            ["Cars & Auto_Chrysler Enthusiasts"],
            logged_in_owner_type_id=7,
        )

        assert ids == [5500]
        assert warnings == []
        # Distinctive token comes first; generic noun would only run as fallback.
        assert captured_search_keys[0] == "Chrysler"

    @pytest.mark.asyncio
    async def test_warning_lists_all_attempted_search_keys_when_unresolved(self, monkeypatch: pytest.MonkeyPatch):
        captured_search_keys: list[str] = []

        class FakeClient:
            async def list_buyer_audiences(self, *, search_key, **_kwargs):
                captured_search_keys.append(search_key)
                return {"items": [{"audienceId": 1, "audienceName": "Unrelated"}]}

        import pubmatic_mcp

        monkeypatch.setattr(pubmatic_mcp, "get_pubmatic_client", lambda: FakeClient())

        ids, warnings = await _resolve_audience_segments(
            ["Atlantis Mariners"],
            logged_in_owner_type_id=7,
        )

        assert ids == []
        assert len(warnings) == 1
        # Both candidates were tried before giving up.
        assert "Atlantis" in captured_search_keys
        assert "Mariners" in captured_search_keys
        # Warning surfaces both attempted keys so the trader can debug.
        assert "Atlantis" in warnings[0]
        assert "Mariners" in warnings[0]
