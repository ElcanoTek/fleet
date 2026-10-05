package agent

import (
	"strings"
	"testing"
)

// pagesBlockedRule is the Pages refresh producer's blocked_when, resolved.
var pagesBlockedRule = &CompletionBlockedWhen{
	Tools:          []string{"mcp_pages_record_refresh_check"},
	Argument:       "outcome",
	In:             []string{"blocked", "failed", "source_unreachable"},
	DetailArgument: "detail",
}

func blockedRun(t *testing.T, steps []struct{ tool, input string }, broker *pagesBroker) (*Agent, error) {
	t.Helper()
	verifier := &scriptedVerifier{replies: []string{`{"missing_actions":[]}`}}
	// scriptedRun installs the predicate; the rule rides the same Options
	// field in production (NewAgent builds both).
	saved := testBlockedRule
	testBlockedRule = pagesBlockedRule
	t.Cleanup(func() { testBlockedRule = saved })
	a, _, _, err := scriptedRun(t, steps, verifier, nil, broker, pagesPredicate)
	if verifier.calls != 0 {
		t.Fatalf("a predicate-completed run must not consult the verifier (calls=%d)", verifier.calls)
	}
	return a, err
}

// TestScheduledBlockedOutcomeIsRecorded: a refresh that recorded
// outcome=source_unreachable through the declared completion tool, and
// published nothing, finishes successfully with run outcome "blocked" and the
// call's own detail — no longer an indistinguishable green success.
func TestScheduledBlockedOutcomeIsRecorded(t *testing.T) {
	broker := &pagesBroker{calls: map[string]int{}}
	a, err := blockedRun(t, []struct{ tool, input string }{
		{"confirm_audit", cleanAudit},
		{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"source_unreachable","detail":"Mailbox had no report for 2026-10-04;\n fast_io was unreachable."}`},
	}, broker)
	if err != nil {
		t.Fatalf("a blocked run is a success, got %v", err)
	}
	outcome, detail := a.logSession.SnapshotRunOutcome()
	if outcome != "blocked" {
		t.Fatalf("run outcome = %q, want blocked", outcome)
	}
	if want := "outcome=source_unreachable: Mailbox had no report for 2026-10-04; fast_io was unreachable."; detail != want {
		t.Fatalf("detail = %q, want %q", detail, want)
	}
	if !hasSessionMessageType(a.logSession, messageTypeCompletionBlocked) || !sessionContains(a.logSession, "[completion_blocked] outcome=source_unreachable") {
		t.Fatal("missing the [completion_blocked] breadcrumb")
	}
}

// TestScheduledBlockedRuleDoesNotMatch: a run that published (another
// completion tool succeeded), recorded a non-blocking value, or whose
// recording call failed is an ordinary success with no outcome.
func TestScheduledBlockedRuleDoesNotMatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []struct{ tool, input string }
		broker *pagesBroker
	}{
		{"published as well", []struct{ tool, input string }{
			{"confirm_audit", cleanAudit},
			{"mcp_pages_update_page_data", `{"slug":"page-a"}`},
			{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"blocked"}`},
		}, &pagesBroker{calls: map[string]int{}}},
		{"non-blocking value", []struct{ tool, input string }{
			{"confirm_audit", cleanAudit},
			{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"unchanged"}`},
		}, &pagesBroker{calls: map[string]int{}}},
		{"value in a nested object is not a top-level argument", []struct{ tool, input string }{
			{"confirm_audit", cleanAudit},
			{"mcp_pages_record_refresh_check", `{"slug":"page-a","result":{"outcome":"blocked"}}`},
		}, &pagesBroker{calls: map[string]int{}}},
		{"the last recording call decides", []struct{ tool, input string }{
			{"confirm_audit", cleanAudit},
			{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"blocked"}`},
			{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"unchanged"}`},
		}, &pagesBroker{calls: map[string]int{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := blockedRun(t, tc.steps, tc.broker)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if outcome, _ := a.logSession.SnapshotRunOutcome(); outcome != "" {
				t.Fatalf("run outcome = %q, want none", outcome)
			}
			if hasSessionMessageType(a.logSession, messageTypeCompletionBlocked) {
				t.Fatal("unexpected [completion_blocked] breadcrumb")
			}
		})
	}
}

func TestNewCompletionBlockedRuleNeedsAWholeRule(t *testing.T) {
	for _, opt := range []*CompletionBlockedWhen{nil, {}, {Tools: []string{"x"}, Argument: "outcome"}, {Tools: []string{"x"}, In: []string{"blocked"}}} {
		if rule := newCompletionBlockedRule(opt); rule != nil {
			t.Fatalf("%+v built a rule", opt)
		}
	}
	if rule := newCompletionBlockedRule(pagesBlockedRule); rule == nil || !rule.tools["mcp_pages_record_refresh_check"] || !rule.in["failed"] {
		t.Fatalf("rule = %+v", rule)
	}
	if strings.TrimSpace(runOutcomeBlocked) != "blocked" {
		t.Fatal("runOutcomeBlocked must mirror models.RunOutcomeBlocked")
	}
}
