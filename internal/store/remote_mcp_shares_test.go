package store

import (
	"context"
	"errors"
	"testing"
)

// Sharing a connection (#443 follow-up): grants are owner-managed, resolve for
// the grantee (directly or via the everyone wildcard), keep the owner's email
// on the row (the AEAD AAD binds secrets to the owner), and die with the
// server row (cascade).
func TestRemoteMCPShares(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const owner = "owner@elcano.com"
	const mate = "mate@elcano.com"
	const other = "other@elcano.com"

	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(owner))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("grant and list", func(t *testing.T) {
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, "Mate@Elcano.com"); err != nil {
			t.Fatalf("share: %v", err)
		}
		// Idempotent re-grant.
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, mate); err != nil {
			t.Fatalf("re-share: %v", err)
		}
		byOwner, err := s.ListRemoteMCPSharesByOwner(ctx, owner)
		if err != nil {
			t.Fatalf("by owner: %v", err)
		}
		if got := byOwner[srv.ID]; len(got) != 1 || got[0] != mate {
			t.Errorf("shares = %v, want [%s] (normalized, deduped)", got, mate)
		}
	})

	t.Run("grantee resolution", func(t *testing.T) {
		shared, err := s.ListRemoteMCPServersSharedWith(ctx, mate)
		if err != nil {
			t.Fatalf("shared with: %v", err)
		}
		if len(shared) != 1 || shared[0].ID != srv.ID || shared[0].UserEmail != owner {
			t.Fatalf("shared = %+v, want owner-attributed row", shared)
		}
		if _, err := s.ListRemoteMCPServersSharedWith(ctx, other); err != nil {
			t.Fatalf("shared with other: %v", err)
		} else if got, _ := s.ListRemoteMCPServersSharedWith(ctx, other); len(got) != 0 {
			t.Errorf("other should see nothing, got %+v", got)
		}
		use, err := s.GetRemoteMCPServerForUse(ctx, mate, srv.ID)
		if err != nil {
			t.Fatalf("for use: %v", err)
		}
		if use.UserEmail != owner {
			t.Errorf("for-use row must keep the OWNER email (AAD binding), got %q", use.UserEmail)
		}
		if _, err := s.GetRemoteMCPServerForUse(ctx, other, srv.ID); !errors.Is(err, ErrRemoteMCPNotFound) {
			t.Errorf("ungranted user must not resolve the server, got %v", err)
		}
	})

	t.Run("everyone wildcard", func(t *testing.T) {
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, GranteeEveryone); err != nil {
			t.Fatalf("share everyone: %v", err)
		}
		got, err := s.ListRemoteMCPServersSharedWith(ctx, other)
		if err != nil || len(got) != 1 {
			t.Fatalf("everyone grant should reach other users: %v %+v", err, got)
		}
		// The owner's own listing of shared-with-me stays empty (no self-share).
		own, _ := s.ListRemoteMCPServersSharedWith(ctx, owner)
		if len(own) != 0 {
			t.Errorf("owner must not see their own server as shared-with-me: %+v", own)
		}
		if err := s.UnshareRemoteMCPServer(ctx, owner, srv.ID, GranteeEveryone); err != nil {
			t.Fatalf("unshare everyone: %v", err)
		}
	})

	t.Run("owner-only management and validation", func(t *testing.T) {
		if err := s.ShareRemoteMCPServer(ctx, mate, srv.ID, other); !errors.Is(err, ErrRemoteMCPNotFound) {
			t.Errorf("non-owner share must fail with not-found, got %v", err)
		}
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, owner); !errors.Is(err, ErrRemoteMCPShareInvalid) {
			t.Errorf("self-share must be invalid, got %v", err)
		}
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, "not-an-email"); !errors.Is(err, ErrRemoteMCPShareInvalid) {
			t.Errorf("garbage grantee must be invalid, got %v", err)
		}
		if err := s.UnshareRemoteMCPServer(ctx, owner, srv.ID, "ghost@elcano.com"); !errors.Is(err, ErrRemoteMCPNotFound) {
			t.Errorf("revoking an unknown grant must not read as success, got %v", err)
		}
	})

	t.Run("revocation and cascade", func(t *testing.T) {
		if err := s.UnshareRemoteMCPServer(ctx, owner, srv.ID, mate); err != nil {
			t.Fatalf("unshare: %v", err)
		}
		if got, _ := s.ListRemoteMCPServersSharedWith(ctx, mate); len(got) != 0 {
			t.Errorf("revoked grant still resolves: %+v", got)
		}
		if err := s.ShareRemoteMCPServer(ctx, owner, srv.ID, mate); err != nil {
			t.Fatalf("re-share: %v", err)
		}
		if err := s.DeleteRemoteMCPServer(ctx, owner, srv.ID); err != nil {
			t.Fatalf("delete server: %v", err)
		}
		if got, _ := s.ListRemoteMCPServersSharedWith(ctx, mate); len(got) != 0 {
			t.Errorf("grants must cascade with the server row: %+v", got)
		}
	})
}
