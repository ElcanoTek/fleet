package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// The Operations Center refuses a session principal whose email holds a
// control character with a 400, before the account is looked up: such an
// email names no account.
func TestUserPrincipalRefusesControlCharacterIdentity(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	for _, ctl := range []string{"\x00", "\x01", "\x1f", "\x7f", "\u200b", "\u212a", "\u017f"} {
		w := httptest.NewRecorder()
		r := asSession(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tasks", nil), "alice@example.com"+ctl)
		user, _, handled := h.userPrincipal(w, r)
		var body struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if !handled || user != nil || w.Code != http.StatusBadRequest || body.Detail != "invalid user identity" {
			t.Errorf("email with %q: handled=%v user=%v status %d detail %q; want a 400 invalid user identity", ctl, handled, user, w.Code, body.Detail)
		}
	}
}

// POST /users checks the username as sent, before lowercasing: a look-alike
// of an existing ASCII account (U+212A KELVIN SIGN for k) is refused with a
// 400 instead of naming, and promoting, that account.
func TestCreateUserRefusesLookalikeUsername(t *testing.T) {
	t.Parallel()
	h, store := setupTest(t)
	kate := seedOpsUser(t, store, uuid.New(), "kate@x.com", "readonly", "")
	for _, name := range []string{"\u212aate@x.com", "kate\x01@x.com"} {
		body, _ := json.Marshal(map[string]string{"username": name, "password": "testpassword123", "role": "admin"})
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/users", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.CreateUser(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("username %q: status %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	got, err := store.Identity().GetByEmail(t.Context(), "kate@x.com")
	if err != nil || got.Role == identity.RoleAdmin || got.ID != kate.ID {
		t.Fatalf("kate after the refused creates = %+v, %v; want unchanged and not an admin", got, err)
	}
}
