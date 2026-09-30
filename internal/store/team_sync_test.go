package store

import (
	"strings"
	"testing"
)

// teamOf reads email's users.team_id ("" when NULL), disabled accounts
// included (GetUser hides those).
func teamOf(t *testing.T, f teamFixture, email string) string {
	t.Helper()
	a, ok, err := f.s.AccountAccess(f.ctx, email)
	if err != nil || !ok {
		t.Fatalf("AccountAccess %s = (%v, %v)", email, ok, err)
	}
	return a.Team
}

func providerTeam(t *testing.T, f teamFixture, subject string) *string {
	t.Helper()
	st, ok, err := f.s.ExternalAccessState(f.ctx, "https://auth.example.com", subject)
	if err != nil || !ok {
		t.Fatalf("ExternalAccessState %s = (%v, %v)", subject, ok, err)
	}
	return st.Team
}

func teamGrant(email, subject string, version int64, team *string) ExternalAccessState {
	return ExternalAccessState{
		Issuer: "https://auth.example.com", Subject: subject, Email: email,
		Version: version, Allowed: true, EventID: "grant", ChatRole: RoleMember,
		OpsRole: "none", Team: team, IssuedAt: 1_000,
	}
}

func strPtr(s string) *string { return &s }

// A grant carrying a team moves the person with the same unsharing an admin
// move does; a grant without one leaves the team alone; "" clears it.
func TestApplyExternalAccessAppliesTeamWithUnsharing(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's shared chat")
	if got, err := f.s.GetTeamVisibleConversation(f.ctx, "alice@x.com", c.ID); err != nil || got == nil {
		t.Fatalf("precondition: alice cannot see bob's shared chat: (%v, %v)", got, err)
	}

	if _, changed, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("bob@x.com", "bob", 1, nil)); err != nil || !changed {
		t.Fatalf("grant without team = (%v, %v)", changed, err)
	}
	if got := teamOf(t, f, "bob@x.com"); got != "quant" {
		t.Fatalf("team after a grant without team = %q, want quant (untouched)", got)
	}
	if got := providerTeam(t, f, "bob"); got != nil {
		t.Fatalf("provider team = %q, want NULL (not managed)", *got)
	}

	if _, changed, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("bob@x.com", "bob", 2, strPtr(" ops "))); err != nil || !changed {
		t.Fatalf("grant with team = (%v, %v)", changed, err)
	}
	if got := teamOf(t, f, "bob@x.com"); got != "ops" {
		t.Fatalf("team = %q, want ops", got)
	}
	if got := providerTeam(t, f, "bob"); got == nil || *got != "ops" {
		t.Fatalf("provider team = %v, want ops", got)
	}
	// The move unshared bob's chat from the team he left.
	if got, err := f.s.GetTeamVisibleConversation(f.ctx, "alice@x.com", c.ID); err != nil || got != nil {
		t.Fatalf("alice still sees bob's chat after the provider moved him out of quant: (%v, %v)", got, err)
	}

	if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("bob@x.com", "bob", 3, strPtr(""))); err != nil {
		t.Fatalf("grant clearing team: %v", err)
	}
	if got := teamOf(t, f, "bob@x.com"); got != "" {
		t.Fatalf("team = %q, want cleared", got)
	}
}

// A revoke never changes the team, even when it carries one.
func TestApplyExternalAccessRevokeKeepsTeam(t *testing.T) {
	f := newTeamFixture(t)
	revoke := teamGrant("alice@x.com", "alice", 1, strPtr("ops"))
	revoke.Allowed = false
	if _, _, err := f.s.ApplyExternalAccess(f.ctx, revoke); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := teamOf(t, f, "alice@x.com"); got != "quant" {
		t.Fatalf("team after revoke = %q, want quant", got)
	}
}

// A label that differs only in case is left exactly as it is: team gates match
// exactly, so rewriting the case would detach the person from their team's
// shared projects.
func TestApplyExternalAccessKeepsTeamThatDiffersOnlyInCase(t *testing.T) {
	f := newTeamFixture(t)
	if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("alice@x.com", "alice", 1, strPtr("QUANT"))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := teamOf(t, f, "alice@x.com"); got != "quant" {
		t.Fatalf("team = %q, want quant unchanged", got)
	}
}

// A newly provisioned account is created in the provider's team.
func TestApplyExternalAccessCreatesAccountInTeam(t *testing.T) {
	f := newTeamFixture(t)
	if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("erin@x.com", "erin", 1, strPtr("ops"))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := teamOf(t, f, "erin@x.com"); got != "ops" {
		t.Fatalf("team = %q, want ops", got)
	}
}

func TestApplyExternalAccessRejectsInvalidTeam(t *testing.T) {
	f := newTeamFixture(t)
	for _, team := range []string{strings.Repeat("x", maxTeamNameLen+1), "bad\x07team"} {
		if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("alice@x.com", "alice", 1, strPtr(team))); err == nil {
			t.Errorf("team %q accepted", team)
		}
	}
	if !ValidExternalTeam(strings.Repeat("x", maxTeamNameLen)) || !ValidExternalTeam("") {
		t.Error("a 64-byte team or no team must be valid")
	}
}

// A Fleet-side team change is adopted into a provider row that manages team,
// and never into one that does not (NULL stays NULL).
func TestAdoptFleetAccessChangeAdoptsTeamOnlyWhenManaged(t *testing.T) {
	f := newTeamFixture(t)
	if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("alice@x.com", "alice", 1, strPtr("quant"))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ApplyExternalAccess(f.ctx, teamGrant("bob@x.com", "bob", 1, nil)); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"alice@x.com", "bob@x.com"} {
		token, err := f.s.ProviderStateToken(f.ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := f.s.AdoptFleetAccessChange(f.ctx, email, token, true, true, RoleMember, "none", "ops"); err != nil || !ok {
			t.Fatalf("adopt %s = (%v, %v)", email, ok, err)
		}
	}
	if got := providerTeam(t, f, "alice"); got == nil || *got != "ops" {
		t.Fatalf("alice provider team = %v, want ops (adopted)", got)
	}
	if got := providerTeam(t, f, "bob"); got != nil {
		t.Fatalf("bob provider team = %q, want NULL (not managed)", *got)
	}
	// A deletion adopts allowed=false and leaves the team alone.
	token, _ := f.s.ProviderStateToken(f.ctx, "alice@x.com")
	if _, err := f.s.AdoptFleetAccessChange(f.ctx, "alice@x.com", token, false, false, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := providerTeam(t, f, "alice"); got == nil || *got != "ops" {
		t.Fatalf("alice provider team after deletion = %v, want ops", got)
	}
}

func TestTeamMemberEmails(t *testing.T) {
	f := newTeamFixture(t)
	got, err := f.s.TeamMemberEmails(f.ctx, " quant ")
	if err != nil || strings.Join(got, ",") != "alice@x.com,bob@x.com" {
		t.Fatalf("TeamMemberEmails = (%v, %v), want alice, bob", got, err)
	}
	if got, _ := f.s.TeamMemberEmails(f.ctx, "Quant"); len(got) != 0 {
		t.Fatalf("case-differing team matched %v; RenameTeam matches exactly", got)
	}
}

// Events carry the team, and the Chat read the Recorder does reports it.
func TestAccountAccessAndEventsCarryTeam(t *testing.T) {
	f := newTeamFixture(t)
	a, ok, err := f.s.AccountAccess(f.ctx, "alice@x.com")
	if err != nil || !ok || a.Team != "quant" {
		t.Fatalf("AccountAccess = (%+v, %v, %v), want team quant", a, ok, err)
	}
	all, err := f.s.ListAccountAccess(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.Email == "carol@x.com" && a.Team != "" {
			t.Fatalf("carol team = %q, want none", a.Team)
		}
	}
	if _, err := f.s.EnqueueAccountEvent(f.ctx, AccountEvent{Type: AccountEventAccessChanged, Email: "alice@x.com",
		Enabled: true, ChatRole: RoleMember, OpsRole: "none", Team: "quant", Source: AccountEventSourceAdminUI}); err != nil {
		t.Fatal(err)
	}
	evs, err := f.s.ClaimDueAccountEvents(f.ctx, 1<<40, 10, 60)
	if err != nil || len(evs) != 1 || evs[0].Team != "quant" {
		t.Fatalf("claimed = (%+v, %v), want team quant", evs, err)
	}
}
