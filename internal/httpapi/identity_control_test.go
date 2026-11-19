package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/principal"
)

// controlChars are the characters an identity value is refused for in these
// tests: U+0000 (which Postgres cannot store), other C0 controls and DEL, a
// format character, and characters that lowercase or fold onto an ASCII
// letter (KELVIN SIGN, LONG S).
var controlChars = []string{"\x00", "\x01", "\x1f", "\x7f", "\u200b", "\u212a", "\u017f"}

// The chat listener's trust boundary refuses a principal whose email holds a
// control character with a 400, before any handler or lookup runs: such an
// email names no account.
func TestAuthMiddlewareRefusesControlCharacterIdentity(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for _, ctl := range controlChars {
		for _, method := range []principal.AuthMethod{principal.MethodSession, principal.MethodGateway} {
			reached := false
			h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			ctx := principal.With(context.Background(), principal.Principal{Email: "alice@example.com" + ctl, AuthMethod: method})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil))
			if reached || w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "invalid user identity" {
				t.Errorf("%s principal with %q: status %d body %q reached=%v; want 400 invalid user identity", method, ctl, w.Code, w.Body.String(), reached)
			}
		}
	}
}

// A password reset for an email that holds a refused character is refused
// with a 400 before the account is looked up: a password is for signing in,
// which such an identity cannot do.
func TestAdminPasswordResetRefusesControlCharacterEmail(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for _, path := range []string{"/admin/users/alice%40example.com%00/password", "/admin/users/alice%40example.com%E2%84%AA/password"} {
		w := httptest.NewRecorder()
		s.handleAdminUserItem(w, httptest.NewRequestWithContext(context.Background(), http.MethodPut, path, strings.NewReader("{}")))
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "invalid user identity" {
			t.Errorf("PUT %s: status %d body %q; want 400 invalid user identity", path, w.Code, w.Body.String())
		}
	}
}

// mpPost sends a JSON POST through handler as ctx's principal.
func mpPost(ctx context.Context, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

// Creating an account, or writing a participant into a multiplayer chat, with
// an email that holds a refused character is refused with a 400. A
// participant entry like that would also be stored as JSON text that other
// people's listings cast to jsonb.
func TestIdentityWritesRefuseControlCharacters(t *testing.T) {
	t.Parallel()
	srv := serverFixture(t)
	ctx := context.Background()
	st := srv.concreteStore(t)
	conv, err := st.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	routes := srv.Routes().ServeHTTP
	for _, ctl := range controlChars {
		bad := "bob@example.com" + ctl
		for _, body := range []map[string]any{
			{"add": []string{bad}},
			{"multiplayer": true, "participants": []string{bad}},
			{"multiplayer": true, "excluded": []string{bad}},
		} {
			w := mpPost(asUser(ctx, "alice@example.com"), routes, "/conversations/"+conv.ID+"/multiplayer", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("multiplayer %q: status %d %q; want 400", body, w.Code, w.Body.String())
			}
		}
		w := mpPost(asUserRole(ctx, "admin@example.com", "admin"), srv.handleAdminUserCreate, "/admin/users", map[string]any{"email": bad, "password": "password-long-enough"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("admin create %q: status %d %q; want 400", bad, w.Code, w.Body.String())
		}
	}
	// A backslash is refused in an email: `a\u0000@…` would read, in the
	// stored JSON list, like the sequence a damaged list carries.
	if w := mpPost(asUser(ctx, "alice@example.com"), routes, "/conversations/"+conv.ID+"/multiplayer", map[string]any{"add": []string{`dan\u0000@example.com`}}); w.Code != http.StatusBadRequest {
		t.Errorf("multiplayer add of an address with a backslash: status %d %q; want 400", w.Code, w.Body.String())
	}
	// Clean values still go through, and nothing was stored for the refused ones.
	if w := mpPost(asUser(ctx, "alice@example.com"), routes, "/conversations/"+conv.ID+"/multiplayer", map[string]any{"add": []string{"bob@example.com"}}); w.Code != http.StatusOK {
		t.Fatalf("multiplayer add: status %d %q", w.Code, w.Body.String())
	}
	got, err := st.Get(ctx, "alice@example.com", conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Multiplayer || len(got.MultiplayerParticipants) != 1 || got.MultiplayerParticipants[0] != "bob@example.com" || len(got.MultiplayerExcluded) != 0 {
		t.Fatalf("multiplayer %v participants %q excluded %q, want only bob@example.com", got.Multiplayer, got.MultiplayerParticipants, got.MultiplayerExcluded)
	}
}

// A request that changes several multiplayer lists is checked whole before
// anything is written: a refused entry in add leaves the participant list
// and the multiplayer switch exactly as they were. Entries that only need
// trimming (a trailing tab or line break) are accepted, as before.
func TestMultiplayerChangeIsCheckedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	srv := serverFixture(t)
	ctx := context.Background()
	st := srv.concreteStore(t)
	conv, err := st.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	routes := srv.Routes().ServeHTTP
	owner := asUser(ctx, "alice@example.com")
	path := "/conversations/" + conv.ID + "/multiplayer"
	if w := mpPost(owner, routes, path, map[string]any{"multiplayer": true, "participants": []string{"bob@example.com\t", "carol@example.com\r\n"}}); w.Code != http.StatusOK {
		t.Fatalf("participants needing a trim: status %d %q; want 200", w.Code, w.Body.String())
	}
	for _, body := range []map[string]any{
		{"participants": []string{}, "add": []string{"dave\x01@example.com"}},
		{"multiplayer": false, "add": []string{"erin\x01@example.com"}},
	} {
		if w := mpPost(owner, routes, path, body); w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d %q; want 400", body, w.Code, w.Body.String())
		}
	}
	got, err := st.Get(ctx, "alice@example.com", conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Multiplayer || strings.Join(got.MultiplayerParticipants, ",") != "bob@example.com,carol@example.com" {
		t.Fatalf("multiplayer %v participants %q, want on with bob and carol (no partial write)", got.Multiplayer, got.MultiplayerParticipants)
	}
}

// An account or a participant entry stored before identities were checked
// can still be removed: DELETE /admin/users/{email} and a multiplayer remove
// take the value as given and match only that exact entry, and a removed entry
// like that is not written back as an exclusion.
func TestExistingControlCharacterIdentitiesCanBeRemoved(t *testing.T) {
	t.Parallel()
	srv := serverFixture(t)
	ctx := context.Background()
	st := srv.concreteStore(t)
	const old = "old\x01@example.com"
	if _, err := st.Identity().DB().ExecContext(ctx,
		`INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ($1, 'x', 1, 1)`, old); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.handleAdminUserItem(w, httptest.NewRequestWithContext(asUserRole(ctx, "admin@example.com", "admin"),
		http.MethodDelete, "/admin/users/old%01%40example.com", nil))
	if w.Code/100 != 2 {
		t.Fatalf("DELETE the old account: status %d %q; want success", w.Code, w.Body.String())
	}
	var n int
	if err := st.Identity().DB().QueryRowContext(ctx, `SELECT count(*) FROM core.users WHERE email = $1`, old).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old account rows = %d (err %v), want 0", n, err)
	}

	conv, err := st.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Identity().DB().ExecContext(ctx,
		`UPDATE conversations SET multiplayer = TRUE, multiplayer_participants = $2 WHERE id = $1`,
		conv.ID, `["bob@example.com","old\u0001@example.com"]`); err != nil {
		t.Fatal(err)
	}
	if w := mpPost(asUser(ctx, "alice@example.com"), srv.Routes().ServeHTTP, "/conversations/"+conv.ID+"/multiplayer", map[string]any{"remove": []string{old}}); w.Code != http.StatusOK {
		t.Fatalf("remove the old participant: status %d %q; want 200", w.Code, w.Body.String())
	}
	got, err := st.Get(ctx, "alice@example.com", conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Join(got.MultiplayerParticipants, ",") != "bob@example.com" || len(got.MultiplayerExcluded) != 0 {
		t.Fatalf("participants %q excluded %q, want only bob and no exclusion", got.MultiplayerParticipants, got.MultiplayerExcluded)
	}
}

// An admin cannot delete or demote their own account through a look-alike of
// their address: the admin user routes refuse a path email that holds a
// character lowercasing onto an ASCII letter (U+0130 here), and the
// self-protection guards compare the stored form of both addresses.
func TestAdminCannotRemoveThemselvesThroughALookalike(t *testing.T) {
	t.Parallel()
	srv := serverFixture(t)
	ctx := context.Background()
	st := srv.concreteStore(t)
	if _, err := st.CreateUser(ctx, "ivan@example.com", "password-long-enough"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Identity().DB().ExecContext(ctx, `UPDATE core.users SET role = 'admin' WHERE email = 'ivan@example.com'`); err != nil {
		t.Fatal(err)
	}
	admin := asUserRole(ctx, "ivan@example.com", "admin")
	for _, method := range []string{http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/admin/users/%C4%B0van%40example.com", "/admin/users/Ivan%40Example.com"} {
			w := httptest.NewRecorder()
			srv.handleAdminUserItem(w, httptest.NewRequestWithContext(admin, method, path, strings.NewReader(`{"role":"member"}`)))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %s by ivan: status %d %q; want 400", method, path, w.Code, w.Body.String())
			}
		}
	}
	u, err := st.GetUser(ctx, "ivan@example.com")
	if err != nil || u == nil || u.Role != "admin" {
		t.Fatalf("ivan's account after the refused requests: %+v, %v; want it kept, still an admin", u, err)
	}
}

// An ADMIN_EMAILS entry kept as written because it holds a look-alike
// (config's splitEmails) makes no one an admin: kate@x.com is not matched by
// a KELVIN SIGN entry.
func TestAdminEmailsLookalikeEntryGrantsNoOne(t *testing.T) {
	t.Parallel()
	s := &Server{cfg: &config.Config{AdminEmails: []string{"\u212aate@x.com"}}}
	if s.isAdmin("kate@x.com") {
		t.Fatal("a look-alike ADMIN_EMAILS entry made kate@x.com an admin")
	}
}

// U+017F (LONG S) is already lowercase, so an account stored before
// identities were checked may hold it. DELETE /admin/users/{email} still
// reaches it: only the characters lowercasing turns into ASCII letters
// (U+0130, U+212A) are refused in the path.
func TestAdminCanRemoveALegacyLongSAccount(t *testing.T) {
	t.Parallel()
	srv := serverFixture(t)
	ctx := context.Background()
	db := srv.concreteStore(t).Identity().DB()
	const legacy = "\u017fam@example.com"
	if _, err := db.ExecContext(ctx, `INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ($1, 'x', 1, 1)`, legacy); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.handleAdminUserItem(w, httptest.NewRequestWithContext(asUserRole(ctx, "admin@example.com", "admin"),
		http.MethodDelete, "/admin/users/%C5%BFam%40example.com", nil))
	if w.Code/100 != 2 {
		t.Fatalf("DELETE the legacy account: status %d %q; want success", w.Code, w.Body.String())
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM core.users WHERE email = $1`, legacy).Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy account rows = %d (err %v), want 0", n, err)
	}
}
