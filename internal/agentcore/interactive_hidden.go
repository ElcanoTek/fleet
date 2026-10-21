package agentcore

import (
	"fmt"
	"log"
	"strings"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Interactive-hidden tools (docs/INTERACTIVE-HIDDEN-TOOLS.md, ADR-0085).
//
// A bundle's per-server `tools:` allowlist (Gate-2) applies to every run, chat
// and scheduled alike. agent_policy.interactive_hidden_tools lets a bundle keep
// an MCP tool out of interactive chat only: a raw write a scheduled protocol
// needs, but that chat must reach through a reviewed flow (one plan, one
// approval card) instead of one card per call.
//
// The list is matched by suffix exactly like critical_tools ("create_deal"
// selects "mcp_<server>_create_deal"), against the model-facing
// mcp_<server>_<tool> name. It is a NARROWING, like the persona policy
// (Gate-4): it can only subtract from what the earlier gates registered, never
// add. The interactive driver hands it to Run as RunConfig.HiddenToolSuffixes
// (driver data, not a Mode branch), and a sub-agent spawned from a chat turn
// inherits it, so the child cannot reach what its parent could not. Scheduled
// and Operations Center runs never set it, so they are unchanged.
//
// Enforcement points, all reading the same suffix list:
//
//  1. Registration — buildFantasyToolsWithRoster drops a hidden tool BEFORE
//     the disclosure decision, so it is neither in the direct tool list nor in
//     the deferred registry behind tool_search / tool_describe / tool_call.
//  2. The call gate — every mcpTool built for a run carries the list and
//     refuses a hidden name before the policy gate (no approval card is
//     staged, nothing is journaled, the broker is never called).
//  3. Explanations — a direct call to a hidden name (out-of-roster, #1744) and
//     a tool_call naming one both answer with interactiveHiddenRefusal rather
//     than "tool not found", so the model switches to the flow its
//     instructions describe instead of hunting for the tool.
//  4. The approval-card describer (ADR-0082) is not called when it is hidden:
//     it runs on the chat turn's scope and must not reach what the model
//     could not.

// gateInteractiveHidden is the toolRoster.removed gate for a tool this list
// kept out of an interactive run.
const gateInteractiveHidden = "interactive_hidden"

// buildInteractiveHiddenList normalizes interactive_hidden_tools: trimmed,
// de-duplicated, in declaration order. A blank member, or one with a character
// no MCP tool name can contain (so it could never match anything), is dropped
// and reported.
func buildInteractiveHiddenList(members []string) ([]string, []string) {
	out := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	var problems []string
	for _, raw := range members {
		s := strings.TrimSpace(raw)
		switch {
		case s == "":
			problems = append(problems, "ignoring a blank interactive_hidden_tools member")
			continue
		case !validToolNameSuffix(s):
			problems = append(problems, fmt.Sprintf("ignoring interactive_hidden_tools member %q: a tool name holds only ASCII letters, digits, '_', '-' and '.', so it can never match a tool", s))
			continue
		case seen[s]:
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, problems
}

// installInteractiveHiddenList is buildInteractiveHiddenList for
// ConfigureAgentPolicy: every dropped member is one boot log line.
func installInteractiveHiddenList(members []string) []string {
	list, problems := buildInteractiveHiddenList(members)
	for _, problem := range problems {
		log.Printf("agent_policy: %s", problem)
	}
	return list
}

// validToolNameSuffix reports whether s uses only the MCP spec's tool-name
// characters (ASCII letters, digits, '_', '-', '.').
func validToolNameSuffix(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return s != ""
}

// InteractiveHiddenToolProblems is the preflight face of
// buildInteractiveHiddenList, for `fleet validate-config`: the members
// ConfigureAgentPolicy would drop. Checks that need the bundle's own tool
// lists (a member that matches nothing) live in cmd/fleet.
func InteractiveHiddenToolProblems(p AgentPolicy) []string {
	_, problems := buildInteractiveHiddenList(p.InteractiveHiddenTools)
	return problems
}

// InteractiveHiddenTools returns a copy of the installed list, for the
// interactive driver to put on each turn's RunConfig. nil when the bundle
// declares none, so a turn's config is unchanged.
func InteractiveHiddenTools() []string {
	policyMu.RLock()
	defer policyMu.RUnlock()
	if len(activeInteractiveHidden) == 0 {
		return nil
	}
	return append([]string(nil), activeInteractiveHidden...)
}

// InteractiveHiddenTool reports whether the installed list hides toolName
// from interactive chat. For a host-side caller that dispatches a tool for a
// chat turn outside the loop (the approval-card describer).
func InteractiveHiddenTool(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return hiddenByList(toolName, activeInteractiveHidden)
}

// InteractiveHiddenIn reports whether list (a snapshot from
// InteractiveHiddenTools) hides toolName, under the same rule the run gate
// applies. For a host-side caller that checks many names against one
// snapshot (the chat Tools picker).
func InteractiveHiddenIn(list []string, toolName string) bool {
	return hiddenByList(toolName, list)
}

// MCPToolName is the model-facing name a connector's tool registers under
// in a run (mcp_<server>_<tool>), the name interactive_hidden_tools and
// critical_tools suffixes are matched against.
func MCPToolName(server, tool string) string {
	return mcpToolName(mcp.ServerTool{ServerName: server, Tool: mcp.Tool{Name: tool}})
}

// hiddenByList reports whether any suffix in list selects name, under the
// critical_tools suffix rule. An empty list hides nothing.
func hiddenByList(name string, list []string) bool {
	for _, s := range list {
		if s != "" && suffixMatches(name, s) {
			return true
		}
	}
	return false
}

// interactiveHiddenRefusal is the tool result a call to a hidden tool gets,
// from every enforcement point, so the model reads one explanation.
func interactiveHiddenRefusal(name string) string {
	return fmt.Sprintf("%s is not available in interactive chat and was not called: this deployment reserves it for scheduled runs (agent_policy.interactive_hidden_tools). Do not look for another way to call it; use the workflow your instructions give for this action in chat.", name)
}
