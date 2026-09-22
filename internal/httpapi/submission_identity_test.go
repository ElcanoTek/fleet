package httpapi

// A submission's identity on the wire (#1591, #1592).
//
// Two questions a browser cannot answer from its own state, and the two
// answers the server now gives it:
//
//   - "which conversation did my brand-new chat create?" — the POST response
//     header, so a stream that dies before the `conversation` frame still
//     leaves an id the recovery chain can probe.
//   - "is the turn you are running the one you started FOR ME?" — the
//     submission id echoed by /inflight, so a client whose acknowledgement was
//     lost does not bind its bubble to a turn that was already running.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A brand-new chat learns its conversation id from the `conversation` SSE
// frame. The response header carries the same id, and it arrives first — which
// is the whole point: a socket that dies in between leaves the browser holding
// an id every recovery endpoint is keyed by, instead of a pending key none of
// them knows (#1591).
func TestPostChat_NamesTheConversationOnTheResponseHeader(t *testing.T) {
	s := mockServer(t)

	w := postChatJSON(t, s, "u@x.com", map[string]any{
		"message": "hello mock",
		"persona": "generic",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}

	named := w.Header().Get("X-Fleet-Conversation-Id")
	if named == "" {
		t.Fatal("POST /chat did not name its conversation on the response headers")
	}
	// It must be the SAME id the stream goes on to announce, or a client that
	// promoted its pending slot on the header would be writing the rest of the
	// turn into a different conversation than the one the server is running.
	framed := conversationFrameID(t, w.Body.String())
	if framed != named {
		t.Fatalf("header names %q but the conversation frame names %q", named, framed)
	}
	if conv, err := s.store.Get(context.Background(), "u@x.com", named); err != nil || conv == nil {
		t.Fatalf("header named a conversation the store does not have: conv=%v err=%v", conv, err)
	}
}

// conversationFrameID pulls the id out of the SSE `conversation` frame.
func conversationFrameID(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "event: conversation" {
			continue
		}
		for _, next := range lines[i+1:] {
			next = strings.TrimSpace(next)
			if !strings.HasPrefix(next, "data: ") {
				continue
			}
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(next, "data: ")), &p); err != nil {
				t.Fatalf("decode conversation frame: %v", err)
			}
			return p.ID
		}
	}
	t.Fatalf("no conversation frame in SSE body:\n%s", body)
	return ""
}

// The lost-acknowledgement shape (#1592). A client believes the conversation
// is idle and posts directly; the server, which knows better, queues the
// input behind the turn already running — and that acknowledgement never
// arrives. /inflight has to say WHOSE submission the running turn belongs to,
// or the browser attaches its new bubble to a stranger's turn and renders that
// turn's answer under a prompt it never saw.
func TestInflight_EchoesTheSubmissionTheRunningTurnBelongsTo(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	// Turn 1 starts under its own submission and blocks.
	go postChatJSON(t, s, user, map[string]any{
		"message": "first question", "conversation_id": conv.ID, "submission_id": "sub-first",
	})
	<-eng.started

	probe := inflightProbe(t, s, conv.ID)
	if probe["inflight"] != true {
		t.Fatalf("expected a running turn, got %v", probe)
	}
	if probe["submission_id"] != "sub-first" {
		t.Fatalf("running turn reports submission_id=%v, want sub-first", probe["submission_id"])
	}

	// The second client thought the conversation was idle: a DIRECT submission
	// (no mode, no idempotency key), which the server queues.
	w := postChatJSON(t, s, user, map[string]any{
		"message": "second question", "conversation_id": conv.ID, "submission_id": "sub-second",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy submit: status=%d body=%s", w.Code, w.Body.String())
	}

	// This is the assertion the bug turns on: while the first turn runs,
	// /inflight must keep naming the FIRST submission. A client holding
	// sub-second reads that as "not mine" and leaves its slot mid-flight
	// instead of binding it to a turn that was already running.
	probe = inflightProbe(t, s, conv.ID)
	if probe["submission_id"] != "sub-first" {
		t.Fatalf("queued submission was reported as the running turn: %v", probe)
	}

	// And once the queued input drains into its own turn, /inflight names IT —
	// so the same client, still re-probing, finally attaches to its own turn.
	eng.release <- struct{}{}
	eng.release <- struct{}{}
	waitFor(t, "the queued input to run as its own turn", func() bool {
		return inflightProbe(t, s, conv.ID)["submission_id"] == "sub-second"
	})
}

// A queued submission that ALSO carries an idempotency key must still report
// its own identity. The two ids are different things — client_input_id is the
// idempotency key and carries the queue's unique index, submission_id is who
// the submission belongs to — and folding one into the other inverts #1592:
// the drained turn echoed the idempotency key, a value this client never
// minted, so the client read its OWN turn as a stranger's and kept polling
// instead of attaching.
func TestInflight_QueuedSubmissionKeepsItsIdentityAlongsideAnInputID(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{
		"message": "first question", "conversation_id": conv.ID, "submission_id": "sub-first",
	})
	<-eng.started

	// BOTH ids, which is the case that regressed: a retrying client sends an
	// idempotency key and its identity together.
	w := postChatJSON(t, s, user, map[string]any{
		"message": "second question", "conversation_id": conv.ID,
		"input_id": "idem-key-42", "submission_id": "sub-second",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy submit: status=%d body=%s", w.Code, w.Body.String())
	}

	eng.release <- struct{}{}
	eng.release <- struct{}{}
	waitFor(t, "the queued input to run as its own turn", func() bool {
		got := inflightProbe(t, s, conv.ID)["submission_id"]
		if got == "idem-key-42" {
			t.Fatalf("drained turn reported the idempotency key as its submission identity")
		}
		return got == "sub-second"
	})
}

// A submission id must never reach the idempotency column, because that column
// carries a unique index: two distinct submissions that happen to name no
// input_id would otherwise dedup against each other and the second would
// silently vanish instead of queueing.
func TestBusySubmit_SubmissionIDIsNotAnIdempotencyKey(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{
		"message": "first question", "conversation_id": conv.ID, "submission_id": "sub-first",
	})
	<-eng.started

	// Two submissions, same id, no input_id. They are distinct submissions and
	// both must queue; dedup here would be idempotency the caller never asked
	// for.
	for i, msg := range []string{"second question", "third question"} {
		w := postChatJSON(t, s, user, map[string]any{
			"message": msg, "conversation_id": conv.ID, "submission_id": "sub-same",
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("busy submit %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}

	rows, err := s.store.ListQueuedInputs(t.Context(), user, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("queued rows = %d, want 2 — a shared submission id deduped as an idempotency key", len(rows))
	}
	for _, r := range rows {
		if r.SubmissionID != "sub-same" {
			t.Errorf("row %s carries submission_id %q, want sub-same", r.ID, r.SubmissionID)
		}
		if r.ClientInputID == "sub-same" {
			t.Errorf("row %s stored the submission id in the idempotency column", r.ID)
		}
	}
}

// inflightProbe reads GET /conversations/{id}/inflight as the fixture's owner,
// which is the only user any of these tests create a conversation for.
func inflightProbe(t *testing.T, s *Server, convID string) map[string]any {
	t.Helper()
	const user = "alice@x.com"
	rr := do(t, s.Routes(), http.MethodGet, "/conversations/"+convID+"/inflight", nil, user)
	if rr.Code != http.StatusOK {
		t.Fatalf("inflight: status %d body=%s", rr.Code, rr.Body.String())
	}
	var probe map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode inflight probe: %v", err)
	}
	return probe
}

// A turn nobody's submission named reports no submission id at all. The
// omission is the point: a client must read "absent" as no evidence, not as
// "this turn is not yours" — webhook and scheduled turns name no submission,
// and so does any client older than #1592.
func TestInflight_OmitsSubmissionIDWhenTheTurnNamesNone(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	_, _, tok, _ := s.registerTurn(conv.ID, turnCancel)
	defer s.finishTurn(conv.ID, tok)

	probe := inflightProbe(t, s, conv.ID)
	if _, present := probe["submission_id"]; present {
		t.Fatalf("expected no submission_id key, got %v", probe)
	}
}
