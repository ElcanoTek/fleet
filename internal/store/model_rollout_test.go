package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func TestRolloutConversationModel(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	// 1. Active conversation with messages on Argon -> updated + notified, updated_at preserved.
	activeConv, err := s.CreateConversation(ctx, "alice@example.com", "Q4 pacing", "victoria", "google/gemini-4-argon", false)
	if err != nil {
		t.Fatalf("CreateConversation(active): %v", err)
	}
	userText, _ := json.Marshal(agent.TextContent{Text: "How is Q4 pacing?"})
	asstText, _ := json.Marshal(agent.TextContent{Text: "On track."})
	if _, err := s.AppendHistory(ctx, activeConv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: userText},
		{Role: "assistant", Type: "text", Content: asstText},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	beforeActive, err := s.Get(ctx, "alice@example.com", activeConv.ID)
	if err != nil || beforeActive == nil {
		t.Fatalf("Get(before): %v", err)
	}

	// 2. Empty conversation on Argon -> updated, NOT notified (no spurious notice in empty draft).
	emptyConv, err := s.CreateConversation(ctx, "bob@example.com", "New chat", "victoria", "google/gemini-4-argon", false)
	if err != nil {
		t.Fatalf("CreateConversation(empty): %v", err)
	}

	// 3. Conversation already on Hydrogen -> untouched, NOT notified.
	alreadyUpgraded, err := s.CreateConversation(ctx, "carol@example.com", "Current chat", "victoria", "google/gemini-4-hydrogen", false)
	if err != nil {
		t.Fatalf("CreateConversation(already): %v", err)
	}
	if _, err := s.AppendHistory(ctx, alreadyUpgraded.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: userText},
	}); err != nil {
		t.Fatalf("AppendHistory(already): %v", err)
	}

	// 4. Lockdown conversation -> skipped when includeLockdown=false.
	lockdownConv, err := s.CreateConversation(ctx, "dave@example.com", "Lockdown", "victoria", "google/gemini-4-argon", true)
	if err != nil {
		t.Fatalf("CreateConversation(lockdown): %v", err)
	}

	res, err := s.RolloutConversationModel(ctx, "google/gemini-4-hydrogen", "", false, true)
	if err != nil {
		t.Fatalf("RolloutConversationModel: %v", err)
	}
	if res.MatchedCount != 3 || res.UpdatedCount != 2 || res.NotifiedCount != 1 {
		t.Fatalf("got result %+v, want Matched=3 Updated=2 Notified=1", res)
	}

	afterActive, err := s.Get(ctx, "alice@example.com", activeConv.ID)
	if err != nil || afterActive == nil {
		t.Fatalf("Get(afterActive): %v", err)
	}
	if afterActive.Model != "google/gemini-4-hydrogen" {
		t.Errorf("active model = %q, want google/gemini-4-hydrogen", afterActive.Model)
	}
	if afterActive.UpdatedAt != beforeActive.UpdatedAt {
		t.Errorf("updated_at mutated: got %d, want %d", afterActive.UpdatedAt, beforeActive.UpdatedAt)
	}

	hist, err := s.LoadHistory(ctx, activeConv.ID)
	if err != nil {
		t.Fatalf("LoadHistory(active): %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("active history len = %d, want 3", len(hist))
	}
	last := hist[len(hist)-1]
	if last.Type != agent.EntryTypeNotice || last.Role != "system" {
		t.Fatalf("last entry = (%q, %q), want (system, notice)", last.Role, last.Type)
	}
	var nc agent.NoticeContent
	if err := json.Unmarshal(last.Content, &nc); err != nil {
		t.Fatalf("unmarshal notice: %v", err)
	}
	if nc.Kind != agent.NoticeKindModelUpgraded || nc.FromModel != "google/gemini-4-argon" || nc.ToModel != "google/gemini-4-hydrogen" {
		t.Errorf("notice content = %+v", nc)
	}

	emptyHist, err := s.LoadHistory(ctx, emptyConv.ID)
	if err != nil {
		t.Fatalf("LoadHistory(empty): %v", err)
	}
	if len(emptyHist) != 0 {
		t.Errorf("empty conversation should not get a notice; got %d entries", len(emptyHist))
	}

	afterLockdown, err := s.Get(ctx, "dave@example.com", lockdownConv.ID)
	if err != nil || afterLockdown == nil {
		t.Fatalf("Get(lockdown): %v", err)
	}
	if afterLockdown.Model != "google/gemini-4-argon" {
		t.Errorf("lockdown model = %q, want google/gemini-4-argon when includeLockdown=false", afterLockdown.Model)
	}

	// Re-running the exact same rollout is a zero-write idempotent no-op.
	res2, err := s.RolloutConversationModel(ctx, "google/gemini-4-hydrogen", "", false, true)
	if err != nil {
		t.Fatalf("second RolloutConversationModel: %v", err)
	}
	if res2.UpdatedCount != 0 || res2.NotifiedCount != 0 {
		t.Errorf("idempotent re-run mutated %+v, want 0 updates and 0 notices", res2)
	}
}

func TestAcquireModelRolloutLock_SerializesAcrossConnections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	release1, err := s.AcquireModelRolloutLock(ctx)
	if err != nil {
		t.Fatalf("AcquireModelRolloutLock(1): %v", err)
	}
	defer release1()

	acquired2 := make(chan func(), 1)
	errCh := make(chan error, 1)
	go func() {
		r2, lerr := s.AcquireModelRolloutLock(ctx)
		if lerr != nil {
			errCh <- lerr
			return
		}
		acquired2 <- r2
	}()

	select {
	case r2 := <-acquired2:
		r2()
		t.Fatal("second AcquireModelRolloutLock succeeded while first lock was still held")
	case lerr := <-errCh:
		t.Fatalf("second AcquireModelRolloutLock errored: %v", lerr)
	case <-time.After(100 * time.Millisecond):
		// Expected: second caller is blocked on pg_advisory_lock.
	}

	release1()

	select {
	case r2 := <-acquired2:
		r2()
	case lerr := <-errCh:
		t.Fatalf("second AcquireModelRolloutLock errored after release: %v", lerr)
	case <-time.After(5 * time.Second):
		t.Fatal("second AcquireModelRolloutLock did not unblock after first lock released")
	}
}
