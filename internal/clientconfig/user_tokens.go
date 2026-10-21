package clientconfig

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ElcanoTek/fleet/internal/usertoken"
)

// Load-time spelling and placement rules for the signed-in-user tokens
// (ADR-0087).
//
// The spawn paths substitute ONLY the exact "${FLEET_USER_EMAIL}" /
// "${FLEET_USER_NAME}" spellings, ONLY in an env value of a stdio server, and
// they cannot tell an intended literal from the token. Three things would
// therefore go wrong silently:
//
//   - an ESCAPE, "$${FLEET_USER_EMAIL}" — anywhere, including inside a
//     ${OUTER:-...} default body, which the interpolator never expands —
//     collapses to (or leaves a "$" in front of) the exact token before spawn,
//     so a bundle asking for the literal text would be handed the user's PII;
//   - a PADDED or colon form, "${ FLEET_USER_EMAIL }" (padding of any kind,
//     Unicode whitespace and invisible characters included) or
//     "${FLEET_USER_NAME:-x}", names the token but is not its spelling, so it
//     would reach every connector — scheduled and user-less spawns included —
//     as a literal;
//   - a token anywhere but an env value (an env KEY, the command or args, a
//     url or header) is never substituted, so it ships verbatim.
//
// There is no way to express either literal in an env value, so all of these
// are refused wherever a bundle can write one: the manifest (the raw text
// before any pass, then each parsed server) and an Agent Plugin's mcp.json
// (each parsed entry, which is skipped). The two tokens are new, so no bundle
// predating them can be broken by the rules. What counts as a spelling is
// internal/usertoken's one matcher, shared with the spawn-time backstop
// (agentcore.ExpandUserEnv) so the two sides can never disagree.

// userTokenSpellingProblem returns a description of the first escaped or
// non-canonical user-token spelling in s, or "" when every spelling present is
// exactly "${FLEET_USER_EMAIL}" / "${FLEET_USER_NAME}". The offending text is
// quoted so invisible padding shows up in the error.
func userTokenSpellingProblem(s string) string {
	sp, bad := usertoken.FirstInexact(s)
	if !bad {
		return ""
	}
	if sp.Escaped {
		return fmt.Sprintf("%q: an escaped %s cannot be written — fleet substitutes the signed-in user for that spelling at MCP launch, so the escape would hand a connector the user's identity instead of the literal text (see docs/MCP-BUNDLE-ENV.md)", sp.Text, sp.Var)
	}
	return fmt.Sprintf("%q: %s is a RESERVED runtime token that fleet substitutes at MCP launch; write exactly ${%s} — no spaces inside the braces, no other padding, and no default (see docs/MCP-BUNDLE-ENV.md)", sp.Text, sp.Var, sp.Var)
}

// rawUserTokenSpellingProblem checks the RAW manifest text — before the
// interpolation pass collapses any escape — so an escape inside a nested
// ${OUTER:-...} default body is caught too.
func rawUserTokenSpellingProblem(raw []byte, manifestPath string) error {
	if p := userTokenSpellingProblem(string(raw)); p != "" {
		return fmt.Errorf("client config manifest %s: %s", manifestPath, p)
	}
	return nil
}

// validateServerEnvContract runs the per-server env checks: identity_env
// (which also refuses a user token in a seat-routing key) and the user-token
// placement rules below.
func validateServerEnvContract(s *ServerDef) error {
	if err := validateIdentityEnv(s); err != nil {
		return err
	}
	return serverUserTokenProblem(s)
}

// serverUserTokenProblem enforces where a user token may appear in a PARSED
// server definition — a manifest entry after interpolation, or an Agent
// Plugin's mcp.json entry (which never sees the manifest's raw-text passes):
// only as an env VALUE of a stdio server, and only in the exact spelling.
// The spawn paths substitute the env map's values and nothing else, so a token
// in an env KEY would name a literal "${FLEET_USER_EMAIL}" variable, and one
// in the command, args, url or a header (name or value) would ship verbatim to
// the process or onto the wire. Fleet never spawns an http server, so its env
// could never carry the user either.
//
// Any spelling counts (usertoken.Mentions), so a form the raw-text checks
// cannot see — a YAML escape that only becomes padding once the YAML is
// parsed — is caught here too.
func serverUserTokenProblem(s *ServerDef) error {
	where := func(field string) error {
		return fmt.Errorf("mcp_servers[%q]: %s names ${FLEET_USER_EMAIL}/${FLEET_USER_NAME}; fleet substitutes the signed-in user only in an env value of a stdio server it spawns, so it can never resolve here (see docs/MCP-BUNDLE-ENV.md)", s.Name, field)
	}
	if usertoken.Mentions(s.Command) {
		return where("command")
	}
	for i, a := range s.Args {
		if usertoken.Mentions(a) {
			return where(fmt.Sprintf("args[%d]", i))
		}
	}
	if usertoken.Mentions(s.URL) {
		return where("url")
	}
	for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
		if usertoken.Mentions(k) {
			return where(fmt.Sprintf("header name %q", k))
		}
		if usertoken.Mentions(s.Headers[k]) {
			return where(fmt.Sprintf("header %q", k))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if usertoken.Mentions(k) {
			return where(fmt.Sprintf("env key %q", k))
		}
		v := s.Env[k]
		if !usertoken.Mentions(v) {
			continue
		}
		if s.Type == "http" {
			return fmt.Errorf("mcp_servers[%q]: env %q uses ${FLEET_USER_EMAIL}/${FLEET_USER_NAME}, which fleet substitutes only for stdio servers it spawns; an http server's env never reaches it (see docs/MCP-BUNDLE-ENV.md)", s.Name, k)
		}
		if p := userTokenSpellingProblem(v); p != "" {
			return fmt.Errorf("mcp_servers[%q]: env %q: %s", s.Name, k, p)
		}
	}
	return nil
}
