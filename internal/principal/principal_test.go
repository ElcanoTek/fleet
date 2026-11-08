// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package principal

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
)

type fakeDir struct {
	users    map[string]*identity.User
	external map[[2]string]string
	err      error
	lookups  int
}

func (d *fakeDir) GetByEmail(_ context.Context, email string) (*identity.User, error) {
	d.lookups++
	if d.err != nil {
		return nil, d.err
	}
	u, ok := d.users[email]
	if !ok {
		return nil, identity.ErrNotFound
	}
	return u, nil
}

func (d *fakeDir) LookupExternalSessionEpoch(_ context.Context, issuer, subject string) (string, error) {
	return d.external[[2]string{issuer, subject}], nil
}

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := From(ctx); ok {
		t.Fatal("empty context carries a principal")
	}
	p := Principal{Email: "a@x.com", AuthMethod: MethodSession}
	ctx = With(ctx, p)
	got, ok := From(ctx)
	if !ok || got.Email != "a@x.com" || got.AuthMethod != MethodSession {
		t.Fatalf("From = %+v, %v", got, ok)
	}
	if Email(ctx) != "a@x.com" {
		t.Fatalf("Email = %q", Email(ctx))
	}
	if _, ok := From(Without(ctx)); ok {
		t.Fatal("Without left a principal")
	}
	if bg := context.Background(); Without(bg) != bg {
		t.Fatal("Without wrapped a context with no principal")
	}
}

func TestMethodClasses(t *testing.T) {
	for m, want := range map[AuthMethod][2]bool{
		MethodSession:      {true, false},
		MethodGateway:      {true, false},
		MethodAPIKey:       {false, true},
		MethodBearer:       {false, true},
		MethodAdminKey:     {false, true},
		MethodServiceToken: {false, true},
		"":                 {false, false},
	} {
		p := Principal{AuthMethod: m}
		if p.IsBrowserSession() != want[0] || p.IsCredential() != want[1] {
			t.Errorf("%q: browser=%v credential=%v, want %v", m, p.IsBrowserSession(), p.IsCredential(), want)
		}
	}
}

func TestResolve(t *testing.T) {
	id := uuid.New()
	dir := &fakeDir{
		users: map[string]*identity.User{
			"a@x.com":   {ID: id, Email: "a@x.com", Role: identity.RoleViewer, OpsScope: identity.OpsView, Enabled: true, SessionEpoch: "live"},
			"off@x.com": {Email: "off@x.com", Role: identity.RoleMember, Enabled: false, SessionEpoch: "live"},
		},
		external: map[[2]string]string{{"https://auth", "sub-1"}: "ext-live"},
	}
	ctx := context.Background()

	t.Run("fills the account fields from the row, not the caller", func(t *testing.T) {
		in := Principal{Email: " A@X.com ", Role: "admin", AuthMethod: MethodSession, Session: Session{Epoch: "live"}}
		got, u, err := Resolve(ctx, dir, in, Enabled)
		if err != nil || u == nil {
			t.Fatalf("err = %v", err)
		}
		if got.Email != "a@x.com" || got.UserID != id || got.Role != "viewer" || got.OpsScope != "view" || !got.Resolved() {
			t.Fatalf("resolved = %+v", got)
		}
	})
	t.Run("no row is not a member", func(t *testing.T) {
		if _, _, err := Resolve(ctx, dir, Principal{Email: "ghost@x.com", AuthMethod: MethodSession}, Enabled); !errors.Is(err, ErrNotMember) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("membership outranks a stale session", func(t *testing.T) {
		_, _, err := Resolve(ctx, dir, Principal{Email: "off@x.com", AuthMethod: MethodSession, Session: Session{Epoch: "stale"}}, Enabled)
		if !errors.Is(err, ErrNotMember) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a stale epoch is revoked", func(t *testing.T) {
		_, _, err := Resolve(ctx, dir, Principal{Email: "a@x.com", AuthMethod: MethodSession, Session: Session{Epoch: "stale"}}, Enabled)
		if !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no claim is admitted", func(t *testing.T) {
		if _, _, err := Resolve(ctx, dir, Principal{Email: "a@x.com", AuthMethod: MethodSession}, Enabled); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an oidc session is checked against its external generation", func(t *testing.T) {
		ok := Principal{Email: "a@x.com", AuthMethod: MethodSession, Session: Session{Epoch: "ext-live", Source: SourceOIDC, Issuer: "https://auth", Subject: "sub-1"}}
		if _, _, err := Resolve(ctx, dir, ok, Enabled); err != nil {
			t.Fatalf("err = %v", err)
		}
		// The account epoch is not the external generation.
		bad := ok
		bad.Session.Epoch = "live"
		if _, _, err := Resolve(ctx, dir, bad, Enabled); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("err = %v", err)
		}
		noSubject := ok
		noSubject.Session.Subject = ""
		if _, _, err := Resolve(ctx, dir, noSubject, Enabled); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("only a session principal carries a claim", func(t *testing.T) {
		// A gateway leg or a key never had a cookie to revoke.
		p := Principal{Email: "a@x.com", AuthMethod: MethodGateway, Session: Session{Epoch: "stale"}}
		if _, _, err := Resolve(ctx, dir, p, Enabled); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no identity", func(t *testing.T) {
		if _, _, err := Resolve(ctx, dir, Principal{AuthMethod: MethodGateway}, nil); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a lookup failure is neither verdict", func(t *testing.T) {
		broken := &fakeDir{err: errors.New("db down")}
		_, _, err := Resolve(ctx, broken, Principal{Email: "a@x.com", AuthMethod: MethodSession, Session: Session{Epoch: "x"}}, Enabled)
		if err == nil || errors.Is(err, ErrNotMember) || errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("one lookup", func(t *testing.T) {
		d := &fakeDir{users: dir.users}
		_, _, _ = Resolve(ctx, d, Principal{Email: "a@x.com", AuthMethod: MethodSession, Session: Session{Epoch: "live"}}, Enabled)
		if d.lookups != 1 {
			t.Fatalf("lookups = %d", d.lookups)
		}
	})
}
