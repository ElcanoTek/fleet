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
		{DefaultCoreModel, "Google", true},
		{"z-ai/glm-5.2", "Z.AI", false},
		{"z-ai/glm-4.6", "Z.AI", false},
		{"~z-ai/glm-latest", "Z.AI", false}, // `~` alias inherits the pin
		{"deepseek/deepseek-v4-flash-0731", "DeepSeek", false},
		// The strong tier has NO pin: OpenRouter serves it from xAI alone, so
		// there is no provider spread to collapse and the prompt cache is already
		// single-upstream. openai/ still pins for any other OpenAI slug.
		{DefaultMaxModel, "", false},
		{"openai/gpt-5.4", "OpenAI", false},
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
// exactly two ways to satisfy it — a strict pin (one upstream, so there is no
// pool to vary) or a soft pin carrying a serving-precision floor. This asserts
// the property rather than the current lab, so swapping the default cannot
// quietly drop the guarantee.
func TestDefaultCoreModelCannotBeServedAtArbitraryPrecision(t *testing.T) {
	p := upstreamPinFor(DefaultCoreModel)
	if p == nil {
		t.Fatalf("upstreamPinFor(%q) = nil: the default must be pinned", DefaultCoreModel)
	}
	strict := len(p.Only) > 0 && p.AllowFallbacks != nil && !*p.AllowFallbacks
	if strict {
		return // one upstream: no pool, so no precision to vary
	}
	if len(p.Quantizations) == 0 {
		t.Errorf("upstreamPinFor(%q) = %+v: a soft-pinned default needs a serving-precision floor", DefaultCoreModel, p)
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
		DefaultCoreModel:    "Google",
		"deepseek/v3":       "DeepSeek",
		"~z-ai/glm-latest":  "Z.AI",
		"openai/gpt-5.4":    "OpenAI",
		DefaultMaxModel:     "", // strong tier is unpinned
		"mistralai/mixtral": "",
	}
	for slug, want := range cases {
		if got := preferredUpstreamFor(slug); got != want {
			t.Errorf("preferredUpstreamFor(%q) = %q, want %q", slug, got, want)
		}
	}
}
