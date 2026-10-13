package acp

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
)

// titlingTurns is a fake turn whose first call, like a new conversation's
// first turn, holds its stream open past turn.completed until release closes
// (the server titling the conversation); later calls end at once.
func titlingTurns(release <-chan struct{}) func(w *sseWriter, r *http.Request) {
	var calls atomic.Int32
	return func(w *sseWriter, r *http.Request) {
		first := calls.Add(1) == 1
		w.emit("conversation", map[string]any{"id": "conv-r"})
		w.emit("text.delta", map[string]any{"text": "answer"})
		w.emit("turn.completed", map[string]any{})
		if !first {
			return
		}
		select {
		case <-release:
			w.emit("conversation.title_updated", map[string]any{"id": "conv-r", "title": "T"})
		case <-r.Context().Done():
		}
	}
}

func (h *harness) chatCount() int {
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	return len(h.fleet.chats)
}

// The first prompt answers as its turn ends, while the server still holds
// the stream open; the next prompt on the session is not submitted until
// that stream has ended, because fleet would queue it behind the turn still
// finishing and its answer would never reach the client.
func TestNextPromptWaitsForTheLastTurnToFinish(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: titlingTurns(release)})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	sid := h.newSession(t)
	resp, err := h.prompt(sid, "first")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("first prompt = %+v, %v; want end_turn while the stream is open", resp, err)
	}
	second := h.promptAsync(t, sid, "second", nil, 1)
	time.Sleep(100 * time.Millisecond)
	if n := h.chatCount(); n != 1 {
		t.Fatalf("%d turns reached fleet while the first was still finishing, want 1", n)
	}
	close(release)
	r := await(t, second, "the second prompt")
	if r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("second prompt = %+v, %v; want end_turn", r.resp, r.err)
	}
	if n := h.chatCount(); n != 2 {
		t.Errorf("%d turns reached fleet, want 2", n)
	}
}

// A session/cancel while the next prompt waits for the last turn to finish
// ends it cancelled, and it never reaches fleet.
func TestCancelWhileWaitingForTheLastTurnSubmitsNothing(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: titlingTurns(release)})
	t.Cleanup(func() { close(release) })
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	second := h.promptAsync(t, sid, "second", nil, 1)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	r := await(t, second, "the cancelled prompt")
	if r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("second prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	if n := h.chatCount(); n != 1 {
		t.Errorf("%d turns reached fleet, want 1", n)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.cancels) != 0 {
		t.Errorf("a Stop was sent for a turn that had ended: %q", h.fleet.cancels)
	}
}

// The client going away while the next prompt waits keeps it from fleet.
func TestHangUpWhileWaitingForTheLastTurnSubmitsNothing(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: titlingTurns(release)})
	t.Cleanup(func() { close(release) })
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	h.promptAsync(t, sid, "second", nil, 1)
	h.hangUp()
	idle, _ := h.agent.inFlight()
	select {
	case <-idle:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting prompt was still in flight after the hang-up")
	}
	if n := h.chatCount(); n != 1 {
		t.Errorf("%d turns reached fleet, want 1", n)
	}
}

// A stream that never ends holds the next prompt for restWait at most: the
// read is then given up on, and the prompt goes ahead.
func TestWaitForTheLastTurnIsBounded(t *testing.T) {
	prev := restWait
	restWait = 100 * time.Millisecond
	t.Cleanup(func() { restWait = prev })
	h := newHarness(t, harnessOpts{turn: titlingTurns(make(chan struct{}))}) // never released
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := h.prompt(sid, "second")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("second prompt = %+v, %v; want end_turn", resp, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the second prompt waited %s", took)
	}
	if n := h.chatCount(); n != 2 {
		t.Errorf("%d turns reached fleet, want 2", n)
	}
}

// awaitRest: a read that has ended lets the prompt go, and clears it; a stop
// ends the wait, and wins when the read ended with it (the client going away
// ends both), whichever the select sees first.
func TestAwaitRest(t *testing.T) {
	ended := make(chan struct{})
	close(ended)
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if s := (&session{}); !s.awaitRest(context.Background()) {
		t.Error("no rest to wait for, yet the prompt was stopped")
	}
	if s := (&session{rest: ended}); !s.awaitRest(context.Background()) || s.rest != nil {
		t.Error("an ended read held the prompt, or was kept")
	}
	if s := (&session{rest: make(chan struct{})}); s.awaitRest(stopped) {
		t.Error("a stop did not end the wait")
	}
	for range 100 {
		if s := (&session{rest: ended}); s.awaitRest(stopped) {
			t.Fatal("a stopped prompt went ahead because its read had ended too")
		}
	}
}
