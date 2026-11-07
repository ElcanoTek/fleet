package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// The approval-scan tests pin approvalCols/scanApproval: every full approval
// reader (GetApproval, ListPendingApprovals, ListResolvedApprovals,
// ListExpiredApprovals, LatestApprovalByTool) returns the same struct for the
// same row, with every staging- and execution-time column filled — so no
// caller depends on which reader happened to load it.

const scanTestUser = "scan@example.com"

var scanTestSeat = ApprovalSeat{Server: "mail", Account: "work"}

func pickApproval(t *testing.T, list []Approval, id string) *Approval {
	t.Helper()
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	t.Fatalf("approval %s missing from listing", id)
	return nil
}

func sameApproval(t *testing.T, label string, got, want *Approval) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s differs from GetApproval:\n got %+v\nwant %+v", label, got, want)
	}
}

func mustSet(t *testing.T, sets map[string]func() (bool, error)) {
	t.Helper()
	for name, set := range sets {
		if ok, err := set(); err != nil || !ok {
			t.Fatalf("set %s: ok=%v err=%v", name, ok, err)
		}
	}
}

func scanTestConversation(t *testing.T) (*Store, string) {
	t.Helper()
	s := newTestStore(t)
	conv, err := s.CreateConversation(context.Background(), scanTestUser, "t", "victoria", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	return s, conv.ID
}

// A pending card past its deadline, carrying every staging-time column.
func TestApprovalReadersScanIdenticalStructs_Pending(t *testing.T) {
	t.Parallel()

	s, convID := scanTestConversation(t)
	ctx := context.Background()
	const user = scanTestUser
	a, err := s.CreateApproval(ctx, convID, user, "send_email", "call-1",
		`{"to":["a@example.com","b@example.com"]}`, time.Now().Add(-time.Minute).Unix(), scanTestSeat)
	if err != nil {
		t.Fatal(err)
	}
	mustSet(t, map[string]func() (bool, error){
		"card":       func() (bool, error) { return s.SetApprovalCard(ctx, user, a.ID, `{"title":"Send"}`) },
		"narrowable": func() (bool, error) { return s.SetApprovalNarrowableArg(ctx, user, a.ID, "to") },
		"group":      func() (bool, error) { return s.SetApprovalGroup(ctx, user, a.ID, "turn-1") },
		"resume":     func() (bool, error) { return s.ArmApprovalResume(ctx, user, a.ID) },
	})
	want, err := s.GetApproval(ctx, user, a.ID)
	if err != nil || want == nil {
		t.Fatalf("GetApproval: %v %v", want, err)
	}
	if want.CardJSON == "" || want.NarrowableArg != "to" || want.GroupID != "turn-1" || want.ResumeState != ApprovalResumeArmed || want.MCPAccount != "work" {
		t.Fatalf("staging columns not read back: %+v", want)
	}
	list, err := s.ListPendingApprovals(ctx, user, convID)
	if err != nil {
		t.Fatal(err)
	}
	sameApproval(t, "ListPendingApprovals", pickApproval(t, list, a.ID), want)
	expired, err := s.ListExpiredApprovals(ctx, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	sameApproval(t, "ListExpiredApprovals", pickApproval(t, expired, a.ID), want)
	latest, err := s.LatestApprovalByTool(ctx, convID, "send_email")
	if err != nil {
		t.Fatal(err)
	}
	sameApproval(t, "LatestApprovalByTool", latest, want)
}

// A settled card carrying every execution-time column: approved narrowed,
// progress, stop requested, result card and outcome.
func TestApprovalReadersScanIdenticalStructs_Settled(t *testing.T) {
	t.Parallel()

	s, convID := scanTestConversation(t)
	ctx := context.Background()
	const user = scanTestUser
	a, err := s.CreateApproval(ctx, convID, user, "send_email", "call-2", `{"to":["a@example.com"]}`, 0, scanTestSeat)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimApprovalNarrowed(ctx, user, a.ID, ApprovalExecutingSentinel, `{"arg":"to","staged":["a@example.com"],"approved":["a@example.com"]}`); err != nil || !ok {
		t.Fatalf("claim narrowed: %v %v", ok, err)
	}
	mustSet(t, map[string]func() (bool, error){
		"progress":    func() (bool, error) { return s.SetApprovalProgress(ctx, user, a.ID, `{"done":1}`) },
		"stop":        func() (bool, error) { return s.MarkApprovalStopRequested(ctx, user, a.ID) },
		"result card": func() (bool, error) { return s.SetApprovalResultCard(ctx, user, a.ID, `{"title":"Sent"}`) },
	})
	if err := s.SetApprovalResult(ctx, user, a.ID, "sent", false); err != nil {
		t.Fatal(err)
	}
	want, err := s.GetApproval(ctx, user, a.ID)
	if err != nil || want == nil {
		t.Fatalf("GetApproval: %v %v", want, err)
	}
	if want.NarrowedJSON == "" || want.ProgressJSON == "" || want.ResultCardJSON == "" || want.StopRequestedAt == 0 || !want.IsErr.Valid {
		t.Fatalf("execution columns not read back: %+v", want)
	}
	resolved, err := s.ListResolvedApprovals(ctx, user, convID)
	if err != nil {
		t.Fatal(err)
	}
	sameApproval(t, "ListResolvedApprovals", pickApproval(t, resolved, a.ID), want)
	latest, err := s.LatestApprovalByTool(ctx, convID, "send_email")
	if err != nil {
		t.Fatal(err)
	}
	sameApproval(t, "LatestApprovalByTool", latest, want)
}
