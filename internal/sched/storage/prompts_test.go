package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/db"
)

// seedPromptAccounts creates accounts (core.users rows) for emails: a prompt
// is saved for its owner only while the owner has an account.
func seedPromptAccounts(t *testing.T, database *db.Database, emails ...string) {
	t.Helper()
	for _, e := range emails {
		if _, err := database.Conn().ExecContext(context.Background(),
			`INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ($1, '', $2, $2) ON CONFLICT DO NOTHING`,
			e, time.Now().Unix()); err != nil {
			t.Fatalf("seed account %s: %v", e, err)
		}
	}
}

// A prompt is saved for an account only while it exists — so one saved
// while the account is being deleted never outlives it — and for an API key
// or the admin key, which have no account, as before.
func TestPromptForAnAbsentAccountIsRefused(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := store.CreatePromptLibrary(ctx, "gone@example.com", "Draft", "", "c", "private"); !errors.Is(err, ErrPromptOwnerGone) {
		t.Fatalf("an owner with no account: err %v, want ErrPromptOwnerGone", err)
	}
	for _, owner := range []string{"apikey:k1", "admin"} {
		if _, err := store.CreatePromptLibrary(ctx, owner, "Draft "+owner, "", "c", "private"); err != nil {
			t.Fatalf("owner %s: %v", owner, err)
		}
	}
}

func TestPromptLibraryVisibilityAndOwnership(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	seedPromptAccounts(t, database, "alice@example.com", "bob@example.com")
	alicePrivate, err := store.CreatePromptLibrary(ctx, "alice@example.com", "Private draft", "", "Do the private thing", "private")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePromptLibrary(ctx, "alice@example.com", "Team brief", "Shared", "Write the team brief", "workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePromptLibrary(ctx, "alice@example.com", "team BRIEF", "", "Duplicate", "private"); !errors.Is(err, ErrPromptConflict) {
		t.Fatalf("case-insensitive duplicate error = %v, want conflict", err)
	}
	if _, err := store.CreatePromptLibrary(ctx, "bob@example.com", "Bob only", "", "Bob's prompt", "private"); err != nil {
		t.Fatal(err)
	}

	alice, err := store.ListPromptLibrary(ctx, "alice@example.com")
	if err != nil || len(alice) != 2 {
		t.Fatalf("alice list = %v, %v", alice, err)
	}
	bob, err := store.ListPromptLibrary(ctx, "bob@example.com")
	if err != nil || len(bob) != 2 { // Bob's private + Alice's workspace prompt.
		t.Fatalf("bob list = %v, %v", bob, err)
	}

	if _, err := store.UpdatePromptLibrary(ctx, alicePrivate.ID, "bob@example.com", "Stolen", "", "No", "private", false); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("foreign update error = %v, want not found", err)
	}
	updated, err := store.UpdatePromptLibrary(ctx, alicePrivate.ID, "admin", "Admin corrected", "", "Corrected", "workspace", true)
	if err != nil || updated.Name != "Admin corrected" || updated.Visibility != "workspace" {
		t.Fatalf("admin update = %+v, %v", updated, err)
	}
	if err := store.DeletePromptLibrary(ctx, alicePrivate.ID, "bob@example.com", false); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("foreign delete error = %v, want not found", err)
	}
	if err := store.DeletePromptLibrary(ctx, alicePrivate.ID, "admin", true); err != nil {
		t.Fatal(err)
	}
}

func TestPromptLibraryValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, content, visibility string
	}{
		{"", "body", "private"},
		{"name", "", "private"},
		{"name", "body", "everyone"},
	} {
		if err := validatePromptLibraryEntry(tc.name, "", tc.content, tc.visibility); !errors.Is(err, ErrPromptInvalid) {
			t.Errorf("validate(%q,%q,%q) = %v", tc.name, tc.content, tc.visibility, err)
		}
	}
}
