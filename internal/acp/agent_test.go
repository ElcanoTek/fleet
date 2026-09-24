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
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

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
}

type harnessOpts struct {
	turn         func(w *sseWriter, r *http.Request)
	cancelStatus int
	cfgErr       error
	publicURL    string
	timeout      time.Duration
	serverURL    string // override (e.g. a closed port)
}

// newHarness wires a real SDK client to the real Agent over in-memory pipes,
// the way an ACP client wires to `fleet acp`'s stdio.
func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	ff := &fakeFleet{t: t, turn: o.turn, cancelStatus: o.cancelStatus}
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
	return &harness{fleet: ff, client: rc, conn: conn}
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
			w.w.WriteHeader(http.StatusForbidden)
		}})
		_, err := h.prompt(h.newSession(t), "x")
		if rpcCode(err) != -32000 || !strings.Contains(err.Error(), "FLEET_SERVER_TOKEN") {
			t.Fatalf("err = %v, want auth_required naming the token", err)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Fatal("the token value leaked into an error")
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
	for _, want := range []string{"Review this", "[main.go](file:///repo/main.go)", "Contents of file:///repo/go.mod:\n```\nmodule x\n```"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt %q missing %q", got, want)
		}
	}
	if _, err := promptText([]acpsdk.ContentBlock{acpsdk.TextBlock("  ")}); err == nil {
		t.Error("an empty prompt must be refused")
	}
}

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
		_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"in-7","position":2,"state":"queued"},"conversation_id":"conv-q"}`)
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	resp, err := h.prompt(sid, "second, while the web chat is busy")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("got %+v, %v; want an accepted end_turn", resp, err)
	}
	if got := h.client.text(); !strings.Contains(got, "queued (position 2)") {
		t.Errorf("no queued notice: %q", got)
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

// A prompt already cancelled when it gets the session is never submitted.
func TestCancelledPromptIsNeverSubmitted(t *testing.T) {
	ff := &fakeFleet{t: t}
	srv := httptest.NewServer(ff)
	defer srv.Close()
	ag := NewAgent(chattui.NewClient(chattui.Config{ServerURL: srv.URL, Email: "bot@example.com", Token: "test-token"}), nil, "", 0, "test")
	s, err := ag.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := ag.Prompt(ctx, acpsdk.PromptRequest{SessionId: s.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("x")}})
	if err != nil || resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("got %+v, %v; want cancelled", resp, err)
	}
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.chats) != 0 {
		t.Errorf("a cancelled prompt was POSTed: %+v", ff.chats)
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
		"running":   "already running this message from an earlier attempt",
		"completed": "already ran this message from an earlier attempt",
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

// "Accepted earlier but did not run" means the key is spent and nothing ran,
// so the prompt is resubmitted under the messageId's next key (key-r1). A
// client that resends the same messageId afterwards starts from its key again
// and walks the same chain, so nothing needs remembering (the chain itself is
// covered end to end by TestResentMessageIdFindsItsRetryRun).
func TestNeverRunReplayIsResubmittedOnce(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-1","mode":"direct","state":"cancelled"},"conversation_id":"conv-c"}`)
			return
		}
		w.emit("conversation", map[string]any{"id": "conv-c"})
		w.emit("text.delta", map[string]any{"text": "done"})
		w.emit("turn.completed", map[string]any{})
	}})
	sid := h.newSession(t)
	mid := "7b1c2d3e-0000-4000-8000-000000000001"
	resp, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("send it")}})
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn || h.client.text() != "done" {
		t.Fatalf("got %+v, %v, text %q", resp, err, h.client.text())
	}
	if _, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("send it")}}); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 3 {
		t.Fatalf("chats = %d, want 3", len(h.fleet.chats))
	}
	k0, k1, k2 := h.fleet.chats[0].InputID, h.fleet.chats[1].InputID, h.fleet.chats[2].InputID
	if !strings.HasPrefix(k0, "acp-msg-") || !strings.HasSuffix(k0, "-"+msgHash(mid)) || k1 != k0+"-r1" || k2 != k0 {
		t.Errorf("keys = %q, %q, %q; want the messageId key, its -r1 retry, then the resend from the messageId key", k0, k1, k2)
	}
}

// A cancel whose answer was lost (no turn id, no acknowledgement) stops the
// prompt by its key: fleet withdraws it, cancels its turn or refuses to launch
// it, atomically with registration. A Stop fleet refuses is reported as
// unconfirmed, never as stopped.
func TestLostAnswerIsReconciledByKey(t *testing.T) {
	for name, cancelStatus := range map[string]int{"accepted": 0, "refused": http.StatusInternalServerError} {
		t.Run(name, func(t *testing.T) {
			seeded := false
			started := make(chan struct{})
			h := newHarness(t, harnessOpts{cancelStatus: cancelStatus, turn: func(w *sseWriter, r *http.Request) {
				if !seeded { // the first prompt seeds the session's conversation
					seeded = true
					w.emit("conversation", map[string]any{"id": "conv-L"})
					w.emit("turn.completed", map[string]any{})
					return
				}
				close(started) // accepted, but the answer never comes
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
	k1, k2 := idempotencyKey(&long, sess, "x"), idempotencyKey(&long, sess, "y")
	if len(k1) > 128 || k1 != k2 {
		t.Fatalf("key len %d, stable %v", len(k1), k1 == k2)
	}
}

// Past the bound, an unresolved prompt is forgotten as one record: its retry
// key and its target conversation go together, so no retained key is left
// without its conversation (a retry into a newer one would run it again).
func TestUnresolvedPromptsAreEvictedWhole(t *testing.T) {
	sess := &session{convID: "newer"}
	for i := range maxUnsettled {
		msg, key := fmt.Sprintf("prompt %d", i), fmt.Sprintf("key-%d", i)
		sess.setUnsettled(msg, key)
		sess.settle(key, "", true) // first prompts: no conversation yet
	}
	// messageId prompts are unresolved too, but keyed by messageId rather
	// than by text: they fill the conversation map and force evictions there.
	for i := range 20 {
		sess.settle(fmt.Sprintf("acp-msg-%d", i), "newer", true)
	}
	if len(sess.unsettled) > maxUnsettled || len(sess.keyConv) > maxUnsettled {
		t.Fatalf("bounds exceeded: %d unsettled, %d keyConv", len(sess.unsettled), len(sess.keyConv))
	}
	for msg, key := range sess.unsettled {
		if c, ok := sess.keyConv[key]; !ok || c != "" {
			t.Fatalf("%q kept key %q but lost its target conversation (got %q, %v)", msg, key, c, ok)
		}
	}
}

// messageIds are opaque: "job-1" and " job-1 " are different messages and
// get different keys; trimming only decides whether an id was sent.
func TestMessageIdsAreOpaque(t *testing.T) {
	sess := &session{ns: "ns"}
	a, b := "job-1", " job-1 "
	if idempotencyKey(&a, sess, "x") == idempotencyKey(&b, sess, "x") {
		t.Fatal("distinct messageIds shared a key")
	}
	blank := "   "
	if k := idempotencyKey(&blank, sess, "x"); strings.HasPrefix(k, "acp-msg-") {
		t.Fatalf("a blank messageId was used as a key: %q", k)
	}
}

// A first prompt whose replay reports "accepted but never ran" is resubmitted
// under a fresh key in the conversation that accepted it, not in a
// conversation the session started since.
func TestFreshRetryStaysInTheOriginalConversation(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			if conn, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close() // A's whole answer is lost
			}
		case 2:
			w.emit("conversation", map[string]any{"id": "conv-B"})
			w.emit("turn.completed", map[string]any{})
		case 3: // A's retry: fleet had accepted it in conv-A, but it never ran
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"row-a","mode":"direct","state":"cancelled"},"conversation_id":"conv-A"}`)
		default:
			w.emit("conversation", map[string]any{"id": "conv-A"})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "prompt A"); err == nil {
		t.Fatal("want the lost-answer error")
	}
	if _, err := h.prompt(sid, "prompt B"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prompt(sid, "prompt A"); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 4 {
		t.Fatalf("chats = %d, want 4", len(h.fleet.chats))
	}
	if fresh := h.fleet.chats[3]; fresh.ConversationID != "conv-A" || fresh.InputID == h.fleet.chats[0].InputID {
		t.Fatalf("fresh retry = conv %q key %q, want conv-A under a new key", fresh.ConversationID, fresh.InputID)
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

// keyedFleet is a fake /chat that remembers every input_id it accepted, the
// way fleet's queue does (per user, as input_id_scope "user" declares): a
// resend of a known key is answered with that input's replay, whatever
// conversation it is posted to. firstRun decides a new key's fate: it
// returns the state the input ends in ("completed" streams a turn;
// "cancelled" records an accepted input that never ran, and answers with its
// replay; "lost" records it as completed but drops the answer).
type keyedFleet struct {
	mu       sync.Mutex
	state    map[string]string // input_id -> completed | cancelled
	conv     map[string]string // input_id -> conversation that accepted it
	runs     int               // turns actually streamed
	firstRun func(key, conv string) string
}

func newKeyedHarness(t *testing.T, k *keyedFleet) *harness {
	k.state, k.conv = map[string]string{}, map[string]string{}
	var h *harness
	h = newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		h.fleet.mu.Lock()
		req := h.fleet.chats[len(h.fleet.chats)-1]
		h.fleet.mu.Unlock()
		k.mu.Lock()
		defer k.mu.Unlock()
		replay := func(key string) {
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w.w, `{"queued":true,"input":{"id":"row-%s","mode":"direct","state":%q},"conversation_id":%q}`, key, k.state[key], k.conv[key])
		}
		if _, seen := k.state[req.InputID]; seen {
			replay(req.InputID)
			return
		}
		conv := req.ConversationID
		if conv == "" {
			conv = "conv-" + req.InputID[len(req.InputID)-4:]
		}
		k.conv[req.InputID] = conv
		switch k.firstRun(req.InputID, conv) {
		case "cancelled":
			k.state[req.InputID] = "cancelled"
			replay(req.InputID)
		case "lost":
			k.state[req.InputID] = "completed"
			k.runs++
			if c, _, err := w.w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
		default:
			k.state[req.InputID] = "completed"
			k.runs++
			w.emit("conversation", map[string]any{"id": conv})
			w.emit("turn.completed", map[string]any{})
		}
	}})
	return h
}

// A messageId whose first attempt never ran is retried under a key derived
// from it, so a later resend of the messageId — however many other
// messageIds were retried in between — walks to the attempt that ran and is
// answered "already ran" instead of running a second time.
func TestResentMessageIdFindsItsRetryRun(t *testing.T) {
	k := &keyedFleet{firstRun: func(key, _ string) string {
		if strings.HasPrefix(key, "acp-msg-") && !strings.Contains(key, "-r") {
			return "cancelled" // every messageId's first attempt: accepted, never ran
		}
		return "completed"
	}}
	h := newKeyedHarness(t, k)
	sid := h.newSession(t)
	send := func(mid string) error {
		_, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("do " + mid)}})
		return err
	}
	for i := range maxUnsettled + 10 {
		if err := send(fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	k.mu.Lock()
	before := k.runs
	k.mu.Unlock()
	if before != maxUnsettled+10 {
		t.Fatalf("runs = %d, want one per messageId", before)
	}
	if err := send("msg-0"); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.runs != before {
		t.Fatalf("resending the oldest messageId ran it again (%d runs, want %d)", k.runs, before)
	}
}

// A Stop that lands just after the watched turn ended stops nothing (fleet
// answers 409): the prompt keeps reading the turn to its end and, still
// answering "cancelled" as ACP requires, says the turn had finished and what
// it did stands — not a silent "cancelled" over tools that ran to completion.
func TestStopOfAnEndedTurnReportsItsOutcome(t *testing.T) {
	started := make(chan struct{})
	var h *harness
	h = newHarness(t, harnessOpts{cancelStatus: http.StatusConflict, turn: func(w *sseWriter, _ *http.Request) {
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

// A messageId whose whole chain of retry keys was accepted and never ran
// fails asking for a new message: a resend of the same messageId would walk
// the same chain, so "send it again" would never run it.
func TestExhaustedRetryChainAsksForANewMessage(t *testing.T) {
	k := &keyedFleet{firstRun: func(string, string) string { return "cancelled" }}
	h := newKeyedHarness(t, k)
	sid := h.newSession(t)
	mid := "msg-x"
	_, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, MessageId: &mid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("do it")}})
	if err == nil || !strings.Contains(err.Error(), "new message") {
		t.Fatalf("err = %v, want a request for a new message", err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != maxRetryKeys+1 {
		t.Fatalf("chats = %d, want the key and its %d retries", len(h.fleet.chats), maxRetryKeys)
	}
}
