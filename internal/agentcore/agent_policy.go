package agentcore

import (
	"log"
	"strings"
	"sync"
)

// AgentPolicy carries the client-bundle-configurable tool-behavior lists:
// which MCP tools are safe to dispatch in parallel, which tool-name suffixes are
// "critical" (require audit gating before execution), and the substitute-suffix
// map (which committed suffix may be discharged by which executed suffix).
//
// These lists are client-specific (e.g. ad-tech DSP deal-creation/execution
// tools); fleet itself ships NONE of them. The only critical suffixes fleet
// guarantees unconditionally are the generic outbound-email tools (see
// baseCriticalToolSuffixes); everything else is supplied by the client bundle
// via ConfigureAgentPolicy.
type AgentPolicy struct {
	// ParallelSafeTools are the fully-prefixed MCP tool names (mcp_<server>_<tool>)
	// safe to dispatch concurrently within a single assistant turn.
	ParallelSafeTools []string
	// CriticalToolSuffixes are the bare tool-name suffixes that require an audit
	// before execution (matched by suffix so "create_deal" matches a tool named
	// "<server>_create_deal"). The base suffixes are always merged in.
	CriticalToolSuffixes []string
	// CriticalToolSubstitutes maps a committed-tool suffix to the substitute
	// suffixes that may discharge its commitment (e.g. a high-level
	// execute_deal_from_prompt_inputs discharged by a lower-level create_deal).
	CriticalToolSubstitutes map[string][]string
	// CriticalToolTimeouts maps a bare tool-name suffix to a per-tool approval
	// default-deny window in seconds (#225). Matched by suffix exactly like
	// CriticalToolSuffixes ("send_email" matches "<server>_send_email"); the
	// longest matching suffix wins. It is the highest-priority layer of the
	// approval-timeout resolution chain (per-tool > per-conversation > global
	// FLEET_APPROVAL_TIMEOUT_SECONDS > hardcoded default). Empty = no per-tool
	// overrides, so every tool falls through to the per-conversation/global value.
	CriticalToolTimeouts map[string]int
	// CriticalToolModes maps a bare tool-name suffix to its approval MODE
	// (#1153): ApprovalModeApprove (default) or ApprovalModeNotify. Matched by
	// suffix exactly like CriticalToolSuffixes, longest match wins. Empty = every
	// critical tool blocks on a card, which is the behavior that existed before.
	CriticalToolModes map[string]string
	// CriticalToolUndoHints maps the same suffixes to a one-line, bundle-authored
	// statement of how to reverse the action, rendered on a notify record card.
	// Fleet does not know any client's undo verb and must not invent one.
	CriticalToolUndoHints map[string]string
}

// Approval modes a bundle may declare per critical tool (#1153).
const (
	// ApprovalModeApprove blocks the call on a card until a human decides. The
	// default, and what every critical tool did before modes existed.
	ApprovalModeApprove = "approve"
	// ApprovalModeNotify executes the call immediately and posts a card
	// RECORDING what happened. Only legitimate when undoing the action is cheap
	// and complete — that is the entire argument for it, and the bundle has to
	// back it up with an undo hint the card can show.
	ApprovalModeNotify = "notify"
)

// baseCriticalToolSuffixes are ALWAYS critical regardless of the configured
// bundle — generic destructive / external-effect tools fleet ships behavior for.
// These are deliberately client-agnostic (outbound email).
var baseCriticalToolSuffixes = []string{
	sendEmailToolSuffix, // "send_email"
	"send_template_email",
}

var (
	policyMu sync.RWMutex

	// activeParallelSafe is the set of fully-prefixed MCP tool names safe to run
	// concurrently. Empty by default (generic fleet runs nothing in parallel
	// until a bundle opts tools in).
	activeParallelSafe = map[string]bool{}

	// activeCriticalSuffixes is the ordered list of critical tool-name suffixes.
	// Defaults to the base (generic) suffixes only. Order is not load-bearing for
	// correctness: matchCriticalSuffix selects the longest match by length, and
	// isCriticalTool tests membership, not order.
	activeCriticalSuffixes = append([]string(nil), baseCriticalToolSuffixes...)

	// activeCriticalSubstitutes maps committed suffix -> allowed executed
	// substitutes. Empty by default.
	activeCriticalSubstitutes = map[string][]string{}

	// activeCriticalTimeouts maps a critical-tool suffix -> per-tool approval
	// default-deny window in seconds (#225). Empty by default (no per-tool
	// overrides); ApprovalTimeoutForTool returns 0 then, and callers fall back
	// to the per-conversation / global timeout.
	activeCriticalTimeouts = map[string]int{}

	// activeCriticalModes / activeCriticalUndoHints back the per-tool approval
	// mode (#1153). Empty by default: every critical tool blocks on a card.
	activeCriticalModes     = map[string]string{}
	activeCriticalUndoHints = map[string]string{}
)

// nonReversibleSuffixes can never be declared `notify`, whatever a bundle says.
// The whole case for executing without asking is "we can always roll it back",
// and for outbound email that is simply false — there is no undo, and the
// approval card IS the review step. A bundle that tries is logged and pinned
// back to `approve` rather than quietly honored.
var nonReversibleSuffixes = map[string]bool{
	sendEmailToolSuffix:   true,
	"send_template_email": true,
}

// ConfigureAgentPolicy installs the client bundle's tool-behavior policy. Call
// once at startup (cmd/fleet) before any turn runs. The base critical suffixes
// are always merged in (deduped, base-first). Safe to call with a zero
// AgentPolicy, which yields the generic defaults (no parallel tools, only the
// base critical email suffixes, no substitutes). Idempotent: each call fully
// replaces the previously installed policy.
func ConfigureAgentPolicy(p AgentPolicy) {
	policyMu.Lock()
	defer policyMu.Unlock()

	parallel := make(map[string]bool, len(p.ParallelSafeTools))
	for _, t := range p.ParallelSafeTools {
		if t != "" {
			parallel[t] = true
		}
	}
	activeParallelSafe = parallel

	seen := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	critical := make([]string, 0, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		if s != "" && !seen[s] {
			seen[s] = true
			critical = append(critical, s)
		}
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" && !seen[s] {
			seen[s] = true
			critical = append(critical, s)
		}
	}
	activeCriticalSuffixes = critical

	subs := make(map[string][]string, len(p.CriticalToolSubstitutes))
	for k, v := range p.CriticalToolSubstitutes {
		subs[k] = append([]string(nil), v...)
	}
	activeCriticalSubstitutes = subs

	timeouts := make(map[string]int, len(p.CriticalToolTimeouts))
	for k, v := range p.CriticalToolTimeouts {
		if k != "" && v > 0 {
			timeouts[k] = v
		}
	}
	activeCriticalTimeouts = timeouts

	modes := make(map[string]string, len(p.CriticalToolModes))
	for k, v := range p.CriticalToolModes {
		k = strings.TrimSpace(k)
		v = strings.ToLower(strings.TrimSpace(v))
		if k == "" {
			continue
		}
		if v != ApprovalModeNotify && v != ApprovalModeApprove {
			log.Printf("agent_policy: ignoring unknown approval mode %q for %q (want %q or %q)", v, k, ApprovalModeApprove, ApprovalModeNotify)
			continue
		}
		if v == ApprovalModeNotify && nonReversibleSuffixes[k] {
			log.Printf("agent_policy: refusing mode %q for %q — a sent message cannot be undone, so its approval card is the review step; pinned to %q", ApprovalModeNotify, k, ApprovalModeApprove)
			continue
		}
		modes[k] = v
	}
	activeCriticalModes = modes

	hints := make(map[string]string, len(p.CriticalToolUndoHints))
	for k, v := range p.CriticalToolUndoHints {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			hints[k] = v
		}
	}
	activeCriticalUndoHints = hints
}

// ApprovalModeForTool returns the bundle-declared approval mode for toolName and
// the one-line undo hint to render alongside it (#1153). Matching mirrors
// ApprovalTimeoutForTool — longest matching suffix wins — and an undeclared tool
// gets ApprovalModeApprove, so adding modes changes nothing for any bundle that
// does not use them.
func ApprovalModeForTool(toolName string) (mode, undoHint string) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	mode = ApprovalModeApprove
	bestLen := -1
	for suffix, m := range activeCriticalModes {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				mode = m
			}
		}
	}
	hintLen := -1
	for suffix, h := range activeCriticalUndoHints {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > hintLen {
				hintLen = len(suffix)
				undoHint = h
			}
		}
	}
	return mode, undoHint
}

// ApprovalTimeoutForTool returns the per-tool approval default-deny window (in
// seconds) configured for toolName via the bundle's
// agent_policy.critical_tool_timeouts, or 0 if none applies (#225). Matching
// mirrors isCriticalTool — a suffix matches when the tool name equals it or ends
// with "_<suffix>" — and the LONGEST matching suffix wins so a specific
// "execute_deal" pins over a generic "deal". 0 tells the caller to fall back to
// the per-conversation / global timeout.
func ApprovalTimeoutForTool(toolName string) int {
	policyMu.RLock()
	defer policyMu.RUnlock()
	bestLen := -1
	best := 0
	for suffix, secs := range activeCriticalTimeouts {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				best = secs
			}
		}
	}
	return best
}

// isParallelSafeTool reports whether the fully-prefixed MCP tool name is safe to
// dispatch concurrently under the active policy.
func isParallelSafeTool(name string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return activeParallelSafe[name]
}
