package httpapi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// correlationEngine records the correlation a turn's context carries and
// fails the turn with err (nil runs the fake engine's normal turn).
type correlationEngine struct {
	fakeEngine
	err error

	cmu                    sync.Mutex
	convID, turnID, userID string
	reqID                  string
	traceID                string
	seen                   bool
}

func (e *correlationEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	e.cmu.Lock()
	e.convID = logging.Value(ctx, logging.KeyConvID)
	e.turnID = logging.Value(ctx, logging.KeyTurnID)
	e.userID = logging.Value(ctx, logging.KeyUserID)
	e.reqID = logging.Value(ctx, logging.KeyRequestID)
	e.traceID = logging.Value(ctx, logging.KeyTraceID)
	e.seen = true
	e.cmu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return e.fakeEngine.RunTurn(ctx, in, sink)
}

func (e *correlationEngine) snapshot() (conv, turn, user, req, trace string, seen bool) {
	e.cmu.Lock()
	defer e.cmu.Unlock()
	return e.convID, e.turnID, e.userID, e.reqID, e.traceID, e.seen
}

// turnSpanRecorder installs a recording tracer provider for one test (the
// global provider; these tests must not run in parallel).
// The cleanup also fails the test if a span the test started was never ended.
func turnSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	return spantest.InstallChecked(t).SpanRecorder
}

func waitEndedSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range sr.Ended() {
			if s.Name() == name {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no ended %q span", name)
	return nil
}

// The chat.turn root span records a RunTurn failure; a user's Stop is not
// an error (#2052).
func TestChatTurnSpanRecordsRunTurnError(t *testing.T) {
	sr := turnSpanRecorder(t)
	eng := &correlationEngine{err: errors.New("provider exploded")}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	postChatRequest(t, srv, map[string]any{"message": "go", "conversation_id": conv.ID})

	span := waitEndedSpan(t, sr, "chat.turn")
	if span.Status().Code != codes.Error {
		t.Fatalf("chat.turn status = %v after a RunTurn error, want Error", span.Status())
	}
}

func TestChatTurnSpanStopIsNotAnError(t *testing.T) {
	sr := turnSpanRecorder(t)
	eng := &cancelErrEngine{started: make(chan struct{})}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	go postChatRequest(t, srv, map[string]any{"message": "go", "conversation_id": conv.ID})
	<-eng.started
	entry, ok := srv.getInflight(conv.ID)
	if !ok {
		t.Fatal("no inflight turn")
	}
	srv.cancelInflightTurn(conv.ID, entry.turnID)

	span := waitEndedSpan(t, sr, "chat.turn")
	if span.Status().Code == codes.Error {
		t.Fatalf("chat.turn status = %v after a Stop, want not Error", span.Status())
	}
}

// Every launch path of a turn carries conv_id, turn_id and user_id on its
// context. A webhook-triggered turn also continues the webhook request's
// trace and carries its request_id.
func TestWebhookTurnCarriesCorrelation(t *testing.T) {
	_ = turnSpanRecorder(t)
	eng := &correlationEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, eng, st)

	body := []byte(`{"title":"Fix the widget"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	convID, _ := webhookResult(t, w)
	reqID := w.Header().Get("X-Request-Id")

	eventually(t, func() bool { _, _, _, _, _, seen := eng.snapshot(); return seen }, "webhook turn never ran")
	conv, turn, user, req, trace, _ := eng.snapshot()
	if conv != convID || turn == "" || user != webhookOwner {
		t.Errorf("webhook turn correlation conv=%q turn=%q user=%q, want conv=%q, a turn id, user=%q", conv, turn, user, convID, webhookOwner)
	}
	if reqID == "" || req != reqID {
		t.Errorf("webhook turn request_id = %q, want the webhook request's %q", req, reqID)
	}
	if trace == "" || trace != reqID {
		t.Errorf("webhook turn trace_id = %q, want the webhook request's trace %q", trace, reqID)
	}
}

// A turn launched with no request (a queue drain) still carries the row
// owner's user_id.
func TestRequestlessTurnCarriesUser(t *testing.T) {
	eng := &correlationEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	const user = "owner@x.com"
	conv, _ := st.CreateConversation(context.Background(), user, "t", "generic", "", false)
	if _, _, err := st.EnqueueInput(context.Background(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "drained", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := st.ClaimNextQueuedInput(context.Background(), conv.ID, "claim-placeholder")
	if err != nil || row == nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}

	// startTurn with a nil request is the queue-drain shape.
	srv.startTurn(nil, nil, user, conv, chatRequest{ConversationID: conv.ID, Message: row.Message},
		&queuedLaunch{rowID: row.ID, claimTurnID: row.TurnID}, func() {}, nil)

	eventually(t, func() bool { _, _, _, _, _, seen := eng.snapshot(); return seen }, "drained turn never ran")
	c, turn, gotUser, _, _, _ := eng.snapshot()
	if c != conv.ID || turn == "" || gotUser != user {
		t.Errorf("drained turn correlation conv=%q turn=%q user=%q, want conv=%q, a turn id, user=owner@x.com", c, turn, gotUser, conv.ID)
	}
}
