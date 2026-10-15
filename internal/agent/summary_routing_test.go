package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// floorSlug is a slug with a per-slug serving-precision floor
// (agentcore precisionFloorSlugs) and no family pin.
const floorSlug = "qwen/qwen3.8-27b"

// providerBodyRecorder records the "provider" block of every chat-completions
// request before handing it to the fake LLM.
type providerBodyRecorder struct {
	next      http.Handler
	mu        sync.Mutex
	providers []json.RawMessage // nil entry = request had no provider block
}

func (c *providerBodyRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Provider json.RawMessage `json:"provider"`
	}
	if json.Unmarshal(body, &req) == nil {
		c.mu.Lock()
		c.providers = append(c.providers, req.Provider)
		c.mu.Unlock()
	}
	c.next.ServeHTTP(w, r)
}

func (c *providerBodyRecorder) only(t *testing.T) json.RawMessage {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.providers) != 1 {
		t.Fatalf("recorded %d model requests, want 1", len(c.providers))
	}
	return c.providers[0]
}

// A manual summarize on a floored slug must reach OpenRouter with the same
// provider policy a turn on that slug carries. Before this, the summarize call
// had no provider block, so a long conversation's summary could be served at
// fp4 on a slug whose turns were held to fp8.
func TestSummarize_SendsTheSlugsRoutingPolicy(t *testing.T) {
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("the brief")}})
	rec := &providerBodyRecorder{next: fake.Handler()}
	mgr := newFakeLLMManagerWithHandler(t, rec, nil)

	history := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "tier these domains"}),
		mustEntry("assistant", "text", TextContent{Text: "done: 412 premium"}),
	}
	if _, err := mgr.Summarize(context.Background(), SummarizeInput{History: history, Model: floorSlug}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	raw := rec.only(t)
	var got struct {
		Quantizations  []string `json:"quantizations"`
		AllowFallbacks *bool    `json:"allow_fallbacks"`
		Order          []string `json:"order"`
		Only           []string `json:"only"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("summarize request provider block = %s, want the precision floor", raw)
	}
	allowed := map[string]bool{}
	for _, q := range got.Quantizations {
		allowed[q] = true
	}
	if !allowed["fp8"] || allowed["fp4"] || allowed["unknown"] {
		t.Errorf("summarize request quantizations = %v, want the fp8-and-above floor", got.Quantizations)
	}
	if got.AllowFallbacks == nil || !*got.AllowFallbacks || len(got.Order) != 0 || len(got.Only) != 0 {
		t.Errorf("summarize request provider = %s, want the floor alone (fallbacks on, no order/only)", raw)
	}
}

// A slug with no routing policy keeps sending no provider block, so the change
// leaves unpinned families' summarize requests as they were.
func TestSummarize_UnpinnedSlugSendsNoProviderBlock(t *testing.T) {
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("the brief")}})
	rec := &providerBodyRecorder{next: fake.Handler()}
	mgr := newFakeLLMManagerWithHandler(t, rec, nil)

	history := []HistoryEntry{mustEntry("user", "text", TextContent{Text: "hello"})}
	if _, err := mgr.Summarize(context.Background(), SummarizeInput{History: history, Model: "mistralai/mixtral-8x7b"}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if raw := rec.only(t); len(raw) != 0 && string(raw) != "null" {
		t.Errorf("unpinned summarize request provider block = %s, want none", raw)
	}
}

// optionsCapturingModel answers like itMockModel and records each Generate
// call's provider options, under a slug of the test's choosing.
type optionsCapturingModel struct {
	itMockModel
	slug string
	omu  sync.Mutex
	opts []fantasy.ProviderOptions
}

func (m *optionsCapturingModel) Model() string { return m.slug }

func (m *optionsCapturingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.omu.Lock()
	m.opts = append(m.opts, call.ProviderOptions)
	m.omu.Unlock()
	return m.itMockModel.Generate(ctx, call)
}

// Both compaction summarizers (chat and scheduled) buy their summary from the
// run's own model, so they must carry that model's routing policy too.
// Compaction fires on exactly the long runs where a degraded summary does the
// most damage.
func TestCompactionSummarizers_CarryTheModelsRoutingPolicy(t *testing.T) {
	builders := map[string]func(fantasy.LanguageModel) func(context.Context, agentcore.CompactionSummarizeInput) fantasy.Message{
		"interactive": func(m fantasy.LanguageModel) func(context.Context, agentcore.CompactionSummarizeInput) fantasy.Message {
			return buildInteractiveCompactionSummarizer(TurnConfig{Model: m})
		},
		"scheduled": buildScheduledCompactionSummarizer,
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			model := &optionsCapturingModel{itMockModel: itMockModel{generateText: "condensed"}, slug: floorSlug}
			in := agentcore.CompactionSummarizeInput{
				Droppable: []fantasy.Message{fantasy.NewUserMessage("step 1: tiered 4,112 domains")},
			}
			build(model)(context.Background(), in)

			model.omu.Lock()
			defer model.omu.Unlock()
			if len(model.opts) != 1 {
				t.Fatalf("summarizer made %d model calls, want 1", len(model.opts))
			}
			or, ok := model.opts[0][openrouter.Name].(*openrouter.ProviderOptions)
			if !ok || or.Provider == nil {
				t.Fatalf("summarizer call options = %#v, want an OpenRouter provider policy", model.opts[0])
			}
			want := agentcore.RoutingProviderOptions(floorSlug)[openrouter.Name].(*openrouter.ProviderOptions).Provider
			if len(or.Provider.Quantizations) != len(want.Quantizations) {
				t.Fatalf("summarizer quantizations = %v, want %v", or.Provider.Quantizations, want.Quantizations)
			}
			for i := range want.Quantizations {
				if or.Provider.Quantizations[i] != want.Quantizations[i] {
					t.Errorf("summarizer quantizations = %v, want %v", or.Provider.Quantizations, want.Quantizations)
					break
				}
			}
		})
	}
}
