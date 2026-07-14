package storage

import (
	"context"
	"errors"
	"testing"
)

func TestPromptLibraryVisibilityAndOwnership(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
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
