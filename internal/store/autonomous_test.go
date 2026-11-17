package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func TestAutonomousAcceptanceRoundTrip(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	ver, ok, err := s.GetAutonomousAcceptance(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetAutonomousAcceptance initial: %v", err)
	}
	if ok || ver != 0 {
		t.Fatalf("initial acceptance = (%d, %v), want (0, false)", ver, ok)
	}

	if err := s.AcceptAutonomousPolicy(ctx, "  Alice@Example.com ", CurrentAutonomousPolicyVersion); err != nil {
		t.Fatalf("AcceptAutonomousPolicy: %v", err)
	}

	ver, ok, err = s.GetAutonomousAcceptance(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetAutonomousAcceptance after accept: %v", err)
	}
	if !ok || ver != CurrentAutonomousPolicyVersion {
		t.Fatalf("after accept = (%d, %v), want (%d, true)", ver, ok, CurrentAutonomousPolicyVersion)
	}
}

func TestUIAutoSubmitStoreLifecycleAndSingleClaimer(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AcceptAutonomousPolicy(ctx, "alice@example.com", CurrentAutonomousPolicyVersion); err != nil {
		t.Fatalf("AcceptAutonomousPolicy: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "alice@example.com", "Auto-submit test", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.SetApprovalMode(ctx, "alice@example.com", conv.ID, ConversationApprovalAutonomous); err != nil {
		t.Fatalf("SetApprovalMode: %v", err)
	}

	nowMs := time.Now().UnixMilli()
	row1, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_card_1",
		ActionID:       "submit",
		ValuesJSON:     `{"deal":"d1"}`,
		DeadlineMs:     nowMs + 10_000,
		Status:         UIAutoSubmitStatusPending,
		CreatedAtMs:    nowMs,
		UpdatedAtMs:    nowMs,
	})
	if err != nil {
		t.Fatalf("CreateUIAutoSubmit row1: %v", err)
	}
	if row1.RemainingMs <= 0 {
		t.Fatalf("expected positive RemainingMs on pending row, got %d", row1.RemainingMs)
	}

	active, err := s.ListActiveUIAutoSubmits(ctx, conv.ID)
	if err != nil || len(active) != 1 || active[0].ID != row1.ID {
		t.Fatalf("ListActiveUIAutoSubmits = (%v, %v), want [%s]", active, err, row1.ID)
	}

	// Pause transitions pending -> paused and keeps it visible in ListActiveUIAutoSubmits.
	paused, err := s.PauseUIAutoSubmits(ctx, conv.ID, "call_card_1")
	if err != nil || len(paused) != 1 || paused[0].Status != UIAutoSubmitStatusPaused {
		t.Fatalf("PauseUIAutoSubmits = (%v, %v), want paused", paused, err)
	}
	// Sweeping past deadline must NOT enqueue a paused row.
	sweptPaused, err := s.SweepAndEnqueueDueUIAutoSubmits(ctx, nowMs+20_000, 10, 20)
	if err != nil || len(sweptPaused) != 0 {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits on paused row = (%v, %v), want empty", sweptPaused, err)
	}

	// Create a due pending row and hammer SweepAndEnqueueDueUIAutoSubmits from 8 goroutines:
	// atomic tx + FOR UPDATE SKIP LOCKED must enqueue it exactly once!
	dueRow, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_reused",
		ActionID:       "submit",
		ValuesJSON:     `{"deal":"d2"}`,
		DeadlineMs:     nowMs - 100,
		Status:         UIAutoSubmitStatusPending,
		CreatedAtMs:    nowMs - 10_100,
		UpdatedAtMs:    nowMs - 10_100,
	})
	if err != nil {
		t.Fatalf("CreateUIAutoSubmit dueRow: %v", err)
	}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		enqueuedIDs []string
		sweepErrors []error
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, serr := s.SweepAndEnqueueDueUIAutoSubmits(ctx, nowMs, 10, 20)
			mu.Lock()
			defer mu.Unlock()
			if serr != nil {
				sweepErrors = append(sweepErrors, serr)
				return
			}
			for _, item := range got {
				if item.Enqueued {
					enqueuedIDs = append(enqueuedIDs, item.Row.ID)
				}
			}
		}()
	}
	wg.Wait()
	if len(sweepErrors) > 0 {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits errors: %v", sweepErrors)
	}
	if len(enqueuedIDs) != 1 || enqueuedIDs[0] != dueRow.ID {
		t.Fatalf("concurrent SweepAndEnqueueDueUIAutoSubmits enqueued %v, want [%s]", enqueuedIDs, dueRow.ID)
	}

	// Pausing a row that already fired must return status = "fired", NOT overwrite it to paused.
	pauseAfterFire, err := s.PauseUIAutoSubmits(ctx, conv.ID, "call_reused")
	if err != nil || len(pauseAfterFire) != 1 || pauseAfterFire[0].Status != UIAutoSubmitStatusFired {
		t.Fatalf("PauseUIAutoSubmits on fired row = (%v, %v), want status=fired", pauseAfterFire, err)
	}

	// CancelActiveUIAutoSubmits cancels BOTH paused ("call_card_1") AND any queued autosubmit:* row!
	cancelledRows, cancelledQueued, err := s.CancelActiveUIAutoSubmits(ctx, conv.ID, "")
	if err != nil {
		t.Fatalf("CancelActiveUIAutoSubmits: %v", err)
	}
	if len(cancelledRows) != 2 {
		t.Fatalf("expected both paused call_card_1 and queued-fired call_reused cancelled, got %v", cancelledRows)
	}
	if cancelledQueued != 1 {
		t.Fatalf("expected 1 queued autosubmit row cancelled, got %d", cancelledQueued)
	}
	if remActive, _ := s.ListActiveUIAutoSubmits(ctx, conv.ID); len(remActive) != 0 {
		t.Fatalf("expected 0 active autosubmits after CancelActiveUIAutoSubmits, got %v", remActive)
	}

	// Reusing the SAME call_id ("call_reused") on a later turn must allocate a
	// distinct UUID row.ID and enqueue cleanly without colliding on client_input_id!
	verifyReusedCallIDAndNewerUserActivity(ctx, t, s, conv.ID, dueRow.ID)
}

func verifyReusedCallIDAndNewerUserActivity(ctx context.Context, t *testing.T, s *Store, convID, firstRowID string) {
	t.Helper()
	laterNowMs := time.Now().UnixMilli()
	dueRow2, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
		ConversationID: convID,
		UserEmail:      "alice@example.com",
		CallID:         "call_reused",
		ActionID:       "submit",
		ValuesJSON:     `{"deal":"d3"}`,
		DeadlineMs:     laterNowMs - 50,
		Status:         UIAutoSubmitStatusPending,
		CreatedAtMs:    laterNowMs - 10_050,
		UpdatedAtMs:    laterNowMs - 10_050,
	})
	if err != nil {
		t.Fatalf("CreateUIAutoSubmit reused call_id: %v", err)
	}
	if dueRow2.ID == firstRowID {
		t.Fatalf("reused call_id reused old row ID %q", dueRow2.ID)
	}
	swept2, err := s.SweepAndEnqueueDueUIAutoSubmits(ctx, laterNowMs, 10, 20)
	if err != nil || len(swept2) != 1 || !swept2[0].Enqueued {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits reused call_id = (%+v, %v), want 1 enqueued", swept2, err)
	}

	// Verify newer user history entry cancels a pending row instead of enqueuing.
	freshNowMs := time.Now().UnixMilli()
	checkRow, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
		ConversationID: convID,
		UserEmail:      "alice@example.com",
		CallID:         "call_card_activity",
		ActionID:       "submit",
		ValuesJSON:     `{"deal":"d4"}`,
		DeadlineMs:     freshNowMs - 10,
		Status:         UIAutoSubmitStatusPending,
		CreatedAtMs:    freshNowMs - 10_010,
		UpdatedAtMs:    freshNowMs - 10_010,
	})
	if err != nil {
		t.Fatalf("CreateUIAutoSubmit checkRow: %v", err)
	}
	userPayload, _ := json.Marshal(map[string]string{"text": "Wait, change the floor to 3.00"})
	if _, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{{
		Role:    "user",
		Type:    "text",
		Content: userPayload,
	}}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	newer, err := s.HasNewerUserActivityForAutoSubmit(ctx, checkRow)
	if err != nil || !newer {
		t.Fatalf("HasNewerUserActivityForAutoSubmit after user turn = (%v, %v), want (true, nil)", newer, err)
	}
	sweptAfterUser, err := s.SweepAndEnqueueDueUIAutoSubmits(ctx, freshNowMs+100, 10, 20)
	if err != nil || len(sweptAfterUser) != 1 || sweptAfterUser[0].Enqueued || sweptAfterUser[0].Row.Status != UIAutoSubmitStatusCancelled {
		t.Fatalf("sweeper after newer user msg = (%+v, %v), want cancelled without enqueue", sweptAfterUser, err)
	}
}

func TestRecoverUIAutoSubmitsAndAtomicInputCancel(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "alice@example.com", "Boot Recovery", "default", "m1", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.AcceptAutonomousPolicy(ctx, "alice@example.com", CurrentAutonomousPolicyVersion); err != nil {
		t.Fatalf("AcceptAutonomousPolicy: %v", err)
	}
	if err := s.SetApprovalMode(ctx, "alice@example.com", conv.ID, ConversationApprovalAutonomous); err != nil {
		t.Fatalf("SetApprovalMode: %v", err)
	}

	nowMs := time.Now().UnixMilli()
	_, err = s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_boot_pending",
		ActionID:       "submit",
		ValuesJSON:     `{"deal":"boot"}`,
		DeadlineMs:     nowMs - 5_000,
		Status:         UIAutoSubmitStatusPending,
		CreatedAtMs:    nowMs - 15_000,
		UpdatedAtMs:    nowMs - 15_000,
	})
	if err != nil {
		t.Fatalf("CreateUIAutoSubmit: %v", err)
	}

	recovered, err := s.RecoverUIAutoSubmits(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverUIAutoSubmits = (%d, %v), want (1, nil)", recovered, err)
	}
	active, err := s.ListActiveUIAutoSubmits(ctx, conv.ID)
	if err != nil || len(active) != 1 || active[0].Status != UIAutoSubmitStatusPaused {
		t.Fatalf("after RecoverUIAutoSubmits active = (%+v, %v), want 1 paused", active, err)
	}
	// Sweeper must NOT fire a recovered/paused row.
	swept, err := s.SweepAndEnqueueDueUIAutoSubmits(ctx, nowMs+100, 10, 20)
	if err != nil || len(swept) != 0 {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits after boot recovery = (%+v, %v), want empty", swept, err)
	}

	// Enqueueing a human input atomically cancels active ui_autosubmits in the same tx.
	if _, _, err := s.EnqueueInput(ctx, InputQueueRow{
		ID:             "q-atomic-cancel",
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		ClientInputID:  "cli-atomic-1",
		Message:        "Manual follow-up",
		Attachments:    "[]",
		Mode:           InputModeQueued,
	}); err != nil {
		t.Fatalf("EnqueueInput: %v", err)
	}
	activeAfterInput, err := s.ListActiveUIAutoSubmits(ctx, conv.ID)
	if err != nil || len(activeAfterInput) != 0 {
		t.Fatalf("expected EnqueueInput to atomically cancel paused autosubmit, got (%+v, %v)", activeAfterInput, err)
	}
}
