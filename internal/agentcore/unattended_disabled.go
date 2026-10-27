package agentcore

import (
	"fmt"
	"log"
	"sort"
	"strings"
)

// Unattended-disabled built-ins (docs/UNATTENDED-BUILTINS.md, ADR-0094).
//
// A scheduled run has no person at the keyboard to read an approval card, and
// the runs that read outside input (an inbox, a web page, a vendor report) are
// exactly the ones a crafted message can steer. Two of fleet's own built-ins
// reach past the run's sandbox on the model's word alone: create_task enqueues
// a NEW task whose prompt the next run treats as its instruction, and
// web_fetch fetches a URL host-side, outside the sandbox's egress seal.
//
// agent_policy.unattended_disabled_tools lets a bundle remove them from every
// unattended run, whatever a task's own grants say. It is a NARROWING: it can
// only take a built-in away from scheduled runs, never add one, and it leaves
// interactive chat (where a person sees each turn) unchanged. Members are
// exact built-in names from unattendedDisableable; anything else is dropped
// with a boot log line and fails validate-config, so a typo is never a
// silently-open door.

// unattendedDisableable is the set of built-in names the list may name: the
// fleet built-ins that act outside the run's sandbox on the model's word.
// Governance tools (confirm_audit, ask, notify, …) are deliberately absent —
// removing them would not narrow a run, it would break its audit.
var unattendedDisableable = map[string]bool{
	"create_task":  true,
	"web_fetch":    true,
	"download_url": true,
}

// activeUnattendedDisabled is the installed list (ConfigureAgentPolicy).
var activeUnattendedDisabled map[string]bool

// buildUnattendedDisabledSet normalizes unattended_disabled_tools: trimmed,
// de-duplicated. A blank or unknown member is dropped and reported.
func buildUnattendedDisabledSet(members []string) (map[string]bool, []string) {
	out := make(map[string]bool, len(members))
	var problems []string
	for _, raw := range members {
		s := strings.TrimSpace(raw)
		switch {
		case s == "":
			problems = append(problems, "ignoring a blank unattended_disabled_tools member")
		case !unattendedDisableable[s]:
			problems = append(problems, fmt.Sprintf("ignoring unattended_disabled_tools member %q: not a built-in this list can remove (want one of %s)", s, strings.Join(unattendedDisableableNames(), ", ")))
		default:
			out[s] = true
		}
	}
	return out, problems
}

func unattendedDisableableNames() []string {
	names := make([]string, 0, len(unattendedDisableable))
	for n := range unattendedDisableable {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// installUnattendedDisabledSet is buildUnattendedDisabledSet for
// ConfigureAgentPolicy: every dropped member is one boot log line.
func installUnattendedDisabledSet(members []string) map[string]bool {
	set, problems := buildUnattendedDisabledSet(members)
	for _, problem := range problems {
		log.Printf("agent_policy: %s", problem)
	}
	return set
}

// UnattendedDisabledToolProblems is the preflight face of
// buildUnattendedDisabledSet, for `fleet validate-config`.
func UnattendedDisabledToolProblems(p AgentPolicy) []string {
	_, problems := buildUnattendedDisabledSet(p.UnattendedDisabledTools)
	return problems
}

// UnattendedToolDisabled reports whether the installed policy removes the
// built-in named toolName from unattended (scheduled) runs.
func UnattendedToolDisabled(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return activeUnattendedDisabled[toolName]
}
