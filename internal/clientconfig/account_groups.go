package clientconfig

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Account groups: named sets of connector accounts a bundle offers in the chat
// Tools picker (docs/ACCOUNT-GROUPS.md).
//
// Many deployments hold several accounts on the same connectors: one per
// client, per brand, per region. Picking "the same client everywhere" by hand
// means opening every connector's account menu. A group names that set once:
// which account of which connector belong together. The picker shows one chip
// per group, and one click sets every connector in the group to the group's
// accounts.
//
// fleet knows nothing about what a group IS. The manifest supplies a heading
// ("Clients", "Brands", …) and the groups; fleet checks the shape here and, at
// request time, which members exist on this box.

// AccountGroupsConfig is the manifest's account_groups block.
type AccountGroupsConfig struct {
	// Label is the heading over the chips, in the bundle's words. Empty means
	// fleet's generic "Account groups".
	Label string `yaml:"label"`
	// Groups are the bundle's fixed groups, in display order.
	Groups []AccountGroup `yaml:"groups"`
	// Source names a tool of the bundle that lists more groups at run time,
	// for a deployment whose groups change without a bundle release (clients
	// kept in another system). nil = the fixed groups only.
	Source *AccountGroupsSource `yaml:"source"`
}

// AccountGroupsSource is a bundle tool fleet calls, outside any chat turn, to
// read account groups (ADR-0119). The tool takes no arguments and answers
// {"groups": [{"id", "name", "accounts": [{"server", "account"}]}]}.
type AccountGroupsSource struct {
	// Server is the mcp_servers entry that exports the tool. It is called on
	// its default account.
	Server string `yaml:"server"`
	// Tool is the tool's own name on that server (not the mcp_<server>_ form).
	// It must be a declared read: listed in agent_policy.parallel_safe_tools
	// and not critical.
	Tool string `yaml:"tool"`
	// RefreshSeconds is how long an answer is used before the tool is asked
	// again. 0 means DefaultAccountGroupsRefreshSeconds.
	RefreshSeconds int `yaml:"refresh_seconds"`
}

const (
	// DefaultAccountGroupsRefreshSeconds is how long a source's answer is
	// served when the manifest does not say.
	DefaultAccountGroupsRefreshSeconds = 300
	// MinAccountGroupsRefreshSeconds keeps a typo from turning every picker
	// open into a call to the source.
	MinAccountGroupsRefreshSeconds = 60
	// MaxAccountGroupsRefreshSeconds is a day: longer than that is a fixed
	// list, which belongs in groups.
	MaxAccountGroupsRefreshSeconds = 86400
)

var accountGroupToolRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// AccountGroup is one named set of connector accounts.
type AccountGroup struct {
	// ID is the group's stable identifier. Optional in the manifest: when
	// empty it is derived from Name.
	ID string `yaml:"id" json:"id"`
	// Name is what the chip reads.
	Name string `yaml:"name" json:"name"`
	// Accounts are the group's members. One connector may appear more than
	// once (a client with a USD and a GBP seat on the same exchange).
	Accounts []AccountGroupMember `yaml:"accounts" json:"accounts"`
}

// AccountGroupMember is one {connector, account} pair. Account "" is the
// connector's default account.
type AccountGroupMember struct {
	Server  string `yaml:"server" json:"server"`
	Account string `yaml:"account" json:"account"`
}

const (
	// MaxAccountGroups bounds the chip row: a picker with more groups than
	// this needs search, not chips.
	MaxAccountGroups = 500
	// MaxAccountGroupNameRunes bounds a chip's label.
	MaxAccountGroupNameRunes = 80
	// MaxAccountGroupMembers bounds one group's members.
	MaxAccountGroupMembers = 200
	// MaxAccountGroupMembersPerServer mirrors the per-conversation cap on
	// accounts of one connector (store.MaxMCPAccountsPerConnector): a group
	// that names more could never be turned on whole.
	MaxAccountGroupMembersPerServer = 6
	maxAccountGroupLabelRunes       = 40
)

var (
	accountGroupIDRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	accountGroupAccountRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	accountGroupSlugRun   = regexp.MustCompile(`[^a-z0-9]+`)
)

// AccountGroupID derives a group id from its name: lowercased, runs of
// anything but letters and digits folded to one hyphen. "" when the name has
// no letter or digit to build one from.
func AccountGroupID(name string) string {
	id := strings.Trim(accountGroupSlugRun.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(id) > 64 {
		id = strings.TrimRight(id[:64], "-")
	}
	return id
}

// CanonicalAccountGroupAccount folds an account name the way the credential
// catalog lists it (creds.AccountsFor): trimmed, lowercased, hyphens and
// spaces to underscores. "" stays "" (the default account).
func CanonicalAccountGroupAccount(account string) string {
	return strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(account)))
}

// NormalizeAccountGroups checks a list of groups and returns it in canonical
// form: names trimmed, ids filled in, accounts folded, repeated members
// dropped. knownServer reports whether a connector name is one the bundle
// declares; nil skips that check (a caller with no catalog to check against).
// where prefixes every error ("account_groups.groups").
//
// It is the one validator for groups from any origin, so a group that passes
// the manifest check and one that arrives later from another source obey the
// same rules.
func NormalizeAccountGroups(where string, groups []AccountGroup, knownServer func(string) bool) ([]AccountGroup, error) {
	if len(groups) > MaxAccountGroups {
		return nil, fmt.Errorf("%s: %d groups; at most %d are supported", where, len(groups), MaxAccountGroups)
	}
	out := make([]AccountGroup, 0, len(groups))
	seenID := make(map[string]bool, len(groups))
	for i, g := range groups {
		at := fmt.Sprintf("%s[%d]", where, i)
		name := strings.TrimSpace(g.Name)
		if name == "" {
			return nil, fmt.Errorf("%s: name is required", at)
		}
		at = fmt.Sprintf("%s[%q]", where, name)
		if utf8.RuneCountInString(name) > MaxAccountGroupNameRunes {
			return nil, fmt.Errorf("%s: name is longer than %d characters", at, MaxAccountGroupNameRunes)
		}
		if strings.ContainsAny(name, "\r\n\t") {
			return nil, fmt.Errorf("%s: name must be a single line", at)
		}
		id := strings.TrimSpace(g.ID)
		if id == "" {
			id = AccountGroupID(name)
		}
		if !accountGroupIDRE.MatchString(id) {
			return nil, fmt.Errorf("%s: id %q must be lowercase letters, digits, hyphens or underscores (at most 64), starting with a letter or digit; set id: explicitly when the name has none", at, id)
		}
		if seenID[id] {
			return nil, fmt.Errorf("%s: duplicate group id %q", at, id)
		}
		seenID[id] = true
		if len(g.Accounts) == 0 {
			return nil, fmt.Errorf("%s: accounts is empty; a group names at least one connector account", at)
		}
		if len(g.Accounts) > MaxAccountGroupMembers {
			return nil, fmt.Errorf("%s: %d accounts; at most %d are supported", at, len(g.Accounts), MaxAccountGroupMembers)
		}
		members := make([]AccountGroupMember, 0, len(g.Accounts))
		seenMember := make(map[AccountGroupMember]bool, len(g.Accounts))
		perServer := map[string]int{}
		for _, m := range g.Accounts {
			server := strings.TrimSpace(m.Server)
			if server == "" {
				return nil, fmt.Errorf("%s: an account entry has no server", at)
			}
			if knownServer != nil && !knownServer(server) {
				return nil, fmt.Errorf("%s: server %q is not an optional entry in mcp_servers", at, server)
			}
			account := CanonicalAccountGroupAccount(m.Account)
			if account != "" && !accountGroupAccountRE.MatchString(account) {
				return nil, fmt.Errorf("%s: account %q of server %q must be letters, digits or underscores (at most 64)", at, m.Account, server)
			}
			member := AccountGroupMember{Server: server, Account: account}
			if seenMember[member] {
				continue
			}
			seenMember[member] = true
			perServer[server]++
			if perServer[server] > MaxAccountGroupMembersPerServer {
				return nil, fmt.Errorf("%s: more than %d accounts of server %q; a chat runs a connector as at most that many", at, MaxAccountGroupMembersPerServer, server)
			}
			members = append(members, member)
		}
		out = append(out, AccountGroup{ID: id, Name: name, Accounts: members})
	}
	return out, nil
}

// validateAccountGroups checks the manifest block (as loaded into
// b.accountGroups) against the manifest's own mcp_servers and replaces it with
// the canonical form. A member must name an OPTIONAL server: the picker can
// only turn on, and set the account of, a connector that has a switch. It
// runs with the rest of validate, before Agent Plugin servers join the
// catalog, so a group names manifest servers only.
func (b *Bundle) validateAccountGroups() error {
	cfg := b.accountGroups
	label := strings.TrimSpace(cfg.Label)
	if utf8.RuneCountInString(label) > maxAccountGroupLabelRunes {
		return fmt.Errorf("account_groups.label is longer than %d characters", maxAccountGroupLabelRunes)
	}
	if strings.ContainsAny(label, "\r\n\t") {
		return fmt.Errorf("account_groups.label must be a single line")
	}
	optional := make(map[string]bool, len(b.MCPCatalog))
	for i := range b.MCPCatalog {
		if b.MCPCatalog[i].Optional {
			optional[b.MCPCatalog[i].Name] = true
		}
	}
	groups, err := NormalizeAccountGroups("account_groups.groups", cfg.Groups, func(server string) bool { return optional[server] })
	if err != nil {
		return err
	}
	source, err := b.validateAccountGroupsSource(cfg.Source)
	if err != nil {
		return err
	}
	b.accountGroups = AccountGroupsConfig{Label: label, Groups: groups, Source: source}
	return nil
}

// validateAccountGroupsSource checks the source's shape against the manifest:
// the server exists and, when it lists its tools, lists this one. Whether the
// tool is a declared read is agent_policy's to say, so that is checked where
// the policy is (agentcore.AccountGroupsSourceProblems: boot, validate-config
// and every call).
func (b *Bundle) validateAccountGroupsSource(src *AccountGroupsSource) (*AccountGroupsSource, error) {
	if src == nil {
		return nil, nil
	}
	out := AccountGroupsSource{
		Server:         strings.TrimSpace(src.Server),
		Tool:           strings.TrimSpace(src.Tool),
		RefreshSeconds: src.RefreshSeconds,
	}
	if out.Server == "" || out.Tool == "" {
		return nil, fmt.Errorf("account_groups.source: server and tool are both required")
	}
	if !accountGroupToolRE.MatchString(out.Tool) {
		return nil, fmt.Errorf("account_groups.source: tool %q is not a tool name (give the tool's own name, without the mcp_<server>_ prefix)", out.Tool)
	}
	var server *ServerDef
	for i := range b.MCPCatalog {
		if b.MCPCatalog[i].Name == out.Server {
			server = &b.MCPCatalog[i]
			break
		}
	}
	if server == nil {
		return nil, fmt.Errorf("account_groups.source: server %q is not an entry in mcp_servers", out.Server)
	}
	if len(server.Tools) > 0 && !slices.Contains(server.Tools, out.Tool) {
		return nil, fmt.Errorf("account_groups.source: tool %q is not in server %q's tools allowlist", out.Tool, out.Server)
	}
	switch {
	case out.RefreshSeconds == 0:
		out.RefreshSeconds = DefaultAccountGroupsRefreshSeconds
	case out.RefreshSeconds < MinAccountGroupsRefreshSeconds || out.RefreshSeconds > MaxAccountGroupsRefreshSeconds:
		return nil, fmt.Errorf("account_groups.source: refresh_seconds %d is out of range (%d to %d)", out.RefreshSeconds, MinAccountGroupsRefreshSeconds, MaxAccountGroupsRefreshSeconds)
	}
	return &out, nil
}

// AccountGroups returns the bundle's account groups: the heading, the fixed
// groups and the source, validated at load. The zero value when the bundle declares
// none. Names only; nothing here is a credential.
func (b *Bundle) AccountGroups() AccountGroupsConfig {
	if b == nil {
		return AccountGroupsConfig{}
	}
	out := AccountGroupsConfig{Label: b.accountGroups.Label}
	if src := b.accountGroups.Source; src != nil {
		cp := *src
		out.Source = &cp
	}
	out.Groups = make([]AccountGroup, len(b.accountGroups.Groups))
	for i, g := range b.accountGroups.Groups {
		g.Accounts = append([]AccountGroupMember(nil), g.Accounts...)
		out.Groups[i] = g
	}
	return out
}
