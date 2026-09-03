package clientconfig

import (
	"strings"
	"unicode"
)

// Connector catalog copy — the display_name/description a bundle attaches to
// each mcp_servers entry. These two strings are the ONLY thing a user reads
// before deciding whether to switch a connector on: chat's Tools picker (the
// wrench popover) and Settings → Connections both render
// `display_name || name` over `description`, and render nothing at all when
// the description is empty. A connector that ships neither shows up as a raw
// snake_case identifier with a blank body — indistinguishable from a broken
// row.
//
// The engine cannot author that copy (bundles are data, fleet is engine), so
// it does the two things it can:
//
//   - deriveDisplayName gives every connector a human-shaped label, so the
//     worst case is a plain one ("Openx") rather than a wire identifier
//     ("openx_mcp");
//   - validate() logs one loud line per connector missing either field, so a
//     bundle author finds out at boot instead of from a user pointing at an
//     empty popover row.
//
// Neither is a hard load error on purpose: display copy is cosmetic, and
// failing a whole bundle over a missing sentence would take a deployment down
// for a docs bug. The house style both sides are written to is documented in
// docs/MCP-CATALOG.md ("Connector copy"); bundle repos assert it in their own
// manifest tests, which is where bundle DATA belongs.

// displayNameNoise is the set of name segments that carry no meaning in a
// user-facing label. They are protocol/plumbing words a bundle author uses to
// disambiguate a file or a process, not words a person picking a connector
// needs to read.
var displayNameNoise = map[string]bool{
	"mcp":       true,
	"server":    true,
	"connector": true,
}

// deriveDisplayName turns a bundle server name into a fallback label:
// "openx_mcp" → "Openx", "knowledge_base" → "Knowledge Base", "s3_feeds" →
// "S3 Feeds". It is deliberately dumb — it cannot know that "openx_mcp" is
// spelled "OpenX" — so it is a floor under a missing display_name, never a
// substitute for one. A name made entirely of noise segments (or of nothing
// at all) falls back to the raw name rather than rendering an empty label.
func deriveDisplayName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	fields := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r)
	})
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		if displayNameNoise[strings.ToLower(f)] {
			continue
		}
		words = append(words, capitalizeFirst(f))
	}
	if len(words) == 0 {
		return trimmed
	}
	return strings.Join(words, " ")
}

// capitalizeFirst upper-cases the leading rune and leaves the rest of the
// segment alone, so an author's own casing survives ("openX" stays "OpenX",
// "s3" becomes "S3") instead of being flattened by a blanket Title().
func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
