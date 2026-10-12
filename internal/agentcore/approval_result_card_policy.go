package agentcore

import (
	"fmt"
	"log"
	"strings"
)

// Approval result cards (docs/APPROVAL-RESULT-CARDS.md, ADR-0084).
//
// A settled approval card said "Applied · <title>" above the tool's raw
// result. agent_policy.critical_tool_result_cards lets a bundle say "for
// these tools, the result itself carries a structured outcome card": the
// approved call's own answer, checked against a fixed schema, renders as the
// completed card (one-line outcome, a row per record with its ids, status
// and link, a retry action). No second call is made: the card is read from
// the result the approved call already returned. This file is only the
// policy half; the chat server does the rest (internal/httpapi
// approval_result_card.go).

// buildResultCardSet resolves critical_tool_result_cards. A member must be a
// critical suffix (no other tool ever settles a card) and must not name a
// handler-only card (fleet's own handler resolves it; there is no tool
// result to read). Notify mode is allowed: a notify record is settled by the
// call's result too, after the call runs.
func buildResultCardSet(members []string, critical map[string]bool) (map[string]bool, []string) {
	set := make(map[string]bool, len(members))
	var problems []string
	for _, s := range members {
		s = strings.TrimSpace(s)
		if s == "" || set[s] {
			continue
		}
		switch {
		case !critical[s]:
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_result_cards member %q: it is not in critical_tools, so no approval card is ever settled by its result and the entry does nothing", s))
			continue
		case handlerOnlyCardTools[s]:
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_result_cards member %q: its card is resolved by fleet's own handler, which returns no tool result to read a card from", s))
			continue
		}
		set[s] = true
	}
	return set, problems
}

// installResultCardSet builds the set ConfigureAgentPolicy installs and logs
// each member it drops. Caller holds policyMu.
func installResultCardSet(members []string, critical map[string]bool) map[string]bool {
	set, problems := buildResultCardSet(members, critical)
	for _, problem := range problems {
		log.Printf("agent_policy: %s", problem)
	}
	return set
}

// ResultCardProblems is the preflight face of the result-card set, for
// `fleet validate-config`: the critical_tool_result_cards members
// ConfigureAgentPolicy would drop, checked against the same merged critical
// list the gate uses (base email suffixes included).
func ResultCardProblems(p AgentPolicy) []string {
	_, problems := buildResultCardSet(p.CriticalToolResultCards, mergedCriticalSuffixes(p))
	return problems
}

// ApprovalResultCard reports whether toolName's result may carry an outcome
// card: the bundle lists a matching suffix in critical_tool_result_cards (the
// tool name equals the suffix or ends with "_<suffix>", as for
// critical_tools).
func ApprovalResultCard(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeResultCards {
		if suffixMatches(toolName, suffix) {
			return true
		}
	}
	return false
}
