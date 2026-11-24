package store

import "strings"

// Several accounts of one connector in one conversation.
//
// conversations.mcp_accounts maps a connector to the account it runs as in
// that conversation (migration 052). A conversation may now run one bundled
// connector as several accounts at once, each its own credentialed process
// with its own tools (docs/MULTI-ACCOUNT-CHATS.md). The column keeps its shape:
// one string per connector. A value that names several accounts is the list
// joined by mcpAccountListSep, with the empty element standing for "my
// default" (the account the conversation would use with no override):
//
//	"twc"       one named account (unchanged from before)
//	",twc"      my default and twc
//	"twc,arena" twc and arena
//
// One account is therefore stored exactly as it always was, so a conversation
// nobody gave a second account reads and writes byte-identical rows, and every
// path that only moves the map around (multiplayer participant keys, transfer,
// a user rename) needs no change. A binary from before this format reads a
// list as one unknown account name, which it already treats as stale and
// replaces with the default account, never with another client's.
//
// An account name can never contain the separator: bundled accounts are
// environment-variable suffixes folded to [a-z0-9_] and hosted labels match
// [a-z0-9_]{1,32}.
const mcpAccountListSep = ","

// MaxMCPAccountsPerConnector caps how many accounts of one connector a
// conversation can run at once. Every account is a separate connector process
// and a full copy of the connector's tools in the turn's roster, so the cap
// bounds both.
const MaxMCPAccountsPerConnector = 6

// SplitMCPAccounts decodes one conversations.mcp_accounts value into its
// accounts, in stored order, without repeats. "" in the result is the default
// account. An empty value is no override at all and returns nil.
func SplitMCPAccounts(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, mcpAccountListSep)
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// JoinMCPAccounts encodes accounts as one conversations.mcp_accounts value:
// trimmed, repeats dropped, the default account ("") first and the named ones
// in the order given. A list that is only the default account encodes to "",
// which callers store as no entry.
func JoinMCPAccounts(accounts []string) string {
	named := make([]string, 0, len(accounts))
	seen := make(map[string]bool, len(accounts))
	hasDefault := false
	for _, a := range accounts {
		a = strings.TrimSpace(a)
		if a == "" {
			hasDefault = true
			continue
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		named = append(named, a)
	}
	if len(named) == 0 {
		return ""
	}
	if hasDefault {
		return mcpAccountListSep + strings.Join(named, mcpAccountListSep)
	}
	return strings.Join(named, mcpAccountListSep)
}

// IsMCPAccountList reports whether a stored value names more than one account.
func IsMCPAccountList(value string) bool {
	return strings.Contains(value, mcpAccountListSep)
}
