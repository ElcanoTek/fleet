package acp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
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

// A stream that never ends holds the next prompt for Agent.restWait at most: the
// read is then given up on, and the prompt goes ahead.
func TestWaitForTheLastTurnIsBounded(t *testing.T) {
	h := newHarness(t, harnessOpts{turn: titlingTurns(make(chan struct{}))}) // never released
	h.agent.restWait = 100 * time.Millisecond
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

// After a Stop the server still finishes the stopped turn (it records it,
// persists its history) before it ends the stream, and counts it as running
// until then. The next prompt waits for that too: the Stop does not cut the
// stream once the turn's terminal frame is in.
func TestNextPromptAfterAStopWaitsForTheStoppedTurn(t *testing.T) {
	release := make(chan struct{})
	var streamEnded, secondEarly atomic.Bool
	var calls atomic.Int32
	h := newHarness(t, harnessOpts{})
	h.fleet.turn = func(w *sseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			if !streamEnded.Load() {
				secondEarly.Store(true)
			}
			w.emit("conversation", map[string]any{"id": "conv-s"})
			w.emit("turn.completed", map[string]any{})
			return
		}
		w.emit("conversation", map[string]any{"id": "conv-s"})
		w.emit("turn.started", map[string]any{"turn_id": "t1"})
		w.emit("text.delta", map[string]any{"text": "working"})
		for { // until the Stop arrives
			h.fleet.mu.Lock()
			n := len(h.fleet.cancels)
			h.fleet.mu.Unlock()
			if n > 0 {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
		w.emit("turn.cancelled", map[string]any{})
		select { // the stopped turn's post-turn work
		case <-release:
		case <-r.Context().Done():
			<-release // the server goes on with it whatever the client does
		}
		streamEnded.Store(true)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	sid := h.newSession(t)
	first := h.promptAsync(t, sid, "first", nil, 1)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if r := await(t, first, "the stopped prompt"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("first prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	second := h.promptAsync(t, sid, "second", nil, 1)
	time.Sleep(100 * time.Millisecond)
	close(release)
	if r := await(t, second, "the next prompt"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("second prompt = %+v, %v; want end_turn", r.resp, r.err)
	}
	if secondEarly.Load() {
		t.Error("the next prompt reached fleet while the stopped turn's stream was still open")
	}
}

// The client going away ends the read of a finished turn's stream at once,
// not after Agent.restWait.
func TestRestReadEndsWhenTheClientGoesAway(t *testing.T) {
	cut := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-g"})
		w.emit("turn.completed", map[string]any{})
		<-r.Context().Done()
		close(cut)
	}})
	sid := h.newSession(t)
	if _, err := h.prompt(sid, "first"); err != nil {
		t.Fatal(err)
	}
	h.hangUp()
	select {
	case <-cut:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was still read after the client went away")
	}
}

// recordingTurns is a turnClient for stopTurn alone: it records Stops.
type recordingTurns struct {
	mu    sync.Mutex
	stops int
}

func (r *recordingTurns) CheckIdentity(context.Context) error { return nil }
func (r *recordingTurns) StreamTurn(context.Context, string, string, string, func(chattui.Event)) (string, <-chan struct{}, error) {
	return "", nil, errors.New("not used")
}
func (r *recordingTurns) Cancel(string, string) error      { return r.stop() }
func (r *recordingTurns) CancelInput(string, string) error { return r.stop() }
func (r *recordingTurns) stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	return nil
}

// A Stop or a timeout that fires after the turn's terminal frame was handled,
// but before StreamTurn has returned, sends no Stop (it is conversation-scoped
// and could hit a follow-up queued from another surface), reports that the
// turn had ended, and leaves the stream to the prompt, so the next prompt
// still waits for the turn's post-turn work.
func TestStopTurnNeverStopsATurnThatEnded(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, cancelTimeout := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelTimeout()
	for name, stop := range map[string]context.Context{"cancelled": cancelled, "timed out": timedOut} {
		turns := &recordingTurns{}
		a := NewAgent(turns, nil, "", 0, "test")
		tr := newTranslator("s", "", func(acpsdk.SessionUpdate) {})
		tr.handle(chattui.Event{Name: "conversation", Data: map[string]any{"id": "c"}})
		tr.handle(chattui.Event{Name: "turn.started", Data: map[string]any{"turn_id": "t"}})
		tr.handle(chattui.Event{Name: "turn.completed", Data: map[string]any{}})
		stopped := make(chan stopOutcome, 1)
		cutStream := false
		a.stopTurn(stop, tr, "key", make(chan struct{}), stopped, func() { cutStream = true })
		out := <-stopped
		if !out.alreadyEnded || out.intervened || out.err != nil {
			t.Errorf("%s: outcome %+v, want alreadyEnded", name, out)
		}
		if turns.stops != 0 {
			t.Errorf("%s: %d Stops sent for a turn that had ended", name, turns.stops)
		}
		if cutStream {
			t.Errorf("%s: the stream was cut, so the next prompt would not wait for the turn's post-turn work", name)
		}
	}
}
