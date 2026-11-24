package httpapi

// Several accounts of one connector in one conversation
// (docs/MULTI-ACCOUNT-CHATS.md).

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestMCPAccountChoiceJSON(t *testing.T) {
	t.Parallel()
	var got map[string]mcpAccountChoice
	if err := json.Unmarshal([]byte(`{"a":"twc","b":["","twc"],"c":null,"d":""}`), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]mcpAccountChoice{"a": {"twc"}, "b": {"", "twc"}, "c": nil, "d": {""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %#v, want %#v", got, want)
	}
	if err := json.Unmarshal([]byte(`{"a":7}`), &got); err == nil {
		t.Fatal("a number decoded as an account choice")
	}
	if err := json.Unmarshal([]byte(`{"a":{"x":"y"}}`), &got); err == nil {
		t.Fatal("an object decoded as an account choice")
	}
}

func TestMCPAccountsWireKeepsSingleAccountsAsStrings(t *testing.T) {
	t.Parallel()
	got := mcpAccountsWire(map[string]string{"ix": "twc", "ox": ",twc,arena"})
	want := map[string]any{"ix": "twc", "ox": []string{"", "twc", "arena"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire = %#v, want %#v", got, want)
	}
}

func TestBundledTurnAccounts(t *testing.T) {
	t.Parallel()
	info := agent.OptionalServerInfo{Name: "ix", Accounts: []string{"arena", "twc", "raptive"}}
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	wide := agent.OptionalServerInfo{Name: "ix", Accounts: many}
	cases := []struct {
		name     string
		info     agent.OptionalServerInfo
		override string
		pref     string
		want     []string
	}{
		{"no override uses the deployment account", info, "", "", []string{""}},
		{"no override uses the user's default", info, "", "twc", []string{"twc"}},
		{"one account, as before", info, "arena", "twc", []string{"arena"}},
		{"a stale single account falls back to the default", info, "gone", "twc", []string{"twc"}},
		{"a list, default first", info, ",twc,arena", "", []string{"", "twc", "arena"}},
		{"the default in a list is the user's default", info, ",arena", "twc", []string{"twc", "arena"}},
		{"the user's default named twice binds once", info, ",twc,arena", "twc", []string{"twc", "arena"}},
		{"a stale member is left out, the rest stay", info, "twc,gone,arena", "", []string{"twc", "arena"}},
		{"a list with nothing left falls back to the default", info, "gone,older", "raptive", []string{"raptive"}},
		{"the cap holds even for a row written around the API", wide, strings.Join(many, ","), "", many[:store.MaxMCPAccountsPerConnector]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bundledTurnAccounts(tc.info, tc.override, tc.pref); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bundledTurnAccounts(%q, pref %q) = %q, want %q", tc.override, tc.pref, got, tc.want)
			}
		})
	}
}

// What the picker shows is the stored choice read against the live catalog,
// with "" still meaning "my default": the same reading the turn makes.
func TestBundledChosenAccounts(t *testing.T) {
	t.Parallel()
	info := agent.OptionalServerInfo{Name: "ix", Accounts: []string{"arena", "twc", "raptive"}}
	cases := []struct {
		name, override, pref string
		want                 []string
	}{
		{"no override", "", "twc", []string{""}},
		{"one account", "arena", "", []string{"arena"}},
		{"a list keeps the default as the default", ",twc,arena", "", []string{"", "twc", "arena"}},
		{"a removed account is not shown", "twc,gone,arena", "", []string{"twc", "arena"}},
		{"a list down to one account is one account", "gone,arena", "", []string{"arena"}},
		{"nothing left is the default", "gone,older", "raptive", []string{""}},
		{"the default beside its own name is one account, by name", ",twc,arena", "twc", []string{"twc", "arena"}},
		{"a default that is no longer provisioned does not hide the default", ",arena", "gone", []string{"", "arena"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bundledChosenAccounts(info, tc.override, tc.pref); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bundledChosenAccounts(%q, pref %q) = %q, want %q", tc.override, tc.pref, got, tc.want)
			}
		})
	}
}

func multiAccountServer(t *testing.T) (*Server, *fakeChatStore) {
	t.Helper()
	fs := newFakeChatStore()
	return &Server{store: fs, agent: &alwaysOnFakeEngine{
		fakeTurnEngine: &fakeTurnEngine{},
		optional: []agent.OptionalServerInfo{
			{Name: "ix_mcp", Accounts: []string{"arena", "raptive", "twc"}},
			{Name: "email"},
		},
	}}, fs
}

func TestCleanMCPAccountsLists(t *testing.T) {
	t.Parallel()
	s, _ := multiAccountServer(t)
	ctx := context.Background()

	got, err := s.cleanMCPAccounts(ctx, "u@x.com", map[string]mcpAccountChoice{
		// Ticked in a different order than the catalog, with a repeat.
		"IX_MCP": {"twc", "", "arena", "twc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"ix_mcp": ",arena,twc"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("clean = %#v, want %#v (default first, then catalog order)", got, want)
	}

	// One account is stored exactly as it was before lists existed.
	got, err = s.cleanMCPAccounts(ctx, "u@x.com", map[string]mcpAccountChoice{"ix_mcp": {"twc"}})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"ix_mcp": "twc"}) {
		t.Fatalf("single = %#v (err %v)", got, err)
	}
	// Only the default account is no override.
	got, err = s.cleanMCPAccounts(ctx, "u@x.com", map[string]mcpAccountChoice{"ix_mcp": {""}, "email": nil})
	if err != nil || len(got) != 0 {
		t.Fatalf("default-only = %#v (err %v), want no override", got, err)
	}
	// An account the connector does not have is refused, never dropped: the
	// user would believe it is on.
	if _, err := s.cleanMCPAccounts(ctx, "u@x.com", map[string]mcpAccountChoice{"ix_mcp": {"twc", "nope"}}); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("unknown member: err = %v, want it named", err)
	}
	if _, err := s.cleanMCPAccounts(ctx, "u@x.com", map[string]mcpAccountChoice{"email": {"", "x"}}); err == nil {
		t.Fatal("a connector with no accounts accepted one")
	}
}

func TestCleanMCPAccountsCap(t *testing.T) {
	t.Parallel()
	info := agent.OptionalServerInfo{Name: "ix_mcp", Accounts: []string{"a", "b", "c", "d", "e", "f", "g"}}
	s := &Server{store: newFakeChatStore(), agent: &alwaysOnFakeEngine{fakeTurnEngine: &fakeTurnEngine{}, optional: []agent.OptionalServerInfo{info}}}
	atCap := mcpAccountChoice(info.Accounts[:store.MaxMCPAccountsPerConnector])
	if _, err := s.cleanMCPAccounts(context.Background(), "u@x.com", map[string]mcpAccountChoice{"ix_mcp": atCap}); err != nil {
		t.Fatalf("%d accounts refused: %v", len(atCap), err)
	}
	over := append(mcpAccountChoice{""}, atCap...)
	if _, err := s.cleanMCPAccounts(context.Background(), "u@x.com", map[string]mcpAccountChoice{"ix_mcp": over}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("%d accounts: err = %v, want the cap", len(over), err)
	}
}

func TestApplyConnectorPrefsSeveralAccounts(t *testing.T) {
	t.Parallel()
	s, _ := multiAccountServer(t)
	ctx := context.Background()

	kept, seats, extra := s.applyConnectorPrefs(ctx, "u@x.com", []string{"email", "ix_mcp"}, map[string]string{"ix_mcp": ",arena,twc"})
	if !reflect.DeepEqual(kept, []string{"email", "ix_mcp"}) {
		t.Fatalf("kept = %v", kept)
	}
	if len(seats) != 0 {
		t.Fatalf("seats = %v, want none: the first account is the default one", seats)
	}
	if want := map[string][]string{"ix_mcp": {"arena", "twc"}}; !reflect.DeepEqual(extra, want) {
		t.Fatalf("extra = %v, want %v", extra, want)
	}

	// One account per connector: nothing extra, exactly the old result.
	_, seats, extra = s.applyConnectorPrefs(ctx, "u@x.com", []string{"email", "ix_mcp"}, map[string]string{"ix_mcp": "twc"})
	if !reflect.DeepEqual(seats, map[string]string{"ix_mcp": "twc"}) || extra != nil {
		t.Fatalf("single: seats = %v extra = %v", seats, extra)
	}
}

// A task scheduled from a chat that runs a connector as several accounts pins
// every one of them.
func TestChatTaskConnectorsForSeveralAccounts(t *testing.T) {
	t.Parallel()
	s, fs := multiAccountServer(t)
	ctx := context.Background()
	const user = "u@x.com"
	conv, err := fs.CreateConversation(ctx, user, "t", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	conv.OptionalMCPServersEnabled = []string{"ix_mcp", "email"}
	conv.MCPAccounts = map[string]string{"ix_mcp": ",arena,twc"}

	got, err := s.chatTaskConnectorsFor(user, conv.ID)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []TaskConnector{
		{Server: "email"},
		{Server: "ix_mcp"},
		{Server: "ix_mcp", Account: "arena"},
		{Server: "ix_mcp", Account: "twc"},
	}
	if !reflect.DeepEqual(got.Selection, want) {
		t.Fatalf("selection = %+v, want %+v", got.Selection, want)
	}
}

// The conversation endpoints round-trip a list, and a conversation on one
// account reads the payload it always did.
func TestConversationMCPServersSeveralAccounts(t *testing.T) {
	t.Parallel()

	srv, st, _, user := remoteMCPFixture(t)
	srv.agent = &catalogEngine{catalog: []agent.OptionalServerInfo{
		{Name: "gamma", Accounts: []string{"arena", "twc"}},
	}}
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/conversations/"+conv.ID+"/mcp-servers", strings.NewReader(body))
		req = req.WithContext(asUser(req.Context(), user))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		return w.Code, w.Body.String()
	}
	gamma := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/conversations/"+conv.ID+"/mcp-servers", nil)
		req = req.WithContext(asUser(req.Context(), user))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		var resp struct {
			Servers []map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		for _, e := range resp.Servers {
			if e["name"] == "gamma" {
				return e
			}
		}
		t.Fatalf("no gamma row in %s", w.Body.String())
		return nil
	}

	code, body := post(`{"enabled_optional":["gamma"],"accounts":{"gamma":["twc","","arena"]}}`)
	if code != 200 {
		t.Fatalf("POST list: %d %s", code, body)
	}
	var resp struct {
		Accounts map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if want := []any{"", "arena", "twc"}; !reflect.DeepEqual(resp.Accounts["gamma"], want) {
		t.Fatalf("POST response accounts.gamma = %#v, want %#v", resp.Accounts["gamma"], want)
	}
	got, err := st.Get(ctx, user, conv.ID)
	if err != nil || got == nil || got.MCPAccounts["gamma"] != ",arena,twc" {
		t.Fatalf("persisted mcp_accounts = %v (err %v)", got, err)
	}
	row := gamma()
	if row["account"] != "" {
		t.Fatalf("account = %v, want the first of the list (the default)", row["account"])
	}
	if want := []any{"", "arena", "twc"}; !reflect.DeepEqual(row["selected_accounts"], want) {
		t.Fatalf("selected_accounts = %#v, want %#v", row["selected_accounts"], want)
	}

	// Back to one account: the string form on the way in and out, and no
	// selected_accounts key on the row.
	code, body = post(`{"enabled_optional":["gamma"],"accounts":{"gamma":"twc"}}`)
	if code != 200 || !strings.Contains(body, `"gamma":"twc"`) {
		t.Fatalf("POST single: %d %s", code, body)
	}
	row = gamma()
	if row["account"] != "twc" {
		t.Fatalf("account = %v, want twc", row["account"])
	}
	if _, present := row["selected_accounts"]; present {
		t.Fatalf("a one-account row carries selected_accounts: %v", row)
	}

	// A hosted connection mounts one seat per conversation.
	if code, body := post(`{"enabled_optional":["github"],"accounts":{"github":["","work"]}}`); code != 400 {
		t.Fatalf("hosted list: %d %s, want 400", code, body)
	}

	// An account removed from the box after it was chosen: the picker shows
	// what the next turn will run as, not the stored list.
	if err := st.SetConversationMCPAccounts(ctx, user, conv.ID, map[string]string{"gamma": "twc,gone,arena"}); err != nil {
		t.Fatal(err)
	}
	row = gamma()
	if want := []any{"twc", "arena"}; row["account"] != "twc" || !reflect.DeepEqual(row["selected_accounts"], want) {
		t.Fatalf("stale member: account = %v, selected_accounts = %#v, want twc and %#v", row["account"], row["selected_accounts"], want)
	}
	// Down to one account, the row is a one-account row.
	if err := st.SetConversationMCPAccounts(ctx, user, conv.ID, map[string]string{"gamma": "gone,arena"}); err != nil {
		t.Fatal(err)
	}
	row = gamma()
	if _, present := row["selected_accounts"]; row["account"] != "arena" || present {
		t.Fatalf("one account left: %v", row)
	}

	// "" beside the account that is the user's own default is one account,
	// on the way in and on the way out.
	if err := st.SetConnectorPref(ctx, user, store.ConnectorPref{
		Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, DefaultAccount: "twc",
	}); err != nil {
		t.Fatal(err)
	}
	code, body = post(`{"enabled_optional":["gamma"],"accounts":{"gamma":["","twc","arena"]}}`)
	if code != 200 {
		t.Fatalf("POST default twice: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if want := []any{"arena", "twc"}; !reflect.DeepEqual(resp.Accounts["gamma"], want) {
		t.Fatalf("POST response accounts.gamma = %#v, want %#v", resp.Accounts["gamma"], want)
	}
	row = gamma()
	if want := []any{"arena", "twc"}; !reflect.DeepEqual(row["selected_accounts"], want) {
		t.Fatalf("selected_accounts = %#v, want %#v", row["selected_accounts"], want)
	}
	code, body = post(`{"enabled_optional":["gamma"],"accounts":{"gamma":["","twc"]}}`)
	if code != 200 || !strings.Contains(body, `"gamma":"twc"`) {
		t.Fatalf("POST default beside its own name: %d %s, want the one account", code, body)
	}
}

// A connector that is off keeps its account choice, and a persona policy is
// written against registered tool names: it can permit one account of a
// connector and not another. Whether the persona may use the connector is
// asked of every account, not only the first.
func TestConnectorPermittedInChatChecksEveryAccount(t *testing.T) {
	t.Parallel()
	policy := &agentcore.PersonaToolPermissions{Allow: []string{"mcp:ix_arena/get_deal"}}
	info := agent.OptionalServerInfo{Name: "ix", Tools: []string{"get_deal"}, SelectedAccount: "twc"}
	if connectorPermittedInChat(info, nil, policy) {
		t.Fatal("one account the persona cannot use was permitted")
	}
	info.SelectedAccounts = []string{"twc", "arena"}
	if !connectorPermittedInChat(info, nil, policy) {
		t.Fatal("a connector with a second account the persona can use was not permitted")
	}

	// The resolver that feeds it carries every account of a stored list.
	s, _ := multiAccountServer(t)
	engine := s.agent.(*alwaysOnFakeEngine)
	engine.optional = []agent.OptionalServerInfo{
		{Name: "ix_mcp", Accounts: []string{"arena", "raptive", "twc"}, Tools: []string{"get_deal"}, ToolCount: 1},
	}
	got := s.availableOptionalConnectorsForUser(context.Background(), "u@x.com", map[string]string{"ix_mcp": "twc,arena"})
	if len(got) != 1 || !reflect.DeepEqual(got[0].ToolSeats(), []string{"twc", "arena"}) || got[0].SelectedAccount != "twc" {
		t.Fatalf("available = %+v", got)
	}
	one := s.availableOptionalConnectorsForUser(context.Background(), "u@x.com", map[string]string{"ix_mcp": "twc"})
	if len(one) != 1 || one[0].SelectedAccounts != nil || !reflect.DeepEqual(one[0].ToolSeats(), []string{"twc"}) {
		t.Fatalf("one account = %+v", one)
	}
}
