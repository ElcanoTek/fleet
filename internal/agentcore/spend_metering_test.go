package agentcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// Spend metering on every run outcome. The provider bills a completion when it
// reports the completion's usage (the stream's finish part), whatever happens
// to the run afterwards: a Stop while the step's tools run, a provider error on
// a later call, a contained panic. These tests pin that Result.Usage — what the
// chat driver records in turn_metrics — and the session-log counters — what a
// scheduled run records in task_iterations — carry that usage on each of those
// exits.

type blockToolInput struct{}

// meteredPassPolicy lets every finish through and accounts into a state bound
// to the run's session log, as the scheduled policy does.
type meteredPassPolicy struct {
	passPolicy
	orch *orchestrationState
}

func (p meteredPassPolicy) orchestration() *orchestrationState { return p.orch }

// toolThenFinishModel streams one tool call and a ToolCalls finish part that
// reports usage, then (on any later call) a plain stop.
func toolThenFinishModel(toolName string, usage fantasy.Usage) *mockModel {
	var streams atomic.Int32
	return &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		n := streams.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			if n == 1 {
				if !yield(fantasy.StreamPart{
					Type:          fantasy.StreamPartTypeToolCall,
					ID:            "call-1",
					ToolCallName:  toolName,
					ToolCallInput: "{}",
				}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: usage})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
}

// TestStopWhileToolsRunKeepsTheStepsUsage: fantasy reads a step's finish-part
// usage, runs the step's tools, and returns ctx.Err() without closing the step
// when the run is cancelled meanwhile. The completion was billed, so the
// cancelled run's usage — and a scheduled run's session-log counters — must
// include it.
func TestStopWhileToolsRunKeepsTheStepsUsage(t *testing.T) {
	for _, mode := range []Mode{ModeInteractive, ModeScheduled} {
		t.Run(mode.String(), func(t *testing.T) {
			started := make(chan struct{})
			tool := fantasy.NewAgentTool("block_tool", "blocks until the run is stopped",
				func(ctx context.Context, _ blockToolInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
					close(started)
					<-ctx.Done()
					return fantasy.NewTextResponse("stopped"), nil
				})
			session := NewLogSession()
			var policy Policy = NewInteractivePolicy(0, 0, nil, nil)
			if mode == ModeScheduled {
				policy = NewScheduledPolicy(session, 0, 0)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				select {
				case <-started:
					cancel()
				case <-time.After(10 * time.Second):
				}
			}()
			res, err := Run(ctx, mode, RunConfig{NativeTools: []fantasy.AgentTool{tool}}, Deps{
				Input:      stubInput{system: "sys", user: "run the tool", label: "stop-mid-tool"},
				Policy:     policy,
				Model:      toolThenFinishModel("block_tool", fantasy.Usage{InputTokens: 1000, OutputTokens: 50}),
				LogSession: session,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.Cancelled {
				t.Fatalf("Result.Cancelled = false, want a cancelled run")
			}
			if res.Usage.PromptTokens != 1000 || res.Usage.CompletionTokens != 50 {
				t.Fatalf("cancelled run usage = %d/%d, want 1000/50 (the completion whose tools were running)",
					res.Usage.PromptTokens, res.Usage.CompletionTokens)
			}
			if mode == ModeScheduled && (session.PromptTokens != 1000 || session.CompletionTokens != 50) {
				t.Fatalf("session log usage = %d/%d, want 1000/50", session.PromptTokens, session.CompletionTokens)
			}
		})
	}
}

// TestCompletionCrossingTheCeilingStillRunsItsTools: a completion is metered
// when its step closes, after its own tool calls ran, so the calls of the
// completion that crosses a cost/token ceiling are checked against the totals
// before it and run; the next step's budget guard then stops the run before
// another completion is bought.
func TestCompletionCrossingTheCeilingStillRunsItsTools(t *testing.T) {
	for _, mode := range []Mode{ModeInteractive, ModeScheduled} {
		t.Run(mode.String(), func(t *testing.T) {
			var ran atomic.Bool
			tool := fantasy.NewAgentTool("write_tool", "does the work",
				func(context.Context, blockToolInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					ran.Store(true)
					return fantasy.NewTextResponse("written"), nil
				})
			session := NewLogSession()
			var policy Policy = NewInteractivePolicy(0, 500, nil, nil)
			if mode == ModeScheduled {
				policy = NewScheduledPolicy(session, 0, 500)
			}
			model := toolThenFinishModel("write_tool", fantasy.Usage{InputTokens: 1000, OutputTokens: 50})
			res, err := Run(context.Background(), mode, RunConfig{NativeTools: []fantasy.AgentTool{tool}}, Deps{
				Input:      stubInput{system: "sys", user: "write it", label: "ceiling-crossing"},
				Policy:     policy,
				Model:      model,
				LogSession: session,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !ran.Load() {
				t.Fatal("the tool call of the completion that crossed the ceiling was refused")
			}
			if !res.StoppedByBudget {
				t.Fatalf("run not stopped by its token ceiling: %+v", res)
			}
			model.mu.Lock()
			calls := model.callCount
			model.mu.Unlock()
			if calls != 1 {
				t.Fatalf("provider calls = %d, want 1 (no completion after the ceiling)", calls)
			}
			if res.Usage.PromptTokens != 1000 || res.Usage.CompletionTokens != 50 {
				t.Fatalf("run usage = %d/%d, want 1000/50", res.Usage.PromptTokens, res.Usage.CompletionTokens)
			}
		})
	}
}

// TestProviderErrorAfterAPaidRoundKeepsItsUsage: a run whose first round was
// billed and ran no tool, then failed on its next provider call, still returns
// the first round's usage with the error (the failed call reported none).
func TestProviderErrorAfterAPaidRoundKeepsItsUsage(t *testing.T) {
	var streams atomic.Int32
	model := &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		if streams.Add(1) == 1 {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
					Usage: fantasy.Usage{InputTokens: 1000, OutputTokens: 50}})
			}, nil
		}
		return nil, &fantasy.ProviderError{StatusCode: 400, Message: "bad request"}
	}}
	res, err := Run(context.Background(), ModeInteractive, RunConfig{}, Deps{
		Input:  stubInput{system: "sys", user: "answer", label: "error-after-paid-round"},
		Policy: NewInteractivePolicy(0, 0, nil, nil),
		Model:  model,
		// The first round wrote nothing, so the finalize hook asks for one
		// more round — the one the provider then refuses.
		Finalize: func(_ context.Context, in FinalizeInput) (string, error) {
			if in.Redrive != nil {
				in.Redrive("answer in words")
			}
			return "", nil
		},
	})
	if err == nil {
		t.Fatal("Run succeeded, want the provider error")
	}
	if res.Usage.PromptTokens != 1000 || res.Usage.CompletionTokens != 50 {
		t.Fatalf("failed run usage = %d/%d, want 1000/50 (the billed first round)",
			res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}

// TestFailedRunAfterAFallbackSwapNamesTheServingModel: the primary refuses
// the first call, the fallback bills a tool step and then refuses too. The
// failed run's usage is the fallback's completion, and the result names the
// fallback, so the turn's spend is attributed to the model that served it.
func TestFailedRunAfterAFallbackSwapNamesTheServingModel(t *testing.T) {
	t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "0")
	refuse := func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return nil, &fantasy.ProviderError{StatusCode: 400, Message: "bad request"}
	}
	primary := &namedMockModel{name: "primary-model"}
	primary.streamFunc = refuse
	fallback := &namedMockModel{name: "fallback-model"}
	var fallbackCalls atomic.Int32
	fallback.streamFunc = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		if fallbackCalls.Add(1) == 1 {
			return toolThenFinishModel("quick_tool", fantasy.Usage{InputTokens: 1000, OutputTokens: 50}).streamFunc(ctx, call)
		}
		return refuse(ctx, call)
	}
	tool := fantasy.NewAgentTool("quick_tool", "returns at once",
		func(context.Context, blockToolInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("done"), nil
		})
	res, err := Run(context.Background(), ModeInteractive, RunConfig{NativeTools: []fantasy.AgentTool{tool}}, Deps{
		Input:         stubInput{system: "sys", user: "use the tool", label: "swap-then-fail"},
		Policy:        NewInteractivePolicy(0, 0, nil, nil),
		Model:         primary,
		FallbackModel: fallback,
	})
	if err == nil {
		t.Fatal("Run succeeded, want the fallback's refusal")
	}
	if fallbackCalls.Load() < 2 {
		t.Fatalf("fallback calls = %d, want the swap and a second, refused call", fallbackCalls.Load())
	}
	if res.ModelSlug != "fallback-model" {
		t.Fatalf("failed run ModelSlug = %q, want the serving fallback-model", res.ModelSlug)
	}
	if res.Usage.PromptTokens != 1000 || res.Usage.CompletionTokens != 50 {
		t.Fatalf("failed run usage = %d/%d, want 1000/50", res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}

// TestContainedRunPanicKeepsItsUsage: a panic the run backstop contains (here
// in the finalize hook, after a billed round) returns the run's usage with the
// opaque error rather than an empty Result.
func TestContainedRunPanicKeepsItsUsage(t *testing.T) {
	capturePanicEvents(t)
	res, err := Run(context.Background(), ModeInteractive, RunConfig{ConversationID: "conv-panic-usage"}, Deps{
		Input:  stubInput{system: "sys", user: "work", label: "panic-usage"},
		Policy: NewInteractivePolicy(0, 0, nil, nil),
		Model:  &mockModel{streamFunc: streamStop()},
		Finalize: func(context.Context, FinalizeInput) (string, error) {
			panic("finalize raw panic")
		},
	})
	if !errors.Is(err, ErrRunBoundaryPanic) {
		t.Fatalf("Run error = %v, want ErrRunBoundaryPanic", err)
	}
	if res.Usage.PromptTokens != 50 || res.Usage.CompletionTokens != 10 {
		t.Fatalf("panicked run usage = %d/%d, want 50/10", res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}

// TestCompletedRunMetersEachCompletionOnce: metering at the finish part must
// not also meter at the step's close — a two-step run (tool call, then the
// answer) reports exactly the two completions' usage.
func TestCompletedRunMetersEachCompletionOnce(t *testing.T) {
	tool := fantasy.NewAgentTool("quick_tool", "returns at once",
		func(context.Context, blockToolInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("done"), nil
		})
	var streams atomic.Int32
	model := &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		n := streams.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			if n == 1 {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "quick_tool", ToolCallInput: "{}"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls,
					Usage: fantasy.Usage{InputTokens: 1000, OutputTokens: 50}})
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "all done"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
				Usage: fantasy.Usage{InputTokens: 1200, OutputTokens: 30}})
		}, nil
	}}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeScheduled, RunConfig{NativeTools: []fantasy.AgentTool{tool}}, Deps{
		Input:      stubInput{system: "sys", user: "use the tool", label: "meter-once"},
		Policy:     meteredPassPolicy{orch: newOrchestrationState(session)},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.PromptTokens != 2200 || res.Usage.CompletionTokens != 80 {
		t.Fatalf("run usage = %d/%d, want 2200/80 (each completion once)", res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
	if session.PromptTokens != 2200 || session.CompletionTokens != 80 {
		t.Fatalf("session log usage = %d/%d, want 2200/80", session.PromptTokens, session.CompletionTokens)
	}
}
