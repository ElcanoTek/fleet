package handlers

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// testSharedToken is the web gateway's shared token in these tests' handler
// configs: a request carrying it plus X-User-Email is what a signed-in person
// looks like to the Operations Center (header trust).
const testSharedToken = "test-shared-token-placeholder"

// seededSessions maps a test "session token" to the account it signs in as.
// Before #2049 M4 these tests signed people in with the Operations Center's
// password bearer; that bearer is retired, and a person reaches the
// Operations Center only as the web gateway's header-trusted session. A test
// still presents `Authorization: Bearer <token>`, and asSeededSession turns a
// known token into exactly what the gateway forwards for that person's
// session — the real header-trust path then authenticates it against the
// identity table. An unknown token is left alone, so it fails as an unknown
// bearer always did (401). Tokens are unique per test, so parallel tests do
// not collide.
var seededSessions sync.Map // token → email

// seedOpsUser creates an account with Operations Center access in the
// identity table: legacyRole admin makes a Fleet admin; client / readonly a
// viewer with ops_scope run / view. token (optional) is the test session it
// signs in with (seededSessions). It returns the account as this plane sees
// it.
func seedOpsUser(t testing.TB, st *storage.Storage, id uuid.UUID, username, legacyRole, token string) *models.User {
	t.Helper()
	acct := identity.NewAccount{ID: id, Email: username, Role: identity.RoleViewer}
	switch legacyRole {
	case "admin":
		acct.Role = identity.RoleAdmin
	case "client":
		acct.OpsScope = identity.OpsRun
	case "readonly":
		acct.OpsScope = identity.OpsView
	default:
		acct.OpsScope = identity.OpsNone
	}
	u, err := st.Identity().CreateAccount(t.Context(), acct)
	if err != nil {
		t.Fatalf("seed account %s: %v", username, err)
	}
	if token != "" {
		seededSessions.Store(token, u.Email)
	}
	return &models.User{ID: u.ID, Username: u.Email, Role: u.LegacyOpsRole()}
}

// identityTestStorage opens the scheduler on a fresh one-database fleet of
// the test's own (the identity table next to it), for the header-trust tests
// that only need accounts.
func identityTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	st := storage.New()
	if err := st.Initialize(fleetdb.New(t).SchedDSN(), fleetdb.SchedPool()); err != nil {
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// asSeededSession is the router middleware that stands in for the web
// gateway (see seededSessions).
func asSeededSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applySeededSession(r)
		next.ServeHTTP(w, r)
	})
}

// applySeededSession rewrites one request (see seededSessions).
func applySeededSession(r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return
	}
	if email, known := seededSessions.Load(strings.TrimSpace(tok)); known {
		r.Header.Del("Authorization")
		r.Header.Set("X-Orchestrator-Server-Token", testSharedToken)
		r.Header.Set("X-User-Email", email.(string))
	}
}
