package store

import (
	"errors"
	"testing"
	"time"
)

// teamShareReadable reports whether caller can reach convID through every
// team door: the transcript, the cheap gate, and the team listing.
func teamShareReadable(t *testing.T, f teamFixture, caller, convID string) bool {
	t.Helper()
	snap, err := f.s.GetTeamVisibleConversation(f.ctx, caller, convID)
	if err != nil {
		t.Fatalf("GetTeamVisibleConversation(%s): %v", caller, err)
	}
	can, err := f.s.CanTeamRead(f.ctx, caller, convID)
	if err != nil {
		t.Fatalf("CanTeamRead(%s): %v", caller, err)
	}
	listed := false
	if list, err := f.s.ListTeamConversations(f.ctx, caller); err == nil {
		for _, c := range list {
			if c.ID == convID {
				listed = true
			}
		}
	} else if !errors.Is(err, ErrNoTeam) {
		t.Fatalf("ListTeamConversations(%s): %v", caller, err)
	}
	if (snap != nil) != can || can != listed {
		t.Fatalf("team doors disagree for %s: transcript=%v gate=%v listed=%v", caller, snap != nil, can, listed)
	}
	return can
}

// Renaming a team keeps its shares with it, and a team someone later forms
// under the FREED name reads nothing.
//
// Red/green: the share used to name its audience by label only, and
// RenameTeam relabelled users and projects but not the share. After a rename
// the share still said "quant", the renamed team no longer matched it, and
// anyone who then created a team called "quant" matched it and could read the
// original team's chats and shared outputs.
func TestRenameTeamKeepsSharesAndAFreedNameInheritsNothing(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	if !teamShareReadable(t, f, "alice@x.com", c.ID) {
		t.Fatal("fixture: a teammate must read the shared chat")
	}

	if _, _, err := f.s.RenameTeam(f.ctx, "quant", "research"); err != nil {
		t.Fatalf("RenameTeam: %v", err)
	}
	if !teamShareReadable(t, f, "alice@x.com", c.ID) {
		t.Error("after a rename the share must stay with the renamed team")
	}
	got, err := f.s.GetTeamVisibleConversationMeta(f.ctx, "alice@x.com", c.ID)
	if err != nil || got == nil || got.TeamID != "research" {
		t.Errorf("shared-with label after rename = %+v (%v), want research", got, err)
	}

	// carol (no team) forms a team under the freed name, self-serve.
	if _, err := f.s.SetOwnTeam(f.ctx, "carol@x.com", "quant", false); err != nil {
		t.Fatalf("SetOwnTeam(quant) after the rename freed it: %v", err)
	}
	if teamShareReadable(t, f, "carol@x.com", c.ID) {
		t.Error("a team formed under a renamed team's old name must not read its shares")
	}
	link, err := f.s.ResolveTeamLink(f.ctx, "carol@x.com", c.ID)
	if err != nil || link.Status == TeamLinkOpen {
		t.Errorf("ResolveTeamLink for the new 'quant' = %+v (%v), want not open", link, err)
	}
	// The owner's own bookkeeping still sees the share under the new name.
	impact, err := f.s.LeaveTeamImpact(f.ctx, "bob@x.com", "research")
	if err != nil || impact.SharedChats == nil || *impact.SharedChats != 1 {
		t.Errorf("LeaveTeamImpact(research) = %+v (%v), want 1 shared chat", impact, err)
	}
}

// The read gates require the stamped team ID, not only a matching label: a
// stamp whose id is not the id currently registered under that label is not a
// share anyone can read.
func TestTeamShareGateRequiresTheTeamID(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO teams (name, created_at) VALUES ('elsewhere', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE conversations SET team_shared_with_id = (SELECT id FROM teams WHERE name = 'elsewhere') WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if teamShareReadable(t, f, "alice@x.com", c.ID) {
		t.Error("a label match with the wrong team id must not open the share")
	}
}

// A team whose every member is gone leaves its teams row behind. The next
// person to take that name forms a NEW team: the old row is retired, its
// leftover shares are unshared, and nothing is inherited.
func TestJoiningAnUnheldTeamNameRetiresTheOldTeam(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	// Simulate the members vanishing without the unshare path running (an
	// account removed out of band): nobody holds "quant" any more.
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE users SET team_id = NULL WHERE team_id = 'quant'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE projects SET team_id = '' WHERE team_id = 'quant'`); err != nil {
		t.Fatal(err)
	}
	quant := "quant"
	if _, err := f.s.SetUserRoleTeam(f.ctx, "carol@x.com", nil, &quant); err != nil {
		t.Fatalf("admin move carol → quant: %v", err)
	}
	if teamShareReadable(t, f, "carol@x.com", c.ID) {
		t.Error("a new team under a dead team's name must not read its shares")
	}
	var visible bool
	var n int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT team_visible FROM conversations WHERE id = $1`, c.ID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Error("the dead team's share must be cleared, not just hidden")
	}
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM teams WHERE name = 'quant'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the dead team's row must be retired, %d left", n)
	}
}

// Renaming onto an existing team is a merge (RenameTeam's documented
// behavior): the merged team is the audience, and the old row goes away.
func TestRenameOntoExistingTeamMergesShares(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	// ops gets a teams row of its own (a share of dana's) so the merge path runs.
	opsProject, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "dana@x.com", Name: "Ops", TeamID: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	d := f.sharedChat(t, "dana@x.com", opsProject.ID, "Ops note")

	if _, _, err := f.s.RenameTeam(f.ctx, "quant", "ops"); err != nil {
		t.Fatalf("RenameTeam onto ops: %v", err)
	}
	if !teamShareReadable(t, f, "dana@x.com", c.ID) || !teamShareReadable(t, f, "alice@x.com", d.ID) {
		t.Error("after a merge both teams' shares belong to the merged team")
	}
	var n int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*) FROM teams WHERE name = 'quant'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the merged-away team's row must be gone, %d left", n)
	}
}

// Migration 084 on data written before it: a live share gets its team's id; a
// share a pre-fix rename left behind (its label no longer its owner's team) is
// unshared rather than guessed at. Applied in a rolled-back transaction, like
// TestMigration055UnfilesChatsTheirOwnersCannotSee.
func TestMigration084StampsIDsAndUnsharesRenameOrphans(t *testing.T) {
	f := newTeamFixture(t)
	orphan := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	opsProject, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "dana@x.com", Name: "Ops", TeamID: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	live := f.sharedChat(t, "dana@x.com", opsProject.ID, "Ops note")

	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// The pre-084 world: no ids, and the old RenameTeam's footprint (users and
	// projects relabelled, the share left saying "quant").
	for _, q := range []string{
		`UPDATE conversations SET team_shared_with_id = NULL`,
		`DELETE FROM teams`,
		`UPDATE users SET team_id = 'research' WHERE team_id = 'quant'`,
		`UPDATE projects SET team_id = 'research' WHERE team_id = 'quant'`,
	} {
		if _, err := tx.ExecContext(f.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sqlText := embeddedMigrationSQL(t, 84)
	for i := 0; i < 2; i++ { // idempotent
		if _, err := tx.ExecContext(f.ctx, sqlText); err != nil {
			t.Fatalf("apply migration 084 (pass %d): %v", i+1, err)
		}
	}

	var visible bool
	var label *string
	var id *int64
	if err := tx.QueryRowContext(f.ctx,
		`SELECT team_visible, team_shared_with, team_shared_with_id FROM conversations WHERE id = $1`, orphan.ID,
	).Scan(&visible, &label, &id); err != nil {
		t.Fatal(err)
	}
	if visible || label != nil || id != nil {
		t.Errorf("rename orphan = visible %v / label %v / id %v, want unshared", visible, label, id)
	}
	var stampedOK bool
	if err := tx.QueryRowContext(f.ctx, `
		SELECT c.team_visible AND c.team_shared_with = 'ops'
		       AND c.team_shared_with_id = (SELECT t.id FROM teams t WHERE t.name = 'ops')
		  FROM conversations c WHERE c.id = $1`, live.ID).Scan(&stampedOK); err != nil {
		t.Fatal(err)
	}
	if !stampedOK {
		t.Error("a live share must keep its audience and gain its team's id")
	}
	var quantRows int
	if err := tx.QueryRowContext(f.ctx, `SELECT count(*) FROM teams WHERE name = 'quant'`).Scan(&quantRows); err != nil {
		t.Fatal(err)
	}
	if quantRows != 0 {
		t.Error("the orphan's stale label must not be registered as a team")
	}
}

// A FIRST share for a team racing a rename of that team must end with the
// share carried by the rename, never left under the old name.
//
// Red/green (Codex P1 on #1853): the first share INSERTs the team's teams
// row; until that commits, the rename's SELECT ... FOR UPDATE cannot see it,
// so the rename finished without relabelling and the share committed as
// "quant" with quant's id — invisible to the renamed team, and a label a
// later team reusing the name could match. The test holds the share open
// after its stamp (teamShareBeforeCommitHook), starts the rename, gives it
// time to finish if nothing stops it, then lets the share commit. With the
// name-scoped advisory lock the rename waits for the share and relabels it;
// without it the rename completes inside the window and the share is
// stranded under "quant".
func TestFirstShareRacingRenameIsCarriedByTheRename(t *testing.T) {
	f := newTeamFixture(t)
	c, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Quant secret", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "bob@x.com", c.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}

	stamped := make(chan struct{})
	release := make(chan struct{})
	teamShareBeforeCommitHook = func() {
		close(stamped)
		<-release
	}
	t.Cleanup(func() { teamShareBeforeCommitHook = nil })

	shareErr := make(chan error, 1)
	go func() {
		_, err := f.s.SetConversationTeamVisible(f.ctx, "bob@x.com", c.ID, true)
		shareErr <- err
	}()
	<-stamped
	teamShareBeforeCommitHook = nil // the rename's own writes must not pause

	renameErr := make(chan error, 1)
	go func() {
		_, _, err := f.s.RenameTeam(f.ctx, "quant", "research")
		renameErr <- err
	}()
	select {
	case err := <-renameErr: // finished while the share was still open
		renameErr <- err
	case <-time.After(1500 * time.Millisecond): // waiting on the share, as it should
	}
	close(release)
	if err := <-shareErr; err != nil {
		t.Fatalf("share: %v", err)
	}
	if err := <-renameErr; err != nil {
		t.Fatalf("rename: %v", err)
	}

	var label string
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT COALESCE(team_shared_with, '') FROM conversations WHERE id = $1`, c.ID).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "research" {
		t.Errorf("share label after the race = %q, want research (carried by the rename)", label)
	}
	if !teamShareReadable(t, f, "alice@x.com", c.ID) {
		t.Error("the renamed team must read a share that raced the rename")
	}
	if _, err := f.s.SetOwnTeam(f.ctx, "carol@x.com", "quant", false); err != nil {
		t.Fatalf("SetOwnTeam(quant): %v", err)
	}
	if teamShareReadable(t, f, "carol@x.com", c.ID) {
		t.Error("a team reusing the old name must not read the share")
	}
}

// An admin moving a member into a team while that member's team is being
// renamed onto the same name must queue, not deadlock.
//
// Red/green (Codex P2 on #1853): the move locked the member's users row and
// then waited for the destination name's advisory lock; RenameTeam held both
// names' locks and then waited for that users row. The test makes the
// interleaving deterministic: a separate transaction holds bob's row, the
// move queues on it first, the rename second, and then the row is released —
// so the move gets the row first and asks for the name lock the rename holds.
// With "team-name locks before user rows" both complete.
func TestTeamMoveRacingRenameDoesNotDeadlock(t *testing.T) {
	f := newTeamFixture(t)
	waiting := func() int {
		var n int
		if err := f.s.db.QueryRowContext(f.ctx,
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for waiting() < n {
			if time.Now().After(deadline) {
				t.Fatalf("expected %d blocked lock waiters", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	holder, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(f.ctx, `SELECT 1 FROM users WHERE email = 'bob@x.com' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	research := "research"
	moveErr := make(chan error, 1)
	go func() {
		_, err := f.s.SetUserRoleTeam(f.ctx, "bob@x.com", nil, &research)
		moveErr <- err
	}()
	waitFor(1)
	renameErr := make(chan error, 1)
	go func() {
		_, _, err := f.s.RenameTeam(f.ctx, "quant", "research")
		renameErr <- err
	}()
	waitFor(2)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-moveErr; err != nil {
		t.Errorf("move: %v", err)
	}
	if err := <-renameErr; err != nil {
		t.Errorf("rename: %v", err)
	}
}
