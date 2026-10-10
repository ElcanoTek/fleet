package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// Resume after approval, the durable half (docs/RESUME-AFTER-APPROVAL.md).

const resumeUser = "alice@example.com"

func resumeConv(t *testing.T, s *Store) *Conversation {
	t.Helper()
	conv, err := s.CreateConversation(context.Background(), resumeUser, "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv
}

// stageResume creates a pending approval and, when arm is set, arms it.
func stageResume(t *testing.T, s *Store, convID, tool string, arm bool) *Approval {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateApproval(ctx, convID, resumeUser, tool, "call_"+tool, `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if arm {
		ok, err := s.ArmApprovalResume(ctx, resumeUser, a.ID)
		if err != nil || !ok {
			t.Fatalf("ArmApprovalResume = %t, %v", ok, err)
		}
	}
	return a
}

func approveWithResult(t *testing.T, s *Store, id string, isErr bool) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.ClaimApproval(ctx, resumeUser, id, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval = %t, %v", ok, err)
	}
	if err := s.SetApprovalResult(ctx, resumeUser, id, "done", isErr); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
}

func resumeState(t *testing.T, s *Store, id string) string {
	t.Helper()
	a, err := s.GetApproval(context.Background(), resumeUser, id)
	if err != nil || a == nil {
		t.Fatalf("GetApproval: %v %v", a, err)
	}
	return a.ResumeState
}

func resumeRows(t *testing.T, s *Store, convID string) []InputQueueRow {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		inputQueueSelect+` WHERE conversation_id = $1 AND mode = 'resume' ORDER BY created_at, id`, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []InputQueueRow
	for rows.Next() {
		r, err := scanInputRow(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func notices(t *testing.T, s *Store, convID string) []agent.NoticeContent {
	t.Helper()
	h, err := s.LoadHistory(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	var out []agent.NoticeContent
	for _, e := range h {
		if e.Type != agent.EntryTypeNotice {
			continue
		}
		var c agent.NoticeContent
		if err := json.Unmarshal(e.Content, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func claimResume(t *testing.T, s *Store, convID string) ApprovalResumeResult {
	t.Helper()
	res, err := s.ClaimApprovalResume(context.Background(), ApprovalResumeRequest{
		ConversationID: convID, MaxPerHour: 10, MaxPending: 20,
	})
	if err != nil {
		t.Fatalf("ClaimApprovalResume: %v", err)
	}
	return res
}

// One approved and one declined card settle into ONE resume row naming both,
// owned by the conversation's user; a second claim finds nothing left to
// claim, so a card can never start two resumes.
func TestClaimApprovalResume_ClaimsSettledCardsIntoOneInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := resumeConv(t, s)
	a1 := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
	a2 := stageResume(t, s, conv.ID, "mcp_deals_update_deal_b", true)
	other := stageResume(t, s, conv.ID, "mcp_sendgrid_send_email", false)
	approveWithResult(t, s, a1.ID, false)
	if ok, err := s.ClaimApproval(ctx, resumeUser, a2.ID, "rejected", "User declined this action."); err != nil || !ok {
		t.Fatalf("decline: %v %v", ok, err)
	}
	if ok, err := s.ClaimApproval(ctx, resumeUser, other.ID, "rejected", "User declined to send."); err != nil || !ok {
		t.Fatalf("decline other: %v %v", ok, err)
	}

	res := claimResume(t, s, conv.ID)
	if res.Input == nil || len(res.Claimed) != 2 || res.Skipped != "" {
		t.Fatalf("result = %+v, want one input for the two armed cards", res)
	}
	got := map[string]string{}
	for _, it := range res.Claimed {
		got[it.ID] = it.Outcome
	}
	if got[a1.ID] != ApprovalResumeOutcomeApproved || got[a2.ID] != ApprovalResumeOutcomeDeclined {
		t.Fatalf("claimed outcomes = %v", got)
	}
	in := res.Input
	if in.Mode != InputModeResume || in.State != InputStateQueued || in.UserEmail != resumeUser {
		t.Fatalf("input = %+v, want a queued resume row owned by the conversation's user", in)
	}
	for _, want := range []string{"[Approvals resolved]", a1.ID + " outcome=approved", a2.ID + " outcome=declined", "Written by fleet, not the user"} {
		if !strings.Contains(in.Message, want) {
			t.Errorf("input text %q lacks %q", in.Message, want)
		}
	}
	if strings.Contains(in.Message, other.ID) {
		t.Errorf("a card that did not opt in must not be named: %q", in.Message)
	}
	if resumeState(t, s, a1.ID) != ApprovalResumeClaimed || resumeState(t, s, other.ID) != "" {
		t.Fatalf("states = %q / %q", resumeState(t, s, a1.ID), resumeState(t, s, other.ID))
	}
	again := claimResume(t, s, conv.ID)
	if again.Input != nil || len(again.Claimed) != 0 {
		t.Fatalf("second claim = %+v, want a no-op", again)
	}
	if n := len(resumeRows(t, s, conv.ID)); n != 1 {
		t.Fatalf("%d resume rows, want 1", n)
	}
}

// Concurrent claims (two settlements racing) still produce exactly one row.
func TestClaimApprovalResume_ConcurrentClaimsEnqueueOnce(t *testing.T) {
	s := newTestStore(t)
	conv := resumeConv(t, s)
	a := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
	approveWithResult(t, s, a.ID, true)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ClaimApprovalResume(context.Background(), ApprovalResumeRequest{ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20}); err != nil {
				t.Errorf("ClaimApprovalResume: %v", err)
			}
		}()
	}
	wg.Wait()
	rows := resumeRows(t, s, conv.ID)
	if len(rows) != 1 {
		t.Fatalf("%d resume rows, want exactly 1", len(rows))
	}
	if !strings.Contains(rows[0].Message, "outcome=approved result=error") {
		t.Fatalf("an approved call that failed must say so: %q", rows[0].Message)
	}
}

// Another card still pending or executing defers the resume (nothing
// claimed); a pending preview card does not, and neither does a sentinel the
// caller knows is a failed outcome write — but that card itself is not
// claimed, because its outcome is not in the conversation.
func TestClaimApprovalResume_WaitsForOutstandingCards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := resumeConv(t, s)
	a := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
	pending := stageResume(t, s, conv.ID, "mcp_deals_create_deal", false)
	approveWithResult(t, s, a.ID, false)

	res := claimResume(t, s, conv.ID)
	if len(res.Outstanding) != 1 || res.Outstanding[0] != pending.ID || len(res.Claimed) != 0 {
		t.Fatalf("result = %+v, want deferred on the pending card", res)
	}
	if resumeState(t, s, a.ID) != ApprovalResumeArmed {
		t.Fatal("a deferred claim must leave the card armed")
	}

	// The pending card goes executing: still outstanding.
	if ok, err := s.ClaimApproval(ctx, resumeUser, pending.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if res := claimResume(t, s, conv.ID); len(res.Outstanding) != 1 {
		t.Fatalf("executing card must defer: %+v", res)
	}
	// Ignored (its outcome write failed in-process): no longer outstanding.
	res, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{
		ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20,
		IgnoreOutstanding: func(id string) bool { return id == pending.ID },
	})
	if err != nil || res.Input == nil || len(res.Claimed) != 1 || res.Claimed[0].ID != a.ID {
		t.Fatalf("result = %+v, %v; want the settled card claimed", res, err)
	}

	// A pending preview card never blocks (display-only, never expires).
	conv2 := resumeConv(t, s)
	b := stageResume(t, s, conv2.ID, "mcp_deals_update_deal", true)
	stageResume(t, s, conv2.ID, "preview_email", false)
	approveWithResult(t, s, b.ID, false)
	if res := claimResume(t, s, conv2.ID); res.Input == nil {
		t.Fatalf("a pending preview must not defer: %+v", res)
	}
}

// An armed executing card whose sentinel stands (nothing ignored) is
// outstanding; ignoring it lets other cards resume but leaves it armed for
// boot recovery.
func TestClaimApprovalResume_SentinelCardStaysArmed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := resumeConv(t, s)
	a := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
	if ok, err := s.ClaimApproval(ctx, resumeUser, a.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatal(ok, err)
	}
	res, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{
		ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20,
		IgnoreOutstanding: func(string) bool { return true },
	})
	if err != nil || len(res.Claimed) != 0 || res.Input != nil {
		t.Fatalf("result = %+v, %v; want nothing claimed", res, err)
	}
	if resumeState(t, s, a.ID) != ApprovalResumeArmed {
		t.Fatal("the unrecorded card must stay armed")
	}
}

// Over the hourly cap, or with the queue full, the cards are marked skipped
// and the conversation gets a note instead of a turn.
func TestClaimApprovalResume_CapAndQueueFullSkipWithANote(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		maxPerHour, maxPending  int
		seedResumes, seedQueued int
		want, noteWant          string
	}{
		"hourly cap": {2, 20, 2, 0, ApprovalResumeSkipRateLimited, "2 times in the last hour"},
		"queue full": {10, 3, 0, 3, ApprovalResumeSkipQueueFull, "queue is full"},
	} {
		t.Run(name, func(t *testing.T) {
			conv := resumeConv(t, s)
			for i := range tc.seedResumes {
				if _, _, err := s.insertInput(ctx, InputQueueRow{ID: fmt.Sprintf("seed-r-%s-%d", conv.ID, i), ConversationID: conv.ID, UserEmail: resumeUser,
					ClientInputID: fmt.Sprintf("approval-resume:seed-%d", i), Message: "x", Attachments: "[]", Mode: InputModeResume, State: InputStateCompleted}); err != nil {
					t.Fatal(err)
				}
			}
			for i := range tc.seedQueued {
				if _, _, err := s.EnqueueInput(ctx, InputQueueRow{ID: fmt.Sprintf("seed-q-%s-%d", conv.ID, i), ConversationID: conv.ID, UserEmail: resumeUser,
					ClientInputID: fmt.Sprintf("q-%d", i), Message: "x", Attachments: "[]", Mode: InputModeQueued}); err != nil {
					t.Fatal(err)
				}
			}
			a := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
			approveWithResult(t, s, a.ID, false)
			res, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{ConversationID: conv.ID, MaxPerHour: tc.maxPerHour, MaxPending: tc.maxPending})
			if err != nil || res.Input != nil || res.Skipped != tc.want || len(res.Claimed) != 1 {
				t.Fatalf("result = %+v, %v; want skipped %q", res, err, tc.want)
			}
			if got := resumeState(t, s, a.ID); got != ApprovalResumeSkipped {
				t.Fatalf("resume_state = %q, want skipped", got)
			}
			ns := notices(t, s, conv.ID)
			if len(ns) != 1 || ns[0].Kind != NoticeApprovalResumeSkipped || !strings.Contains(ns[0].Text, tc.noteWant) {
				t.Fatalf("notices = %+v, want one skip note containing %q", ns, tc.noteWant)
			}
		})
	}
}

// A deleted conversation is never resumed: its settled armed cards are
// dropped, no row is inserted.
func TestClaimApprovalResume_DeletedConversation(t *testing.T) {
	s := newTestStore(t)
	conv := resumeConv(t, s)
	a := stageResume(t, s, conv.ID, "mcp_deals_update_deal", true)
	approveWithResult(t, s, a.ID, false)
	if err := s.Delete(context.Background(), resumeUser, conv.ID); err != nil {
		t.Fatal(err)
	}
	res := claimResume(t, s, conv.ID)
	if res.Input != nil || res.Skipped != ApprovalResumeSkipConversationGone {
		t.Fatalf("result = %+v, want conversation_gone", res)
	}
	if n := len(resumeRows(t, s, conv.ID)); n != 0 {
		t.Fatalf("%d resume rows for a deleted conversation", n)
	}
}

// Cards that did not opt in, and a superseded armed card, never resume.
func TestClaimApprovalResume_UnarmedAndSupersededNeverResume(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := resumeConv(t, s)
	plain := stageResume(t, s, conv.ID, "mcp_deals_update_deal", false)
	approveWithResult(t, s, plain.ID, false)
	if res := claimResume(t, s, conv.ID); res.Input != nil || len(res.Claimed) != 0 {
		t.Fatalf("an unarmed card resumed: %+v", res)
	}
	old := stageResume(t, s, conv.ID, "mcp_deals_create_deal", true)
	if _, err := s.SupersedePendingApprovals(ctx, conv.ID, "mcp_deals_create_deal"); err != nil {
		t.Fatal(err)
	}
	if got := resumeState(t, s, old.ID); got != ApprovalResumeSuperseded {
		t.Fatalf("superseded card resume_state = %q", got)
	}
	if res := claimResume(t, s, conv.ID); res.Input != nil {
		t.Fatalf("a superseded card resumed: %+v", res)
	}
}

// The restart rule: a due resume that never started (armed settled card, or
// a queued resume row) is dropped with one note per conversation; a pending
// armed card stays armed; nothing is re-run and a second boot notes nothing.
func TestDropApprovalResumesAtBoot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convA := resumeConv(t, s)
	due := stageResume(t, s, convA.ID, "mcp_deals_update_deal", true)
	approveWithResult(t, s, due.ID, false)
	stillPending := stageResume(t, s, convA.ID, "mcp_deals_create_deal", true)

	convB := resumeConv(t, s)
	b := stageResume(t, s, convB.ID, "mcp_deals_update_deal", true)
	approveWithResult(t, s, b.ID, false)
	if res := claimResume(t, s, convB.ID); res.Input == nil {
		t.Fatalf("setup: no resume row queued: %+v", res)
	}

	noted, err := s.DropApprovalResumesAtBoot(ctx)
	if err != nil || noted != 2 {
		t.Fatalf("DropApprovalResumesAtBoot = %d, %v; want 2 conversations noted", noted, err)
	}
	if got := resumeState(t, s, due.ID); got != ApprovalResumeDropped {
		t.Fatalf("due card = %q, want dropped", got)
	}
	if got := resumeState(t, s, stillPending.ID); got != ApprovalResumeArmed {
		t.Fatalf("pending card = %q, want still armed", got)
	}
	rows := resumeRows(t, s, convB.ID)
	if len(rows) != 1 || rows[0].State != InputStateCancelled {
		t.Fatalf("resume rows = %+v, want the queued one cancelled", rows)
	}
	for _, c := range []string{convA.ID, convB.ID} {
		ns := notices(t, s, c)
		if len(ns) != 1 || ns[0].Kind != NoticeApprovalResumeDropped || !strings.Contains(ns[0].Text, "fleet restarted") {
			t.Fatalf("conv %s notices = %+v", c, ns)
		}
	}
	if noted, err := s.DropApprovalResumesAtBoot(ctx); err != nil || noted != 0 {
		t.Fatalf("second boot = %d, %v; want nothing", noted, err)
	}
}
