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
