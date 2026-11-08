package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// A queue-drained turn has no request to continue, so its trace starts with
// its own chat.turn root. turn.started must still name that trace: the root
// span exists before the frame is emitted, on every launch path (#2052).
func TestQueueDrainedTurnStartedCarriesTraceID(t *testing.T) {
	sr := turnSpanRecorder(t)
	eng := &cancelErrEngine{started: make(chan struct{})}
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
	srv.startTurn(nil, nil, user, conv, chatRequest{ConversationID: conv.ID, Message: row.Message},
		&queuedLaunch{rowID: row.ID, claimTurnID: row.TurnID}, func() {}, nil)
	<-eng.started

	entry, ok := srv.getInflight(conv.ID)
	if !ok {
		t.Fatal("no inflight turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	_ = entry.buf.Attach(ctx, 0, rec, nil)
	srv.cancelInflightTurn(conv.ID, entry.turnID)

	span := waitEndedSpan(t, sr, "chat.turn")
	want := span.SpanContext().TraceID().String()
	var started string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if strings.Contains(l, `"turn_id"`) && strings.Contains(l, `"queued":true`) {
			started = l
		}
	}
	if started == "" {
		t.Fatalf("no turn.started frame in the replay:\n%s", rec.Body.String())
	}
	if !strings.Contains(started, `"trace_id":"`+want+`"`) {
		t.Fatalf("turn.started = %s, want trace_id %s (the chat.turn root's trace)", started, want)
	}
}
