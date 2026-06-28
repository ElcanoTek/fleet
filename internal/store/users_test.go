package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCreateUserAndVerify(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "Alice@Example.com", "supersecret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Emails are normalized to lowercase on insert.
	if u.Email != "alice@example.com" {
		t.Errorf("email not normalized: %q", u.Email)
	}

	// Correct password → nil.
	if err := s.VerifyUser(ctx, "alice@example.com", "supersecret"); err != nil {
		t.Errorf("valid password rejected: %v", err)
	}
	// Verify is case-insensitive on the email too.
	if err := s.VerifyUser(ctx, "ALICE@Example.com", "supersecret"); err != nil {
		t.Errorf("uppercase email rejected: %v", err)
	}
	// Wrong password → ErrBadPassword.
	if err := s.VerifyUser(ctx, "alice@example.com", "nope"); !errors.Is(err, ErrBadPassword) {
		t.Errorf("wrong password err: got %v want ErrBadPassword", err)
	}
	// Missing user → ErrUserNotFound.
	if err := s.VerifyUser(ctx, "bob@example.com", "anything"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("missing user err: got %v", err)
	}
}

func TestIsUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "Carol@Example.com", "supersecret"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Known user → true, case-insensitively.
	for _, email := range []string{"carol@example.com", "CAROL@Example.com", "  carol@example.com  "} {
		ok, err := s.IsUser(ctx, email)
		if err != nil {
			t.Fatalf("IsUser(%q): %v", email, err)
		}
		if !ok {
			t.Errorf("IsUser(%q) = false, want true", email)
		}
	}

	// Unknown user → false, no error.
	ok, err := s.IsUser(ctx, "stranger@example.com")
	if err != nil {
		t.Fatalf("IsUser(stranger): %v", err)
	}
	if ok {
		t.Error("IsUser(stranger) = true, want false")
	}

	// Empty email → false, no error (never a member).
	if ok, err := s.IsUser(ctx, ""); err != nil || ok {
		t.Errorf("IsUser(empty) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestVerifyUser_MissingUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.VerifyUser(ctx, "nonexistent@example.com", "password123")
	if err == nil {
		t.Fatal("expected an error for missing user, got nil")
	}
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for missing user, got %v", err)
	}
}

func TestVerifyUser_DBError(t *testing.T) {
	s := newTestStore(t)

	// Create a context that is already canceled to trigger a db error that is not ErrNoRows.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.VerifyUser(ctx, "dberr@example.com", "password123")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context canceled error, got %v", err)
	}
}

func TestCreateUser_UniqueViolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 1. Create a user normally
	if _, err := s.CreateUser(ctx, "alice@example.com", "password123"); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// 2. Attempt to create the exact same user again to trigger unique constraint violation
	_, err := s.CreateUser(ctx, "alice@example.com", "different")
	if err == nil {
		t.Fatal("expected an error for duplicate user, got nil")
	}

	// 3. Verify it returns the specific format for "user already exists"
	expectedErr := fmt.Sprintf("user %s already exists", "alice@example.com")
	if err.Error() != expectedErr {
		t.Errorf("expected error %q, got %q", expectedErr, err.Error())
	}
}

func TestCreateUser_WeakPasswordRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.CreateUser(ctx, "u@x.com", "short")
	if err == nil {
		t.Error("7-char password should be rejected")
	}
	_, err = s.CreateUser(ctx, "u@x.com", "")
	if err == nil {
		t.Error("empty password should be rejected")
	}
}

func TestCreateUser_EmptyEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "   ", "password123"); err == nil || err.Error() != "email required" {
		t.Errorf("empty email should be rejected: %v", err)
	}
}

func TestCreateUser_PasswordTooLong(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	longPass := strings.Repeat("a", 73)
	if _, err := s.CreateUser(ctx, "long@example.com", longPass); err == nil || !strings.Contains(err.Error(), "hash password") {
		t.Errorf("expected hash password error for >72 byte password, got %v", err)
	}
}

func TestCreateUser_DBError(t *testing.T) {
	s := newTestStore(t)

	// Create a context that is already canceled to trigger a db error that is not a unique violation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.CreateUser(ctx, "dberr@example.com", "password123"); err == nil || errors.Is(err, context.Canceled) == false {
		t.Errorf("expected context canceled error, got %v", err)
	}
}

func TestUpdatePassword(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "original1"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePassword(ctx, "u@x.com", "rotated99"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if err := s.VerifyUser(ctx, "u@x.com", "original1"); !errors.Is(err, ErrBadPassword) {
		t.Error("old password should no longer work")
	}
	if err := s.VerifyUser(ctx, "u@x.com", "rotated99"); err != nil {
		t.Errorf("new password rejected: %v", err)
	}
}

func TestUpdatePassword_Missing(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdatePassword(context.Background(), "nope@x.com", "anything8")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, "u@x.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Gone — verify returns not-found.
	if err := s.VerifyUser(ctx, "u@x.com", "password123"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("should be gone: %v", err)
	}
}

func TestDeleteUserCascadesConversations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "cascade@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, "cascade@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.DeleteUser(ctx, "cascade@x.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rows, err := s.List(ctx, "cascade@x.com", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected conversations deleted, still got %d (id=%s)", len(rows), conv.ID)
	}
}

func TestListAndCountUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Errorf("fresh store should have 0 users, got %d", n)
	}

	emails := []string{"zebra@x.com", "alpha@x.com", "mike@x.com"}
	for _, e := range emails {
		if _, err := s.CreateUser(ctx, e, "password123"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len: %d", len(list))
	}
	// Sorted by email ascending.
	if list[0].Email != "alpha@x.com" || list[2].Email != "zebra@x.com" {
		t.Errorf("unsorted: %v", list)
	}
	if n, _ := s.CountUsers(ctx); n != 3 {
		t.Errorf("count: %d", n)
	}
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already normalized",
			input:    "alice@example.com",
			expected: "alice@example.com",
		},
		{
			name:     "uppercase",
			input:    "ALICE@EXAMPLE.COM",
			expected: "alice@example.com",
		},
		{
			name:     "mixed case",
			input:    "AlIcE@ExAmPlE.cOm",
			expected: "alice@example.com",
		},
		{
			name:     "leading and trailing spaces",
			input:    "   bob@example.com   ",
			expected: "bob@example.com",
		},
		{
			name:     "tabs and newlines",
			input:    "\t\ncharlie@example.com\n\t",
			expected: "charlie@example.com",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only whitespace",
			input:    "   \t\n  ",
			expected: "",
		},
		{
			name:     "with plus alias",
			input:    " dave+test@example.com ",
			expected: "dave+test@example.com",
		},
		{
			name:     "with periods",
			input:    "eve.smith@example.com",
			expected: "eve.smith@example.com",
		},
		{
			name:     "unicode spaces",
			input:    "\u00a0 test@example.com \u2003",
			expected: "test@example.com",
		},
		{
			name:     "unicode characters",
			input:    "  äöü@EXÄMPLE.com  ",
			expected: "äöü@exämple.com",
		},
		{
			name:     "missing @ symbol",
			input:    "  notanemail  ",
			expected: "notanemail",
		},
		{
			name:     "multiple @ symbols",
			input:    "  a@b@c.com  ",
			expected: "a@b@c.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeEmail(tt.input); got != tt.expected {
				t.Errorf("normalizeEmail(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
