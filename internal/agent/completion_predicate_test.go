package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Tests for the deterministic completion predicate and the verifier-outage
// fail-open (#1602).

// withFastVerifierRetry removes the pause before the verifier's one retry.
func withFastVerifierRetry(t *testing.T) {
	t.Helper()
	old := verifierRetryDelay
	verifierRetryDelay = 0
	t.Cleanup(func() { verifierRetryDelay = old })
}

func hasSessionMessageType(session *LogSession, messageType string) bool {
	for _, m := range session.SnapshotMessages() {
		if m.MessageType != nil && *m.MessageType == messageType {
			return true
		}
	}
	return false
}

func sessionContains(session *LogSession, text string) bool {
	for _, m := range session.SnapshotMessages() {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

// scriptedVerifier is the end-of-run verifier (or the reviewer): each Generate
// takes the next reply, repeating the last. A reply of "ERR" fails the call
// like a provider timeout.
type scriptedVerifier struct {
	itMockModel
	replies []string
	calls   int
}

func (m *scriptedVerifier) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	reply := m.replies[min(m.calls, len(m.replies)-1)]
	m.calls++
	if reply == "ERR" {
		return nil, context.DeadlineExceeded
	}
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: reply}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// pagesBroker answers the Pages tools; failing names return an MCP tool error.
type pagesBroker struct {
	failing map[string]bool
	// payloadError names tools that answer a transport-successful payload
	// carrying a top-level "error" (no isError flag).
	payloadError map[string]bool
	calls        map[string]int
}

func (b *pagesBroker) CallMCP(_ context.Context, _, tool string, _ map[string]any) (string, bool, error) {
	b.calls[tool]++
	if b.failing[tool] {
		return "upstream 500", true, nil
	}
	if b.payloadError[tool] {
		return `{"error":"upstream 400"}`, false, nil
	}
	return `{"ok":true,"checked_at":"2026-09-22T06:00:00Z","reason":"source_not_updated"}`, false, nil
}

// eventRecorder captures observer events from the run's stream sink.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
	loads  []map[string]any
}

func (r *eventRecorder) Observe(eventType string, payload map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType)
	r.loads = append(r.loads, payload)
}

const cleanAudit = `{"success":true,"critical_actions":[],"reasoning":"Checked the source","artifacts_checked":["source.csv"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`

// scriptedRun drives a scheduled Execute through the given tool steps, then a
// closing answer on every later call.
func scriptedRun(t *testing.T, steps []struct{ tool, input string }, verifier, reviewer fantasy.LanguageModel, broker *pagesBroker, predicate []string) (*Agent, *eventRecorder, int, error) {
	t.Helper()
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			if step < len(steps) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: fmt.Sprint(step), ToolCallName: steps[step].tool, ToolCallInput: steps[step].input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Source not updated; refresh check recorded."})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	if verifier != nil {
		a.fallbackModel = verifier
	}
	if reviewer != nil {
		a.phoneAFriendEnabled = true
		a.reviewerModel = reviewer
	}
	a.mcpBroker = broker
	a.mcpCatalog = []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "record_refresh_check", Description: "Record that a refresh ran and why the page did not change"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data_upload", Description: "Publish a page's data from a staged upload"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data", Description: "Publish a page's data inline"}},
	}
	if len(predicate) > 0 {
		a.completionAnySucceeded = map[string]bool{}
		for _, name := range predicate {
			a.completionAnySucceeded[name] = true
		}
	}
	rec := &eventRecorder{}
	err := a.Execute(agentcore.WithStreamObserver(context.Background(), rec), "Refresh the page; when the source has no new coverage, record a refresh check instead of publishing.")
	return a, rec, calls, err
}

var pagesPredicate = []string{"mcp_pages_update_page_data", "mcp_pages_update_page_data_upload", "mcp_pages_record_refresh_check"}

// The issue's acceptance: a Pages refresh whose declared completion tool
// (record_refresh_check, the no-update branch) succeeded finishes with no
// verifier and no phone-a-friend call, a breadcrumb and an event naming the
// tool, and no verifier entry in aux_usage.
func TestScheduledCompletionPredicateSkipsTheModelGates(t *testing.T) {
	verifier := &scriptedVerifier{replies: []string{`{"missing_actions":["publish update_page_data"]}`}}
	reviewer := &scriptedVerifier{replies: []string{`{"needs_revision":true,"issues":["x"]}`}}
	broker := &pagesBroker{calls: map[string]int{}}
	a, rec, calls, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", cleanAudit},
		{"mcp_pages_record_refresh_check", `{"slug":"page-a","reason":"source_not_updated"}`},
	}, verifier, reviewer, broker, pagesPredicate)
	if err != nil {
		t.Fatalf("a run whose declared completion tool succeeded must finish, got %v", err)
	}
	if verifier.calls != 0 || reviewer.calls != 0 {
		t.Fatalf("model gates ran: verifier=%d reviewer=%d, want 0 and 0", verifier.calls, reviewer.calls)
	}
	if calls != 3 {
		t.Fatalf("model calls = %d, want 3 (the first finish accepted)", calls)
	}
	if !sessionContains(a.logSession, "[completion_predicate] satisfied by mcp_pages_record_refresh_check") ||
		!hasSessionMessageType(a.logSession, messageTypeCompletionPredicate) {
		t.Fatal("missing the [completion_predicate] breadcrumb")
	}
	for _, r := range a.logSession.SnapshotAuxUsage() {
		if r.Label == agentcore.AuxUsageEndOfRunVerifier {
			t.Fatalf("aux_usage has a verifier entry on the predicate path: %+v", r)
		}
	}
	found := false
	for i, e := range rec.events {
		if e == evtCompletionPredicate && rec.loads[i]["tool"] == "mcp_pages_record_refresh_check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing the %s event naming the tool, events=%v", evtCompletionPredicate, rec.events)
	}
}

// A declared predicate none of whose tools succeeded changes nothing: the
// verifier runs exactly as before, whether the listed tool failed or was never
// called, and a run with no clause is not touched by it.
func TestScheduledCompletionPredicateUnsatisfiedStillVerifies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		steps     []struct{ tool, input string }
		predicate []string
	}{
		{"listed tool failed", []struct{ tool, input string }{{"confirm_audit", cleanAudit}, {"mcp_pages_record_refresh_check", `{"slug":"x"}`}}, pagesPredicate},
		{"listed tool never called", []struct{ tool, input string }{{"confirm_audit", cleanAudit}}, pagesPredicate},
		{"listed tool returned an error payload", []struct{ tool, input string }{{"confirm_audit", cleanAudit}, {"mcp_pages_record_refresh_check", `{"slug":"x"}`}}, pagesPredicate},
		{"no clause", []struct{ tool, input string }{{"confirm_audit", cleanAudit}, {"mcp_pages_record_refresh_check", `{"slug":"x"}`}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier := &scriptedVerifier{replies: []string{`{"missing_actions":[]}`}}
			broker := &pagesBroker{calls: map[string]int{}, failing: map[string]bool{"record_refresh_check": tc.name == "listed tool failed"},
				payloadError: map[string]bool{"record_refresh_check": tc.name == "listed tool returned an error payload"}}
			a, _, _, err := scriptedRun(t, tc.steps, verifier, nil, broker, tc.predicate)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if verifier.calls != 1 {
				t.Fatalf("verifier calls = %d, want the ordinary single check", verifier.calls)
			}
			if hasSessionMessageType(a.logSession, messageTypeCompletionPredicate) {
				t.Fatal("the predicate claimed a run none of its tools completed")
			}
		})
	}
}

// The predicate replaces the model verifier, never the audit gate: a declared
// commitment that was not executed still blocks finishing, even though the
// completion tool succeeded — and the verifier is never consulted meanwhile.
func TestScheduledCompletionPredicateDoesNotOverrideTheAudit(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	verifier := &scriptedVerifier{replies: []string{`{"missing_actions":[]}`}}
	broker := &pagesBroker{calls: map[string]int{}}
	a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", `{"success":true,"critical_actions":[{"tool":"mcp_pages_update_page_data"}],"reasoning":"Ready to publish","artifacts_checked":["payload.json"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`},
		{"mcp_pages_record_refresh_check", `{"slug":"x"}`},
	}, verifier, nil, broker, pagesPredicate)
	if !errors.Is(err, agentcore.ErrMaxEnforcementRounds) {
		t.Fatalf("an outstanding declared commitment must keep blocking the finish, got %v", err)
	}
	if !sessionContains(a.logSession, "have not successfully executed") {
		t.Fatal("the audit's outstanding-commitment enforcement never fired")
	}
	if verifier.calls != 0 || hasSessionMessageType(a.logSession, messageTypeCompletionPredicate) {
		t.Fatalf("predicate/verifier consulted before the audit cleared (verifier=%d)", verifier.calls)
	}
}

// publishAudit authorizes the one critical publish the fail-open tests land.
const publishAudit = `{"success":true,"critical_actions":[{"tool":"mcp_pages_update_page_data"}],"reasoning":"Ready to publish","artifacts_checked":["payload.json"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`

// A verifier that cannot answer is retried once; if it still cannot, a run
// whose audit cleared and whose critical publish landed succeeds with the
// completion_unverified_verifier_error warning instead of dead-lettering. One
// good answer on the retry is an ordinary verdict and leaves no warning.
func TestScheduledVerifierOutageAfterCleanAuditFinishesWithWarning(t *testing.T) {
	withFastVerifierRetry(t)
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	for _, tc := range []struct {
		name        string
		replies     []string
		wantCalls   int
		wantWarning bool
	}{
		{"timeout twice", []string{"ERR"}, 2, true},
		{"answers on the retry", []string{"ERR", `{"missing_actions":[]}`}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier := &scriptedVerifier{replies: tc.replies}
			a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
				{"confirm_audit", publishAudit},
				{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
			}, verifier, nil, &pagesBroker{calls: map[string]int{}}, nil)
			if err != nil {
				t.Fatalf("a verifier outage after a clean audit must not fail the run, got %v", err)
			}
			if verifier.calls != tc.wantCalls {
				t.Fatalf("verifier calls = %d, want %d (one retry)", verifier.calls, tc.wantCalls)
			}
			if got := hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError); got != tc.wantWarning {
				t.Fatalf("warning recorded = %t, want %t", got, tc.wantWarning)
			}
		})
	}
}

// The fail-open needs BOTH attempts to have run and both to be outages: a
// malformed first verdict (a content failure) followed by a transport error
// must keep the spend-a-check path, however clean the rest of the run is.
func TestScheduledVerifierMalformedThenOutageStillSpendsChecks(t *testing.T) {
	withFastVerifierRetry(t)
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	verifier := &scriptedVerifier{replies: []string{"the page is incomplete", "ERR", "the page is incomplete", "ERR", "the page is incomplete", "ERR"}}
	a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", publishAudit},
		{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
	}, verifier, nil, &pagesBroker{calls: map[string]int{}}, nil)
	if !errors.Is(err, agentcore.ErrCompletionUnverified) {
		t.Fatalf("want ErrCompletionUnverified, got %v", err)
	}
	if hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError) {
		t.Fatal("a malformed-then-outage check must not fail open")
	}
}

// The audit alone is the model grading itself: a run that executed no
// critical tool — a refresh that wrongly decided there was nothing to do —
// has no failed critical call either, so a verifier outage must keep the
// spend-a-check path rather than record it as a success.
func TestScheduledVerifierOutageWithNoCriticalCallStillDeadLetters(t *testing.T) {
	withFastVerifierRetry(t)
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	verifier := &scriptedVerifier{replies: []string{"ERR"}}
	a, _, _, err := scriptedRun(t, []struct{ tool, input string }{{"confirm_audit", cleanAudit}}, verifier, nil, &pagesBroker{calls: map[string]int{}}, nil)
	if !errors.Is(err, agentcore.ErrCompletionUnverified) {
		t.Fatalf("want ErrCompletionUnverified, got %v", err)
	}
	if verifier.calls != 2*maxCompletionVerifications {
		t.Fatalf("verifier calls = %d, want %d (three checks, each retried once)", verifier.calls, 2*maxCompletionVerifications)
	}
	if hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError) {
		t.Fatal("a run that dead-lettered must not carry the success warning")
	}
}

// With a failed critical call on the record the outcome is in doubt, so a
// verifier outage keeps the pre-#1602 semantics: each double failure spends a
// check and the third ends the run ErrCompletionUnverified.
func TestScheduledVerifierOutageWithFailedCriticalCallStillDeadLetters(t *testing.T) {
	withFastVerifierRetry(t)
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	verifier := &scriptedVerifier{replies: []string{"ERR"}}
	broker := &pagesBroker{calls: map[string]int{}}
	// critical_actions=[] authorizes no critical call: the publish is BLOCKED,
	// a failed critical call that nothing later superseded.
	a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", cleanAudit},
		{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
	}, verifier, nil, broker, nil)
	if !errors.Is(err, agentcore.ErrCompletionUnverified) {
		t.Fatalf("want ErrCompletionUnverified, got %v", err)
	}
	if verifier.calls != 2*maxCompletionVerifications {
		t.Fatalf("verifier calls = %d, want %d (three checks, each retried once)", verifier.calls, 2*maxCompletionVerifications)
	}
	if hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError) {
		t.Fatal("a run that dead-lettered must not carry the success warning")
	}
}

func TestFailedCriticalCalls(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	records := []toolExecRecord{
		{Name: "mcp_pages_update_page_data", Succeeded: false}, // stale version …
		{Name: "mcp_pages_update_page_data", Succeeded: true},  // … retried and landed
		{Name: "mcp_other_update_page_data", Succeeded: false}, // never recovered
		{Name: "mcp_pages_get_page_data", Succeeded: false},    // not critical
	}
	if got := failedCriticalCalls(records); len(got) != 1 || got[0] != "mcp_other_update_page_data" {
		t.Fatalf("failedCriticalCalls = %v, want only the unrecovered critical tool", got)
	}
}

// A verifier outage fails open only where an audit actually ran in this policy
// (#1602 follow-up). A delegated policy skips the self-audit ritual, so finish
// enforcement clearing proves nothing: a sub-agent's outage keeps the
// spend-a-check path and ends ErrCompletionUnverified. (Children do not run
// this gate today — the driver skips the wrapper for them — so this pins the
// premise at the policy seam, where a future change would reach it.)
func TestVerifierOutageNeedsThisPolicysAuditToFailOpen(t *testing.T) {
	withFastVerifierRetry(t)
	verifier := &scriptedVerifier{replies: []string{"ERR"}}
	a := newTestScheduledAgent(t, &itMockModel{})
	a.fallbackModel = verifier
	p := &scheduledPolicy{
		inner:  agentcore.NewDelegatedPolicy(a.logSession, 50, 0, 0),
		agent:  a,
		task:   "delegated work",
		runCtx: context.Background(),
	}
	for check := 1; check <= maxCompletionVerifications; check++ {
		if ok, _ := p.CanFinish(check); ok {
			t.Fatalf("check %d: a run that never audited failed open on a verifier outage", check)
		}
	}
	if p.verifierWarning != "" || !errors.Is(p.TerminalError(), agentcore.ErrCompletionUnverified) {
		t.Fatalf("want ErrCompletionUnverified and no warning, got err=%v warning=%q", p.TerminalError(), p.verifierWarning)
	}
	if verifier.calls != 2*maxCompletionVerifications {
		t.Fatalf("verifier calls = %d, want %d (each check retried once, then spent)", verifier.calls, 2*maxCompletionVerifications)
	}
}

// Malformed verdicts — an empty reply included — are content failures: even
// after a clean audit whose critical publish landed they never fail open — a
// degraded verifier model must not become auto-success.
func TestScheduledMalformedVerdictStillSpendsChecks(t *testing.T) {
	withFastVerifierRetry(t)
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	for _, reply := range []string{`{"reasoning":"not published"}`, "the page is incomplete", ""} {
		verifier := &scriptedVerifier{replies: []string{reply}}
		a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
			{"confirm_audit", publishAudit},
			{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
		}, verifier, nil, &pagesBroker{calls: map[string]int{}}, nil)
		if !errors.Is(err, agentcore.ErrCompletionUnverified) {
			t.Fatalf("%q: want ErrCompletionUnverified, got %v", reply, err)
		}
		if hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError) {
			t.Fatalf("%q: a malformed verdict must not produce the fail-open warning", reply)
		}
	}
}

// pagesTwinPolicy installs the Pages write twins as critical and, when
// aliased, as one action (critical_tool_aliases, #1604).
func pagesTwinPolicy(t *testing.T, aliased bool) {
	t.Helper()
	p := agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_page_data", "update_page_data_upload"}}
	if aliased {
		p.CriticalToolAliases = map[string][]string{"update_page_data": {"update_page_data_upload"}}
	}
	agentcore.ConfigureAgentPolicy(p)
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
}

// A failed critical action is not superseded by a landed action whose name
// merely ends in the same text: mcp_x_bulk_create_deal and
// mcp_x_bulk_create_deal_upload (class create_deal on prefix mcp_x_bulk) are two
// actions, so the verifier outage must not fail open over the failure.
func TestFailedCriticalCallsDoesNotCollideAcrossThePrefixBoundary(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{"create_deal", "create_deal_upload", "bulk_create_deal"},
		CriticalToolAliases:  map[string][]string{"create_deal": {"create_deal_upload"}},
	})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	records := []toolExecRecord{
		{Name: "mcp_x_bulk_create_deal", Succeeded: false},
		{Name: "mcp_x_bulk_create_deal_upload", Succeeded: true},
	}
	if got := failedCriticalCalls(records); fmt.Sprint(got) != "[mcp_x_bulk_create_deal]" {
		t.Fatalf("failedCriticalCalls = %v, want [mcp_x_bulk_create_deal]", got)
	}
}

// A failed inline write superseded by a successful upload of the same data on
// the same server is ONE critical action that landed: with the twins aliased,
// a verifier outage fails open exactly like any clean audited publish.
func TestScheduledVerifierOutageAfterUploadTwinSupersedesFailedInline(t *testing.T) {
	withFastVerifierRetry(t)
	pagesTwinPolicy(t, true)
	verifier := &scriptedVerifier{replies: []string{"ERR"}}
	broker := &pagesBroker{calls: map[string]int{}, failing: map[string]bool{"update_page_data": true}}
	a, _, _, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", publishAudit},
		{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
		{"mcp_pages_update_page_data_upload", `{"slug":"x","upload_id":"u-1"}`},
	}, verifier, nil, broker, nil)
	if err != nil {
		t.Fatalf("an audited publish that landed through the upload twin must fail open on a verifier outage, got %v", err)
	}
	if broker.calls["update_page_data"] != 1 || broker.calls["update_page_data_upload"] != 1 {
		t.Fatalf("writes = %v, want the failed inline attempt and the landed upload", broker.calls)
	}
	if verifier.calls != 2 || !hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError) {
		t.Fatalf("want the outage retried once and the fail-open warning (verifier calls=%d)", verifier.calls)
	}
}

// failedCriticalCalls judges the LAST attempt at each critical action: an alias
// twin on the same server supersedes a failure, the same twin on another
// server or client-variant seat does not, and without aliases the two
// spellings stay two actions (the pre-#1604 behaviour).
func TestFailedCriticalCallsKeysByAliasClass(t *testing.T) {
	inlineFailed := toolExecRecord{Name: "mcp_pages_update_page_data", Succeeded: false}
	for _, tc := range []struct {
		name    string
		aliased bool
		then    toolExecRecord
		want    []string
	}{
		{"same-server twin supersedes", true, toolExecRecord{Name: "mcp_pages_update_page_data_upload", Succeeded: true}, nil},
		{"cross-server twin does not", true, toolExecRecord{Name: "mcp_pagesb_update_page_data_upload", Succeeded: true}, []string{"mcp_pages_update_page_data"}},
		{"client-variant twin does not", true, toolExecRecord{Name: "mcp_pages_client2_update_page_data_upload", Succeeded: true}, []string{"mcp_pages_update_page_data"}},
		{"no aliases: two actions", false, toolExecRecord{Name: "mcp_pages_update_page_data_upload", Succeeded: true}, []string{"mcp_pages_update_page_data"}},
		{"last attempt is reported by its own name", true, toolExecRecord{Name: "mcp_pages_update_page_data_upload", Succeeded: false}, []string{"mcp_pages_update_page_data_upload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pagesTwinPolicy(t, tc.aliased)
			records := []toolExecRecord{inlineFailed, tc.then}
			if got := failedCriticalCalls(records); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("failedCriticalCalls = %v, want %v", got, tc.want)
			}
			if !succeededCriticalCall(records) && tc.then.Succeeded {
				t.Fatal("a landed twin must count as a succeeded critical call")
			}
		})
	}
}
