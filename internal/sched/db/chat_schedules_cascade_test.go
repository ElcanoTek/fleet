package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// openBoth opens the scheduler and the chat store on one fresh, empty
// database, in the given order — the order their migration runners run.
func openBoth(t *testing.T, schedFirst bool) (*Database, *store.Store) {
	t.Helper()
	base := testdb.Empty(t)
	d := New()
	if err := d.Init(config.WithSchema(base, config.SchedSchema), PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second}); err != nil {
		t.Fatalf("open sched: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if schedFirst {
		// Since #2049 M6 one runner migrates chat before sched whichever store
		// opens first, so this order exists only on a database a pre-M6
		// release migrated scheduler-first: 077 found no chat.conversations
		// and installed no cascade. Recreate that state.
		if _, err := d.conn.Exec(`DROP TRIGGER conversations_delete_chat_schedules ON chat.conversations;
			DROP FUNCTION chat.conversations_delete_chat_schedules()`); err != nil {
			t.Fatal(err)
		}
	}
	c, err := store.Open(config.WithSchema(base, config.ChatSchema), store.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("open chat: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return d, c
}

func countWhere(t *testing.T, conn *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestChatScheduleCascade_EitherMigrationOrder: whichever store migrates
// first on a fresh database, once fleet serve has started (and run
// EnsureChatScheduleCascade) hard-deleting a conversation removes its
// schedules and their history. On a sched-first database the schedules
// orphaned before the cascade existed are removed when it is installed.
func TestChatScheduleCascade_EitherMigrationOrder(t *testing.T) {
	t.Parallel()
	for _, schedFirst := range []bool{true, false} {
		name := "chat first"
		if schedFirst {
			name = "sched first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d, chat := openBoth(t, schedFirst)
			const user = "owner@elcanotek.com"
			convA, err := chat.CreateConversation(ctx, user, "A", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			convB, err := chat.CreateConversation(ctx, user, "B", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			rows := [][2]string{{"sa", convA.ID}, {"sb", convB.ID}}
			if schedFirst {
				// A schedule whose conversation was deleted while the
				// cascade was missing (only possible in this order).
				rows = append(rows, [2]string{"orphan", "conversation-deleted-earlier"})
			}
			for _, row := range rows {
				if _, err := d.conn.Exec(`INSERT INTO chat_schedules
					(id, conversation_id, user_email, prompt, cron_expr, status, next_run_at, created_at, updated_at)
					VALUES ($1, $2, $3, 'P', '0 9 * * *', 'active', $4, $4, $4)`, row[0], row[1], user, now+3600); err != nil {
					t.Fatal(err)
				}
				if _, err := d.conn.Exec(`INSERT INTO chat_schedule_runs (schedule_id, run_number, trigger_kind, outcome, input_id)
					VALUES ($1, 1, 'schedule', 'queued', 'in')`, row[0]); err != nil {
					t.Fatal(err)
				}
			}

			// fleet serve's startup step.
			installed, orphans, err := d.EnsureChatScheduleCascade(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if schedFirst && (!installed || orphans != 1) {
				t.Fatalf("sched-first: installed=%v orphans=%d; want the cascade installed and the orphan removed", installed, orphans)
			}
			if !schedFirst && (installed || orphans != 0) {
				t.Fatalf("chat-first: installed=%v orphans=%d; want a no-op (077 installed it)", installed, orphans)
			}
			if again, _, err := d.EnsureChatScheduleCascade(ctx); err != nil || again {
				t.Fatalf("second ensure = %v, %v; want a no-op", again, err)
			}

			if err := chat.Delete(ctx, user, convA.ID); err != nil {
				t.Fatal(err)
			}
			if n := countWhere(t, d.conn, `SELECT count(*) FROM chat_schedules WHERE id IN ('sa', 'orphan')`); n != 0 {
				t.Fatalf("%d schedule(s) of deleted conversations left behind", n)
			}
			if n := countWhere(t, d.conn, `SELECT count(*) FROM chat_schedule_runs WHERE schedule_id IN ('sa', 'orphan')`); n != 0 {
				t.Fatalf("%d run-history row(s) of deleted conversations left behind", n)
			}
			if n := countWhere(t, d.conn, `SELECT count(*) FROM chat_schedules WHERE id = 'sb'`); n != 1 {
				t.Fatal("the other conversation's schedule went too")
			}
		})
	}
}

// TestKeepChatScheduleCascade_RetriesUntilInstalled: the startup step keeps
// retrying with a growing backoff while it fails, and its first success
// installs the cascade and removes the schedules orphaned meanwhile.
func TestKeepChatScheduleCascade_RetriesUntilInstalled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, chat := openBoth(t, true) // sched first: 077 could not install it
	conv, err := chat.CreateConversation(ctx, "owner@elcanotek.com", "A", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, row := range [][2]string{{"kept", conv.ID}, {"orphan", "deleted-while-failing"}} {
		if _, err := d.conn.Exec(`INSERT INTO chat_schedules
			(id, conversation_id, user_email, prompt, cron_expr, status, next_run_at, created_at, updated_at)
			VALUES ($1, $2, 'owner@elcanotek.com', 'P', '0 9 * * *', 'active', $3, $3, $3)`, row[0], row[1], now); err != nil {
			t.Fatal(err)
		}
	}
	attempts := 0
	ensure := func(ctx context.Context) (bool, int, error) {
		attempts++
		if attempts <= 3 {
			return false, 0, errors.New("injected: database unavailable")
		}
		return d.EnsureChatScheduleCascade(ctx)
	}
	var waits []time.Duration
	var installed bool
	var removed int
	ok := KeepChatScheduleCascade(ctx, ensure, 10*time.Millisecond, 25*time.Millisecond,
		func(i bool, o int, err error, retryIn time.Duration) {
			if err != nil {
				waits = append(waits, retryIn)
				return
			}
			installed, removed = i, o
		})
	if !ok || attempts != 4 || !installed || removed != 1 {
		t.Fatalf("ok=%v attempts=%d installed=%v removed=%d; want success on the 4th attempt, installing and removing the orphan", ok, attempts, installed, removed)
	}
	if want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}; fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("backoff = %v, want %v (doubling, capped)", waits, want)
	}
	if err := chat.Delete(ctx, "owner@elcanotek.com", conv.ID); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, d.conn, `SELECT count(*) FROM chat_schedules`); n != 0 {
		t.Fatalf("%d schedule(s) left after the conversation was deleted", n)
	}
}
