package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Seats (#988): the per-conversation Tools picker lists ONE entry per hosted
// connection name with its labeled seats, and a conversation may pin a seat —
// but only one the user actually holds.
func TestConversationMCPServersSeats(t *testing.T) {
	srv, st, row, user := remoteMCPFixture(t)
	ctx := context.Background()

	work, err := st.CreateRemoteMCPServer(ctx, store.RemoteMCPServerInput{
		UserEmail: user, Name: "GitHub", Account: "work",
		URL:       "https://mcp.github.example.com",
		Transport: store.RemoteMCPTransportStreamableHTTP,
		Issuer:    "https://auth.github.example.com",
		ClientID:  "client-2",
	})
	if err != nil {
		t.Fatalf("create work seat: %v", err)
	}
	for _, id := range []string{row.ID, work.ID} {
		if err := st.SetRemoteMCPStatus(ctx, user, id, store.RemoteMCPStatusConnected, ""); err != nil {
			t.Fatalf("SetRemoteMCPStatus: %v", err)
		}
	}
	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/conversations/"+conv.ID+"/mcp-servers", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		return w.Code, w.Body.String()
	}
	get := func() []map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/conversations/"+conv.ID+"/mcp-servers", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		if w.Code != 200 {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Servers []map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response: %v", err)
		}
		return resp.Servers
	}

	// One GitHub entry, not one per seat; the labeled seat is pickable, the
	// unlabeled one is the default.
	var github []map[string]any
	for _, e := range get() {
		if e["name"] == "GitHub" {
			github = append(github, e)
		}
	}
	if len(github) != 1 {
		t.Fatalf("GitHub entries = %d, want 1 grouped entry", len(github))
	}
	if accts, _ := github[0]["accounts"].([]any); len(accts) != 1 || accts[0] != "work" {
		t.Fatalf("accounts = %v, want [work]", github[0]["accounts"])
	}
	if github[0]["default_account"] != "" || github[0]["account"] != "" {
		t.Fatalf("default_account/account = %v/%v, want both empty", github[0]["default_account"], github[0]["account"])
	}

	// Pin the work seat for this conversation (name matched case-insensitively).
	code, body := post(`{"enabled_optional":["github"],"accounts":{"github":"work"}}`)
	if code != 200 {
		t.Fatalf("POST pin: %d %s", code, body)
	}
	var resp struct {
		Accounts map[string]string `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Accounts["github"] != "work" {
		t.Fatalf("POST response = %s (err %v), want accounts.github=work", body, err)
	}
	got, err := st.Get(ctx, user, conv.ID)
	if err != nil || got == nil || got.MCPAccounts["github"] != "work" {
		t.Fatalf("persisted mcp_accounts = %v (err %v)", got, err)
	}
	for _, e := range get() {
		if e["name"] == "GitHub" && e["account"] != "work" {
			t.Fatalf("GET after pin: account = %v, want work", e["account"])
		}
	}

	// A seat the user does not hold is a 400, never a silent substitution.
	if code, body := post(`{"enabled_optional":["github"],"accounts":{"github":"personal"}}`); code != 400 {
		t.Fatalf("unknown seat: %d %s, want 400", code, body)
	}
	// A bundled connector with no provisioned seats cannot be pinned either.
	if code, body := post(`{"enabled_optional":["gamma"],"accounts":{"gamma":"x"}}`); code != 400 {
		t.Fatalf("unknown bundled seat: %d %s, want 400", code, body)
	}
	// Omitting accounts clears the override.
	if code, body := post(`{"enabled_optional":["github"]}`); code != 200 {
		t.Fatalf("POST clear: %d %s", code, body)
	}
	got, _ = st.Get(ctx, user, conv.ID)
	if len(got.MCPAccounts) != 0 {
		t.Fatalf("override not cleared: %v", got.MCPAccounts)
	}
}
