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
		{DefaultCoreModel, "Z.AI", false},
		{"z-ai/glm-4.6", "Z.AI", false},
		{"~z-ai/glm-latest", "Z.AI", false}, // `~` alias inherits the pin
		{DefaultMaxModel, "Anthropic", false},
		{"google/gemini-3-flash-preview", "Google", true},
		{"deepseek/deepseek-v3.1", "", false},
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
