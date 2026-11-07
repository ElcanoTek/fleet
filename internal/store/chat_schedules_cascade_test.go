package store

import (
	"context"
	"testing"
	"time"
)

// TestConversationHardDeleteDropsChatSchedules pins chat migration 099: every
// path that hard-deletes a conversation takes its chat schedules (live and
// terminal) and their run history with it, now that sched.chat_schedules
// cannot carry a foreign key to conversations.
func TestConversationHardDeleteDropsChatSchedules(t *testing.T) {
	t.Parallel()
	const user = "deleter@elcanotek.com"
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()

	// scheduled makes a conversation holding a cancelled patrol with a run in
	// its history (a cancelled schedule does not exempt the chat from any
	// sweep) and returns the conversation id.
	scheduled := func(t *testing.T, s *Store, title string) string {
		t.Helper()
		ctx := context.Background()
		conv, err := s.CreateConversation(ctx, user, title, "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
			ConversationID: conv.ID, UserEmail: user, Prompt: "Secret instructions.", CronExpr: "0 9 * * *", AllowMultiple: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CancelChatSchedule(ctx, user, conv.ID, sc.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO sched.chat_schedule_runs (schedule_id, run_number, trigger_kind, outcome)
			VALUES ($1, 1, 'schedule', 'busy')`, sc.ID); err != nil {
			t.Fatal(err)
		}
		return conv.ID
	}
	age := func(t *testing.T, s *Store, convID string) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE conversations SET updated_at = $1 WHERE id = $2`, old, convID); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func(t *testing.T, s *Store, convID string) (schedules, runs int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT count(*) FROM sched.chat_schedules WHERE conversation_id = $1`, convID).Scan(&schedules); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM sched.chat_schedule_runs r
			LEFT JOIN sched.chat_schedules s ON s.id = r.schedule_id WHERE s.id IS NULL OR s.conversation_id = $1`, convID).Scan(&runs); err != nil {
			t.Fatal(err)
		}
		return schedules, runs
	}

	cases := []struct {
		name   string
		delete func(t *testing.T, s *Store, convID string)
	}{
		{"Delete", func(t *testing.T, s *Store, convID string) {
			if err := s.Delete(context.Background(), user, convID); err != nil {
				t.Fatal(err)
			}
		}},
		{"DeleteByIDs", func(t *testing.T, s *Store, convID string) {
			if _, err := s.DeleteByIDs(context.Background(), user, []string{convID}); err != nil {
				t.Fatal(err)
			}
		}},
		{"DeleteAllUnpinned", func(t *testing.T, s *Store, _ string) {
			if _, err := s.DeleteAllUnpinned(context.Background(), user); err != nil {
				t.Fatal(err)
			}
		}},
		{"TTL sweep", func(t *testing.T, s *Store, convID string) {
			age(t, s, convID)
			if _, _, err := s.SweepExpired(context.Background(), 24*time.Hour, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"cap eviction", func(t *testing.T, s *Store, convID string) {
			age(t, s, convID)
			if _, err := s.CreateConversation(context.Background(), user, "newer", "victoria", "", false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.SweepExpired(context.Background(), 365*24*time.Hour, 1); err != nil {
				t.Fatal(err)
			}
		}},
		{"soft-delete purge", func(t *testing.T, s *Store, convID string) {
			s.SetSoftDelete(true)
			if err := s.Delete(context.Background(), user, convID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE conversations SET deleted_at = NOW() - INTERVAL '31 days' WHERE id = $1`, convID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.SweepExpired(context.Background(), 365*24*time.Hour, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"DeleteUser", func(t *testing.T, s *Store, _ string) {
			if err := s.DeleteUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			if _, err := s.CreateUser(context.Background(), user, "test-password-123"); err != nil {
				t.Fatal(err)
			}
			convID := scheduled(t, s, tc.name)
			if n, r := remaining(t, s, convID); n != 1 || r != 1 {
				t.Fatalf("setup: %d schedules, %d runs", n, r)
			}
			tc.delete(t, s, convID)
			var gone bool
			if err := s.db.QueryRow(`SELECT NOT EXISTS (SELECT 1 FROM conversations WHERE id = $1)`, convID).Scan(&gone); err != nil || !gone {
				t.Fatalf("%s did not hard-delete the conversation (err %v)", tc.name, err)
			}
			if n, r := remaining(t, s, convID); n != 0 || r != 0 {
				t.Fatalf("after %s: %d schedules and %d runs left behind", tc.name, n, r)
			}
		})
	}
}

// TestDeleteUserWithoutSchedSchema: the chat-only CLI verbs (fleet chat user
// del, fleet admin rm) may erase an account on a database whose scheduler
// schema does not exist yet; erasure must not fail on the missing table.
func TestDeleteUserWithoutSchedSchema(t *testing.T) {
	t.Parallel()
	s, err := Open(emptyTestDSN(t), testPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	const user = "chatonly@elcanotek.com"
	if _, err := s.CreateUser(ctx, user, "test-password-123"); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, user, "c", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, user, conv.ID); err != nil {
		t.Fatalf("Delete on a chat-only database: %v", err)
	}
	if err := s.DeleteUser(ctx, user); err != nil {
		t.Fatalf("DeleteUser on a chat-only database: %v", err)
	}
}
