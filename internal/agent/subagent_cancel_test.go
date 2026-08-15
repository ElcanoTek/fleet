package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Stopping a parent must stop its children (#1043 follow-up).
//
// The structural guarantee is that a child's context is DERIVED from the
// context of the spawn tool call, which is the run's own context: fantasy hands
// the stream context to every tool it dispatches (sequential and parallel
// alike), the spawn body only wraps it (forced working dir, optional per-child
// timeout), and the child's agentcore.Run re-checks ctx.Err() at the top of
// every enforcement round. So a Stop that cancels the turn cancels the child
// too, and a child can never outlive the parent's tool call — spawn is
// synchronous, so the parent's call does not return until the child has.
//
// TestSpawn_TimeoutBranchAndChargeBack already pins the spawn-level half
// (cancel the context handed to spawn → the child stops, spend is charged,
// reservations are released). These tests pin the END-TO-END chain a user
// actually exercises: the Stop button cancels the TURN, and the delegation
// running inside it dies with it.

// spawningParentModel emits one spawn_subagent call on its first stream (the
// shape a real parent produces), then answers on any later call.
type spawningParentModel struct {
	task    string
	streams atomic.Int64
}

func (m *spawningParentModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	first := m.streams.Add(1) == 1
	return func(yield func(fantasy.StreamPart) bool) {
		if first {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "call-parent-1",
				ToolCallName:  "spawn_subagent",
				ToolCallInput: `{"task":"` + m.task + `","role":"explore"}`,
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *spawningParentModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *spawningParentModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *spawningParentModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *spawningParentModel) Provider() string { return "mock" }
func (m *spawningParentModel) Model() string    { return "spawning-parent" }

// cancelWatchingChildModel blocks until its context is cancelled (or a generous
// deadline elapses), recording which happened. A child that keeps running after
// the parent was stopped shows up here as ranToCompletion.
type cancelWatchingChildModel struct {
	started         chan struct{}
	startOnce       atomic.Bool
	sawCancel       atomic.Bool
	ranToCompletion atomic.Bool
	completionAfter time.Duration
}

func (m *cancelWatchingChildModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	if m.startOnce.CompareAndSwap(false, true) {
		close(m.started)
	}
	t := time.NewTimer(m.completionAfter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		m.sawCancel.Store(true)
		return nil, ctx.Err()
	case <-t.C:
		m.ranToCompletion.Store(true)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "child finished anyway"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *cancelWatchingChildModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *cancelWatchingChildModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *cancelWatchingChildModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *cancelWatchingChildModel) Provider() string { return "mock" }
func (m *cancelWatchingChildModel) Model() string    { return "cancel-watching-child" }

// TestInteractiveTurn_StopCancelsInFlightChild is the Stop-button contract: a
// chat turn cancelled while a delegation is in flight takes the child down with
// it, and the turn itself returns promptly rather than waiting out the child.
func TestInteractiveTurn_StopCancelsInFlightChild(t *testing.T) {
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	child := &cancelWatchingChildModel{started: make(chan struct{}), completionAfter: 30 * time.Second}
	parent := &spawningParentModel{task: "a long research task"}

	tc := TurnConfig{
		SystemPrompt:  "sys",
		Messages:      []fantasy.Message{fantasy.NewUserMessage("delegate this")},
		Model:         parent,
		MaxTokens:     1024,
		MaxIterations: 10,
		NativeTools:   tools.DefaultTools(),
		Config:        &config.Config{MaxIterations: 10, LLMMaxTokens: 1024, MCPServers: map[string]config.MCPServerConfig{}},
		MaxCostUSD:    1.0,
		Subagent: SubagentOptions{
			Enabled: true, MaxDepth: 1, MaxChildren: 5, BudgetFraction: 1.0,
			// The child runs on its own model handle (host-side resolution),
			// which is what makes the parent/child split observable here.
			Resolver: staticResolver{m: child}, ModelSlug: "child-model",
		},
	}

	ctx, cancel := context.WithCancel(tools.WithForcedWorkingDir(context.Background(), t.TempDir()))
	defer cancel()

	type turnOutcome struct {
		res agentcore.Result
		err error
	}
	done := make(chan turnOutcome, 1)
	go func() {
		res, err := RunInteractiveTurn(ctx, tc, &recordingObserver{})
		done <- turnOutcome{res, err}
	}()

	// Stop the turn only once the child is genuinely running — otherwise the
	// test could pass by cancelling before the delegation ever started.
	select {
	case <-child.started:
	case <-time.After(15 * time.Second):
		t.Fatal("the child never started; the delegation did not reach the child model")
	}
	cancel()

	// The turn must come back promptly — the child's own 30s completion timer
	// is the failure mode this guards (a turn that waits out its children is
	// exactly what "Stop does nothing" looks like to a user).
	//
	// Note we assert on the turn RETURNING, not on Result.Cancelled: the cancel
	// lands while the spawn tool is executing, so the round completes with a
	// failed tool result and the interactive loop's 1-round collapse ends the
	// turn normally rather than mid-stream. Chat's own cancel bookkeeping (the
	// partial-transcript commit and the turn.cancelled frame) is pinned by
	// internal/httpapi's TestChatCancel_PersistsPartialTurn.
	var out turnOutcome
	select {
	case out = <-done:
		if out.err != nil {
			t.Fatalf("a cancelled turn must return a partial result, not an error: %v", out.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the turn did not return after Stop — it is waiting out the child (a child must never outlive a stopped parent)")
	}

	// The delegation reported failure rather than inventing an answer.
	var sawSpawnResult bool
	for _, e := range out.res.Entries {
		if e.Type == "tool_result" && e.ToolName == "spawn_subagent" {
			sawSpawnResult = true
			if !strings.Contains(e.Text, `"success":false`) {
				t.Fatalf("a cancelled child must report success=false to its parent, got %q", e.Text)
			}
		}
	}
	if !sawSpawnResult {
		t.Fatal("no spawn_subagent tool result was recorded for the cancelled delegation")
	}

	if !child.sawCancel.Load() {
		t.Fatal("the child's model never observed cancellation: the parent's Stop did not reach the child's context")
	}
	if child.ranToCompletion.Load() {
		t.Fatal("the child ran to completion after its parent was stopped")
	}
}

// TestSpawn_ChildContextDerivesFromTheParentCall pins the mechanism the test
// above exercises, at the seam: the context the CHILD's model sees is a
// descendant of the context handed to the spawn tool call, so cancelling the
// parent's context cancels the child — no detached background context anywhere
// in the spawn path.
func TestSpawn_ChildContextDerivesFromTheParentCall(t *testing.T) {
	child := &cancelWatchingChildModel{started: make(chan struct{}), completionAfter: 30 * time.Second}
	parent := newParentForSpawn(t, child, 1.0, 0, 1, 5)
	ctx, _ := spawnCtx(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan spawnSubagentOutput, 1)
	go func() {
		resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "long task"}, "call-1")
		if err != nil {
			t.Errorf("spawn transport error: %v", err)
			done <- spawnSubagentOutput{}
			return
		}
		done <- parseSpawnOutput(t, resp)
	}()

	select {
	case <-child.started:
	case <-time.After(15 * time.Second):
		t.Fatal("child never started")
	}
	cancel()

	select {
	case out := <-done:
		if out.Success {
			t.Fatal("a child cancelled with its parent must report success=false")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("spawn did not return after the parent's context was cancelled")
	}
	if !child.sawCancel.Load() {
		t.Fatal("the child's context was not cancelled with the parent's")
	}
	// The parent's books are square: no reservation is left held by a child that
	// was cancelled rather than finished.
	parent.mu.Lock()
	resCost, resTok := parent.subagent.reservedCostUSD, parent.subagent.reservedTokens
	parent.mu.Unlock()
	if resCost != 0 || resTok != 0 {
		t.Fatalf("cancelled child leaked a budget reservation: cost=%v tokens=%d", resCost, resTok)
	}
}
