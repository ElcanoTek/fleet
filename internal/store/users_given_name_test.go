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

	// The store applies the same rule the turn does (agentcore.ValidMCPUserName)
	// for every writer, so nothing it saves is later dropped on a turn.
	for _, bad := range []string{
		strings.Repeat("é", MaxGivenNameRunes+1), // too long
		"a\x00b",              // control character
		"elyse.messer@x.com",  // an address
		"ops2",                // a handle
		"${FLEET_USER_EMAIL}", // a token
	} {
		if _, err := s.SetOwnGivenName(ctx, "elyse.messer@x.com", bad); !IsInputError(err) {
			t.Fatalf("SetOwnGivenName(%q): err = %v, want an input error", bad, err)
		}
	}
	if name, _ := s.UserGivenName(ctx, "elyse.messer@x.com"); name != "Elyse" {
		t.Fatalf("a refused write changed the saved name to %q", name)
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
