package acp

import (
	"context"
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
	if len(h.fleet.cancels) != 1 || h.fleet.cancels[0] != `conv-slow {"scope":"turn"}` {
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
