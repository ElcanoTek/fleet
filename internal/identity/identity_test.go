package identity

import "testing"

func TestParseOpsRoleBothVocabularies(t *testing.T) {
	for _, c := range []struct {
		in    string
		scope OpsScope
		admin bool
		ok    bool
	}{
		{"none", OpsNone, false, true},
		{"readonly", OpsView, false, true},
		{"view", OpsView, false, true},
		{"client", OpsRun, false, true},
		{"run", OpsRun, false, true},
		{" client ", "", false, false},
		{" none ", "", false, false},
		{"run\n", "", false, false},
		{"admin", OpsNone, true, true},
		{"", "", false, false},
		{"supreme", "", false, false},
		{"Admin", "", false, false},
	} {
		scope, admin, ok := ParseOpsRole(c.in)
		if scope != c.scope || admin != c.admin || ok != c.ok {
			t.Errorf("ParseOpsRole(%q) = %q %v %v; want %q %v %v", c.in, scope, admin, ok, c.scope, c.admin, c.ok)
		}
	}
}

func TestLegacyOpsRoleAndAccess(t *testing.T) {
	for _, c := range []struct {
		u      User
		legacy string
		access bool
	}{
		{User{Role: RoleAdmin, OpsScope: OpsNone, Enabled: true}, "admin", true},
		{User{Role: RoleAdmin, OpsScope: OpsView, Enabled: true}, "admin", true}, // scope ignored for an admin
		{User{Role: RoleMember, OpsScope: OpsRun, Enabled: true}, "client", true},
		{User{Role: RoleViewer, OpsScope: OpsView, Enabled: true}, "readonly", true},
		{User{Role: RoleMember, OpsScope: OpsNone, Enabled: true}, "", false},
		{User{Role: RoleAdmin, OpsScope: OpsNone, Enabled: false}, "", false}, // disabled: no access
		{User{Role: RoleMember, OpsScope: OpsRun, Enabled: false}, "", false},
	} {
		u := c.u
		if got := u.LegacyOpsRole(); got != c.legacy {
			t.Errorf("%+v LegacyOpsRole = %q, want %q", u, got, c.legacy)
		}
		if got := u.HasOpsAccess(); got != c.access {
			t.Errorf("%+v HasOpsAccess = %v, want %v", u, got, c.access)
		}
	}
	var none *User
	if none.HasOpsAccess() {
		t.Error("a nil account has Operations Center access")
	}
}

// TestMapLegacy is the mapping table of docs/IDENTITY.md.
func TestMapLegacy(t *testing.T) {
	chat := func(r Role) *LegacyChat { return &LegacyChat{Role: r, Enabled: true} }
	for _, c := range []struct {
		name  string
		chat  *LegacyChat
		sched LegacySched
		want  Merged
	}{
		{"chat admin + sched admin", chat(RoleAdmin), LegacySched{"admin", true}, Merged{Role: RoleAdmin, OpsScope: OpsNone, Enabled: true}},
		{"chat admin + sched client", chat(RoleAdmin), LegacySched{"client", true}, Merged{Role: RoleAdmin, OpsScope: OpsNone, Enabled: true}},
		{"member + client", chat(RoleMember), LegacySched{"client", true}, Merged{Role: RoleMember, OpsScope: OpsRun, Enabled: true}},
		{"viewer + readonly", chat(RoleViewer), LegacySched{"readonly", true}, Merged{Role: RoleViewer, OpsScope: OpsView, Enabled: true}},
		{"member + disabled client", chat(RoleMember), LegacySched{"client", false}, Merged{Role: RoleMember, OpsScope: OpsNone, Enabled: true}},
		{"split admin", chat(RoleMember), LegacySched{"admin", true}, Merged{Role: RoleMember, OpsScope: OpsRun, Enabled: true, SplitAdmin: true}},
		{"split admin, disabled sched", chat(RoleMember), LegacySched{"admin", false}, Merged{Role: RoleMember, OpsScope: OpsNone, Enabled: true}},
		{"sched only", nil, LegacySched{"readonly", true}, Merged{Role: RoleViewer, OpsScope: OpsView, Enabled: true, Created: true}},
		{"sched-only admin", nil, LegacySched{"admin", true}, Merged{Role: RoleViewer, OpsScope: OpsRun, Enabled: true, Created: true, SplitAdmin: true}},
		{"sched-only disabled", nil, LegacySched{"client", false}, Merged{Role: RoleViewer, OpsScope: OpsNone, Enabled: false, Created: true}},
		{"unknown role", chat(RoleMember), LegacySched{"superuser", true}, Merged{Role: RoleMember, OpsScope: OpsNone, Enabled: true, UnknownRole: true}},
		// A padded stored role was never a role: no access (Codex 4505570166).
		{"padded admin", chat(RoleMember), LegacySched{" admin", true}, Merged{Role: RoleMember, OpsScope: OpsNone, Enabled: true, UnknownRole: true}},
		{"padded client, sched only", nil, LegacySched{"client ", true}, Merged{Role: RoleViewer, OpsScope: OpsNone, Enabled: true, Created: true, UnknownRole: true}},
		{"re-cased readonly", chat(RoleViewer), LegacySched{"ReadOnly", true}, Merged{Role: RoleViewer, OpsScope: OpsNone, Enabled: true, UnknownRole: true}},
		{"disabled chat keeps its flag", &LegacyChat{Role: RoleMember}, LegacySched{"client", true}, Merged{Role: RoleMember, OpsScope: OpsRun}},
	} {
		if got := MapLegacy(c.chat, c.sched); got != c.want {
			t.Errorf("%s: MapLegacy = %+v, want %+v", c.name, got, c.want)
		}
	}
}
