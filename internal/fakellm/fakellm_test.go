package fakellm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// rewriteRoundTripper swaps every outbound request's scheme+host to the fake's
// origin, mirroring fleet's OPENROUTER_BASE_URL seam (internal/agentcore/
// provider.go). This drives the REAL fantasy wire path against the fake.
type rewriteRoundTripper struct {
	target string
}

func (rt *rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(rt.target)
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func httpClientWith(rt http.RoundTripper) *http.Client { return &http.Client{Transport: rt} }

func newProvider(t *testing.T, base string) fantasy.Provider {
	t.Helper()
	p, err := openrouter.New(
		openrouter.WithAPIKey("sk-or-fake"),
		openrouter.WithHTTPClient(httpClientWith(&rewriteRoundTripper{target: base})),
	)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

func TestFakeLLM_TextTurn(t *testing.T) {
	srv := fakellm.New().Scenario("t", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("hello world from fake"),
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	provider := newProvider(t, ts.URL)
	model, err := provider.LanguageModel(context.Background(), "anthropic/claude-opus-4.8")
	if err != nil {
		t.Fatalf("language model: %v", err)
	}

	agent := fantasy.NewAgent(model)
	res, err := agent.Generate(context.Background(), fantasy.AgentCall{
		Prompt: "say hi [[scenario:t]]",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := res.Response.Content.Text()
	if !strings.Contains(got, "hello world from fake") {
		t.Fatalf("text turn: got %q, want it to contain the canned reply", got)
	}
}

func TestFakeLLM_Echo(t *testing.T) {
	// An "[[echo:TEXT]]" marker streams back exactly TEXT, turn-independently and
	// without a registered scenario — the deterministic, distinct-reply seam the
	// live conversation-management specs use to give each chat its own title.
	srv := fakellm.New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	provider := newProvider(t, ts.URL)
	model, err := provider.LanguageModel(context.Background(), "anthropic/claude-opus-4.8")
	if err != nil {
		t.Fatalf("language model: %v", err)
	}

	agent := fantasy.NewAgent(model)
	res, err := agent.Generate(context.Background(), fantasy.AgentCall{
		Prompt: "make a chat [[echo:Keep this chat]]",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := strings.TrimSpace(res.Response.Content.Text())
	if got != "Keep this chat" {
		t.Fatalf("echo: got %q, want exactly %q", got, "Keep this chat")
	}
	if srv.Hits("__echo__") < 1 {
		t.Fatalf("echo: expected the echo path to be hit, got %d", srv.Hits("__echo__"))
	}
}

func TestFakeLLM_ToolLoop(t *testing.T) {
	// turn 0 → call the "echo" tool; turn 1 → final text. Proves the full
	// multi-turn loop works over the real wire: the fake emits a tool_call,
	// fantasy executes the registered tool, appends the tool result, re-calls,
	// and the fake (now seeing one assistant turn) returns the final text.
	srv := fakellm.New().Scenario("loop", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(fakellm.ToolCall{ID: "c1", Name: "echo", Arguments: `{"text":"PING"}`}),
		fakellm.TextStep("the tool returned its echo, loop complete"),
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	provider := newProvider(t, ts.URL)
	model, err := provider.LanguageModel(context.Background(), "anthropic/claude-opus-4.8")
	if err != nil {
		t.Fatalf("language model: %v", err)
	}

	var toolRan bool
	echo := fantasy.NewAgentTool("echo", "echoes input",
		func(_ context.Context, p struct {
			Text string `json:"text"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			toolRan = true
			return fantasy.NewTextResponse("ECHO:" + p.Text), nil
		})

	agent := fantasy.NewAgent(model, fantasy.WithTools(echo))
	res, err := agent.Generate(context.Background(), fantasy.AgentCall{
		Prompt: "do the loop [[scenario:loop]]",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !toolRan {
		t.Fatalf("tool loop: the echo tool was never executed")
	}
	got := res.Response.Content.Text()
	if !strings.Contains(got, "loop complete") {
		t.Fatalf("tool loop: final text %q missing expected marker", got)
	}
	if srv.Hits("loop") < 2 {
		t.Fatalf("tool loop: expected >=2 requests (tool turn + final turn), got %d", srv.Hits("loop"))
	}
}

func TestFakeLLM_InjectedError(t *testing.T) {
	srv := fakellm.New().Scenario("boom", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.StatusStep(500),
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	provider := newProvider(t, ts.URL)
	model, err := provider.LanguageModel(context.Background(), "anthropic/claude-opus-4.8")
	if err != nil {
		t.Fatalf("language model: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := fantasy.NewAgent(model)
	_, err = agent.Generate(ctx, fantasy.AgentCall{
		Prompt: "trigger error [[scenario:boom]]",
	})
	if err == nil {
		t.Fatalf("injected 500: expected an error, got nil")
	}
}
