package agentcore

// #788 Run-level + wrapper-ordering integration for lifecycle hooks. Drives the
// real agentcore.Run loop / policyGuardedTool.Run with a scriptable Executor so
// the shared-seam acceptance (both modes), the block/context/audit outcomes,
// and the "hooks run before the policy gate, cannot widen" contract are pinned.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// recordingPolicy is a Policy double that counts RecordToolResult calls and
// captures the last recorded result text.
type recordingPolicy struct {
	recorded   atomic.Int32
	lastResult atomic.Value
}

func (p *recordingPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (p *recordingPolicy) RecordToolResult(_, _, resultText string, _ bool) {
	p.recorded.Add(1)
	p.lastResult.Store(resultText)
}
func (p *recordingPolicy) CanFinish(int) (bool, []string) { return true, nil }

// echoTool is a trivial native tool that returns fixed text and records whether
// it ran (so a pre-hook block can be shown to skip execution).
type echoTool struct {
	name string
	ran  *bool
	out  string
}

func (e echoTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: e.name, Description: "echo", Parameters: map[string]any{"type": "object"}}
}
func (e echoTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (e echoTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
func (e echoTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if e.ran != nil {
		*e.ran = true
	}
	return fantasy.NewTextResponse(e.out), nil
}

func TestPolicyGuardedTool_PreHookBlocksBeforePolicy(t *testing.T) {
	ran := false
	pol := &recordingPolicy{}
	engine := engineWith(t, &scriptExecutor{out: `{"decision":"block","reason":"blocked by org hook"}`}, nil,
		LifecycleHook{ID: "gate", Event: HookPreToolUse, Matcher: "*", Command: "c"})
	guarded := &policyGuardedTool{inner: echoTool{name: "bash", ran: &ran, out: "ran"}, policy: pol, hooks: engine}

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "blocked by org hook") {
		t.Errorf("pre-hook block should return the reason as an error response, got %+v", resp)
	}
	if ran {
		t.Error("pre-hook block must NOT execute the inner tool")
	}
	if pol.recorded.Load() != 0 {
		t.Error("a pre-hook block must not record a tool result (matches policy-block behavior)")
	}
}

func TestPolicyGuardedTool_PostHookAppendsContext(t *testing.T) {
	pol := &recordingPolicy{}
	engine := engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"formatter: clean"}`}, nil,
		LifecycleHook{ID: "fmt", Event: HookPostToolUse, Matcher: "*", Command: "c"})
	guarded := &policyGuardedTool{inner: echoTool{name: "edit_file", out: "edited"}, policy: pol, hooks: engine}

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if !strings.Contains(resp.Content, "edited") || !strings.Contains(resp.Content, "[hook:fmt] formatter: clean") {
		t.Errorf("post-hook fragment should be appended to the result, got %q", resp.Content)
	}
	// The recorded (policy/log) bytes must be the SAME appended text.
	if !strings.Contains(pol.lastResult.Load().(string), "formatter: clean") {
		t.Error("RecordToolResult must see the same appended bytes the model sees")
	}
}

func TestRun_UserPromptSubmitBlocks(t *testing.T) {
	session := NewLogSession()
	engineWith(t, &scriptExecutor{out: `{"decision":"block","reason":"prompt rejected"}`}, nil,
		LifecycleHook{ID: "ingress", Event: HookUserPromptSubmit, Command: "c"})
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("do it")}, label: "block"},
		Policy:     newRoundsPolicy(session, 0),
		Executor:   &scriptExecutor{out: `{"decision":"block","reason":"prompt rejected"}`},
		Model:      scriptedToolModel(nil),
		LogSession: session,
	})
	if !errors.Is(err, ErrBlockedByHook) {
		t.Fatalf("user_prompt_submit block should surface ErrBlockedByHook, got %v", err)
	}
}

func TestRun_UserPromptSubmitAddsContext(t *testing.T) {
	session := NewLogSession()
	engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"REMINDER: cite sources"}`}, nil,
		LifecycleHook{ID: "ctx", Event: HookUserPromptSubmit, Command: "c"})
	model := &capturingModel{slug: "hook-ctx-model", inputByCall: []int{10}}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("question")}, label: "ctx"},
		Policy:     newRoundsPolicy(session, 0),
		Executor:   &scriptExecutor{out: `{"decision":"continue","additional_context":"REMINDER: cite sources"}`},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The appended context must appear as a message the model saw.
	var saw bool
	for _, m := range model.call(0) {
		if strings.Contains(msgText(m), "REMINDER: cite sources") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("user_prompt_submit additional_context should be appended to the model input")
	}
}

func TestRun_TurnEndFiresOnNormalCompletion(t *testing.T) {
	session := NewLogSession()
	obs := &recordObserver{}
	engineWith(t, &scriptExecutor{out: `{"decision":"continue"}`}, obs,
		LifecycleHook{ID: "done", Event: HookTurnEnd, Command: "c"})
	_, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("go")}, label: "te"},
		Policy:     newRoundsPolicy(session, 0),
		Executor:   &scriptExecutor{out: `{"decision":"continue"}`},
		Observer:   obs,
		Model:      scriptedToolModel(nil),
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The shared-seam acceptance: hook.decision observed in a SCHEDULED run too.
	evs := obs.decisions()
	var sawTurnEnd bool
	for _, e := range evs {
		if e["event"] == string(HookTurnEnd) && e["hook_id"] == "done" {
			sawTurnEnd = true
		}
	}
	if !sawTurnEnd {
		t.Fatalf("turn_end hook should fire once on normal completion (events: %+v)", evs)
	}
}
