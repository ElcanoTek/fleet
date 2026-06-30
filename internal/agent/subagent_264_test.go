package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"

	"github.com/google/uuid"
)

// Agent delegation (#264) tests: the deltas this issue adds on top of #175's
// governed spawn_subagent — parallel dispatch, JSON output, per-task opt-in, the
// 10% per-child budget cap, one-level depth (children get no tool), the max-5
// fan-out refusal, per-child timeout, max_iterations, and parent_task_id linkage.
// All use the fake-LLM seam — no real key, no network, no sandbox.

// sleepingChildModel sleeps before finishing each step and reports a fixed
// per-step cost, so a child handed a one-step budget runs exactly one step (sleeps
// once) before its ceiling fires. Used to measure parallel vs sequential fan-out.
type sleepingChildModel struct {
	delay       time.Duration
	costUSD     float64
	streamCount atomic.Int64
}

func (m *sleepingChildModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.streamCount.Add(1)
	// Sleep honoring cancellation so a per-child timeout / parent cancel actually
	// shortens the run. On cancellation, surface it as a stream error so the child
	// produces NO clean final answer (→ success=false), exactly like a real timeout.
	t := time.NewTimer(m.delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "child done"}) {
			return
		}
		part := fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 2},
		}
		if m.costUSD > 0 {
			part.ProviderMetadata = fantasy.ProviderMetadata{
				openrouter.Name: &openrouter.ProviderMetadata{Usage: openrouter.UsageAccounting{Cost: m.costUSD}},
			}
		}
		yield(part)
	}, nil
}

func (m *sleepingChildModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *sleepingChildModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *sleepingChildModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *sleepingChildModel) Provider() string { return "mock" }
func (m *sleepingChildModel) Model() string    { return "slow-child" }

// multiSpawnParentModel emits N spawn_subagent tool calls on its FIRST stream
// (finishing with FinishReasonToolCalls so fantasy executes them), then finishes
// with Stop on every later call. It is the parent that fans out in one turn.
type multiSpawnParentModel struct {
	n           int
	childCost   float64
	streamCount atomic.Int64
}

func (m *multiSpawnParentModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	first := m.streamCount.Add(1) == 1
	return func(yield func(fantasy.StreamPart) bool) {
		if first {
			for i := 0; i < m.n; i++ {
				input := fmt.Sprintf(`{"task":"subtask %d","max_cost_usd":%g}`, i, m.childCost)
				if !yield(fantasy.StreamPart{
					Type:          fantasy.StreamPartTypeToolCall,
					ID:            fmt.Sprintf("call-%d", i),
					ToolCallName:  "spawn_subagent",
					ToolCallInput: input,
				}) {
					return
				}
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *multiSpawnParentModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *multiSpawnParentModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *multiSpawnParentModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *multiSpawnParentModel) Provider() string { return "mock" }
func (m *multiSpawnParentModel) Model() string    { return "parent" }

// staticResolver resolves any slug to a fixed model — the host-side child-model
// resolution seam (the real one is the Manager). Lets the parent run a fast model
// while children run a different (slow) one.
type staticResolver struct{ m fantasy.LanguageModel }

func (r staticResolver) Resolve(context.Context, string) (fantasy.LanguageModel, error) {
	return r.m, nil
}

// TestSpawnTool_IsMarkedParallel pins the registration property that makes #264's
// concurrent fan-out possible: the tool is advertised with Parallel=true, so
// fantasy dispatches multiple spawn calls in one turn concurrently.
func TestSpawnTool_IsMarkedParallel(t *testing.T) {
	a := newParentForSpawn(t, &budgetMockModel{name: "c"}, 0, 0, 1, 5)
	tool := a.newSpawnSubagentTool()
	if !tool.Info().Parallel {
		t.Fatal("spawn_subagent must be marked Parallel=true so fantasy fans out concurrent delegations (#264)")
	}
	if tool.Info().Name != "spawn_subagent" {
		t.Fatalf("unexpected tool name %q", tool.Info().Name)
	}
}

// TestReserveChildBudget_TenPercentCap pins the #264 per-child cap: with the
// default 10% fraction, a request above 10% of remaining is refused, and an
// unspecified request defaults to ~10% of remaining.
func TestReserveChildBudget_TenPercentCap(t *testing.T) {
	p := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0 /*ceiling*/, 0, 1, 5)
	p.subagent.budgetFraction = 0.10 // the #264 default (newParentForSpawn uses 1.0)

	// $1.00 remaining → 10% cap = $0.10. A $0.50 request is over the limit → refused.
	if _, _, refusal := p.reserveChildBudget(0.50, 0); refusal == "" || !strings.Contains(refusal, "OVER_LIMIT") {
		t.Fatalf("a $0.50 request against a $0.10 per-child cap must be refused, got %q", refusal)
	}
	// An unspecified request defaults to the full 10% slice.
	cost, _, refusal := p.reserveChildBudget(0, 0)
	if refusal != "" {
		t.Fatalf("default slice unexpectedly refused: %s", refusal)
	}
	if cost < 0.10-1e-9 || cost > 0.10+1e-9 {
		t.Fatalf("default child slice = $%.4f, want $0.10 (10%% of $1.00 remaining)", cost)
	}
	// A request exactly at the cap is allowed.
	p2 := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0, 0, 1, 5)
	p2.subagent.budgetFraction = 0.10
	if _, _, refusal := p2.reserveChildBudget(0.10, 0); refusal != "" {
		t.Fatalf("a request exactly at the 10%% cap must be allowed, got %q", refusal)
	}
}

// TestBuildChild_DepthLimitChildHasNoDelegationTool proves #264's "parent →
// sub-agent only": at the default depth (maxDepth=1) a child is built WITHOUT the
// spawn tool enabled, so it is never even registered — and at a deeper maxDepth a
// child below the limit DOES get it.
func TestBuildChild_DepthLimitChildHasNoDelegationTool(t *testing.T) {
	t.Run("default depth 1 — child cannot delegate", func(t *testing.T) {
		t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
		capModel := &toolCapturingModel{}
		parent := newParentForSpawn(t, capModel, 1.0, 0, 1 /*maxDepth*/, 5)
		child := parent.buildChild(capModel, nil, nil, 0.01, 0, 0)
		if child.subagent.enabled {
			t.Fatal("a child at the depth limit must have delegation DISABLED (#264 parent → sub-agent only)")
		}
		// And structurally: the child's Execute must not advertise the tool.
		_ = child.Execute(context.Background(), "do the work")
		if containsStr(capModel.advertised, "spawn_subagent") {
			t.Fatalf("a depth-limited child must NOT advertise spawn_subagent; advertised=%v", capModel.advertised)
		}
	})

	t.Run("maxDepth 2 — child below the limit can delegate", func(t *testing.T) {
		parent := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0, 0, 2 /*maxDepth*/, 5)
		child := parent.buildChild(&budgetMockModel{name: "c"}, nil, nil, 0.01, 0, 0)
		if !child.subagent.enabled {
			t.Fatal("with maxDepth=2 a depth-1 child should still be able to delegate")
		}
		if child.subagent.depth != 1 {
			t.Fatalf("child depth = %d, want 1", child.subagent.depth)
		}
	})
}

// TestBuildChild_MaxIterationsCappedAtParent pins that a requested per-child
// iteration cap (#264) is honored but never exceeds the parent's own.
func TestBuildChild_MaxIterationsCappedAtParent(t *testing.T) {
	parent := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0, 0, 1, 5)
	parent.maxIterations = 50

	// Request fewer than the parent's → honored.
	if c := parent.buildChild(&budgetMockModel{name: "c"}, nil, nil, 0.01, 0, 10); c.maxIterations != 10 {
		t.Fatalf("child maxIterations = %d, want 10 (the smaller request)", c.maxIterations)
	}
	// Request MORE than the parent's → clamped to the parent's.
	if c := parent.buildChild(&budgetMockModel{name: "c"}, nil, nil, 0.01, 0, 9999); c.maxIterations != 50 {
		t.Fatalf("child maxIterations = %d, want 50 (clamped to parent)", c.maxIterations)
	}
	// Unspecified (0) → inherit the parent's.
	if c := parent.buildChild(&budgetMockModel{name: "c"}, nil, nil, 0.01, 0, 0); c.maxIterations != 50 {
		t.Fatalf("child maxIterations = %d, want 50 (inherited)", c.maxIterations)
	}
}

// TestBuildChild_ParentTaskIDLinkage proves a spawned child's session carries the
// parent task id (#264 traceability), distinct from the parent's session id.
func TestBuildChild_ParentTaskIDLinkage(t *testing.T) {
	taskID := uuid.New()
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	parent := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         &budgetMockModel{name: "c"},
		SystemPrompt:  "sys",
		MaxIterations: 50,
		TaskID:        taskID,
		Subagent:      SubagentOptions{Enabled: true, MaxDepth: 1, MaxChildren: 5},
	})
	if parent.subagent.parentTaskID != taskID.String() {
		t.Fatalf("parent.subagent.parentTaskID = %q, want %q", parent.subagent.parentTaskID, taskID.String())
	}
	child := parent.buildChild(&budgetMockModel{name: "c"}, nil, nil, 0.01, 0, 0)
	if child.logSession.ParentTaskID != taskID.String() {
		t.Fatalf("child session ParentTaskID = %q, want %q", child.logSession.ParentTaskID, taskID.String())
	}
	if child.logSession.ID == parent.logSession.ID {
		t.Fatal("child session must have its own unique id, not the parent's")
	}
	if !strings.HasPrefix(child.logSession.ID, "subagent-") {
		t.Fatalf("child session id %q should be prefixed subagent-", child.logSession.ID)
	}
}

// TestRecordSubagentSpawn_AppendsToParentLog proves the parent's (persisted) log
// gains a subagent_spawned linkage entry carrying the child id + spend (#264).
func TestRecordSubagentSpawn_AppendsToParentLog(t *testing.T) {
	taskID := uuid.New()
	parent := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         &budgetMockModel{name: "c"},
		SystemPrompt:  "sys",
		MaxIterations: 50,
		TaskID:        taskID,
		Subagent:      SubagentOptions{Enabled: true, MaxDepth: 1, MaxChildren: 5},
	})
	parent.recordSubagentSpawn("subagent-xyz", agentcore.RunUsage{CostUSD: 0.02, PromptTokens: 100, CompletionTokens: 20}, true)
	msgs := parent.logSession.SnapshotMessages()
	var found *LogMessage
	for i := range msgs {
		if msgs[i].MessageType != nil && *msgs[i].MessageType == "subagent_spawned" {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("parent log must carry a subagent_spawned linkage entry")
	}
	for _, want := range []string{taskID.String(), "subagent-xyz", "\"cost_usd\":0.02", "\"tokens\":120", "\"success\":true"} {
		if !strings.Contains(found.Content, want) {
			t.Fatalf("subagent_spawned payload %q missing %q", found.Content, want)
		}
	}
}

// TestSpawn_JSONResultShape proves a successful delegation returns the #264 JSON
// contract {result, cost_usd, tokens, success} the parent model can branch on.
func TestSpawn_JSONResultShape(t *testing.T) {
	child := &budgetMockModel{name: "child", inTokens: 100, outTokens: 20, costUSD: 0.02}
	parent := newParentForSpawn(t, child, 1.0, 0, 1, 5)
	resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "do work", MaxCostUSD: 0.02})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	out := parseSpawnOutput(t, resp)
	if !out.Success {
		t.Fatalf("expected success, got %q", out.Result)
	}
	if out.CostUSD <= 0 {
		t.Fatalf("expected non-zero cost_usd, got %v", out.CostUSD)
	}
	if out.Tokens <= 0 {
		t.Fatalf("expected non-zero tokens, got %d", out.Tokens)
	}
	if strings.TrimSpace(out.Result) == "" {
		t.Fatal("expected a non-empty result (the child's answer)")
	}
}

// TestSpawn_DefaultFanoutIsFiveSixthRefused pins #264's "max 5 concurrent
// sub-agents": with the package-default fan-out cap (newSubagentConfig clamps a 0
// to 5), the 6th spawn in a run is refused with the documented message.
func TestSpawn_DefaultFanoutIsFiveSixthRefused(t *testing.T) {
	child := &budgetMockModel{name: "c", inTokens: 1, outTokens: 1, costUSD: 0.001}
	// maxChildren=0 → clamped to the package default (5).
	parent := newParentForSpawn(t, child, 10.0, 0, 1, 0)
	if parent.subagent.maxChildren != 5 {
		t.Fatalf("default maxChildren = %d, want 5 (#264)", parent.subagent.maxChildren)
	}
	for i := 0; i < 5; i++ {
		resp, _ := parent.spawn(context.Background(), spawnSubagentInput{Task: "t", MaxCostUSD: 0.001})
		if out := parseSpawnOutput(t, resp); !out.Success {
			t.Fatalf("spawn %d within the cap was refused: %q", i, out.Result)
		}
	}
	resp, _ := parent.spawn(context.Background(), spawnSubagentInput{Task: "sixth", MaxCostUSD: 0.001})
	if out := parseSpawnOutput(t, resp); out.Success || !strings.Contains(out.Result, "max concurrent sub-agents reached") {
		t.Fatalf("6th spawn must be refused with the documented message, got success=%v result=%q", out.Success, out.Result)
	}
}

// TestSpawn_TimeoutBranchAndChargeBack covers the #264 timeout plumbing on both
// the happy and abnormal exit paths: a positive timeout_minutes wraps the child
// ctx (exercised with a fast child that finishes well within it), and an
// inherited-context cancellation (the same mechanism a real timeout fires) still
// charges the child's spend back and releases the in-flight reservation — never a
// panic, never a leak.
func TestSpawn_TimeoutBranchAndChargeBack(t *testing.T) {
	t.Run("timeout_minutes set, fast child finishes within it", func(t *testing.T) {
		child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2, costUSD: 0.01}
		parent := newParentForSpawn(t, child, 1.0, 0, 1, 5)
		resp, err := parent.spawn(context.Background(), spawnSubagentInput{Task: "quick", MaxCostUSD: 0.01, TimeoutMinutes: 1})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if out := parseSpawnOutput(t, resp); !out.Success {
			t.Fatalf("a fast child under a 1-minute timeout should succeed, got %q", out.Result)
		}
		parent.mu.Lock()
		resCost, resTok := parent.subagent.reservedCostUSD, parent.subagent.reservedTokens
		parent.mu.Unlock()
		if resCost != 0 || resTok != 0 {
			t.Fatalf("reservation leaked after a successful child: cost=%v tok=%d", resCost, resTok)
		}
	})

	t.Run("inherited-context cancel stops the child, charges spend, releases reservation", func(t *testing.T) {
		child := &sleepingChildModel{delay: 5 * time.Second, costUSD: 0.01}
		parent := newParentForSpawn(t, child, 1.0, 0, 1, 5)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "slow", MaxCostUSD: 0.01})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if out := parseSpawnOutput(t, resp); out.Success {
			t.Fatalf("a cancelled child should report success=false, got %q", out.Result)
		}
		parent.mu.Lock()
		resCost, resTok := parent.subagent.reservedCostUSD, parent.subagent.reservedTokens
		parent.mu.Unlock()
		if resCost != 0 || resTok != 0 {
			t.Fatalf("reservation leaked after a cancelled child: cost=%v tok=%d", resCost, resTok)
		}
	})
}

// TestSpawn_TimeoutMinutesValidated rejects a negative timeout / iteration cap
// with an error result, never a panic.
func TestSpawn_TimeoutMinutesValidated(t *testing.T) {
	parent := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0, 0, 1, 5)
	for _, in := range []spawnSubagentInput{
		{Task: "t", TimeoutMinutes: -1},
		{Task: "t", MaxIterations: -1},
	} {
		resp, err := parent.spawn(context.Background(), in)
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if out := parseSpawnOutput(t, resp); out.Success {
			t.Fatalf("negative input must be refused, got success for %+v", in)
		}
	}
}

// TestSpawn_ParallelExecutionWallClock is the #264 acceptance check: multiple
// spawn_subagent calls emitted in a SINGLE model turn run CONCURRENTLY, so the
// run's wall-clock is far below the sum of the children's sequential durations.
// Driven entirely through fantasy's real tool dispatch (the parent model emits the
// batch) — proving the Parallel=true marking actually fans out.
func TestSpawn_ParallelExecutionWallClock(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test; skipped in -short")
	}
	const (
		n         = 4
		childWork = 200 * time.Millisecond
		childCost = 0.01
	)
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")

	slow := &sleepingChildModel{delay: childWork, costUSD: childCost}
	parentModel := &multiSpawnParentModel{n: n, childCost: childCost}
	parent := NewAgent(Options{
		// MaxCostUSD on the config is the parent ceiling Execute installs into the
		// live policy (it OWNS a.runtimePolicy — a pre-set value would be replaced).
		// $5 leaves ample room; each $0.01 child gets a one-step budget so it stops
		// after a single sleep, and the per-child fraction is 100% so all n fit.
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MaxCostUSD: 5.0, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         parentModel,
		SystemPrompt:  "sys",
		MaxIterations: 50,
		Subagent: SubagentOptions{
			Enabled:        true,
			MaxDepth:       1,
			MaxChildren:    n + 1,
			BudgetFraction: 1.0,
			ModelSlug:      "slow-child", // children resolve to the slow model, not the parent's
			Resolver:       staticResolver{m: slow},
		},
	})

	start := time.Now()
	_ = parent.Execute(context.Background(), "fan out") // errors at the round cap; we only time round 0
	elapsed := time.Since(start)

	if got := slow.streamCount.Load(); got < int64(n) {
		t.Fatalf("expected all %d children to run (each streams once), got %d", n, got)
	}
	sequential := time.Duration(n) * childWork
	// Concurrent execution must finish well under the sequential sum. A generous
	// bound (60%) absorbs scheduler jitter / CI load while still failing a
	// regression to sequential dispatch.
	if elapsed >= time.Duration(float64(sequential)*0.6) {
		t.Fatalf("fan-out took %s; sequential would be ~%s — children did NOT run concurrently (Parallel marking regressed?)",
			elapsed, sequential)
	}
}
