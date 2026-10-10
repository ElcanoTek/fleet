package agentcore

import (
	"fmt"
	"strings"
)

// Resume after approval (docs/RESUME-AFTER-APPROVAL.md, ADR-0083).
//
// An approval card resolves outside any turn: the outcome is written to the
// conversation's history, and the model only reads it when the user types
// again. agent_policy.critical_tool_resume lets a bundle say "for these tools,
// start one new turn when the card is settled", so an agent that promised to
// verify an approved write actually does. This file is only the policy half:
// which tools opted in and how many automatic resumes a conversation may start
// per hour. The turn itself is started by the chat server (internal/httpapi
// approval_resume.go) through the ordinary queued-turn path, so it runs under
// the same agentcore.Run governance as any other turn.

const (
	// DefaultResumeMaxPerHour is the per-conversation cap on automatic resumes
	// in a rolling hour when the bundle does not set one. Every resume follows
	// a settled card (a person's click, or a timeout), so the cap is a
	// backstop against a loop (a short per-tool timeout re-staging forever),
	// not the expected rate.
	DefaultResumeMaxPerHour = 10
	// MaxResumeMaxPerHour bounds what a bundle may declare: one a minute.
	MaxResumeMaxPerHour = 60
)

// buildResumeSet resolves critical_tool_resume and its hourly cap. A member
// that is not a critical suffix is dropped and reported (no card is ever
// staged for such a tool, so the entry would do nothing, and it is most likely
// a typo), and so is one declared notify mode (it runs without a card). An
// out-of-range cap is reported and replaced by the default.
func buildResumeSet(p AgentPolicy, critical map[string]bool) (map[string]bool, int, []string) {
	set := make(map[string]bool, len(p.CriticalToolResume))
	var problems []string
	notify := map[string]bool{}
	for k, v := range p.CriticalToolModes {
		if strings.EqualFold(strings.TrimSpace(v), ApprovalModeNotify) {
			notify[strings.TrimSpace(k)] = true
		}
	}
	for _, s := range p.CriticalToolResume {
		s = strings.TrimSpace(s)
		if s == "" || set[s] {
			continue
		}
		switch {
		case !critical[s]:
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_resume member %q: it is not in critical_tools, so no approval card is ever staged for it and nothing would resume", s))
			continue
		case notify[s] && !nonReversibleSuffixes[s]:
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_resume member %q: its critical_tool_modes entry is notify, so it runs without a card and there is no approval to resume after", s))
			continue
		}
		set[s] = true
	}
	limit := p.CriticalToolResumeMaxPerHour
	switch {
	case limit == 0:
		limit = DefaultResumeMaxPerHour
	case limit < 0 || limit > MaxResumeMaxPerHour:
		problems = append(problems, fmt.Sprintf("critical_tool_resume_max_per_hour %d is outside 1..%d; using the default %d", limit, MaxResumeMaxPerHour, DefaultResumeMaxPerHour))
		limit = DefaultResumeMaxPerHour
	}
	return set, limit, problems
}

// ResumeAfterApprovalProblems is the preflight face of buildResumeSet, for
// `fleet validate-config`: the members ConfigureAgentPolicy would drop and an
// out-of-range cap, checked against the same merged critical list the gate
// uses (base email suffixes included).
func ResumeAfterApprovalProblems(p AgentPolicy) []string {
	critical := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		critical[s] = true
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical[s] = true
		}
	}
	_, _, problems := buildResumeSet(p, critical)
	return problems
}

// ResumeAfterApproval reports whether a settled approval card for toolName
// starts a resume turn: the bundle lists a matching suffix in
// critical_tool_resume (the tool name equals the suffix or ends with
// "_<suffix>", as for critical_tools).
func ResumeAfterApproval(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeResume {
		if suffixMatches(toolName, suffix) {
			return true
		}
	}
	return false
}

// ResumeAfterApprovalEnabled reports whether any tool opted in. When false
// the chat server does no resume bookkeeping at all, so a bundle without the
// key behaves exactly as before.
func ResumeAfterApprovalEnabled() bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return len(activeResume) > 0
}

// ResumeMaxPerHour is the installed per-conversation hourly cap.
func ResumeMaxPerHour() int {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return activeResumeMaxPerHour
}
