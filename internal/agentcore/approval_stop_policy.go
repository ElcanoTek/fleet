package agentcore

import (
	"fmt"
	"log"
	"strings"
)

// Stopping a running approved call (docs/APPROVAL-STOP.md).
//
// An approved call can run for minutes (a plan booking dozens of records).
// agent_policy.critical_tool_stop lets a bundle say "for these tools, the
// person may press Stop while the approved call runs": the chat server sends
// the MCP cancellation for that request and keeps waiting for the server's
// answer, which a cooperating server gives after finishing its current item.
// This file is only the policy half; the chat server does the rest (internal/
// httpapi approval_stop.go).

// ApprovalStopProblems is the preflight face of the stop set, for `fleet
// validate-config`: the critical_tool_stop members ConfigureAgentPolicy would
// drop, checked against the same merged critical list the gate uses (base
// email suffixes included).
func ApprovalStopProblems(p AgentPolicy) []string {
	_, problems := buildStopSet(p.CriticalToolStop, p.CriticalToolModes, mergedCriticalSuffixes(p))
	return problems
}

// buildStopSet resolves critical_tool_stop: buildStagedCardSuffixSet's rules,
// plus a member naming a native executable tool (bash) is dropped and
// reported: it runs inline, never as an MCP request, so there is nothing to
// cancel and its running card could never offer Stop.
func buildStopSet(members []string, modes map[string]string, critical map[string]bool) (map[string]bool, []string) {
	set, problems := buildStagedCardSuffixSet("critical_tool_stop", members, modes, critical)
	for _, s := range members {
		s = strings.TrimSpace(s)
		if set[s] && nativeExecutableTools[s] {
			delete(set, s)
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_stop member %q: it selects fleet's native %s tool, which runs no MCP call and cannot be stopped (name an MCP tool by its server, e.g. <server>_%s)", s, s, s))
		}
	}
	return set, problems
}

// installStopSet is buildStopSet for the boot install: each dropped member is
// one log line.
func installStopSet(members []string, modes map[string]string, critical map[string]bool) map[string]bool {
	set, problems := buildStopSet(members, modes, critical)
	for _, problem := range problems {
		log.Printf("agent_policy: %s", problem)
	}
	return set
}

// ApprovalStop reports whether a running approved call of toolName offers
// Stop: the bundle lists a matching suffix in critical_tool_stop (the tool
// name equals the suffix or ends with "_<suffix>", as for critical_tools).
func ApprovalStop(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeStop {
		if suffixMatches(toolName, suffix) {
			return true
		}
	}
	return false
}
