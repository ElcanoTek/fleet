package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// LIVE sub-agent test (#1043 follow-up). Guarded by FLEET_SUBAGENT_LIVE=1 plus a
// real OPENROUTER_API_KEY, so CI stays offline and deterministic (every other
// sub-agent test uses the fake-LLM seam). It exists because the failure it pins
// was invisible to the mocked suite: a real model, handed the parent's prompt
// and the scheduled finish gate, spent its whole budget hunting for
// protocols/self-audit.md instead of answering — the mock models simply stopped,
// so nothing caught it.
//
// Run it with:
//
//	FLEET_SUBAGENT_LIVE=1 OPENROUTER_API_KEY=… go test ./internal/agent/ \
//	    -tags fleet_host_executor -run TestLive_ChatDelegation -v
//
// FLEET_LIVE_MODEL overrides the model slug (default: a cheap fast one).
func liveModel(t *testing.T) (fantasy.LanguageModel, *agentcore.ModelResolver) {
	t.Helper()
	if os.Getenv("FLEET_SUBAGENT_LIVE") != "1" {
		t.Skip("set FLEET_SUBAGENT_LIVE=1 to run the live sub-agent tests")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Skip("OPENROUTER_API_KEY unset")
	}
	resolver, err := agentcore.NewModelResolver(key, agentcore.DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("model resolver: %v", err)
	}
	slug := strings.TrimSpace(os.Getenv("FLEET_LIVE_MODEL"))
	if slug == "" {
		slug = "deepseek/deepseek-chat-v3.1"
	}
	m, err := resolver.Resolve(context.Background(), slug)
	if err != nil {
		t.Fatalf("resolve %s: %v", slug, err)
	}
	return m, resolver
}

// liveObserver captures the turn's events so the test can assert on the
// sub-agent progress stream the browser would render.
type liveObserver struct {
	recordingObserver
	t *testing.T
}

func (o *liveObserver) Observe(eventType string, payload map[string]any) {
	o.recordingObserver.Observe(eventType, payload)
	if eventType == SubagentProgressEvent {
		o.t.Logf("progress: phase=%v tool=%v step=%v detail=%v",
			payload["phase"], payload["tool"], payload["step"], payload["detail"])
	}
}

// TestLive_ChatDelegationReturnsTheChildsAnswer is the end-to-end reproduction
// of the reported bug, against a real model: a chat turn delegates, and the
// child's answer must come back — quickly, cheaply, and with live progress
// events along the way.
func TestLive_ChatDelegationReturnsTheChildsAnswer(t *testing.T) {
	model, resolver := liveModel(t)
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	obs := &liveObserver{t: t}

	tc := TurnConfig{
		SystemPrompt: "You are a helpful assistant." + DelegationPromptSection(),
		Messages: []fantasy.Message{fantasy.NewUserMessage(
			"Spawn one sub-agent (role=explore) with this exact task: " +
				"'Reply with a haiku about the sea, and nothing else.' " +
				"Then report back exactly what the sub-agent replied.")},
		Model:         model,
		MaxTokens:     2048,
		Temperature:   0.3,
		MaxIterations: 12,
		NativeTools:   tools.DefaultTools(),
		Config: &config.Config{
			MaxIterations: 12, LLMMaxTokens: 2048,
			MCPServers: map[string]config.MCPServerConfig{},
		},
		MaxCostUSD: 1.0,
		Subagent: SubagentOptions{
			Enabled: true, MaxDepth: 1, MaxChildren: 5, BudgetFraction: 0.10, Resolver: resolver,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := RunInteractiveTurn(ctx, tc, obs)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("interactive turn: %v", err)
	}
	t.Logf("turn finished in %s, cost $%.5f, %d prompt + %d completion tokens",
		elapsed, res.Usage.CostUSD, res.Usage.PromptTokens, res.Usage.CompletionTokens)
	t.Logf("final text: %s", res.FinalText)

	// The parent must have actually delegated (the point of the run) …
	var spawned bool
	for _, e := range res.Entries {
		if e.Type == "tool_call" && e.ToolName == "spawn_subagent" {
			spawned = true
		}
	}
	if !spawned {
		t.Fatalf("the model did not call spawn_subagent; entries=%d", len(res.Entries))
	}

	// … and the delegation must have streamed progress, starting with started
	// and ending with a successful finished event carrying the child's spend.
	events := obs.progress()
	if len(events) < 2 {
		t.Fatalf("expected live progress events for the child, got %d", len(events))
	}
	if phase, _ := events[0].payload["phase"].(string); phase != subagentPhaseStarted {
		t.Fatalf("first progress phase = %q, want started", phase)
	}
	last := events[len(events)-1].payload
	if phase, _ := last["phase"].(string); phase != subagentPhaseFinished {
		t.Fatalf("last progress phase = %q, want finished (phases: %v)", phase, obs.phases())
	}
	if success, _ := last["success"].(bool); !success {
		t.Fatalf("the child did not succeed: %v", last)
	}
	if cost, _ := last["cost_usd"].(float64); cost <= 0 {
		t.Fatalf("the child's spend must be reported and charged back, got %v", last["cost_usd"])
	}

	// The child's answer must reach the user's reply. A haiku is three lines of
	// sea imagery — assert on the shape we asked for rather than exact words.
	if strings.TrimSpace(res.FinalText) == "" {
		t.Fatal("the turn produced no reply at all — the delegation swallowed the turn")
	}
	if !strings.Contains(strings.ToLower(res.FinalText), "sea") &&
		!strings.Contains(strings.ToLower(res.FinalText), "wave") &&
		!strings.Contains(strings.ToLower(res.FinalText), "ocean") &&
		!strings.Contains(strings.ToLower(res.FinalText), "tide") {
		t.Fatalf("the reply does not carry the child's haiku: %q", res.FinalText)
	}
	// The self-audit ritual is a ROOT-run gate: a child must never narrate it
	// back to the user. This is the regression that made delegations useless.
	if strings.Contains(strings.ToLower(res.FinalText), "self-audit") ||
		strings.Contains(strings.ToLower(res.FinalText), "confirm_audit") {
		t.Fatalf("the child narrated the audit ritual into the answer: %q", res.FinalText)
	}
}

// TestLive_ExploreChildUsesToolsAndReportsTrail delegates work that requires a
// tool, so the live run exercises the tool-phase progress events and the trail
// (steps / tools_used) the child card renders. Network egress is the host-side
// web_fetch broker (ADR-0036), so no sandbox is required.
func TestLive_ExploreChildUsesToolsAndReportsTrail(t *testing.T) {
	model, resolver := liveModel(t)
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	obs := &liveObserver{t: t}

	parent := NewAgent(Options{
		Config: &config.Config{
			MaxIterations: 12, LLMMaxTokens: 2048,
			MCPServers: map[string]config.MCPServerConfig{},
		},
		Model:         model,
		SystemPrompt:  "You are a scheduled agent.",
		MaxIterations: 12,
		NativeTools:   tools.DefaultTools(),
		Subagent: SubagentOptions{
			Enabled: true, MaxDepth: 1, MaxChildren: 5, BudgetFraction: 1.0, Resolver: resolver,
		},
	})
	parent.runtimePolicy = agentcore.NewScheduledPolicy(parent.logSession, parent.maxIterations, 1.0, 0)
	parent.subagent.budgetFraction = 1.0
	parent.spawnObserver = obs

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx = tools.WithForcedWorkingDir(ctx, t.TempDir())

	resp, err := parent.spawn(ctx, spawnSubagentInput{
		Task: "Fetch https://example.com with the web_fetch tool and report, in one sentence, " +
			"what the page says. Use the tool — do not answer from memory.",
		Role:           SubagentRoleExplore,
		TimeoutMinutes: 3,
	}, "call-live-1")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := parseSpawnOutput(t, resp)
	t.Logf("child result: success=%v steps=%d tools=%v cost=$%.5f answer=%q",
		out.Success, out.Steps, out.ToolsUsed, out.CostUSD, out.Result)

	if !out.Success {
		t.Fatalf("the child failed: %+v", out)
	}
	if out.Steps < 1 || len(out.ToolsUsed) == 0 {
		t.Fatalf("a tool-using child must report its trail; steps=%d tools=%v", out.Steps, out.ToolsUsed)
	}
	// The trail must also have streamed live, as tool-phase events.
	var sawToolPhase bool
	for _, e := range obs.progress() {
		if phase, _ := e.payload["phase"].(string); phase == subagentPhaseTool {
			sawToolPhase = true
		}
	}
	if !sawToolPhase {
		t.Fatalf("no tool-phase progress event reached the parent; phases=%v", obs.phases())
	}
}
