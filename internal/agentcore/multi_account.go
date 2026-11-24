package agentcore

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// A chat can run one connector as several accounts at once
// (docs/MULTI-ACCOUNT-CHATS.md). Each account is its own credentialed process,
// and the only thing that tells its tools apart from another account's is the
// account in the tool name (`mcp_<connector>_<account>_<tool>`). The model
// reads tool names as opaque identifiers, so without being told it has no
// reason to treat two near-identical tool sets as two different clients'
// logins. This section says which accounts are on and what that obliges.
//
// It is rendered here, from the roster buildFantasyTools actually registered,
// for the same reason the live registry is (live_registry.go): only the roster
// knows what survived. An account is a best-effort server that can fail to
// connect while its siblings come up, and a connected one can still lose every
// tool to the allowlist, the persona policy, the schema check or
// interactive_hidden_tools. Written from the request, the section offered a
// tool prefix the registry beside it did not have.
//
// Prompt-cache contract (docs/PROMPT-CACHE-CONTRACT.md): a pure function of
// the selection and the roster. Connectors are sorted, accounts keep the
// selection's order, and there are no counters or timestamps.

// multiAccountHeading is referenced by the docs; keep it stable.
const multiAccountHeading = "## Connectors on for several accounts in this chat"

// multiAccountConnector is one server the selection names more than once.
type multiAccountConnector struct {
	server string
	// accounts are canonical account names in selection order; "" is the
	// default account.
	accounts []string
}

// multiAccountConnectors lists the servers a selection runs as more than one
// account, sorted by server name. nil when every server has one account, which
// is every run that never asked for a second.
func multiAccountConnectors(selection MCPSelection) []multiAccountConnector {
	byServer := map[string][]string{}
	seen := map[string]bool{}
	for _, c := range selection {
		if c.Server == "" {
			continue
		}
		account := creds.CanonicalAccount(c.Account)
		key := RegisteredMCPName(c.Server, account)
		if seen[key] {
			continue
		}
		seen[key] = true
		byServer[c.Server] = append(byServer[c.Server], account)
	}
	var out []multiAccountConnector
	for server, accounts := range byServer {
		if len(accounts) > 1 {
			out = append(out, multiAccountConnector{server: server, accounts: accounts})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].server < out[j].server })
	return out
}

// multiAccountSection renders the section for this selection and roster, or ""
// when no server runs as more than one account. An account with no tool in the
// roster is named as not available and offered no tool prefix.
func (r toolRoster) multiAccountSection(selection MCPSelection) string {
	connectors := multiAccountConnectors(selection)
	if len(connectors) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(multiAccountHeading)
	sb.WriteString("\n\n")
	sb.WriteString("The user turned these connectors on for more than one account. Each account is a separate login to the same system, with its own data and its own copy of the connector's tools. The account is part of the tool name:\n\n")
	anyMissing := false
	for _, c := range connectors {
		parts := make([]string, 0, len(c.accounts))
		for _, account := range c.accounts {
			label := "the default account"
			if account != "" {
				label = fmt.Sprintf("account `%s`", PromptSafeName(account))
			}
			registered := RegisteredMCPName(c.server, account)
			if r.toolsByServer[registered] == 0 {
				anyMissing = true
				parts = append(parts, label+" (NOT available this turn: no tools)")
				continue
			}
			prefix := PromptSafeName(MCPToolName(registered, ""))
			if account == "" {
				parts = append(parts, fmt.Sprintf("%s (tools `%s*` with no account in the name)", label, prefix))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s (tools `%s*`)", label, prefix))
		}
		fmt.Fprintf(&sb, "- `%s`: %s\n", PromptSafeName(c.server), strings.Join(parts, "; "))
	}
	sb.WriteString("\nRules while more than one account is on:\n")
	sb.WriteString("- A call acts on exactly one account: the one in the tool's name. Ids, names and lists from one account mean nothing in another; never pass them across.\n")
	sb.WriteString("- Say which account in every answer that reports data and before every action, so the user never has to guess which account a number or a change belongs to.\n")
	sb.WriteString("- If a request does not say which account or accounts it is for and the conversation does not make it plain, ask before acting. Do not pick one, and do not run it on every account unless the user asked for all of them.\n")
	sb.WriteString("- A change for several accounts is one call per account, each with its own approval. Never describe a call as covering more than its own account.\n")
	if anyMissing {
		sb.WriteString("- An account marked NOT available has no tools this turn (it could not be connected, or its tools are not offered in chat). If the user asks for it, say it is unavailable. Never do its work on another account.\n")
	}
	return sb.String()
}

// withRosterSections appends to the driver's system prompt what only the
// built roster can say: the multi-account section, when the driver asked for
// it and the selection has one, then the live registry, which stays last. The
// join is deterministic, so it is safe inside the cached prefix. Without a
// multi-account section the result is exactly withLiveRegistry's.
func withRosterSections(systemPrompt string, r toolRoster, cfg RunConfig) string {
	if cfg.MultiAccountNotice {
		if section := r.multiAccountSection(cfg.Selection); section != "" {
			base := strings.TrimRight(systemPrompt, "\n")
			if base == "" {
				systemPrompt = section
			} else {
				systemPrompt = base + "\n\n" + section
			}
		}
	}
	return withLiveRegistry(systemPrompt, r)
}
