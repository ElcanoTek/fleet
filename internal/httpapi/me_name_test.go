package httpapi

// PUT /me/name (ADR-0087): the caller's own first name, the only source of the
// bundle's ${FLEET_USER_NAME} token. These run against Postgres, like the
// /me/team tests beside them.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

func TestSelfServeFirstName(t *testing.T) {
	s := memberFixture(t, "elyse.messer@x.com")
	h := s.Routes()
	ctx := context.Background()

	// Nothing saved: /me reports no name, and a turn gets the email only —
	// "elyse.messer@" is NOT turned into "Elyse".
	w := do(t, h, http.MethodGet, "/me", nil, "elyse.messer@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me: status %d (body %s)", w.Code, w.Body.String())
	}
	if me := decodeMe(t, w.Body.Bytes()); me.GivenName != "" {
		t.Fatalf("fresh account given_name = %q, want empty", me.GivenName)
	}
	if got := s.mcpUserFor(ctx, "elyse.messer@x.com"); got != (agentcore.MCPUserIdentity{Email: "elyse.messer@x.com"}) {
		t.Fatalf("mcpUserFor with no saved name = %+v, want the email only", got)
	}

	// Save one (trimmed); the next turn's identity carries it.
	w = do(t, h, http.MethodPut, "/me/name", map[string]any{"given_name": "  Elyse "}, "elyse.messer@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /me/name: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if me := decodeMe(t, w.Body.Bytes()); me.GivenName != "Elyse" {
		t.Fatalf("after save, given_name = %q, want Elyse", me.GivenName)
	}
	if got := s.mcpUserFor(ctx, "elyse.messer@x.com"); got != (agentcore.MCPUserIdentity{Email: "elyse.messer@x.com", Name: "Elyse"}) {
		t.Fatalf("mcpUserFor after save = %+v, want the saved name", got)
	}

	// Values that are not a name are refused and leave the saved one alone.
	for _, bad := range []string{
		"elyse.messer@x.com",    // an address is not a name
		"ops2",                  // a handle
		"${FLEET_USER_EMAIL}",   // a token
		"Line\nbreak",           // control character
		"Two  Spaces",           // padded
		strings.Repeat("a", 65), // too long
	} {
		w = do(t, h, http.MethodPut, "/me/name", map[string]any{"given_name": bad}, "elyse.messer@x.com")
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT /me/name %q: status %d want 400 (body %s)", bad, w.Code, w.Body.String())
		}
	}
	w = do(t, h, http.MethodGet, "/me/name", nil, "elyse.messer@x.com")
	if me := decodeMe(t, w.Body.Bytes()); me.GivenName != "Elyse" {
		t.Fatalf("a refused write changed the saved name to %q", me.GivenName)
	}

	// A missing field is a 400, not an accidental clear.
	w = do(t, h, http.MethodPut, "/me/name", map[string]any{}, "elyse.messer@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status %d want 400 (body %s)", w.Code, w.Body.String())
	}

	// "" clears it, and the identity goes back to email only.
	w = do(t, h, http.MethodPut, "/me/name", map[string]any{"given_name": ""}, "elyse.messer@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("clear: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if me := decodeMe(t, w.Body.Bytes()); me.GivenName != "" {
		t.Fatalf("after clear, given_name = %q", me.GivenName)
	}
	if got := s.mcpUserFor(ctx, "elyse.messer@x.com"); got.Name != "" {
		t.Fatalf("mcpUserFor after clear = %+v, want no name", got)
	}
}

// TestViewerCannotSetOwnFirstName: /me/name sits behind rejectViewerWrites
// like /me/team, so a read-only viewer reads it but cannot write it.
func TestViewerCannotSetOwnFirstName(t *testing.T) {
	s := memberFixture(t, "eyes@x.com")
	setRole(t, s, "eyes@x.com", "viewer", "")
	h := s.Routes()

	w := do(t, h, http.MethodPut, "/me/name", map[string]any{"given_name": "Eve"}, "eyes@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT /me/name: status %d want 403 (body %s)", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/me/name", nil, "eyes@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer GET /me/name: status %d want 200", w.Code)
	}
}
