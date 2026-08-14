package agentcore

import "testing"

// Pins the canonical-upstream table: the default core tier (z-ai/glm-5.2, no
// :nitro variant) is soft-pinned to the Z.AI upstream so OpenRouter's implicit
// per-upstream prompt cache keeps hitting across calls, and unpinned families
// still return nil (OpenRouter default routing).
func TestUpstreamPinFor(t *testing.T) {
	cases := []struct {
		slug      string
		wantOrder string // "" = expect nil pin
		strict    bool
	}{
		{DefaultCoreModel, "DeepSeek", false},
		{"z-ai/glm-5.2", "Z.AI", false},
		{"z-ai/glm-4.6", "Z.AI", false},
		{"~z-ai/glm-latest", "Z.AI", false}, // `~` alias inherits the pin
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
