package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// TestHandlePermissionDecision_RejectsNonOwner asserts the permission-decision
// route is owner-scoped like its siblings: a member who is not the conversation
// owner gets 404 and cannot resolve the prompt, even with a valid/guessable
// requestId. The owner passes the ownership gate (200; resolved:false here since
// there is no matching pending request).
func TestHandlePermissionDecision_RejectsNonOwner(t *testing.T) {
	s := serverFixture(t)
	s.permissions = newPermissionRegistry() // fixture leaves it nil; the owner path reaches resolve
	st := s.concreteStore(t)
	seedUser(t, st, "owner@example.com")
	seedUser(t, st, "attacker@example.com")

	conv, err := st.CreateConversation(context.Background(), "owner@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	h := s.Routes()
	path := "/conversations/" + conv.ID + "/permissions/perm-1"
	body := map[string]any{"allowed": true}

	if w := do(t, h, http.MethodPost, path, body, "attacker@example.com"); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner decision status = %d, want 404 (must not resolve another user's prompt)", w.Code)
	}
	if w := do(t, h, http.MethodPost, path, body, "owner@example.com"); w.Code != http.StatusOK {
		t.Fatalf("owner decision status = %d, want 200", w.Code)
	}
}
