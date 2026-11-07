package admincli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEvaluateCacheRowStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                              string
		turns, promptTokens, cachedTokens int64
		wantHitRate                       float64
		wantUncached                      int64
		wantStatus                        string
	}{
		{
			name:        "no data when 0 turns",
			turns:       0,
			wantHitRate: 0,
			wantStatus:  "no_data",
		},
		{
			name:         "broken when >=3 turns, >=10k prompt tokens, and 0 cached",
			turns:        4,
			promptTokens: 25_000,
			cachedTokens: 0,
			wantHitRate:  0,
			wantUncached: 25_000,
			wantStatus:   "broken",
		},
		{
			name:         "low when >=3 turns, >=10k prompt tokens, and <20% hit rate",
			turns:        5,
			promptTokens: 50_000,
			cachedTokens: 5_000,
			wantHitRate:  10.0,
			wantUncached: 45_000,
			wantStatus:   "low",
		},
		{
			name:         "ok when healthy hit rate",
			turns:        10,
			promptTokens: 100_000,
			cachedTokens: 78_000,
			wantHitRate:  78.0,
			wantUncached: 22_000,
			wantStatus:   "ok",
		},
		{
			name:         "single cold turn is ok, not broken",
			turns:        1,
			promptTokens: 15_000,
			cachedTokens: 0,
			wantHitRate:  0,
			wantUncached: 15_000,
			wantStatus:   "ok",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, uncached, status := evaluateCacheRowStatus(tc.turns, tc.promptTokens, tc.cachedTokens)
			if hit != tc.wantHitRate {
				t.Errorf("hitRate = %.1f, want %.1f", hit, tc.wantHitRate)
			}
			if uncached != tc.wantUncached {
				t.Errorf("uncached = %d, want %d", uncached, tc.wantUncached)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
		})
	}
}

func TestCacheStatsValidationAndRender(t *testing.T) {
	if code := runCacheStats([]string{"stray"}, &bytes.Buffer{}); code != 1 {
		t.Errorf("stray arg exit = %d, want 1", code)
	}
	if code := runCacheStats([]string{"--days", "0"}, &bytes.Buffer{}); code != 1 {
		t.Errorf("--days 0 exit = %d, want 1", code)
	}
	if code := runCacheStats([]string{"--by", "invalid"}, &bytes.Buffer{}); code != 1 {
		t.Errorf("--by invalid exit = %d, want 1", code)
	}

	rep := &CacheStatsReport{
		Status:     "ok",
		Summary:    "75.0% cache hit rate across 4 turns (30000 cached / 40000 prompt tokens, 10000 cache misses, $0.12)",
		WindowDays: 14,
		From:       "2026-10-20",
		To:         "2026-11-03",
		Totals: CacheStatsRow{
			Key:             "TOTAL",
			Turns:           4,
			CostUSD:         0.12,
			PromptTokens:    40_000,
			CachedTokens:    30_000,
			UncachedTokens:  10_000,
			CacheHitRatePct: 75.0,
			Status:          "ok",
		},
		ByDay: []CacheStatsRow{
			{
				Key:             "2026-11-03",
				Turns:           4,
				CostUSD:         0.12,
				PromptTokens:    40_000,
				CachedTokens:    30_000,
				UncachedTokens:  10_000,
				CacheHitRatePct: 75.0,
				Status:          "ok",
			},
		},
		ByUser: []CacheStatsRow{
			{
				Key:             "brad@elcanotek.com",
				Turns:           4,
				CostUSD:         0.12,
				PromptTokens:    40_000,
				CachedTokens:    30_000,
				UncachedTokens:  10_000,
				CacheHitRatePct: 75.0,
				Status:          "ok",
			},
		},
	}

	var buf bytes.Buffer
	if err := renderCacheStatsReport(&buf, rep, "all"); err != nil {
		t.Fatalf("renderCacheStatsReport: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Status: [OK]",
		"By Date (UTC)",
		"2026-11-03",
		"By User",
		"brad@elcanotek.com",
		"75.0%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
}
