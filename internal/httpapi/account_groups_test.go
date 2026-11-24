package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestResolveAccountGroups(t *testing.T) {
	t.Parallel()
	cfg := clientconfig.AccountGroupsConfig{Label: "Clients", Groups: []clientconfig.AccountGroup{
		{ID: "twc", Name: "The Weather Company", Accounts: []clientconfig.AccountGroupMember{
			{Server: "ix", Account: "twc"},
			{Server: "ox", Account: "twc"},
			{Server: "pm", Account: "twc"},
			{Server: "ix"},
		}},
	}}
	// ix has the account, ox does not, pm is not in this user's picker.
	got := resolveAccountGroups(cfg, map[string][]string{"ix": {"arena", "twc"}, "ox": {"arena"}})
	want := &accountGroupsView{Label: "Clients", Groups: []accountGroupView{
		{ID: "twc", Name: "The Weather Company", Accounts: []accountGroupMemberView{
			{Server: "ix", Account: "twc", Status: accountGroupMemberOK},
			{Server: "ox", Account: "twc", Status: accountGroupMemberAccountMissing},
			{Server: "pm", Account: "twc", Status: accountGroupMemberConnectorMissing},
			// The default account needs no provisioned name.
			{Server: "ix", Account: "", Status: accountGroupMemberOK},
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
	if got := resolveAccountGroups(clientconfig.AccountGroupsConfig{Label: "Clients"}, nil); got != nil {
		t.Fatalf("no groups resolved to %+v, want nil", got)
	}
}

func TestPickableBundledAccounts(t *testing.T) {
	t.Parallel()
	catalog := []agent.OptionalServerInfo{
		{Name: "ix", Accounts: []string{"twc"}},
		{Name: "ox"},
		{Name: "setup", Accounts: []string{"twc"}},
		{Name: "hidden", Accounts: []string{"twc"}},
	}
	rows := []map[string]any{
		{"name": "ix", "status": connectorStatusAvailable},
		{"name": "ox"},
		{"name": "setup", "status": connectorStatusNeedsSetup},
		{"name": "mail", "always_on": true},
		// A hosted connection that happens to share a bundled name.
		{"name": "hidden", "remote": true},
		{"name": "not-in-catalog"},
	}
	got := pickableBundledAccounts(rows, catalog, nil)
	want := map[string][]string{"ix": {"twc"}, "ox": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pickable = %#v, want %#v", got, want)
	}
	// A connector the user turned off under Settings → Connections is still
	// listed in a conversation that had it on, but no turn runs it.
	got = pickableBundledAccounts(rows, catalog, map[string]bool{"ix": true})
	if want := (map[string][]string{"ox": nil}); !reflect.DeepEqual(got, want) {
		t.Fatalf("pickable with ix turned off = %#v, want %#v", got, want)
	}
}

// The catalog responses carry the bundle's groups, each member checked
// against the rows the same response lists; a bundle with none adds no key.
func TestMCPServerCatalogCarriesAccountGroups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: ix
    display_name: Index Exchange
    description: Deals.
    command: /bin/true
    optional: true
    always: true
  - name: ox
    display_name: OpenX
    description: Deals.
    command: /bin/true
    optional: true
    always: true
account_groups:
  label: Clients
  groups:
    - name: The Weather Company
      accounts:
        - {server: ix, account: twc}
        - {server: ox, account: twc}
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine := &alwaysOnFakeEngine{fakeTurnEngine: &fakeTurnEngine{}, optional: []agent.OptionalServerInfo{
		{Name: "ix", Accounts: []string{"twc"}, ToolCount: 1, Tools: []string{"get_deal"}},
	}}
	s := &Server{store: newFakeChatStore(), agent: engine, clientConfig: bundle}

	get := func() map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest("GET", "/mcp-servers", nil)
		req = req.WithContext(asUser(req.Context(), "u@x.com"))
		w := httptest.NewRecorder()
		s.listMCPServerCatalog(w, req)
		if w.Code != 200 {
			t.Fatalf("GET /mcp-servers: %d %s", w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	var view accountGroupsView
	if err := json.Unmarshal(get()["account_groups"], &view); err != nil {
		t.Fatalf("account_groups: %v", err)
	}
	want := accountGroupsView{Label: "Clients", Groups: []accountGroupView{
		{ID: "the-weather-company", Name: "The Weather Company", Accounts: []accountGroupMemberView{
			{Server: "ix", Account: "twc", Status: accountGroupMemberOK},
			// ox is declared by the bundle but not running on this box.
			{Server: "ox", Account: "twc", Status: accountGroupMemberConnectorMissing},
		}},
	}}
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("account_groups = %+v, want %+v", view, want)
	}

	// A bundle with no groups: the response has no such key.
	empty, err := clientconfig.Load(writeBareBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	s.clientConfig = empty
	if _, present := get()["account_groups"]; present {
		t.Fatal("a bundle with no account groups added the key")
	}
}

func writeBareBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("mcp_servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A connector the user turned off under Settings → Connections stays listed in
// a conversation that already had it on, so it can be switched off there, but
// no turn runs it. A group member on it is reported missing, not usable.
func TestConversationCatalogAccountGroupsSkipConnectorsTurnedOffInSettings(t *testing.T) {
	t.Parallel()
	srv, st, _, user := remoteMCPFixture(t)
	srv.agent = &catalogEngine{catalog: []agent.OptionalServerInfo{
		{Name: "gamma", Accounts: []string{"twc"}, Tools: []string{"get"}, ToolCount: 1},
	}}
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: gamma
    display_name: Gamma
    description: Decks.
    command: /bin/true
    optional: true
    always: true
account_groups:
  groups:
    - name: The Weather Company
      accounts: [{server: gamma, account: twc}]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.clientConfig = bundle

	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOptionalMCPServers(ctx, user, conv.ID, []string{"gamma"}); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		req := httptest.NewRequest("GET", "/conversations/"+conv.ID+"/mcp-servers", nil)
		req = req.WithContext(asUser(req.Context(), user))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		var body struct {
			Servers       []map[string]any  `json:"servers"`
			AccountGroups accountGroupsView `json:"account_groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		listed := false
		for _, row := range body.Servers {
			if row["name"] == "gamma" {
				listed = true
			}
		}
		if !listed {
			t.Fatalf("gamma is not listed: %s", w.Body.String())
		}
		return body.AccountGroups.Groups[0].Accounts[0].Status
	}
	if got := status(); got != accountGroupMemberOK {
		t.Fatalf("status = %q, want ok", got)
	}
	if err := st.SetConnectorPref(ctx, user, store.ConnectorPref{Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	// Still listed (the conversation has it on), but not a usable member.
	if got := status(); got != accountGroupMemberConnectorMissing {
		t.Fatalf("status with gamma turned off in Settings = %q, want %q", got, accountGroupMemberConnectorMissing)
	}
}

func writeSourceBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: ix
    display_name: Index Exchange
    description: Deals.
    command: /bin/true
    optional: true
    always: true
  - name: deals
    display_name: Deals
    description: Reads clients.
    command: /bin/true
    always: true
    tools: [list_account_groups]
account_groups:
  label: Clients
  groups:
    - id: house
      name: House
      accounts: [{server: ix}]
  source:
    server: deals
    tool: list_account_groups
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
