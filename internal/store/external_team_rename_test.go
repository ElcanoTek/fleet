// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"testing"
)

// A team rename from central Auth is the same share-keeping rename an admin's
// Rename team does, applied once per version: a redelivery or an older
// version changes nothing, and a rename of a team nothing here is in is
// recorded as done.
func TestApplyExternalTeamRenameKeepsSharesOncePerVersion(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")
	const issuer = "https://auth.example.com"
	apply := func(version int64, from, to string) (bool, int64) {
		t.Helper()
		applied, users, err := f.s.ApplyExternalTeamRename(f.ctx, ExternalTeamRename{
			Issuer: issuer + "/", Version: version, EventID: "evt", From: from, To: to,
		}, 1000+version)
		if err != nil {
			t.Fatalf("ApplyExternalTeamRename(%d %s->%s): %v", version, from, to, err)
		}
		return applied, users
	}
	if applied, users := apply(1, "quant", "research"); !applied || users != 2 {
		t.Fatalf("first rename: applied=%v users=%d", applied, users)
	}
	if !teamShareReadable(t, f, "alice@x.com", c.ID) {
		t.Error("the share must stay with the renamed team")
	}
	if p, err := f.s.GetProject(f.ctx, f.project.ID); err != nil || p.TeamID != "research" {
		t.Errorf("project team after rename = %+v %v", p, err)
	}
	// A redelivery, and an older version, change nothing: renaming "research"
	// back would detach the share if either ran.
	if applied, _ := apply(1, "research", "quant"); applied {
		t.Error("a redelivered version was applied")
	}
	if u, _ := f.s.GetUser(f.ctx, "alice@x.com"); u.TeamID != "research" {
		t.Fatalf("redelivery moved alice to %q", u.TeamID)
	}
	// A rename of a team nobody here is in is done, and recorded.
	if applied, users := apply(2, "nobody-here", "still-nobody"); !applied || users != 0 {
		t.Fatalf("rename of an absent team: applied=%v users=%d", applied, users)
	}
	if applied, _ := apply(2, "research", "quant"); applied {
		t.Error("version 2 applied twice")
	}
	// A merge: renaming onto an existing team joins the two.
	if applied, users := apply(3, "ops", "research"); !applied || users != 1 {
		t.Fatalf("merge: applied=%v users=%d", applied, users)
	}
	if u, _ := f.s.GetUser(f.ctx, "dana@x.com"); u.TeamID != "research" {
		t.Errorf("dana after the merge: %q", u.TeamID)
	}
	// Malformed renames are input errors.
	for _, bad := range []ExternalTeamRename{
		{Issuer: issuer, Version: 4, EventID: "e", From: "", To: "x"},
		{Issuer: issuer, Version: 0, EventID: "e", From: "a", To: "b"},
		{Issuer: issuer, Version: 4, EventID: "e", From: "a", To: " padded "},
		{Issuer: "", Version: 4, EventID: "e", From: "a", To: "b"},
	} {
		if _, _, err := f.s.ApplyExternalTeamRename(f.ctx, bad, 2000); !IsInputError(err) {
			t.Errorf("%+v: err = %v, want an input error", bad, err)
		}
	}
}

// An older version than the last applied one is skipped too, not only an
// equal one, and two deliveries of one version racing each other apply it
// once: the per-issuer lock serializes the version check.
func TestApplyExternalTeamRenameOlderAndConcurrent(t *testing.T) {
	f := newTeamFixture(t)
	const issuer = "https://auth.example.com"
	if applied, _, err := f.s.ApplyExternalTeamRename(f.ctx, ExternalTeamRename{Issuer: issuer, Version: 50, EventID: "e50", From: "quant", To: "research"}, 1000); err != nil || !applied {
		t.Fatalf("version 50: %v %v", applied, err)
	}
	if applied, _, err := f.s.ApplyExternalTeamRename(f.ctx, ExternalTeamRename{Issuer: issuer, Version: 30, EventID: "e30", From: "research", To: "quant"}, 1001); err != nil || applied {
		t.Fatalf("an older version was applied: %v %v", applied, err)
	}
	if u, _ := f.s.GetUser(f.ctx, "alice@x.com"); u.TeamID != "research" {
		t.Fatalf("an older version moved alice to %q", u.TeamID)
	}
	for round := int64(0); round < 5; round++ {
		version := 60 + round
		from, to := "research", "quant"
		if round%2 == 1 {
			from, to = "quant", "research"
		}
		start := make(chan struct{})
		results := make(chan bool, 4)
		for range 4 {
			go func() {
				<-start
				applied, _, err := f.s.ApplyExternalTeamRename(f.ctx, ExternalTeamRename{Issuer: issuer, Version: version, EventID: "e", From: from, To: to}, 2000+version)
				if err != nil {
					t.Errorf("concurrent apply: %v", err)
				}
				results <- applied
			}()
		}
		close(start)
		n := 0
		for range 4 {
			if <-results {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("version %d applied %d times", version, n)
		}
	}
}

// A team the provider wrote before a rename Fleet has applied names the team
// by its old name; applying it would move the person back and unshare their
// work. It is ignored (roles still apply). A team written at or after the
// rename, or by a provider that sends no rename version, applies as before.
func TestExternalAccessIgnoresATeamWrittenBeforeAnAppliedRename(t *testing.T) {
	f := newTeamFixture(t)
	const issuer = "https://auth.example.com"
	if _, _, err := f.s.ApplyExternalTeamRename(f.ctx, ExternalTeamRename{Issuer: issuer, Version: 100, EventID: "r", From: "quant", To: "research"}, 1000); err != nil {
		t.Fatal(err)
	}
	gen := func(v int64) *int64 { return &v }
	team := func(s string) *string { return &s }
	push := func(version int64, to string, rename *int64, role string) {
		t.Helper()
		if _, _, err := f.s.ApplyExternalAccess(f.ctx, ExternalAccessState{
			Issuer: issuer, Subject: "sub-alice", Email: "alice@x.com", Version: version, Allowed: true,
			EventID: "e", ChatRole: role, OpsRole: "none", Team: team(to), IssuedAt: 1000, TeamRename: rename,
		}); err != nil {
			t.Fatalf("ApplyExternalAccess v%d: %v", version, err)
		}
	}
	// Written before rename 100: the old name is ignored, the role applies.
	push(1, "quant", gen(99), "viewer")
	if u, _ := f.s.GetUser(f.ctx, "alice@x.com"); u.TeamID != "research" || u.Role != "viewer" {
		t.Fatalf("stale team applied: team %q role %q", u.TeamID, u.Role)
	}
	// Written after it: a real move applies.
	push(2, "ops", gen(100), "member")
	if u, _ := f.s.GetUser(f.ctx, "alice@x.com"); u.TeamID != "ops" {
		t.Fatalf("a current team was ignored: %q", u.TeamID)
	}
	// No rename version (an older Auth): applies as before.
	push(3, "research", nil, "member")
	if u, _ := f.s.GetUser(f.ctx, "alice@x.com"); u.TeamID != "research" {
		t.Fatalf("a team without a rename version was ignored: %q", u.TeamID)
	}
}
