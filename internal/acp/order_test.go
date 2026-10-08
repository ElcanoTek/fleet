package acp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
)

func promptLine(id int, sid, text string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":%q}]}}`, id, sid, text)
}

func cancelLine(sid string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, sid)
}

// boundNote is how a wait that reaches orderWait says so on stderr.
const boundNote = "had not reached fleet acp within"

// keptOrder fails the test if a wait for the agent to catch up reached its
// bound (orderedInput.await): every line here reaches the agent promptly, so
// each wait must end because it caught up, not after orderWait.
func keptOrder(t *testing.T, r *stdioRun) {
	t.Helper()
	if strings.Contains(r.stderr.String(), boundNote) {
		t.Errorf("a wait reached its bound:\n%s", r.stderr)
	}
}

// answers reads stdout until it has the reply to each of ids, in whatever
// order they come: a prompt's reply is written once its handler returns,
// which can be after a later prompt it let run has been answered.
func (c *acpClient) answers(ids ...int) map[int]map[string]any {
	c.t.Helper()
	got := map[int]map[string]any{}
	for len(got) < len(ids) {
		m := c.next()
		if id, ok := m["id"].(float64); ok && slices.Contains(ids, int(id)) {
			got[int(id)] = m
		}
	}
	return got
}

// stopReasonOf is the stop reason of a session/prompt answer ("" for an
// error).
func stopReasonOf(m map[string]any) string {
	result, _ := m["result"].(map[string]any)
	reason, _ := result["stopReason"].(string)
	return reason
}

// Seen live from Buzz's buzz-acp: a session/cancel 0.25 ms behind a prompt
// was handled before the prompt was tracked, reached nothing, and the turn
// ran on until buzz-acp gave up on it and killed fleet acp. Here the prompt
// reaches the agent late (promptReceived), as the SDK's scheduling can leave
// it; the cancel still stops it.
func TestRunCancelReachesAPromptSentRightBeforeIt(t *testing.T) {
	prev := promptReceived
	promptReceived = func(acpsdk.PromptRequest) { time.Sleep(300 * time.Millisecond) }
	t.Cleanup(func() { promptReceived = prev }) // runs last: after run and its prompts are done
	started := make(chan struct{})
	ff := &fakeFleet{t: t, turn: blockingTurn(started)}
	r := startRun(t, ff)
	sid := r.openSession()

	// One write, as buzz-acp's two messages arrive: the prompt, then the cancel.
	r.send(promptLine(3, sid, "long job") + "\n" + cancelLine(sid))
	if reason := stopReasonOf(r.answer(3)); reason != "cancelled" {
		t.Fatalf("stop reason %q, want cancelled", reason)
	}
	ff.mu.Lock()
	chats, stops := len(ff.chats), len(ff.cancels)
	ff.mu.Unlock()
	if chats > 0 && stops == 0 {
		t.Error("the prompt was submitted and never stopped")
	}
	keptOrder(t, r)
}

// The other way round: a prompt sent right after a session/cancel is a new
// turn, which that cancel must not stop. Here the cancel reaches the agent
// late (cancelReceived); the prompt behind it still runs once the turn the
// cancel was for has stopped.
func TestRunPromptSentRightAfterACancelIsNotStoppedByIt(t *testing.T) {
	prev := cancelReceived
	cancelReceived = func() { time.Sleep(300 * time.Millisecond) }
	t.Cleanup(func() { cancelReceived = prev })
	started := make(chan struct{})
	var ff *fakeFleet
	ff = &fakeFleet{t: t, turn: func(w *sseWriter, r *http.Request) {
		if ff.nth() == 1 {
			blockingTurn(started)(w, r)
			return
		}
		w.emit("conversation", map[string]any{"id": "conv-slow"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-next"})
		w.emit("text.delta", map[string]any{"text": "next answer"})
		w.emit("turn.completed", map[string]any{})
	}}
	r := startRun(t, ff)
	sid := r.openSession()
	r.prompt(3, sid, "long job")
	r.working()

	r.send(cancelLine(sid) + "\n" + promptLine(4, sid, "the next message"))
	got := r.answers(3, 4)
	if reason := stopReasonOf(got[3]); reason != "cancelled" {
		t.Errorf("the running prompt: stop reason %q, want cancelled", reason)
	}
	if reason := stopReasonOf(got[4]); reason != "end_turn" {
		t.Errorf("the prompt sent after the cancel: stop reason %q, want end_turn", reason)
	}
	keptOrder(t, r)
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.chats) != 2 || ff.chats[1].Message != "the next message" {
		t.Errorf("fleet got %+v, want the next message submitted after the long job", ff.chats)
	}
}

// Two prompts sent back to back on one session run in the order they were
// sent, though the first reaches the agent late.
func TestRunPromptsSentBackToBackKeepTheirOrder(t *testing.T) {
	prev := promptReceived
	promptReceived = func(p acpsdk.PromptRequest) {
		if len(p.Prompt) > 0 && p.Prompt[0].Text != nil && p.Prompt[0].Text.Text == "first" {
			time.Sleep(300 * time.Millisecond)
		}
	}
	t.Cleanup(func() { promptReceived = prev })
	ff := &fakeFleet{t: t}
	r := startRun(t, ff)
	sid := r.openSession()

	r.send(promptLine(3, sid, "first") + "\n" + promptLine(4, sid, "second"))
	r.answers(3, 4)
	keptOrder(t, r)
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.chats) != 2 || ff.chats[0].Message != "first" || ff.chats[1].Message != "second" {
		t.Errorf("fleet got %+v, want first then second", ff.chats)
	}
}

// A prompt turned away before it is tracked (here: an unknown session)
// still counts as arrived, so the cancel and the prompt behind it do not
// wait for it.
func TestRunAPromptTurnedAwayStillCountsAsArrived(t *testing.T) {
	r := startRun(t, &fakeFleet{t: t})
	sid := r.openSession()
	start := time.Now()
	r.send(promptLine(3, "no-such-session", "x") + "\n" + cancelLine("no-such-session") + "\n" + promptLine(4, sid, "hello"))
	got := r.answers(3, 4)
	if m := got[3]; m["error"] == nil {
		t.Fatalf("a prompt on an unknown session was answered %v", m)
	}
	if reason := stopReasonOf(got[4]); reason != "end_turn" {
		t.Errorf("stop reason %q, want end_turn", reason)
	}
	if took := time.Since(start); took >= orderWait {
		t.Errorf("took %s: a line waited for the prompt that was turned away", took)
	}
	keptOrder(t, r)
}

// A wait that reaches its bound hands its line on but keeps its counts: what
// it waited for is late, not lost. Here the first prompt reaches the agent
// only after the cancel behind it has been handed on. The next cancel must
// still wait for the prompt sent before it, rather than count that late
// arrival as its own.
func TestOrderedInputKeepsItsCountsWhenAWaitReachesItsBound(t *testing.T) {
	prev := orderWait
	orderWait = 50 * time.Millisecond
	t.Cleanup(func() { orderWait = prev })
	ag := NewAgent(nil, nil, "", 0, "test")
	diag := &lockedBuffer{}
	ag.diag = diag
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	lines := make(chan string, 8)
	go func() { // the SDK's side: each line as it is handed on
		sc := bufio.NewScanner(newOrderedInput(pr, ag))
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	send := func(text string) { go func() { _, _ = io.WriteString(pw, text) }() }
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("handed on %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%q was never handed on", want)
		}
	}

	send(promptLine(1, "s", "late") + "\n" + cancelLine("s") + "\n")
	expect(promptLine(1, "s", "late"))
	expect(cancelLine("s")) // after orderWait: the prompt never arrived
	ag.arrive()             // it arrives late
	_ = ag.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: "s"})

	send(promptLine(2, "s", "next") + "\n" + cancelLine("s") + "\n")
	expect(promptLine(2, "s", "next"))
	expect(cancelLine("s"))
	if n := strings.Count(diag.String(), boundNote); n != 2 {
		t.Errorf("stderr noted %d waits that reached the bound, want 2: the second cancel must wait for the second prompt, not take the first one's late arrival for it:\n%s", n, diag)
	}
}

// The client going away ends a wait at once: no prompt is submitted after a
// hang-up, so there is no order left to keep.
func TestAwaitOrderEndsWhenTheClientGoesAway(t *testing.T) {
	ag := NewAgent(nil, nil, "", 0, "test")
	done := make(chan bool, 1)
	go func() {
		_, _, caughtUp := ag.awaitOrder(1, 0, time.Minute)
		done <- caughtUp
	}()
	ag.hangUp()
	select {
	case caughtUp := <-done:
		if !caughtUp {
			t.Error("the wait ended as not caught up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the client went away")
	}
}

// Every byte reaches the SDK as the client sent it: a blank line, lines over
// the SDK's limit (handed on undecoded, so the cancel just over it is not
// counted), and a last line with no newline.
func TestOrderedInputPassesEveryLineThrough(t *testing.T) {
	ag := NewAgent(nil, nil, "", 0, "test")
	huge := strings.Repeat("x", maxLine+70<<10) + "\n"
	cancelPrefix := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s","_meta":{"pad":"`
	justOver := cancelPrefix + strings.Repeat("x", maxLine+100-len(cancelPrefix)) + `"}}}` + "\n"
	in := "\n" + huge + justOver + `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n" + `{"jsonrpc":"2.0","id":2`
	o := newOrderedInput(strings.NewReader(in), ag)
	got, err := io.ReadAll(o)
	if err != nil || string(got) != in {
		t.Fatalf("read %d bytes, %v; want the %d bytes sent, unchanged", len(got), err, len(in))
	}
	if o.prompts != 0 || o.cancels != 0 {
		t.Errorf("counted %d prompts and %d cancels in lines that are neither", o.prompts, o.cancels)
	}
}

// A line over the SDK's limit is passed on as it comes, not held in memory
// until its end arrives (which, for a client that never sends one, would be
// never).
func TestOrderedInputPassesALongLineOnAsItComes(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	go func() { _, _ = pw.Write(bytes.Repeat([]byte("x"), maxLine+70<<10)) }() // no newline, ever
	o := newOrderedInput(pr, NewAgent(nil, nil, "", 0, "test"))
	got := make(chan int, 1)
	go func() {
		n := 0
		buf := make([]byte, 64<<10)
		for n <= maxLine {
			m, err := o.Read(buf)
			if err != nil {
				break
			}
			n += m
		}
		got <- n
	}()
	select {
	case n := <-got:
		if n <= maxLine {
			t.Errorf("read %d bytes, want more than maxLine", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the line was held back waiting for its end")
	}
}

// sdkDelivers must count exactly what acp-go-sdk hands Prompt and Cancel,
// or the reader's counts and the agent's drift apart. Each line goes through
// a real SDK connection, and the agent's own counts say whether it arrived,
// so an SDK release that changes its decoding or dispatch fails here.
func TestSDKDeliversWhatTheSDKDelivers(t *testing.T) {
	ag := NewAgent(nil, nil, "", 0, "test")
	inR, inW := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(ag, io.Discard, inR)
	conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = inW.Close() })
	counts := func() (prompts, cancels uint64) {
		ag.mu.Lock()
		defer ag.mu.Unlock()
		return ag.arrivals, ag.cancels
	}
	await := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("waited 5s for %s", what)
			}
		}
	}
	// A cancel the SDK always delivers, sent after each line: notifications
	// are handled in order, so once it is counted every notification before it
	// has been. A request runs on its own goroutine, so one that was not
	// delivered gets a short grace instead.
	fence := cancelLine("fence")
	for _, c := range []struct {
		line, want string
	}{
		{promptLine(1, "s", "hi"), acpsdk.AgentMethodSessionPrompt},
		{`{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`, acpsdk.AgentMethodSessionPrompt}, // a notification reaches Prompt too
		{cancelLine("s"), acpsdk.AgentMethodSessionCancel},
		{`{"jsonrpc":"2.0","id":2,"method":"session/cancel","params":{"sessionId":"s"}}`, acpsdk.AgentMethodSessionCancel}, // sent as a request
		{`{"jsonrpc":"2.0","method":"session\/cancel","params":{"sessionId":"s"}}`, acpsdk.AgentMethodSessionCancel},       // an escaped slash is the same method
		{`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s"}}`, ""},                              // no prompt: the SDK refuses it (Validate)
		{`{"jsonrpc":"2.0","method":"session/cancel","params":5}`, ""},                                                     // undecodable params
		{`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s","prompt":[]},"error":5}`, ""},        // the SDK cannot parse the message
		{`{"jsonrpc":"2.0","id":5,"result":{}}`, ""},
		{"not json", ""},
	} {
		if got := sdkDelivers([]byte(c.line)); got != c.want {
			t.Errorf("sdkDelivers(%s) = %q, want %q", c.line, got, c.want)
		}
		prompts, cancels := counts()
		wantPrompts, wantCancels := prompts, cancels+1 // the fence
		switch c.want {
		case acpsdk.AgentMethodSessionPrompt:
			wantPrompts++
		case acpsdk.AgentMethodSessionCancel:
			wantCancels++
		}
		if _, err := io.WriteString(inW, c.line+"\n"+fence+"\n"); err != nil {
			t.Fatal(err)
		}
		await("the fence", func() bool { _, n := counts(); return n >= wantCancels })
		if c.want == acpsdk.AgentMethodSessionPrompt {
			await("the prompt", func() bool { n, _ := counts(); return n >= wantPrompts })
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		if p, n := counts(); p != wantPrompts || n != wantCancels {
			t.Errorf("%s: the SDK delivered %d prompts and %d cancels, want %d and %d (sdkDelivers says %q)",
				c.line, p-prompts, n-cancels-1, wantPrompts-prompts, wantCancels-cancels-1, c.want)
		}
	}
}
