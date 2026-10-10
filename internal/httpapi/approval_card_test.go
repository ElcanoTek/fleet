package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

const goodCard = `{
  "title": "Update 2 deals",
  "subtitle": "Raise the floor",
  "items": [
    {"label": "Q4 Video", "id": "PM-123", "link": "https://ssp.example.com/deals/123",
     "changes": [{"label": "Floor", "before": "$2.00", "after": "$2.50"}],
     "flags": [{"code": "deal_active", "label": "Deal is Active"}]},
    {"label": "Q4 Display", "settings": [{"label": "Currency", "value": "USD"}],
     "changes": [{"label": "End date", "after": "2027-01-31"}]}
  ],
  "footer": "Changes apply immediately."
}`

func TestParseApprovalCard(t *testing.T) {
	card, canonical, err := parseApprovalCard([]byte(goodCard))
	if err != nil {
		t.Fatalf("a valid card was refused: %v", err)
	}
	if card.Title != "Update 2 deals" || len(card.Items) != 2 || card.Items[0].Changes[0].Before == nil || card.Items[1].Changes[0].Before != nil {
		t.Fatalf("decoded card = %+v", card)
	}
	// Canonical form round-trips through the parser unchanged.
	if _, again, err := parseApprovalCard(canonical); err != nil || string(again) != string(canonical) {
		t.Fatalf("canonical card did not round-trip: %v\n%s\n%s", err, canonical, again)
	}

	refused := map[string]string{
		"empty":                ``,
		"not json":             `deal updated`,
		"array":                `[{"title":"x","items":[]}]`,
		"unknown top key":      `{"title":"x","items":[],"html":"<b>x</b>"}`,
		"unknown item key":     `{"title":"x","items":[{"label":"a","onclick":"x"}]}`,
		"number value":         `{"title":"x","items":[{"label":"a","changes":[{"label":"Floor","after":2.5}]}]}`,
		"missing title":        `{"items":[]}`,
		"blank title":          `{"title":"  ","items":[]}`,
		"missing items":        `{"title":"x"}`,
		"missing item label":   `{"title":"x","items":[{"id":"1"}]}`,
		"missing after":        `{"title":"x","items":[{"label":"a","changes":[{"label":"Floor","before":"1"}]}]}`,
		"http link":            `{"title":"x","items":[{"label":"a","link":"http://ssp.example.com/1"}]}`,
		"javascript link":      `{"title":"x","items":[{"label":"a","link":"javascript:alert(1)"}]}`,
		"credentialed link":    `{"title":"x","items":[{"label":"a","link":"https://u:p@ssp.example.com/1"}]}`,
		"relative link":        `{"title":"x","items":[{"label":"a","link":"/deals/1"}]}`,
		"bad flag code":        `{"title":"x","items":[{"label":"a","flags":[{"code":"Deal Active!","label":"Active"}]}]}`,
		"control character":    `{"title":"x\u0007","items":[]}`,
		"bidi override":        `{"title":"x\u202e","items":[]}`,
		"trailing data":        `{"title":"x","items":[]} {"title":"y","items":[]}`,
		"title over the limit": `{"title":"` + strings.Repeat("a", approvalCardMaxTitle+1) + `","items":[]}`,
	}
	for name, raw := range refused {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseApprovalCard([]byte(raw)); err == nil {
				t.Fatalf("refused card was accepted: %s", raw)
			}
		})
	}

	var many strings.Builder
	many.WriteString(`{"title":"x","items":[`)
	for i := 0; i <= approvalCardMaxItems; i++ {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`{"label":"a"}`)
	}
	many.WriteString(`]}`)
	if _, _, err := parseApprovalCard([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "items") {
		t.Fatalf("%d items accepted: %v", approvalCardMaxItems+1, err)
	}
	if _, _, err := parseApprovalCard([]byte(`{"title":"` + strings.Repeat("a", approvalCardMaxBytes) + `","items":[]}`)); err == nil {
		t.Fatal("an over-size card was accepted")
	}
}

// describerBroker answers the describer with a scripted result and records
// what it was asked; block holds the call until the context ends.
type describerBroker struct {
	mu     sync.Mutex
	text   string
	isErr  bool
	err    error
	block  bool
	calls  []string
	args   map[string]any
	budget time.Duration
}

func (b *describerBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.mu.Lock()
	b.calls = append(b.calls, server+"."+tool)
	b.args = args
	b.budget, _ = mcp.CallTimeout(ctx)
	block := b.block
	b.mu.Unlock()
	if block {
		<-ctx.Done()
		return "", false, ctx.Err()
	}
	return b.text, b.isErr, b.err
}

func (b *describerBroker) called() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// cardStageStore records the card the stager attaches.
type cardStageStore struct {
	stageStore
	card    string
	cardErr error
}

func (s *cardStageStore) SetApprovalCard(_ context.Context, _, _, card string) (bool, error) {
	if s.cardErr != nil {
		return false, s.cardErr
	}
	s.card = card
	return true, nil
}

func describerPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"update_deal", "create_deal"},
		ParallelSafeTools:          []string{"mcp_deals_describe_deal_update"},
		CriticalToolCardDescribers: map[string]string{"update_deal": "describe_deal_update"},
	})
}

var describerCatalog = []mcp.ServerTool{
	{ServerName: "deals", Tool: mcp.Tool{Name: "update_deal"}},
	{ServerName: "deals", Tool: mcp.Tool{Name: "create_deal"}},
	{ServerName: "deals", Tool: mcp.Tool{Name: "describe_deal_update"}},
	{ServerName: "deals_client_a", Tool: mcp.Tool{Name: "update_deal"}},
	{ServerName: "deals_client_a", Tool: mcp.Tool{Name: "describe_deal_update"}},
}

func newDescribingStager(broker agentcore.MCPBroker, st chatStore, sink agent.EventSink) *approvalStager {
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: sink}
	a.BindTurnMCPScope(agent.TurnMCPScope{
		Broker:    broker,
		Catalog:   describerCatalog,
		Selection: agentcore.MCPSelection{{Server: "deals"}, {Server: "deals", Account: "client_a"}},
	})
	return a
}

func eventNamed(sink *eventSink, name string) map[string]any {
	for i, n := range sink.names {
		if n == name {
			ev, _ := sink.payloads[i].(map[string]any)
			return ev
		}
	}
	return nil
}

const describedArgs = `{"deal_id":"PM-123","floor":2.5,"big":12345678901234567890}`

// The happy path: the describer runs on the SAME server with the SAME args,
// bounded, and its canonical card rides the approval row and the live event.
func TestStage_DescriberCard(t *testing.T) {
	describerPolicy(t)
	broker := &describerBroker{text: goodCard}
	st := &cardStageStore{}
	sink := &eventSink{}
	a := newDescribingStager(broker, st, sink)

	id, err := a.Stage("mcp_deals_update_deal", "call-1", describedArgs)
	if err != nil || id != "ap-new" {
		t.Fatalf("Stage = %q, %v", id, err)
	}
	if got := broker.called(); len(got) != 1 || got[0] != "deals.describe_deal_update" {
		t.Fatalf("describer calls = %v, want one call of deals.describe_deal_update", got)
	}
	if broker.args["deal_id"] != "PM-123" || broker.args["big"].(json.Number).String() != "12345678901234567890" {
		t.Fatalf("describer args = %v, want the staged arguments unchanged", broker.args)
	}
	if broker.budget != approvalCardDescriberTimeout {
		t.Fatalf("describer call budget = %s, want %s across the broker", broker.budget, approvalCardDescriberTimeout)
	}
	_, canonical, _ := parseApprovalCard([]byte(goodCard))
	if st.card != string(canonical) {
		t.Fatalf("stored card = %s, want the canonical card", st.card)
	}
	ev := eventNamed(sink, "tool.approval_required")
	if raw, ok := ev["card"].(json.RawMessage); !ok || string(raw) != string(canonical) {
		t.Fatalf("approval_required card = %v, want the canonical card", ev["card"])
	}
	if eventNamed(sink, "tool.approval_card_fallback") != nil {
		t.Fatal("a successful describe reported a fallback")
	}
	// The frame still serializes as JSON with the card as an object.
	b, err := json.Marshal(ev)
	if err != nil || !strings.Contains(string(b), `"card":{"title":"Update 2 deals"`) {
		t.Fatalf("approval_required wire = %s err=%v", b, err)
	}
}

// A named-account seat calls the describer on its own registered server, and
// is accepted as a read through its base server's parallel-safe entry.
func TestStage_DescriberOnNamedAccountSeat(t *testing.T) {
	describerPolicy(t)
	broker := &describerBroker{text: goodCard}
	st := &cardStageStore{}
	a := newDescribingStager(broker, st, &eventSink{})
	if _, err := a.Stage("mcp_deals_client_a_update_deal", "call-1", describedArgs); err != nil {
		t.Fatal(err)
	}
	if got := broker.called(); len(got) != 1 || got[0] != "deals_client_a.describe_deal_update" {
		t.Fatalf("describer calls = %v, want the variant server", got)
	}
	if st.card == "" {
		t.Fatal("no card stored for the variant seat")
	}
}

// Every failure stages the generic card (no card on the row or the event)
// and reports why, without failing the stage.
func TestStage_DescriberFallbacks(t *testing.T) {
	prev := approvalCardDescriberTimeout
	approvalCardDescriberTimeout = 50 * time.Millisecond
	t.Cleanup(func() { approvalCardDescriberTimeout = prev })
	describerPolicy(t)
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	cases := []struct {
		name   string
		broker *describerBroker
		store  *cardStageStore
		reason string
	}{
		{"timeout", &describerBroker{block: true}, &cardStageStore{}, cardFallbackTimeout},
		{"transport error", &describerBroker{err: errors.New("server gone")}, &cardStageStore{}, cardFallbackError},
		{"tool error", &describerBroker{text: "deal not found", isErr: true}, &cardStageStore{}, cardFallbackError},
		{"not json", &describerBroker{text: "Updating the deal floor"}, &cardStageStore{}, cardFallbackInvalid},
		{"off schema", &describerBroker{text: `{"title":"x","items":[],"extra":1}`}, &cardStageStore{}, cardFallbackInvalid},
		{"secret in card", &describerBroker{text: `{"title":"x","items":[{"label":"` + secret + `"}]}`}, &cardStageStore{}, cardFallbackRedacted},
		{"store error", &describerBroker{text: goodCard}, &cardStageStore{cardErr: errors.New("db down")}, cardFallbackStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &eventSink{}
			a := newDescribingStager(tc.broker, tc.store, sink)
			start := time.Now()
			id, err := a.Stage("mcp_deals_update_deal", "call-1", describedArgs)
			if err != nil || id != "ap-new" {
				t.Fatalf("Stage = %q, %v; a describer failure must never block staging", id, err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatalf("staging waited %s; the describer bound did not hold", time.Since(start))
			}
			if len(tc.broker.called()) != 1 {
				t.Fatalf("describer calls = %v, want exactly one (no retries)", tc.broker.called())
			}
			ev := eventNamed(sink, "tool.approval_required")
			if ev == nil || ev["card"] != nil {
				t.Fatalf("approval_required = %v, want the generic card (no card field)", ev)
			}
			fb := eventNamed(sink, "tool.approval_card_fallback")
			if fb == nil || fb["reason"] != tc.reason || fb["approval_id"] != "ap-new" {
				t.Fatalf("fallback event = %v, want reason %q", fb, tc.reason)
			}
		})
	}
}

// No describer is called when none is declared for the tool, or when the
// declared one cannot be trusted as a read at call time.
func TestStage_DescriberNotCalled(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	cases := []struct {
		name     string
		policy   agentcore.AgentPolicy
		tool     string
		fallback string
	}{
		{"not declared", agentcore.AgentPolicy{
			CriticalToolSuffixes:       []string{"update_deal", "create_deal"},
			ParallelSafeTools:          []string{"mcp_deals_describe_deal_update"},
			CriticalToolCardDescribers: map[string]string{"update_deal": "describe_deal_update"},
		}, "mcp_deals_create_deal", ""},
		{"describer is critical", agentcore.AgentPolicy{
			CriticalToolSuffixes:       []string{"update_deal", "describe_deal_update"},
			ParallelSafeTools:          []string{"mcp_deals_describe_deal_update"},
			CriticalToolCardDescribers: map[string]string{"update_deal": "describe_deal_update"},
		}, "mcp_deals_update_deal", ""},
		{"describer not parallel-safe", agentcore.AgentPolicy{
			CriticalToolSuffixes:       []string{"update_deal"},
			CriticalToolCardDescribers: map[string]string{"update_deal": "describe_deal_update"},
		}, "mcp_deals_update_deal", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentcore.ConfigureAgentPolicy(tc.policy)
			broker := &describerBroker{text: goodCard}
			sink := &eventSink{}
			a := newDescribingStager(broker, &cardStageStore{}, sink)
			if _, err := a.Stage(tc.tool, "call-1", describedArgs); err != nil {
				t.Fatal(err)
			}
			if got := broker.called(); len(got) != 0 {
				t.Fatalf("describer called: %v", got)
			}
			if ev := eventNamed(sink, "tool.approval_required"); ev == nil || ev["card"] != nil {
				t.Fatalf("approval_required = %v, want the generic card", ev)
			}
		})
	}
}

// The describer is resolved on the staged tool's own server: none there, or
// several candidates, resolve to nothing.
func TestResolveDescriber(t *testing.T) {
	catalog := []mcp.ServerTool{
		{ServerName: "a", Tool: mcp.Tool{Name: "ix_describe_change"}},
		{ServerName: "b", Tool: mcp.Tool{Name: "describe_change"}},
		{ServerName: "c", Tool: mcp.Tool{Name: "x_describe_change"}},
		{ServerName: "c", Tool: mcp.Tool{Name: "y_describe_change"}},
	}
	if got, ok := resolveDescriber(catalog, "a", "describe_change"); !ok || got != "ix_describe_change" {
		t.Errorf("a: %q %v", got, ok)
	}
	if got, ok := resolveDescriber(catalog, "b", "describe_change"); !ok || got != "describe_change" {
		t.Errorf("b: %q %v", got, ok)
	}
	if _, ok := resolveDescriber(catalog, "c", "describe_change"); ok {
		t.Error("c: two candidates must resolve to nothing")
	}
	if _, ok := resolveDescriber(catalog, "d", "describe_change"); ok {
		t.Error("d: another server's describer must not be used")
	}
}

// cardGetStore serves the conversation GET from fixed rows.
type cardGetStore struct {
	chatStore
	pending  []store.Approval
	resolved []store.Approval
}

func (s *cardGetStore) LoadHistory(context.Context, string) ([]agent.HistoryEntry, error) {
	return nil, nil
}
func (s *cardGetStore) ListPendingApprovals(context.Context, string, string) ([]store.Approval, error) {
	return s.pending, nil
}
func (s *cardGetStore) ListResolvedApprovals(context.Context, string, string) ([]store.Approval, error) {
	return s.resolved, nil
}
func (s *cardGetStore) ListExecutingApprovals(context.Context, string, string, string) ([]store.Approval, error) {
	return nil, nil
}
func (s *cardGetStore) ListPendingMemoryProposalsForConversation(context.Context, string, string) ([]store.Memory, error) {
	return nil, nil
}
func (s *cardGetStore) GetBranchOrigin(context.Context, string, string) (*store.BranchOrigin, error) {
	return nil, nil
}

// pending_approvals and resolved_approvals carry the stored card; a row with
// none, or with one that no longer validates, carries no card field.
func TestConversationGet_ApprovalCards(t *testing.T) {
	_, canonical, _ := parseApprovalCard([]byte(goodCard))
	st := &cardGetStore{
		pending: []store.Approval{
			{ID: "p1", ToolName: "mcp_deals_update_deal", ArgsJSON: `{}`, Status: "pending", CardJSON: string(canonical)},
			{ID: "p2", ToolName: "mcp_pages_deploy_page", ArgsJSON: `{}`, Status: "pending"},
			{ID: "p3", ToolName: "mcp_deals_update_deal", ArgsJSON: `{}`, Status: "pending", CardJSON: `{"title":"x","items":[],"html":"<b>"}`},
		},
		resolved: []store.Approval{
			{ID: "r1", ToolName: "mcp_deals_update_deal", ArgsJSON: `{}`, Status: "rejected", ResultText: "User declined this action.", CardJSON: string(canonical)},
		},
	}
	s := &Server{store: st}
	req := httptest.NewRequest(http.MethodGet, "/conversations/c1", nil)
	rec := httptest.NewRecorder()
	s.handleConversationGet(rec, req, "u@e.com", "c1", &store.Conversation{ID: "c1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pending  []map[string]json.RawMessage `json:"pending_approvals"`
		Resolved []map[string]json.RawMessage `json:"resolved_approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Pending) != 3 || len(body.Resolved) != 1 {
		t.Fatalf("got %d pending, %d resolved", len(body.Pending), len(body.Resolved))
	}
	if string(body.Pending[0]["card"]) != string(canonical) {
		t.Errorf("p1 card = %s", body.Pending[0]["card"])
	}
	if _, ok := body.Pending[1]["card"]; ok {
		t.Error("p2 has no card but the payload carries one")
	}
	if _, ok := body.Pending[2]["card"]; ok {
		t.Error("p3's stored card is off-schema and must not reach the client")
	}
	if string(body.Resolved[0]["card"]) != string(canonical) {
		t.Errorf("r1 card = %s", body.Resolved[0]["card"])
	}
}

// The bounded one-card GET (?approval_id=) carries the card on a pending and
// on a resolved row alike (Codex P2 on #1717: the resolved branch dropped it).
func TestConversationApprovalGet_CarriesCard(t *testing.T) {
	_, canonical, _ := parseApprovalCard([]byte(goodCard))
	for _, status := range []string{"pending", "rejected"} {
		t.Run(status, func(t *testing.T) {
			st := &claimStore{approval: store.Approval{ID: "ap1", ConversationID: "c1", UserEmail: "u@e.com",
				ToolName: "mcp_deals_update_deal", ArgsJSON: `{}`, Status: status, CardJSON: string(canonical)}}
			s := &Server{store: st}
			rec := httptest.NewRecorder()
			s.handleConversationApprovalGet(rec, httptest.NewRequest(http.MethodGet, "/conversations/c1?approval_id=ap1", nil), "u@e.com", "c1", "ap1")
			var body map[string][]map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%d %s: %v", rec.Code, rec.Body.String(), err)
			}
			rows := make([]map[string]json.RawMessage, 0, 1)
			rows = append(rows, body["pending_approvals"]...)
			rows = append(rows, body["resolved_approvals"]...)
			if len(rows) != 1 || string(rows[0]["card"]) != string(canonical) {
				t.Fatalf("one-card GET rows = %v, want the card", rows)
			}
		})
	}
}
