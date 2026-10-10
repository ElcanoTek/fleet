package agentcore

// Approval progress (docs/APPROVAL-PROGRESS.md).
//
// An approved card's MCP call can run for minutes (APPROVED-CALL-BUDGET.md),
// and the card only said "running" until it ended.
// agent_policy.critical_tool_progress lets a bundle say "for these tools, ask
// the server for MCP progress notifications while the approved call runs, show
// them on the card, and tell the person when it finishes". This file is only
// the policy half; the chat server does the rest (internal/httpapi
// approval_progress.go).

// ApprovalProgressProblems is the preflight face of the progress set, for
// `fleet validate-config`: the critical_tool_progress members
// ConfigureAgentPolicy would drop, checked against the same merged critical
// list the gate uses (base email suffixes included).
func ApprovalProgressProblems(p AgentPolicy) []string {
	_, problems := buildStagedCardSuffixSet("critical_tool_progress", p.CriticalToolProgress, p.CriticalToolModes, mergedCriticalSuffixes(p))
	return problems
}

// ApprovalProgress reports whether an approved call of toolName asks its
// server for progress and notifies on finish: the bundle lists a matching
// suffix in critical_tool_progress (the tool name equals the suffix or ends
// with "_<suffix>", as for critical_tools).
func ApprovalProgress(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeProgress {
		if suffixMatches(toolName, suffix) {
			return true
		}
	}
	return false
}
