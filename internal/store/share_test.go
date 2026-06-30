package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// seedSharedConv creates a conversation with two text messages for the share
// tests and returns it.
func seedSharedConv(t *testing.T, s *Store, owner string) *Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, owner, "Sorting in Go", "victoria", "openai/gpt-5", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	entries := []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"how do I sort a slice?"}`)},
		{Role: "assistant", Type: "reasoning", Content: json.RawMessage(`{"text":"internal chain of thought"}`)},
		// A tool_result whose text would be sensitive to expose publicly.
		{Role: "tool", Type: "tool_result", Content: json.RawMessage(`{"text":"SECRET_TOOL_OUTPUT","name":"run_python"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"use sort.Slice"}`)},
	}
	if err := s.AppendHistory(ctx, conv.ID, entries); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	return conv
}

// TestShareToken_RoundTrip: issuing a token exposes a read-only snapshot via the
// token (with messages, sans id/user_email), and the owner's Get/List carry the
// token so the UI can show a badge + copy-link (#226).
func TestShareToken_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := seedSharedConv(t, s, "alice@example.com")

	if err := s.SetShareToken(ctx, "alice@example.com", conv.ID, "tok-abc", nil); err != nil {
		t.Fatalf("SetShareToken: %v", err)
	}

	snap, err := s.GetConversationByShareToken(ctx, "tok-abc", time.Now().Unix())
	if err != nil {
		t.Fatalf("GetConversationByShareToken: %v", err)
	}
	if snap == nil {
		t.Fatal("expected a snapshot for a valid token, got nil")
	}
	if snap.Title != "Sorting in Go" || snap.Model != "openai/gpt-5" || snap.Persona != "victoria" {
		t.Errorf("snapshot metadata wrong: %+v", snap)
	}
	// Only the two user/assistant TEXT entries are exposed; the reasoning and
	// tool_result entries are filtered out server-side (security boundary).
	if len(snap.Messages) != 2 {
		t.Fatalf("snapshot messages = %d, want 2 (text only; tool_result + reasoning must be filtered)", len(snap.Messages))
	}
	for _, m := range snap.Messages {
		if m.Type != "text" || (m.Role != "user" && m.Role != "assistant") {
			t.Errorf("snapshot leaked a non-text entry: role=%s type=%s", m.Role, m.Type)
		}
		if strings.Contains(string(m.Content), "SECRET_TOOL_OUTPUT") {
			t.Error("snapshot leaked tool_result content — tool internals must not be shared")
		}
		// The public snapshot must omit the internal messages.id (#226): LoadHistory
		// populates HistoryEntry.ID for the owner's branching flow (#454), but it
		// must be zeroed here so it never reaches an anonymous viewer.
		if m.ID != 0 {
			t.Errorf("snapshot leaked internal messages.id %d — #226 omits internal identifiers", m.ID)
		}
	}
	// Belt-and-suspenders: the marshaled public JSON must carry no "id" field.
	if blob, err := json.Marshal(snap); err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	} else if strings.Contains(string(blob), `"id"`) {
		t.Errorf("public snapshot JSON exposes an id field: %s", blob)
	}
	if snap.SharedAt == 0 {
		t.Error("snapshot SharedAt not set")
	}

	// Owner's own reads carry the token (for the badge + copy-link).
	got, err := s.Get(ctx, "alice@example.com", conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ShareToken != "tok-abc" {
		t.Errorf("Get ShareToken = %q, want tok-abc", got.ShareToken)
	}
	list, err := s.List(ctx, "alice@example.com", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ShareToken != "tok-abc" {
		t.Errorf("List did not carry ShareToken: %+v", list)
	}
}

// TestShareToken_ExpiredRevokedUnknown: each of an expired token, a revoked
// token, and an unknown token resolves to (nil, nil) — indistinguishable so a
// probe can't tell which (#226).
func TestShareToken_ExpiredRevokedUnknown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv := seedSharedConv(t, s, "alice@example.com")
	now := time.Now().Unix()

	// Expired.
	past := now - 10
	if err := s.SetShareToken(ctx, "alice@example.com", conv.ID, "tok-exp", &past); err != nil {
		t.Fatalf("SetShareToken(expired): %v", err)
	}
	if snap, err := s.GetConversationByShareToken(ctx, "tok-exp", now); err != nil || snap != nil {
		t.Errorf("expired token: got (%v, %v), want (nil, nil)", snap, err)
	}

	// Re-share with no expiry, then revoke → nil.
	if err := s.SetShareToken(ctx, "alice@example.com", conv.ID, "tok-live", nil); err != nil {
		t.Fatalf("SetShareToken(live): %v", err)
	}
	if snap, _ := s.GetConversationByShareToken(ctx, "tok-live", now); snap == nil {
		t.Fatal("live token should resolve before revoke")
	}
	if err := s.RevokeShareToken(ctx, "alice@example.com", conv.ID); err != nil {
		t.Fatalf("RevokeShareToken: %v", err)
	}
	if snap, err := s.GetConversationByShareToken(ctx, "tok-live", now); err != nil || snap != nil {
		t.Errorf("revoked token: got (%v, %v), want (nil, nil)", snap, err)
	}

	// Unknown token.
	if snap, err := s.GetConversationByShareToken(ctx, "never-issued", now); err != nil || snap != nil {
		t.Errorf("unknown token: got (%v, %v), want (nil, nil)", snap, err)
	}

	// Revoke is idempotent: a second revoke of an already-unshared conversation
	// still succeeds (the handler relies on this to answer 204 either way).
	if err := s.RevokeShareToken(ctx, "alice@example.com", conv.ID); err != nil {
		t.Errorf("second RevokeShareToken should be a no-op success, got %v", err)
	}
}

// TestSweepExpired_PreservesSharedConversation is the audit the issue called
// for: a shared link must NOT be silently revoked by the retention sweep
// deleting its (unpinned, old) conversation. A backdated unshared conversation
// is still swept (control), proving the exemption is the share token (#226).
func TestSweepExpired_PreservesSharedConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	backdate := time.Now().Add(-30 * 24 * time.Hour).Unix()

	shared := seedSharedConv(t, s, "alice@example.com")
	if err := s.SetShareToken(ctx, "alice@example.com", shared.ID, "tok-keep", nil); err != nil {
		t.Fatalf("SetShareToken: %v", err)
	}
	control, err := s.CreateConversation(ctx, "alice@example.com", "old chat", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation(control): %v", err)
	}
	// Backdate both well past the TTL.
	for _, id := range []string{shared.ID, control.ID} {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, backdate, id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	if _, _, err := s.SweepExpired(ctx, 14*24*time.Hour, 100); err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}

	// Shared conversation survives; its link still resolves.
	if snap, _ := s.GetConversationByShareToken(ctx, "tok-keep", time.Now().Unix()); snap == nil {
		t.Error("shared conversation was swept — the share link broke (sweep must exempt share_token IS NOT NULL)")
	}
	// Control (unshared, equally old, unpinned) is gone.
	if got, _ := s.Get(ctx, "alice@example.com", control.ID); got != nil {
		t.Error("control conversation should have been swept by TTL")
	}
}
