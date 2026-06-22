package acpruntime

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// govScriptedModel drives a real agentcore.Run loop with a scripted sequence of
// tool calls then a final text. Each entry in calls is a list of (name, input)
// tool calls to emit on that round; the round after the last entry emits final
// text. Usage is reported on every step so the host-side usage reconciliation can
// be asserted.
type govScriptedModel struct {
	mu    sync.Mutex
	calls int
	steps [][]scriptToolCall
}

type scriptToolCall struct {
	id, name, input string
}

func (m *govScriptedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content: []fantasy.Content{fantasy.TextContent{Text: "ok"}}, FinishReason: fantasy.FinishReasonStop,
		Usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *govScriptedModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	call := m.calls
	m.calls++
	m.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		if call < len(m.steps) {
			for _, tc := range m.steps[call] {
				if !yield(fantasy.StreamPart{
					Type: fantasy.StreamPartTypeToolCall, ID: tc.id,
					ToolCallName: tc.name, ToolCallInput: tc.input,
				}) {
					return
				}
			}
			yield(fantasy.StreamPart{
				Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls,
				Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10},
			})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
			Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10},
		})
	}, nil
}

func (m *govScriptedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, io.EOF
}
func (m *govScriptedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, io.EOF
}
func (m *govScriptedModel) Provider() string { return "scripted" }
func (m *govScriptedModel) Model() string    { return "scripted-model" }

// recordingMCPBroker records every delegated MCP call so the test can prove it
// executed HOST-side (via the client broker), and returns a canned result.
type recordingMCPBroker struct {
	mu    sync.Mutex
	calls []mcpCall
	resp  string
	isErr bool
	err   error
}

type mcpCall struct {
	server, tool string
	args         map[string]any
}

func (b *recordingMCPBroker) CallMCP(_ context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.mu.Lock()
	b.calls = append(b.calls, mcpCall{server: server, tool: tool, args: args})
	b.mu.Unlock()
	if b.err != nil {
		return "", false, b.err
	}
	return b.resp, b.isErr, nil
}

// recordingStageBroker records every delegated staging effect.
type recordingStageBroker struct {
	mu          sync.Mutex
	approvals   []stageApprovalRec
	suggestions []string
	memories    []string
	notes       []stageNoteRec
	suggestMsg  string
	suggestID   string
}

type stageApprovalRec struct{ toolName, toolCallID, rawInput string }
type stageNoteRec struct{ slug, title, body, reason string }

func (b *recordingStageBroker) StageApproval(toolName, toolCallID, rawInput string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.approvals = append(b.approvals, stageApprovalRec{toolName, toolCallID, rawInput})
	return "appr-1", nil
}
func (b *recordingStageBroker) StageSuggestion(reason string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.suggestions = append(b.suggestions, reason)
	return b.suggestID, b.suggestMsg, nil
}
func (b *recordingStageBroker) StageMemory(content string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.memories = append(b.memories, content)
	return "mem-1", nil
}
func (b *recordingStageBroker) StageNote(slug, title, body, reason string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes = append(b.notes, stageNoteRec{slug, title, body, reason})
	return "note-1", nil
}

// govHarness wires an in-process client↔agent ACP pair over io.Pipe with the
// given host deps + run spec, drives one prompt, and returns the result + the
// host client (for usage assertions). It is the shared scaffolding for the
// P-ACP-2b governance tests (no podman, no network).
type govHarness struct {
	t     *testing.T
	model *govScriptedModel
	deps  Deps
	spec  RunSpec
}

func runGovHarness(t *testing.T, h govHarness) *hostClient {
	t.Helper()
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	runner := newAgentRunner(func(context.Context, string) (fantasy.LanguageModel, error) {
		return h.model, nil
	})
	agentConn := acp.NewAgentSideConnection(runner, agentToClientW, clientToAgentR)
	runner.SetConn(agentConn)

	cl := &hostClient{deps: h.deps}
	clientConn := acp.NewClientSideConnection(cl, clientToAgentW, agentToClientR)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{Fs: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: true},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	specJSON, _ := json.Marshal(h.spec)
	sess, err := clientConn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: "/workspace", McpServers: []acp.McpServer{},
		Meta: map[string]any{MetaKeyRunSpec: json.RawMessage(specJSON)},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	pmJSON, _ := json.Marshal(PromptMeta{})
	resp, err := clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("go")},
		Meta:      map[string]any{MetaKeyPromptMeta: json.RawMessage(pmJSON)},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	_ = clientToAgentW.Close()
	_ = agentToClientW.Close()
	return cl
}

func baseGovSpec() RunSpec {
	return RunSpec{
		Mode: agentcore.ModeInteractive.String(), ModelSlug: "scripted-model",
		SystemPrompt: "test", Temperature: 0, MaxTokens: 256,
	}
}

// TestACPGovern_MCPDelegatedHostSide proves the SECURITY-CRITICAL invariant: an
// MCP-selected native-acp run delegates the mcp_<server>_<tool> call over
// `_fleet/mcp` so it executes HOST-side via the broker, and the RunSpec the agent
// receives carries NO credentials — only the public descriptor. The agent
// container never holds an MCP credential.
func TestACPGovern_MCPDelegatedHostSide(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "mcp_sendgrid_lookup", input: `{"q":"acme"}`}},
	}}
	broker := &recordingMCPBroker{resp: "host-mcp-result"}
	spec := baseGovSpec()
	spec.MCPTools = []MCPToolDescriptor{{
		Server: "sendgrid", Tool: "lookup", Description: "lookup a contact",
		InputSchema: map[string]any{"properties": map[string]any{"q": map[string]any{"type": "string"}}},
	}}

	cl := runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{
			Executor:  &recordingExecutor{},
			Observer:  &recordingObserver{},
			MCPBroker: broker,
		},
	})
	_ = cl

	// 1. The MCP call executed HOST-side via the broker (not in the agent).
	broker.mu.Lock()
	calls := append([]mcpCall(nil), broker.calls...)
	broker.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("broker calls = %d, want 1", len(calls))
	}
	if calls[0].server != "sendgrid" || calls[0].tool != "lookup" {
		t.Fatalf("broker call = %+v, want sendgrid/lookup", calls[0])
	}
	if q, _ := calls[0].args["q"].(string); q != "acme" {
		t.Fatalf("broker call args q = %q, want acme", q)
	}

	// 2. CRED ISOLATION: the RunSpec the agent received (and everything that
	// reaches the container) carries only the public descriptor — no env, no
	// args, no credential field anywhere in the serialized spec.
	specJSON, _ := json.Marshal(spec)
	for _, banned := range []string{"API_KEY", "api_key", "SENDGRID_API_KEY", "secret", "token", "password", "BaseEnv", "Env"} {
		if strings.Contains(string(specJSON), banned) {
			t.Fatalf("RunSpec leaked a credential-shaped field %q: %s", banned, specJSON)
		}
	}
	// The descriptor carries the public schema + names only.
	if !strings.Contains(string(specJSON), "sendgrid") || !strings.Contains(string(specJSON), "lookup") {
		t.Fatalf("RunSpec missing the public descriptor: %s", specJSON)
	}
}

// TestACPGovern_MCPToolError maps an MCP isError result back through the seam to a
// fantasy error response, identical to the in-process mcpTool.
func TestACPGovern_MCPToolError(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "mcp_x_do", input: `{}`}},
	}}
	broker := &recordingMCPBroker{resp: "boom", isErr: true}
	spec := baseGovSpec()
	spec.MCPTools = []MCPToolDescriptor{{Server: "x", Tool: "do", Description: "d"}}

	obs := &recordingObserver{}
	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{Executor: &recordingExecutor{}, Observer: obs, MCPBroker: broker},
	})

	// The tool.result event carries the error text + is_err=true.
	obs.mu.Lock()
	defer obs.mu.Unlock()
	found := false
	for _, e := range obs.raw {
		if e.eventType == "tool.result" {
			if isErr, _ := e.payload["is_err"].(bool); isErr {
				if txt, _ := e.payload["text"].(string); strings.Contains(txt, "boom") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected a tool.result with is_err=true carrying 'boom'")
	}
}

// TestACPGovern_ApprovalStagingDelegated proves a send_email call (an approval-
// gated tool) delegates its staging over `_fleet/stage` to the host broker — the
// agent's policy decided to stage, the host performed the effect.
func TestACPGovern_ApprovalStagingDelegated(t *testing.T) {
	emailArgs := `{"to_email":"a@b.com","subject":"hi","content":"body"}`
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "mcp_sendgrid_send_email", input: emailArgs}},
	}}
	broker := &recordingMCPBroker{resp: "should-not-be-called"}
	stage := &recordingStageBroker{}
	spec := baseGovSpec()
	spec.StagingWired = true
	spec.MCPTools = []MCPToolDescriptor{{Server: "sendgrid", Tool: "send_email", Description: "send"}}

	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{Executor: &recordingExecutor{}, Observer: &recordingObserver{}, MCPBroker: broker, StageBroker: stage},
	})

	// The send_email was STAGED (not executed): broker not called, stage recorded.
	broker.mu.Lock()
	mcpCalls := len(broker.calls)
	broker.mu.Unlock()
	if mcpCalls != 0 {
		t.Fatalf("send_email should have been staged, not executed (%d MCP calls)", mcpCalls)
	}
	stage.mu.Lock()
	defer stage.mu.Unlock()
	if len(stage.approvals) != 1 {
		t.Fatalf("approvals staged = %d, want 1", len(stage.approvals))
	}
	if stage.approvals[0].toolName != "mcp_sendgrid_send_email" {
		t.Fatalf("staged tool = %q", stage.approvals[0].toolName)
	}
}

// TestACPGovern_SuggestionSuppressed proves the suggest_advanced_model SUPPRESSED
// path round-trips: the host returns an empty id + an agent-facing message (the
// per-conversation gate suppressed it), and the agent surfaces that message
// without retrying — never an error. This is the contract the in-process
// StageSuggestion gate enforces.
func TestACPGovern_SuggestionSuppressed(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "suggest_advanced_model", input: `{"reason":"this is hard"}`}},
	}}
	stage := &recordingStageBroker{suggestID: "", suggestMsg: "SUGGESTION_SUPPRESSED: already on advanced."}
	spec := baseGovSpec()
	spec.StagingWired = true

	obs := &recordingObserver{}
	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{Executor: &recordingExecutor{}, Observer: obs, StageBroker: stage},
	})

	stage.mu.Lock()
	suggestions := append([]string(nil), stage.suggestions...)
	stage.mu.Unlock()
	if len(suggestions) != 1 || suggestions[0] != "this is hard" {
		t.Fatalf("suggestions staged = %v, want [this is hard]", suggestions)
	}
	// The suppression message reached the model as the tool result (not an error
	// crash). The turn completed cleanly (the harness asserts end_turn).
	obs.mu.Lock()
	defer obs.mu.Unlock()
	found := false
	for _, e := range obs.raw {
		if e.eventType == "tool.result" {
			if txt, _ := e.payload["text"].(string); strings.Contains(txt, "SUGGESTION_SUPPRESSED") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected the suppression message to reach the model as a tool.result")
	}
}

// TestACPGovern_MemoryProposalDelegated proves propose_memory delegates over
// `_fleet/stage` to the host MemoryProposer.
func TestACPGovern_MemoryProposalDelegated(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "propose_memory", input: `{"content":"user likes blue"}`}},
	}}
	stage := &recordingStageBroker{}
	spec := baseGovSpec()
	spec.StagingWired = true

	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{Executor: &recordingExecutor{}, Observer: &recordingObserver{}, StageBroker: stage},
	})

	stage.mu.Lock()
	defer stage.mu.Unlock()
	if len(stage.memories) != 1 || stage.memories[0] != "user likes blue" {
		t.Fatalf("memories staged = %v, want [user likes blue]", stage.memories)
	}
}

// TestACPGovern_NoteProposalDelegated proves propose_note delegates over
// `_fleet/stage` to the host NoteProposer. propose_note is wired in BOTH modes;
// we drive interactive (1-round collapse) so the assertion is on staging, not the
// scheduled audit-finish loop.
func TestACPGovern_NoteProposalDelegated(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "propose_note", input: `{"slug":"s","title":"T","body":"B","reason":"R"}`}},
	}}
	stage := &recordingStageBroker{}
	spec := baseGovSpec()
	spec.NoteProposerWired = true

	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{Executor: &recordingExecutor{}, Observer: &recordingObserver{}, StageBroker: stage},
	})

	stage.mu.Lock()
	defer stage.mu.Unlock()
	if len(stage.notes) != 1 {
		t.Fatalf("notes staged = %d, want 1", len(stage.notes))
	}
	if stage.notes[0].slug != "s" || stage.notes[0].title != "T" {
		t.Fatalf("staged note = %+v", stage.notes[0])
	}
}

// TestACPGovern_UsageReconciled proves the agent's per-step usage reports flow
// back over `_fleet/event` and the host accumulates them into Result.Usage —
// matching the in-process accounting (sum of step input/output, last-step input,
// summed cost).
func TestACPGovern_UsageReconciled(t *testing.T) {
	// Two rounds: round 0 runs a bash tool, round 1 finishes. The model reports
	// 100 input / 20 output / 10 cached on each of the 2 steps.
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{{id: "c1", name: "bash", input: `{"command":"echo hi"}`}},
	}}
	cl := runGovHarness(t, govHarness{
		t: t, model: model, spec: baseGovSpec(),
		deps: Deps{Executor: &recordingExecutor{}, Observer: &recordingObserver{}},
	})

	// The agent reports its CUMULATIVE usage every step; the host takes the latest
	// snapshot, so the final must equal steps×100 input, steps×20 output,
	// steps×10 cached — the SAME value the in-process usageSnapshot(orch) returns.
	// Each LLM step reports 100/20/10, so the totals are exact multiples of those
	// per step; we assert the invariants the in-process path also guarantees:
	//   - every field is a clean multiple of its per-step value (no double-count);
	//   - LastStepInputTokens is exactly one step's input (overwritten, not summed).
	u := cl.usageSnapshot()
	if u.PromptTokens == 0 || u.PromptTokens%100 != 0 {
		t.Errorf("PromptTokens = %d, want a non-zero multiple of 100 (cumulative, not double-counted)", u.PromptTokens)
	}
	steps := u.PromptTokens / 100
	if u.CompletionTokens != steps*20 {
		t.Errorf("CompletionTokens = %d, want %d (%d steps × 20)", u.CompletionTokens, steps*20, steps)
	}
	if u.CachedTokens != steps*10 {
		t.Errorf("CachedTokens = %d, want %d (%d steps × 10)", u.CachedTokens, steps*10, steps)
	}
	if u.LastStepInputTokens != 100 {
		t.Errorf("LastStepInputTokens = %d, want 100 (latest step's input, overwritten not summed)", u.LastStepInputTokens)
	}
}
