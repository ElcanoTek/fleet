package agentcore

import "sync"

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
}

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
)

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
}

// isParallelSafeTool reports whether the fully-prefixed MCP tool name is safe to
// dispatch concurrently under the active policy.
func isParallelSafeTool(name string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return activeParallelSafe[name]
}
