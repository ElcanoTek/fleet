package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/principal"
)

// A lookup key holding U+0000 matches no stored row. Postgres refuses the
// parameter, so every account lookup the sign-in and request paths run by
// email, issuer or subject fails and matches no account.
func TestLookupKeysWithNULMatchNoAccount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const (
		owner    = "alice@example.com"
		password = "password-long-enough"
		issuer   = "https://auth.example.com"
		subject  = "alice-sub"
		email    = owner + "\x00"
		sub      = subject + "\x00"
	)
	if _, err := s.CreateUser(ctx, owner, password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pwEpoch, err := s.SessionEpoch(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	extEpoch, err := s.ExternalSessionEpoch(ctx, issuer, subject, owner)
	if err != nil {
		t.Fatal(err)
	}
	dir := s.Identity()

	// Membership: the gateway's request path (principal.Resolve over the
	// identity table) and the chat store's own reads.
	if p, u, err := principal.Resolve(ctx, dir, principal.Principal{Email: email, AuthMethod: principal.MethodGateway}, principal.Enabled); err == nil || u != nil || p.Resolved() {
		t.Errorf("principal.Resolve(%q) resolved the account (err %v)", email, err)
	}
	if u, err := dir.GetByEmail(ctx, email); err == nil || u != nil {
		t.Errorf("identity GetByEmail(%q) = %v, %v; want no account", email, u, err)
	}
	if u, err := s.GetUser(ctx, email); err == nil || u != nil {
		t.Errorf("GetUser(%q) = %v, %v; want no account", email, u, err)
	}
	if ok, _ := s.IsUser(ctx, email); ok {
		t.Errorf("IsUser(%q) = true", email)
	}

	// Sessions: the epoch a password or elcano cookie is minted against, and
	// the password check.
	if got, _ := s.SessionEpoch(ctx, email); got == pwEpoch {
		t.Errorf("SessionEpoch(%q) returned the account's epoch", email)
	}
	if got, _ := dir.SessionEpoch(ctx, email); got == pwEpoch {
		t.Errorf("identity SessionEpoch(%q) returned the account's epoch", email)
	}
	if err := s.VerifyUser(ctx, email, password); err == nil {
		t.Errorf("VerifyUser(%q) accepted the account's password", email)
	}

	// Central sign-in: the mint (fetchExternalSessionEpoch's query, an upsert
	// keyed by issuer and subject) and the request-path read.
	if got, err := s.ExternalSessionEpoch(ctx, issuer, sub, "other@example.com"); err == nil || got == extEpoch {
		t.Errorf("ExternalSessionEpoch(subject %q) = %q, %v; want a failure", sub, got, err)
	}
	if got, _ := s.LookupExternalSessionEpoch(ctx, issuer, sub); got == extEpoch {
		t.Errorf("LookupExternalSessionEpoch(subject %q) returned the account's epoch", sub)
	}
	if got, _ := dir.LookupExternalSessionEpoch(ctx, issuer, sub); got == extEpoch {
		t.Errorf("identity LookupExternalSessionEpoch(subject %q) returned the account's epoch", sub)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT email FROM core.external_auth_epochs WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != owner {
		t.Errorf("the account's central identity now names %q", stored)
	}
}

// An account, or a multiplayer participant, whose email holds a character
// pgtext.ValidIdentity refuses is refused as an input error before anything
// is written: on creation, a bootstrap admin, a legacy import, and a
// participant set or added.
func TestIdentityWritesRefuseControlCharacterEmails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedAccount(t, s, "owner@example.com")
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "owner@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ctl := range []string{"\x00", "\x01", "\x1f", "\x7f", "\u200b", "\u212a", "\u017f"} {
		bad := "new" + ctl + "@example.com"
		if _, err := s.CreateUser(ctx, bad, "password-long-enough"); !IsInputError(err) {
			t.Errorf("CreateUser(%q): err = %v, want an input error", bad, err)
		}
		if _, err := s.Identity().CreateAccount(ctx, identity.NewAccount{Email: bad}); !identity.IsInputError(err) {
			t.Errorf("identity CreateAccount(%q): err = %v, want an input error", bad, err)
		}
		if _, err := s.SetConversationMultiplayer(ctx, "owner@example.com", conv.ID, true, []string{bad}, nil); !IsInputError(err) {
			t.Errorf("SetConversationMultiplayer participant %q: err = %v, want an input error", bad, err)
		}
		if _, err := s.UpdateConversationMultiplayerAccess(ctx, "owner@example.com", conv.ID, []string{bad}, nil); !IsInputError(err) {
			t.Errorf("UpdateConversationMultiplayerAccess add %q: err = %v, want an input error", bad, err)
		}
		if err := s.Identity().EnsureAdmin(ctx, bad); !identity.IsInputError(err) {
			t.Errorf("identity EnsureAdmin(%q): err = %v, want an input error", bad, err)
		}
		if _, err := s.ImportUser(ctx, ImportedUser{Email: bad, PasswordHash: "x"}); !IsInputError(err) {
			t.Errorf("ImportUser(%q): err = %v, want an input error", bad, err)
		}
	}
	got, err := s.Get(ctx, "owner@example.com", conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Multiplayer || len(got.MultiplayerParticipants) != 0 || len(got.MultiplayerExcluded) != 0 {
		t.Fatalf("a refused change was written: %+v", got)
	}
}

// A multiplayer list stored with the \u0000 sequence (an entry written before
// participants were checked) cannot be cast to jsonb. It does not fail other
// people's listings, search, or account deletion: the listing reads such a
// list the way that never grants access (a participant list names nobody, an
// exclusion list excludes everybody), and deletion still removes the deleted
// account from it.
func TestMultiplayerListsWithNULSequenceFailClosed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for _, email := range []string{"alice@example.com", "bob@example.com", "carol@example.com"} {
		if _, err := s.CreateUser(ctx, email, "password-long-enough"); err != nil {
			t.Fatal(err)
		}
	}
	// Bob is invited to a team chat he is not on the team of, through a list
	// Postgres cannot cast; carol is excluded from an instance-wide chat
	// through another.
	invited, err := s.CreateConversation(ctx, "alice@example.com", "invited", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	excluded, err := s.CreateConversation(ctx, "alice@example.com", "excluded", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct{ id, team, col, list string }{
		{invited.ID, "eng", "multiplayer_participants", `["bob@example.com","x\u0000@example.com"]`},
		{excluded.ID, "", "multiplayer_excluded", `["carol@example.com","y\u0000@example.com"]`},
	} {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET multiplayer = TRUE, team_shared_with = $2, `+q.col+` = $3 WHERE id = $1`,
			q.id, q.team, q.list); err != nil {
			t.Fatal(err)
		}
	}
	for _, who := range []string{"bob@example.com", "carol@example.com"} {
		convs, err := s.ListSharedAndMultiplayerConversations(ctx, who)
		if err != nil {
			t.Fatalf("ListSharedAndMultiplayerConversations(%s): %v", who, err)
		}
		for _, c := range convs {
			if who == "bob@example.com" && c.ID == invited.ID {
				t.Errorf("a participant list that cannot be read granted bob the chat")
			}
			if who == "carol@example.com" && c.ID == excluded.ID {
				t.Errorf("an exclusion list that cannot be read let carol in")
			}
		}
		if _, _, err := s.SearchConversations(ctx, who, "anything", 10, 0); err != nil {
			t.Fatalf("SearchConversations(%s): %v", who, err)
		}
	}
	if err := s.DeleteUser(ctx, "bob@example.com"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var participants string
	if err := s.db.QueryRowContext(ctx, `SELECT multiplayer_participants FROM conversations WHERE id = $1`, invited.ID).Scan(&participants); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(participants, "bob@example.com") || !strings.Contains(participants, `x\u0000@example.com`) {
		t.Fatalf("participants after deleting bob = %s, want only the other entry", participants)
	}
}

// Capability lookups taken from a URL or a callback — a share token, an OAuth
// state — match nothing when the value holds U+0000: the query is refused, and
// the stored row stays where it was.
func TestCapabilityLookupsWithNULMatchNothing(t *testing.T) {
	t.Parallel()
	s := newTestStoreWithCipher(t)
	seedAccount(t, s, "alice@example.com")
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetShareToken(ctx, "alice@example.com", conv.ID, "share-token-1", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetConversationByShareToken(ctx, "share-token-1\x00", time.Now().Unix()); err == nil || got != nil {
		t.Errorf("GetConversationByShareToken(token + NUL) = %v, %v; want a failure", got, err)
	}
	if got, err := s.GetConversationByShareToken(ctx, "share-token-1", time.Now().Unix()); err != nil || got == nil {
		t.Fatalf("GetConversationByShareToken(token) = %v, %v", got, err)
	}

	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginOAuthFlow(ctx, "state-1", srv.ID, "alice@example.com", "verifier-1", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumeOAuthFlow(ctx, "state-1\x00"); err == nil || got != nil {
		t.Errorf("ConsumeOAuthFlow(state + NUL) = %v, %v; want a failure", got, err)
	}
	if got, err := s.ConsumeOAuthFlow(ctx, "state-1"); err != nil || got == nil || got.CodeVerifier != "verifier-1" {
		t.Fatalf("ConsumeOAuthFlow(state) = %v, %v; want the flow, still unconsumed", got, err)
	}
}

// A grantee or a transfer target is an identity entering the store: one that
// lowercases onto another, ASCII address (U+212A KELVIN SIGN for k) is refused
// instead of naming that account.
func TestGranteesAndTransferTargetsAreCheckedAsSent(t *testing.T) {
	t.Parallel()
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	for _, email := range []string{"alice@example.com", "kate@example.com"} {
		if _, err := s.CreateUser(ctx, email, "password-long-enough"); err != nil {
			t.Fatal(err)
		}
	}
	const lookalike = "\u212aate@example.com"
	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ShareRemoteMCPServer(ctx, "alice@example.com", srv.ID, lookalike); !errors.Is(err, ErrRemoteMCPShareInvalid) {
		t.Errorf("share with %q: err = %v, want ErrRemoteMCPShareInvalid", lookalike, err)
	}
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferConversation(ctx, "alice@example.com", conv.ID, lookalike); !errors.Is(err, ErrTransferTarget) {
		t.Errorf("transfer to %q: err = %v, want ErrTransferTarget", lookalike, err)
	}
	if got, err := s.Get(ctx, "alice@example.com", conv.ID); err != nil || got == nil {
		t.Fatalf("the conversation left alice: %v, %v", got, err)
	}
	if _, err := s.TransferProjectOwnership(ctx, "no-such-project", lookalike, "alice@example.com"); !errors.Is(err, ErrNotAProjectMember) {
		t.Errorf("project transfer to %q: err = %v, want ErrNotAProjectMember", lookalike, err)
	}
}

// An email that holds a look-alike of an ASCII letter (U+212A KELVIN SIGN
// for k) is never lowercased into another account's key, so every account
// lookup with it matches nothing: it is compared exactly as given.
func TestLookalikeLookupsMatchNoAccount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const password = "password-long-enough"
	if _, err := s.CreateUser(ctx, "kate@example.com", password); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.SessionEpoch(ctx, "kate@example.com")
	if err != nil {
		t.Fatal(err)
	}
	const lookalike = "\u212aate@example.com"
	if u, err := s.GetUser(ctx, lookalike); err == nil || u != nil {
		t.Errorf("GetUser(%q) = %v, %v; want no account", lookalike, u, err)
	}
	if u, err := s.Identity().GetByEmail(ctx, lookalike); err == nil || u != nil {
		t.Errorf("identity GetByEmail(%q) = %v, %v; want no account", lookalike, u, err)
	}
	if got, _ := s.SessionEpoch(ctx, lookalike); got == epoch {
		t.Errorf("SessionEpoch(%q) returned kate's epoch", lookalike)
	}
	if err := s.VerifyUser(ctx, lookalike, password); err == nil {
		t.Errorf("VerifyUser(%q) accepted kate's password", lookalike)
	}
	if _, err := s.Identity().SetRole(ctx, lookalike, "admin"); err == nil {
		t.Errorf("identity SetRole(%q) changed an account", lookalike)
	}
	if u, _ := s.GetUser(ctx, "kate@example.com"); u == nil || u.Role == "admin" {
		t.Fatalf("kate after the look-alike writes = %+v, want unchanged", u)
	}
}

// A legacy import's conversation or memory whose owner ImportUser would
// refuse is refused too, instead of being filed under the ASCII account the
// owner lowercases to.
func TestImportedRowsRefuseALookalikeOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "kate@example.com", "password-long-enough"); err != nil {
		t.Fatal(err)
	}
	const lookalike = "\u212aate@example.com"
	if _, err := s.ImportConversation(ctx, ImportedConversation{ID: "legacy-conv-1", UserEmail: lookalike, Title: "t", CreatedAt: 1, UpdatedAt: 1}); !IsInputError(err) {
		t.Errorf("ImportConversation owned by %q: err = %v, want an input error", lookalike, err)
	}
	if _, err := s.ImportMemory(ctx, ImportedMemory{ID: "legacy-mem-1", UserEmail: lookalike, Content: "c", Source: "manual", CreatedAt: 1, UpdatedAt: 1}, false); !IsInputError(err) {
		t.Errorf("ImportMemory owned by %q: err = %v, want an input error", lookalike, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM conversations WHERE user_email = 'kate@example.com') + (SELECT count(*) FROM memories WHERE user_email = 'kate@example.com')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows filed under kate = %d (err %v), want 0", n, err)
	}
}

// Only an entry the list rule admits is written into the exclusion list. A
// remove value with a backslash — the literal characters x\u0000@y.com — is
// only deleted: written back, it would read as the sequence a damaged list
// carries, and the listing would then hide the chat from an invited member.
func TestMultiplayerRemoveWritesOnlyValidExclusions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := s.CreateUser(ctx, email, "password-long-enough"); err != nil {
			t.Fatal(err)
		}
	}
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConversationMultiplayer(ctx, "alice@example.com", conv.ID, true, []string{"bob@example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET team_shared_with = 'eng' WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpdateConversationMultiplayerAccess(ctx, "alice@example.com", conv.ID, nil, []string{`x\u0000@y.com`})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(got.MultiplayerExcluded) != 0 {
		t.Fatalf("excluded = %q, want nothing written", got.MultiplayerExcluded)
	}
	convs, err := s.ListSharedAndMultiplayerConversations(ctx, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range convs {
		found = found || c.ID == conv.ID
	}
	if !found {
		t.Fatal("bob, an invited member, no longer sees the chat")
	}
}
