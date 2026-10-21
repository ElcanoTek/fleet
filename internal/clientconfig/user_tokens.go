package clientconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// Load-time spelling rules for the signed-in-user tokens (ADR-0087).
//
// The spawn paths substitute ONLY the exact "${FLEET_USER_EMAIL}" /
// "${FLEET_USER_NAME}" spellings, and they cannot tell an intended literal
// from the token. Two spellings would therefore go wrong silently:
//
//   - an ESCAPE, "$${FLEET_USER_EMAIL}" — anywhere, including inside a
//     ${OUTER:-...} default body, which the interpolator never expands —
//     collapses to (or leaves a "$" in front of) the exact token before spawn,
//     so a bundle asking for the literal text would be handed the user's PII;
//   - a PADDED or colon form, "${ FLEET_USER_EMAIL }" / "${FLEET_USER_NAME:-x}",
//     names the token but is not its spelling, so it would reach every
//     connector — scheduled and user-less spawns included — as a literal.
//
// There is no way to express either literal in an env value, so both are
// refused wherever a bundle can write one: the manifest (raw, before any
// pass) and an Agent Plugin's mcp.json env. The two tokens are new, so no
// bundle predating them can be broken by the rule.

// userTokenSpelling matches every spelling that NAMES a user token: an
// optional leading escape "$", then "${", optional spaces, the name, optional
// spaces, and an optional ":..." op before the closing brace.
var userTokenSpelling = regexp.MustCompile(`(\$?)\$\{(\s*)(FLEET_USER_EMAIL|FLEET_USER_NAME)(\s*)(:[^}]*)?\}`)

// userTokenSpellingProblem returns a description of the first escaped or
// non-canonical user-token spelling in s, or "" when every spelling present is
// exactly "${FLEET_USER_EMAIL}" / "${FLEET_USER_NAME}".
func userTokenSpellingProblem(s string) string {
	if !strings.Contains(s, "FLEET_USER_") {
		return ""
	}
	for _, m := range userTokenSpelling.FindAllStringSubmatch(s, -1) {
		name := m[3]
		switch {
		case m[1] != "":
			return fmt.Sprintf("%s: an escaped %s cannot be written — fleet substitutes the signed-in user for that spelling at MCP launch, so the escape would hand a connector the user's identity instead of the literal text (see docs/MCP-BUNDLE-ENV.md)", m[0], name)
		case m[2] != "" || m[4] != "" || m[5] != "":
			return fmt.Sprintf("%s: %s is a RESERVED runtime token that fleet substitutes at MCP launch; write exactly ${%s} — no spaces inside the braces and no default (see docs/MCP-BUNDLE-ENV.md)", m[0], name, name)
		}
	}
	return ""
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
// (which also refuses a user token in a seat-routing key) and the
// http-server user-token refusal below.
func validateServerEnvContract(s *ServerDef) error {
	if err := validateIdentityEnv(s); err != nil {
		return err
	}
	return httpServerUserTokenProblem(s)
}

// httpServerUserTokenProblem refuses a user token in an http server's env map:
// fleet never spawns an http server, so the value could never reach it, and a
// validate-config pass that accepted it would claim a contract that does not
// exist.
func httpServerUserTokenProblem(s *ServerDef) error {
	if s.Type != "http" {
		return nil
	}
	for k, v := range s.Env {
		if strings.Contains(v, "${"+reservedUserEmailVar+"}") || strings.Contains(v, "${"+reservedUserNameVar+"}") {
			return fmt.Errorf("mcp_servers[%q]: env %q uses ${FLEET_USER_EMAIL}/${FLEET_USER_NAME}, which fleet substitutes only for stdio servers it spawns; an http server's env never reaches it (see docs/MCP-BUNDLE-ENV.md)", s.Name, k)
		}
	}
	return nil
}
