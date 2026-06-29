package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// seedConvs provisions n conversations for userEmail (membership-seeded) and
// returns their IDs. Used by the bulk-operation handler tests.
func seedConvs(t *testing.T, s *Server, userEmail string, n int) []string {
	t.Helper()
	st := s.concreteStore(t)
	seedUser(t, st, userEmail)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		c, err := st.CreateConversation(context.Background(), userEmail, "", "victoria", "", false)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, c.ID)
	}
	return ids
}

// TestBulkDelete_Targeted proves DELETE /conversations with conversation_ids
// deletes exactly those IDs and returns {"deleted": N}.
func TestBulkDelete_Targeted(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	ids := seedConvs(t, s, "u@x.com", 3)

	w := do(t, h, http.MethodDelete, "/conversations",
		map[string]any{"conversation_ids": ids, "confirm": true}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if deleted, _ := resp["deleted"].(float64); deleted != 3 {
		t.Errorf("deleted = %v, want 3", resp["deleted"])
	}
	// All three are gone.
	for _, id := range ids {
		if g := do(t, h, http.MethodGet, "/conversations/"+id, nil, "u@x.com"); g.Code != http.StatusNotFound {
			t.Errorf("conversation %s not deleted: %d", id, g.Code)
		}
	}
}

// TestBulkDelete_ForeignIDRejected proves a foreign ID in the list returns 403
// and deletes nothing (the whole request is a no-op).
func TestBulkDelete_ForeignIDRejected(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	ownerIDs := seedConvs(t, s, "owner@x.com", 2)
	intruderIDs := seedConvs(t, s, "intruder@x.com", 1)

	req := append(append([]string{}, ownerIDs[0]), intruderIDs[0])
	w := do(t, h, http.MethodDelete, "/conversations",
		map[string]any{"conversation_ids": req, "confirm": true}, "owner@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	// Owner's conversation must survive.
	if g := do(t, h, http.MethodGet, "/conversations/"+ownerIDs[0], nil, "owner@x.com"); g.Code != http.StatusOK {
		t.Errorf("owner conversation deleted by rejected request: %d", g.Code)
	}
}

// TestBulkDelete_TooManyIDs proves the 100-ID cap returns 400.
func TestBulkDelete_TooManyIDs(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	seedConvs(t, s, "u@x.com", 1)

	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "00000000-0000-0000-0000-000000000001"
	}
	w := do(t, h, http.MethodDelete, "/conversations",
		map[string]any{"conversation_ids": ids, "confirm": true}, "u@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestBulkDelete_AllMatchingRequiresConfirm proves all_matching without
// confirm:true is rejected with 400.
func TestBulkDelete_AllMatchingRequiresConfirm(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	seedConvs(t, s, "u@x.com", 1)

	w := do(t, h, http.MethodDelete, "/conversations?folder=Old%20work",
		map[string]any{"all_matching": true, "confirm": false}, "u@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestBulkDelete_AllMatching deletes conversations matching the folder filter.
func TestBulkDelete_AllMatching(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	st := s.concreteStore(t)
	seedUser(t, st, "u@x.com")

	keep, _ := st.CreateConversation(context.Background(), "u@x.com", "", "victoria", "", false)
	gone, _ := st.CreateConversation(context.Background(), "u@x.com", "", "victoria", "", false)
	// Put `gone` in the "Old work" folder.
	if _, err := st.BulkPatch(context.Background(), "u@x.com", []string{gone.ID}, nil, strPtr("Old work"), nil); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	w := do(t, h, http.MethodDelete, "/conversations?folder=Old%20work",
		map[string]any{"all_matching": true, "confirm": true}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if deleted, _ := resp["deleted"].(float64); deleted != 1 {
		t.Errorf("deleted = %v, want 1", resp["deleted"])
	}
	// `keep` survives, `gone` is removed.
	if g := do(t, h, http.MethodGet, "/conversations/"+keep.ID, nil, "u@x.com"); g.Code != http.StatusOK {
		t.Errorf("non-matching conversation was deleted: %d", g.Code)
	}
	if g := do(t, h, http.MethodGet, "/conversations/"+gone.ID, nil, "u@x.com"); g.Code != http.StatusNotFound {
		t.Errorf("matching conversation survived: %d", g.Code)
	}
}

// TestBulkPatch_AppliesChanges proves PATCH /conversations/bulk writes the
// supplied fields to every ID and returns {"updated": N}.
func TestBulkPatch_AppliesChanges(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	ids := seedConvs(t, s, "u@x.com", 2)

	w := do(t, h, http.MethodPatch, "/conversations/bulk", map[string]any{
		"conversation_ids": ids,
		"changes": map[string]any{
			"pinned": true,
			"folder": "Archive",
			"labels": []string{"done"},
		},
	}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if updated, _ := resp["updated"].(float64); updated != 2 {
		t.Errorf("updated = %v, want 2", resp["updated"])
	}
	// Verify the fields landed.
	for _, id := range ids {
		g := do(t, h, http.MethodGet, "/conversations/"+id, nil, "u@x.com")
		if g.Code != http.StatusOK {
			t.Fatalf("get %s: %d", id, g.Code)
		}
		var getResp struct {
			Conversation store.Conversation `json:"conversation"`
		}
		_ = json.Unmarshal(g.Body.Bytes(), &getResp)
		c := getResp.Conversation
		if !c.Pinned || c.Folder != "Archive" || len(c.Labels) != 1 || c.Labels[0] != "done" {
			t.Errorf("conversation %s not patched: %+v", id, c)
		}
	}
}

// TestBulkPatch_ForeignIDRejected proves a foreign ID in the bulk-patch list
// returns 403 and rolls the transaction back (no row is mutated).
func TestBulkPatch_ForeignIDRejected(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	ownerIDs := seedConvs(t, s, "owner@x.com", 1)
	intruderIDs := seedConvs(t, s, "intruder@x.com", 1)

	w := do(t, h, http.MethodPatch, "/conversations/bulk", map[string]any{
		"conversation_ids": []string{ownerIDs[0], intruderIDs[0]},
		"changes":          map[string]any{"pinned": true},
	}, "owner@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	// Owner's conversation must be untouched (pinned still false).
	g := do(t, h, http.MethodGet, "/conversations/"+ownerIDs[0], nil, "owner@x.com")
	var getResp struct {
		Conversation store.Conversation `json:"conversation"`
	}
	_ = json.Unmarshal(g.Body.Bytes(), &getResp)
	if getResp.Conversation.Pinned {
		t.Errorf("owner conversation mutated by rejected bulk patch")
	}
}

// strPtr is a tiny helper to take the address of a string literal.
func strPtr(s string) *string { return &s }
