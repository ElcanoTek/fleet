package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// The auth middleware is the trust boundary of the chat listener (ADR-0108).
// A request carries a principal the gateway attached in-process, or presents
// a user API key on the listener's own port; nothing a caller writes into an
// identity header is believed, and the retired service token is refused with
// a message naming its replacement.

// edgeKeys is a real API-key store holding one user key for u@x.com and one
// task key (which names no user).
func edgeKeys(t *testing.T) (mgr *apikeys.Manager, userKey, taskKey string) {
	t.Helper()
	dir := t.TempDir()
	mgr, err := apikeys.NewManager(filepath.Join(dir, "api_keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, userKey, err = mgr.CreateUserKey(context.Background(), "cli", "U@x.com", 0, nil, "test"); err != nil {
		t.Fatal(err)
	}
	if _, taskKey, err = mgr.CreateTypedKey(context.Background(), "intake", apikeys.KeyTypeTask, nil, 0, nil, "test"); err != nil {
		t.Fatal(err)
	}
	return mgr, userKey, taskKey
}

// edgeCall runs authMiddleware over one request and returns the status, the
// response's "error" code, and the principal the handler saw.
func edgeCall(ctx context.Context, t *testing.T, s *Server, headers map[string]string) (int, string, principal.Principal) {
	t.Helper()
	var seen principal.Principal
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = principal.From(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Error, seen
}

func TestAuth_EdgeRefusals(t *testing.T) {
	t.Parallel()
	mgr, _, taskKey := edgeKeys(t)
	s := &Server{apiKeys: mgr}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		code    int
		errCode string
	}{
		{"nothing", nil, http.StatusUnauthorized, "authentication_required"},
		{"identity header alone", map[string]string{"X-User-Email": "u@x.com"}, http.StatusUnauthorized, "authentication_required"},
		{"retired service token", map[string]string{"X-Chat-Server-Token": "anything", "X-User-Email": "u@x.com"}, http.StatusUnauthorized, "service_token_retired"},
		{"unknown key", map[string]string{"X-API-Key": "fleet_user_nope"}, http.StatusUnauthorized, "invalid_api_key"},
		{"a key that names no user", map[string]string{"X-API-Key": taskKey}, http.StatusForbidden, "not_a_user_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, errCode, _ := edgeCall(context.Background(), t, s, tc.headers)
			if code != tc.code || errCode != tc.errCode {
				t.Fatalf("status %d error %q, want %d %q", code, errCode, tc.code, tc.errCode)
			}
		})
	}
}

// A user key carries its own principal — the account it names, whatever
// identity header rides along — as X-API-Key or as a bearer.
func TestAuth_UserKeyCarriesItsPrincipal(t *testing.T) {
	t.Parallel()
	mgr, userKey, _ := edgeKeys(t)
	s := &Server{apiKeys: mgr}
	for _, tc := range []struct {
		headers map[string]string
		method  principal.AuthMethod
	}{
		{map[string]string{"X-API-Key": userKey, "X-User-Email": "mallory@x.com"}, principal.MethodAPIKey},
		{map[string]string{"Authorization": "Bearer " + userKey, "X-User-Email": "mallory@x.com"}, principal.MethodBearer},
	} {
		code, _, p := edgeCall(context.Background(), t, s, tc.headers)
		if code != http.StatusOK || p.Email != "u@x.com" || p.AuthMethod != tc.method || p.KeyID == "" {
			t.Fatalf("%v: status %d principal %+v", tc.method, code, p)
		}
	}
}

// The gateway's principal is used as is; one naming no user is refused.
func TestAuth_GatewayPrincipal(t *testing.T) {
	t.Parallel()
	s := &Server{}
	code, _, p := edgeCall(asUser(context.Background(), " U@x.com "), t, s, map[string]string{"X-User-Email": "mallory@x.com"})
	if code != http.StatusOK || p.Email != "u@x.com" {
		t.Fatalf("session: status %d principal %+v", code, p)
	}
	code, _, _ = edgeCall(asGateway(context.Background()), t, s, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("no user: status %d, want 400", code)
	}
}

func TestUserFromCtx_Empty(t *testing.T) {
	t.Parallel()

	if got := userFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx: got %q", got)
	}
}
