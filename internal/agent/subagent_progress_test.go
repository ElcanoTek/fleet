package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Sub-agent liveness + visibility (#1043 follow-up). Fake-LLM seam only — no
// real key, no network, no sandbox.

// recordingObserver captures every event a run forwards, for the progress
// assertions. Concurrency-safe: children stream from their own goroutines.
type recordingObserver struct {
	mu     sync.Mutex
	events []subagentEvent
}

type subagentEvent struct {
	typ     string
	payload map[string]any
}

func (o *recordingObserver) Observe(eventType string, payload map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	copied := make(map[string]any, len(payload))
	for k, v := range payload {
		copied[k] = v
	}
	o.events = append(o.events, subagentEvent{typ: eventType, payload: copied})
}

func (o *recordingObserver) progress() []subagentEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]subagentEvent, 0, len(o.events))
	for _, e := range o.events {
		if e.typ == SubagentProgressEvent {
			out = append(out, e)
		}
	}
	return out
}

func (o *recordingObserver) phases() []string {
	var out []string
	for _, e := range o.progress() {
		phase, _ := e.payload["phase"].(string)
		out = append(out, phase)
	}
	return out
}

// answeringChildModel writes an answer and stops — the ordinary shape of a
// useful child. It deliberately never calls confirm_audit: that is the whole
// point of the delegated finish gate.
type answeringChildModel struct{ answer string }

func (m *answeringChildModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: m.answer}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *answeringChildModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *answeringChildModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *answeringChildModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *answeringChildModel) Provider() string { return "mock" }
func (m *answeringChildModel) Model() string    { return "answering-child" }

// toolThenAnswerChildModel calls one tool, then answers. It gives the progress
// forwarder a real tool step to relabel.
type toolThenAnswerChildModel struct {
	mu    sync.Mutex
	calls int
}

func (m *toolThenAnswerChildModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	first := m.calls == 0
	m.calls++
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		if first {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "child-call-1",
				ToolCallName:  "task_tracker",
				ToolCallInput: `{"tasks":[{"title":"look it up","status":"done"}]}`,
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "the answer"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *toolThenAnswerChildModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *toolThenAnswerChildModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *toolThenAnswerChildModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *toolThenAnswerChildModel) Provider() string { return "mock" }
func (m *toolThenAnswerChildModel) Model() string    { return "tool-then-answer" }

// TestSpawn_ChildAnswersWithoutSelfAuditRitual is the regression test for the
// reported failure ("it spawns a sub-agent but nothing ever comes back"): a
// child that simply answers and stops must return that answer with success=true.
// Before the delegated finish gate it was refused the finish, re-prompted to
// read protocols/self-audit.md, and ran out of enforcement rounds — the parent
// got `[sub-agent produced no final answer]` (or audit narration) instead.
func TestSpawn_ChildAnswersWithoutSelfAuditRitual(t *testing.T) {
	parent := newParentForSpawn(t, &answeringChildModel{answer: "waves crash on the shore"}, 1.0, 0, 1, 5)
	ctx, _ := spawnCtx(t)

	resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "write a haiku"}, "call-1")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := parseSpawnOutput(t, resp)
	if !out.Success {
		t.Fatalf("a child that answered must report success=true; result=%q", out.Result)
	}
	if !strings.Contains(out.Result, "waves crash on the shore") {
		t.Fatalf("the child's answer must be returned verbatim to the parent; got %q", out.Result)
	}
	if strings.Contains(strings.ToLower(out.Result), "self-audit") {
		t.Fatalf("the child's answer must not carry audit-ritual narration; got %q", out.Result)
	}
}

// TestSpawn_StreamsChildProgressToTheParentObserver pins the visibility fix: a
// child's steps reach the parent's observer as attributed subagent.progress
// events — started first, the child's tool call in the middle, finished last
// with the spend and the trail. Without these the chat UI has nothing to show
// between the spawn arguments and the final result.
func TestSpawn_StreamsChildProgressToTheParentObserver(t *testing.T) {
	obs := &recordingObserver{}
	parent := newParentForSpawn(t, &toolThenAnswerChildModel{}, 1.0, 0, 1, 5)
	parent.nativeTools = tools.DefaultTools()
	parent.spawnObserver = obs
	ctx, _ := spawnCtx(t)

	resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "look something up", Role: "worker"}, "call-42")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := parseSpawnOutput(t, resp)

	events := obs.progress()
	if len(events) < 2 {
		t.Fatalf("expected progress events for the child, got %d (%v)", len(events), obs.phases())
	}
	if first, _ := events[0].payload["phase"].(string); first != subagentPhaseStarted {
		t.Fatalf("first progress event must be %q, got %q", subagentPhaseStarted, first)
	}
	last := events[len(events)-1]
	if phase, _ := last.payload["phase"].(string); phase != subagentPhaseFinished {
		t.Fatalf("last progress event must be %q, got %q (all: %v)", subagentPhaseFinished, phase, obs.phases())
	}

	// Every event must carry the correlation + identity fields a UI needs to
	// attach it to the right spawn chip (a turn may fan out several children).
	for i, e := range events {
		if got, _ := e.payload["tool_call_id"].(string); got != "call-42" {
			t.Fatalf("event %d missing the parent tool_call_id: %v", i, e.payload)
		}
		if got, _ := e.payload["child_session_id"].(string); got != out.ChildSessionID {
			t.Fatalf("event %d child_session_id = %q, want %q", i, got, out.ChildSessionID)
		}
		if got, _ := e.payload["role"].(string); got != SubagentRoleWorker {
			t.Fatalf("event %d role = %q, want worker", i, got)
		}
	}

	// The child's tool call is reported as a step, and the trail is echoed back
	// to the model in the tool result so an unsuccessful child is still legible.
	var sawTool bool
	for _, e := range events {
		if phase, _ := e.payload["phase"].(string); phase == subagentPhaseTool {
			if tool, _ := e.payload["tool"].(string); tool == "task_tracker" {
				sawTool = true
			}
		}
	}
	if !sawTool {
		t.Fatalf("the child's tool call must be forwarded as a tool-phase event; phases=%v", obs.phases())
	}
	if out.Steps < 1 {
		t.Fatalf("the spawn result must report the child's step count, got %d", out.Steps)
	}
	if !containsStr(out.ToolsUsed, "task_tracker") {
		t.Fatalf("the spawn result must report tools_used, got %v", out.ToolsUsed)
	}
	if success, _ := last.payload["success"].(bool); !success {
		t.Fatalf("terminal event must report success for a child that answered: %v", last.payload)
	}
}

// TestSpawn_WithoutObserverIsUnchanged proves the forwarder is optional: a run
// with nobody watching spawns exactly as before (nil observer, no panic).
func TestSpawn_WithoutObserverIsUnchanged(t *testing.T) {
	parent := newParentForSpawn(t, &answeringChildModel{answer: "done"}, 1.0, 0, 1, 5)
	parent.spawnObserver = nil
	ctx, _ := spawnCtx(t)

	resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "t"}, "call-1")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if out := parseSpawnOutput(t, resp); !out.Success {
		t.Fatalf("spawn without an observer must still work: %+v", out)
	}
}

// TestChildPromptCarriesTheSubagentContract pins that every child (either role)
// is told what a child is — the inherited parent prompt otherwise tells it to
// behave like the run it was cloned from, including the audit ritual.
func TestChildPromptCarriesTheSubagentContract(t *testing.T) {
	for _, role := range []string{SubagentRoleExplore, SubagentRoleWorker} {
		t.Run(role, func(t *testing.T) {
			parent := newParentForSpawn(t, &answeringChildModel{answer: "x"}, 1.0, 0, 1, 5)
			child := parent.buildChild(role, parent.model, nil, nil, 0.1, 0, 0)
			if !strings.Contains(child.systemPrompt, "You are a sub-agent") {
				t.Fatal("child prompt must carry the sub-agent contract section")
			}
			if !strings.Contains(child.systemPrompt, "self-audit") {
				t.Fatal("child prompt must tell the child not to run the self-audit ritual")
			}
			if !strings.HasPrefix(child.systemPrompt, parent.systemPrompt) {
				t.Fatal("the child's prompt must still START with the inherited parent prompt")
			}
			if role == SubagentRoleExplore && !strings.Contains(child.systemPrompt, "Read-only research role") {
				t.Fatal("an explore child must keep its read-only section")
			}
		})
	}
}

// TestChildRunUsesTheDelegatedPolicy pins the wiring end to end: the child's own
// Execute installs the delegated policy (finishes without the ritual) while a
// root run keeps the full gate.
func TestChildRunUsesTheDelegatedPolicy(t *testing.T) {
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	parent := newParentForSpawn(t, &answeringChildModel{answer: "child answer"}, 1.0, 0, 1, 5)
	child := parent.buildChild(SubagentRoleWorker, parent.model, nil, nil, 0.5, 0, 0)
	if !child.isDelegatedChild() {
		t.Fatal("buildChild must stamp the role that marks a run delegated")
	}
	if parent.isDelegatedChild() {
		t.Fatal("a root run must never look delegated")
	}
	childCtx, _ := spawnCtx(t)
	if err := child.Execute(childCtx, "answer the question"); err != nil {
		t.Fatalf("child Execute: %v", err)
	}
	if got := strings.TrimSpace(latestAssistantText(child.LogSession())); got != "child answer" {
		t.Fatalf("child run produced %q, want its answer — the delegated gate must let it finish", got)
	}
}

// TestSummarizeToolInput pins the argument summary that makes a live step line
// readable ("query=…") instead of a raw JSON blob, and its fallbacks.
func TestSummarizeToolInput(t *testing.T) {
	got := summarizeToolInput(`{"query":"revenue by month","limit":5}`)
	if got != "limit=5, query=revenue by month" {
		t.Fatalf("summarizeToolInput = %q", got)
	}
	if got := summarizeToolInput("not json at all"); got != "not json at all" {
		t.Fatalf("non-JSON input must fall back to the raw text, got %q", got)
	}
	if got := summarizeToolInput("  "); got != "" {
		t.Fatalf("empty input must summarize to empty, got %q", got)
	}
	long := strings.Repeat("x", 500)
	if s := summarizeToolInput(`{"q":"` + long + `"}`); len([]rune(s)) > subagentDetailChars+1 {
		t.Fatalf("summary must stay bounded, got %d runes", len([]rune(s)))
	}
}

// TestChildProgress_CoalescesTextPreviews pins the rate limit: a child writing
// hundreds of deltas must not flood the parent's stream, while tool steps are
// forwarded one for one.
func TestChildProgress_CoalescesTextPreviews(t *testing.T) {
	obs := &recordingObserver{}
	p := newChildProgress(obs, "call-1", "subagent-x", SubagentRoleExplore)
	for i := 0; i < 200; i++ {
		p.Observe("text.delta", map[string]any{"text": "word "})
	}
	if n := len(obs.progress()); n > 2 {
		t.Fatalf("text deltas must be coalesced into at most a couple of previews, got %d", n)
	}
	for i := 0; i < 3; i++ {
		p.Observe("tool.call", map[string]any{"name": "view_file", "input": `{"path":"a.txt"}`})
	}
	steps, toolsUsed := p.snapshot()
	if steps != 3 {
		t.Fatalf("steps = %d, want 3 (every child tool call counts)", steps)
	}
	if len(toolsUsed) != 1 || toolsUsed[0] != "view_file" {
		t.Fatalf("toolsUsed = %v, want [view_file] (deduped)", toolsUsed)
	}
}

// TestNilChildProgressIsSafe pins the nil-receiver contract the spawn path
// relies on when no one is watching the run.
func TestNilChildProgressIsSafe(t *testing.T) {
	var p *childProgress
	if got := newChildProgress(nil, "c", "s", "explore"); got != nil {
		t.Fatal("newChildProgress(nil parent) must return nil")
	}
	if p.observer() != nil {
		t.Fatal("a nil forwarder must produce a nil Observer, not a typed nil")
	}
	p.Observe("tool.call", map[string]any{"name": "bash"})
	p.started("t", "/w", "m")
	p.finished(true, agentcore.RunUsage{}, 0, "")
	if steps, tools := p.snapshot(); steps != 0 || tools != nil {
		t.Fatal("a nil forwarder must snapshot an empty trail")
	}
}

// TestInteractiveTurn_SpawnHostCarriesTheTurnObserver pins the chat wiring: the
// host the interactive spawn tool binds to must carry the turn's observer, or a
// chat delegation streams nothing at all.
func TestInteractiveTurn_SpawnHostCarriesTheTurnObserver(t *testing.T) {
	// Keep the child's log file and isolated workdir inside the test's temp
	// dirs: without both, a child writes its sibling session log and its
	// subagents/ subdir into the package directory.
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	ctx, _ := spawnCtx(t)
	obs := &recordingObserver{}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Model:        &answeringChildModel{answer: "hi"},
		MaxTokens:    1024,
		Config:       &config.Config{MaxIterations: 10, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Subagent:     SubagentOptions{Enabled: true, MaxDepth: 1, MaxChildren: 5},
	}
	host := newInteractiveSpawnHost(tc, agentcore.NewInteractivePolicy(1.0, 0, nil, nil), obs)
	if host.spawnObserver == nil {
		t.Fatal("the interactive spawn host must carry the turn's observer for child progress")
	}
	resp, err := host.spawn(ctx, spawnSubagentInput{Task: "do it"}, "call-7")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := parseSpawnOutput(t, resp)
	if !out.Success {
		t.Fatalf("a chat child that answered must succeed: %+v", out)
	}
	phases := obs.phases()
	if len(phases) == 0 {
		t.Fatal("a chat delegation must stream progress events to the turn's sink")
	}
	// The payload must be JSON-serializable for the SSE frame.
	for _, e := range obs.progress() {
		if _, err := json.Marshal(e.payload); err != nil {
			t.Fatalf("progress payload must marshal for SSE: %v", err)
		}
	}
}
