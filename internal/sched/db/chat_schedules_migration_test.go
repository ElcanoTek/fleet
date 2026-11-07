package db

import (
	"context"
	"database/sql"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// chatScheduleRow is every column the chat table had (chat migration 086).
type chatScheduleRow struct {
	ID, ConversationID, UserEmail, Title, Prompt, CronExpr, Timezone string
	DurationSeconds                                                  int
	Status                                                           string
	NextRunAt                                                        int64
	LastRunAt                                                        sql.NullInt64
	RunCount, MaxRuns                                                int
	CreatedAt, UpdatedAt                                             int64
}

func readChatScheduleRows(t *testing.T, conn *sql.DB) []chatScheduleRow {
	t.Helper()
	rows, err := conn.Query(`SELECT id, conversation_id, user_email, title, prompt, cron_expr, timezone,
		duration_seconds, status, next_run_at, last_run_at, run_count, max_runs, created_at, updated_at
		FROM chat_schedules ORDER BY id`)
	if err != nil {
		t.Fatalf("read chat_schedules: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chatScheduleRow
	for rows.Next() {
		var r chatScheduleRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.UserEmail, &r.Title, &r.Prompt, &r.CronExpr, &r.Timezone,
			&r.DurationSeconds, &r.Status, &r.NextRunAt, &r.LastRunAt, &r.RunCount, &r.MaxRuns, &r.CreatedAt, &r.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// migrationsThrough is the embedded sched migration set up to and including
// version last, as a directory "m" for migrateUp.
func migrationsThrough(t *testing.T, last int) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || v > last {
			continue
		}
		data, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out["m/"+e.Name()] = &fstest.MapFile{Data: data}
	}
	return out
}

// The old-shape rows seedOldChatSchedules writes.
const (
	ownerMv, teammateMv = "owner@elcanotek.com", "mate@elcanotek.com"
	dueID               = "11111111-1111-4111-8111-111111111111" // active patrol, due during the upgrade
	futureID            = "22222222-2222-4222-8222-222222222222" // active patrol, not due yet
	pausedID            = "33333333-3333-4333-8333-333333333333"
	timerID             = "44444444-4444-4444-8444-444444444444" // active one-shot timer
	completedID         = "55555555-5555-4555-8555-555555555555"
	cancelledID         = "66666666-6666-4666-8666-666666666666"
)

// seedOldChatSchedules writes chat.chat_schedules rows of every status and
// kind, and the patrol turns of the due one in chat.chat_input_queue — plus
// queue keys the history backfill must ignore (not a run number; an unknown
// schedule; a user's own input).
func seedOldChatSchedules(t *testing.T, chatConn *sql.DB, convA, convB string, now int64) {
	t.Helper()
	owner, teammate := ownerMv, teammateMv
	seed := make([]string, 0, 12)
	seed = append(seed,
		`INSERT INTO chat_schedules VALUES ('`+dueID+`', '`+convA+`', '`+owner+`', 'Deal sweep', 'Check pacing.', '0 */2 * * *', 'America/New_York', 0, 'active', `+itoa(now-90)+`, `+itoa(now-7200)+`, 7, 0, `+itoa(now-86400)+`, `+itoa(now-7200)+`)`,
		`INSERT INTO chat_schedules VALUES ('`+futureID+`', '`+convA+`', '`+owner+`', 'Weekly', 'Summarize.', '0 9 * * 1', 'Europe/London', 0, 'active', `+itoa(now+86400)+`, NULL, 0, 4, `+itoa(now-600)+`, `+itoa(now-600)+`)`,
		`INSERT INTO chat_schedules VALUES ('`+pausedID+`', '`+convB+`', '`+teammate+`', 'Inbox triage', 'Triage.', '*/30 * * * *', 'UTC', 0, 'paused', `+itoa(now-30)+`, `+itoa(now-4000)+`, 3, 10, `+itoa(now-90000)+`, `+itoa(now-100)+`)`,
		`INSERT INTO chat_schedules VALUES ('`+timerID+`', '`+convB+`', '`+teammate+`', 'Backfill check', 'Verify.', '', 'UTC', 1800, 'active', `+itoa(now+1200)+`, NULL, 0, 1, `+itoa(now-600)+`, `+itoa(now-600)+`)`,
		`INSERT INTO chat_schedules VALUES ('`+completedID+`', '`+convB+`', '`+teammate+`', 'Done', 'Once.', '', 'UTC', 60, 'completed', `+itoa(now-5000)+`, `+itoa(now-5000)+`, 1, 1, `+itoa(now-6000)+`, `+itoa(now-5000)+`)`,
		`INSERT INTO chat_schedules VALUES ('`+cancelledID+`', '`+convA+`', '`+owner+`', 'Old', 'Old.', '0 8 * * *', 'UTC', 0, 'cancelled', `+itoa(now+3600)+`, NULL, 0, 0, `+itoa(now-9000)+`, `+itoa(now-8000)+`)`,
	)
	inputs := [][2]string{
		{"in-6", "chat-patrol:" + dueID + ":6"},
		{"in-7", "chat-patrol:" + dueID + ":7"},
		{"in-7m", "chat-patrol:" + dueID + ":7:" + itoa(now-3000)},
		{"in-junk", "chat-patrol:" + dueID + ":x"},
		{"in-ghost", "chat-patrol:99999999-9999-4999-8999-999999999999:1"},
		{"in-user", "ui-123"},
	}
	for i, in := range inputs {
		seed = append(seed, `INSERT INTO chat_input_queue (id, conversation_id, user_email, client_input_id, message, attachments, mode, state, position, created_at, updated_at)
			VALUES ('`+in[0]+`', '`+convA+`', '`+owner+`', '`+in[1]+`', 'm', '[]', 'patrol', 'completed', `+itoa(int64(i+1))+`, `+itoa(now-int64(10000-i))+`, `+itoa(now)+`)`)
	}
	for _, q := range seed {
		if _, err := chatConn.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

// assertBackfilledHistory: the due patrol's runs 6 and 7 and its Run now,
// and nothing else.
func assertBackfilledHistory(t *testing.T, d *Database) {
	t.Helper()
	hist, err := d.ListChatScheduleRuns(context.Background(), dueID, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(hist))
	for _, r := range hist {
		got = append(got, itoa(int64(r.RunNumber))+"/"+string(r.Trigger)+"/"+string(r.Outcome)+"/"+r.InputID)
	}
	want := []string{"7/manual/queued/in-7m", "7/schedule/queued/in-7", "6/schedule/queued/in-6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backfilled history = %v, want %v", got, want)
	}
	var total int
	if err := d.conn.QueryRow(`SELECT count(*) FROM chat_schedule_runs`).Scan(&total); err != nil || total != 3 {
		t.Fatalf("history rows = %d (err %v), want 3", total, err)
	}
}

// TestMigration077_MovesChatSchedules is the upgrade of a production box
// (#2049 M5): the chat store holds chat.chat_schedules (old shape) and its
// patrol turns in chat.chat_input_queue, the scheduler is at 076 (its empty
// chat schedule tables). Migration 077 must move every schedule over losslessly, start each run history from
// the queue, drop the chat table, be safe to re-run, round-trip through its
// down migration — and the first engine pass after the upgrade must fire a
// slot that fell due during the upgrade exactly once, with the run number the
// old engine would have used.
func TestMigration077_MovesChatSchedules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := testdb.Empty(t)

	// The chat store, fully migrated: chat.chat_schedules exists (086, 096).
	chat, err := store.Open(config.WithSchema(base, config.ChatSchema), store.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("open chat: %v", err)
	}
	t.Cleanup(func() { _ = chat.Close() })
	convA, err := chat.CreateConversation(ctx, ownerMv, "Deals", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	convB, err := chat.CreateConversation(ctx, teammateMv, "Inbox", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	chatConn, err := sql.Open("pgx", config.WithSchema(base, config.ChatSchema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chatConn.Close() })

	now := time.Now().Unix()
	seedOldChatSchedules(t, chatConn, convA.ID, convB.ID, now)
	before := readChatScheduleRows(t, chatConn)

	// The scheduler at 076 (the engine's tables, still empty), then the move.
	schedConn, err := sql.Open("pgx", config.WithSchema(base, config.SchedSchema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = schedConn.Close() })
	if err := migrateUp(ctx, schedConn, migrationsThrough(t, 76), "m"); err != nil {
		t.Fatalf("migrate to 076: %v", err)
	}
	if err := RunMigrations(schedConn); err != nil {
		t.Fatalf("migrate to 077: %v", err)
	}

	// Moved verbatim, and gone from the chat schema.
	if after := readChatScheduleRows(t, schedConn); !reflect.DeepEqual(after, before) {
		t.Fatalf("moved rows differ:\nbefore %+v\nafter  %+v", before, after)
	}
	var reg sql.NullString
	if err := schedConn.QueryRow(`SELECT to_regclass('chat.chat_schedules')::text`).Scan(&reg); err != nil || reg.Valid {
		t.Fatalf("chat.chat_schedules should be dropped (to_regclass = %v, err %v)", reg, err)
	}
	var leased int
	if err := schedConn.QueryRow(`SELECT count(*) FROM chat_schedules WHERE lease_owner IS NOT NULL OR lease_expires_at IS NOT NULL
		OR fire_id IS NOT NULL OR fire_trigger IS NOT NULL OR fire_run_number IS NOT NULL OR fire_failures <> 0`).Scan(&leased); err != nil || leased != 0 {
		t.Fatalf("moved rows must start unleased (leased = %d, err %v)", leased, err)
	}

	// Run history starts from the queue.
	d := &Database{conn: schedConn}
	assertBackfilledHistory(t, d)

	// Re-running the migration's SQL is a no-op.
	up, err := fs.ReadFile(migrationsFS, "migrations/077_move_chat_schedules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedConn.Exec(string(up)); err != nil {
		t.Fatalf("re-run 077: %v", err)
	}
	if after := readChatScheduleRows(t, schedConn); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-run changed rows: %+v", after)
	}

	// The down migration puts the old table back as it was; up again moves it
	// back and rebuilds the same history.
	down, err := fs.ReadFile(migrationsFS, "migrations/077_move_chat_schedules.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedConn.Exec(string(down)); err != nil {
		t.Fatalf("077 down: %v", err)
	}
	if back := readChatScheduleRows(t, chatConn); !reflect.DeepEqual(back, before) {
		t.Fatalf("down migration rows differ:\nbefore %+v\nafter  %+v", before, back)
	}
	if _, err := schedConn.Exec(string(up)); err != nil {
		t.Fatalf("077 up after down: %v", err)
	}
	if after := readChatScheduleRows(t, schedConn); !reflect.DeepEqual(after, before) {
		t.Fatalf("up after down differs: %+v", after)
	}
	assertBackfilledHistory(t, d)

	// The first engine pass after the upgrade: the slot that fell due during
	// it fires once, as run 8 (the old engine's next number, so its key
	// cannot collide with run 7's turn); nothing else is due; a second pass
	// while the fire is in flight claims nothing.
	fires, err := d.ClaimDueChatSchedules(ctx, time.Unix(now, 0), time.Minute, 25, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fires) != 1 || fires[0].ScheduleID != dueID || fires[0].RunNumber != 8 ||
		fires[0].Timezone != "America/New_York" || fires[0].Trigger != models.ChatScheduleTriggerSchedule {
		t.Fatalf("first pass fires = %+v, want only %s as run 8", fires, dueID)
	}
	if key := store.ChatScheduleInputKey(fires[0]); key != "chat-patrol:"+dueID+":8:"+fires[0].FireID {
		t.Fatalf("idempotency key = %q", key)
	}
	if again, err := d.ClaimDueChatSchedules(ctx, time.Unix(now, 0), time.Minute, 25, nil); err != nil || len(again) != 0 {
		t.Fatalf("second pass while leased = %+v, %v; want nothing", again, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
