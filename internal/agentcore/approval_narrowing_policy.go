package agentcore

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Approval narrowing (docs/APPROVAL-NARROWING.md, ADR-0089).
//
// One critical call can cover many records: a deal plan's execute call names
// every item it runs in one list argument. The approval card used to be all or
// nothing for that call, so a person who wanted to leave one deal out had to
// decline and ask again. agent_policy.critical_tool_narrowable_args lets a
// bundle declare, per critical suffix, the ONE top-level list argument a
// person may narrow on the card:
//
//	critical_tool_narrowable_args:
//	  execute_plan: {arg: deal_ids}
//
// On approve, the chat server may then replace that argument with a non-empty
// subset of the staged values, in staged order, and nothing else. This file
// is only the policy half: which tools declared which argument. The checks
// and the rewrite are the chat server's (internal/httpapi
// approval_narrowing.go).

// narrowableArgName is the shape of a declared argument name: a plain
// top-level JSON key, the way MCP tool schemas name parameters.
var narrowableArgName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// buildNarrowableArgs resolves critical_tool_narrowable_args. An entry is
// kept only when its key stages a card for an MCP call (critical, not notify
// mode, not a handler-only card: buildStagedCardSuffixSet's rules) and its
// argument name is a plain top-level key. Every dropped entry is a problem
// for the caller to log (boot) or report (validate-config).
func buildNarrowableArgs(entries map[string]string, modes map[string]string, critical map[string]bool) (map[string]string, []string) {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(entries))
	var problems []string
	for _, k := range keys {
		suffix, arg := strings.TrimSpace(k), strings.TrimSpace(entries[k])
		if suffix == "" {
			problems = append(problems, "ignoring a critical_tool_narrowable_args entry with an empty tool suffix")
			continue
		}
		if !narrowableArgName.MatchString(arg) {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_narrowable_args entry %q: arg %q is not a top-level argument name (letters, digits and _, starting with a letter or _)", suffix, arg))
			continue
		}
		if selectsOutboundEmail(suffix) {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_narrowable_args entry %q: it selects outbound email sends, which are never narrowed (one approval per message as written)", suffix))
			continue
		}
		if nativeExecutableTools[suffix] {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_narrowable_args entry %q: it selects fleet's native %s tool, which runs no MCP call and is never narrowed (name an MCP tool by its server, e.g. <server>_%s)", suffix, suffix, suffix))
			continue
		}
		if SelectsOnlySyntheticTransport(suffix) {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_narrowable_args entry %q: it selects only inline http_tools or a2a peer tools, whose transports send no request _meta, so they are never narrowed", suffix))
			continue
		}
		set, staged := buildStagedCardSuffixSet("critical_tool_narrowable_args", []string{suffix}, modes, critical)
		if len(staged) > 0 {
			problems = append(problems, staged...)
			continue
		}
		if set[suffix] {
			out[suffix] = arg
		}
	}
	return out, problems
}

// installNarrowableArgs is buildNarrowableArgs for the boot install: each
// dropped entry is one log line.
func installNarrowableArgs(entries map[string]string, modes map[string]string, critical map[string]bool) map[string]string {
	out, problems := buildNarrowableArgs(entries, modes, critical)
	for _, problem := range problems {
		log.Printf("agent_policy: %s", problem)
	}
	return out
}

// nativeExecutableTools are the native tools whose approved card fleet runs
// itself (httpapi runStagedTool: bash runs inline, not as an MCP call). A
// behaviour that needs an MCP request (a narrowed argument told to the server
// in _meta, a cooperative cancellation) can never apply to one, so a policy
// key that names one exactly is reported instead of silently doing nothing.
var nativeExecutableTools = map[string]bool{toolNameBash: true}

// SelectsOnlySyntheticTransport reports whether a policy suffix can match
// only tools of fleet's synthetic servers (inline http_tools under
// mcp.HTTPToolServerName, a2a peers under mcp.A2AToolServerName): it names
// the server ("mcp__http_publish", "_a2a_helpdesk_send"). Their transports
// forward a call's arguments only (no request _meta, no cancellation), so a
// behaviour built on either is reported for such a key instead of being
// silently ignored at run time. A bare tool suffix ("publish") may also match
// an MCP tool, so it is not refused.
func SelectsOnlySyntheticTransport(suffix string) bool {
	for _, server := range []string{mcp.HTTPToolServerName, mcp.A2AToolServerName} {
		if strings.HasPrefix(suffix, "mcp_"+server+"_") || strings.HasPrefix(suffix, server+"_") {
			return true
		}
	}
	return false
}

// selectsOutboundEmail reports whether a policy suffix would select an
// outbound-email tool: it matches one of the base email suffixes
// (send_email, send_template_email; e.g. "email", "template_email"), or it is
// a narrower name of one (e.g. "mail_send_email").
func selectsOutboundEmail(suffix string) bool {
	for _, base := range baseCriticalToolSuffixes {
		if suffixMatches(base, suffix) || suffixMatches(suffix, base) {
			return true
		}
	}
	return false
}

// IsOutboundEmailTool reports whether toolName is an outbound-email send under
// the base email suffixes (send_email, send_template_email), the same
// predicate the policy check uses. Such a call is never narrowed.
func IsOutboundEmailTool(toolName string) bool {
	for _, base := range baseCriticalToolSuffixes {
		if suffixMatches(toolName, base) {
			return true
		}
	}
	return false
}

// NarrowableArgProblems is the preflight face of the narrowable-argument map,
// for `fleet validate-config`: the critical_tool_narrowable_args entries
// ConfigureAgentPolicy would drop, checked against the same merged critical
// list the gate uses (base email suffixes included).
func NarrowableArgProblems(p AgentPolicy) []string {
	_, problems := buildNarrowableArgs(p.CriticalToolNarrowableArgs, p.CriticalToolModes, mergedCriticalSuffixes(p))
	return problems
}

// NarrowableArgFor returns the list argument a person may narrow on toolName's
// approval card (the longest matching suffix wins, like CardDescriberFor), or
// "" when the bundle declared none: the card then approves the call whole, as
// before.
func NarrowableArgFor(toolName string) string {
	policyMu.RLock()
	defer policyMu.RUnlock()
	best, bestLen := "", -1
	for suffix, arg := range activeNarrowableArgs {
		if suffixMatches(toolName, suffix) && len(suffix) > bestLen {
			best, bestLen = arg, len(suffix)
		}
	}
	return best
}
