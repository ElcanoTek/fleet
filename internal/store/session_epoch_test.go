package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The session epoch is the per-account revocation generation carried in the web
// session cookie: chat-server refuses a cookie whose epoch no longer matches the
// row. It is derived from the stored bcrypt hash rather than kept in its own
// column, so these tests pin the properties that derivation depends on — most
// importantly that EVERY password write moves it, since that is what makes a
// password reset evict outstanding sessions.

func TestSessionEpoch_MovesOnPasswordChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.CreateUser(ctx, "u@x.com", "original1")
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionEpoch == "" {
		t.Fatal("CreateUser returned an empty session epoch")
	}

	before, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("SessionEpoch: %v", err)
	}
	if before != created.SessionEpoch {
		t.Errorf("SessionEpoch = %q, CreateUser reported %q — the mint path and the gate must agree", before, created.SessionEpoch)
	}

	if err := s.UpdatePassword(ctx, "u@x.com", "rotated99"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	after, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("SessionEpoch: %v", err)
	}
	if after == before {
		t.Error("session epoch unchanged across a password reset — sessions minted before it would survive")
	}
}

func TestExternalSessionEpochIsIndependentAndRevocable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	passwordEpoch, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	externalEpoch, err := s.ExternalSessionEpoch(ctx, "https://auth.example.com", "account-123", "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if externalEpoch == "" || externalEpoch == passwordEpoch {
		t.Fatalf("external epoch = %q, password epoch = %q", externalEpoch, passwordEpoch)
	}
	stable, err := s.ExternalSessionEpoch(ctx, "https://auth.example.com", "account-123", "u@x.com")
	if err != nil || stable != externalEpoch {
		t.Fatalf("stable external epoch = %q, %v", stable, err)
	}
	rotated, applied, err := s.RevokeExternalSessions(ctx, "event-123", "https://auth.example.com", "account-123", "u@x.com")
	if err != nil || rotated == externalEpoch {
		t.Fatalf("rotated external epoch = %q, %v", rotated, err)
	}
	if !applied {
		t.Fatal("first external revocation was not applied")
	}
	duplicate, applied, err := s.RevokeExternalSessions(ctx, "event-123", "https://auth.example.com", "account-123", "u@x.com")
	if err != nil || applied || duplicate != rotated {
		t.Fatalf("duplicate revocation epoch=%q applied=%v err=%v", duplicate, applied, err)
	}
	// A central sign-out ends the account's Fleet password sessions too: the
	// password epoch moves (via users.session_salt) without a password change.
	passwordAfter, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil || passwordAfter == passwordEpoch {
		t.Fatalf("central revocation left the Fleet password epoch in place: %q, %v", passwordAfter, err)
	}
	// The password itself still works, and the new epoch is what a fresh
	// password login would be minted with.
	if err := s.VerifyUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatalf("password rejected after central revocation: %v", err)
	}
	// A replayed delivery (same jti) does not evict sessions minted since.
	if _, _, err := s.RevokeExternalSessions(ctx, "event-123", "https://auth.example.com", "account-123", "u@x.com"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.SessionEpoch(ctx, "u@x.com"); again != passwordAfter {
		t.Fatalf("duplicate event rotated the password epoch again: %q → %q", passwordAfter, again)
	}
	// A different event does.
	if _, applied, err := s.RevokeExternalSessions(ctx, "event-456", "https://auth.example.com", "account-123", "u@x.com"); err != nil || !applied {
		t.Fatalf("second event: applied=%v err=%v", applied, err)
	}
	if third, _ := s.SessionEpoch(ctx, "u@x.com"); third == passwordAfter {
		t.Fatal("a new logout event did not rotate the password epoch")
	}
	// A password change afterwards still moves it.
	if err := s.UpdatePassword(ctx, "u@x.com", "rotated99"); err != nil {
		t.Fatal(err)
	}
	if fourth, _ := s.SessionEpoch(ctx, "u@x.com"); fourth == passwordAfter {
		t.Fatal("password change after a central logout did not move the epoch")
	}
}

// A central logout for an email with no Fleet user row (Auth admits people
// Fleet has not provisioned) must still rotate the external epoch and not fail.
func TestRevokeExternalSessionsWithoutFleetUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch, applied, err := s.RevokeExternalSessions(ctx, "event-789", "https://auth.example.com", "account-789", "ghost@x.com")
	if err != nil || !applied || epoch == "" {
		t.Fatalf("epoch=%q applied=%v err=%v", epoch, applied, err)
	}
	if looked, _ := s.LookupExternalSessionEpoch(ctx, "https://auth.example.com", "account-789"); looked != epoch {
		t.Fatalf("tombstone epoch = %q, want %q", looked, epoch)
	}
}

// The default '' salt keeps the historical derivation byte-for-byte, so the
// migration that added users.session_salt signs nobody out.
func TestSessionEpochDefaultSaltIsBackwardCompatible(t *testing.T) {
	const hash = "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	sum := sha256.Sum256([]byte(hash))
	if got, want := sessionEpochFor(hash, ""), hex.EncodeToString(sum[:sessionEpochBytes]); got != want {
		t.Fatalf("epoch with empty salt = %q, want historical %q", got, want)
	}
	if sessionEpochFor(hash, "salt") == sessionEpochFor(hash, "") {
		t.Fatal("salt did not change the epoch")
	}
}

// bcrypt salts every hash, so even a reset to the SAME password rotates the
// epoch. An operator who resets an account to its existing password during an
// incident still evicts the attacker.
func TestSessionEpoch_MovesWhenPasswordIsReusedVerbatim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	before, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePassword(ctx, "u@x.com", "password123"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	after, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("re-hashing the same password left the epoch unchanged")
	}
}

// Routine admin edits (role, team) must not move the epoch, or they would log
// people out. Only a password write and a central logout (the session salt) may.
func TestSessionEpoch_SurvivesRoleAndTeamEdits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	before, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}

	role, team := RoleAdmin, "ops"
	updated, err := s.SetUserRoleTeam(ctx, "u@x.com", &role, &team)
	if err != nil {
		t.Fatalf("SetUserRoleTeam: %v", err)
	}
	if updated.SessionEpoch != before {
		t.Errorf("role/team edit moved the epoch: %q → %q", before, updated.SessionEpoch)
	}
}

// Every constructor of store.User populates SessionEpoch, so the middleware's
// plain mismatch check can never be satisfied by an unpopulated field.
func TestSessionEpoch_PopulatedByEveryUserRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	want, err := s.SessionEpoch(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetUser(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.SessionEpoch != want {
		t.Errorf("GetUser epoch = %q, want %q", got.SessionEpoch, want)
	}

	list, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListUsers returned %d users, want 1", len(list))
	}
	if list[0].SessionEpoch != want {
		t.Errorf("ListUsers epoch = %q, want %q", list[0].SessionEpoch, want)
	}
}

// An unprovisioned email still reports a well-formed epoch (the login mint paths
// need one for an address that may not be a chat user), and it must differ from
// every real account's so a cookie minted in that window stops working once the
// account exists.
func TestSessionEpoch_UnknownEmailIsWellFormedAndDistinct(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	unknown, err := s.SessionEpoch(ctx, "stranger@x.com")
	if err != nil {
		t.Fatalf("SessionEpoch: %v", err)
	}
	if unknown == "" {
		t.Fatal("unknown email reported an empty epoch")
	}

	if _, err := s.CreateUser(ctx, "stranger@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	provisioned, err := s.SessionEpoch(ctx, "stranger@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if provisioned == unknown {
		t.Error("provisioning the account left the epoch at its unknown-email value")
	}
}

// The epoch is derived in two places: in Go from a hash the process already
// holds (CreateUser), and in SQL from the stored row (every read that needs the
// epoch, so password_hash stays inside Postgres). The two MUST agree byte for
// byte — a drift would revoke every outstanding session the moment a request
// crossed from one derivation to the other.
func TestSessionEpochSQLMatchesGo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Values a password_hash column can hold, plus the empty string the
	// unknown-email path derives from.
	for _, hash := range []string{
		"",
		"$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		`single ' quote and \backslash`,
		"ünïcödé ✓",
	} {
		var got string
		if err := s.db.QueryRowContext(ctx,
			`SELECT `+sessionEpochExpr+` FROM (SELECT $1::text AS password_hash, ''::text AS session_salt) AS h`, hash).Scan(&got); err != nil {
			t.Fatalf("SQL derivation for %q: %v", hash, err)
		}
		if want := sessionEpochFor(hash, ""); got != want {
			t.Errorf("epoch for %q: SQL %q, Go %q", hash, got, want)
		}
	}

	// And over a real bcrypt hash as actually stored, read back out of the row
	// the SQL derivation reads.
	created, err := s.CreateUser(ctx, "u@x.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = $1`, "u@x.com").Scan(&stored); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if want := sessionEpochFor(stored, ""); created.SessionEpoch != want {
		t.Errorf("CreateUser epoch = %q, want %q", created.SessionEpoch, want)
	}
	got, err := s.GetUser(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.SessionEpoch != created.SessionEpoch {
		t.Errorf("GetUser (SQL) epoch = %q, CreateUser (Go) epoch = %q", got.SessionEpoch, created.SessionEpoch)
	}
}

// Two accounts must not share an epoch, or a reset on one would leave the other
// admitting a cookie minted for it.
func TestSessionEpoch_DistinctPerAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, e := range []string{"a@x.com", "b@x.com"} {
		if _, err := s.CreateUser(ctx, e, "password123"); err != nil {
			t.Fatalf("CreateUser(%s): %v", e, err)
		}
	}
	a, err := s.SessionEpoch(ctx, "a@x.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SessionEpoch(ctx, "b@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two accounts share the epoch %q", a)
	}
}

func TestLookupExternalSessionEpochNeverWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "u@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	// Unknown identity: no row, empty epoch, and still no row afterwards.
	got, err := s.LookupExternalSessionEpoch(ctx, "https://auth.example.com", "never-logged-in")
	if err != nil || got != "" {
		t.Fatalf("lookup of unknown identity = %q, %v", got, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_auth_epochs`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("request-path lookup created %d epoch rows", rows)
	}
	minted, err := s.ExternalSessionEpoch(ctx, "https://auth.example.com", "account-123", "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.LookupExternalSessionEpoch(ctx, "https://auth.example.com", "account-123")
	if err != nil || got != minted {
		t.Fatalf("lookup after mint = %q, want %q (%v)", got, minted, err)
	}
	if _, _, err := s.RevokeExternalSessions(ctx, "event-1", "https://auth.example.com", "account-123", "u@x.com"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LookupExternalSessionEpoch(ctx, "https://auth.example.com", "account-123")
	if got == minted || got == "" {
		t.Fatalf("lookup after revoke = %q, want a rotated generation", got)
	}
}
