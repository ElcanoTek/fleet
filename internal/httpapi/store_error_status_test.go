// Pins the HTTP status each memory / project store failure maps to
// (writeMemoryStoreError). These handlers used to answer 400 for EVERY store
// error — a missing row, a member who is not the owner, and a database fault
// alike — so the UI reported an outage as the user's own bad request.

package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func TestMemoryAndProjectStoreErrorStatuses(t *testing.T) {
	s := memberFixture(t, "owner@x.com", "mate@x.com")
	setRole(t, s, "owner@x.com", "member", "quant")
	setRole(t, s, "mate@x.com", "member", "quant")
	h := s.Routes()
	st := s.concreteStore(t)
	ctx := context.Background()

	// A personal memory that does not exist: 404, not 400.
	if w := do(t, h, http.MethodPatch, "/memories/does-not-exist", map[string]any{"pinned": true}, "owner@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing memory: %d want 404 body=%q", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodDelete, "/memories/does-not-exist", nil, "owner@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing memory: %d want 404 body=%q", w.Code, w.Body.String())
	}
	// Bad input stays a 400.
	mem, err := st.CreateMemory(ctx, "owner@x.com", "remember the spread", "manual", "fact")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	if w := do(t, h, http.MethodPatch, "/memories/"+mem.ID, map[string]any{}, "owner@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH empty patch: %d want 400 body=%q", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPost, "/memories", map[string]any{"content": "   "}, "owner@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("POST blank memory: %d want 400 body=%q", w.Code, w.Body.String())
	}

	// A team-shared project: a member who is not the owner may read it but
	// gets 403 (not 400) on PATCH / DELETE, which are owner-only in the store.
	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: "owner@x.com", Name: "Shared", TeamID: "quant"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if w := do(t, h, http.MethodGet, "/projects/"+proj.ID, nil, "mate@x.com"); w.Code != http.StatusOK {
		t.Fatalf("member GET project: %d body=%q", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPatch, "/projects/"+proj.ID, map[string]any{"name": "Hijacked"}, "mate@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner PATCH project: %d want 403 body=%q", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodDelete, "/projects/"+proj.ID, nil, "mate@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner DELETE project: %d want 403 body=%q", w.Code, w.Body.String())
	}
	// The owner's bad input is still a 400.
	if w := do(t, h, http.MethodPatch, "/projects/"+proj.ID, map[string]any{"name": ""}, "owner@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("owner PATCH blank name: %d want 400 body=%q", w.Code, w.Body.String())
	}
}
