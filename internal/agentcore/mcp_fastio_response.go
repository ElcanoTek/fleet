package agentcore

import (
	"strings"
)

// Nested-bullet keys we strip from fast.io tool responses. Each key
// corresponds to a `- **<key>:**` markdown bullet whose body is
// either constant boilerplate or AI-generated noise. See
// trimFastIOResponse for the reasoning per key.
const (
	fastIODropBulletVirus     = "virus"
	fastIODropBulletOrigin    = "origin"
	fastIODropBulletLong      = "long"
	fastIODropBulletImportant = "important"
)

// fastIODropSections lists the top-level H1 sections we drop wholesale
// from fast.io tool responses. The cheap-probe + main loop both
// iterate this slice so adding a new section is a single-line change.
var fastIODropSections = []string{
	"_buildHash",
	"_next",
	"download_token",
	"resource_uri",
	"upload_method",
}

// fastIODropBullets lists the nested-bullet keys we strip. Mirrors
// fastIODropSections in shape so the cheap-probe path can iterate
// both lists symmetrically.
var fastIODropBullets = []string{
	fastIODropBulletVirus,
	fastIODropBulletOrigin,
	fastIODropBulletLong,
	fastIODropBulletImportant,
}

// trimFastIOResponse strips noise that fast.io's MCP responses bake
// into every reply. It's a pure string transform run by the
// dispatcher (see fantasy.go) before the tool result lands in the
// model's conversation. Two classes of noise we strip:
//
//  1. Top-level meta H1 sections — `# _buildHash`, `# _next`,
//     `# download_token`, `# resource_uri`, `# upload_method`.
//     These are either constant across calls (the build hash, the
//     boilerplate "Upload a file: upload action create-session…"
//     hints) or redundant with another field already in the
//     response (the JWT in `_download_token` is repeated verbatim
//     inside `download_url`'s query string).
//
//  2. Noisy nested bullets inside `# node` / `# nodes` blocks —
//     the AI-generated `summary.long` paragraph (often 500–1500
//     bytes of synthesized text the agent can ignore), the
//     `virus.status: unscanned / reason: scan not run` block
//     (~always those exact values), the `origin` block of internal
//     ids the agent never references, and inside `# blob_upload`,
//     the `important:` paragraph that ships the same explainer
//     every call.
//
// Sections we KEEP because they carry signal the agent acts on:
//   - `_total_fetched` / `_displayed` — pagination markers.
//   - `_fetched_ids` — opaque IDs for follow-up bulk-details lookups.
//   - `_warnings` — operational warnings (token expiry, filesize
//     mismatch warnings on upload sessions).
//   - `web_url` — the human-shareable URL.
//
// Cost in production before this trimmer: a typical file-discovery
// turn fires 8–12 fast.io calls; the `_next` + `_buildHash` blocks
// alone add 4–7 KB across the turn, and a single details lookup of
// an AI-summarized file can carry ~1.5 KB of the `summary.long`
// paragraph. Compounded, the discovery turn that broke conversation
// 3460d911 burned ~25 KB on fast.io meta before the agent gave up.
// Post-trim that drops to ~5–6 KB of actual file metadata.
func trimFastIOResponse(text string) string {
	if text == "" {
		return text
	}

	// Cheap probe: skip the parse if none of the drop markers are
	// present. Most non-fast.io responses pass through unchanged
	// because trimFastIOResponse only runs for the fast_io server,
	// but defending here too keeps the function safe to call from
	// helpers that don't gate by server name. We derive the probes
	// from the same drop-key lists below so a future drop-list
	// addition picks up the cheap-probe path automatically.
	hit := false
	for _, name := range fastIODropSections {
		if strings.Contains(text, "# "+name) {
			hit = true
			break
		}
	}
	if !hit {
		for _, key := range fastIODropBullets {
			if strings.Contains(text, "- **"+key+":**") {
				hit = true
				break
			}
		}
	}
	if !hit {
		return text
	}

	// Phase 1 — drop noisy top-level sections. These are H1 markdown
	// headers (`# name`) followed by lines until the next H1 or EOF.
	// We list them explicitly rather than "drop anything starting with
	// _" because some `_…` sections (_total_fetched, _displayed,
	// _warnings) carry signal we want to preserve.
	dropSections := make(map[string]bool, len(fastIODropSections))
	for _, n := range fastIODropSections {
		dropSections[n] = true
	}

	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if name, ok := parseFastIOH1Header(line); ok {
			skipping = dropSections[name]
			if skipping {
				continue
			}
		}
		if skipping {
			continue
		}
		kept = append(kept, line)
	}

	// Phase 2 — strip noisy nested bullets. These show up inside `#
	// node` / `# nodes` blocks (storage details) and `# blob_upload`
	// (upload create-session). The structure is a 2-space-indented
	// bullet whose key is one of the names below; we drop the bullet
	// AND all its deeper-indented child lines.
	dropBullets := map[string]bool{
		fastIODropBulletVirus:     true, // status:unscanned/reason:scan not run; constant noise
		fastIODropBulletOrigin:    true, // internal upload_session_id + creator id
		fastIODropBulletLong:      true, // AI-generated multi-paragraph summary
		fastIODropBulletImportant: true, // boilerplate explainer inside blob_upload
	}

	out := stripNoisyNestedBullets(kept, dropBullets)

	// Collapse runs of blank lines created by section removal so the
	// rendered output reads cleanly.
	joined := strings.Join(out, "\n")
	joined = strings.TrimRight(joined, "\n")
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}
	return joined
}

// parseFastIOH1Header returns the section name (e.g. "_buildHash",
// "download_token") for a line that looks like `# name`, plus ok=true.
// Returns ok=false for any other line. We accept any name (not just
// underscore-prefixed) so the trimmer can drop the public-named
// noise sections like `# download_token` that fast.io emits at the
// top level.
func parseFastIOH1Header(line string) (string, bool) {
	if !strings.HasPrefix(line, "# ") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(line, "# "))
	if name == "" {
		return "", false
	}
	// Section names are single tokens. Reject anything with internal
	// whitespace so we don't accidentally swallow a real H1 like
	// `# important note`. The fast.io API only emits single-token
	// names today.
	if strings.ContainsAny(name, " \t") {
		return "", false
	}
	return name, true
}

// stripNoisyNestedBullets removes specific child blocks from the
// markdown-bullet structure fast.io uses for `# node` /
// `# blob_upload` payloads. A "bullet" starts with `- **key:**` at
// any indentation level; its block extends until the next sibling
// or less-indented line.
//
// We keep the implementation deliberately simple — a stack-based
// indentation scanner. Fast.io's response shape is stable enough
// that a parser tuned to "indented markdown bullets, names from a
// fixed set" reads cleanly without pulling in a full markdown AST.
func stripNoisyNestedBullets(lines []string, drop map[string]bool) []string {
	out := make([]string, 0, len(lines))
	skipIndent := -1 // < 0 means not currently skipping a block
	for _, line := range lines {
		if skipIndent >= 0 {
			// Decide whether this line continues the dropped block
			// (deeper-indented than the bullet that triggered the
			// drop) or signals the end of it (same-or-shallower).
			indent := leadingSpaces(line)
			// A blank line keeps us in skipping mode — the bullet's
			// children may resume on the next line.
			if strings.TrimSpace(line) == "" {
				continue
			}
			if indent > skipIndent {
				continue
			}
			skipIndent = -1
			// Fall through so this line gets re-evaluated as a
			// potential new bullet trigger.
		}
		if name, ok := parseNoisyBulletKey(line); ok && drop[name] {
			skipIndent = leadingSpaces(line)
			continue
		}
		out = append(out, line)
	}
	return out
}

// parseNoisyBulletKey extracts `key` from a line shaped like
// `<indent>- **key:** <maybe value>`. Returns (key, true) on match,
// ("", false) otherwise. We accept either bare `- **key:**` (block
// follows below) or `- **key:** value` (scalar) — both shapes are
// drop-candidates because the values themselves are the noise.
func parseNoisyBulletKey(line string) (string, bool) {
	stripped := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(stripped, "- **") {
		return "", false
	}
	rest := strings.TrimPrefix(stripped, "- **")
	closeIdx := strings.Index(rest, ":**")
	if closeIdx < 0 {
		return "", false
	}
	return rest[:closeIdx], true
}

// leadingSpaces returns the count of leading space (and tab)
// characters on a line. Used by the indentation scanner to find
// block boundaries.
func leadingSpaces(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' || r == '\t' {
			n++
			continue
		}
		break
	}
	return n
}
