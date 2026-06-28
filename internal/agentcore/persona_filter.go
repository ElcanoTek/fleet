package agentcore

import (
	"log"
	"strings"

	"charm.land/fantasy"
)

// Per-persona tool allowlist (#294): different personas see different tool
// subsets. This is Gate-4 — a least-privilege NARROWING layered on top of
// Gate-1 (server opt-in), Gate-2 (per-server tool allowlist), and Gate-3 (the
// per-task credential allowlist). It can only SUBTRACT from the tools a run is
// already permitted to offer, never add: the persona filter runs over the slice
// buildFantasyTools already produced (after all three earlier gates), so a tool
// a persona "allows" but that the server/credential gates already dropped never
// reappears. A persona with no policy keeps current behavior.
//
// Enforcement is at tool-REGISTRATION (run.go applies resolvePersonaTools to the
// slice before it is handed to fantasy.WithTools), not at call time: a tool the
// model never sees in its tool list cannot be hallucinated into a call. This is
// the same security-meaningful enforcement point Gate-2 uses.
//
// Credentials are unaffected — they stay host-side, brokered out-of-process; the
// filter only decides which tool SCHEMAS are advertised to the model.

// PersonaToolPermissions is the per-persona tool policy, mirrored from
// clientconfig.PersonaToolPermissions and converted at the driver boundary so
// agentcore carries no dependency on clientconfig.
//
//   - Both lists empty → no narrowing (all permitted tools are offered).
//   - Allow non-empty → only matching tools are offered (default-deny).
//   - Only Deny set → all tools except matching ones are offered (default-allow
//     with exceptions).
//   - Deny takes precedence when a tool matches both lists.
type PersonaToolPermissions struct {
	Allow []string
	Deny  []string
}

// empty reports whether the policy declares no narrowing at all. An empty policy
// is the backward-compatible passthrough: every permitted tool is offered.
func (p PersonaToolPermissions) empty() bool {
	return len(p.Allow) == 0 && len(p.Deny) == 0
}

// resolvePersonaTools filters all down to the tools the persona's policy
// permits, emitting a persona_tool_blocked observer event for every suppressed
// tool so operators can see which tools were filtered without log archaeology.
//
// all is the slice buildFantasyTools already produced — i.e. the tools that
// survived every earlier gate. Because the filter only drops entries from that
// slice, the persona allowlist can NARROW but never WIDEN beyond the server /
// credential gates. A nil/empty policy returns all unchanged (and allocates
// nothing), so the default path is zero-overhead and behavior is unchanged for
// any persona without a manifest entry.
func resolvePersonaTools(persona string, policy PersonaToolPermissions, all []fantasy.AgentTool, obs Observer) []fantasy.AgentTool {
	if policy.empty() {
		return all
	}
	out := make([]fantasy.AgentTool, 0, len(all))
	blocked := 0
	for _, t := range all {
		name := t.Info().Name
		// Deny wins over allow on any conflict: check it first.
		if matchesAnyPattern(name, policy.Deny) {
			emitPersonaToolBlocked(obs, persona, name, "deny")
			blocked++
			continue
		}
		// A non-empty allow list is default-deny: a tool that matches nothing is
		// dropped. An empty allow list (deny-only policy) admits everything the
		// deny list did not catch.
		if len(policy.Allow) > 0 && !matchesAnyPattern(name, policy.Allow) {
			emitPersonaToolBlocked(obs, persona, name, "not_in_allow")
			blocked++
			continue
		}
		out = append(out, t)
	}
	log.Printf("persona %q tool policy: %d of %d tools offered (%d blocked)", persona, len(out), len(all), blocked)
	return out
}

// emitPersonaToolBlocked records a single suppressed-tool audit event. nil-safe.
func emitPersonaToolBlocked(obs Observer, persona, tool, reason string) {
	if obs == nil {
		return
	}
	obs.Observe("persona_tool_blocked", map[string]any{
		"persona": persona,
		"tool":    tool,
		"reason":  reason,
	})
}

// matchesAnyPattern reports whether toolName matches any pattern in patterns.
func matchesAnyPattern(toolName string, patterns []string) bool {
	for _, p := range patterns {
		if matchesToolPattern(toolName, p) {
			return true
		}
	}
	return false
}

// matchesToolPattern reports whether the fantasy tool name matches one pattern.
// Supported patterns:
//
//	"*"                star — matches every tool
//	"mcp:server/tool"  the single fantasy name "mcp_<server>_<tool>"
//	"mcp:server/*"     any fantasy name with prefix "mcp_<server>_"
//	"prefix/*"         any fantasy name with the literal prefix "prefix"
//	"bash"             exact match against the fantasy name
//
// Patterns are trimmed; a blank pattern matches nothing. The "mcp:" form maps
// the manifest's human-readable server/tool reference onto the fantasy naming
// scheme mcpTool.Name() produces ("mcp_<server>_<tool>"), so a manifest author
// writes mcp:filesystem/read_file rather than the wire name.
func matchesToolPattern(toolName, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "mcp:"); ok {
		server, tool, found := strings.Cut(rest, "/")
		if found {
			prefix := "mcp_" + server + "_"
			if tool == "*" {
				return strings.HasPrefix(toolName, prefix)
			}
			return toolName == prefix+tool
		}
		// "mcp:server" with no slash: treat as that server's whole surface.
		return strings.HasPrefix(toolName, "mcp_"+rest+"_")
	}
	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(toolName, prefix)
	}
	return toolName == pattern
}
