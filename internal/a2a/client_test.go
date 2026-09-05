// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

// Tests for the outbound client (#1368) against a fake A2A peer: what goes on
// the wire (version, depth, credential headers, returnImmediately), how unary
// results and errors decode, and the SSE wait semantics. AllowPrivate is on
// because httptest listens on loopback — the SSRF guard itself is asserted in
// its own test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
)

// fakePeer is a minimal A2A JSON-RPC server: it records the last request's
// headers and params and answers each method from its configured state.
type fakePeer struct {
	mu        sync.Mutex
	lastReq   clientRequestSeen
	state     wire.TaskState
	artifacts []*wire.Artifact
	// subscribe controls the SubscribeToTask stream; nil uses the default
	// snapshot → keepalive → terminal statusUpdate script.
	subscribe func(w http.ResponseWriter, r *http.Request, id json.RawMessage)
	// bareMessage makes SendMessage answer with a Message instead of a Task.
	bareMessage bool
	// rpcError, when set, makes every method answer this JSON-RPC error.
	rpcError *ErrorObject
}

type clientRequestSeen struct {
	method  string
	headers http.Header
	params  json.RawMessage
}

func (f *fakePeer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.lastReq = clientRequestSeen{method: req.Method, headers: r.Header.Clone(), params: req.Params}
		state, artifacts, rpcErr, bare, sub := f.state, f.artifacts, f.rpcError, f.bareMessage, f.subscribe
		f.mu.Unlock()

		write := func(resp Response) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}
		if rpcErr != nil {
			write(Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
			return
		}
		task := &wire.Task{ID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: state}, Artifacts: artifacts}
		switch req.Method {
		case MethodSendMessage:
			if bare {
				write(NewResponse(req.ID, wire.StreamResponse{Event: wire.NewMessage(wire.MessageRoleAgent, wire.NewTextPart("direct reply"))}))
				return
			}
			task.Status.State = wire.TaskStateSubmitted
			write(NewResponse(req.ID, wire.StreamResponse{Event: task}))
		case MethodGetTask, MethodCancelTask:
			if req.Method == MethodCancelTask {
				task.Status.State = wire.TaskStateCanceled
			}
			write(NewResponse(req.ID, task))
		case MethodSubscribeToTask:
			if sub != nil {
				sub(w, r, req.ID)
				return
			}
			if state.Terminal() {
				write(NewErrorResponse(req.ID, wire.ErrUnsupportedOperation, "task is already terminal", nil))
				return
			}
			streamDefault(w, req.ID, task)
		default:
			write(NewErrorResponse(req.ID, wire.ErrMethodNotFound, "unknown method", nil))
		}
	})
}

func (f *fakePeer) last() clientRequestSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func sseStart(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fl.Flush()
	return fl
}

func sseFrame(w http.ResponseWriter, fl http.Flusher, resp Response) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fl.Flush()
}

// streamDefault: snapshot (WORKING) → keepalive → terminal statusUpdate → close.
func streamDefault(w http.ResponseWriter, id json.RawMessage, task *wire.Task) {
	fl := sseStart(w)
	task.Status.State = wire.TaskStateWorking
	sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: task}))
	fmt.Fprint(w, ": keepalive\n\n")
	fl.Flush()
	sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: &wire.TaskStatusUpdateEvent{
		TaskID: task.ID, ContextID: task.ContextID,
		Status: wire.TaskStatus{State: wire.TaskStateCompleted, Message: wire.NewMessage(wire.MessageRoleAgent, wire.NewTextPart("done"))},
	}}))
}

func newTestPeer(t *testing.T, peer *fakePeer) *Client {
	t.Helper()
	srv := httptest.NewServer(peer.handler())
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/a2a", map[string]string{"X-API-Key": "fleet_task_not_a_real_key"}, ClientOptions{AllowPrivate: true})
}

func TestClientSendMessageWire(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateSubmitted}
	client := newTestPeer(t, peer)

	res, err := client.SendMessage(context.Background(), "do the thing", "", 2)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Task == nil || res.Task.ID != "remote-1" || res.Task.Status.State != wire.TaskStateSubmitted {
		t.Fatalf("task oneof not decoded: %+v", res)
	}
	seen := peer.last()
	if seen.method != MethodSendMessage {
		t.Errorf("method = %q", seen.method)
	}
	if got := seen.headers.Get(wire.SvcParamVersion); got != string(wire.Version) {
		t.Errorf("A2A-Version = %q, want %q", got, wire.Version)
	}
	if got := seen.headers.Get(DepthHeader); got != "3" {
		t.Errorf("%s = %q, want depth+1 = 3", DepthHeader, got)
	}
	if got := seen.headers.Get("X-API-Key"); got != "fleet_task_not_a_real_key" {
		t.Errorf("credential header not applied: %q", got)
	}
	if got := seen.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var params wire.SendMessageRequest
	if err := json.Unmarshal(seen.params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.Config == nil || !params.Config.ReturnImmediately {
		t.Errorf("send must set returnImmediately: %+v", params.Config)
	}
	if params.Message == nil || params.Message.Role != wire.MessageRoleUser || params.Message.TaskID != "" {
		t.Errorf("message shape: %+v", params.Message)
	}
	if text := params.Message.Parts[0].Text(); text != "do the thing" {
		t.Errorf("text part = %q", text)
	}
	// A follow-up answer carries the task id.
	if _, err := client.SendMessage(context.Background(), "the second one", "remote-1", 0); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	_ = json.Unmarshal(peer.last().params, &params)
	if params.Message.TaskID != "remote-1" {
		t.Errorf("follow-up taskId = %q", params.Message.TaskID)
	}
}

func TestClientSendMessageBareMessage(t *testing.T) {
	peer := &fakePeer{bareMessage: true}
	client := newTestPeer(t, peer)
	res, err := client.SendMessage(context.Background(), "hi", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Task != nil || res.Message == nil || res.Message.Parts[0].Text() != "direct reply" {
		t.Fatalf("bare message not decoded: %+v", res)
	}
}

func TestClientGetTaskAndCancel(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	client := newTestPeer(t, peer)
	task, err := client.GetTask(context.Background(), "remote-1")
	if err != nil || task.Status.State != wire.TaskStateWorking {
		t.Fatalf("GetTask: %v %+v", err, task)
	}
	var params wire.GetTaskRequest
	_ = json.Unmarshal(peer.last().params, &params)
	if params.ID != "remote-1" {
		t.Errorf("GetTask params id = %q", params.ID)
	}
	task, err = client.Cancel(context.Background(), "remote-1")
	if err != nil || task.Status.State != wire.TaskStateCanceled {
		t.Fatalf("Cancel: %v %+v", err, task)
	}
}

func TestClientRPCErrorMapsToSentinel(t *testing.T) {
	peer := &fakePeer{rpcError: &ErrorObject{Code: -32001, Message: "no such task"}}
	client := newTestPeer(t, peer)
	_, err := client.GetTask(context.Background(), "nope")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || rpcErr.Message != "no such task" {
		t.Fatalf("want RPCError -32001, got %v", err)
	}
	if !errors.Is(err, wire.ErrTaskNotFound) {
		t.Errorf("errors.Is(wire.ErrTaskNotFound) must hold through SentinelFor")
	}
	if rpcErr.Reason() == "" {
		t.Errorf("Reason() should name the spec reason for a known code")
	}
	// An implementation-defined code has no sentinel and no reason.
	if (&RPCError{Code: -32050}).Reason() != "" || SentinelFor(-32050) != nil {
		t.Errorf("unknown codes must not map to a sentinel")
	}
}

func TestClientHTTPErrorKeepsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Unauthorized: send a fleet API key in X-API-Key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, map[string]string{"X-API-Key": "secret-value-never-echoed"}, ClientOptions{AllowPrivate: true})
	_, err := client.GetTask(context.Background(), "x")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("want HTTPError 401, got %v", err)
	}
	if !strings.Contains(httpErr.Body, "X-API-Key") {
		t.Errorf("remediation body dropped: %q", httpErr.Body)
	}
	if strings.Contains(err.Error(), "secret-value-never-echoed") {
		t.Errorf("credential value leaked into the error: %v", err)
	}
}

func TestClientWaitForUpdateStream(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	client := newTestPeer(t, peer)
	res, err := client.WaitForUpdate(context.Background(), "remote-1", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForUpdate: %v", err)
	}
	if !res.Changed || !res.Terminal || res.TimedOut {
		t.Fatalf("want changed+terminal, got %+v", res)
	}
	if res.Task == nil || res.Task.Status.State != wire.TaskStateCompleted {
		t.Fatalf("status update not applied: %+v", res.Task)
	}
	if res.Task.Status.Message == nil || res.Task.Status.Message.Parts[0].Text() != "done" {
		t.Errorf("status message dropped")
	}
	if got := peer.last().headers.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q", got)
	}
}

// TestClientWaitForUpdateArtifactThenStatusInOneCall is the settle-grace
// contract: fleet's server emits a finished task's artifacts and THEN the
// terminal status, back to back. One wait must return both — a WORKING
// snapshot with finished artifacts would cost the caller a second wait.
func TestClientWaitForUpdateArtifactThenStatusInOneCall(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	peer.subscribe = func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
		fl := sseStart(w)
		task := &wire.Task{ID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: wire.TaskStateWorking}}
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: task}))
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: &wire.TaskArtifactUpdateEvent{
			TaskID: "remote-1", ContextID: "remote-1", LastChunk: true,
			Artifact: &wire.Artifact{ID: "art-1", Name: "report", Parts: []*wire.Part{wire.NewTextPart("done text")}},
		}}))
		time.Sleep(50 * time.Millisecond) // the server's own write gap
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: &wire.TaskStatusUpdateEvent{
			TaskID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: wire.TaskStateCompleted},
		}}))
	}
	client := newTestPeer(t, peer)
	start := time.Now()
	res, err := client.WaitForUpdate(context.Background(), "remote-1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !res.Terminal || res.Task.Status.State != wire.TaskStateCompleted {
		t.Fatalf("one wait must carry the terminal status that followed the artifact: %+v", res)
	}
	if len(res.Task.Artifacts) != 1 || res.Task.Artifacts[0].Name != "report" {
		t.Fatalf("artifact lost: %+v", res.Task.Artifacts)
	}
	if elapsed := time.Since(start); elapsed >= streamSettleGrace {
		t.Fatalf("a status update must end the read at once, not after the settle grace (%s)", elapsed)
	}
}

// TestClientWaitForUpdateArtifactAloneSettles covers a peer that streams an
// artifact and then goes quiet: the wait returns the artifact after the
// settle grace, marked Changed (not TimedOut), well before the wait budget.
func TestClientWaitForUpdateArtifactAloneSettles(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	peer.subscribe = func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
		fl := sseStart(w)
		task := &wire.Task{ID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: wire.TaskStateWorking}}
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: task}))
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: &wire.TaskArtifactUpdateEvent{
			TaskID: "remote-1", ContextID: "remote-1",
			Artifact: &wire.Artifact{ID: "art-1", Name: "report", Parts: []*wire.Part{wire.NewTextPart("partial")}},
		}}))
		<-r.Context().Done() // quiet until the client hangs up
	}
	client := newTestPeer(t, peer)
	start := time.Now()
	res, err := client.WaitForUpdate(context.Background(), "remote-1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if !res.Changed || res.Terminal || res.TimedOut || len(res.Task.Artifacts) != 1 || res.Task.Artifacts[0].Name != "report" {
		t.Fatalf("artifact-only wait must settle as Changed with the artifact: %+v", res)
	}
	if elapsed < streamSettleGrace || elapsed > 5*time.Second {
		t.Fatalf("expected a return after the settle grace (~%s), got %s", streamSettleGrace, elapsed)
	}
}

func TestClientWaitForUpdateTimeoutReturnsSnapshot(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	peer.subscribe = func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
		fl := sseStart(w)
		task := &wire.Task{ID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: wire.TaskStateWorking}}
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: task}))
		// Keep the stream open with keepalives until the client gives up.
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		}
	}
	client := newTestPeer(t, peer)
	start := time.Now()
	res, err := client.WaitForUpdate(context.Background(), "remote-1", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Changed || res.Task == nil || res.Task.Status.State != wire.TaskStateWorking {
		t.Fatalf("want timed-out snapshot, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("wait overran its budget: %s", elapsed)
	}
}

func TestClientWaitForUpdateTerminalRefusalFallsBackToGetTask(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateCompleted}
	client := newTestPeer(t, peer)
	res, err := client.WaitForUpdate(context.Background(), "remote-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Terminal || res.Task == nil || res.Task.Status.State != wire.TaskStateCompleted {
		t.Fatalf("want GetTask fallback on -32004, got %+v", res)
	}
	if peer.last().method != MethodGetTask {
		t.Errorf("fallback should have called GetTask, last method %q", peer.last().method)
	}
}

func TestClientWaitForUpdateStreamClosedEarly(t *testing.T) {
	peer := &fakePeer{}
	peer.subscribe = func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
		fl := sseStart(w)
		task := &wire.Task{ID: "remote-1", ContextID: "remote-1", Status: wire.TaskStatus{State: wire.TaskStateWorking}}
		sseFrame(w, fl, NewResponse(id, wire.StreamResponse{Event: task}))
	}
	client := newTestPeer(t, peer)
	res, err := client.WaitForUpdate(context.Background(), "remote-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.StreamClosed || res.Terminal || res.Changed {
		t.Fatalf("want stream-closed snapshot, got %+v", res)
	}
}

func TestClientWaitOversizedEventRefused(t *testing.T) {
	peer := &fakePeer{}
	peer.subscribe = func(w http.ResponseWriter, _ *http.Request, _ json.RawMessage) {
		fl := sseStart(w)
		fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", maxEventBytes+10))
		fl.Flush()
	}
	client := newTestPeer(t, peer)
	if _, err := client.WaitForUpdate(context.Background(), "remote-1", time.Second); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized event must be refused, got %v", err)
	}
}

func TestClientUnaryBodyCapRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"%s"}}`, strings.Repeat("a", maxUnaryBody))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, nil, ClientOptions{AllowPrivate: true})
	if _, err := client.GetTask(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body must be refused, got %v", err)
	}
}

func TestClientRedirectRefusedEvenWhenPrivateAllowed(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	client := NewClient(srv.URL+"/a2a", map[string]string{"X-API-Key": "k"}, ClientOptions{AllowPrivate: true})
	_, err := client.GetTask(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect must be refused, got %v", err)
	}
	if followed {
		t.Fatal("the redirect target was requested — a credential could have been relayed")
	}
}

func TestClientSSRFGuardBlocksLoopback(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true }))
	defer srv.Close()
	client := NewClient(srv.URL, nil, ClientOptions{}) // guard ON
	_, err := client.GetTask(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("loopback peer must be refused at dial with the guard on, got %v", err)
	}
	if reached {
		t.Fatal("the loopback peer was reached despite the guard")
	}
	if _, err := client.WaitForUpdate(context.Background(), "x", time.Second); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("the streaming client must carry the same guard, got %v", err)
	}
}

// TestClientNullResultRefused: a JSON `null` result is neither a result nor an
// error. Decoded into wire.Task it would be a zero-valued task (empty id and
// state) the caller treats as data; the client must refuse it instead.
func TestClientNullResultRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":null}`, req.ID)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL+"/a2a", nil, ClientOptions{AllowPrivate: true})
	task, err := client.GetTask(context.Background(), "remote-1")
	if err == nil || !strings.Contains(err.Error(), "neither result nor error") {
		t.Fatalf("want null result refused, got task=%+v err=%v", task, err)
	}
}

// TestClientWaitForUpdateHonorsCallerCancel: the GetTask fallback after an
// empty stream must run under the CALLER's context, not a WithoutCancel copy —
// once the caller is gone, no further request goes to the peer and the
// partial result comes back at once.
func TestClientWaitForUpdateHonorsCallerCancel(t *testing.T) {
	peer := &fakePeer{state: wire.TaskStateWorking}
	peer.subscribe = func(w http.ResponseWriter, r *http.Request, _ json.RawMessage) {
		// Open the stream but send no snapshot; hold it until the client hangs up.
		fl := sseStart(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		}
	}
	client := newTestPeer(t, peer)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	res, err := client.WaitForUpdate(ctx, "remote-1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("wait outlived the caller's cancel: %s", elapsed)
	}
	if res.Task != nil || !res.TimedOut {
		t.Fatalf("want the partial (snapshot-less) result, got %+v", res)
	}
	if peer.last().method == MethodGetTask {
		t.Fatal("GetTask fallback was issued for a caller that had already cancelled")
	}
}
