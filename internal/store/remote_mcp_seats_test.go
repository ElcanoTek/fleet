package store

import (
	"context"
	"errors"
	"testing"
)

// Seats (#988): several logins under one connection name, each its own row
// with its own sealed credential; exactly one default per (user, name).

func seatInput(email, name, account string) RemoteMCPServerInput {
	in := sampleServerInput(email)
	in.Name = name
	in.Account = account
	return in
}

func TestRemoteMCPSeatsCreateDefaultAndUniqueness(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const email = "u@x.com"

	first, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "github", ""))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if !first.IsDefault || first.Account != "" {
		t.Fatalf("first seat = account %q default %v, want unlabeled default", first.Account, first.IsDefault)
	}
	// A second unlabeled login under the same name is the old duplicate.
	if _, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "github", "")); !errors.Is(err, ErrRemoteMCPSeatExists) {
		t.Fatalf("second unlabeled seat err = %v, want ErrRemoteMCPSeatExists", err)
	}
	// Labels canonicalize (case + separators) and must be well-formed.
	work, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "github", " Work-Acct "))
	if err != nil {
		t.Fatalf("create labeled: %v", err)
	}
	if work.Account != "work_acct" || work.IsDefault {
		t.Fatalf("labeled seat = account %q default %v, want work_acct non-default", work.Account, work.IsDefault)
	}
	if _, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "github", "WORK_ACCT")); !errors.Is(err, ErrRemoteMCPSeatExists) {
		t.Fatalf("case-twin seat err = %v, want ErrRemoteMCPSeatExists", err)
	}
	if _, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "github", "bad/label")); !errors.Is(err, ErrRemoteMCPAccountInvalid) {
		t.Fatalf("bad label err = %v, want ErrRemoteMCPAccountInvalid", err)
	}
	// Another user's same-named seats are independent.
	if _, err := s.CreateRemoteMCPServer(ctx, seatInput("other@x.com", "github", "work_acct")); err != nil {
		t.Fatalf("other user's seat: %v", err)
	}

	// Each seat holds its own secrets under the same (owner, url) AAD.
	if secret, _, err := s.LoadServerSecrets(ctx, work); err != nil || secret != "shh-secret" {
		t.Fatalf("labeled seat secrets = %q, %v", secret, err)
	}

	list, err := s.ListRemoteMCPServers(ctx, email)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d, err %v", len(list), err)
	}
}

func TestRemoteMCPSeatsSetDefaultRenameAndDeletePromotion(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const email = "u@x.com"

	legacy, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "gamma", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	team, err := s.CreateRemoteMCPServer(ctx, seatInput(email, "gamma", "team"))
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.SetRemoteMCPDefaultSeat(ctx, email, team.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if err := s.SetRemoteMCPDefaultSeat(ctx, email, team.ID); err != nil {
		t.Fatalf("set default (idempotent): %v", err)
	}
	if err := s.SetRemoteMCPDefaultSeat(ctx, "other@x.com", team.ID); !errors.Is(err, ErrRemoteMCPNotFound) {
		t.Fatalf("foreign set default err = %v", err)
	}
	got, _ := s.GetRemoteMCPServer(ctx, email, team.ID)
	old, _ := s.GetRemoteMCPServer(ctx, email, legacy.ID)
	if !got.IsDefault || old.IsDefault {
		t.Fatalf("default flags after set: team=%v legacy=%v", got.IsDefault, old.IsDefault)
	}

	// Rename keeps the credential readable (AAD does not include the label).
	if err := s.RenameRemoteMCPAccount(ctx, email, legacy.ID, "Primary"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	renamed, _ := s.GetRemoteMCPServer(ctx, email, legacy.ID)
	if renamed.Account != "primary" {
		t.Fatalf("renamed account = %q", renamed.Account)
	}
	if secret, _, err := s.LoadServerSecrets(ctx, renamed); err != nil || secret != "shh-secret" {
		t.Fatalf("secrets after rename = %q, %v", secret, err)
	}
	if err := s.RenameRemoteMCPAccount(ctx, email, legacy.ID, "team"); !errors.Is(err, ErrRemoteMCPSeatExists) {
		t.Fatalf("rename onto a taken label err = %v", err)
	}

	// Deleting the default promotes a remaining seat so the name keeps one.
	if err := s.DeleteRemoteMCPServer(ctx, email, team.ID); err != nil {
		t.Fatalf("delete default: %v", err)
	}
	promoted, _ := s.GetRemoteMCPServer(ctx, email, legacy.ID)
	if !promoted.IsDefault {
		t.Fatal("remaining seat was not promoted to default")
	}
}

// Sharing is per seat: a grant on "work" must not expose "personal".
func TestRemoteMCPSeatsShareIsPerSeat(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const owner, mate = "owner@x.com", "mate@x.com"
	work, err := s.CreateRemoteMCPServer(ctx, seatInput(owner, "github", "work"))
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := s.CreateRemoteMCPServer(ctx, seatInput(owner, "github", "personal")); err != nil {
		t.Fatalf("create personal: %v", err)
	}
	if err := s.ShareRemoteMCPServer(ctx, owner, work.ID, mate); err != nil {
		t.Fatalf("share: %v", err)
	}
	shared, err := s.ListRemoteMCPServersSharedWith(ctx, mate)
	if err != nil || len(shared) != 1 || shared[0].ID != work.ID || shared[0].Account != "work" {
		t.Fatalf("shared with mate = %+v, err %v; want only the work seat", shared, err)
	}
}
