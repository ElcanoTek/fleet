package acpingress

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// ── Test harness: an in-memory fake editor (ACP client) ↔ a real IngressAgent
// over an io.Pipe pair, with a TurnEngine that drives the REAL governed
// interactive turn (agent.RunInteractiveTurn → agentcore.Run, the same loop the
// web path uses) against a scripted fake model. No DB, no network, no podman.
// The fake stores are in-memory; the fake model is deterministic. This exercises
// the REAL InteractivePolicy + the REAL Observer event vocabulary + the REAL
// staging gates — so the tests prove governance fidelity, not a mock of it.

// scriptedModel emits a fixed sequence of stream parts per Stream call. Each
// "round" of the loop pulls the next entry; the final entry should end the turn.
type scriptedModel struct {
	mu     sync.Mutex
	rounds [][]fantasy.StreamPart
	call   int
}

func (m *scriptedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: "ok"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *scriptedModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	idx := m.call
	if idx >= len(m.rounds) {
		idx = len(m.rounds) - 1
	}
	parts := m.rounds[idx]
	m.call++
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *scriptedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, io.EOF
}
func (m *scriptedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, io.EOF
}
func (m *scriptedModel) Provider() string { return "scripted" }
func (m *scriptedModel) Model() string    { return "scripted-model" }

// textRound emits user-visible text then finishes the turn.
func textRound(text string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: text},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}
}

// bashRound emits a bash tool call then finishes the round with tool_calls.
func bashRound(id, command string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: "bash", ToolCallInput: `{"command":` + jsonString(command) + `}`},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
	}
}

// toolRound emits a single named tool call (arbitrary input) then finishes the
// round with tool_calls.
func toolRound(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
	}
}

func jsonString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

// recordingObserver mirrors the acpruntime test helper: it records the run's
// (eventType, payload) stream so a test can assert the SAME governance/observer
// events the web path emits.
type recordingObserver struct {
	mu  sync.Mutex
	ev  []string
	raw []obsEvent
}

type obsEvent struct {
	kind    string
	payload map[string]any
}

func (o *recordingObserver) Observe(eventType string, payload map[string]any) {
	o.mu.Lock()
	o.ev = append(o.ev, eventType)
	o.raw = append(o.raw, obsEvent{kind: eventType, payload: payload})
	o.mu.Unlock()
}

func (o *recordingObserver) events() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ev...)
}

// fakeEngine drives the REAL governed turn the web path drives. It mirrors
// Manager.RunTurn's assembly (system prompt + messages + host sandbox + native
// tools + ApprovalStager) but uses a host-mode sandbox + a scripted model so it
// is deterministic and DB-free. The injected spyObs (if set) is fanned the SAME
// run events so a test can assert the observer vocabulary.
type fakeEngine struct {
	model  fantasy.LanguageModel
	pool   *sandbox.Pool
	spyObs agentcore.Observer
}

func newFakeEngine(model fantasy.LanguageModel) *fakeEngine {
	return &fakeEngine{
		model: model,
		pool: sandbox.NewPool(sandbox.PoolConfig{
			Size:         0,
			Mode:         sandbox.ModeHost,
			BridgeScript: tools.PythonBridgeScript(),
		}),
	}
}

// RunTurn assembles + runs the real interactive turn, mapping the result onto a
// TurnResult the same way Manager.RunTurn does.
func (e *fakeEngine) RunTurn(ctx context.Context, in agent.TurnInput, sink agent.EventSink) (*agent.TurnResult, error) {
	sb, cleanup, err := e.pool.Take()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Replay prior history minimally (user/assistant text only — enough for the
	// parity + cancellation tests; the full replay lives in the agent package's
	// production path, exercised by its own tests).
	messages := append(replayTextHistory(in.History), fantasy.NewUserMessage(in.UserMessage))

	tc := agent.TurnConfig{
		SystemPrompt:   "you are a test fleet agent",
		Messages:       messages,
		Label:          in.ConversationID,
		Model:          e.model,
		MaxTokens:      512,
		NativeTools:    tools.NewTurnTools(sb).Tools,
		Sandbox:        sb,
		ApprovalStager: in.ApprovalStager,
		MemoryProposer: in.MemoryProposer,
	}

	obs := agentcore.Observer(observerFanout{sink: sink, spy: e.spyObs})
	res, runErr := agent.RunInteractiveTurn(ctx, tc, obs)
	if runErr != nil {
		if ctx.Err() != nil {
			return &agent.TurnResult{NewHistory: mapRunEntries(in.UserMessage, res.Entries), Cancelled: true}, nil
		}
		return nil, runErr
	}
	return &agent.TurnResult{
		FinalText:  res.FinalText,
		NewHistory: mapRunEntries(in.UserMessage, res.Entries),
		Cancelled:  res.Cancelled,
	}, nil
}

// replayTextHistory rebuilds user/assistant text messages from persisted
// history (the minimal replay the tests need).
func replayTextHistory(entries []agent.HistoryEntry) []fantasy.Message {
	var out []fantasy.Message
	for _, e := range entries {
		if e.Type != "text" {
			continue
		}
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		if c.Text == "" {
			continue
		}
		switch e.Role {
		case "user":
			out = append(out, fantasy.NewUserMessage(c.Text))
		case "assistant":
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: c.Text}}})
		}
	}
	return out
}

// mapRunEntries maps the run transcript to persisted history: the user message
// first, then the assistant text + tool_call/tool_result entries (mirrors
// Manager.RunTurn's persistence shape, trimmed to what the tests assert).
func mapRunEntries(userMessage string, entries []agentcore.RunEntry) []agent.HistoryEntry {
	out := []agent.HistoryEntry{histEntry("user", "text", map[string]any{"text": userMessage})}
	for _, e := range entries {
		switch e.Type {
		case "text":
			out = append(out, histEntry("assistant", "text", map[string]any{"text": e.Text}))
		case "reasoning":
			out = append(out, histEntry("assistant", "reasoning", map[string]any{"text": e.Text}))
		case "tool_call":
			out = append(out, histEntry("assistant", "tool_call", map[string]any{"id": e.ToolCallID, "name": e.ToolName, "input": e.ToolInput}))
		case "tool_result":
			out = append(out, histEntry("tool", "tool_result", map[string]any{"id": e.ToolCallID, "name": e.ToolName, "text": e.Text, "is_err": e.IsErr}))
		}
	}
	return out
}

func histEntry(role, typ string, content map[string]any) agent.HistoryEntry {
	b, _ := json.Marshal(content)
	return agent.HistoryEntry{Role: role, Type: typ, Content: b}
}

// observerFanout adapts an agent.EventSink to an agentcore.Observer and, when a
// spy is set, fans the SAME events to it (so a test asserts the observer stream
// the web path would emit, in addition to the ACP session/update mirror).
type observerFanout struct {
	sink agent.EventSink
	spy  agentcore.Observer
}

func (o observerFanout) Observe(eventType string, payload map[string]any) {
	if o.sink != nil {
		o.sink.Emit(eventType, payload)
	}
	if o.spy != nil {
		o.spy.Observe(eventType, payload)
	}
}

// ── in-memory stores ──

type memStore struct {
	mu      sync.Mutex
	convs   map[string]*store.Conversation
	history map[string][]agent.HistoryEntry
	apps    map[string]*store.Approval
	seq     int
}

func newMemStore() *memStore {
	return &memStore{convs: map[string]*store.Conversation{}, history: map[string][]agent.HistoryEntry{}, apps: map[string]*store.Approval{}}
}

func (s *memStore) CreateConversation(_ context.Context, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "conv-" + itoa(s.seq)
	c := &store.Conversation{ID: id, UserEmail: userEmail, Title: title, Persona: persona, Model: model, Lockdown: lockdown}
	s.convs[id] = c
	return c, nil
}

func (s *memStore) LoadHistory(_ context.Context, convID string) ([]agent.HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.HistoryEntry(nil), s.history[convID]...), nil
}

func (s *memStore) AppendHistory(_ context.Context, convID string, entries []agent.HistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[convID] = append(s.history[convID], entries...)
	return nil
}

func (s *memStore) CreateApproval(_ context.Context, convID, userEmail, toolName, toolCallID, argsJSON string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "appr-" + itoa(s.seq)
	a := &store.Approval{ID: id, ConversationID: convID, UserEmail: userEmail, ToolName: toolName, ToolCallID: toolCallID, ArgsJSON: argsJSON, Status: "pending"}
	s.apps[id] = a
	return a, nil
}

func (s *memStore) ClaimApproval(_ context.Context, _, approvalID, newStatus, resultText string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[approvalID]
	if a == nil || a.Status != "pending" {
		return false, nil
	}
	a.Status = newStatus
	a.ResultText = resultText
	return true, nil
}

func (s *memStore) SetApprovalResult(_ context.Context, _, approvalID, resultText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.apps[approvalID]; a != nil {
		a.ResultText = resultText
	}
	return nil
}

// approvalByTool returns the (single) approval staged for a tool, for assertions.
func (s *memStore) approvalByTool(tool string) *store.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.ToolName == tool {
			return a
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── fake staged-tool runner ──

type fakeRunner struct {
	mu   sync.Mutex
	ran  []string // tool names executed
	resp string
}

func (r *fakeRunner) RunStagedTool(_ context.Context, approval *store.Approval) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, approval.ToolName)
	if r.resp == "" {
		return "executed " + approval.ToolName, nil
	}
	return r.resp, nil
}

func (r *fakeRunner) executed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

// ── fake editor (ACP client) ──

// fakeEditor is an in-memory ACP client (the "editor"). It accumulates streamed
// text + tool-call updates, and answers request_permission per a configurable
// policy (allow / reject / block-until-ctx-cancel for the timeout test).
type fakeEditor struct {
	mu sync.Mutex

	text     []byte
	toolBeg  []string          // tool_call ids started
	toolEnd  map[string]string // tool_call id → terminal status
	thoughts []byte

	permN    int
	permReqs []acp.RequestPermissionRequest
	decide   func(req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
}

func newFakeEditor() *fakeEditor {
	return &fakeEditor{toolEnd: map[string]string{}}
}

func (c *fakeEditor) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	u := p.Update
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		c.text = append(c.text, u.AgentMessageChunk.Content.Text.Text...)
	case u.AgentThoughtChunk != nil && u.AgentThoughtChunk.Content.Text != nil:
		c.thoughts = append(c.thoughts, u.AgentThoughtChunk.Content.Text.Text...)
	case u.ToolCall != nil:
		c.toolBeg = append(c.toolBeg, string(u.ToolCall.ToolCallId))
	case u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil:
		c.toolEnd[string(u.ToolCallUpdate.ToolCallId)] = string(*u.ToolCallUpdate.Status)
	}
	return nil
}

func (c *fakeEditor) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permN++
	c.permReqs = append(c.permReqs, p)
	decide := c.decide
	c.mu.Unlock()
	if decide != nil {
		return decide(p)
	}
	// Default: reject.
	return rejectResp(p), nil
}

func (c *fakeEditor) streamedText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.text)
}

func (c *fakeEditor) toolStarts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.toolBeg...)
}

func (c *fakeEditor) toolStatus(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toolEnd[id]
}

func (c *fakeEditor) permissionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.permN
}

// lastPermReq returns the most recent permission request the editor received,
// for asserting its shape (title / options).
func (c *fakeEditor) lastPermReq() (acp.RequestPermissionRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.permReqs) == 0 {
		return acp.RequestPermissionRequest{}, false
	}
	return c.permReqs[len(c.permReqs)-1], true
}

func allowResp(p acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	for _, o := range p.Options {
		if o.Kind == acp.PermissionOptionKindAllowOnce {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
				Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
			}}
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}
}

func rejectResp(p acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	for _, o := range p.Options {
		if o.Kind == acp.PermissionOptionKindRejectOnce {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
				Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
			}}
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}
}

// unused stubs to satisfy acp.Client.

func (c *fakeEditor) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}
func (c *fakeEditor) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}
func (c *fakeEditor) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}
func (c *fakeEditor) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}
func (c *fakeEditor) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}
func (c *fakeEditor) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}
func (c *fakeEditor) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}
