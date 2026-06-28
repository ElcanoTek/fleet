package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// Governed sub-agents (#175, part b). These tests use the fake-LLM seam only
// (mock fantasy.LanguageModel) — no real key, no network, no sandbox.

// budgetMockModel is a fantasy.LanguageModel whose every stream step reports a
// fixed token + cost spend, so a child run's spend is deterministic. costUSD is
// attached via OpenRouter provider metadata (the only path openrouterCost reads).
type budgetMockModel struct {
	name        string
	inTokens    int64
	outTokens   int64
	costUSD     float64
	streamCount int
}

func (m *budgetMockModel) finishPart() fantasy.StreamPart {
	part := fantasy.StreamPart{
		Type:         fantasy.StreamPartTypeFinish,
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: m.inTokens, OutputTokens: m.outTokens},
	}
	if m.costUSD > 0 {
		part.ProviderMetadata = fantasy.ProviderMetadata{
			openrouter.Name: &openrouter.ProviderMetadata{
				Usage: openrouter.UsageAccounting{Cost: m.costUSD},
			},
		}
	}
	return part
}

func (m *budgetMockModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.streamCount++
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "child working"}) {
			return
		}
		yield(m.finishPart())
	}, nil
}

func (m *budgetMockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: "ok"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: m.inTokens, OutputTokens: m.outTokens},
	}, nil
}

func (m *budgetMockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *budgetMockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *budgetMockModel) Provider() string { return "mock" }
func (m *budgetMockModel) Model() string {
	if m.name != "" {
		return m.name
	}
	return "mock-child"
}

// newParentForSpawn builds a parent scheduled Agent wired with a live
// ScheduledPolicy carrying the given budget, the sub-agents feature on, and the
// given caps. The child model is `child`. It returns the parent ready for spawn().
func newParentForSpawn(t *testing.T, child fantasy.LanguageModel, maxCostUSD float64, maxTokens, maxDepth, maxChildren int) *Agent {
	t.Helper()
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         child,
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
		Subagent: SubagentOptions{
			Enabled:     true,
			MaxDepth:    maxDepth,
			MaxChildren: maxChildren,
		},
	})
	// Install a live policy carrying the budget the spawn tool reads + charges.
	a.runtimePolicy = agentcore.NewScheduledPolicy(a.logSession, a.maxIterations, maxCostUSD, maxTokens)
	return a
}

// TestSpawn_ChildRunsThroughGovernedCoreWithSlicedBudgetAndDepth is the core
// integration: a parent spawns a child; the child runs through (*Agent).Execute →
// agentcore.Run; it carries depth+1, a sliced budget, and the parent's allowlist;
// and its actual spend is charged back into the parent's budget.
func TestSpawn_ChildRunsThroughGovernedCoreWithSlicedBudgetAndDepth(t *testing.T) {
	// Child model: 6 uncached tokens + $0.001 per step. The child's token ceiling
	// is sliced from the parent's remaining 1000 tokens (default 50% = 500), well
	// above one step's 6, so the child finishes on its own (no confirm_audit) only
	// if it reaches finish enforcement — which it won't, so it loops and bounds out.
	// To keep this deterministic we instead give the child a TINY sliced budget by
	// requesting one explicitly, forcing the child's ceiling to fire after a step.
	child := &budgetMockModel{name: "child-model", inTokens: 100, outTokens: 20, costUSD: 0.05}
	parent := newParentForSpawn(t, child, 1.0 /*cost*/, 100000 /*tokens*/, 2, 4)
	// Give the parent a non-nil credential allowlist so we can assert the child
	// inherits a COPY (monotonic privilege: same entries, separate backing array).
	parent.credentialAllowlist = agentcore.CredentialAllowlist{{Server: "alpha"}}

	// Request a small child budget so the child's own ceiling fires quickly and the
	// child run terminates deterministically (StoppedByBudget) without needing the
	// child model to call confirm_audit.
	resp, err := parent.spawn(context.Background(), spawnSubagentInput{
		Task:           "do a scoped subtask",
		MaxCostUSD:     0.05, // one step's worth → child stops after its first paid step
		MaxTotalTokens: 50,
	})
	if err != nil {
		t.Fatalf("spawn returned a transport error (should be tool-level at worst): %v", err)
	}
	if resp.IsError {
		t.Fatalf("spawn unexpectedly refused: %q", resp.Content)
	}

	// (1) The child actually ran through agentcore.Run: the child model streamed at
	// least once (proving Execute → Run drove it).
	if child.streamCount == 0 {
		t.Fatal("child model never streamed — the child did not run through agentcore.Run")
	}

	// (3) The child's spend was charged back into the PARENT's budget. After the
	// charge, the parent's remaining budget is strictly less than its ceiling.
	pb := parent.runtimePolicy.Budget()
	if pb.SpentCostUSD <= 0 {
		t.Fatalf("child spend ($%.4f) was not charged back to the parent budget", pb.SpentCostUSD)
	}
	if pb.SpentTokens <= 0 {
		t.Fatalf("child token spend (%d) was not charged back to the parent budget", pb.SpentTokens)
	}
	// The parent ceiling is the hard wall: spend never exceeds it.
	if pb.SpentCostUSD > pb.MaxCostUSD {
		t.Fatalf("child spend $%.4f breached the parent cost ceiling $%.4f", pb.SpentCostUSD, pb.MaxCostUSD)
	}

	// (2) + depth: assert directly on buildChild that privilege only narrows and
	// depth advances. (spawn() builds the child internally; buildChild is the unit
	// that sets these invariants.)
	c := parent.buildChild(child, parent.narrowedCredentialAllowlist(), nil, 0.05, 50)
	if c.subagent.depth != parent.subagent.depth+1 {
		t.Fatalf("child depth = %d, want parent+1 = %d", c.subagent.depth, parent.subagent.depth+1)
	}
	if len(c.credentialAllowlist) != 1 || c.credentialAllowlist[0].Server != "alpha" {
		t.Fatalf("child should inherit the parent allowlist verbatim, got %v", c.credentialAllowlist)
	}
	// Separate backing array: mutating the child's copy must not touch the parent's.
	c.credentialAllowlist[0].Server = "MUTATED"
	if parent.credentialAllowlist[0].Server != "alpha" {
		t.Fatal("child allowlist shares the parent's backing array — monotonic privilege must hand the child a COPY")
	}
	if c.costCeilingOverride != 0.05 || c.tokenCeilingOverride != 50 {
		t.Fatalf("child ceilings not sliced: cost=%v tokens=%v", c.costCeilingOverride, c.tokenCeilingOverride)
	}
}

// TestSpawn_BudgetNeverExceedsParentCeiling proves the HARD wall: across repeated
// spawns, the total charged-back child spend can never exceed the parent's
// remaining budget — once it is exhausted, further spawns are refused.
func TestSpawn_BudgetNeverExceedsParentCeiling(t *testing.T) {
	// Each child step costs $0.05. Parent cost ceiling $0.20, no token ceiling.
	// maxChildren is high so fan-out is NOT the binding constraint here — the
	// BUDGET is. The child requests a tiny ceiling so each child stops after ~1
	// paid step and charges ~$0.05 back.
	child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2, costUSD: 0.05}
	parent := newParentForSpawn(t, child, 0.20 /*cost ceiling*/, 0 /*no token ceiling*/, 2, 100)

	spent := 0.0
	refusedForBudget := false
	for i := 0; i < 10; i++ {
		resp, err := parent.spawn(context.Background(), spawnSubagentInput{
			Task:       "subtask",
			MaxCostUSD: 0.05,
		})
		if err != nil {
			t.Fatalf("spawn %d transport error: %v", i, err)
		}
		if resp.IsError {
			if strings.Contains(resp.Content, "SUBAGENT_BUDGET_EXHAUSTED") {
				refusedForBudget = true
				break
			}
			// A non-budget refusal (e.g. fan-out) would be a test setup error.
			t.Fatalf("spawn %d refused for a non-budget reason: %q", i, resp.Content)
		}
		spent = parent.runtimePolicy.Budget().SpentCostUSD
		// The invariant under test: cumulative charged spend NEVER exceeds the
		// parent ceiling, at every step.
		if spent > parent.runtimePolicy.Budget().MaxCostUSD+1e-9 {
			t.Fatalf("cumulative child spend $%.4f breached parent ceiling $%.4f after spawn %d",
				spent, parent.runtimePolicy.Budget().MaxCostUSD, i)
		}
	}
	if !refusedForBudget {
		t.Fatalf("expected a SUBAGENT_BUDGET_EXHAUSTED refusal once the parent budget was spent; final spend $%.4f", spent)
	}
	// Final spend is bounded by the ceiling.
	final := parent.runtimePolicy.Budget().SpentCostUSD
	if final > 0.20+1e-9 {
		t.Fatalf("final cumulative child spend $%.4f exceeded the parent ceiling $0.20", final)
	}
}

// TestSpawn_DepthCapRefusesAtMaxDepth proves a spawn is refused once this run's
// depth has reached maxDepth.
func TestSpawn_DepthCapRefusesAtMaxDepth(t *testing.T) {
	child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2}
	const maxDepth = 1
	parent := newParentForSpawn(t, child, 0, 0, maxDepth, 4)
	// Force this run to BE at maxDepth (as if it were a child that may not recurse).
	parent.subagent.depth = maxDepth

	resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "go deeper"})
	if err != nil {
		t.Fatalf("spawn transport error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "SUBAGENT_DEPTH_EXCEEDED") {
		t.Fatalf("expected a depth-cap refusal at depth==maxDepth, got isError=%v content=%q", resp.IsError, resp.Content)
	}
	// A refused spawn must NOT have run a child.
	if child.streamCount != 0 {
		t.Fatal("a depth-refused spawn must not run any child")
	}
	// And it must NOT have consumed a fan-out slot (the depth check precedes it).
	if parent.subagent.childrenSpawned != 0 {
		t.Fatalf("depth refusal consumed a fan-out slot: childrenSpawned=%d", parent.subagent.childrenSpawned)
	}
}

// TestSpawn_FanOutCapRefusesExtraChild proves the (maxChildren+1)-th spawn is
// refused.
func TestSpawn_FanOutCapRefusesExtraChild(t *testing.T) {
	const maxChildren = 2
	// Child charges $0.001/step; the parent has a GENEROUS $10 ceiling and each
	// spawn requests a tiny $0.001 slice so the child stops after one paid step.
	// This keeps fan-out — not budget — the binding constraint while making the
	// children terminate fast (no max-rounds churn).
	child := &budgetMockModel{name: "c", inTokens: 1, outTokens: 1, costUSD: 0.001}
	parent := newParentForSpawn(t, child, 10.0 /*ample cost*/, 0 /*unlimited tokens*/, 2, maxChildren)

	for i := 0; i < maxChildren; i++ {
		resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "subtask", MaxCostUSD: 0.001})
		if err != nil {
			t.Fatalf("spawn %d transport error: %v", i, err)
		}
		if resp.IsError {
			t.Fatalf("spawn %d (within fan-out cap) unexpectedly refused: %q", i, resp.Content)
		}
	}
	// The (maxChildren+1)-th spawn must be refused.
	resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "one too many", MaxCostUSD: 0.001})
	if err != nil {
		t.Fatalf("overflow spawn transport error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "SUBAGENT_FANOUT_EXCEEDED") {
		t.Fatalf("expected a fan-out refusal on the (max+1)-th spawn, got isError=%v content=%q", resp.IsError, resp.Content)
	}
}

// TestSpawn_AllowServersOnlyNarrows proves the child's MCP server selection can
// only be a SUBSET of the parent's loaded set (monotonic privilege, server axis):
// a request naming a server the parent has NOT loaded is dropped, never added.
func TestSpawn_AllowServersOnlyNarrows(t *testing.T) {
	child := &budgetMockModel{name: "c"}
	parent := newParentForSpawn(t, child, 0, 0, 2, 4)
	parent.loadedServers = map[string]bool{"alpha": true, "beta": true}

	// Request alpha (loaded) + gamma (NOT loaded). Only alpha survives.
	got := parent.childSelection([]string{"alpha", "gamma"})
	if len(got) != 1 || !got["alpha"] {
		t.Fatalf("childSelection must intersect with the parent's loaded set; got %v", got)
	}
	if got["gamma"] {
		t.Fatal("childSelection added a server the parent never loaded — privilege must only narrow")
	}

	// No request → inherit the full parent set (not a superset).
	all := parent.childSelection(nil)
	if len(all) != 2 || !all["alpha"] || !all["beta"] {
		t.Fatalf("childSelection(nil) should inherit the full parent set, got %v", all)
	}
}

// TestSpawn_DisabledIsRefused pins the off-by-default gate at the body level
// (defence in depth: the tool is also simply not registered when disabled).
func TestSpawn_DisabledIsRefused(t *testing.T) {
	child := &budgetMockModel{name: "c"}
	parent := newParentForSpawn(t, child, 0, 0, 2, 4)
	parent.subagent.enabled = false

	resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "x"})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "disabled") {
		t.Fatalf("disabled spawn must refuse, got isError=%v content=%q", resp.IsError, resp.Content)
	}
}

// toolCapturingModel records the tool names advertised on each stream call so a
// test can assert which tools Execute registered. It finishes immediately so the
// parent run bounds out at the round cap (we only care about round-0 tools).
type toolCapturingModel struct {
	advertised []string
}

func (m *toolCapturingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.advertised == nil {
		for _, tl := range call.Tools {
			if ft, ok := tl.(fantasy.FunctionTool); ok {
				m.advertised = append(m.advertised, ft.GetName())
			}
		}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}
func (m *toolCapturingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *toolCapturingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *toolCapturingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *toolCapturingModel) Provider() string { return "mock" }
func (m *toolCapturingModel) Model() string    { return "mock-capture" }

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestExecute_RegistersSpawnToolOnlyWhenEnabled pins the off-by-default gate at
// the REGISTRATION level: Execute advertises spawn_subagent iff the feature is on.
func TestExecute_RegistersSpawnToolOnlyWhenEnabled(t *testing.T) {
	t.Run("enabled advertises the tool", func(t *testing.T) {
		t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
		m := &toolCapturingModel{}
		a := NewAgent(Options{
			Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
			Model:         m,
			SystemPrompt:  "sys",
			MaxIterations: 50,
			Subagent:      SubagentOptions{Enabled: true, MaxDepth: 2, MaxChildren: 4},
		})
		_ = a.Execute(context.Background(), "task") // errors at round cap; we only inspect round-0 tools
		if !containsStr(m.advertised, "spawn_subagent") {
			t.Fatalf("spawn_subagent must be advertised when enabled; advertised=%v", m.advertised)
		}
	})

	t.Run("disabled does not advertise the tool", func(t *testing.T) {
		t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
		m := &toolCapturingModel{}
		a := NewAgent(Options{
			Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
			Model:         m,
			SystemPrompt:  "sys",
			MaxIterations: 50,
			Subagent:      SubagentOptions{Enabled: false},
		})
		_ = a.Execute(context.Background(), "task")
		if containsStr(m.advertised, "spawn_subagent") {
			t.Fatalf("spawn_subagent must NOT be advertised when disabled (off by default); advertised=%v", m.advertised)
		}
	})
}

// TestSliceBudget_HardCapsAtParentRemaining unit-tests the budget slicer: a
// request is never honored beyond the parent's remaining budget, and unlimited
// parents yield unlimited children.
func TestSliceBudget_HardCapsAtParentRemaining(t *testing.T) {
	// Finite parent: $1.00 ceiling, $0.90 spent → $0.10 remaining.
	b := agentcore.BudgetState{MaxCostUSD: 1.0, SpentCostUSD: 0.90}
	// Request more than remaining → capped at remaining.
	got, refusal := sliceCostBudget(b, 0.50)
	if refusal != "" {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
	if got > 0.10+1e-9 {
		t.Fatalf("sliceCostBudget granted $%.4f > remaining $0.10 — the parent ceiling is not a hard cap", got)
	}

	// Unlimited parent (0 ceiling) → unlimited child (0).
	if got, _ := sliceCostBudget(agentcore.BudgetState{}, 0.5); got != 0 {
		t.Fatalf("unlimited parent should yield unlimited (0) child cost, got %v", got)
	}

	// Exhausted parent → refusal.
	if _, refusal := sliceCostBudget(agentcore.BudgetState{MaxCostUSD: 1.0, SpentCostUSD: 1.0}, 0); refusal == "" {
		t.Fatal("an exhausted parent budget must refuse a spawn")
	}

	// Token side: 10000 ceiling, 8000 spent → 2000 remaining; request 5000 → capped.
	tb := agentcore.BudgetState{MaxTotalTokens: 10000, SpentTokens: 8000}
	gotT, refusal := sliceTokenBudget(tb, 5000)
	if refusal != "" {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
	if gotT > 2000 {
		t.Fatalf("sliceTokenBudget granted %d > remaining 2000 — not a hard cap", gotT)
	}

	// Token side, near-exhausted (below the floor) → refusal.
	if _, refusal := sliceTokenBudget(agentcore.BudgetState{MaxTotalTokens: 10000, SpentTokens: 9900}, 0); refusal == "" {
		t.Fatal("a near-exhausted token budget (below the min child floor) must refuse a spawn")
	}
}
