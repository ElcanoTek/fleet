package clientconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeAccountGroups(t *testing.T) {
	t.Parallel()
	known := func(server string) bool { return server == "ix" || server == "ox" }

	got, err := NormalizeAccountGroups("g", []AccountGroup{
		{Name: "  The Weather Company ", Accounts: []AccountGroupMember{
			{Server: "ix", Account: "TWC"},
			{Server: "ix", Account: "twc"}, // the same account again
			{Server: "ox", Account: "twc-usd"},
		}},
		{ID: "house", Name: "Elcano", Accounts: []AccountGroupMember{{Server: "ix"}}},
	}, known)
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountGroup{
		{ID: "the-weather-company", Name: "The Weather Company", Accounts: []AccountGroupMember{
			{Server: "ix", Account: "twc"},
			{Server: "ox", Account: "twc_usd"},
		}},
		{ID: "house", Name: "Elcano", Accounts: []AccountGroupMember{{Server: "ix"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}

	seven := make([]AccountGroupMember, 0, 7)
	for _, a := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		seven = append(seven, AccountGroupMember{Server: "ix", Account: a})
	}
	refused := []struct {
		name   string
		groups []AccountGroup
		want   string
	}{
		{"no name", []AccountGroup{{Accounts: []AccountGroupMember{{Server: "ix"}}}}, "name is required"},
		{"no members", []AccountGroup{{Name: "A"}}, "accounts is empty"},
		{"unknown server", []AccountGroup{{Name: "A", Accounts: []AccountGroupMember{{Server: "nope"}}}}, `server "nope" is not an optional entry`},
		{"member without a server", []AccountGroup{{Name: "A", Accounts: []AccountGroupMember{{Account: "x"}}}}, "has no server"},
		{"bad account", []AccountGroup{{Name: "A", Accounts: []AccountGroupMember{{Server: "ix", Account: "a,b"}}}}, `account "a,b"`},
		{"duplicate id", []AccountGroup{
			{Name: "A B", Accounts: []AccountGroupMember{{Server: "ix"}}},
			{Name: "a-b", Accounts: []AccountGroupMember{{Server: "ox"}}},
		}, `duplicate group id "a-b"`},
		{"a name with no id in it", []AccountGroup{{Name: "★", Accounts: []AccountGroupMember{{Server: "ix"}}}}, "set id: explicitly"},
		{"a bad explicit id", []AccountGroup{{ID: "Has Space", Name: "A", Accounts: []AccountGroupMember{{Server: "ix"}}}}, "must be lowercase"},
		{"a name on two lines", []AccountGroup{{Name: "A\nB", Accounts: []AccountGroupMember{{Server: "ix"}}}}, "single line"},
		{"a long name", []AccountGroup{{Name: strings.Repeat("n", MaxAccountGroupNameRunes+1), Accounts: []AccountGroupMember{{Server: "ix"}}}}, "longer than"},
		{"too many accounts of one server", []AccountGroup{{Name: "A", Accounts: seven}}, "more than 6 accounts"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeAccountGroups("g", tc.groups, known); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// With no catalog to check against, the server name is taken as given.
	if _, err := NormalizeAccountGroups("g", []AccountGroup{{Name: "A", Accounts: []AccountGroupMember{{Server: "anything"}}}}, nil); err != nil {
		t.Fatalf("nil knownServer refused a server: %v", err)
	}
}

func writeAccountGroupsBundle(t *testing.T, block string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: ix
    display_name: Index Exchange
    description: Deals.
    command: /bin/true
    optional: true
    always: true
    account_vars: [IX_KEY]
  - name: mail
    display_name: Mail
    description: Always on.
    command: /bin/true
    always: true
` + block
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadAccountGroups(t *testing.T) {
	t.Parallel()

	b, err := Load(writeAccountGroupsBundle(t, `
account_groups:
  label: Clients
  groups:
    - name: The Weather Company
      accounts:
        - {server: ix, account: twc}
    - id: house
      name: House
      accounts:
        - {server: ix}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := b.AccountGroups()
	want := AccountGroupsConfig{Label: "Clients", Groups: []AccountGroup{
		{ID: "the-weather-company", Name: "The Weather Company", Accounts: []AccountGroupMember{{Server: "ix", Account: "twc"}}},
		{ID: "house", Name: "House", Accounts: []AccountGroupMember{{Server: "ix"}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountGroups() = %#v, want %#v", got, want)
	}
	// The accessor hands out a copy.
	got.Groups[0].Accounts[0].Account = "changed"
	if b.AccountGroups().Groups[0].Accounts[0].Account != "twc" {
		t.Fatal("AccountGroups() exposed the bundle's own slice")
	}

	// No block: nothing, and a nil bundle is safe.
	b, err = Load(writeAccountGroupsBundle(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.AccountGroups(); got.Label != "" || len(got.Groups) != 0 {
		t.Fatalf("a bundle with no block has groups: %#v", got)
	}
	if got := (*Bundle)(nil).AccountGroups(); len(got.Groups) != 0 {
		t.Fatalf("nil bundle has groups: %#v", got)
	}
}

func TestLoadAccountGroupsRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ block, want string }{
		"a connector with no switch": {`
account_groups:
  groups:
    - name: A
      accounts: [{server: mail}]
`, `server "mail" is not an optional entry`},
		"an unknown key": {`
account_groups:
  groups:
    - name: A
      servers: [ix]
`, "servers"},
		"a long label": {`
account_groups:
  label: ` + strings.Repeat("l", 41) + `
`, "account_groups.label"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeAccountGroupsBundle(t, tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
