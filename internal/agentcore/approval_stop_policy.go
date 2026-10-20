package agentcore

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
	_, problems := buildStagedCardSuffixSet("critical_tool_stop", p.CriticalToolStop, p.CriticalToolModes, mergedCriticalSuffixes(p))
	return problems
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
