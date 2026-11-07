package db

// DB-backed tests for the a2a_push_configs table (#1279 Phase 2, migration
// 066): sealed round-trips, the client-id upsert contract, idempotent delete,
// and the work-scan/mark cycle the push dispatcher runs. Gated on
// DATABASE_URL like the rest of the package.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/secretbox"
)

func cleanA2APush(t *testing.T, db *Database) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM a2a_push_configs"); err != nil {
		t.Fatalf("clean a2a_push_configs: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM tasks"); err != nil {
		t.Fatalf("clean tasks: %v", err)
	}
}

func testPushCipher(t *testing.T) *secretbox.Cipher {
	t.Helper()
	key := make([]byte, secretbox.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := secretbox.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func seedPushTask(t *testing.T, db *Database, status models.TaskStatus) uuid.UUID {
	t.Helper()
	task := &models.Task{
		ID: uuid.New(), Prompt: "push fixture", Status: status,
		CreatedAt: time.Now().UTC(), Timezone: "UTC",
	}
	if err := db.AddTask(context.Background(), task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task.ID
}

func TestA2APushConfigSealedRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	db.SetA2APushCipher(testPushCipher(t))
	ctx := context.Background()
	taskID := seedPushTask(t, db, models.TaskStatusSuccess) // terminal on purpose: configs must be creatable there

	stored, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "client-chosen-id", URL: "https://example.com/hook",
		Token: "not-a-real-token-value", AuthScheme: "Bearer", AuthCredentials: "not-a-real-credential",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if stored.ID != "client-chosen-id" {
		t.Fatalf("client-supplied id must round-trip, got %q", stored.ID)
	}

	// The secrets live sealed: the raw row must not contain the plaintext.
	var tokenSealed []byte
	if err := db.conn.QueryRowContext(ctx,
		"SELECT token_sealed FROM a2a_push_configs WHERE task_id=$1 AND id=$2", taskID, stored.ID).
		Scan(&tokenSealed); err != nil {
		t.Fatal(err)
	}
	if string(tokenSealed) == "not-a-real-token-value" || len(tokenSealed) == 0 {
		t.Fatalf("token must be stored sealed, got %d bytes", len(tokenSealed))
	}

	got, err := db.GetA2APushConfig(ctx, taskID, "client-chosen-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Token != "not-a-real-token-value" || got.AuthScheme != "Bearer" || got.AuthCredentials != "not-a-real-credential" {
		t.Fatalf("unseal round-trip wrong: %+v", got)
	}

	// Same id again = update, not conflict; and the delivery marker resets.
	if _, err := db.MarkA2APushAttempted(ctx, taskID, stored.ID, models.TaskStatusSuccess); err != nil {
		t.Fatal(err)
	}
	upd, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "client-chosen-id", URL: "https://example.com/hook2",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if upd.URL != "https://example.com/hook2" || upd.LastPushedStatus != nil {
		t.Fatalf("upsert must update and reset the marker: %+v", upd)
	}

	// Minted id when the client gave none; both configs list oldest-first.
	minted, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{TaskID: taskID, URL: "https://example.com/hook3"})
	if err != nil {
		t.Fatal(err)
	}
	if minted.ID == "" {
		t.Fatal("server must mint an id when none is supplied")
	}
	all, err := db.ListA2APushConfigs(ctx, taskID)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %d", err, len(all))
	}

	// Delete is idempotent; a deleted config is gone.
	if err := db.DeleteA2APushConfig(ctx, taskID, "client-chosen-id"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteA2APushConfig(ctx, taskID, "client-chosen-id"); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if _, err := db.GetA2APushConfig(ctx, taskID, "client-chosen-id"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted config must be ErrNoRows, got %v", err)
	}
}

func TestA2APushStoreFailsClosedWithoutCipher(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	db.SetA2APushCipher(nil)
	taskID := seedPushTask(t, db, models.TaskStatusPending)

	_, err := db.UpsertA2APushConfig(context.Background(), models.A2APushConfig{
		TaskID: taskID, URL: "https://example.com/hook", Token: "not-a-real-token-value",
	})
	if !errors.Is(err, ErrA2APushCipherMissing) {
		t.Fatalf("secret storage without a cipher must fail closed, got %v", err)
	}
	// A secret-free config is storable (nothing to seal)...
	if _, err := db.UpsertA2APushConfig(context.Background(), models.A2APushConfig{
		TaskID: taskID, URL: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("secret-free config should store: %v", err)
	}
}

func TestA2APushWorkScanAndMark(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	db.SetA2APushCipher(testPushCipher(t))
	ctx := context.Background()
	taskID := seedPushTask(t, db, models.TaskStatusPending)

	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "w1", URL: "https://example.com/hook", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh config on a live task: one due delivery for the CURRENT status,
	// with the secrets unsealed for the sender.
	work, err := db.ListA2APushWork(ctx, 10)
	if err != nil || len(work) != 1 {
		t.Fatalf("work: %v %d", err, len(work))
	}
	if work[0].Status != models.TaskStatusPending || work[0].Config.Token != "not-a-real-token-value" {
		t.Fatalf("work row wrong: %+v", work[0])
	}

	// The one-winner claim: first mark applies, an identical second does not.
	if ok, err := db.MarkA2APushAttempted(ctx, taskID, "w1", models.TaskStatusPending); err != nil || !ok {
		t.Fatalf("first mark: %v %v", ok, err)
	}
	if ok, _ := db.MarkA2APushAttempted(ctx, taskID, "w1", models.TaskStatusPending); ok {
		t.Fatal("second identical mark must lose the claim")
	}
	if work, _ := db.ListA2APushWork(ctx, 10); len(work) != 0 {
		t.Fatalf("marked config must leave the work list, got %d", len(work))
	}

	// A status move re-queues it.
	if _, err := db.conn.ExecContext(ctx, "UPDATE tasks SET status='running' WHERE id=$1", taskID); err != nil {
		t.Fatal(err)
	}
	if work, _ := db.ListA2APushWork(ctx, 10); len(work) != 1 || work[0].Status != models.TaskStatusRunning {
		t.Fatalf("status move must re-queue the config: %+v", work)
	}
}

// TestA2APushWorkSkipsUnreadableRow pins #2040: a config whose secrets were
// sealed under a different key (the store key rotated) no longer fails the
// whole scan — the good row is still returned, and the bad row is marked so
// it stops occupying the batch until the task's next status change.
func TestA2APushWorkSkipsUnreadableRow(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	ctx := context.Background()
	taskID := seedPushTask(t, db, models.TaskStatusRunning)

	// Sealed under the OLD key, then the key changes.
	db.SetA2APushCipher(testPushCipher(t))
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "stale", URL: "https://example.com/old", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}
	db.SetA2APushCipher(testPushCipher(t))
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "good", URL: "https://example.com/new", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}

	work, err := db.ListA2APushWork(ctx, 10)
	if err != nil {
		t.Fatalf("one unreadable row must not fail the scan: %v", err)
	}
	if len(work) != 1 || work[0].Config.ID != "good" || work[0].Config.Token != "not-a-real-token-value" {
		t.Fatalf("want only the good row, got %+v", work)
	}
	var marker sql.NullString
	if err := db.conn.QueryRowContext(ctx,
		"SELECT last_pushed_status FROM a2a_push_configs WHERE task_id=$1 AND id='stale'", taskID).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker.String != string(models.TaskStatusRunning) {
		t.Fatalf("unreadable row must be marked for its current status, got %+v", marker)
	}
	// Marked: the next scan no longer sees it (only the still-unmarked good row).
	if work, err := db.ListA2APushWork(ctx, 10); err != nil || len(work) != 1 || work[0].Config.ID != "good" {
		t.Fatalf("rescan: %v %+v", err, work)
	}
	// Re-registering reseals under the current key and makes it deliverable.
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "stale", URL: "https://example.com/old", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}
	if work, err := db.ListA2APushWork(ctx, 10); err != nil || len(work) != 2 {
		t.Fatalf("re-registered row must be deliverable again: %v %+v", err, work)
	}
}

// TestA2APushUnreadableMarkSparesReRegistration: the mark for an unreadable
// row applies only to the sealed bytes the scan saw, so a re-registration that
// lands between the scan and the mark keeps the recovered config due.
func TestA2APushUnreadableMarkSparesReRegistration(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	ctx := context.Background()
	taskID := seedPushTask(t, db, models.TaskStatusRunning)
	db.SetA2APushCipher(testPushCipher(t))
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "stale", URL: "https://example.com/old", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}
	var oldToken, oldCreds []byte
	if err := db.conn.QueryRowContext(ctx,
		"SELECT token_sealed, auth_credentials_sealed FROM a2a_push_configs WHERE task_id=$1 AND id='stale'", taskID).Scan(&oldToken, &oldCreds); err != nil {
		t.Fatal(err)
	}
	// The re-registration lands after the scan read the old bytes.
	db.SetA2APushCipher(testPushCipher(t))
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
		TaskID: taskID, ID: "stale", URL: "https://example.com/old", Token: "not-a-real-token-value",
	}); err != nil {
		t.Fatal(err)
	}
	marked, err := db.markUnreadableA2APush(ctx, taskID, "stale", models.TaskStatusRunning, oldToken, oldCreds)
	if err != nil || marked {
		t.Fatalf("mark against the old sealed bytes must not apply: marked=%v err=%v", marked, err)
	}
	if work, err := db.ListA2APushWork(ctx, 10); err != nil || len(work) != 1 || work[0].Config.ID != "stale" {
		t.Fatalf("re-registered config must stay due: %v %+v", err, work)
	}
}

// TestA2APushConfigPerTaskCap pins the #2040 cap: a task holds at most
// MaxA2APushConfigsPerTask configs; updating an existing id at the cap is
// still allowed, and another task is unaffected.
func TestA2APushConfigPerTaskCap(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanA2APush(t, db)
	db.SetA2APushCipher(testPushCipher(t))
	ctx := context.Background()
	taskID := seedPushTask(t, db, models.TaskStatusRunning)

	for i := range MaxA2APushConfigsPerTask {
		if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{
			TaskID: taskID, ID: fmt.Sprintf("c%d", i), URL: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("config %d: %v", i, err)
		}
	}
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{TaskID: taskID, URL: "https://example.com/hook"}); !errors.Is(err, ErrA2APushConfigLimit) {
		t.Fatalf("over-cap create: want ErrA2APushConfigLimit, got %v", err)
	}
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{TaskID: taskID, ID: "c0", URL: "https://example.com/hook2"}); err != nil {
		t.Fatalf("update at the cap must succeed: %v", err)
	}
	if all, err := db.ListA2APushConfigs(ctx, taskID); err != nil || len(all) != MaxA2APushConfigsPerTask {
		t.Fatalf("list: %v %d", err, len(all))
	}
	other := seedPushTask(t, db, models.TaskStatusRunning)
	if _, err := db.UpsertA2APushConfig(ctx, models.A2APushConfig{TaskID: other, URL: "https://example.com/hook"}); err != nil {
		t.Fatalf("another task is unaffected: %v", err)
	}
}
