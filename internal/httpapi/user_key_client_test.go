package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/chattui"
)

// `fleet chat` / `fleet acp` against the real chat listener (ADR-0108): a
// user API key is the whole credential. The server acts as the key's account
// (GET /me answers for it), a key for a disabled or missing account is
// refused, and --email only checks the key's account.
func TestChatCLIAuthenticatesWithAUserKey(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	mgr, userKey, taskKey := edgeKeys(t)
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	c := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey})
	if err := c.CheckIdentity(context.Background()); err != nil {
		t.Fatalf("user key: %v", err)
	}
	if c.Email() != "u@x.com" {
		t.Fatalf("client learned email %q, want the key's account", c.Email())
	}

	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey, Email: "other@x.com"}).CheckIdentity(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "acts as u@x.com, not other@x.com") {
		t.Fatalf("a key for another account than --email: %v", err)
	}
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: taskKey}).CheckIdentity(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "names no user") {
		t.Fatalf("a task key: %v", err)
	}
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: "fleet_user_unknown"}).CheckIdentity(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not authorized (401)") {
		t.Fatalf("an unknown key: %v", err)
	}

	// The account is resolved on every request: a key for an address with no
	// enabled account is refused.
	_, ghostKey, err := mgr.CreateUserKey("ghost", "ghost@x.com", 0, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: ghostKey}).CheckIdentity(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not an enabled fleet user") {
		t.Fatalf("a key for no account: %v", err)
	}
}
