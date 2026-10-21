package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGivenName covers users.given_name (migration 081): the saved first name
// behind ${FLEET_USER_NAME} (ADR-0087). A new account has none — nothing is
// filled from the email — and the store keeps exactly what the user saved.
func TestGivenName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "elyse.messer@x.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	name, err := s.UserGivenName(ctx, "elyse.messer@x.com")
	if err != nil || name != "" {
		t.Fatalf("fresh account given name = %q, %v; want empty (never derived from the email)", name, err)
	}
	if u, err := s.GetUser(ctx, "elyse.messer@x.com"); err != nil || u.GivenName != "" {
		t.Fatalf("GetUser fresh = %+v, %v", u, err)
	}

	u, err := s.SetOwnGivenName(ctx, " Elyse.Messer@X.com ", "  Elyse ")
	if err != nil {
		t.Fatalf("SetOwnGivenName: %v", err)
	}
	if u.GivenName != "Elyse" {
		t.Fatalf("saved given name = %q, want trimmed Elyse", u.GivenName)
	}
	if name, err = s.UserGivenName(ctx, "ELYSE.MESSER@x.com"); err != nil || name != "Elyse" {
		t.Fatalf("UserGivenName = %q, %v; want Elyse", name, err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].GivenName != "Elyse" {
		t.Fatalf("ListUsers = %+v, %v; want the saved name", users, err)
	}

	// The store's own backstop: length and control characters.
	if _, err := s.SetOwnGivenName(ctx, "elyse.messer@x.com", strings.Repeat("é", MaxGivenNameRunes+1)); !IsInputError(err) {
		t.Fatalf("too long: err = %v, want an input error", err)
	}
	if _, err := s.SetOwnGivenName(ctx, "elyse.messer@x.com", "a\x00b"); !IsInputError(err) {
		t.Fatalf("control char: err = %v, want an input error", err)
	}
	// 64 multi-byte runes fit: the limit is characters, not bytes.
	if _, err := s.SetOwnGivenName(ctx, "elyse.messer@x.com", strings.Repeat("é", MaxGivenNameRunes)); err != nil {
		t.Fatalf("64 runes: %v", err)
	}

	// "" clears it.
	if u, err = s.SetOwnGivenName(ctx, "elyse.messer@x.com", ""); err != nil || u.GivenName != "" {
		t.Fatalf("clear = %+v, %v", u, err)
	}

	// Unknown accounts.
	if _, err := s.SetOwnGivenName(ctx, "nobody@x.com", "No"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown set: err = %v, want ErrUserNotFound", err)
	}
	if _, err := s.UserGivenName(ctx, "nobody@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown read: err = %v, want ErrUserNotFound", err)
	}
}
