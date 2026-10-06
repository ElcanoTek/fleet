package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"

	"github.com/ElcanoTek/fleet/internal/chattui"
)

// fakeFleet is a stand-in for a running fleet server: it speaks the real
// POST /chat SSE contract (the frames chattui.Client parses) and records the
// requests, including the Stop call. No model, sandbox or database — the
// governed turn itself is the server's business and covered by its own suites.
type fakeFleet struct {
	t *testing.T
	// turn writes one turn's frames; nil means the default tool-using turn.
	turn func(w *sseWriter, r *http.Request)
	// cancelStatus is what the Stop endpoint answers (0 = 204).
	cancelStatus int
	// cancelHold, when set, holds every Stop, once recorded, until it is
	// closed: a slow or hung server. A Stop whose caller gives up ends.
	cancelHold chan struct{}

	mu      sync.Mutex
	chats   []chatReq
	cancels []string
	headers []http.Header
}

type chatReq struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
	Model          string `json:"model"`
	InputID        string `json:"input_id"`
	SubmissionID   string `json:"submission_id"`
}

type sseWriter struct {
	w http.ResponseWriter
}

func (s *sseWriter) emit(name string, data map[string]any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	s.w.(http.Flusher).Flush()
}

func (f *fakeFleet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/client-config":
		_, _ = io.WriteString(w, `{"models":{"default_model":"test/default"}}`)
	case r.URL.Path == "/chat":
		var body chatReq
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.chats = append(f.chats, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		sw := &sseWriter{w: w}
		if f.turn != nil {
			f.turn(sw, r)
			return
		}
		sw.emit("conversation", map[string]any{"id": "conv-1"})
		sw.emit("turn.started", map[string]any{})
		sw.emit("reasoning.delta", map[string]any{"id": "r1", "text": "thinking"})
		sw.emit("tool.call", map[string]any{"id": "call-1", "name": "run_python", "input": `{"code":"1+1"}`})
		sw.emit("tool.result", map[string]any{"id": "call-1", "name": "run_python", "text": "2", "is_err": false})
		sw.emit("text.delta", map[string]any{"text": "Hello "})
		sw.emit("text.delta", map[string]any{"text": "there"})
		sw.emit("text.replace", map[string]any{"text": "Hello there"})
		sw.emit("turn.completed", map[string]any{"prompt_tokens": 10, "completion_tokens": 4, "cached_tokens": 2})
	case strings.HasPrefix(r.URL.Path, "/conversations/") && strings.HasSuffix(r.URL.Path, "/cancel"):
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.cancels = append(f.cancels, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/conversations/"), "/cancel")+" "+string(b))
		f.mu.Unlock()
		if f.cancelHold != nil {
			select {
			case <-f.cancelHold:
			case <-r.Context().Done():
				return
			}
		}
		if f.cancelStatus != 0 {
			w.WriteHeader(f.cancelStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// recordingClient is the ACP client side: it records every session/update.
type recordingClient struct {
	mu      sync.Mutex
	updates []acpsdk.SessionUpdate
}

func (c *recordingClient) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, n.Update)
	c.mu.Unlock()
	return nil
}

func (c *recordingClient) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, u := range c.updates {
		if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
			b.WriteString(u.AgentMessageChunk.Content.Text.Text)
		}
	}
	return b.String()
}

func (c *recordingClient) RequestPermission(context.Context, acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	return acpsdk.RequestPermissionResponse{}, errors.New("fleet acp must not request permission")
}
func (c *recordingClient) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, errors.New("unexpected fs read")
}
func (c *recordingClient) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, errors.New("unexpected fs write")
}
func (c *recordingClient) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, errors.New("unexpected terminal")
}
func (c *recordingClient) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, errors.New("unexpected terminal")
}
func (c *recordingClient) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, errors.New("unexpected terminal")
}
func (c *recordingClient) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, errors.New("unexpected terminal")
}
func (c *recordingClient) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, errors.New("unexpected terminal")
}

type harness struct {
	fleet  *fakeFleet
	client *recordingClient
	conn   *acpsdk.ClientSideConnection
	agent  *Agent
	// hangUp closes the client's side of stdin, as a client exiting does.
	hangUp func()
	// raw writes one JSON-RPC line to fleet acp's stdin, for a message the
	// SDK client does not send on its own. The pipe keeps it whole beside
	// the SDK's writes, each of which is also one whole line.
	raw func(line string)
}

type harnessOpts struct {
	turn         func(w *sseWriter, r *http.Request)
	cancelStatus int
	cancelHold   chan struct{} // see fakeFleet.cancelHold
	cfgErr       error
	publicURL    string
	timeout      time.Duration
	serverURL    string // override (e.g. a closed port)
}

// newHarness wires a real SDK client to the real Agent over in-memory pipes,
// the way an ACP client wires to `fleet acp`'s stdio.
func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	// Most fake turns never send a terminal frame after a Stop; do not wait
	// long for one (the real server sends turn.cancelled).
	prevSettle := stopSettleWait
	stopSettleWait = 50 * time.Millisecond
	t.Cleanup(func() { stopSettleWait = prevSettle })
	ff := &fakeFleet{t: t, turn: o.turn, cancelStatus: o.cancelStatus, cancelHold: o.cancelHold}
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)
	serverURL := srv.URL
	if o.serverURL != "" {
		serverURL = o.serverURL
	}
	cc := chattui.NewClient(chattui.Config{ServerURL: serverURL, Email: "bot@example.com", Token: "test-token", ClientName: "fleet-acp"})
	cc.AdoptDefaultModel("test/default")
	ag := NewAgent(cc, o.cfgErr, o.publicURL, o.timeout, "test")

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	rc := &recordingClient{}
	conn := acpsdk.NewClientSideConnection(rc, c2aW, a2cR)
	agentConn := acpsdk.NewAgentSideConnection(ag, a2cW, c2aR)
	ag.SetConnection(agentConn)
	t.Cleanup(func() {
		for _, c := range []io.Closer{c2aR, c2aW, a2cR, a2cW} {
			_ = c.Close()
		}
	})
	return &harness{
		fleet: ff, client: rc, conn: conn, agent: ag,
		hangUp: func() { _ = c2aW.Close() },
		raw: func(line string) {
			if _, err := io.WriteString(c2aW, line+"\n"); err != nil {
				t.Errorf("raw write: %v", err)
			}
		},
	}
}

// promptAsync sends a prompt (with messageID, when not nil) without waiting
// for its answer, and returns once the Agent tracks inFlight prompts on the
// session, this one included: it has then taken every arrival step (the SDK
// has superseded the prompt before it, it has its arrival key, and a
// session/cancel reaches it). Answered prompts leave the count, so inFlight
// counts only those still running or waiting.
func (h *harness) promptAsync(t *testing.T, sid acpsdk.SessionId, text string, messageID *string, inFlight int) <-chan promptResult {
	t.Helper()
	done := make(chan promptResult, 1)
	go func() {
		r, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: messageID, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
		done <- promptResult{r, err}
	}()
	h.waitTracked(t, sid, inFlight)
	return done
}

// waitTracked waits until the Agent tracks exactly n prompts on sid.
func (h *harness) waitTracked(t *testing.T, sid acpsdk.SessionId, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.agent.mu.Lock()
		got := len(h.agent.inflight[sid])
		h.agent.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d prompts in flight on the session, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type promptResult struct {
	resp acpsdk.PromptResponse
	err  error
}

func await(t *testing.T, done <-chan promptResult, what string) promptResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
		return promptResult{}
	}
}

func (h *harness) newSession(t *testing.T) acpsdk.SessionId {
	t.Helper()
	ctx := context.Background()
	if _, err := h.conn.Initialize(ctx, acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	s, err := h.conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: "/work", McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	return s.SessionId
}

func (h *harness) prompt(sid acpsdk.SessionId, text string) (acpsdk.PromptResponse, error) {
	return h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
}

func rpcCode(err error) int {
	var re *acpsdk.RequestError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

func TestInitializeAdvertisesOnlyWhatShips(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtocolVersion != SpecVersion {
		t.Errorf("protocolVersion = %d, want %d", resp.ProtocolVersion, SpecVersion)
	}
	caps := resp.AgentCapabilities
	if caps.LoadSession || caps.PromptCapabilities.Image || caps.PromptCapabilities.Audio {
		t.Errorf("over-advertised capabilities: %+v", caps)
	}
	if !caps.PromptCapabilities.EmbeddedContext {
		t.Error("embedded text resources are accepted but not advertised")
	}
	if resp.AgentInfo == nil || resp.AgentInfo.Name != "fleet" {
		t.Errorf("agentInfo = %+v", resp.AgentInfo)
	}
	// authMethods is required by the schema: an empty array, never null.
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"authMethods":[]`) {
		t.Errorf("authMethods must serialize as []: %s", raw)
	}
}

func TestPromptRunsOneTurnAndStreamsIt(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	sid := h.newSession(t)

	resp, err := h.prompt(sid, "say hi")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Errorf("stopReason = %q", resp.StopReason)
	}
	if got := resp.Meta["fleet.conversationId"]; got != "conv-1" {
		t.Errorf("conversation id meta = %v", got)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 4 || resp.Usage.TotalTokens != 14 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got := h.client.text(); got != "Hello there" {
		t.Errorf("streamed text = %q, want %q (text.replace matching the deltas must add nothing)", got, "Hello there")
	}

	var thought, toolStart, toolDone bool
	for _, u := range h.client.updates {
		switch {
		case u.AgentThoughtChunk != nil:
			thought = u.AgentThoughtChunk.Content.Text.Text == "thinking"
		case u.ToolCall != nil:
			toolStart = u.ToolCall.ToolCallId == "call-1" && u.ToolCall.Title == "run_python" && u.ToolCall.Status == acpsdk.ToolCallStatusInProgress
			if u.ToolCall.RawInput != nil {
				t.Error("tool input must stay in fleet's run log, not be forwarded")
			}
		case u.ToolCallUpdate != nil:
			toolDone = u.ToolCallUpdate.ToolCallId == "call-1" && u.ToolCallUpdate.Status != nil && *u.ToolCallUpdate.Status == acpsdk.ToolCallStatusCompleted
		}
	}
	if !thought || !toolStart || !toolDone {
		t.Errorf("thought=%v toolStart=%v toolDone=%v; updates=%+v", thought, toolStart, toolDone, h.client.updates)
	}

	// The turn went to the governed POST /chat seam as the provisioned user.
	if len(h.fleet.chats) != 1 || h.fleet.chats[0].Message != "say hi" || h.fleet.chats[0].ConversationID != "" || h.fleet.chats[0].Model != "test/default" {
		t.Fatalf("chat requests = %+v", h.fleet.chats)
	}
	hdr := h.fleet.headers[0]
	if hdr.Get("X-Chat-Server-Token") != "test-token" || hdr.Get("X-User-Email") != "bot@example.com" || hdr.Get("X-Fleet-Client") != "fleet-acp" {
		t.Errorf("headers = %v", hdr)
	}

	// The second prompt continues the same fleet conversation.
	if _, err := h.prompt(sid, "again"); err != nil {
		t.Fatal(err)
	}
	if got := h.fleet.chats[1]; got.ConversationID != "conv-1" || got.Model != "" {
		t.Errorf("follow-up = %+v, want conversation conv-1 and no model override", got)
	}
}

func TestTextReplaceReconcilesAppendOnly(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		final  string
		want   string
	}{
		{"extension sends the suffix", []string{"Hel"}, "Hello", "Hello"},
		{"no deltas sends the final text", nil, "Final", "Final"},
		{"divergent draft gets a revised answer", []string{"first draft"}, "better answer", "first draft" + revisedMarker + "better answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
				w.emit("conversation", map[string]any{"id": "conv-r"})
				for _, d := range tc.deltas {
					w.emit("text.delta", map[string]any{"text": d})
				}
				w.emit("text.replace", map[string]any{"text": tc.final})
				w.emit("turn.completed", map[string]any{})
			}})
			if _, err := h.prompt(h.newSession(t), "x"); err != nil {
				t.Fatal(err)
			}
			if got := h.client.text(); got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
		})
	}
}

// blockingTurn starts a conversation, streams one chunk, then holds the stream
// open until the request is aborted — a long-running turn.
func blockingTurn(started chan<- struct{}) func(w *sseWriter, r *http.Request) {
	return func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-slow"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-slow"})
		w.emit("text.delta", map[string]any{"text": "working"})
		close(started)
		<-r.Context().Done()
	}
}

func TestCancelStopsTheFleetTurn(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: blockingTurn(started)})
	sid := h.newSession(t)

	type result struct {
		resp acpsdk.PromptResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r, err := h.prompt(sid, "long job")
		done <- result{r, err}
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("prompt after cancel = %+v, %v; want stopReason cancelled and no error", r.resp, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return after session/cancel")
	}
	// Dropping the stream does not stop a fleet turn; the explicit Stop must
	// have been sent, scoped to the turn so queued follow-ups survive.
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-slow {"scope":"turn","turn_id":"turn-slow"}` {
		t.Errorf("cancel calls = %q", h.fleet.cancels)
	}
}

// TestCancelBeforeTheConversationIsKnown is the race a quick Stop hits: the
// session/cancel arrives before the stream's first frame (the conversation id)
// has been read. The adapter must still stop that turn server-side once it
// learns the id, not return early with nothing to stop.
func TestCancelBeforeTheConversationIsKnown(t *testing.T) {
	requested := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		close(requested)
		<-release // the turn is accepted, but no frame has been written yet
		w.emit("conversation", map[string]any{"id": "conv-late"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-late"})
		<-r.Context().Done()
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "long job")
		done <- r
	}()
	<-requested
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the cancel land while no frame exists
	close(release)
	select {
	case r := <-done:
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q", r.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || !strings.HasPrefix(h.fleet.cancels[0], "conv-late ") {
		t.Errorf("cancel calls = %q, want the late-learned conversation stopped", h.fleet.cancels)
	}
}

func TestTimeoutStopsTheTurnAndSaysSo(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: blockingTurn(started), timeout: 200 * time.Millisecond})
	_, err := h.prompt(h.newSession(t), "long job")
	if rpcCode(err) != -32603 || !strings.Contains(err.Error(), "did not finish within 200ms") {
		t.Fatalf("err = %v, want an internal error naming the timeout", err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 {
		t.Errorf("a timed-out turn must be stopped server-side; cancels = %q", h.fleet.cancels)
	}
}

func TestFailuresBecomeClearErrors(t *testing.T) {
	t.Run("401/403 is auth_required", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			http.Error(w.w, "forbidden", http.StatusForbidden) // the shared-token check's exact refusal
		}})
		_, err := h.prompt(h.newSession(t), "x")
		if rpcCode(err) != -32000 || !strings.Contains(err.Error(), "FLEET_SERVER_TOKEN") {
			t.Fatalf("err = %v, want auth_required naming the token", err)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Fatal("the token value leaked into an error")
		}
	})
	t.Run("unprovisioned user 403 is auth_required naming the user", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			w.w.Header().Set("Content-Type", "application/json")
			w.w.WriteHeader(http.StatusForbidden)
			_, _ = w.w.Write([]byte(`{"error":"not_a_member"}`))
		}})
		_, err := h.prompt(h.newSession(t), "x")
		if rpcCode(err) != -32000 || !strings.Contains(err.Error(), "bot@example.com is not a fleet user") {
			t.Fatalf("err = %v, want auth_required naming the unprovisioned user", err)
		}
		if strings.Contains(err.Error(), "FLEET_SERVER_TOKEN") || strings.Contains(err.Error(), "test-token") {
			t.Fatalf("a membership refusal must not blame (or leak) the token: %v", err)
		}
	})
	t.Run("daemon down", func(t *testing.T) {
		h := newHarness(t, harnessOpts{serverURL: "http://127.0.0.1:1"})
		_, err := h.prompt(h.newSession(t), "x")
		if rpcCode(err) != -32603 || !strings.Contains(err.Error(), "connect http://127.0.0.1:1") {
			t.Fatalf("err = %v, want an internal error naming the unreachable server", err)
		}
	})
	t.Run("turn.error", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			w.emit("conversation", map[string]any{"id": "c"})
			w.emit("turn.error", map[string]any{"message": "budget exhausted"})
		}})
		_, err := h.prompt(h.newSession(t), "x")
		if !strings.Contains(fmt.Sprint(err), "budget exhausted") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing config is auth_required at session/new", func(t *testing.T) {
		h := newHarness(t, harnessOpts{cfgErr: errors.New("no user email: pass --email")})
		if _, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: 1}); err != nil {
			t.Fatalf("initialize must still succeed: %v", err)
		}
		_, err := h.conn.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{}})
		if rpcCode(err) != -32000 || !strings.Contains(err.Error(), "--email") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown session", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.newSession(t)
		if _, err := h.prompt("nope", "x"); rpcCode(err) != -32002 {
			t.Fatalf("err = %v, want resource not found", err)
		}
	})
}

// wireError is err as the ACP client decoded it off the wire.
func wireError(t *testing.T, err error) *acpsdk.RequestError {
	t.Helper()
	var re *acpsdk.RequestError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a JSON-RPC error", err)
	}
	return re
}

// resolveErr is the error `fleet acp` starts with when its config does not
// resolve: the real chattui.Resolve, with nothing in the environment and no
// env file to read.
func resolveErr(t *testing.T, f chattui.Flags) error {
	t.Helper()
	_, err := chattui.Resolve(f, func(string) string { return "" },
		func(string) ([]byte, error) { return nil, os.ErrNotExist },
		func(string, ...string) (map[string]string, error) { return nil, os.ErrNotExist })
	if err == nil {
		t.Fatal("the config resolved; want an error")
	}
	return err
}

// Every error fleet acp answers with carries its reason in the JSON-RPC
// message, not only in data.error: some clients show code and message alone
// (Emacs's agent-shell hides data behind a Details button), and with the SDK's
// generic message an unknown user, a viewer, a wrong token and a missing email
// all read "Authentication required". code and data.error are as before.
func TestErrorMessagesCarryTheReason(t *testing.T) {
	refuse := func(status int, contentType, body string) func(w *sseWriter, _ *http.Request) {
		return func(w *sseWriter, _ *http.Request) {
			w.w.Header().Set("Content-Type", contentType)
			w.w.WriteHeader(status)
			_, _ = io.WriteString(w.w, body)
		}
	}
	promptWith := func(o harnessOpts, blocks ...acpsdk.ContentBlock) func(t *testing.T) error {
		if len(blocks) == 0 {
			blocks = []acpsdk.ContentBlock{acpsdk.TextBlock("x")}
		}
		return func(t *testing.T) error {
			h := newHarness(t, o)
			_, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: h.newSession(t), Prompt: blocks})
			return err
		}
	}
	terminal := func(frame, message string) func(w *sseWriter, _ *http.Request) {
		return func(w *sseWriter, _ *http.Request) {
			w.emit("conversation", map[string]any{"id": "c"})
			w.emit(frame, map[string]any{"message": message})
		}
	}
	noEmail := resolveErr(t, chattui.Flags{})
	noToken := resolveErr(t, chattui.Flags{Email: "bot@example.com", EnvFile: "/nonexistent/fleet.env"})
	longFailure := "the provider refused the request. " + strings.Repeat("Its detail runs on and on. ", 20)

	for _, tc := range []struct {
		name string
		err  func(t *testing.T) error
		code int
		// data is data.error, exactly as before message carried it.
		data string
		// dataPrefix, when set, is data.error's start instead (the rest is the
		// OS's text).
		dataPrefix string
		// message is the message wanted; "" means data.error itself.
		message string
	}{
		{
			name: "403 wrong token",
			err:  promptWith(harnessOpts{turn: refuse(http.StatusForbidden, "text/plain", "forbidden\n")}),
			code: -32000,
			data: "server rejected the request (403): check FLEET_SERVER_TOKEN matches the server",
		},
		{
			name: "403 not a fleet user",
			err:  promptWith(harnessOpts{turn: refuse(http.StatusForbidden, "application/json", `{"error":"not_a_member"}`)}),
			code: -32000,
			data: "server rejected the request (403): bot@example.com is not a fleet user; an admin can add it with `fleet chat user add bot@example.com --password -`, or use --email/FLEET_USER_EMAIL for a provisioned user",
		},
		{
			name: "403 viewer",
			err:  promptWith(harnessOpts{turn: refuse(http.StatusForbidden, "application/json", `{"error":"read_only"}`)}),
			code: -32000,
			data: "server rejected the request (403): bot@example.com has the read-only viewer role and cannot send messages; an admin can change it with `fleet chat user role bot@example.com --role member`",
		},
		{
			name: "403 IP filter",
			err:  promptWith(harnessOpts{turn: refuse(http.StatusForbidden, "text/plain", "Access denied\n")}),
			code: -32000,
			data: "server rejected the request (403): the server's IP access control (FLEET_IP_ALLOWLIST / FLEET_IP_DENYLIST) does not admit this client's address; connect from an admitted address, or ask an admin to admit this one",
		},
		{
			// A proxy's page over several lines, echoing the token: message is
			// one line, and the token is redacted from both.
			name:    "401 from a proxy",
			err:     promptWith(harnessOpts{turn: refuse(http.StatusUnauthorized, "text/html", "<html>\n  <h1>401</h1>\n  Authorization: Bearer test-token\n</html>\n")}),
			code:    -32000,
			data:    "not authorized (401) for bot@example.com: <html>\n  <h1>401</h1>\n  Authorization: Bearer [redacted]\n</html>",
			message: "not authorized (401) for bot@example.com: <html> <h1>401</h1> Authorization: Bearer [redacted] </html>",
		},
		{
			name: "missing email at session/new",
			err: func(t *testing.T) error {
				h := newHarness(t, harnessOpts{cfgErr: noEmail})
				if _, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: 1}); err != nil {
					t.Fatalf("initialize must still succeed: %v", err)
				}
				_, err := h.conn.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{}})
				return err
			},
			code: -32000,
			data: "no user email: pass --email <you@example.com> or set FLEET_USER_EMAIL (your audit identity, so it is never guessed)",
		},
		{
			name: "missing token at session/prompt",
			err: func(t *testing.T) error {
				h := newHarness(t, harnessOpts{cfgErr: noToken})
				_, err := h.prompt("fleet-acp-any", "x")
				return err
			},
			code: -32000,
			data: noToken.Error(),
		},
		{
			name: "turn.error",
			err:  promptWith(harnessOpts{turn: terminal("turn.error", "budget exhausted")}),
			code: -32603,
			data: "turn failed: budget exhausted",
		},
		{
			name: "turn.model_required",
			err:  promptWith(harnessOpts{turn: terminal("turn.model_required", "the model is no longer offered; pick another")}),
			code: -32603,
			data: "turn requires another model: the model is no longer offered; pick another",
		},
		{
			// Too long for one line: message keeps the first sentence.
			name:    "a long turn.error",
			err:     promptWith(harnessOpts{turn: terminal("turn.error", longFailure)}),
			code:    -32603,
			data:    "turn failed: " + longFailure,
			message: "turn failed: the provider refused the request.",
		},
		{
			name: "any other status",
			err:  promptWith(harnessOpts{turn: refuse(http.StatusInternalServerError, "text/plain", "database unavailable\n")}),
			code: -32603,
			data: "server returned 500: database unavailable",
		},
		{
			name:       "daemon down",
			err:        promptWith(harnessOpts{serverURL: "http://127.0.0.1:1"}),
			code:       -32603,
			dataPrefix: "connect http://127.0.0.1:1: ",
		},
		{
			name: "--timeout",
			err:  promptWith(harnessOpts{turn: blockingTurn(make(chan struct{})), timeout: 200 * time.Millisecond}),
			code: -32603,
			data: "the fleet turn did not finish within 200ms and was stopped (raise it with fleet acp --timeout)",
		},
		{
			name: "--timeout whose Stop failed",
			err: promptWith(harnessOpts{turn: blockingTurn(make(chan struct{})), timeout: 200 * time.Millisecond,
				cancelStatus: http.StatusBadGateway, publicURL: "https://fleet.example.com"}),
			code: -32603,
			data: "the fleet turn did not finish within 200ms, and stopping it failed (cancel returned 502: ): it may still be running — stop it at https://fleet.example.com/chat?c=conv-slow",
		},
		{
			name: "client MCP servers",
			err: func(t *testing.T) error {
				h := newHarness(t, harnessOpts{})
				if _, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: 1}); err != nil {
					t.Fatal(err)
				}
				_, err := h.conn.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{
					{Stdio: &acpsdk.McpServerStdio{Name: "x", Command: "/bin/true", Args: []string{}, Env: []acpsdk.EnvVariable{}}},
				}})
				return err
			},
			code: -32602,
			data: "fleet does not accept MCP servers from the ACP client: its connectors come from the operator's bundle and run host-side with brokered credentials",
		},
		{
			name: "image",
			err:  promptWith(harnessOpts{}, acpsdk.ImageBlock("aGk=", "image/png")),
			code: -32602,
			data: "fleet acp does not accept image content (promptCapabilities.image is false)",
		},
		{
			name: "audio",
			err:  promptWith(harnessOpts{}, acpsdk.AudioBlock("aGk=", "audio/wav")),
			code: -32602,
			data: "fleet acp does not accept audio content (promptCapabilities.audio is false)",
		},
		{
			name: "binary blob",
			err: promptWith(harnessOpts{}, acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
				BlobResourceContents: &acpsdk.BlobResourceContents{Uri: "file:///repo/logo.png", Blob: "aGk="},
			})),
			code: -32602,
			data: "fleet acp accepts text, resource_link and embedded text resources only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			re := wireError(t, tc.err(t))
			if re.Code != tc.code {
				t.Errorf("code = %d, want %d", re.Code, tc.code)
			}
			data, _ := re.Data.(map[string]any)
			got, _ := data["error"].(string)
			switch {
			case tc.dataPrefix != "" && !strings.HasPrefix(got, tc.dataPrefix):
				t.Errorf("data.error = %q, want it to start %q", got, tc.dataPrefix)
			case tc.dataPrefix == "" && got != tc.data:
				t.Errorf("data.error = %q\nwant        %q", got, tc.data)
			}
			want := tc.message
			if want == "" {
				want = got
			}
			if re.Message != want || want == "" {
				t.Errorf("message = %q\nwant      %q", re.Message, want)
			}
			if strings.ContainsAny(re.Message, "\r\n") || utf8.RuneCountInString(re.Message) > maxErrorMessage+1 {
				t.Errorf("message is not one concise line: %q", re.Message)
			}
			if strings.Contains(re.Message, "test-token") || strings.Contains(got, "test-token") {
				t.Fatal("the token value leaked into an error")
			}
		})
	}

	t.Run("unknown session", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.newSession(t)
		re := wireError(t, func() error { _, err := h.prompt("nope", "x"); return err }())
		if re.Code != -32002 {
			t.Errorf("code = %d, want -32002 (resource not found)", re.Code)
		}
		if data, _ := re.Data.(map[string]any); len(data) != 1 || data["sessionId"] != "nope" {
			t.Errorf("data = %v, want the session id alone", re.Data)
		}
		if want := `fleet acp has no session "nope" (it was closed, or opened by an earlier fleet acp process); start a new session`; re.Message != want {
			t.Errorf("message = %q\nwant      %q", re.Message, want)
		}
	})
}

// errorMessage keeps a reason whole when it fits one concise line, else its
// first sentence, else the words that fit.
func TestErrorMessageIsOneConciseLine(t *testing.T) {
	long := func(sentence string) string {
		return sentence + " " + strings.Repeat("And then more detail. ", 30)
	}
	for _, tc := range []struct {
		name, reason, want string
	}{
		{"short, kept whole", "turn failed: rate limited. Retry in 20s.", "turn failed: rate limited. Retry in 20s."},
		{"line breaks and runs of spaces collapse", "server returned 502:\n<html>\n\t<h1>Bad  Gateway</h1>\r\n</html>", "server returned 502: <html> <h1>Bad Gateway</h1> </html>"},
		{"long: the first sentence", long("turn failed: the provider refused the request."), "turn failed: the provider refused the request."},
		{"a full stop in parentheses does not end it", long("turn failed (see the log. It has more) and stopped."), "turn failed (see the log. It has more) and stopped."},
		{"nor one in a command", long("run `fleet status. now` and retry!"), "run `fleet status. now` and retry!"},
		{"empty", " \n ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorMessage(tc.reason); got != tc.want {
				t.Errorf("errorMessage = %q\nwant           %q", got, tc.want)
			}
		})
	}
	t.Run("one long sentence: the words that fit", func(t *testing.T) {
		reason := "the turn failed because " + strings.Repeat("ünïcode words ", 60)
		got := errorMessage(reason)
		body, ok := strings.CutSuffix(got, "…")
		if !ok || !strings.HasPrefix(reason, body+" ") || utf8.RuneCountInString(body) > maxErrorMessage || utf8.RuneCountInString(body) < maxErrorMessage-20 {
			t.Errorf("errorMessage = %q, want the whole words of the first %d runes, then …", got, maxErrorMessage)
		}
	})
	t.Run("one long word: cut at the bound", func(t *testing.T) {
		got := errorMessage(strings.Repeat("é", 2*maxErrorMessage))
		if want := strings.Repeat("é", maxErrorMessage) + "…"; got != want {
			t.Errorf("errorMessage = %q, want %d runes then …", got, maxErrorMessage)
		}
	})
	t.Run("an empty reason keeps the kind's name", func(t *testing.T) {
		if got := reasonError(acpsdk.NewInternalError, "").Message; got != "Internal error" {
			t.Errorf("message = %q", got)
		}
	})
}

func TestPolicyBlockIsARefusal(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.policy_blocked", map[string]any{"policy": "prompt-injection"})
		w.emit("turn.error", map[string]any{"message": "blocked"})
	}})
	resp, err := h.prompt(h.newSession(t), "x")
	if err != nil || resp.StopReason != acpsdk.StopReasonRefusal {
		t.Fatalf("got %+v, %v; want stopReason refusal", resp, err)
	}
}

func TestApprovalsPointBackToFleet(t *testing.T) {
	turn := func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-a"})
		w.emit("text.delta", map[string]any{"text": "Drafted the email."})
		w.emit("tool.approval_required", map[string]any{"approval_id": "ap-1", "tool": "mcp_sendgrid_send_email"})
		w.emit("text.replace", map[string]any{"text": "Drafted the email."})
		w.emit("turn.completed", map[string]any{})
	}
	t.Run("with a public URL", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: turn, publicURL: "https://fleet.example.com/"})
		if _, err := h.prompt(h.newSession(t), "email bob"); err != nil {
			t.Fatal(err)
		}
		got := h.client.text()
		if !strings.Contains(got, "mcp_sendgrid_send_email") || !strings.Contains(got, "https://fleet.example.com/chat?c=conv-a") {
			t.Errorf("text = %q", got)
		}
	})
	t.Run("without one, the CLI command", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: turn})
		if _, err := h.prompt(h.newSession(t), "email bob"); err != nil {
			t.Fatal(err)
		}
		if got := h.client.text(); !strings.Contains(got, "fleet chat --conversation conv-a --approve ap-1") {
			t.Errorf("text = %q", got)
		}
	})
}

func TestRefusesWhatItDoesNotAdvertise(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	if _, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := h.conn.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{
		{Stdio: &acpsdk.McpServerStdio{Name: "x", Command: "/bin/true", Args: []string{}, Env: []acpsdk.EnvVariable{}}},
	}})
	if rpcCode(err) != -32602 {
		t.Errorf("client MCP servers: err = %v, want invalid params", err)
	}
	sid := h.newSession(t)
	_, err = h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{acpsdk.ImageBlock("aGk=", "image/png")}})
	if rpcCode(err) != -32602 {
		t.Errorf("image prompt: err = %v, want invalid params", err)
	}
	if len(h.fleet.chats) != 0 {
		t.Errorf("a refused prompt must not reach fleet: %+v", h.fleet.chats)
	}
}

func TestPromptTextFlattensAttachments(t *testing.T) {
	uri := "file:///repo/main.go"
	got, err := promptText([]acpsdk.ContentBlock{
		acpsdk.TextBlock("Review this"),
		acpsdk.ResourceLinkBlock("main.go", uri),
		{Resource: &acpsdk.ContentBlockResource{Type: "resource", Resource: acpsdk.EmbeddedResourceResource{
			TextResourceContents: &acpsdk.TextResourceContents{Uri: "file:///repo/go.mod", Text: "module x"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review this", "[main.go](file:///repo/main.go)", "Contents of file:///repo/go.mod:\n````\nmodule x\n````"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt %q missing %q", got, want)
		}
	}
	if _, err := promptText([]acpsdk.ContentBlock{acpsdk.TextBlock("  ")}); err == nil {
		t.Error("an empty prompt must be refused")
	}
}

// An embedded resource that carries its own code fences stays whole inside
// fleet's: the fence is longer than any backtick run in the text, so no line
// of the file can close it early and go on as if it were the user's prompt.
// The first case is CodeCompanion.nvim's rendering of a buffer, byte for byte,
// as a client set up to send it as an embedded resource delivers it.
func TestEmbeddedResourceFenceEnclosesItsText(t *testing.T) {
	codeCompanion := "<attachment filepath=\"/path/sample3.py\" buffer_number=\"1\">User's current visible code in a file (including line numbers). This should be the main focus:\n" +
		"````python\n" +
		"1 |def secret_number():\n" +
		"2 |    return 3157\n" +
		"````\n" +
		"</attachment>"
	for _, tc := range []struct{ name, text, fence string }{
		{"codecompanion buffer", codeCompanion, "`````"},
		{"no backticks", "module x", "````"},
		{"inline code only", "use `x` or ``y``", "````"},
		{"three and four backtick fences", "```go\nx := 1\n```\n````\ny\n````", "`````"},
		{"fence at the very start", "```\nnever closed", "````"},
		{"only backticks", "```", "````"},
		{"indented fence and trailing newline", "  ````\n", "`````"},
		{"CRLF lines", "x\r\n````\r\ny\r\n", "`````"},
		{"lone CR lines", "x\r````\ry", "`````"},
		{"empty", "", "````"},
		{"long run on its own line", "x\n" + strings.Repeat("`", 12) + "\ny", strings.Repeat("`", 13)},
		{"long run mid-line", "a" + strings.Repeat("`", 12) + "b", strings.Repeat("`", 13)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := promptText([]acpsdk.ContentBlock{embeddedText("file:///repo/f", tc.text), acpsdk.TextBlock("What does it return?")})
			if err != nil {
				t.Fatal(err)
			}
			want := "Contents of file:///repo/f:\n" + tc.fence + "\n" + tc.text + "\n" + tc.fence + "\n\nWhat does it return?"
			if got != want {
				t.Errorf("prompt =\n%s\nwant\n%s", got, want)
			}
			wantBlocks := []mdBlock{
				{kind: "Paragraph", text: "Contents of file:///repo/f:"},
				{kind: "FencedCodeBlock", text: fencedContent(tc.text)},
				{kind: "Paragraph", text: "What does it return?"},
			}
			if blocks := markdownBlocks(got); !slices.Equal(blocks, wantBlocks) {
				t.Errorf("parsed as %q\nwant      %q", blocks, wantBlocks)
			}
		})
	}
}

// Two attachments, the first with an unclosed ``` line and the second a raw
// HTML document. The web chat's autoFenceRawHtmlDocument pre-processor
// (TypeScript, so not run here) flips its own in-a-fence state on every line
// that starts with ```, whatever its length, so the first attachment puts it
// out of step and it wraps the second's document in a ```html line and a bare
// ``` — inside fleet's block. The test makes those two insertions: a bare ```
// would close a three-backtick fence, and the HTML's tail would render as
// prose while fleet's own closer swallowed the user's question.
func TestFenceSurvivesTheWebHTMLPreprocessor(t *testing.T) {
	a := "```\nnever closed"
	b := "<!DOCTYPE html>\n<html>…</html>\n<!-- c -->\nafter html"
	got, err := promptText([]acpsdk.ContentBlock{
		embeddedText("file:///repo/a", a),
		embeddedText("file:///repo/b.html", b),
		acpsdk.TextBlock("What does it return?"),
	})
	if err != nil {
		t.Fatal(err)
	}
	web := strings.NewReplacer("\n<!DOCTYPE html>\n", "\n```html\n<!DOCTYPE html>\n", "</html>\n", "</html>\n```\n").Replace(got)
	if web == got {
		t.Fatalf("the pre-processor's insertions did not apply to %q", got)
	}
	for _, tc := range []struct{ name, md, b string }{
		{"as sent", got, b},
		{"after the web pre-processor", web, "```html\n<!DOCTYPE html>\n<html>…</html>\n```\n<!-- c -->\nafter html"},
	} {
		want := []mdBlock{
			{kind: "Paragraph", text: "Contents of file:///repo/a:"},
			{kind: "FencedCodeBlock", text: fencedContent(a)},
			{kind: "Paragraph", text: "Contents of file:///repo/b.html:"},
			{kind: "FencedCodeBlock", text: fencedContent(tc.b)},
			{kind: "Paragraph", text: "What does it return?"},
		}
		if blocks := markdownBlocks(tc.md); !slices.Equal(blocks, want) {
			t.Errorf("%s:\n%s\nparsed as %q\nwant      %q", tc.name, tc.md, blocks, want)
		}
	}
}

// A line break in a URI or a link's name stays on fleet's line instead of
// starting Markdown of its own: "file:///x\n````" would make "````:" a fence
// opener that runs on over the attachment and the user's question.
func TestLineBreaksStayInsideURIsAndNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block acpsdk.ContentBlock
		want  []mdBlock
	}{
		{"embedded resource URI", embeddedText("file:///x\n````", "print(1)"), []mdBlock{
			{kind: "Paragraph", text: "Contents of file:///x%0A````:"},
			{kind: "FencedCodeBlock", text: "print(1)\n"},
		}},
		{"embedded resource URI with CRLF", embeddedText("file:///x\r\n````", "print(1)"), []mdBlock{
			{kind: "Paragraph", text: "Contents of file:///x%0D%0A````:"},
			{kind: "FencedCodeBlock", text: "print(1)\n"},
		}},
		{"resource_link URI", acpsdk.ResourceLinkBlock("main.go", "file:///x\n````"), []mdBlock{
			{kind: "Paragraph", text: "[main.go](file:///x%0A````)", link: "file:///x%0A````"},
		}},
		{"resource_link name", acpsdk.ResourceLinkBlock("a\n```\r\nb", "file:///x"), []mdBlock{
			{kind: "Paragraph", text: "[a ``` b](file:///x)", link: "file:///x"},
		}},
		{"resource_link named by its URI", acpsdk.ResourceLinkBlock("", "file:///x\n>q"), []mdBlock{
			{kind: "Paragraph", text: "[file:///x%0A>q](file:///x%0A>q)", link: "file:///x%0A>q"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := promptText([]acpsdk.ContentBlock{tc.block, acpsdk.TextBlock("What does it return?")})
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Concat(tc.want, []mdBlock{{kind: "Paragraph", text: "What does it return?"}})
			if blocks := markdownBlocks(got); !slices.Equal(blocks, want) {
				t.Errorf("%s\nparsed as %q\nwant      %q", got, blocks, want)
			}
		})
	}
}

func embeddedText(uri, text string) acpsdk.ContentBlock {
	return acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{TextResourceContents: &acpsdk.TextResourceContents{Uri: uri, Text: text}})
}

// mdBlock is one top-level block of a parsed prompt: its goldmark kind, its
// source text (a fenced block's content, a paragraph's inline source), and
// the destination of the first link in it, if any.
type mdBlock struct{ kind, text, link string }

// markdownBlocks parses md with goldmark, the CommonMark parser fleet's own
// HTML export uses, so a test checks what a renderer makes of a prompt rather
// than what a hand-rolled reader thinks. goldmark ends lines at "\n" and
// "\r\n" only, where CommonMark (and the web chat's micromark) also ends one
// at a lone "\r", so a lone CR is turned into a line break first.
func markdownBlocks(md string) []mdBlock {
	src := []byte(loneCR.Replace(md))
	doc := goldmark.DefaultParser().Parse(gmtext.NewReader(src))
	var out []mdBlock
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		b := mdBlock{kind: n.Kind().String()}
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.text += string(seg.Value(src))
		}
		for c := n.FirstChild(); c != nil && b.link == ""; c = c.NextSibling() {
			if l, ok := c.(*ast.Link); ok {
				b.link = string(l.Destination)
			}
		}
		out = append(out, b)
	}
	return out
}

var loneCR = strings.NewReplacer("\r\n", "\r\n", "\r", "\n")

// fencedContent is what CommonMark reads as the content of fleet's block
// around text: every content line keeps its line ending, so it is the text
// plus the newline fleet writes before the closer, with markdownBlocks' line
// endings.
func fencedContent(text string) string { return loneCR.Replace(text + "\n") }

// A Stop fleet did not accept must not be reported as a clean stop: the turn
// outlives its stream, so the client is told it may still be running.
func TestUnconfirmedStopIsNotReportedAsStopped(t *testing.T) {
	t.Run("session/cancel", func(t *testing.T) {
		started := make(chan struct{})
		h := newHarness(t, harnessOpts{turn: blockingTurn(started), cancelStatus: http.StatusInternalServerError, publicURL: "https://fleet.example.com"})
		sid := h.newSession(t)
		done := make(chan acpsdk.PromptResponse, 1)
		go func() {
			r, _ := h.prompt(sid, "long job")
			done <- r
		}()
		<-started
		if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		var r acpsdk.PromptResponse
		select {
		case r = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("prompt did not return")
		}
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q (ACP still requires cancelled)", r.StopReason)
		}
		if got := h.client.text(); !strings.Contains(got, "could not confirm this turn stopped") || !strings.Contains(got, "https://fleet.example.com/chat?c=conv-slow") {
			t.Errorf("no warning that the turn may still run: %q", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		started := make(chan struct{})
		h := newHarness(t, harnessOpts{turn: blockingTurn(started), timeout: 200 * time.Millisecond, cancelStatus: http.StatusBadGateway})
		_, err := h.prompt(h.newSession(t), "long job")
		if err == nil || !strings.Contains(err.Error(), "stopping it failed") || strings.Contains(err.Error(), "was stopped") {
			t.Fatalf("err = %v, want a timeout error saying the stop failed", err)
		}
	})
}

// Stopped from the web chat (fleet's own Stop): the server ends the stream with
// turn.cancelled, which is a cancellation, not an internal error.
func TestServerSideStopIsACancellation(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("text.delta", map[string]any{"text": "partial"})
		w.emit("turn.cancelled", map[string]any{})
	}})
	resp, err := h.prompt(h.newSession(t), "x")
	if err != nil || resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("got %+v, %v; want stopReason cancelled", resp, err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("an already-stopped turn must not be stopped again: %q", h.fleet.cancels)
	}
}

// A staged critical tool is paused for a person, not failed — even though its
// placeholder result carries is_err.
func TestStagedToolIsPendingNotFailed(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-a"})
		w.emit("tool.call", map[string]any{"id": "call-s", "name": "mcp_sendgrid_send_email"})
		w.emit("tool.approval_required", map[string]any{"approval_id": "ap-1", "tool": "mcp_sendgrid_send_email"})
		w.emit("tool.result", map[string]any{"id": "call-s", "name": "mcp_sendgrid_send_email", "text": "APPROVAL_REQUIRED: staged for review", "is_err": true})
		w.emit("tool.call", map[string]any{"id": "call-f", "name": "run_python"})
		w.emit("tool.result", map[string]any{"id": "call-f", "name": "run_python", "text": "boom", "is_err": true})
		w.emit("turn.completed", map[string]any{})
	}})
	if _, err := h.prompt(h.newSession(t), "email bob"); err != nil {
		t.Fatal(err)
	}
	status := map[acpsdk.ToolCallId]acpsdk.ToolCallStatus{}
	for _, u := range h.client.updates {
		if u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil {
			status[u.ToolCallUpdate.ToolCallId] = *u.ToolCallUpdate.Status
		}
	}
	if status["call-s"] != acpsdk.ToolCallStatusPending || status["call-f"] != acpsdk.ToolCallStatusFailed {
		t.Errorf("statuses = %v, want call-s pending and call-f failed", status)
	}
}

// The APPROVAL_REQUIRED prefix without a created card (staging itself failed)
// is not something anyone can approve, so it stays failed.
func TestStagingFailureIsNotPending(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("tool.call", map[string]any{"id": "call-b", "name": "bash"})
		w.emit("tool.result", map[string]any{"id": "call-b", "name": "bash", "text": "APPROVAL_REQUIRED: risky. Could not stage for user approval (db down).", "is_err": true})
		w.emit("turn.completed", map[string]any{})
	}})
	if _, err := h.prompt(h.newSession(t), "x"); err != nil {
		t.Fatal(err)
	}
	for _, u := range h.client.updates {
		if u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil && *u.ToolCallUpdate.Status != acpsdk.ToolCallStatusFailed {
			t.Errorf("status = %q, want failed", *u.ToolCallUpdate.Status)
		}
	}
	if strings.Contains(h.client.text(), "Approval needed") {
		t.Error("no card was created, so no approval pointer may be sent")
	}
}

// A cancel that races the turn's own end must not send a Stop: it is
// conversation-scoped and would hit a follow-up queued from another surface.
func TestNoStopAfterTheTurnEnded(t *testing.T) {
	ended := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-done"})
		w.emit("text.delta", map[string]any{"text": "done"})
		w.emit("turn.completed", map[string]any{})
		close(ended)
		<-r.Context().Done() // the socket lingers after the terminal frame
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "x")
		done <- r
	}()
	<-ended
	time.Sleep(50 * time.Millisecond) // let the terminal frame be read
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("a Stop was sent after the turn ended: %q", h.fleet.cancels)
	}
	// Nothing was stopped, and the transcript says the finished turn stands.
	if !strings.Contains(h.client.text(), "already finished") {
		t.Errorf("a cancel after the terminal frame read as a confirmed stop: %q", h.client.text())
	}
}

// A timeout that fires as the turn completes is not a timeout.
func TestTimeoutAfterTheTurnEndedIsAnEndTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{timeout: 100 * time.Millisecond, turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
		<-r.Context().Done() // lingers past the timeout
	}})
	resp, err := h.prompt(h.newSession(t), "x")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("got %+v, %v; want end_turn", resp, err)
	}
}

// A pending approval is still pointed to when the turn later errors.
func TestApprovalPointerSurvivesATurnError(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-e"})
		w.emit("tool.approval_required", map[string]any{"approval_id": "ap-9", "tool": "mcp_sendgrid_send_email"})
		w.emit("turn.error", map[string]any{"message": "provider failed"})
	}})
	_, err := h.prompt(h.newSession(t), "email bob")
	if err == nil {
		t.Fatal("want the turn error")
	}
	if got := h.client.text(); !strings.Contains(got, "--approve ap-9") {
		t.Errorf("approval pointer missing on an errored turn: %q", got)
	}
}

// The server names a new conversation on the response headers before any
// frame (#1591). A stream that dies before the conversation frame must not
// lose it: the session keeps the conversation, and a cancel can address it.
func TestConversationIDFromTheResponseHeader(t *testing.T) {
	t.Run("stream dies before the first frame", func(t *testing.T) {
		calls := 0
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				w.w.Header().Set("X-Fleet-Conversation-Id", "conv-hdr")
				w.w.(http.Flusher).Flush() // headers only, then the socket closes
				return
			}
			w.emit("conversation", map[string]any{"id": "conv-hdr"})
			w.emit("turn.completed", map[string]any{})
		}})
		sid := h.newSession(t)
		if _, err := h.prompt(sid, "first"); err == nil {
			t.Fatal("want the interrupted-stream error")
		}
		if _, err := h.prompt(sid, "retry"); err != nil {
			t.Fatal(err)
		}
		h.fleet.mu.Lock()
		defer h.fleet.mu.Unlock()
		if got := h.fleet.chats[1].ConversationID; got != "conv-hdr" {
			t.Errorf("retry conversation = %q, want conv-hdr (not a second conversation)", got)
		}
	})
	t.Run("cancel before the first frame", func(t *testing.T) {
		started := make(chan struct{})
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
			w.w.Header().Set("X-Fleet-Conversation-Id", "conv-hdr2")
			w.w.Header().Set("X-Fleet-Turn-Id", "turn-hdr2")
			w.w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}})
		sid := h.newSession(t)
		done := make(chan struct{})
		go func() {
			_, _ = h.prompt(sid, "x")
			close(done)
		}()
		<-started
		if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("prompt did not return")
		}
		h.fleet.mu.Lock()
		defer h.fleet.mu.Unlock()
		if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-hdr2 {"scope":"turn","turn_id":"turn-hdr2"}` {
			t.Errorf("cancels = %q, want the header's conversation stopped", h.fleet.cancels)
		}
	})
}

// A preview_email card is display-only (its one action is Dismiss), even though
// it arrives as tool.approval_required: no approve instructions, and the call
// is shown as completed, not failed or pending.
func TestPreviewCardIsNotAnApproval(t *testing.T) {
	h := newHarness(t, harnessOpts{publicURL: "https://fleet.example.com", turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-p"})
		w.emit("tool.call", map[string]any{"id": "call-p", "name": "preview_email"})
		w.emit("tool.approval_required", map[string]any{"approval_id": "prev-1", "tool": "preview_email"})
		w.emit("tool.result", map[string]any{"id": "call-p", "name": "preview_email", "text": "PREVIEW_DISPLAYED: the user is now viewing your draft", "is_err": true})
		w.emit("turn.completed", map[string]any{})
	}})
	if _, err := h.prompt(h.newSession(t), "draft an email"); err != nil {
		t.Fatal(err)
	}
	got := h.client.text()
	if strings.Contains(got, "--approve") || strings.Contains(got, "allow or deny") {
		t.Errorf("a preview must not be presented as an approval: %q", got)
	}
	if !strings.Contains(got, "draft email preview") || !strings.Contains(got, "https://fleet.example.com/chat?c=conv-p") {
		t.Errorf("no preview pointer: %q", got)
	}
	for _, u := range h.client.updates {
		if u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil && *u.ToolCallUpdate.Status != acpsdk.ToolCallStatusCompleted {
			t.Errorf("preview status = %q, want completed", *u.ToolCallUpdate.Status)
		}
	}
}

// A prompt sent while another surface is running a turn in the conversation is
// durably queued by fleet (202): an accepted prompt, not an error, and each
// prompt carries an idempotency key so a re-POST cannot queue it twice.
func TestQueuedPromptIsAcceptedNotFailed(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.emit("conversation", map[string]any{"id": "conv-q"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"in-7","position":2,"ahead":0,"state":"queued"},"conversation_id":"conv-q"}`)
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	resp, err := h.prompt(sid, "second, while the web chat is busy")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("got %+v, %v; want an accepted end_turn", resp, err)
	}
	// position is the ordering key (2 here, over an earlier completed input),
	// never shown as a place in line; ahead is the place.
	if got := h.client.text(); !strings.Contains(got, "your message was queued and will run after it (next in line)") || strings.Contains(got, "position") {
		t.Errorf("queued notice = %q, want the place in line and no position", got)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if a, b := h.fleet.chats[0].InputID, h.fleet.chats[1].InputID; a == "" || b == "" || a == b {
		t.Errorf("input ids = %q, %q; want one distinct idempotency key per prompt", a, b)
	}
}

// The Stop names the watched turn once turn.started has been seen, so the
// server cannot cancel a successor with it.
func TestStopNamesTheWatchedTurn(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-t"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-42"})
		close(started)
		<-r.Context().Done()
	}})
	sid := h.newSession(t)
	done := make(chan struct{})
	go func() {
		_, _ = h.prompt(sid, "long job")
		close(done)
	}()
	<-started
	time.Sleep(50 * time.Millisecond) // let turn.started be read
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || !strings.Contains(h.fleet.cancels[0], `"turn_id":"turn-42"`) {
		t.Errorf("cancels = %q, want the Stop targeted at turn-42", h.fleet.cancels)
	}
}

// Without the watched turn's id the adapter never sends an untargeted Stop
// (it could land on a successor): the Stop names this prompt's own input key,
// which fleet resolves to its turn or refuses to launch.
func TestNoUntargetedStop(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-n"}) // no turn id, header or frame
		close(started)
		<-r.Context().Done()
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "x")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q", r.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	key := h.fleet.chats[0].InputID
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-n {"input_id":"`+key+`","scope":"turn"}` {
		t.Errorf("cancels = %q, want one Stop naming input %q", h.fleet.cancels, key)
	}
}

// nth is the ordinal (from 1) of the POST /chat being served: the fake
// records each request before handing it to turn.
func (f *fakeFleet) nth() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chats)
}

// resent reports whether the POST /chat being served carries an input_id an
// earlier one did: fleet answers such a resend with the input it holds.
func (f *fakeFleet) resent() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.chats[len(f.chats)-1].InputID
	for _, c := range f.chats[:len(f.chats)-1] {
		if c.InputID == last {
			return true
		}
	}
	return false
}

// replayAck is fleet's answer to a resend of an input it already holds.
func replayAck(w *sseWriter, state, conv string) {
	w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","mode":"direct","state":"`+state+`"},"conversation_id":"`+conv+`"}`)
}

// heldTurn streams the start of a turn, closes started, then holds the turn
// until release, when it completes with " and done" — or until the request
// is gone (a Stop ended the stream).
func heldTurn(w *sseWriter, r *http.Request, conv, turn string, started, release chan struct{}) {
	w.emit("conversation", map[string]any{"id": conv})
	w.emit("turn.started", map[string]any{"turn_id": turn})
	w.emit("text.delta", map[string]any{"text": "working"})
	close(started)
	select {
	case <-release:
	case <-r.Context().Done():
		return
	}
	w.emit("text.delta", map[string]any{"text": " and done"})
	w.emit("turn.completed", map[string]any{"prompt_tokens": 3, "completion_tokens": 2})
}

// stopWindow is how long a test lets a wrongly-sent Stop go out before it
// lets a held turn complete: the Stop needs only a loopback round trip.
const stopWindow = 200 * time.Millisecond

// A prompt cancelled while it waits for the session (an earlier prompt still
// running) is never submitted: session/cancel stops the running turn and
// answers both prompts cancelled, and only the first was ever sent to fleet.
func TestCancelledPromptIsNeverSubmitted(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("turn.error", map[string]any{"message": "a cancelled prompt was submitted"})
			return
		}
		heldTurn(w, r, "conv-w", "turn-w", started, release)
	}})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "long job", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "and then this", nil, 2)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	for i, done := range []<-chan promptResult{first, second} {
		if r := await(t, done, fmt.Sprintf("prompt %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
			t.Errorf("prompt %d = %+v, %v; want cancelled", i+1, r.resp, r.err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 1 {
		t.Errorf("a prompt cancelled before submission was POSTed: %+v", h.fleet.chats)
	}
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-w {"scope":"turn","turn_id":"turn-w"}` {
		t.Errorf("cancels = %q, want the running turn stopped", h.fleet.cancels)
	}
}

// An ACP client that resends a prompt whose request is still open (a retry,
// with the same messageId) must not stop the turn it retries. The SDK cancels
// the first request's context the moment the resend arrives; fleet acp does
// not take that as a Stop. The original streams on and answers with its own
// outcome, and the resend, once it gets the session, is answered with the
// replay of the same input: the message runs once, and nothing is stopped.
func TestResendWhileRunningIsAReplayNotAStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.resent() {
			replayAck(w, "completed", "conv-1")
			return
		}
		heldTurn(w, r, "conv-1", "turn-1", started, release)
	}})
	sid := h.newSession(t)
	mid := "res-B"
	first := h.promptAsync(t, sid, "run the slow job", &mid, 1)
	<-started
	second := h.promptAsync(t, sid, "run the slow job", &mid, 2) // the client's retry
	time.Sleep(stopWindow)
	close(release)

	r1, r2 := await(t, first, "the original prompt"), await(t, second, "the resend")
	if r1.err != nil || r1.resp.StopReason != acpsdk.StopReasonEndTurn || r1.resp.UserMessageId == nil || *r1.resp.UserMessageId != mid || r1.resp.Usage == nil {
		t.Errorf("original = %+v, %v; want its own end_turn, userMessageId %q and usage", r1.resp, r1.err, mid)
	}
	if r2.err != nil || r2.resp.StopReason != acpsdk.StopReasonEndTurn || r2.resp.UserMessageId == nil || *r2.resp.UserMessageId != mid {
		t.Errorf("resend = %+v, %v; want end_turn echoing userMessageId %q", r2.resp, r2.err, mid)
	}
	if text := h.client.text(); !strings.Contains(text, "working and done") || !strings.Contains(text, "fleet already took this message from an earlier attempt (it is not run twice)") {
		t.Errorf("text = %q, want the original's whole answer and the resend's replay note", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("the resend stopped the turn it retries: cancels = %q", h.fleet.cancels)
	}
	if len(h.fleet.chats) != 2 || h.fleet.chats[0].InputID != h.fleet.chats[1].InputID || h.fleet.chats[1].ConversationID != "conv-1" {
		t.Errorf("chats = %+v, want the original and one resend of the same input into conv-1", h.fleet.chats)
	}
}

// The same holds for a text-only resend. A prompt whose answer was lost keeps
// its key for its text, so a resend reuses it; a second resend that arrives
// while the first is running is the same message again, even though the
// running one settles the key before the newer one gets the session.
func TestTextResendWhileRunningIsAReplay(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		switch {
		case h.fleet.nth() == 1:
			w.w.WriteHeader(http.StatusOK) // the answer is lost; fleet never committed the input
		case h.fleet.resent() && h.fleet.nth() > 2:
			replayAck(w, "completed", "conv-t") // fleet holds the key now
		case h.fleet.nth() == 2:
			heldTurn(w, r, "conv-t", "turn-t", started, release) // the first resend runs it
		default:
			w.emit("conversation", map[string]any{"id": "conv-t"})
			w.emit("text.delta", map[string]any{"text": "ran a second time"})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "book the room"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	first := h.promptAsync(t, sid, "book the room", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "book the room", nil, 2)
	time.Sleep(stopWindow)
	close(release)

	for i, done := range []<-chan promptResult{first, second} {
		if r := await(t, done, fmt.Sprintf("resend %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
			t.Errorf("resend %d = %+v, %v; want end_turn", i+1, r.resp, r.err)
		}
	}
	if text := h.client.text(); !strings.Contains(text, "working and done") || !strings.Contains(text, "fleet already took this message") || strings.Contains(text, "ran a second time") {
		t.Errorf("text = %q, want the run's answer and the replay note, and no second run", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("a resend stopped the turn: cancels = %q", h.fleet.cancels)
	}
	if len(h.fleet.chats) != 3 || h.fleet.chats[1].InputID != h.fleet.chats[0].InputID || h.fleet.chats[2].InputID != h.fleet.chats[0].InputID {
		t.Errorf("chats = %+v, want every attempt under the lost prompt's key", h.fleet.chats)
	}
}

// A different prompt sent while a turn runs does not stop it either. Prompts
// on a session run one at a time, so the new one waits for the session and
// then runs as its own turn, once the first has answered with its own outcome.
func TestSecondPromptWhileRunningWaitsItsTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("conversation", map[string]any{"id": "conv-1"})
			w.emit("turn.started", map[string]any{"turn_id": "turn-2"})
			w.emit("text.delta", map[string]any{"text": "; second answer"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		heldTurn(w, r, "conv-1", "turn-1", started, release)
	}})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "first job", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "second job", nil, 2)
	time.Sleep(stopWindow)
	if n := h.fleet.nth(); n != 1 {
		t.Errorf("chats = %d while the first turn runs, want 1: the second waits for the session", n)
	}
	close(release)

	for i, done := range []<-chan promptResult{first, second} {
		if r := await(t, done, fmt.Sprintf("prompt %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
			t.Errorf("prompt %d = %+v, %v; want end_turn", i+1, r.resp, r.err)
		}
	}
	if text := h.client.text(); text != "working and done; second answer" {
		t.Errorf("text = %q, want both answers whole, in order", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("the second prompt stopped the first turn: cancels = %q", h.fleet.cancels)
	}
	if len(h.fleet.chats) != 2 || h.fleet.chats[1].Message != "second job" || h.fleet.chats[1].ConversationID != "conv-1" || h.fleet.chats[1].InputID == h.fleet.chats[0].InputID {
		t.Errorf("chats = %+v, want the second prompt as its own input in conv-1", h.fleet.chats)
	}
}

// A session/cancel still stops the newer prompt after a superseded one has
// returned. The SDK keeps one cancel per session, and the returning prompt
// deletes it although it is the newer prompt's, so the SDK's own handling of
// session/cancel reaches nothing; fleet acp's per-prompt registry does.
func TestCancelAfterASupersededPromptStopsTheNewerOne(t *testing.T) {
	started1, release1, started2 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("conversation", map[string]any{"id": "conv-1"})
			w.emit("turn.started", map[string]any{"turn_id": "turn-2"})
			close(started2)
			<-r.Context().Done()
			return
		}
		heldTurn(w, r, "conv-1", "turn-1", started1, release1)
	}})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "first job", nil, 1)
	<-started1
	second := h.promptAsync(t, sid, "second job", nil, 2)
	close(release1)
	if r := await(t, first, "the first prompt"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Errorf("first prompt = %+v, %v; want its own end_turn", r.resp, r.err)
	}
	select {
	case <-started2:
	case <-time.After(10 * time.Second):
		t.Fatal("the second prompt's turn never started")
	}
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if r := await(t, second, "the newer prompt after session/cancel"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Errorf("newer prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-1 {"scope":"turn","turn_id":"turn-2"}` {
		t.Errorf("cancels = %q, want only the newer turn stopped", h.fleet.cancels)
	}
}

// A guard of the Agent's side of a hang-up (it passes on main too): when the
// client closes fleet acp's stdin mid-turn, the Agent triggers the stop of
// every prompt in flight, as a session/cancel would. The running turn gets a
// Stop and answers cancelled, and a prompt still waiting for the session is
// never submitted. This harness does not exit: that `fleet acp` stays alive
// until the Stop is answered (bounded), and handles signals and a broken
// stdout the same way, is run's side, pinned by the TestRun* and
// TestProcess* tests in run_test.go.
func TestHangUpTriggersTheAgentsStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("turn.error", map[string]any{"message": "submitted after the client hung up"})
			return
		}
		heldTurn(w, r, "conv-h", "turn-h", started, release)
	}})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "long job", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "queued behind it", nil, 2)
	h.hangUp()
	// The answers still reach the client: only its side of stdin is closed.
	for i, done := range []<-chan promptResult{first, second} {
		if r := await(t, done, fmt.Sprintf("prompt %d after the hang-up", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
			t.Errorf("prompt %d = %+v, %v; want cancelled", i+1, r.resp, r.err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-h {"scope":"turn","turn_id":"turn-h"}` {
		t.Errorf("cancels = %q, want the running turn stopped when the client hung up", h.fleet.cancels)
	}
	if len(h.fleet.chats) != 1 {
		t.Errorf("a prompt was submitted after the client hung up: %+v", h.fleet.chats)
	}
}

// A client that goes away while prompts wait in a session's line (see
// Agent.line), behind a turn whose Stop is slow: the waiters leave the line
// at once, answered cancelled and never submitted, without waiting for the
// turn ahead to finish stopping, so run's bounded wait (awaitStops) holds
// only for that turn. Only the running turn gets a Stop.
func TestHangUpReleasesPromptsWaitingInLine(t *testing.T) {
	started, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	var h *harness
	h = newHarness(t, harnessOpts{cancelHold: hold, turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("turn.error", map[string]any{"message": "submitted after the client hung up"})
			return
		}
		blockingTurn(started)(w, r)
	}})
	t.Cleanup(release)
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "long job", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "waits in line", nil, 2)
	third := h.promptAsync(t, sid, "waits behind it", nil, 3)
	h.hangUp()

	h.fleet.awaitStop(t) // the running turn's Stop is out, and held
	for i, done := range []<-chan promptResult{second, third} {
		select {
		case r := <-done:
			if r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
				t.Errorf("waiting prompt %d = %+v, %v; want cancelled", i+1, r.resp, r.err)
			}
		case <-time.After(5 * time.Second): // well inside the held Stop's 10s client timeout
			t.Fatalf("waiting prompt %d did not leave the line while the turn ahead was still stopping", i+1)
		}
	}
	h.waitTracked(t, sid, 1) // only the turn being stopped is left in flight
	select {
	case r := <-first:
		t.Fatalf("the running prompt answered (%+v, %v) before its Stop did", r.resp, r.err)
	default:
	}
	release()
	if r := await(t, first, "the running prompt"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Errorf("running prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 1 {
		t.Errorf("a waiting prompt was submitted after the client hung up: %+v", h.fleet.chats)
	}
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-slow {"scope":"turn","turn_id":"turn-slow"}` {
		t.Errorf("cancels = %q, want only the running turn stopped", h.fleet.cancels)
	}
}

func (f *fakeFleet) cancelsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancels)
}

// awaitStop waits (bounded) until the fake has received a Stop, and returns
// the Stops received so far.
func (f *fakeFleet) awaitStop(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := f.cancelsSnapshot()
		if len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake never received a Stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A text-only prompt whose lost answer's Stop fleet confirmed drops its key,
// so the same text sent again runs. That holds for one sent while that Stop
// is still settling: it arrives with the stopped key as its arrival key, but
// must not fall back to it, or it would be answered "an earlier attempt of
// this message was cancelled" instead of running.
func TestTextSentAgainDuringAConfirmedStopRuns(t *testing.T) {
	started, settle := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		switch {
		case h.fleet.nth() == 1:
			w.w.WriteHeader(http.StatusOK) // the answer is lost; fleet never committed the input
		case h.fleet.nth() == 2: // the resend runs it until it is stopped
			w.emit("conversation", map[string]any{"id": "conv-s"})
			w.emit("turn.started", map[string]any{"turn_id": "turn-s"})
			close(started)
			<-settle
			w.emit("turn.cancelled", map[string]any{})
		case h.fleet.resent():
			replayAck(w, "cancelled", "conv-s")
		default:
			w.emit("conversation", map[string]any{"id": "conv-s"})
			w.emit("text.delta", map[string]any{"text": "booked"})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	// The Stop settles on the turn's own terminal frame, which the test holds.
	prevSettle := stopSettleWait
	stopSettleWait = 10 * time.Second
	t.Cleanup(func() { stopSettleWait = prevSettle })
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "book the room"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	first := h.promptAsync(t, sid, "book the room", nil, 1)
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	// The Stop is out, so the cancel has been handled: the next prompt is
	// sent after it, and the cancel does not reach it.
	deadline := time.Now().Add(10 * time.Second)
	for len(h.fleet.cancelsSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := h.promptAsync(t, sid, "book the room", nil, 2)
	close(settle)

	if r := await(t, first, "the stopped resend"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Errorf("stopped resend = %+v, %v; want cancelled", r.resp, r.err)
	}
	if r := await(t, second, "the text sent again"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Errorf("text sent again = %+v, %v; want end_turn", r.resp, r.err)
	}
	if text := h.client.text(); !strings.Contains(text, "booked") || strings.Contains(text, "send it again as a new message") {
		t.Errorf("text = %q, want the message run again, not the cancelled replay", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 3 || h.fleet.chats[2].InputID == h.fleet.chats[0].InputID {
		t.Errorf("chats = %+v, want the text sent again under a fresh key", h.fleet.chats)
	}
}

// Prompts waiting for a session run in the order they were tracked, even
// when a later one reaches the session first: here "second" is held between
// tracking and its wait (where the SDK's goroutine scheduling can leave it)
// until "third" is already waiting. Arrival is the order of track calls; the
// SDK does not promise its handlers start in wire order.
func TestWaitingPromptsRunInArrivalOrder(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	gate := make(chan struct{})
	prevHook := promptTracked
	promptTracked = func(message string) {
		if message == "second" {
			<-gate
		}
	}
	t.Cleanup(func() { promptTracked = prevHook })
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("conversation", map[string]any{"id": "conv-o"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		heldTurn(w, r, "conv-o", "turn-o", started, release)
	}})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "first", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "second", nil, 2)
	third := h.promptAsync(t, sid, "third", nil, 3)
	time.Sleep(stopWindow) // "third" is waiting for the session; "second" is still held
	close(gate)
	time.Sleep(stopWindow) // "second" is waiting too, behind "third" for the session's mutex
	close(release)
	for i, done := range []<-chan promptResult{first, second, third} {
		if r := await(t, done, fmt.Sprintf("prompt %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
			t.Errorf("prompt %d = %+v, %v; want end_turn", i+1, r.resp, r.err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	order := make([]string, 0, len(h.fleet.chats))
	for _, c := range h.fleet.chats {
		order = append(order, c.Message)
	}
	if strings.Join(order, ",") != "first,second,third" {
		t.Errorf("fleet received %q, want the prompts in the order they arrived", order)
	}
}

// Prompts waiting behind a session's running turn are bounded: they have not
// reached fleet, so the server's input-queue cap cannot see them. One beyond
// maxWaitingPrompts is refused at once, never held or sent, and the ones
// admitted still run in order once the session frees.
func TestPromptsWaitingForASessionAreCapped(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("conversation", map[string]any{"id": "conv-c"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		heldTurn(w, r, "conv-c", "turn-c", started, release)
	}})
	sid := h.newSession(t)
	admitted := make([]<-chan promptResult, 0, 1+maxWaitingPrompts)
	admitted = append(admitted, h.promptAsync(t, sid, "long job", nil, 1))
	<-started
	for i := range maxWaitingPrompts {
		admitted = append(admitted, h.promptAsync(t, sid, fmt.Sprintf("job %d", i), nil, i+2))
	}
	// Sent without blocking the test: an uncapped agent would hold it.
	over := make(chan error, 1)
	go func() {
		_, err := h.prompt(sid, "one too many")
		over <- err
	}()
	var err error
	select {
	case err = <-over:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the prompt beyond the cap was held to wait for the session instead of refused")
	}
	// Said in the message itself, which is all some clients show.
	if err == nil || rpcCode(err) != -32603 || !strings.Contains(wireError(t, err).Message, fmt.Sprintf("a prompt running and %d waiting", maxWaitingPrompts)) {
		t.Fatalf("prompt beyond the cap = %v, want an internal error saying the session has too many prompts waiting", err)
	}
	h.waitTracked(t, sid, maxWaitingPrompts+1) // the refused prompt was never tracked
	close(release)
	for i, done := range admitted {
		if r := await(t, done, fmt.Sprintf("prompt %d", i)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
			t.Errorf("admitted prompt %d = %+v, %v; want end_turn", i, r.resp, r.err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != maxWaitingPrompts+1 {
		t.Errorf("chats = %d, want %d: the refused prompt must never reach fleet", len(h.fleet.chats), maxWaitingPrompts+1)
	}
	for _, c := range h.fleet.chats {
		if c.Message == "one too many" {
			t.Errorf("the refused prompt was submitted: %+v", c)
		}
	}
}

// Every same-text prompt that waited behind a resend whose Stop fleet
// confirmed (here, on --timeout) is a resend of one message: the first to get
// the session runs it under a fresh key, and the next reuses that key, so
// fleet answers it with that run instead of running the message twice.
func TestWaitersBehindAConfirmedStopShareOneSuccessorKey(t *testing.T) {
	started := make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{timeout: time.Second, turn: func(w *sseWriter, r *http.Request) {
		switch {
		case h.fleet.nth() == 1:
			w.w.WriteHeader(http.StatusOK) // the answer is lost; fleet never committed the input
		case h.fleet.nth() == 2: // the resend runs until the timeout's Stop ends it
			w.emit("conversation", map[string]any{"id": "conv-t"})
			w.emit("turn.started", map[string]any{"turn_id": "turn-t"})
			close(started)
			<-r.Context().Done()
		case h.fleet.resent():
			replayAck(w, "completed", "conv-t")
		default:
			w.emit("conversation", map[string]any{"id": "conv-t"})
			w.emit("text.delta", map[string]any{"text": "booked"})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "book the room"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	resend := h.promptAsync(t, sid, "book the room", nil, 1)
	<-started
	// Both arrive while the lost answer's key is unresolved, so both take it
	// as their arrival key, and both wait behind the resend's Stop.
	w1 := h.promptAsync(t, sid, "book the room", nil, 2)
	w2 := h.promptAsync(t, sid, "book the room", nil, 3)

	if r := await(t, resend, "the timed-out resend"); r.err == nil || !strings.Contains(r.err.Error(), "was stopped") {
		t.Errorf("timed-out resend = %+v, %v; want the confirmed-stop timeout error", r.resp, r.err)
	}
	for i, done := range []<-chan promptResult{w1, w2} {
		if r := await(t, done, fmt.Sprintf("waiter %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
			t.Errorf("waiter %d = %+v, %v; want end_turn", i+1, r.resp, r.err)
		}
	}
	if text := h.client.text(); strings.Count(text, "booked") != 1 || !strings.Contains(text, "already took this message") {
		t.Errorf("text = %q, want the message run once and the second waiter answered with that run", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 4 {
		t.Fatalf("chats = %+v, want 4", h.fleet.chats)
	}
	if k := h.fleet.chats[2].InputID; k == h.fleet.chats[1].InputID || h.fleet.chats[3].InputID != k {
		t.Errorf("keys = %q, %q, %q; want the stopped key dropped and one successor key shared by both waiters",
			h.fleet.chats[1].InputID, h.fleet.chats[2].InputID, h.fleet.chats[3].InputID)
	}
}

// Every prompt leaves inflight once it is answered (completed, failed, or
// cancelled while running or while waiting), so a session/cancel reaches only
// prompts still open, and the registry does not grow.
func TestInflightEmptiesWhenPromptsAreAnswered(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		switch h.fleet.nth() {
		case 1:
			w.emit("conversation", map[string]any{"id": "conv-i"})
			w.emit("turn.completed", map[string]any{})
		case 3:
			heldTurn(w, r, "conv-i", "turn-i", started, release)
		default:
			w.emit("turn.error", map[string]any{"message": "provider failed"})
		}
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "fine"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prompt(sid, "fails"); err == nil {
		t.Fatal("want the turn error")
	}
	first := h.promptAsync(t, sid, "long job", nil, 1)
	<-started
	second := h.promptAsync(t, sid, "waiting", nil, 2)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	await(t, first, "the running prompt")
	await(t, second, "the waiting prompt")
	h.agent.mu.Lock()
	defer h.agent.mu.Unlock()
	if len(h.agent.inflight) != 0 {
		t.Errorf("inflight = %v after every prompt was answered, want empty", h.agent.inflight)
	}
	if len(h.agent.line) != 0 {
		t.Errorf("line = %v after every prompt was answered, want empty", h.agent.line)
	}
}

// inflight is keyed apart from the sessions for this: a session/cancel still
// reaches a prompt whose session was closed (session/close) while it ran, so
// its turn is stopped instead of left with no way to stop it.
func TestCancelReachesAPromptOfAClosedSession(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: blockingTurn(started)})
	sid := h.newSession(t)
	done := h.promptAsync(t, sid, "long job", nil, 1)
	<-started
	if _, err := h.conn.CloseSession(context.Background(), acpsdk.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if r := await(t, done, "the prompt of the closed session"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Errorf("prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-slow {"scope":"turn","turn_id":"turn-slow"}` {
		t.Errorf("cancels = %q, want the closed session's turn stopped", h.fleet.cancels)
	}
}

// A bare `$/cancel_request` naming a prompt (an unstable ACP notification)
// reaches the agent only as the request context, the same signal the SDK uses
// for a superseding prompt, so fleet acp ignores it, as ACP allows for `$/`
// notifications: the turn is not stopped and runs to its end. session/cancel
// is how a client stops a turn (the Go SDK's client sends one alongside its
// `$/cancel_request` when a prompt's context ends).
func TestCancelRequestDoesNotStopTheTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		if h.fleet.nth() > 1 {
			w.emit("conversation", map[string]any{"id": "conv-c"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		heldTurn(w, r, "conv-c", "turn-c", started, release)
	}})
	sid := h.newSession(t)
	h.raw(`{"jsonrpc":"2.0","id":"p-1","method":"session/prompt","params":{"sessionId":"` + string(sid) + `","prompt":[{"type":"text","text":"long job"}]}}`)
	<-started
	h.raw(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p-1"}}`)
	time.Sleep(stopWindow)
	close(release)
	// Runs once the first turn has finished: prompts on a session are serial.
	if _, err := h.prompt(sid, "next"); err != nil {
		t.Fatal(err)
	}
	if text := h.client.text(); !strings.Contains(text, "working and done") {
		t.Errorf("text = %q, want the turn streamed to its end", text)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("$/cancel_request stopped the turn: cancels = %q", h.fleet.cancels)
	}
	if len(h.fleet.chats) != 2 {
		t.Errorf("chats = %d, want 2", len(h.fleet.chats))
	}
}

// A retry of a prompt whose outcome was lost reuses its idempotency key, so
// fleet recognises the input it may already have accepted; a new prompt, or
// one the client identifies with its own messageId, gets its own key.
func TestRetryReusesTheIdempotencyKey(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
			return
		}
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "book the room"); err == nil {
		t.Fatal("want the lost-stream error")
	}
	if _, err := h.prompt(sid, "book the room"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prompt(sid, "something else"); err != nil {
		t.Fatal(err)
	}
	mid := "3f0c7a52-8a4e-4a57-9d0a-3c1f5b9e2d11"
	resp, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("y")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.UserMessageId == nil || *resp.UserMessageId != mid {
		t.Errorf("userMessageId = %v, want the echoed messageId", resp.UserMessageId)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	k := func(i int) string { return h.fleet.chats[i].InputID }
	if k(0) == "" || k(0) != k(1) {
		t.Errorf("retry key %q != original %q", k(1), k(0))
	}
	if k(2) == k(1) {
		t.Error("a different prompt reused the retried prompt's key")
	}
	if !strings.HasPrefix(k(3), "acp-msg-") || !strings.HasSuffix(k(3), "-"+msgHash(mid)) {
		t.Errorf("messageId key = %q", k(3))
	}
}

// The queued note names the place in line the server counted ("ahead"),
// never the position ordering key, and names none when the server sent no
// count (an older server, or a count that failed).
func TestAcceptedNoteNamesThePlaceInLine(t *testing.T) {
	ahead := func(n int) *int { return &n }
	const where = "the fleet web chat"
	for _, tc := range []struct {
		name string
		q    chattui.QueuedError
		want string
	}{
		{"next", chattui.QueuedError{Position: 40, Ahead: ahead(0), State: "queued"},
			"fleet is already running a turn in this conversation, so your message was queued and will run after it (next in line). Follow it at " + where},
		{"one ahead", chattui.QueuedError{Position: -3, Ahead: ahead(1), State: "queued"},
			"fleet is already running a turn in this conversation, so your message was queued and will run after it and 1 other queued message. Follow it at " + where},
		{"two ahead", chattui.QueuedError{Position: 9, Ahead: ahead(2), State: "queued"},
			"fleet is already running a turn in this conversation, so your message was queued and will run after it and 2 other queued messages. Follow it at " + where},
		{"no count", chattui.QueuedError{Position: 7, State: "queued"},
			"fleet is already running a turn in this conversation, so your message was queued and will run after it. Follow it at " + where},
		{"replay next", chattui.QueuedError{Position: 12, Ahead: ahead(0), State: "queued", Replay: true},
			"this message is already queued from an earlier attempt (next in line). Follow it at " + where},
		{"replay one ahead", chattui.QueuedError{Position: 12, Ahead: ahead(1), State: "queued", Replay: true},
			"this message is already queued from an earlier attempt, with 1 other queued message ahead of it. Follow it at " + where},
		{"replay two ahead", chattui.QueuedError{Position: 12, Ahead: ahead(2), State: "queued", Replay: true},
			"this message is already queued from an earlier attempt, with 2 other queued messages ahead of it. Follow it at " + where},
		{"replay no count", chattui.QueuedError{Position: 12, State: "queued", Replay: true},
			"this message is already queued from an earlier attempt. Follow it at " + where},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptedNote(&tc.q, where); got != tc.want {
				t.Errorf("note = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// A resend answered with an earlier attempt says what became of that attempt
// and where to see it. A direct input's row settles after its turn's stream
// ends (not until boot recovery, if the settlement write keeps failing), so
// "running" may be a turn that has finished, and the note says so rather than
// claiming it is still running.
func TestAcceptedNoteForAReplay(t *testing.T) {
	const where = "the fleet web chat"
	for _, tc := range []struct {
		name string
		q    chattui.QueuedError
		want string
	}{
		{"running", chattui.QueuedError{Mode: "direct", State: "running", Replay: true},
			"fleet already has this message from an earlier attempt; its turn is running or has finished (it is not run twice). See it, and how it ended, at " + where},
		{"injected", chattui.QueuedError{State: "injected", Replay: true},
			"fleet already has this message from an earlier attempt; its turn is running or has finished (it is not run twice). See it, and how it ended, at " + where},
		{"completed", chattui.QueuedError{Mode: "direct", State: "completed", Replay: true},
			"fleet already took this message from an earlier attempt (it is not run twice). How that turn ended — its reply, or an error — is in " + where},
		{"cancelled", chattui.QueuedError{Mode: "direct", State: "cancelled", Replay: true},
			"an earlier attempt of this message was cancelled (stopped, or it failed before it started), so it did not run. To run it, send it again as a new message."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptedNote(&tc.q, where); got != tc.want {
				t.Errorf("note = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// Cancelled while fleet was queueing the prompt (another surface owns the
// running turn): the queued item is withdrawn, so it cannot run after the user
// stopped it.
func TestCancelWithdrawsAQueuedPrompt(t *testing.T) {
	requested := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		close(requested)
		time.Sleep(100 * time.Millisecond) // the ack is slow to arrive
		w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-5","position":1},"conversation_id":"conv-b"}`)
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "do the risky thing")
		done <- r
	}()
	<-requested
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q", r.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	key := h.fleet.chats[0].InputID
	if want := `conv-b {"input_id":"` + key + `","scope":"turn"}`; len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != want {
		t.Errorf("cancels = %q, want the queued input stopped by its key (%s)", h.fleet.cancels, want)
	}
}

// A resend of a message fleet already accepted under the same key is answered
// with that input's state; the original is never run twice.
func TestReplayOfAnAcceptedInput(t *testing.T) {
	ack := func(state string) func(w *sseWriter, _ *http.Request) {
		return func(w *sseWriter, _ *http.Request) {
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","position":1,"mode":"direct","state":"`+state+`"},"conversation_id":"conv-r"}`)
		}
	}
	for state, want := range map[string]string{
		"running":   "already has this message from an earlier attempt; its turn is running or has finished (it is not run twice). See it, and how it ended, at",
		"completed": "already took this message from an earlier attempt (it is not run twice). How that turn ended — its reply, or an error — is in",
	} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t, harnessOpts{turn: ack(state)})
			resp, err := h.prompt(h.newSession(t), "send the report")
			if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
				t.Fatalf("got %+v, %v", resp, err)
			}
			if got := h.client.text(); !strings.Contains(got, want) {
				t.Errorf("text = %q, want %q", got, want)
			}
			h.fleet.mu.Lock()
			defer h.fleet.mu.Unlock()
			if len(h.fleet.chats) != 1 {
				t.Errorf("chats = %d, want 1 (nothing resubmitted)", len(h.fleet.chats))
			}
		})
	}
}

// A cancel whose answer was lost (no turn id, no acknowledgement) stops the
// prompt by its key: fleet withdraws it, cancels its turn or refuses to launch
// it, atomically with registration. A Stop fleet refuses is reported as
// unconfirmed, never as stopped. A confirmed Stop settles the prompt, so the
// same text sent again runs under a fresh key rather than replaying the
// cancelled one; an unconfirmed one keeps the key for a safe retry.
func TestLostAnswerIsReconciledByKey(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelStatus int
		drop         bool // the connection drops mid-cancel: the outcome is unknown
	}{
		{"accepted", 0, false},
		{"refused", http.StatusInternalServerError, false},
		{"connection lost, accepted", 0, true},
		{"connection lost, refused", http.StatusInternalServerError, true},
	} {
		cancelStatus := tc.cancelStatus
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			started := make(chan struct{})
			h := newHarness(t, harnessOpts{cancelStatus: cancelStatus, turn: func(w *sseWriter, r *http.Request) {
				calls++
				if calls != 2 { // the first prompt seeds the session's conversation; the resend completes
					w.emit("conversation", map[string]any{"id": "conv-L"})
					w.emit("turn.completed", map[string]any{})
					return
				}
				close(started) // accepted, but the answer never comes
				if tc.drop {
					time.Sleep(100 * time.Millisecond) // the cancel is in flight
					if conn, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
						_ = conn.Close()
					}
					return
				}
				<-r.Context().Done()
			}})
			sid := h.newSession(t)
			if _, err := h.prompt(sid, "first"); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				_, _ = h.prompt(sid, "second")
				close(done)
			}()
			<-started
			if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("prompt did not return")
			}
			h.fleet.mu.Lock()
			defer h.fleet.mu.Unlock()
			key := h.fleet.chats[1].InputID
			if want := `conv-L {"input_id":"` + key + `","scope":"turn"}`; len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != want {
				t.Errorf("cancels = %q, want %q", h.fleet.cancels, want)
			}
			unconfirmed := strings.Contains(h.client.text(), "could not confirm this turn stopped")
			if unconfirmed != (cancelStatus != 0) {
				t.Errorf("unconfirmed notice = %v with cancel status %d: %q", unconfirmed, cancelStatus, h.client.text())
			}
			h.fleet.mu.Unlock()
			if _, err := h.prompt(sid, "second"); err != nil { // sent again, as the user is told to
				t.Fatal(err)
			}
			h.fleet.mu.Lock()
			if len(h.fleet.chats) != 3 {
				t.Fatalf("chats = %d, want 3", len(h.fleet.chats))
			}
			if reused := h.fleet.chats[2].InputID == key; reused != (cancelStatus != 0) {
				t.Errorf("resend reused the cancelled key = %v with cancel status %d; want a fresh key only after a confirmed Stop", reused, cancelStatus)
			}
		})
	}
}

// Every prompt with an unknown outcome keeps its own key until that prompt is
// answered: an unrelated prompt in between does not make a later retry of the
// first one run twice.
func TestUnresolvedKeysAreKeptPerPrompt(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
			return
		}
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	_, _ = h.prompt(sid, "alpha")
	_, _ = h.prompt(sid, "beta")
	if _, err := h.prompt(sid, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prompt(sid, "beta"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	k := func(i int) string { return h.fleet.chats[i].InputID }
	if k(0) != k(2) || k(1) != k(3) || k(0) == k(1) {
		t.Errorf("keys = %q %q %q %q; want alpha and beta each to keep their own key", k(0), k(1), k(2), k(3))
	}
	for i, c := range h.fleet.chats {
		if c.InputID == "" {
			t.Errorf("chat %d sent no input_id", i)
		}
	}
}

// A client may number messageIds per session. fleet looks a first prompt's key
// up per user, so the same messageId in two sessions must not share a key, or
// the second session's prompt would be answered with the first one's replay.
func TestMessageIdKeysAreScopedPerSession(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-" + randomID()})
		w.emit("turn.completed", map[string]any{})
	}})
	mid := "1"
	for range 2 {
		sid := h.newSession(t)
		if _, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("hi")}}); err != nil {
			t.Fatal(err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 2 || h.fleet.chats[0].InputID == h.fleet.chats[1].InputID {
		t.Fatalf("the same messageId in two sessions shared a key: %+v", h.fleet.chats)
	}
}

// A cancel whose Stop fleet did not accept leaves the original possibly
// running, so a retry of the same text reuses its key (fleet answers it with
// that run) instead of starting it again under a fresh one.
func TestUnconfirmedStopKeepsTheKey(t *testing.T) {
	started := make(chan struct{}, 1)
	calls := 0
	h := newHarness(t, harnessOpts{cancelStatus: http.StatusInternalServerError, turn: func(w *sseWriter, r *http.Request) {
		calls++
		w.emit("conversation", map[string]any{"id": "conv-u"})
		if calls == 1 {
			w.emit("turn.started", map[string]any{"turn_id": "turn-u"})
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	done := make(chan struct{})
	go func() {
		_, _ = h.prompt(sid, "long job")
		close(done)
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	if _, err := h.prompt(sid, "long job"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 2 || h.fleet.chats[0].InputID != h.fleet.chats[1].InputID {
		t.Fatalf("the retry after an unconfirmed stop got a fresh key: %+v", h.fleet.chats)
	}
}

// A first prompt whose whole answer was lost (no header) keeps the
// conversation it was sent to — none — for its retry, even after a later
// prompt created the session's conversation: fleet recognises a first
// submission's key per user only when it names no conversation, so a retry
// posted into the newer conversation would run the prompt again.
func TestUnresolvedFirstPromptRetriesWithoutTheNewerConversation(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			conn, _, err := w.w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close() // the whole answer is lost, headers included
			}
			return
		}
		w.emit("conversation", map[string]any{"id": "conv-B"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "prompt A"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	for _, msg := range []string{"prompt B", "prompt A", "prompt C"} {
		if _, err := h.prompt(sid, msg); err != nil {
			t.Fatal(err)
		}
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	a, b, retry, c := h.fleet.chats[0], h.fleet.chats[1], h.fleet.chats[2], h.fleet.chats[3]
	if retry.InputID != a.InputID || retry.ConversationID != "" {
		t.Errorf("retry of A = key %q conv %q, want key %q and no conversation", retry.InputID, retry.ConversationID, a.InputID)
	}
	if b.ConversationID != "" || c.ConversationID != "conv-B" {
		t.Errorf("B conv %q, C conv %q: the session must stay on the conversation B created", b.ConversationID, c.ConversationID)
	}
}

// A cancelled resend whose answer is a replay of an input still running (or
// queued, or injected) stops that input by its key: the user stopped the
// message they sent, whichever attempt fleet is running it under.
func TestCancelStopsARunningReplayByKey(t *testing.T) {
	requested := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		close(requested)
		time.Sleep(100 * time.Millisecond) // the ack is slow to arrive
		w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","position":1,"mode":"direct","state":"running"},"conversation_id":"conv-r"}`)
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "send it")
		done <- r
	}()
	<-requested
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q", r.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	key := h.fleet.chats[0].InputID
	if want := `conv-r {"input_id":"` + key + `","scope":"turn"}`; len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != want {
		t.Errorf("cancels = %q, want the running replay stopped by its key (%s)", h.fleet.cancels, want)
	}
	if strings.Contains(h.client.text(), "could not confirm") {
		t.Errorf("an accepted Stop was reported unconfirmed: %q", h.client.text())
	}
}

// A retry of an unresolved first prompt is sent with no conversation, so its
// Stop must not name the session's newer conversation (a no-op there that
// would read as confirmed while the original runs on): until fleet names the
// retry's conversation, the stop is reported unconfirmed.
func TestRetryStopNeverNamesTheNewerConversation(t *testing.T) {
	calls := 0
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			if conn, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close() // the whole answer is lost, headers included
			}
		case 2:
			w.emit("conversation", map[string]any{"id": "conv-B"})
			w.emit("turn.completed", map[string]any{})
		default:
			close(started) // the retry is accepted, but no header ever comes
			<-r.Context().Done()
		}
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "prompt A"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	if _, err := h.prompt(sid, "prompt B"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = h.prompt(sid, "prompt A")
		close(done)
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	for _, c := range h.fleet.cancels {
		if strings.HasPrefix(c, "conv-B ") {
			t.Fatalf("the retry's Stop named the newer conversation: %q", h.fleet.cancels)
		}
	}
	if !strings.Contains(h.client.text(), "could not confirm") {
		t.Errorf("the retry's stop was not reported unconfirmed: %q", h.client.text())
	}
}

// A 5xx may follow a committed input (its commit acknowledgement lost), so the
// key is kept and a retry of the same text reuses it; a 4xx is a definite
// refusal, so the next send gets a fresh key.
func TestServerErrorKeepsTheKey(t *testing.T) {
	for status, wantSame := range map[int]bool{http.StatusInternalServerError: true, http.StatusBadRequest: false} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					http.Error(w.w, "boom", status)
					return
				}
				w.emit("conversation", map[string]any{"id": "c"})
				w.emit("turn.completed", map[string]any{})
			}})
			sid := h.newSession(t)
			if _, err := h.prompt(sid, "book the room"); err == nil {
				t.Fatal("want the server error")
			}
			if _, err := h.prompt(sid, "book the room"); err != nil {
				t.Fatal(err)
			}
			h.fleet.mu.Lock()
			defer h.fleet.mu.Unlock()
			if same := h.fleet.chats[0].InputID == h.fleet.chats[1].InputID; same != wantSame {
				t.Errorf("status %d: retry reused the key = %v, want %v", status, same, wantSame)
			}
		})
	}
}

// A cancel that lands before the answer names a conversation, followed by the
// answer being lost, is not a confirmed stop: fleet may have started the turn.
func TestCancelThenLostFirstAnswerIsUnconfirmed(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		close(started)
		<-cancelled
		if conn, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close() // accepted, then the whole answer is lost
		}
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "first job")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the stop watcher is waiting on the answer
	close(cancelled)
	select {
	case r := <-done:
		if r.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("stopReason = %q", r.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	if !strings.Contains(h.client.text(), "could not confirm") {
		t.Errorf("a lost first answer after cancel was reported as a confirmed stop: %q", h.client.text())
	}
}

func msgHash(mid string) string {
	sum := sha256.Sum256([]byte(mid))
	return hex.EncodeToString(sum[:])
}

// A messageId of any length becomes a bounded key (it lands in a btree index
// with a size limit), and the same messageId always maps to the same key.
func TestLongMessageIdIsBoundedAndStable(t *testing.T) {
	long := strings.Repeat("m", 10000)
	sess := &session{ns: "ns"}
	k1, k2 := idempotencyKey(&long, sess, "x", ""), idempotencyKey(&long, sess, "y", "")
	if len(k1) > 128 || k1 != k2 {
		t.Fatalf("key len %d, stable %v", len(k1), k1 == k2)
	}
}

// Every prompt whose answer was lost keeps its key for the session, however
// many there are: a retry of the earliest one still reuses its key (and goes
// to its conversation), so fleet never runs it twice.
func TestUnresolvedPromptsAreNeverEvicted(t *testing.T) {
	sess := &session{convID: "newer"}
	const n = 500
	for i := range n {
		msg, key := fmt.Sprintf("prompt %d", i), fmt.Sprintf("key-%d", i)
		sess.setUnsettled(msg, key)
		sess.settle(key, "", true) // first prompts: no conversation yet
	}
	for i := range n {
		sess.settle(fmt.Sprintf("acp-msg-%d", i), "newer", true)
	}
	if k := idempotencyKey(nil, sess, "prompt 0", ""); k != "key-0" || sess.target(k) != "" {
		t.Fatalf("retry of the first lost prompt = key %q conv %q, want key-0 in its original (none)", k, sess.target(k))
	}
}

// messageIds are opaque: "job-1" and " job-1 " are different messages and
// get different keys; trimming only decides whether an id was sent.
func TestMessageIdsAreOpaque(t *testing.T) {
	sess := &session{ns: "ns"}
	a, b := "job-1", " job-1 "
	if idempotencyKey(&a, sess, "x", "") == idempotencyKey(&b, sess, "x", "") {
		t.Fatal("distinct messageIds shared a key")
	}
	blank := "   "
	if k := idempotencyKey(&blank, sess, "x", ""); strings.HasPrefix(k, "acp-msg-") {
		t.Fatalf("a blank messageId was used as a key: %q", k)
	}
}

// A Stop from another surface that cancels this prompt's input before its turn
// starts is answered 409 by POST /chat; nothing ran and the Stop succeeded, so
// the prompt ends cancelled, not with an internal error.
func TestPreLaunchStopFromAnotherSurfaceIsCancelled(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		http.Error(w.w, "a Stop in this conversation cancelled this message before it started", http.StatusConflict)
	}})
	sid := h.newSession(t)
	r, err := h.prompt(sid, "do it")
	if err != nil || r.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("got %+v, %v; want stopReason cancelled", r, err)
	}
}

// A prompt with a messageId that shares its text with an earlier, unresolved
// text-only prompt must not wipe that prompt's retained key: the earlier
// prompt's retry still reuses its own key, so fleet does not run it twice.
func TestSameTextUnderAMessageIdKeepsTheOtherPromptsKey(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
			return
		}
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	_, _ = h.prompt(sid, "same text")
	mid := "msg-B"
	if _, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("same text")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prompt(sid, "same text"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if a, retry := h.fleet.chats[0].InputID, h.fleet.chats[2].InputID; a != retry {
		t.Fatalf("retry key = %q, want the lost prompt's own key %q", retry, a)
	}
}

// Whitespace inside the prompt is the user's (an indented code block, a
// trailing newline a tool relies on): it is sent exactly, not trimmed.
func TestPromptWhitespaceIsPreserved(t *testing.T) {
	in := "  indented\n\tcode\n"
	got, err := promptText([]acpsdk.ContentBlock{acpsdk.TextBlock(in)})
	if err != nil || got != in {
		t.Fatalf("promptText = %q, %v; want %q unchanged", got, err, in)
	}
}

// A Stop that lands just after the watched turn ended stops nothing (fleet
// answers 409): the prompt keeps reading the turn to its end and, still
// answering "cancelled" as ACP requires, says the turn had finished and what
// it did stands — not a silent "cancelled" over tools that ran to completion.
//
// The terminal frame is enough: the stream may stay open well past it while
// fleet finishes post-turn work (auto-titling), and that wait must not turn
// the definite "already finished" into "could not confirm".
func TestStopOfAnEndedTurnReportsItsOutcome(t *testing.T) {
	for _, lingers := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream ends", true: "stream lingers after the frame"}[lingers], func(t *testing.T) {
			testStopOfAnEndedTurnReportsItsOutcome(t, lingers)
		})
	}
}

func testStopOfAnEndedTurnReportsItsOutcome(t *testing.T, lingers bool) {
	started := make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{cancelStatus: http.StatusConflict, turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-e"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-e"})
		close(started)
		for { // the turn finishes as the Stop arrives
			h.fleet.mu.Lock()
			n := len(h.fleet.cancels)
			h.fleet.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		w.emit("text.delta", map[string]any{"text": "all done"})
		w.emit("turn.completed", map[string]any{})
		if lingers {
			select { // post-turn work holds the stream open past conversationWait
			case <-r.Context().Done():
			case <-time.After(2 * conversationWait):
			}
		}
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "long job")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		text := h.client.text()
		if r.StopReason != acpsdk.StopReasonCancelled || !strings.Contains(text, "all done") || !strings.Contains(text, "already finished") || strings.Contains(text, "could not confirm") {
			t.Fatalf("got %+v, text %q; want the full answer and a note that the turn had already finished", r, text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
}

// A Stop by key (no turn id was known) that finds the input already finished
// stops nothing (fleet answers 409): the prompt says the turn had finished
// and its effects stand, neither a confirmed stop nor an unconfirmed one.
func TestKeyedStopOfAFinishedInputSaysSo(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{cancelStatus: http.StatusConflict, turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-f"})
		close(started)
		<-r.Context().Done() // no turn id is ever named
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "job")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		text := h.client.text()
		if r.StopReason != acpsdk.StopReasonCancelled || !strings.Contains(text, "already finished") || strings.Contains(text, "could not confirm") {
			t.Fatalf("got %+v, text %q; want cancelled with the already-finished note", r, text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 1 || !strings.Contains(h.fleet.cancels[0], `"input_id"`) {
		t.Fatalf("cancels = %q, want one Stop by key", h.fleet.cancels)
	}
}

// A replay that reports the input still running in another conversation
// (one the session is not in) keeps the key routed there: a later resend, or
// a Stop by key, goes to the conversation that holds the input, not to the
// session's newer one where fleet would not find it.
func TestLiveReplayKeepsItsConversation(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			if conn, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close() // A accepted in conv-A, its answer lost
			}
		case 2:
			w.emit("conversation", map[string]any{"id": "conv-B"})
			w.emit("turn.completed", map[string]any{})
		default: // A's resends: still running in conv-A
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-a","mode":"direct","state":"running"},"conversation_id":"conv-A"}`)
		}
	}})
	sid := h.newSession(t)
	mid := "msg-A"
	send := func(text string, id *string) {
		_, _ = h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
	}
	send("prompt A", &mid)
	send("prompt B", nil)
	send("prompt A", &mid) // answered: running in conv-A
	send("prompt A", &mid) // a later resend
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 4 {
		t.Fatalf("chats = %d, want 4", len(h.fleet.chats))
	}
	if got := h.fleet.chats[3].ConversationID; got != "conv-A" {
		t.Fatalf("later resend went to %q, want conv-A where the input runs", got)
	}
}

// A retry of an unresolved key that fleet refuses before looking the key up
// (429 from the rate limiter) keeps the key: the refusal is definite for that
// attempt only, and the earlier one may have run, so the next retry must
// still reuse the key rather than mint a fresh one.
func TestRateLimitedRetryKeepsTheUnresolvedKey(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
		case 2:
			http.Error(w.w, "rate limited", http.StatusTooManyRequests)
		default:
			w.emit("conversation", map[string]any{"id": "c"})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	sid := h.newSession(t)
	_, _ = h.prompt(sid, "book the room")
	_, _ = h.prompt(sid, "book the room")
	if _, err := h.prompt(sid, "book the room"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if k0, k2 := h.fleet.chats[0].InputID, h.fleet.chats[2].InputID; k0 != k2 {
		t.Fatalf("keys %q then %q: the retry after a 429 must reuse the unresolved key", k0, k2)
	}
}

// A replay that reports the key's input cancelled is not resubmitted: fleet
// does not record why it was cancelled, and it may have been a Stop (from any
// surface), so running it under a fresh key could run a stopped message. The
// user is told to send it again as a new message.
func TestCancelledReplayIsNotResubmitted(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","mode":"direct","state":"cancelled"},"conversation_id":"conv-c"}`)
	}})
	sid := h.newSession(t)
	mid := "msg-c"
	r, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("send it")}})
	if err != nil || r.StopReason != acpsdk.StopReasonEndTurn || !strings.Contains(h.client.text(), "send it again as a new message") {
		t.Fatalf("got %+v, %v, text %q; want the not-run note", r, err, h.client.text())
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 1 {
		t.Fatalf("chats = %d: a cancelled input must not be resubmitted", len(h.fleet.chats))
	}
}

// A cancel that races a replay reporting the input already completed stops
// nothing: the prompt says the turn had finished and what it did stands.
func TestCancelDuringACompletedReplayIsAlreadyEnded(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		close(started)
		<-release
		w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","mode":"direct","state":"completed"},"conversation_id":"conv-d"}`)
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "job")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the stop watcher is waiting on the answer
	close(release)
	select {
	case r := <-done:
		text := h.client.text()
		if r.StopReason != acpsdk.StopReasonCancelled || !strings.Contains(text, "already finished") || strings.Contains(text, "could not confirm") {
			t.Fatalf("got %+v, text %q; want cancelled with the already-finished note", r, text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
}

// A Stop fleet accepts can still stop nothing: the turn may complete in the
// instant between fleet's running check and its cancel. The prompt reads on
// to the terminal frame and trusts it — turn.completed means the turn ran to
// its end, so it says so instead of reporting a confirmed stop.
func TestAcceptedStopOfATurnThatCompletedAnywaySaysSo(t *testing.T) {
	started := make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-r"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-r"})
		close(started)
		for { // the Stop is accepted (204), but the turn completes regardless
			h.fleet.mu.Lock()
			n := len(h.fleet.cancels)
			h.fleet.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		w.emit("text.delta", map[string]any{"text": "all done"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	done := make(chan acpsdk.PromptResponse, 1)
	go func() {
		r, _ := h.prompt(sid, "long job")
		done <- r
	}()
	<-started
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		text := h.client.text()
		if r.StopReason != acpsdk.StopReasonCancelled || !strings.Contains(text, "already finished") {
			t.Fatalf("got %+v, text %q; want the already-finished note, not a confirmed stop", r, text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return")
	}
}

// A prompt with a messageId whose outcome is unknown is not filed under its
// text: a later, different text-only prompt with the same words gets its own
// key and runs, rather than being answered with the messageId prompt's replay.
func TestMessageIdKeyIsNotReusedForTheSameText(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
			return
		}
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	mid := "msg-1"
	_, _ = h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("same words")}})
	if _, err := h.prompt(sid, "same words"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if k0, k1 := h.fleet.chats[0].InputID, h.fleet.chats[1].InputID; k0 == k1 {
		t.Fatalf("the text-only prompt reused the messageId prompt's key %q", k0)
	}
}

// A timeout whose Stop fleet accepts just as the turn completes is not a
// timeout: the turn finished, so the prompt ends end_turn with its answer
// (and a text-only prompt's key is not left looking like a failure to retry).
func TestTimeoutThatLosesTheRaceReportsTheCompletedTurn(t *testing.T) {
	var h *harness
	h = newHarness(t, harnessOpts{timeout: 50 * time.Millisecond, turn: func(w *sseWriter, _ *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-t"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-t"})
		for { // the timeout's Stop is accepted, but the turn completes anyway
			h.fleet.mu.Lock()
			n := len(h.fleet.cancels)
			h.fleet.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		w.emit("text.delta", map[string]any{"text": "all done"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	r, err := h.prompt(sid, "long job")
	if err != nil || r.StopReason != acpsdk.StopReasonEndTurn || !strings.Contains(h.client.text(), "all done") {
		t.Fatalf("got %+v, %v, text %q; want the completed turn's end_turn", r, err, h.client.text())
	}
}

// A Stop fleet sent but could not yet confirm (202) is settled by the turn's
// own stream: a turn.cancelled that follows confirms it as a clean stop; no
// terminal frame at all leaves it unconfirmed, and the prompt says the turn
// may still be running rather than claiming it stopped. A turn that instead
// fails on its own (turn.error, turn.model_required) was not stopped: the
// prompt says it had already finished, neither a clean stop nor unconfirmed.
func TestUnconfirmedStopIsSettledByTheStream(t *testing.T) {
	for _, frame := range []string{"turn.error", "turn.model_required"} {
		t.Run("ends on its own: "+frame, func(t *testing.T) {
			started := make(chan struct{})
			var h *harness
			h = newHarness(t, harnessOpts{cancelStatus: http.StatusAccepted, turn: func(w *sseWriter, _ *http.Request) {
				w.emit("conversation", map[string]any{"id": "conv-u"})
				w.emit("turn.started", map[string]any{"turn_id": "turn-u"})
				close(started)
				for {
					h.fleet.mu.Lock()
					n := len(h.fleet.cancels)
					h.fleet.mu.Unlock()
					if n > 0 {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				w.emit(frame, map[string]any{"message": "the provider failed"})
			}})
			sid := h.newSession(t)
			done := make(chan acpsdk.PromptResponse, 1)
			go func() {
				r, _ := h.prompt(sid, "long job")
				done <- r
			}()
			<-started
			if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				text := h.client.text()
				if r.StopReason != acpsdk.StopReasonCancelled || !strings.Contains(text, "already finished") || strings.Contains(text, "could not confirm") {
					t.Fatalf("got %+v, text %q; want the already-finished note", r, text)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("prompt did not return")
			}
		})
	}
	for _, confirms := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream confirms", false: "never confirmed"}[confirms], func(t *testing.T) {
			started := make(chan struct{})
			var h *harness
			h = newHarness(t, harnessOpts{cancelStatus: http.StatusAccepted, turn: func(w *sseWriter, r *http.Request) {
				w.emit("conversation", map[string]any{"id": "conv-u"})
				w.emit("turn.started", map[string]any{"turn_id": "turn-u"})
				close(started)
				for {
					h.fleet.mu.Lock()
					n := len(h.fleet.cancels)
					h.fleet.mu.Unlock()
					if n > 0 {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				if confirms {
					w.emit("turn.cancelled", map[string]any{})
					return
				}
				<-r.Context().Done() // no terminal frame
			}})
			sid := h.newSession(t)
			done := make(chan acpsdk.PromptResponse, 1)
			go func() {
				r, _ := h.prompt(sid, "long job")
				done <- r
			}()
			<-started
			if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				unconfirmed := strings.Contains(h.client.text(), "could not confirm")
				if r.StopReason != acpsdk.StopReasonCancelled || unconfirmed == confirms {
					t.Fatalf("got %+v, text %q; want unconfirmed=%v", r, h.client.text(), !confirms)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("prompt did not return")
			}
		})
	}
}
