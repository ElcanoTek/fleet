package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover approval result cards (docs/APPROVAL-RESULT-CARDS.md,
// ADR-0084): a tool in critical_tool_result_cards may return a `result_card`
// next to its own result; fleet checks it strictly, stores it on the row, and
// serves it only on a success it recorded. Every other tool, and every
// failure, keeps today's settled card.

func useResultCardPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:    []string{"execute_plan", "next_campaign_id", "deploy_page"},
		CriticalToolModes:       map[string]string{"next_campaign_id": agentcore.ApprovalModeNotify, "deploy_page": agentcore.ApprovalModeNotify},
		CriticalToolResultCards: []string{"execute_plan", "next_campaign_id"},
	})
}

// A created deal, verified, as the deal-plan bundle reports it.
const goodResultCard = `{
  "outcome": "done",
  "title": "Created 1 Index Exchange deal · verified",
  "items": [{
    "key": "Elcano_ChatTest_IX_BidSwitch_Display_US",
    "label": "Elcano_ChatTest_IX_BidSwitch_Display_US",
    "id": "IX151710246087067804",
    "internal_id": "666624",
    "link": "https://app.example.com/deals/666624",
    "link_label": "Open in Index Exchange",
    "status": "done",
    "note": "Verified: the deal reads back as created."
  }]
}`

// A plan with a failure: the failed create is offered for retry.
const failedResultCard = `{
  "outcome": "partial",
  "title": "1 of 2 deals created · 1 failed",
  "items": [
    {"key": "A", "label": "Deal A", "id": "IX1", "status": "done"},
    {"key": "B", "label": "Deal B", "status": "failed", "note": "the SSP refused the floor; nothing was created."}
  ],
  "retry": {"keys": ["B"]}
}`

func envelopeWith(card string) string {
	return `{"success": true, "counts": {"created": 1}, "results": [{"deal_id": "A", "status": "created"}], "result_card": ` + card + `}`
}

func TestParseApprovalResultCard_AcceptsTheSchema(t *testing.T) {
	for name, raw := range map[string]string{"done": goodResultCard, "partial with retry": failedResultCard} {
		card, canonical, err := parseApprovalResultCard([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if card.Title == "" || len(canonical) == 0 || strings.Contains(string(canonical), "\n") {
			t.Fatalf("%s: canonical = %s", name, canonical)
		}
		if _, again, err := parseApprovalResultCard(canonical); err != nil || string(again) != string(canonical) {
			t.Fatalf("%s: canonical form does not round-trip: %v", name, err)
		}
	}
}

func TestParseApprovalResultCard_Refusals(t *testing.T) {
	item := func(extra string) string {
		return `{"outcome":"partial","title":"t","items":[{"key":"A","label":"A","status":"done"},{"key":"B","label":"B","status":"failed"}` + extra + `]}`
	}
	cases := map[string]string{
		"unknown top key":         `{"outcome":"done","title":"t","items":[],"extra":1}`,
		"unknown item key":        `{"outcome":"done","title":"t","items":[{"label":"A","status":"done","colour":"red"}]}`,
		"null":                    `{"outcome":"done","title":"t","items":[],"footer":null}`,
		"no items":                `{"outcome":"done","title":"t"}`,
		"no title":                `{"outcome":"done","title":" ","items":[]}`,
		"bad outcome":             `{"outcome":"great","title":"t","items":[]}`,
		"bad status":              `{"outcome":"partial","title":"t","items":[{"label":"A","status":"created"}]}`,
		"no status":               `{"outcome":"partial","title":"t","items":[{"label":"A"}]}`,
		"done with a failed row":  `{"outcome":"done","title":"t","items":[{"label":"A","status":"failed"}]}`,
		"done with unclear":       `{"outcome":"done","title":"t","items":[{"label":"A","status":"unclear"}]}`,
		"done with mismatch":      `{"outcome":"done","title":"t","items":[{"label":"A","status":"mismatch"}]}`,
		"failed with applied row": `{"outcome":"failed","title":"t","items":[{"label":"A","status":"done"}]}`,
		"duplicate key":           `{"outcome":"done","title":"t","items":[{"key":"A","label":"A","status":"done"},{"key":"A","label":"A2","status":"done"}]}`,
		"link label, no link":     `{"outcome":"done","title":"t","items":[{"label":"A","status":"done","link_label":"Open"}]}`,
		"http link":               `{"outcome":"done","title":"t","items":[{"label":"A","status":"done","link":"http://x.example.com"}]}`,
		"control char":            `{"outcome":"done","title":"t\u0007","items":[]}`,
		"bidi char":               `{"outcome":"done","title":"t\u202e","items":[]}`,
		"trailing data":           `{"outcome":"done","title":"t","items":[]} {}`,
		"empty retry":             `{"outcome":"partial","title":"t","items":[{"key":"B","label":"B","status":"failed"}],"retry":{"keys":[]}}`,
		"retry unknown key":       strings.TrimSuffix(item(""), "}") + `,"retry":{"keys":["C"]}}`,
		"retry a done row":        strings.TrimSuffix(item(""), "}") + `,"retry":{"keys":["A"]}}`,
		"retry repeats a key":     strings.TrimSuffix(item(""), "}") + `,"retry":{"keys":["B","B"]}}`,
		"retry an unclear row":    `{"outcome":"unclear","title":"t","items":[{"key":"U","label":"U","status":"unclear"}],"retry":{"keys":["U"]}}`,
		"retry a mismatched row":  `{"outcome":"mismatch","title":"t","items":[{"key":"M","label":"M","status":"mismatch"}],"retry":{"keys":["M"]}}`,
		"done with needs_check":   `{"outcome":"done","title":"t","items":[{"label":"A","status":"needs_check"}]}`,
		"failed with needs_check": `{"outcome":"failed","title":"t","items":[{"label":"A","status":"needs_check"}]}`,
		"retry a needs_check row": `{"outcome":"mismatch","title":"t","items":[{"key":"N","label":"N","status":"needs_check"}],"retry":{"keys":["N"]}}`,
		"bad flag code":           `{"outcome":"done","title":"t","items":[{"label":"A","status":"done","flags":[{"code":"Bad Code","label":"x"}]}]}`,
		"change without after":    `{"outcome":"done","title":"t","items":[{"label":"A","status":"done","changes":[{"label":"Floor","before":"$1"}]}]}`,
	}
	for name, raw := range cases {
		if _, _, err := parseApprovalResultCard([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	// A record that was applied but needs a check (a follow-up step failed)
	// is its own status, on a warning or partial card.
	for _, outcome := range []string{"mismatch", "partial"} {
		nc := `{"outcome":"` + outcome + `","title":"t","items":[{"label":"Deal A","status":"needs_check","note":"The IAB exclusion step failed."}]}`
		if _, _, err := parseApprovalResultCard([]byte(nc)); err != nil {
			t.Fatalf("needs_check on a %s card refused: %v", outcome, err)
		}
	}
	// A not_run row may be retried, like a failed one.
	ok := `{"outcome":"partial","title":"t","items":[{"key":"A","label":"A","status":"done"},{"key":"N","label":"N","status":"not_run"}],"retry":{"keys":["N"]}}`
	if _, _, err := parseApprovalResultCard([]byte(ok)); err != nil {
		t.Fatalf("not_run retry refused: %v", err)
	}
}

func TestExtractResultCard(t *testing.T) {
	if card, ok, reason := extractResultCard(envelopeWith(goodResultCard)); !ok || reason != "" || !strings.Contains(string(card), "IX151710246087067804") {
		t.Fatalf("envelope: ok=%v reason=%q card=%s", ok, reason, card)
	}
	// No card at all is the normal case for any other result: no reason.
	for _, text := range []string{"", "created 3 deals", `["a"]`, `{"success": true}`, `{"broken": `} {
		if _, ok, reason := extractResultCard(text); ok || reason != "" {
			t.Errorf("%q: ok=%v reason=%q, want no card and no reason", text, ok, reason)
		}
	}
	if _, ok, reason := extractResultCard(envelopeWith(`{"outcome":"done"}`)); ok || reason == "" {
		t.Fatalf("an off-schema card must be refused with a reason, got ok=%v reason=%q", ok, reason)
	}
	secret := strings.Replace(goodResultCard, "Verified: the deal reads back as created.", "token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", 1)
	if _, ok, reason := extractResultCard(envelopeWith(secret)); ok || reason == "" {
		t.Fatalf("a secret-like value must withhold the card, got ok=%v reason=%q", ok, reason)
	}
	// The card is read from governed text, where the redactor has already
	// replaced the secret: a card carrying the redactor's marker is refused too
	// (Codex on #1722).
	for _, marker := range []string{"token [REDACTED]", "[REDACTED PRIVATE KEY]"} {
		scrubbed := strings.Replace(goodResultCard, "Verified: the deal reads back as created.", marker, 1)
		if _, ok, reason := extractResultCard(envelopeWith(scrubbed)); ok || reason == "" {
			t.Fatalf("%q: a redacted card must be withheld, got ok=%v reason=%q", marker, ok, reason)
		}
	}
	// A post_tool_use hook's context after the tool's own JSON (the notify
	// path) does not hide the card.
	if _, ok, reason := extractResultCard(envelopeWith(goodResultCard) + "\n\n[hook] remember to check the deal sheet"); !ok || reason != "" {
		t.Fatalf("trailing hook context: ok=%v reason=%q, want the card", ok, reason)
	}
}

// End to end through the governance the approval path applies: a secret in
// the card is scrubbed by the redactor, and the scrubbed card is not stored.
func TestExecuteClaimedApproval_RedactedCardIsNotStored(t *testing.T) {
	useResultCardPolicy(t)
	secret := strings.Replace(goodResultCard, "Verified: the deal reads back as created.", "token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", 1)
	st := &resultCardStore{groupStore: newGroupStore(store.Approval{ID: "ap1", ConversationID: "c1", ToolName: "mcp_deals_execute_plan", ArgsJSON: `{}`, Status: "approved", ResultText: approvalExecutingSentinel})}
	s := resultCardServer(st, envelopeWith(secret), false)
	a := st.state("ap1")
	out := s.executeClaimedApproval(context.Background(), "u@example.com", "c1", "ap1", &a, approvalRequest{Approved: true})
	if out["result_card"] != nil || len(st.cards) != 0 {
		t.Fatalf("reply = %+v, writes = %v; a scrubbed card must not be stored", out, st.cards)
	}
	if strings.Contains(out["result_text"].(string), "ghp_") {
		t.Fatal("the model-visible result must be the redacted one")
	}
}

// resultCardStore is a groupStore that records SetApprovalResultCard writes.
type resultCardStore struct {
	*groupStore
	cmu   sync.Mutex
	cards map[string]string
	err   error
}

func (r *resultCardStore) SetApprovalResultCard(_ context.Context, _, id, card string) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	r.cmu.Lock()
	defer r.cmu.Unlock()
	if r.cards == nil {
		r.cards = map[string]string{}
	}
	r.cards[id] = card
	r.mu.Lock()
	if a := r.approvals[id]; a != nil {
		a.ResultCardJSON = card
	}
	r.mu.Unlock()
	return true, nil
}

func resultCardServer(st chatStore, text string, isErr bool) *Server {
	return &Server{store: st, agent: &approvalEngine{
		fakeEngine: &fakeEngine{},
		broker:     &fakeMCPBroker{text: text, isError: isErr},
		catalog: []mcp.ServerTool{
			{ServerName: "deals", Tool: mcp.Tool{Name: "execute_plan"}},
			{ServerName: "deals", Tool: mcp.Tool{Name: "update_plan"}},
		},
	}}
}

// An approved call of an opted-in tool: its result's card is stored on the
// row before the outcome, rides the reply, and is served on every read.
func TestExecuteClaimedApproval_StoresTheResultCard(t *testing.T) {
	useResultCardPolicy(t)
	st := &resultCardStore{groupStore: newGroupStore(store.Approval{ID: "ap1", ConversationID: "c1", ToolName: "mcp_deals_execute_plan", ArgsJSON: `{}`, Status: "approved", ResultText: approvalExecutingSentinel})}
	s := resultCardServer(st, envelopeWith(goodResultCard), false)
	a := st.state("ap1")
	out := s.executeClaimedApproval(context.Background(), "u@example.com", "c1", "ap1", &a, approvalRequest{Approved: true})
	if out["is_err"] != false {
		t.Fatalf("outcome = %+v", out)
	}
	card, ok := out["result_card"].(json.RawMessage)
	if !ok || !strings.Contains(string(card), `"outcome":"done"`) {
		t.Fatalf("reply result_card = %v, want the canonical card", out["result_card"])
	}
	if st.cards["ap1"] != string(card) {
		t.Fatalf("stored card = %q, want the reply's", st.cards["ap1"])
	}
	// The model-visible result is the tool's own text, card and all.
	if !strings.Contains(out["result_text"].(string), `"result_card"`) {
		t.Fatalf("result_text lost the tool's own answer: %v", out["result_text"])
	}
	settled := st.state("ap1")
	if got := s.approvalClientState(&settled)["result_card"]; got == nil {
		t.Fatal("a settled row with a stored card must serve it")
	}
}

// A tool that did not opt in, an errored call, and a result without a card
// all settle exactly as before: no write, no result_card key.
func TestExecuteClaimedApproval_NoResultCardOtherwise(t *testing.T) {
	useResultCardPolicy(t)
	cases := []struct {
		name, tool, text string
		isErr            bool
	}{
		{"not opted in", "mcp_deals_update_plan", envelopeWith(goodResultCard), false},
		{"tool error", "mcp_deals_execute_plan", envelopeWith(goodResultCard), true},
		{"no card", "mcp_deals_execute_plan", `{"success": true}`, false},
		{"bad card", "mcp_deals_execute_plan", envelopeWith(`{"outcome":"done","title":"t","items":[{"label":"A","status":"failed"}]}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &resultCardStore{groupStore: newGroupStore(store.Approval{ID: "ap1", ConversationID: "c1", ToolName: tc.tool, ArgsJSON: `{}`, Status: "approved", ResultText: approvalExecutingSentinel})}
			s := resultCardServer(st, tc.text, tc.isErr)
			a := st.state("ap1")
			out := s.executeClaimedApproval(context.Background(), "u@example.com", "c1", "ap1", &a, approvalRequest{Approved: true})
			if _, has := out["result_card"]; has || len(st.cards) != 0 {
				t.Fatalf("reply = %+v, writes = %v; want no card", out, st.cards)
			}
			settled := st.state("ap1")
			if _, has := s.approvalClientState(&settled)["result_card"]; has {
				t.Fatal("no result_card key may be served")
			}
		})
	}
}

// A failed store write never fails the approval: the outcome is recorded and
// the reply carries no card.
func TestExecuteClaimedApproval_ResultCardStoreErrorKeepsTheOutcome(t *testing.T) {
	useResultCardPolicy(t)
	st := &resultCardStore{groupStore: newGroupStore(store.Approval{ID: "ap1", ConversationID: "c1", ToolName: "mcp_deals_execute_plan", ArgsJSON: `{}`, Status: "approved", ResultText: approvalExecutingSentinel}), err: sql.ErrConnDone}
	s := resultCardServer(st, envelopeWith(goodResultCard), false)
	a := st.state("ap1")
	out := s.executeClaimedApproval(context.Background(), "u@example.com", "c1", "ap1", &a, approvalRequest{Approved: true})
	if out["is_err"] != false || out["result_card"] != nil {
		t.Fatalf("outcome = %+v, want a recorded success with no card", out)
	}
}

// The stored card is served only on a success fleet recorded, and is
// re-validated on every read.
func TestApprovalResultCardPayload_OnlyOnARecordedSuccess(t *testing.T) {
	_, canonical, err := parseApprovalResultCard([]byte(goodResultCard))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	base := store.Approval{ID: "ap1", ToolName: "mcp_deals_execute_plan", Status: "approved", ResultCardJSON: string(canonical), ResultText: "{}"}
	ok := base
	ok.IsErr = sql.NullBool{Valid: true, Bool: false}
	if s.approvalResultCardPayload(&ok) == nil {
		t.Fatal("a recorded success must serve its card")
	}
	failed := ok
	failed.IsErr = sql.NullBool{Valid: true, Bool: true}
	executing := base
	executing.ResultText = approvalExecutingSentinel
	unknown := base // approved, no is_err, no sentinel: outcome not recorded
	declined := ok
	declined.Status = "rejected"
	malformed := ok
	malformed.ResultCardJSON = `{"outcome":"done","title":"t","items":[{"label":"A","status":"failed"}]}`
	none := ok
	none.ResultCardJSON = ""
	for name, a := range map[string]store.Approval{"failed": failed, "executing": executing, "unknown": unknown, "declined": declined, "malformed": malformed, "none": none} {
		if s.approvalResultCardPayload(&a) != nil {
			t.Errorf("%s: served a card", name)
		}
	}
	// A notify record (approved, is_err false, the record text) serves it.
	record := ok
	record.ResultText = notifyRecordResultPrefix + ": this tool is declared notify-mode in the client bundle."
	if s.approvalResultCardPayload(&record) == nil {
		t.Fatal("a notify record must serve its card")
	}
}

// recordStageStore is a stageStore whose RecordAction rows resolve and can
// take a result card.
type recordStageStore struct {
	stageStore
	cards map[string]string
}

func (s *recordStageStore) ResolveApproval(context.Context, string, string, string, string) error {
	return nil
}

func (s *recordStageStore) SetApprovalResultCard(_ context.Context, _, id, card string) (bool, error) {
	if s.cards == nil {
		s.cards = map[string]string{}
	}
	s.cards[id] = card
	return true, nil
}

// A notify-mode tool in critical_tool_result_cards: once its call returns,
// the record stores the card the result carried and pushes it to the live
// card. A failed call, an undeclared tool or an unknown call id changes
// nothing.
func TestRecordActionResult_StoresTheRecordsCard(t *testing.T) {
	useResultCardPolicy(t)
	allocated := `{"campaign_id": "ELC07375", "result_card": {"outcome": "done", "title": "Campaign ID ELC07375 allocated", "items": []}}`

	st := &recordStageStore{}
	sink := &eventSink{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: sink}
	if err := a.RecordAction("mcp_deal_sheet_next_campaign_id", "call-1", `{"idempotency_key":"booking:abc"}`, ""); err != nil {
		t.Fatal(err)
	}
	a.RecordActionResult("mcp_deal_sheet_next_campaign_id", "call-unknown", allocated, true)
	if len(st.cards) != 0 {
		t.Fatalf("an unknown call id stored %v", st.cards)
	}
	a.RecordActionResult("mcp_deal_sheet_next_campaign_id", "call-1", allocated, true)
	if !strings.Contains(st.cards["ap-new"], "ELC07375 allocated") {
		t.Fatalf("stored cards = %v, want the record's card", st.cards)
	}
	ev := eventNamed(sink, "tool.action_record_card")
	if ev == nil || ev["approval_id"] != "ap-new" || ev["result_card"] == nil {
		t.Fatalf("live event = %v, want the record's card", ev)
	}
	// The call id is spent: a second result is not stored again.
	delete(st.cards, "ap-new")
	a.RecordActionResult("mcp_deal_sheet_next_campaign_id", "call-1", allocated, true)
	if len(st.cards) != 0 {
		t.Fatal("a record takes its result once")
	}

	// A failed call leaves the record as written.
	st2 := &recordStageStore{}
	a2 := &approvalStager{ctx: context.Background(), store: st2, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	_ = a2.RecordAction("mcp_deal_sheet_next_campaign_id", "call-2", `{}`, "")
	a2.RecordActionResult("mcp_deal_sheet_next_campaign_id", "call-2", allocated, false)
	if len(st2.cards) != 0 {
		t.Fatalf("a failed call stored %v", st2.cards)
	}

	// A notify tool that did not opt in is never tracked.
	st3 := &recordStageStore{}
	a3 := &approvalStager{ctx: context.Background(), store: st3, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	_ = a3.RecordAction("mcp_pages_deploy_page", "call-3", `{}`, "")
	a3.RecordActionResult("mcp_pages_deploy_page", "call-3", allocated, true)
	if len(st3.cards) != 0 || len(a3.recordIDs) != 0 {
		t.Fatalf("an undeclared tool stored %v (tracked %v)", st3.cards, a3.recordIDs)
	}
}

// A notify-mode tool runs without an approval, so its record never offers
// "Retry failed": the retry is left out when the record stores its card, and
// a stored record card with a retry is not served (Codex P1 on #1722).
func TestRecordActionResult_NeverOffersARetry(t *testing.T) {
	useResultCardPolicy(t)
	withRetry := `{"ok": false, "result_card": {"outcome": "partial", "title": "1 of 2 allocated", "items": [` +
		`{"key": "a", "label": "A", "status": "done"}, {"key": "b", "label": "B", "status": "failed"}], "retry": {"keys": ["b"]}}}`
	st := &recordStageStore{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	_ = a.RecordAction("mcp_deal_sheet_next_campaign_id", "call-1", `{}`, "")
	a.RecordActionResult("mcp_deal_sheet_next_campaign_id", "call-1", withRetry, true)
	stored := st.cards["ap-new"]
	if stored == "" || strings.Contains(stored, "retry") || !strings.Contains(stored, "1 of 2 allocated") {
		t.Fatalf("stored = %q, want the card without its retry", stored)
	}
	_, withRetryCard, err := parseApprovalResultCard([]byte(`{"outcome":"partial","title":"t","items":[{"key":"b","label":"B","status":"failed"}],"retry":{"keys":["b"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	record := store.Approval{Status: "approved", IsErr: sql.NullBool{Valid: true}, ResultText: notifyRecordResultPrefix + ": x", ResultCardJSON: string(withRetryCard)}
	if (&Server{}).approvalResultCardPayload(&record) != nil {
		t.Fatal("a record card with a retry must not be served")
	}
	// An approved call's card keeps its retry: the retry is staged for approval.
	approved := record
	approved.ResultText = "{}"
	if (&Server{}).approvalResultCardPayload(&approved) == nil {
		t.Fatal("an approved call's card with a retry must be served")
	}
}

// withoutRetry keeps the 4 MiB scan bound: an oversized result is returned
// unchanged (and then ignored by extractResultCard), never re-wrapped into a
// small card that would slip past the bound (Codex on #1722).
func TestWithoutRetry_KeepsTheScanBound(t *testing.T) {
	big := `{"pad": "` + strings.Repeat("x", approvalResultMaxScanBytes) + `", "result_card": ` + failedResultCard + `}`
	if got := withoutRetry(big); got != big {
		t.Fatal("an oversized result must come back unchanged")
	}
	if _, ok, _ := extractResultCard(withoutRetry(big)); ok {
		t.Fatal("an oversized result must not yield a card")
	}
	if out := withoutRetry(envelopeWith(failedResultCard)); strings.Contains(out, `"retry"`) || !strings.Contains(out, "1 of 2 deals created") {
		t.Fatalf("withoutRetry = %s, want the card without its retry", out)
	}
}
