package store

import (
	"context"
	"errors"
	"testing"
)

// TestValidationErrorsAreTyped pins that the store errors the HTTP layer maps
// to 400/409 carry a type (#1950) — the handlers used to match their message
// text, so rewording one silently turned a 400 into a 500 — and that the
// messages read exactly as before, since they are shown to the caller.
func TestValidationErrorsAreTyped(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "dup@example.com", "a-long-enough-password"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := s.CreateLLMProvider(ctx, LLMProviderInput{Name: "dup", Type: "anthropic", Enabled: true}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	role := "superuser"

	inputCases := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"create user: email required", second(s.CreateUser(ctx, "  ", "a-long-enough-password")), "email required"},
		{"create user: short password", second(s.CreateUser(ctx, "new@example.com", "short")), "password must be at least 8 characters"},
		{"update password: short", s.UpdatePassword(ctx, "dup@example.com", "short"), "password must be at least 8 characters"},
		{"set role: invalid", second(s.SetUserRoleTeam(ctx, "dup@example.com", &role, nil)), `invalid role "superuser" (want member|viewer|admin)`},
		{"provider: invalid name", second(s.CreateLLMProvider(ctx, LLMProviderInput{Name: "bad/name", Type: "anthropic"})), `invalid provider name "bad/name" (lowercase slug, no slashes)`},
		{"transfer: owner required", second(s.TransferProjectOwnership(ctx, "p", "  ", "")), "new owner email required"},
	}
	for _, tc := range inputCases {
		if !IsInputError(tc.err) {
			t.Errorf("%s: err = %v (%T), want an InputError", tc.name, tc.err, tc.err)
			continue
		}
		if tc.err.Error() != tc.wantMsg {
			t.Errorf("%s: message = %q, want %q", tc.name, tc.err.Error(), tc.wantMsg)
		}
	}

	_, err := s.CreateUser(ctx, "dup@example.com", "a-long-enough-password")
	if !errors.Is(err, ErrUserExists) || err.Error() != "user dup@example.com already exists" {
		t.Errorf("duplicate user: err = %v, want ErrUserExists reading \"user dup@example.com already exists\"", err)
	}
	_, err = s.CreateLLMProvider(ctx, LLMProviderInput{Name: "dup", Type: "anthropic", Enabled: true})
	if !errors.Is(err, ErrLLMProviderExists) || err.Error() != `a provider named "dup" already exists` {
		t.Errorf("duplicate provider: err = %v, want ErrLLMProviderExists reading %q", err, `a provider named "dup" already exists`)
	}
}

// second returns the error half of a (value, error) result.
func second[T any](_ T, err error) error { return err }
