package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/store"
)

func toolResult(t *testing.T, text string, isErr bool) agent.HistoryEntry {
	t.Helper()
	raw, err := json.Marshal(agent.ToolResultContent{ID: "c", Name: "bash", Text: text, IsErr: isErr})
	if err != nil {
		t.Fatal(err)
	}
	return agent.HistoryEntry{Role: "tool", Type: "tool_result", Content: raw}
}

// The free signals read the turn's own tool results: the longest run of
// failures, loop-guard refusals, the call count, plus the compactions the
// stream counted. The strongest one names the card's reason.
func TestStruggleSignals(t *testing.T) {
	ok := toolResult(t, "fine", false)
	fail := toolResult(t, "error: no such file", true)
	loop := toolResult(t, "LOOP_GUARD (block #1): this exact call…", true)

	cases := []struct {
		name        string
		history     []agent.HistoryEntry
		compactions int
		want        string
	}{
		{"clean turn", []agent.HistoryEntry{ok, ok, fail, ok}, 0, ""},
		{"two failures are routine", []agent.HistoryEntry{fail, fail, ok, fail}, 0, ""},
		{"three failures in a row", []agent.HistoryEntry{ok, fail, fail, fail}, 0, escalationSignalToolErrors},
		{"loop guard wins", []agent.HistoryEntry{fail, fail, fail, loop}, 0, escalationSignalLoopGuard},
		{"compaction", []agent.HistoryEntry{ok}, 1, escalationSignalCompaction},
		{"long turn", repeat(ok, escalationLongTurnToolCalls), 0, escalationSignalLongTurn},
		{"just under long", repeat(ok, escalationLongTurnToolCalls-1), 0, ""},
		// Calls held for the user's approval are not failures, and do not
		// break a real streak either.
		{"approvals are not failures", repeat(toolResult(t, "APPROVAL_REQUIRED: send email — staged for user approval", true), 3), 0, ""},
		{"session-policy denials are not failures", repeat(toolResult(t, "APPROVAL_DENIED: the user pre-denied bash for this conversation (session policy).", true), 3), 0, ""},
		{"held behind another approval is not a failure", repeat(toolResult(t, "APPROVAL_BLOCKED: send_email already has a critical action awaiting the user's approval", true), 3), 0, ""},
		{"approval inside a streak", []agent.HistoryEntry{fail, fail, toolResult(t, "APPROVAL_REQUIRED: x", true), fail}, 0, escalationSignalToolErrors},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			signal, reason := struggleFromHistory(c.history, c.compactions).strongest()
			if signal != c.want {
				t.Fatalf("signal = %q, want %q", signal, c.want)
			}
			if (signal == "") != (reason == "") {
				t.Errorf("reason %q does not match signal %q", reason, signal)
			}
		})
	}
}

func repeat(e agent.HistoryEntry, n int) []agent.HistoryEntry {
	out := make([]agent.HistoryEntry, n)
	for i := range out {
		out[i] = e
	}
	return out
}

// The classifier only runs when there is an earlier exchange to push back
// on, the user typed the message, the conversation is not lockdown, a key
// exists, and the mode is not off.
func TestStartStruggleCheckGates(t *testing.T) {
	var calls int
	prev := checkStruggle
	checkStruggle = func(_ context.Context, _, model, conversation string) (agentcore.StruggleVerdict, error) {
		calls++
		if model != "acme/decider" || !strings.HasSuffix(conversation, "User: still wrong") || !strings.Contains(conversation, "Assistant: the total") {
			t.Errorf("model %q conversation %q", model, conversation)
		}
		return agentcore.StruggleVerdict{Probability: 0.9}, nil
	}
	t.Cleanup(func() { checkStruggle = prev })

	s := &Server{cfg: &config.Config{
		OpenRouterAPIKey:              "sk-or-test",
		EscalationClassifierMode:      config.EscalationClassifierObserve,
		EscalationClassifierModel:     "acme/decider",
		EscalationClassifierThreshold: 75,
	}}
	conv := &store.Conversation{ID: "c1"}
	history := []agent.HistoryEntry{textEntry("user", "revenue by month"), textEntry("assistant", "the total is $1M")}

	c := s.startStruggleCheck(conv, history, "still wrong", "")
	if c == nil {
		t.Fatal("expected a check")
	}
	if v, err := c.wait(); err != nil || v.Probability != 0.9 || c.threshold != 0.75 {
		t.Fatalf("verdict %+v err %v threshold %v", v, err, c.threshold)
	}

	for name, run := range map[string]func() *struggleCheck{
		"first message": func() *struggleCheck { return s.startStruggleCheck(conv, nil, "hi", "") },
		"fleet's own turn": func() *struggleCheck {
			return s.startStruggleCheck(conv, history, "resume", agent.InputKindApprovalResume)
		},
		"lockdown": func() *struggleCheck {
			return s.startStruggleCheck(&store.Conversation{Lockdown: true}, history, "x", "")
		},
		"lockdown-only deployment, chat created before it": func() *struggleCheck {
			s.cfg.LockdownOnly = true
			defer func() { s.cfg.LockdownOnly = false }()
			return s.startStruggleCheck(conv, history, "x", "")
		},
	} {
		if run() != nil {
			t.Errorf("%s: want no check", name)
		}
	}
	s.cfg.EscalationClassifierMode = config.EscalationClassifierOff
	if s.startStruggleCheck(conv, history, "x", "") != nil {
		t.Error("mode off: want no check")
	}
	s.cfg.EscalationClassifierMode = config.EscalationClassifierSuggest
	s.cfg.OpenRouterAPIKey = ""
	if s.startStruggleCheck(conv, history, "x", "") != nil {
		t.Error("no key: want no check")
	}
	if calls != 1 {
		t.Errorf("classifier called %d times, want 1", calls)
	}
}

func doneCheck(mode string, p float64, err error) *struggleCheck {
	c := &struggleCheck{mode: mode, threshold: 0.8, done: make(chan struct{}), verdict: agentcore.StruggleVerdict{Probability: p}, err: err}
	close(c.done)
	return c
}

func escalationEvents(t *testing.T, s *Server, convID string) []store.EscalationEvent {
	t.Helper()
	st, ok := s.store.(*store.Store)
	if !ok {
		t.Fatalf("fixture store is %T", s.store)
	}
	ev, err := st.EscalationEvents(context.Background(), convID)
	if err != nil {
		t.Fatalf("EscalationEvents: %v", err)
	}
	return ev
}

func pendingSuggestion(t *testing.T, s *Server, user, convID string) map[string]any {
	t.Helper()
	pend, err := s.store.ListPendingApprovals(context.Background(), user, convID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	for _, a := range pend {
		if a.ToolName == "suggest_advanced_model" {
			var args map[string]any
			_ = json.Unmarshal([]byte(a.ArgsJSON), &args)
			return args
		}
	}
	return nil
}

// A struggling turn on the everyday model stages one card with the signal's
// reason, and the struggle is recorded for the user; a classifier "yes" in
// the same turn is recorded too, but does not stage a second card.
func TestEscalateAfterTurn_SignalStagesAndRecords(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	s.cfg.EscalationSignalsEnabled = true
	s.cfg.EscalationClassifierMode = config.EscalationClassifierSuggest
	const user = "struggler@x.com"
	conv := seedConv(t, s, user) // on "openrouter/auto", not the advanced tier
	buf := newTurnBuffer(conv.ID, "turn-1")
	fail := toolResult(t, "boom", true)
	res := &TurnResult{Model: "openrouter/auto", NewHistory: []agent.HistoryEntry{fail, fail, fail}}

	s.escalateAfterTurn(context.Background(), buf, conv, user, nil, res, doneCheck(config.EscalationClassifierSuggest, 0.95, nil))

	args := pendingSuggestion(t, s, user, conv.ID)
	if args == nil || args["source"] != suggestionSourceSignal || !strings.Contains(args["reason"].(string), "3 tool calls in a row failed") {
		t.Fatalf("staged card args = %v", args)
	}
	if args["recommend_model"] != agentcore.CurrentAdvancedModel() {
		t.Errorf("recommend_model = %v", args["recommend_model"])
	}
	ev := escalationEvents(t, s, conv.ID)
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want a signal and a classifier row", ev)
	}
	if ev[0].Source != "signal" || ev[0].Signal != escalationSignalToolErrors || !ev[0].Suggested || ev[0].Probability != nil {
		t.Errorf("signal row = %+v", ev[0])
	}
	if ev[1].Source != "classifier" || ev[1].Signal != escalationSignalFrustration || ev[1].Suggested || ev[1].Probability == nil || *ev[1].Probability != 0.95 {
		t.Errorf("classifier row = %+v", ev[1])
	}

	// The per-user counts reach the admin Users page.
	stats, err := s.store.AdminStats(context.Background())
	if err != nil {
		t.Fatalf("AdminStats: %v", err)
	}
	found := false
	for _, r := range stats {
		if r.Email == user {
			found = true
			if r.FrustratedMessages != 1 || r.StruggleSignals != 1 {
				t.Errorf("admin row = %+v", r)
			}
		}
	}
	if !found {
		t.Error("user missing from AdminStats")
	}
}

// Observe mode records the classifier's "yes" without showing a card; a "no"
// and an error record nothing.
func TestEscalateAfterTurn_ClassifierModes(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	const user = "observer@x.com"
	conv := seedConv(t, s, user)
	res := &TurnResult{Model: "openrouter/auto"}

	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, nil, res, doneCheck(config.EscalationClassifierObserve, 0.92, nil))
	if pendingSuggestion(t, s, user, conv.ID) != nil {
		t.Fatal("observe mode must not stage a card")
	}
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t2"), conv, user, nil, res, doneCheck(config.EscalationClassifierSuggest, 0.4, nil))
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t3"), conv, user, nil, res, doneCheck(config.EscalationClassifierSuggest, 0, errors.New("HTTP 502")))
	if ev := escalationEvents(t, s, conv.ID); len(ev) != 1 || ev[0].Source != "classifier" || ev[0].Suggested {
		t.Fatalf("events = %+v, want only the observed yes", ev)
	}
	if pendingSuggestion(t, s, user, conv.ID) != nil {
		t.Fatal("a no or an error must not stage a card")
	}

	// Suggest mode stages a classifier card.
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t4"), conv, user, nil, res, doneCheck(config.EscalationClassifierSuggest, 0.81, nil))
	if args := pendingSuggestion(t, s, user, conv.ID); args == nil || args["source"] != suggestionSourceClassifier || args["reason"] != escalationClassifierReason {
		t.Fatalf("classifier card = %v", args)
	}
}

// On the advanced model the struggle is still recorded, but no card offers
// the model the user is already on.
func TestEscalateAfterTurn_AdvancedModelRecordsOnly(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	s.cfg.EscalationSignalsEnabled = true
	const user = "advanced@x.com"
	conv := seedConv(t, s, user)
	if err := s.store.SetModel(context.Background(), user, conv.ID, agentcore.CurrentAdvancedModel()); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	loop := toolResult(t, "LOOP_GUARD (block #1)", true)
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, nil, &TurnResult{NewHistory: []agent.HistoryEntry{loop}}, nil)
	if pendingSuggestion(t, s, user, conv.ID) != nil {
		t.Fatal("no card on the advanced model")
	}
	if ev := escalationEvents(t, s, conv.ID); len(ev) != 1 || ev[0].Signal != escalationSignalLoopGuard || ev[0].Suggested {
		t.Fatalf("events = %+v", ev)
	}
}

// The admin picker endpoint passes the decision-model list through, and
// degrades to an empty list (with a message) when OpenRouter is unreachable.
func TestHandleAdminDecisionModels(t *testing.T) {
	prev := listDecisionModels
	t.Cleanup(func() { listDecisionModels = prev })
	s := &Server{}

	listDecisionModels = func(context.Context) ([]agentcore.DecisionModel, error) {
		return []agentcore.DecisionModel{{ID: "typesafe/jev-1.14", Name: "TypeSafe: Jev", InputPerMillion: 0.042}}, nil
	}
	rec := httptest.NewRecorder()
	s.handleAdminDecisionModels(rec, httptest.NewRequest(http.MethodGet, "/admin/decision-models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"typesafe/jev-1.14"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	listDecisionModels = func(context.Context) ([]agentcore.DecisionModel, error) { return nil, errors.New("dial tcp: refused") }
	rec = httptest.NewRecorder()
	s.handleAdminDecisionModels(rec, httptest.NewRequest(http.MethodGet, "/admin/decision-models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"models":[]`) || strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// A classifier answer that is late must not hold the finished turn's stream
// open (the client blocks the next message until it closes): past the grace
// the result is recorded in the background, without a card.
func TestEscalateAfterTurn_LateClassifierRecordsWithoutCard(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	const user = "late@x.com"
	conv := seedConv(t, s, user)
	late := &struggleCheck{mode: config.EscalationClassifierSuggest, threshold: 0.8, done: make(chan struct{}),
		verdict: agentcore.StruggleVerdict{Probability: 0.97}}
	go func() {
		time.Sleep(struggleCheckGrace + 500*time.Millisecond)
		close(late.done)
	}()

	start := time.Now()
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, nil, &TurnResult{}, late)
	if waited := time.Since(start); waited > struggleCheckGrace+300*time.Millisecond {
		t.Fatalf("escalateAfterTurn held the stream for %v, past the %v grace", waited, struggleCheckGrace)
	}
	s.background.StopAndWait() // let the late check finish and record
	if pendingSuggestion(t, s, user, conv.ID) != nil {
		t.Fatal("a late answer must not stage a card the closed stream cannot show")
	}
	if ev := escalationEvents(t, s, conv.ID); len(ev) != 1 || ev[0].Source != "classifier" || ev[0].Suggested {
		t.Fatalf("events = %+v, want the late frustration recorded without a card", ev)
	}
}

// A correction the user steers in while the agent works is classified too,
// with the turn so far as context.
func TestEscalateAfterTurn_ClassifiesMidTurnSteer(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	s.cfg.OpenRouterAPIKey = "sk-or-test"
	s.cfg.EscalationClassifierMode = config.EscalationClassifierSuggest
	s.cfg.EscalationClassifierModel = "acme/decider"
	s.cfg.EscalationClassifierThreshold = 80
	var sent string
	prev := checkStruggle
	checkStruggle = func(_ context.Context, _, _, conversation string) (agentcore.StruggleVerdict, error) {
		sent = conversation
		return agentcore.StruggleVerdict{Probability: 0.96}, nil
	}
	t.Cleanup(func() { checkStruggle = prev })

	const user = "steer@x.com"
	conv := seedConv(t, s, user)
	history := []agent.HistoryEntry{textEntry("user", "revenue by month"), textEntry("assistant", "Total is $1M.")}
	turn := []agent.HistoryEntry{
		textEntry("user", "by month please"),
		textEntry("assistant", "Working on the total"),
		textEntry("user", "no, stop, I said BY MONTH"),
	}
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, history, &TurnResult{NewHistory: turn}, nil)

	if !strings.HasSuffix(sent, "User: no, stop, I said BY MONTH") || !strings.Contains(sent, "Assistant: Working on the total") {
		t.Fatalf("classified conversation = %q, want it to end with the steer", sent)
	}
	if args := pendingSuggestion(t, s, user, conv.ID); args == nil || args["source"] != suggestionSourceClassifier {
		t.Fatalf("card = %v, want a classifier card", args)
	}
}

// counterValue reads one series from the metrics registry (0 when absent).
func counterValue(t *testing.T, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(metrics.Render(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return f
		}
	}
	return 0
}

// Every request still running at the grace deadline counts as late, and each
// is counted once by its eventual outcome.
func TestEscalateAfterTurn_CountsEachLateCheck(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	s.cfg.OpenRouterAPIKey = "sk-or-test"
	s.cfg.EscalationClassifierMode = config.EscalationClassifierObserve
	s.cfg.EscalationClassifierModel = "acme/decider"
	s.cfg.EscalationClassifierThreshold = 80
	release := make(chan struct{})
	prev := checkStruggle
	checkStruggle = func(context.Context, string, string, string) (agentcore.StruggleVerdict, error) {
		<-release
		return agentcore.StruggleVerdict{Probability: 0.1}, nil
	}
	t.Cleanup(func() { checkStruggle = prev })

	const user = "twolate@x.com"
	conv := seedConv(t, s, user)
	history := []agent.HistoryEntry{textEntry("user", "revenue by month"), textEntry("assistant", "Total is $1M.")}
	opening := s.startStruggleCheck(conv, history, "by month please", "")
	turn := []agent.HistoryEntry{textEntry("user", "by month please"), textEntry("assistant", "Working"), textEntry("user", "no, BY MONTH")}

	lateSeries := `fleet_escalation_classifier_late_total{mode="observe"}`
	okSeries := `fleet_escalation_classifier_checks_total{mode="observe",outcome="ok"}`
	lateBefore, okBefore := counterValue(t, lateSeries), counterValue(t, okSeries)
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, history, &TurnResult{NewHistory: turn}, opening)
	if got := counterValue(t, lateSeries) - lateBefore; got != 2 {
		t.Errorf("late counted %v times, want 2 (both requests missed the grace)", got)
	}
	close(release)
	s.background.StopAndWait()
	if got := counterValue(t, okSeries) - okBefore; got != 2 {
		t.Errorf("outcomes counted %v times, want 2", got)
	}
}

// A stopped turn shows no card and records nothing, but the classifier
// request it already sent is still counted by its outcome.
func TestEscalateAfterTurn_CancelledTurnCountsCheck(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	const user = "stopped@x.com"
	conv := seedConv(t, s, user)
	series := `fleet_escalation_classifier_checks_total{mode="suggest",outcome="pushback"}`
	before := counterValue(t, series)
	check := doneCheck(config.EscalationClassifierSuggest, 0.97, nil)
	s.escalateAfterTurn(context.Background(), newTurnBuffer(conv.ID, "t1"), conv, user, nil, &TurnResult{Cancelled: true}, check)
	s.background.StopAndWait()
	if got := counterValue(t, series) - before; got != 1 {
		t.Errorf("cancelled turn's check counted %v times, want 1", got)
	}
	if pendingSuggestion(t, s, user, conv.ID) != nil || len(escalationEvents(t, s, conv.ID)) != 0 {
		t.Fatal("a stopped turn must not stage a card or record a struggle")
	}
}

// failingEngine fails every turn before it produces a result, by error or,
// with panics set, by panicking.
type failingEngine struct {
	fakeEngine
	panics bool
}

func (f *failingEngine) RunTurn(context.Context, TurnInput, agent.EventSink) (*TurnResult, error) {
	if f.panics {
		panic("engine bug (escalation-test)")
	}
	return nil, errors.New("provider unavailable (escalation-test)")
}

// A turn that fails before producing a result, by error or by a recovered
// panic, shows no card, but the classifier request already sent alongside it
// is still counted.
func TestFailedTurnCountsStartedClassifierCheck(t *testing.T) {
	prev := checkStruggle
	checkStruggle = func(context.Context, string, string, string) (agentcore.StruggleVerdict, error) {
		return agentcore.StruggleVerdict{Probability: 0.97}, nil
	}
	t.Cleanup(func() { checkStruggle = prev })

	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panics], func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &failingEngine{panics: panics}, st)
			srv.cfg.OpenRouterAPIKey = "sk-or-test"
			srv.cfg.EscalationClassifierMode = config.EscalationClassifierObserve
			srv.cfg.EscalationClassifierModel = "acme/decider"
			srv.cfg.EscalationClassifierThreshold = 80
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			if _, err := st.AppendHistory(context.Background(), conv.ID, []agent.HistoryEntry{
				textEntry("user", "revenue by month"), textEntry("assistant", "Total is $1M."),
			}); err != nil {
				t.Fatal(err)
			}

			series := `fleet_escalation_classifier_checks_total{mode="observe",outcome="pushback"}`
			before := counterValue(t, series)
			postChatRequest(t, srv, map[string]any{"message": "still wrong", "conversation_id": conv.ID})
			srv.background.StopAndWait()
			if got := counterValue(t, series) - before; got != 1 {
				t.Fatalf("failed turn's check counted %v times, want 1", got)
			}
		})
	}
}

// A long turn that staged a tool approval card (e.g. schedule_task) records
// the struggle signal for admin stats without stacking a suggest_advanced_model
// card underneath the pending tool approval card.
func TestEscalateAfterTurn_SuppressesCardWhenTurnStagedApproval(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeTurnEngine{}
	s.cfg.EscalationSignalsEnabled = true
	const user = "jeanne@x.com"
	conv := seedConv(t, s, user)
	buf := newTurnBuffer(conv.ID, "turn-long-approval")
	history := append(repeat(toolResult(t, "ok", false), escalationLongTurnToolCalls),
		toolResult(t, "APPROVAL_REQUIRED: schedule_task — staged for user approval (approval_id=a1)", true))
	res := &TurnResult{Model: "openrouter/auto", NewHistory: history}

	s.escalateAfterTurn(context.Background(), buf, conv, user, nil, res, nil)

	if pendingSuggestion(t, s, user, conv.ID) != nil {
		t.Fatal("must not stage a suggest_advanced_model card alongside a staged tool approval card")
	}
	ev := escalationEvents(t, s, conv.ID)
	if len(ev) != 1 || ev[0].Signal != escalationSignalLongTurn || ev[0].Suggested {
		t.Fatalf("events = %+v, want one recorded long_turn signal with Suggested=false", ev)
	}
}
