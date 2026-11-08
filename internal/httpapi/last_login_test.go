package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// A session mint stamps core.users.last_login (Codex 4505664991 on #2083):
// the password check the gateway mints a web session on, and the OIDC login's
// session-epoch handout. A refused password records nothing.
func TestSessionMintRecordsLastLogin(t *testing.T) {
	t.Parallel()

	s := memberFixture(t)
	st := s.concreteStore(t)
	ctx := t.Context()
	for _, e := range []string{"pw@x.com", "oidc@x.com"} {
		if _, err := st.CreateUser(ctx, e, "placeholder-not-a-secret"); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Routes()
	before := time.Now().Add(-time.Second)

	w := do(t, h, http.MethodPost, "/auth/verify", map[string]string{"email": "pw@x.com", "password": "wrong-password-1"}, "pw@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("verify (wrong): %d %s", w.Code, w.Body.String())
	}
	if at, err := st.LastLogin(ctx, "pw@x.com"); err != nil || at != nil {
		t.Fatalf("a refused password recorded a login: %v, %v", at, err)
	}
	w = do(t, h, http.MethodPost, "/auth/verify", map[string]string{"email": "pw@x.com", "password": "placeholder-not-a-secret"}, "pw@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	if at, err := st.LastLogin(ctx, "pw@x.com"); err != nil || at == nil || at.Before(before) {
		t.Fatalf("password login last_login = %v, %v; want now", at, err)
	}

	body := map[string]string{"issuer": "https://auth.example.com", "subject": "account-1"}
	if w := do(t, h, http.MethodPost, "/auth/external-session-epoch", body, "oidc@x.com"); w.Code != http.StatusOK {
		t.Fatalf("external epoch: %d %s", w.Code, w.Body.String())
	}
	if at, err := st.LastLogin(ctx, "oidc@x.com"); err != nil || at == nil || at.Before(before) {
		t.Fatalf("OIDC login last_login = %v, %v; want now", at, err)
	}
}
