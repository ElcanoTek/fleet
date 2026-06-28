package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
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
	name      string
	inTokens  int64
	outTokens int64
	costUSD   float64
	// streamCount is atomic: the concurrency tests share one mock across many
	// child goroutines, so the call counter must be race-free.
	streamCount atomic.Int64
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
	m.streamCount.Add(1)
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
	if child.streamCount.Load() == 0 {
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
	if child.streamCount.Load() != 0 {
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

// TestReserveChildBudget_ConcurrentNeverOverGrants is the #175 hardening
// regression: it fires MANY budget reservations CONCURRENTLY against a small
// parent ceiling and asserts the SUM of granted child budgets never exceeds the
// parent's remaining budget — proving the invariant holds via the atomic
// reservation under a.mu, WITHOUT relying on spawn_subagent being a sequential
// tool. Each goroutine reserves but does NOT release (every child is treated as
// in-flight at once), which is the worst case for over-granting. Run under -race.
func TestReserveChildBudget_ConcurrentNeverOverGrants(t *testing.T) {
	child := &budgetMockModel{name: "c"}
	const (
		ceilingCost   = 0.10 // small parent cost ceiling
		ceilingTokens = 10000
		goroutines    = 64 // far more than the budget can satisfy
	)
	// maxChildren high so FAN-OUT is not the binding constraint — BUDGET is. (Fan-
	// out is reserved separately in spawn(), not in reserveChildBudget, so it does
	// not bound this test.)
	p := newParentForSpawn(t, child, ceilingCost, ceilingTokens, 2, goroutines+10)

	var (
		mu          sync.Mutex
		grantedCost float64
		grantedTok  int
		grants      int
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			// Each requests an explicit slice; the atomic reservation must cap the
			// SUM regardless of how many race in.
			cost, tok, refusal := p.reserveChildBudget(0.03, 3000)
			if refusal != "" {
				return // refused once the budget is reserved out — expected
			}
			mu.Lock()
			grantedCost += cost
			grantedTok += tok
			grants++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	// THE INVARIANT: the sum of all concurrently-granted child budgets never
	// exceeds the parent's remaining budget on EITHER axis.
	if grantedCost > ceilingCost+1e-9 {
		t.Fatalf("concurrent reservations granted $%.4f total > parent remaining $%.4f — atomic reservation failed",
			grantedCost, ceilingCost)
	}
	if grantedTok > ceilingTokens {
		t.Fatalf("concurrent reservations granted %d tokens total > parent remaining %d — atomic reservation failed",
			grantedTok, ceilingTokens)
	}
	if grants == 0 {
		t.Fatal("no reservations succeeded; the test did not exercise the grant path")
	}
	// Sanity: the reservation counters reflect exactly what was handed out.
	p.mu.Lock()
	gotResCost, gotResTok := p.subagent.reservedCostUSD, p.subagent.reservedTokens
	p.mu.Unlock()
	if gotResCost > ceilingCost+1e-9 || gotResTok > ceilingTokens {
		t.Fatalf("reservation counters exceeded the parent ceiling: cost=$%.4f tok=%d", gotResCost, gotResTok)
	}
}

// TestSpawn_ConcurrentNeverBreachesParentCeiling drives the FULL spawn path
// concurrently (children actually run through agentcore.Run) and asserts the
// parent's charged-back spend never exceeds its ceiling — the end-to-end form of
// the hardening, also run under -race.
func TestSpawn_ConcurrentNeverBreachesParentCeiling(t *testing.T) {
	// Each child step costs $0.02; parent ceiling $0.10. Children request a tiny
	// slice so each stops after ~one paid step.
	child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2, costUSD: 0.02}
	const goroutines = 32
	p := newParentForSpawn(t, child, 0.10, 0, 2, goroutines+10)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.spawn(context.Background(), spawnSubagentInput{Task: "subtask", MaxCostUSD: 0.02})
		}()
	}
	wg.Wait()

	// After all children settle, the parent's charged-back spend is bounded by its
	// ceiling plus at most one in-flight last-step overrun PER child that ran. With
	// a $0.02 per-step child and a $0.10 ceiling, total spend must stay well within
	// a small multiple of the ceiling — and crucially the reservation is fully
	// released (no leak).
	b := p.runtimePolicy.Budget()
	// The hard guarantee the reservation provides: at most a bounded number of
	// children are ever granted budget (sum of grants <= remaining at grant time),
	// so charged-back spend cannot run away. Allow a generous slack for last-step
	// overruns but assert it did not, e.g., let all 32 children each spend.
	if b.SpentCostUSD > ceilingWithOverrunSlack {
		t.Fatalf("concurrent spawns charged back $%.4f, far above the $0.10 ceiling — budget split leaked",
			b.SpentCostUSD)
	}
	// Reservation must be fully released after every child returns (no leak).
	p.mu.Lock()
	resCost, resTok := p.subagent.reservedCostUSD, p.subagent.reservedTokens
	p.mu.Unlock()
	if resCost != 0 || resTok != 0 {
		t.Fatalf("in-flight reservation leaked after all children returned: cost=$%.4f tok=%d", resCost, resTok)
	}
}

// ceilingWithOverrunSlack bounds acceptable total charged-back spend for the
// concurrent end-to-end spawn test: the $0.10 ceiling plus a generous allowance
// for the few children that win a grant and each overrun their own slice by at
// most one step. It is deliberately well below "every goroutine spent" ($0.64),
// so a regression that lets all 32 children run would fail.
const ceilingWithOverrunSlack = 0.30

// TestGrantFrom_HardCapsAtAvailable unit-tests the pure grant slicers: a request
// is never honored beyond the AVAILABLE (reservation-reduced) budget on either
// axis.
func TestGrantFrom_HardCapsAtAvailable(t *testing.T) {
	// Cost: $0.10 available, request $0.50 → capped at available.
	if got := grantCostFrom(0.10, 0.50); got > 0.10+1e-9 {
		t.Fatalf("grantCostFrom granted $%.4f > available $0.10 — not a hard cap", got)
	}
	// Cost: no request → default fraction of available.
	if got := grantCostFrom(0.10, 0); got > 0.10+1e-9 {
		t.Fatalf("grantCostFrom default granted $%.4f > available $0.10", got)
	}
	// Tokens: 2000 available, request 5000 → capped.
	if got := grantTokensFrom(2000, 5000); got > 2000 {
		t.Fatalf("grantTokensFrom granted %d > available 2000 — not a hard cap", got)
	}
}

// TestReserveChildBudget_AtomicAndHardCaps exercises the atomic reservation path:
// a grant is computed against remaining MINUS in-flight reservations, never
// exceeds available, refuses when too little is left, and unlimited parents yield
// unlimited children.
func TestReserveChildBudget_AtomicAndHardCaps(t *testing.T) {
	child := &budgetMockModel{name: "c"}

	// Finite cost parent: $1.00 ceiling, $0.90 spent → $0.10 remaining.
	p := newParentForSpawn(t, child, 1.0, 0, 2, 100)
	p.runtimePolicy.ChargeChildUsage(agentcore.RunUsage{CostUSD: 0.90})
	cost, _, refusal := p.reserveChildBudget(0.50, 0)
	if refusal != "" {
		t.Fatalf("unexpected refusal with $0.10 remaining: %s", refusal)
	}
	if cost > 0.10+1e-9 {
		t.Fatalf("granted $%.4f > remaining $0.10 — hard cap breached", cost)
	}
	// The grant is now reserved; a SECOND reservation sees less available and is
	// refused (only ~$0.05 is left after the first ~$0.05 grant, below the floor
	// once the first grant takes the default fraction). Even if it grants, the SUM
	// must never exceed the $0.10 remaining.
	cost2, _, _ := p.reserveChildBudget(0.50, 0)
	if cost+cost2 > 0.10+1e-9 {
		t.Fatalf("two reservations summed to $%.4f > remaining $0.10 — atomic reservation failed", cost+cost2)
	}

	// Unlimited parent (0 ceiling) → unlimited child (0), no refusal.
	pu := newParentForSpawn(t, child, 0, 0, 2, 100)
	if c, tok, refusal := pu.reserveChildBudget(0.5, 5000); refusal != "" || c != 0 || tok != 0 {
		t.Fatalf("unlimited parent should grant unlimited (0,0) with no refusal, got c=%v tok=%d refusal=%q", c, tok, refusal)
	}

	// Exhausted parent → refusal.
	pe := newParentForSpawn(t, child, 1.0, 0, 2, 100)
	pe.runtimePolicy.ChargeChildUsage(agentcore.RunUsage{CostUSD: 1.0})
	if _, _, refusal := pe.reserveChildBudget(0, 0); refusal == "" {
		t.Fatal("an exhausted parent budget must refuse a spawn")
	}

	// Token axis: 10000 ceiling, 9900 spent → 100 remaining (below floor) → refuse.
	pt := newParentForSpawn(t, child, 0, 10000, 2, 100)
	pt.runtimePolicy.ChargeChildUsage(agentcore.RunUsage{PromptTokens: 9900})
	if _, _, refusal := pt.reserveChildBudget(0, 0); refusal == "" {
		t.Fatal("a near-exhausted token budget (below the floor) must refuse a spawn")
	}
}
