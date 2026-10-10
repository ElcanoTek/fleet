package agentcore

import (
	"fmt"
	"strings"
)

// Grouped approvals (docs/GROUPED-APPROVALS.md).
//
// A turn that stages several critical calls (one per external system, say)
// used to put several separate approval cards in front of the person, each
// with its own buttons and countdown. agent_policy.critical_tool_group_approval
// lets a bundle say "for these tools, the cards one turn stages are one
// decision": the chat server stamps each such card with the turn's group id,
// and the web renders two or more of them as one card with a checkbox per
// call. This file is only the policy half: which tools opted in. Every call
// still resolves through its own approval row, claim and execution, exactly
// as an individual card does (internal/httpapi approval_group.go).

// buildStagedCardSuffixSet resolves an agent_policy list whose members only
// mean something for a tool that stages an approval card. A member that is not
// a critical suffix is dropped and reported (no card is ever staged for such a
// tool, so the entry would do nothing, and it is most likely a typo), and so is
// one whose critical_tool_modes entry is notify (it runs without a card). key
// names the list in the problem text.
func buildStagedCardSuffixSet(key string, members []string, modes map[string]string, critical map[string]bool) (map[string]bool, []string) {
	set := make(map[string]bool, len(members))
	var problems []string
	notify := map[string]bool{}
	for k, v := range modes {
		if strings.EqualFold(strings.TrimSpace(v), ApprovalModeNotify) {
			notify[strings.TrimSpace(k)] = true
		}
	}
	for _, s := range members {
		s = strings.TrimSpace(s)
		if s == "" || set[s] {
			continue
		}
		switch {
		case !critical[s]:
			problems = append(problems, fmt.Sprintf("ignoring %s member %q: it is not in critical_tools, so no approval card is ever staged for it and the entry does nothing", key, s))
			continue
		case notify[s] && !nonReversibleSuffixes[s]:
			problems = append(problems, fmt.Sprintf("ignoring %s member %q: its critical_tool_modes entry is notify, so it runs without an approval card", key, s))
			continue
		}
		set[s] = true
	}
	return set, problems
}

// mergedCriticalSuffixes is the critical set the gate uses: the base email
// suffixes plus the bundle's critical_tools. For the preflight checks, which
// run without installing the policy.
func mergedCriticalSuffixes(p AgentPolicy) map[string]bool {
	critical := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		critical[s] = true
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical[s] = true
		}
	}
	return critical
}

// GroupApprovalProblems is the preflight face of the group-approval set, for
// `fleet validate-config`: the critical_tool_group_approval members
// ConfigureAgentPolicy would drop, checked against the same merged critical
// list the gate uses (base email suffixes included).
func GroupApprovalProblems(p AgentPolicy) []string {
	_, problems := buildStagedCardSuffixSet("critical_tool_group_approval", p.CriticalToolGroupApproval, p.CriticalToolModes, mergedCriticalSuffixes(p))
	return problems
}

// GroupsApproval reports whether an approval card for toolName joins its
// turn's approval group: the bundle lists a matching suffix in
// critical_tool_group_approval (the tool name equals the suffix or ends with
// "_<suffix>", as for critical_tools).
func GroupsApproval(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeGroupApproval {
		if suffixMatches(toolName, suffix) {
			return true
		}
	}
	return false
}
