package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The shaping rules are the whole point of the endpoint, so they are tested
// without a database: a client's verdict on a turn is built from these.
func TestNewTurnOutcomeResponse_Shaping(t *testing.T) {
	modelRequired := `{"reason":"provider_refused","failed_model":"acme/whizz","message":"Pick a different model."}`

	tests := []struct {
		name        string
		rec         store.TurnOutcomeRecord
		liveRunning bool
		wantState   string
		wantReason  string
		wantDetail  string
	}{
		{
			name: "model_required is a failure with its own reason",
			rec: store.TurnOutcomeRecord{
				TurnRecord:    store.TurnRecord{TurnID: "t1", Status: store.TurnStatusError},
				TerminalEvent: "turn.model_required",
				TerminalData:  modelRequired,
			},
			wantState:  turnStateFailed,
			wantReason: turnReasonModelRequired,
			wantDetail: modelRequired,
		},
		{
			name: "a generic error keeps the frame's message",
			rec: store.TurnOutcomeRecord{
				TurnRecord:    store.TurnRecord{TurnID: "t2", Status: store.TurnStatusError},
				TerminalEvent: "turn.error",
				TerminalData:  `{"message":"the turn ended unexpectedly"}`,
			},
			wantState:  turnStateFailed,
			wantReason: turnReasonError,
			wantDetail: `{"message":"the turn ended unexpectedly"}`,
		},
		{
			// A turn that died before its terminal frame reached the ledger.
			// The state still has to get through; the reason honestly does not.
			name: "an error with no persisted frame reports no reason",
			rec: store.TurnOutcomeRecord{
				TurnRecord: store.TurnRecord{TurnID: "t3", Status: store.TurnStatusError},
			},
			wantState: turnStateFailed,
		},
		{
			name: "cancelled is not failed",
			rec: store.TurnOutcomeRecord{
				TurnRecord:    store.TurnRecord{TurnID: "t4", Status: store.TurnStatusCancelled},
				TerminalEvent: "turn.cancelled",
				TerminalData:  `{"reason":"cost_ceiling_reached"}`,
			},
			wantState:  turnStateCancelled,
			wantReason: turnReasonCancelled,
			wantDetail: `{"reason":"cost_ceiling_reached"}`,
		},
		{
			name: "completed carries no reason",
			rec: store.TurnOutcomeRecord{
				TurnRecord:    store.TurnRecord{TurnID: "t5", Status: store.TurnStatusCompleted},
				TerminalEvent: "turn.completed",
				TerminalData:  `{"cost_usd":0.01}`,
			},
			wantState:  turnStateCompleted,
			wantDetail: `{"cost_usd":0.01}`,
		},
		{
			// The seal-then-FinishTurn window: the registry knows the turn is
			// live, the row has not caught up. Running is the safe read.
			name: "a live turn overrides a row that says otherwise",
			rec: store.TurnOutcomeRecord{
				TurnRecord:    store.TurnRecord{TurnID: "t6", Status: store.TurnStatusError},
				TerminalEvent: "turn.error",
				TerminalData:  `{"message":"stale"}`,
			},
			liveRunning: true,
			wantState:   turnStateRunning,
		},
		{
			// The reverse must NOT happen: a registry that has forgotten a turn
			// says nothing about it, and the row is then authoritative.
			name: "a row that says running reports running",
			rec: store.TurnOutcomeRecord{
				TurnRecord: store.TurnRecord{TurnID: "t7", Status: store.TurnStatusRunning},
			},
			wantState: turnStateRunning,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newTurnOutcomeResponse(&tc.rec, tc.liveRunning)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if string(got.Detail) != tc.wantDetail {
				t.Errorf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// A payload the ledger cannot hand back as JSON must not take the state with
// it: the state is what the client decides on.
func TestNewTurnOutcomeResponse_DropsUnparseableDetail(t *testing.T) {
	got := newTurnOutcomeResponse(&store.TurnOutcomeRecord{
		TurnRecord:    store.TurnRecord{TurnID: "t", Status: store.TurnStatusError},
		TerminalEvent: "turn.error",
		TerminalData:  "{not json",
	}, false)
	if got.State != turnStateFailed || got.Reason != turnReasonError {
		t.Errorf("state/reason lost with the payload: %+v", got)
	}
	if got.Detail != nil {
		t.Errorf("detail = %q, want nothing", got.Detail)
	}
}

// finished_at is omitted rather than reported as 0 when the row has none —
// a client reading 0 as a timestamp would place the turn in 1970.
func TestNewTurnOutcomeResponse_OmitsAbsentFinishedAt(t *testing.T) {
	with := newTurnOutcomeResponse(&store.TurnOutcomeRecord{
		TurnRecord: store.TurnRecord{
			TurnID:     "t",
			Status:     store.TurnStatusCompleted,
			FinishedAt: sql.NullInt64{Int64: 1700000000, Valid: true},
		},
	}, false)
	if with.FinishedAt != 1700000000 {
		t.Errorf("finished_at = %d, want 1700000000", with.FinishedAt)
	}
	without := newTurnOutcomeResponse(&store.TurnOutcomeRecord{
		TurnRecord: store.TurnRecord{TurnID: "t", Status: store.TurnStatusError},
	}, false)
	if without.FinishedAt != 0 {
		t.Errorf("finished_at = %d, want 0 (omitted)", without.FinishedAt)
	}
}

// End to end over the real ledger: a turn that fails BEFORE producing a reply
// is exactly the shape the client could not decide on its own — nothing live,
// nothing retained, a transcript ending at the prompt. The endpoint has to
// call it a failure and say why, or the UI shows a question with no answer and
// no Retry (#1593).
func TestTurnOutcomeEndpoint_ReportsAFailureWithNoReply(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok, ok := s.registerTurn(conv.ID, cancel)
	if !ok {
		t.Fatal("registerTurn refused")
	}
	if err := buf.attachPersister(t.Context(), s.concreteStore(t)); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}
	// The user's prompt commits before the first provider call, so it is on
	// record even though the turn never answered.
	if _, err := s.concreteStore(t).CommitUserMessage(t.Context(), conv.ID, turnID,
		agent.HistoryEntry{Role: "user", Type: "text", Content: json.RawMessage(`"summarize this"`)}); err != nil {
		t.Fatalf("CommitUserMessage: %v", err)
	}
	buf.Emit("turn.started", map[string]any{"turn_id": turnID})
	buf.Emit("turn.model_required", map[string]any{
		"reason":       "provider_refused",
		"failed_model": "acme/whizz",
		"message":      "Pick a different model to continue.",
	})
	s.finishTurn(conv.ID, tok)
	t.Cleanup(func() {
		s.inflightMu.Lock()
		delete(s.inflight, conv.ID)
		s.inflightMu.Unlock()
	})

	rr := do(t, s.Routes(), http.MethodGet,
		"/conversations/"+conv.ID+"/turns/"+turnID, nil, "alice@x.com")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", rr.Code, rr.Body.String())
	}
	var got turnOutcomeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != turnStateFailed {
		t.Errorf("state = %q, want %q", got.State, turnStateFailed)
	}
	if got.Reason != turnReasonModelRequired {
		t.Errorf("reason = %q, want %q", got.Reason, turnReasonModelRequired)
	}
	if !got.UserCommitted {
		t.Error("user_committed = false, but the prompt was committed")
	}
	if got.FinishedAt == 0 {
		t.Error("finished_at = 0 on a sealed turn")
	}
	var detail struct {
		FailedModel string `json:"failed_model"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(got.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.FailedModel != "acme/whizz" || detail.Message == "" {
		t.Errorf("detail lost the frame's payload: %+v", detail)
	}
}

// A turn still generating reports running and advertises no outcome — and a
// prompt not yet committed says so, which is what lets a client tell "the
// transcript does not hold my question yet" from "my turn never started".
func TestTurnOutcomeEndpoint_RunningTurnHasNoOutcomeYet(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)
	if err := buf.attachPersister(t.Context(), s.concreteStore(t)); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}
	defer s.finishTurn(conv.ID, tok)
	buf.Emit("turn.started", map[string]any{"turn_id": turnID})

	rr := do(t, s.Routes(), http.MethodGet,
		"/conversations/"+conv.ID+"/turns/"+turnID, nil, "alice@x.com")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", rr.Code, rr.Body.String())
	}
	var got turnOutcomeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != turnStateRunning {
		t.Errorf("state = %q, want %q", got.State, turnStateRunning)
	}
	if got.Reason != "" || got.Detail != nil {
		t.Errorf("a running turn advertised an outcome: %+v", got)
	}
	if got.UserCommitted {
		t.Error("user_committed = true, but no user row was committed")
	}
}

// Ownership: another tenant's probe must not confirm that a turn exists, and
// an unknown turn id is a clean 404 rather than an invented verdict.
func TestTurnOutcomeEndpoint_ScopedAndAbsent(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)
	if err := buf.attachPersister(t.Context(), s.concreteStore(t)); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}
	buf.Emit("turn.completed", map[string]any{})
	s.finishTurn(conv.ID, tok)
	t.Cleanup(func() {
		s.inflightMu.Lock()
		delete(s.inflight, conv.ID)
		s.inflightMu.Unlock()
	})

	h := s.Routes()
	if rr := do(t, h, http.MethodGet,
		"/conversations/"+conv.ID+"/turns/"+turnID, nil, "eve@x.com"); rr.Code != http.StatusNotFound {
		t.Errorf("cross-user probe: status %d, want 404", rr.Code)
	}
	if rr := do(t, h, http.MethodGet,
		"/conversations/"+conv.ID+"/turns/no-such-turn", nil, "alice@x.com"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown turn: status %d, want 404", rr.Code)
	}
	if rr := do(t, h, http.MethodPost,
		"/conversations/"+conv.ID+"/turns/"+turnID, nil, "alice@x.com"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rr.Code)
	}
}
