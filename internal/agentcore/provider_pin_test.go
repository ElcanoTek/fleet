package agentcore

import "testing"

// Pins the canonical-upstream table. Families served by more than one upstream
// are pinned so OpenRouter's implicit per-upstream prompt cache keeps hitting
// across calls — softly (Order, graceful fallback) where a second upstream
// exists, strictly (Only) where the lab serves the family alone. Unpinned
// families still return nil (OpenRouter default routing).
func TestUpstreamPinFor(t *testing.T) {
	cases := []struct {
		slug      string
		wantOrder string // "" = expect nil pin
		strict    bool
	}{
		// The everyday default: Google serves this family alone, so the pin is
		// strict — there is no second upstream to fall back to.
		{DefaultCoreModel, "OpenAI", false}, // everyday default: soft pin to OpenAI, official-weights pool
		{"z-ai/glm-5.2", "Z.AI", false},
		{"z-ai/glm-4.6", "Z.AI", false},
		{"~z-ai/glm-latest", "Z.AI", false}, // `~` alias inherits the pin
		{"deepseek/deepseek-v4-flash-0731", "DeepSeek", false},
		// The strong tier is back in the openai/ family, which IS pinned (soft:
		// Order, fallbacks allowed) — so the escalation path gets the same
		// per-upstream prompt-cache locality as every other OpenAI slug. The
		// second row keeps the family's pin covered independently of whichever
		// slug currently holds the tier.
		{DefaultMaxModel, "Anthropic", false}, // strong tier: soft pin to Anthropic
		{"openai/gpt-5.4", "OpenAI", false},
		// x-ai/ has no entry: it held the strong tier for one release and was
		// deliberately left unpinned (xAI is its only upstream).
		{"x-ai/grok-4.6", "", false},
		{"google/gemini-3-flash-preview", "Google", true},
		// The whole DeepSeek family pins to the first-party upstream: OpenRouter
		// serves it from 28 endpoints spanning 131K–1M context and fp4–fp8, so an
		// unpinned route varies in window and quality between runs.
		{"deepseek/deepseek-v3.1", "DeepSeek", false},
		{"x-ai/grok-4", "", false}, // "x-ai/" must not collide with "z-ai/"
	}
	for _, tc := range cases {
		p := upstreamPinFor(tc.slug)
		if tc.wantOrder == "" {
			if p != nil {
				t.Errorf("upstreamPinFor(%q) = %+v, want nil", tc.slug, p)
			}
			continue
		}
		if p == nil {
			t.Errorf("upstreamPinFor(%q) = nil, want pin to %q", tc.slug, tc.wantOrder)
			continue
		}
		if tc.strict {
			if len(p.Only) != 1 || p.Only[0] != tc.wantOrder || p.AllowFallbacks == nil || *p.AllowFallbacks {
				t.Errorf("upstreamPinFor(%q) = %+v, want strict Only=[%q]", tc.slug, p, tc.wantOrder)
			}
			continue
		}
		if len(p.Order) != 1 || p.Order[0] != tc.wantOrder || p.AllowFallbacks == nil || !*p.AllowFallbacks {
			t.Errorf("upstreamPinFor(%q) = %+v, want soft Order=[%q]", tc.slug, p, tc.wantOrder)
		}
	}
}

// Pins the serving-precision floor. The DeepSeek family is the recommended
// everyday default and is served by a pool spanning fp4-fp8, so the soft pin's
// fallback path must carry a quantization allow-list that excludes every level
// below fp8 (and "unknown", which cannot be shown to clear the floor).
// Families whose pool does not mix precisions carry no filter, so their
// requests stay byte-identical to before the floor existed.
func TestUpstreamPinQuantizationFloor(t *testing.T) {
	belowFloor := []string{"fp4", "fp6", "int4", "int8", "unknown"}

	for _, slug := range []string{"deepseek/deepseek-v4-flash-0731", "deepseek/deepseek-v3.1", "~deepseek/deepseek-v4"} {
		p := upstreamPinFor(slug)
		if p == nil {
			t.Fatalf("upstreamPinFor(%q) = nil, want a pin", slug)
		}
		if len(p.Quantizations) == 0 {
			t.Errorf("upstreamPinFor(%q): no quantization floor; a soft pin may fall back to any precision in the pool", slug)
			continue
		}
		allowed := make(map[string]bool, len(p.Quantizations))
		for _, q := range p.Quantizations {
			allowed[q] = true
		}
		if !allowed["fp8"] {
			t.Errorf("upstreamPinFor(%q): floor %v excludes fp8, the first-party serving precision — the preferred route would be unroutable", slug, p.Quantizations)
		}
		for _, q := range belowFloor {
			if allowed[q] {
				t.Errorf("upstreamPinFor(%q): floor %v admits %q, which is below fp8", slug, p.Quantizations, q)
			}
		}
	}

	// Unmixed families keep no filter — the floor is targeted, not global.
	for _, slug := range []string{"z-ai/glm-5.2", "openai/gpt-5.4", "google/gemini-3-flash-preview", DefaultCoreModel} {
		p := upstreamPinFor(slug)
		if p == nil {
			t.Fatalf("upstreamPinFor(%q) = nil, want a pin", slug)
		}
		if len(p.Quantizations) != 0 {
			t.Errorf("upstreamPinFor(%q).Quantizations = %v, want none", slug, p.Quantizations)
		}
	}
}

// Whichever family holds the default slot, it must never be servable at an
// arbitrary precision from an arbitrary upstream: that is the failure mode that
// reads as "the model got worse" rather than as a routing event. There are
// exactly three ways to satisfy it, and every one of them is a property of the
// REQUEST rather than a claim in a comment — a strict pin (one upstream, so
// there is no pool to vary), a closed `Only` allow-list (the pool cannot grow
// under us), or a serving-precision floor. This reads the emitted policy, not
// the tables behind it, so swapping the default cannot quietly drop the
// guarantee.
func TestDefaultCoreModelCannotBeServedAtArbitraryPrecision(t *testing.T) {
	for _, slug := range []string{DefaultCoreModel, DefaultMaxModel} {
		p := upstreamPinFor(slug)
		if p == nil {
			t.Fatalf("upstreamPinFor(%q) = nil: a default-tier slug must be pinned", slug)
		}
		if len(p.Only) > 0 {
			continue // closed set: strict (one upstream) or a validated official-weights pool
		}
		if len(p.Quantizations) == 0 {
			t.Errorf("upstreamPinFor(%q) = %+v: a soft-pinned default over an open pool needs a serving-precision floor or an Only allow-list", slug, p)
		}
	}
}

// The official-pool exemption must be ENFORCED, not snapshotted (#1589). A
// listed slug's request carries its validated pool as provider.only, so an
// endpoint OpenRouter adds to that pool later — a third party, or a quantized
// serving — cannot inherit the exemption between endpoint checks. The vendor
// stays first in Order (prompt-cache locality) and fallbacks stay on, so the
// cloud resellers of the official weights remain reachable: this closes the
// pool, it does not shrink it to one endpoint.
func TestOfficialPoolExemptionIsEnforcedAtRequestTime(t *testing.T) {
	for slug, pool := range officialPoolSlugs {
		if pool.checked == "" {
			t.Errorf("officialPoolSlugs[%q]: no check date; the pool must be recorded with the endpoint list in hand", slug)
		}
		for _, form := range []string{slug, "~" + slug} {
			p := upstreamPinFor(form)
			if p == nil {
				t.Fatalf("upstreamPinFor(%q) = nil, want a pin", form)
			}
			if len(p.Only) != len(pool.providers) {
				t.Errorf("upstreamPinFor(%q).Only = %v, want the validated pool %v", form, p.Only, pool.providers)
				continue
			}
			for i, want := range pool.providers {
				if p.Only[i] != want {
					t.Errorf("upstreamPinFor(%q).Only[%d] = %q, want %q", form, i, p.Only[i], want)
				}
			}
			if p.AllowFallbacks == nil || !*p.AllowFallbacks {
				t.Errorf("upstreamPinFor(%q) = %+v: the allow-list closes the pool, it must not disable fallbacks onto the resellers in it", form, p)
			}
			// The preferred upstream must be inside the set it may route to,
			// or the allow-list makes the cache-warm route unroutable.
			if len(p.Order) != 1 {
				t.Fatalf("upstreamPinFor(%q).Order = %v, want the vendor alone", form, p.Order)
			}
			var found bool
			for _, name := range p.Only {
				if name == p.Order[0] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("upstreamPinFor(%q): preferred upstream %q is not in Only=%v", form, p.Order[0], p.Only)
			}
		}
	}

	// Siblings do not inherit the allow-list any more than they inherit the
	// exemption: an unchecked slug keeps OpenRouter's open pool (and, where the
	// family mixes precisions, the floor).
	for _, slug := range []string{"openai/gpt-5.6-sol", "anthropic/claude-sonnet-5", "z-ai/glm-5.2"} {
		p := upstreamPinFor(slug)
		if p == nil {
			t.Fatalf("upstreamPinFor(%q) = nil, want a pin", slug)
		}
		if len(p.Only) != 0 {
			t.Errorf("upstreamPinFor(%q).Only = %v, want none: only validated slugs carry an allow-list", slug, p.Only)
		}
	}
}

// The allow-list is handed to the request builder like the floor is, so it must
// not share backing state with the table either.
func TestOfficialPoolAllowlistNotAliased(t *testing.T) {
	const listed = "anthropic/claude-opus-5" // any slug with a validated pool
	first := upstreamPinFor(listed)
	if first == nil || len(first.Only) == 0 {
		t.Fatalf("expected an allow-list on %q", listed)
	}
	want := len(first.Only)
	head := first.Only[0]
	first.Only = append(first.Only, "Some Third Party")
	first.Only[0] = "mutated"

	second := upstreamPinFor(listed)
	if len(second.Only) != want {
		t.Fatalf("allow-list length = %d after a caller mutated an earlier pin, want %d", len(second.Only), want)
	}
	if second.Only[0] != head {
		t.Errorf("allow-list[0] = %q after a caller mutated an earlier pin, want %q", second.Only[0], head)
	}
}

// The official-pool exemption is a claim about one slug's whole endpoint pool
// and must stay per slug: a sibling in the same family (a future OpenAI model
// picked up by third-party hosts, or one nobody checked) must not inherit it,
// and families served by quantized third parties never qualify.
func TestOfficialPoolExemptionIsNarrow(t *testing.T) {
	for _, slug := range []string{"deepseek/deepseek-v4.1-flash", "z-ai/glm-5.2", "moonshotai/kimi-k2.6", "google/gemini-3.8-flash",
		"openai/gpt-5.6-sol", "openai/gpt-6-astra", "anthropic/claude-sonnet-5", "anthropic/claude-opus-4.8"} {
		if pinServesOfficialWeightsOnly(slug) {
			t.Errorf("%q must not be exempt: only validated slugs are", slug)
		}
	}
	for _, slug := range []string{"openai/gpt-5.6-luna-pro", "~openai/gpt-5.6-luna-pro", "anthropic/claude-opus-5"} {
		if !pinServesOfficialWeightsOnly(slug) {
			t.Errorf("%q should be exempt (validated endpoint pool)", slug)
		}
	}
	for slug := range officialPoolSlugs {
		if upstreamPinFor(slug) == nil {
			t.Errorf("%q is exempt from the floor but has no upstream pin at all", slug)
		}
	}
}

// The returned Provider must not share backing state with the table: it is
// handed to the request builder, so a caller appending to Quantizations would
// otherwise corrupt the floor for every later request in the process.
func TestUpstreamPinQuantizationsNotAliased(t *testing.T) {
	const floored = "deepseek/deepseek-v4-flash-0731" // any slug carrying a floor
	first := upstreamPinFor(floored)
	if first == nil || len(first.Quantizations) == 0 {
		t.Fatalf("expected a quantization floor on %q", floored)
	}
	want := len(first.Quantizations)
	first.Quantizations = append(first.Quantizations, "fp4")
	first.Quantizations[0] = "mutated"

	second := upstreamPinFor(floored)
	if len(second.Quantizations) != want {
		t.Fatalf("floor length = %d after a caller mutated an earlier pin, want %d", len(second.Quantizations), want)
	}
	if second.Quantizations[0] != "fp8" {
		t.Errorf("floor[0] = %q after a caller mutated an earlier pin, want %q", second.Quantizations[0], "fp8")
	}
}

// preferredUpstreamFor is the read side of the pin table used to detect a
// fallback; it must agree with upstreamPinFor on which family owns a slug.
func TestPreferredUpstreamFor(t *testing.T) {
	cases := map[string]string{
		DefaultCoreModel:    "OpenAI",
		"deepseek/v3":       "DeepSeek",
		"~z-ai/glm-latest":  "Z.AI",
		"openai/gpt-5.4":    "OpenAI",
		DefaultMaxModel:     "Anthropic", // strong tier: Claude Opus 5 on the anthropic/ soft pin
		"x-ai/grok-4.6":     "",          // xAI is unpinned: single upstream already
		"mistralai/mixtral": "",
	}
	for slug, want := range cases {
		if got := preferredUpstreamFor(slug); got != want {
			t.Errorf("preferredUpstreamFor(%q) = %q, want %q", slug, got, want)
		}
	}
}
