// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
)

// fakeA2APeer answers the four methods the peer tools drive, recording the
// last request's headers, and lets a test script the task state and errors.
type fakeA2APeer struct {
	mu        sync.Mutex
	headers   http.Header
	method    string
	state     wire.TaskState
	artifacts []*wire.Artifact
	rpcError  *a2abridge.ErrorObject
	httpCode  int
	streamFor time.Duration // >0: SubscribeToTask streams a snapshot then keepalives for this long
}

func (f *fakeA2APeer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2abridge.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.headers = r.Header.Clone()
		f.method = req.Method
		state, artifacts, rpcErr, code, streamFor := f.state, f.artifacts, f.rpcError, f.httpCode, f.streamFor
		f.mu.Unlock()
		if code != 0 {
			http.Error(w, "Unauthorized: bad key", code)
			return
		}
		write := func(resp a2abridge.Response) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}
		if rpcErr != nil {
			write(a2abridge.Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
			return
		}
		task := &wire.Task{ID: "remote-42", ContextID: "remote-42", Status: wire.TaskStatus{State: state}, Artifacts: artifacts}
		switch req.Method {
		case a2abridge.MethodSendMessage:
			task.Status.State = wire.TaskStateSubmitted
			write(a2abridge.NewResponse(req.ID, wire.StreamResponse{Event: task}))
		case a2abridge.MethodGetTask:
			write(a2abridge.NewResponse(req.ID, task))
		case a2abridge.MethodCancelTask:
			task.Status.State = wire.TaskStateCanceled
			write(a2abridge.NewResponse(req.ID, task))
		case a2abridge.MethodSubscribeToTask:
			if state.Terminal() {
				write(a2abridge.NewErrorResponse(req.ID, wire.ErrUnsupportedOperation, "already terminal", nil))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			data, _ := json.Marshal(a2abridge.NewResponse(req.ID, wire.StreamResponse{Event: task}))
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
			deadline := time.Now().Add(streamFor)
			for time.Now().Before(deadline) {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
					fmt.Fprint(w, ": keepalive\n\n")
					fl.Flush()
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeA2APeer) lastHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers
}

func (f *fakeA2APeer) lastMethod() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.method
}

func testPeerSpec(srv *httptest.Server, secret string) A2APeerSpec {
	return A2APeerSpec{
		Name:         "helpdesk",
		Description:  "The helpdesk agent: password resets and access requests.",
		RPCURL:       srv.URL + "/v1/a2a",
		Headers:      map[string]string{"X-API-Key": secret},
		MaxDepth:     3,
		DefaultDepth: 0,
		AllowPrivate: true,
	}
}

func callA2A(ctx context.Context, t *testing.T, c *Client, tool string, args map[string]interface{}) *ToolResult {
	t.Helper()
	res, err := c.CallToolOn(ctx, A2AToolServerName, tool, args)
	if err != nil {
		t.Fatalf("CallToolOn %s: %v", tool, err)
	}
	return res
}

func TestA2APeerToolsRoster(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateWorking}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, "k")})

	tools := c.GetAllTools()
	if len(tools) != 4 {
		t.Fatalf("GetAllTools len = %d, want 4 (send/status/wait/cancel)", len(tools))
	}
	want := map[string]bool{"helpdesk_send": true, "helpdesk_status": true, "helpdesk_wait": true, "helpdesk_cancel": true}
	for _, st := range tools {
		if st.ServerName != A2AToolServerName {
			t.Errorf("server name = %q, want %q", st.ServerName, A2AToolServerName)
		}
		if !want[st.Tool.Name] {
			t.Errorf("unexpected tool %q", st.Tool.Name)
		}
		delete(want, st.Tool.Name)
		if typ, _ := st.Tool.InputSchema["type"].(string); typ != "object" {
			t.Errorf("%s schema type = %q", st.Tool.Name, typ)
		}
		if _, ok := st.Tool.InputSchema["required"]; !ok {
			t.Errorf("%s schema has no required list", st.Tool.Name)
		}
		if !strings.Contains(st.Tool.Description, "helpdesk") {
			t.Errorf("%s description does not name the peer: %q", st.Tool.Name, st.Tool.Description)
		}
		if st.Tool.Name == "helpdesk_send" && !strings.Contains(st.Tool.Description, "password resets") {
			t.Errorf("send description must carry the bundle-authored text: %q", st.Tool.Description)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
	if got := A2AToolNames("x"); strings.Join(got, ",") != "x_send,x_status,x_wait,x_cancel" {
		t.Errorf("A2AToolNames = %v", got)
	}
}

func TestA2APeerSecretsNotExposedToModel(t *testing.T) {
	const secret = "fleet_task_super_secret_value"
	peer := &fakeA2APeer{state: wire.TaskStateWorking}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, secret)})

	for _, st := range c.GetAllTools() {
		blob, _ := json.Marshal(st.Tool)
		if strings.Contains(string(blob), secret) {
			t.Fatalf("secret leaked into the catalog entry %s", st.Tool.Name)
		}
	}
	res := callA2A(context.Background(), t, c, "helpdesk_send", map[string]interface{}{"message": "reset my password"})
	if res.IsError {
		t.Fatalf("send failed: %s", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, secret) {
		t.Fatal("secret leaked into the tool result")
	}
	if got := peer.lastHeaders().Get("X-API-Key"); got != secret {
		t.Fatalf("credential not on the wire: %q", got)
	}
	// The result opens with the untrusted-content banner and carries the ids.
	text := res.Content[0].Text
	if !strings.HasPrefix(text, a2aUntrustedBanner) {
		t.Errorf("result must open with the untrusted banner:\n%s", text)
	}
	for _, want := range []string{"peer: helpdesk", "task_id: remote-42", "state: TASK_STATE_SUBMITTED", "helpdesk_status", "helpdesk_wait"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
}

func TestA2APeerReRegisterUpdatesInPlace(t *testing.T) {
	peer := &fakeA2APeer{}
	srv := peer.serve(t)
	c := NewClient()
	spec := testPeerSpec(srv, "k")
	c.AddA2APeers([]A2APeerSpec{spec})
	spec.Description = "Now the billing agent."
	c.AddA2APeers([]A2APeerSpec{spec})
	tools := c.GetAllTools()
	if len(tools) != 4 {
		t.Fatalf("re-register duplicated catalog entries: %d tools", len(tools))
	}
	for _, st := range tools {
		if st.Tool.Name == "helpdesk_send" && !strings.Contains(st.Tool.Description, "billing") {
			t.Errorf("re-register did not update the description: %q", st.Tool.Description)
		}
	}
}

func TestA2APeerDepthGuard(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateWorking}
	srv := peer.serve(t)
	c := NewClient()
	spec := testPeerSpec(srv, "k")
	spec.MaxDepth = 2
	spec.DefaultDepth = 2 // this run is already at the ceiling
	c.AddA2APeers([]A2APeerSpec{spec})

	// No ctx depth: the spec default applies and the send is refused locally,
	// before any network call.
	res := callA2A(context.Background(), t, c, "helpdesk_send", map[string]interface{}{"message": "go deeper"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "A2A_DELEGATION_DEPTH_EXCEEDED") {
		t.Fatalf("want depth refusal, got %+v", res)
	}
	if peer.lastMethod() != "" {
		t.Fatal("a refused send must not reach the peer")
	}
	// The call context overrides the default: depth 0 → allowed, wire carries 1.
	res = callA2A(WithA2ADepth(context.Background(), 0), t, c, "helpdesk_send", map[string]interface{}{"message": "ok"})
	if res.IsError {
		t.Fatalf("depth-0 send refused: %s", res.Content[0].Text)
	}
	if got := peer.lastHeaders().Get(a2abridge.DepthHeader); got != "1" {
		t.Errorf("%s = %q, want 1", a2abridge.DepthHeader, got)
	}
	// A follow-up answer extends no chain: allowed even at the ceiling.
	res = callA2A(context.Background(), t, c, "helpdesk_send", map[string]interface{}{"message": "the second one", "task_id": "remote-42"})
	if res.IsError {
		t.Fatalf("follow-up answer must bypass the depth guard: %s", res.Content[0].Text)
	}
}

func TestA2APeerRemoteErrorsAreToolErrors(t *testing.T) {
	peer := &fakeA2APeer{rpcError: &a2abridge.ErrorObject{Code: -32001, Message: "no such task"}}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, "k")})
	res := callA2A(context.Background(), t, c, "helpdesk_status", map[string]interface{}{"task_id": "zzz"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "remote A2A error -32001") || !strings.Contains(res.Content[0].Text, "no such task") {
		t.Fatalf("want rendered remote error, got %+v", res)
	}

	peer401 := &fakeA2APeer{httpCode: http.StatusUnauthorized}
	srv401 := peer401.serve(t)
	c2 := NewClient()
	c2.AddA2APeers([]A2APeerSpec{testPeerSpec(srv401, "k")})
	res = callA2A(context.Background(), t, c2, "helpdesk_status", map[string]interface{}{"task_id": "x"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "HTTP 401") || !strings.Contains(res.Content[0].Text, "bundle manifest") {
		t.Fatalf("want 401 hint, got %+v", res)
	}

	// Missing required args are refused without a network call.
	res = callA2A(context.Background(), t, c, "helpdesk_status", map[string]interface{}{})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "task_id is required") {
		t.Fatalf("want arg refusal, got %+v", res)
	}
}

func TestA2APeerWaitBoundedAndRendered(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateWorking, streamFor: 10 * time.Second}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, "k")})
	start := time.Now()
	res := callA2A(context.Background(), t, c, "helpdesk_wait", map[string]interface{}{"task_id": "remote-42", "wait_seconds": float64(1)})
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("wait overran wait_seconds: %s", elapsed)
	}
	if res.IsError {
		t.Fatalf("wait errored: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "No change after 1 s") || !strings.Contains(res.Content[0].Text, "state: TASK_STATE_WORKING") {
		t.Fatalf("timed-out wait not rendered as such:\n%s", res.Content[0].Text)
	}

	// A terminal task: the subscription is refused (-32004) and the wait
	// falls back to the snapshot, rendered as finished.
	peer.mu.Lock()
	peer.state = wire.TaskStateCompleted
	peer.mu.Unlock()
	res = callA2A(context.Background(), t, c, "helpdesk_wait", map[string]interface{}{"task_id": "remote-42"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "has finished") {
		t.Fatalf("terminal wait: %+v", res)
	}
}

func TestA2APeerRendersArtifactsWithoutDownloading(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateCompleted, artifacts: []*wire.Artifact{{
		ID: "art-1", Name: "report", Description: "the findings",
		Parts: []*wire.Part{
			wire.NewTextPart("all systems nominal"),
			{Content: wire.URL("https://peer.example/v1/tasks/remote-42/workspace/out.pdf"), Filename: "out.pdf", MediaType: "application/pdf"},
			{Content: wire.Raw([]byte{1, 2, 3}), Filename: "blob.bin", MediaType: "application/octet-stream"},
			{Content: wire.Data{Value: map[string]any{"ok": true}}, MediaType: "application/json"},
		},
	}}}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, "k")})
	res := callA2A(context.Background(), t, c, "helpdesk_status", map[string]interface{}{"task_id": "remote-42"})
	text := res.Content[0].Text
	for _, want := range []string{
		"report — the findings",
		"all systems nominal",
		"file out.pdf (application/pdf): https://peer.example/v1/tasks/remote-42/workspace/out.pdf — not downloaded",
		"inline bytes blob.bin (application/octet-stream, 3 bytes) — not decoded",
		`data (application/json): {"ok":true}`,
		"The remote task has finished.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("render missing %q:\n%s", want, text)
		}
	}
	// A long text part is clipped under the inline cap.
	peer.mu.Lock()
	peer.artifacts = []*wire.Artifact{{ID: "big", Parts: []*wire.Part{wire.NewTextPart(strings.Repeat("y", a2aInlineTextCap*2))}}}
	peer.mu.Unlock()
	res = callA2A(context.Background(), t, c, "helpdesk_status", map[string]interface{}{"task_id": "remote-42"})
	if !strings.Contains(res.Content[0].Text, "truncated") || len(res.Content[0].Text) > a2aInlineTextCap+2048 {
		t.Errorf("long text part not clipped (len %d)", len(res.Content[0].Text))
	}
}

func TestA2APeerCancel(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateWorking}
	srv := peer.serve(t)
	c := NewClient()
	c.AddA2APeers([]A2APeerSpec{testPeerSpec(srv, "k")})
	res := callA2A(context.Background(), t, c, "helpdesk_cancel", map[string]interface{}{"task_id": "remote-42"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "TASK_STATE_CANCELED") {
		t.Fatalf("cancel: %+v", res)
	}
	if peer.lastMethod() != a2abridge.MethodCancelTask {
		t.Errorf("method = %q", peer.lastMethod())
	}
}

// TestAddA2APeers_ConcurrentWithCall is the reload-vs-call race guard: a
// mid-session re-registration concurrent with an in-flight call must be a
// data-race-free no-op for the caller (run under -race).
func TestAddA2APeers_ConcurrentWithCall(t *testing.T) {
	peer := &fakeA2APeer{state: wire.TaskStateWorking}
	srv := peer.serve(t)
	c := NewClient()
	spec := testPeerSpec(srv, "k")
	c.AddA2APeers([]A2APeerSpec{spec})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.AddA2APeers([]A2APeerSpec{spec})
		}()
		go func() {
			defer wg.Done()
			_, _ = c.CallToolOn(context.Background(), A2AToolServerName, "helpdesk_status", map[string]interface{}{"task_id": "remote-42"})
		}()
	}
	wg.Wait()
	if len(c.GetAllTools()) != 4 {
		t.Fatalf("tools after concurrent re-register = %d", len(c.GetAllTools()))
	}
}

func TestArgInt(t *testing.T) {
	for _, tc := range []struct {
		in   interface{}
		want int
	}{{float64(7), 7}, {int(3), 3}, {"12", 12}, {json.Number("5"), 5}, {"junk", 60}, {nil, 60}} {
		if got := argInt(map[string]interface{}{"n": tc.in}, "n", 60); got != tc.want {
			t.Errorf("argInt(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := a2aClip("héllo wörld", 4); !strings.HasPrefix(got, "hé") || !strings.Contains(got, "truncated") {
		t.Errorf("a2aClip at a rune boundary = %q", got)
	}
}
