package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// A conversation can run one bundled connector as several accounts at once
// (docs/MULTI-ACCOUNT-CHATS.md). Each account is its own credentialed process,
// and the only thing that tells its tools apart from another account's is the
// account in the tool name (`mcp_<connector>_<account>_<tool>`). The model
// reads tool names as opaque identifiers, so without being told it has no
// reason to treat two near-identical tool sets as two different clients'
// logins. This section says which accounts are on and what that obliges.

// connectorAccounts is one connector this turn runs as more than one account.
type connectorAccounts struct {
	server string
	// accounts are in the conversation's order; "" is the default account.
	accounts []string
}

// multiAccountConnectors lists the opted-in servers of a turn that run as more
// than one account, sorted by server name so the section is byte-identical for
// the same choice (docs/PROMPT-CACHE-CONTRACT.md). nil when no connector has a
// second account, which is every conversation that never asked for one.
func multiAccountConnectors(enabled []string, defaults map[string]string, extra map[string][]string) []connectorAccounts {
	if len(extra) == 0 {
		return nil
	}
	var out []connectorAccounts
	seenServer := map[string]bool{}
	for _, name := range enabled {
		name = strings.TrimSpace(name)
		if name == "" || seenServer[name] || len(extra[name]) == 0 {
			continue
		}
		seenServer[name] = true
		choices := appendExtraAccountChoices(
			agentcore.MCPSelection{{Server: name, Account: defaults[name]}}, name, defaults[name], extra[name])
		if len(choices) < 2 {
			continue
		}
		accounts := make([]string, 0, len(choices))
		for _, c := range choices {
			accounts = append(accounts, creds.CanonicalAccount(c.Account))
		}
		out = append(out, connectorAccounts{server: name, accounts: accounts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].server < out[j].server })
	return out
}

// writeMultiAccountNotice appends the section. It writes nothing for a turn
// with no multi-account connector, so such a turn's prompt is unchanged.
func writeMultiAccountNotice(sb *strings.Builder, connectors []connectorAccounts) {
	if len(connectors) == 0 {
		return
	}
	sb.WriteString("## Connectors on for several accounts in this chat\n\n")
	sb.WriteString("The user turned these connectors on for more than one account. Each account is a separate login to the same system, with its own data and its own copy of the connector's tools. The account is part of the tool name:\n\n")
	for _, c := range connectors {
		parts := make([]string, 0, len(c.accounts))
		for _, account := range c.accounts {
			prefix := agentcore.PromptSafeName(agentcore.MCPToolName(agentcore.RegisteredMCPName(c.server, account), ""))
			if account == "" {
				parts = append(parts, fmt.Sprintf("the default account (tools `%s*` with no account in the name)", prefix))
				continue
			}
			parts = append(parts, fmt.Sprintf("account `%s` (tools `%s*`)", agentcore.PromptSafeName(account), prefix))
		}
		fmt.Fprintf(sb, "- `%s`: %s\n", agentcore.PromptSafeName(c.server), strings.Join(parts, "; "))
	}
	sb.WriteString("\nRules while more than one account is on:\n")
	sb.WriteString("- A call acts on exactly one account: the one in the tool's name. Ids, names and lists from one account mean nothing in another; never pass them across.\n")
	sb.WriteString("- Say which account in every answer that reports data and before every action, so the user never has to guess which account a number or a change belongs to.\n")
	sb.WriteString("- If a request does not say which account or accounts it is for and the conversation does not make it plain, ask before acting. Do not pick one, and do not run it on every account unless the user asked for all of them.\n")
	sb.WriteString("- A change for several accounts is one call per account, each with its own approval. Never describe a call as covering more than its own account.\n\n")
}
