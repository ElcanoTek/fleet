package agentcore

import "fmt"

// Host reads: a bundle-declared tool fleet calls on its own, outside the
// governed loop, to fetch display data (ADR-0082 for approval-card
// describers at staging; ADR-0119 for the account-groups source with no turn
// at all). Such a call has no model, no approval card and no hook run behind
// it, so the tool must be one the bundle itself declared harmless to call
// unasked. These two functions are that rule, once: the preflight form for
// `fleet validate-config` and boot, and the call-time form under the policy
// actually installed.

// HostReadProblems reports why the tool `tool` on `server` could not be
// called as a host read under policy p, for a manifest key named key
// ("account_groups.source"). Empty when it could. A host read must be listed
// in parallel_safe_tools under its registered name (how a bundle declares a
// read-only tool) and must not be critical under the merged suffix list.
func HostReadProblems(p AgentPolicy, key, server, tool string) []string {
	full := MCPToolName(server, tool)
	var problems []string
	critical := append([]string(nil), baseCriticalToolSuffixes...)
	critical = append(critical, p.CriticalToolSuffixes...)
	for _, s := range critical {
		if s != "" && suffixMatches(full, s) {
			problems = append(problems, fmt.Sprintf("%s: tool %q is a critical tool (suffix %q), and fleet calls this tool without an approval; name a read-only tool", key, full, s))
			break
		}
	}
	listed := false
	for _, t := range p.ParallelSafeTools {
		if t == full {
			listed = true
			break
		}
	}
	if !listed {
		problems = append(problems, fmt.Sprintf("%s: tool %q is not in agent_policy.parallel_safe_tools, which is how a bundle declares a read-only tool; fleet will not call it", key, full))
	}
	return problems
}

// HostReadRefusal reports, under the policy installed in this process, why
// fleet may not call the registered tool name `full` as a host read, or ""
// when it may. Checked before every call, so a policy reload that makes the
// tool critical, drops it from parallel_safe_tools, or puts a pre_tool_use
// hook in front of it stops the call: the hook cannot run outside the loop,
// and skipping the call is the only way not to bypass it.
func HostReadRefusal(full string) string {
	switch {
	case IsCriticalTool(full):
		return "it is a critical tool"
	case !IsParallelSafeTool(full):
		return "it is not in agent_policy.parallel_safe_tools"
	case PreToolUseHookMatches(full):
		return "a pre_tool_use hook matches it"
	}
	return ""
}
