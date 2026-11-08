package clientconfig

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/usertoken"
)

// interpolateManifest performs a pre-unmarshal pass over the raw manifest bytes,
// expanding shell-style env references so the bundle can carry "env-or-default"
// config semantics (the getEnvOrDefault("VAR","literal") behavior the legacy
// internal/config had). It supports three POSIX-style forms:
//
//	${VAR}            Bare reference. If VAR is SET, substitute its value. If VAR
//	                  is UNSET, the token is LEFT INTACT (deferred): per-MCP-server
//	                  env/header values are resolved lazily at spawn time against
//	                  the live process env, where an unset credential is
//	                  legitimate (the server gates off or optional_env drops the
//	                  key). The pre-unmarshal pass therefore must NOT hard-fail on
//	                  an unset bare ${VAR} — that would make loading any bundle
//	                  impossible unless every connector secret were exported up
//	                  front. Load DOES fail afterwards when a deferred token sits
//	                  in a field OUTSIDE those lazily-resolved maps (#1123):
//	                  nothing ever re-reads a url:/command:/sandbox.image, so the
//	                  literal token could never resolve. A value that MUST be
//	                  present at load uses the explicit ${VAR:?message} form.
//	${VAR:-default}   POSIX use-default. If VAR is set AND non-empty, use it; else
//	                  use default (empty env counts as unset). This is the restored
//	                  env-or-default form: env can override, the literal is kept.
//	${VAR:?message}   POSIX required. If VAR is unset OR empty, fail the load with
//	                  message (naming the var + the manifest path).
//
// Escaping: a literal "$${" emits "${" without triggering expansion, so a value
// that genuinely needs a literal ${...} can be written.
//
// Nested braces: the default/message body of a :- / :? form is scanned with
// brace-depth tracking, so a default that itself contains "${...}" (or any
// balanced braces) survives intact; expansion does NOT recurse into it.
//
// YAML-quoting requirement: a :- / :? default contains a ':' (and a URL default
// contains '://'), so the field MUST be quoted in YAML, e.g.
//
//	pubmatic_base_url: "${PUBMATIC_BASE_URL:-https://api.pubmatic.com}"
//
// An unquoted value would make the YAML parser read the ':' as a mapping
// separator. The interpolation runs on raw bytes before unmarshal, so the quotes
// remain around the substituted value and the YAML round-trips correctly.
//
// Connector env/header values (mcp_servers[].env/headers, http_tools[].headers)
// are special: this pass VALIDATES them — an unset ${VAR:?...} or a non-bare
// reserved token still fails the load — but its substitutions there are
// discarded. Load re-sources those maps from the raw manifest so they resolve
// against the live process env at catalog-build time (see
// interpolate/resolveEnvMap): an unset credential is legitimate there, the
// broker child resolves against its own environment, and keeping this pass's
// output would consume the $${ escape one pass early. Load folds the
// FLEET_ENV_FILE env file into the process env BEFORE running this pass
// (#1123), so every other field — and ${VAR:?...} validation — sees the same
// environment the runtime uses.
func interpolateManifest(raw []byte, manifestPath string) ([]byte, error) {
	// Escaped or padded user-token spellings (user_tokens.go) are refused on
	// the raw text, before this pass collapses an escape — including one
	// nested in a default body, which the loop below never descends into.
	if err := rawUserTokenSpellingProblem(raw, manifestPath); err != nil {
		return nil, err
	}
	s := string(raw)
	var fromEnv [][2]int // output ranges substituted from the process env
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		// Escape: "$${" -> literal "${" (consume one leading '$').
		if strings.HasPrefix(s[i:], "$${") {
			sb.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(s[i:], "${") {
			sb.WriteByte(s[i])
			i++
			continue
		}
		// Found "${": scan to the matching '}' tracking brace depth so nested
		// braces in a default body don't terminate the expression early.
		end, ok := matchBrace(s, i+1) // index of the '}' closing the '{' at i+1
		if !ok {
			return nil, fmt.Errorf("client config manifest %s: unterminated ${...} expression at offset %d", manifestPath, i)
		}
		expr := s[i+2 : end] // contents between "${" and "}"
		val, err := expandExpr(expr, manifestPath)
		if err != nil {
			return nil, err
		}
		if val.deferred {
			// Unset bare ${VAR}: leave the literal token in place for spawn-time
			// resolution.
			sb.WriteString(s[i : end+1])
		} else {
			if val.fromEnv && val.text != "" {
				fromEnv = append(fromEnv, [2]int{sb.Len(), sb.Len() + len(val.text)})
			}
			sb.WriteString(val.text)
		}
		i = end + 1
	}
	out := sb.String()
	// User-token provenance (ADR-0087): a spelling the substituted env values
	// helped form ("${A}${B}" with A="${FLEET_USER_" and B="EMAIL}") was not
	// written by the bundle, yet every later check would read it as if it
	// were. Fail the load rather than let it reach a connector — or an
	// identity_env key — as the signed-in user.
	if usertoken.AssembledFrom(out, fromEnv) {
		return nil, fmt.Errorf("client config manifest %s: environment values substituted into the manifest assemble a reserved ${FLEET_USER_EMAIL}/FLEET_USER_NAME spelling; fleet substitutes the signed-in user only where the manifest itself writes the token (see docs/MCP-BUNDLE-ENV.md)", manifestPath)
	}
	return []byte(out), nil
}

// expandResult is the outcome of expanding one ${...} expression.
type expandResult struct {
	text     string // resolved replacement text (when deferred is false)
	deferred bool   // true => leave the literal ${VAR} token in place (unset bare ref)
	fromEnv  bool   // text came from the process env, not the manifest (user-token provenance, ADR-0087)
}

// reservedWorkspaceVar is the RESERVED spawn-time token name a bundle may use
// in an mcp_servers[].env value: ${FLEET_WORKSPACE} is substituted by the MCP
// spawn paths with a fleet-provided writable directory (per-run for a run's
// dedicated client, a stable per-deployment dir for shared spawns — see
// internal/agentcore's WorkspaceEnvToken, the consuming side of this
// contract; the name is inlined here to keep clientconfig dependency-free of
// agentcore). Both interpolation passes leave the bare token INTACT — it is
// never resolved from the process env (even if an operator exports a var of
// that name) and never blanked. Only the bare ${FLEET_WORKSPACE} spelling is
// reserved; any colon-suffixed (:-/:?) spelling fails the load (expandExpr).
//
// ${FLEET_WORKSPACE_ROOT} (agentcore.WorkspaceRootEnvToken) is the deployment
// workspace root, substituted on every spawn path. It shares the operator
// knob's NAME, but the bare token is still never read from the process env
// here: the spawn paths resolve it through the same root lookup the knob
// feeds, so an operator setting and the token always agree, and an unset knob
// yields the ./workspace default instead of a blank.
//
// ${FLEET_USER_EMAIL} (agentcore.UserEmailEnvToken, ADR-0087) is the signed-in
// user's email on an interactive chat turn, substituted only when the
// credential owner opens that turn's scope and dropped on every other spawn
// path. It is reserved for the same reason as the others, and more so: an
// operator-exported FLEET_USER_EMAIL resolved at load would hand ONE fixed
// person's identity to every user's turn.
//
// FLEET_USER_NAME (usertoken.ReservedNameVar) is reserved with NO token behind
// it — fleet has no name for the signed-in user — so it is never read from the
// process env and every spelling of it fails the load (user_tokens.go).
const (
	reservedWorkspaceVar     = "FLEET_WORKSPACE"
	reservedWorkspaceRootVar = "FLEET_WORKSPACE_ROOT"
	reservedTaskIDVar        = "FLEET_TASK_ID"
	reservedUserEmailVar     = usertoken.EmailVar
	reservedUserNameVar      = usertoken.ReservedNameVar
)

func reservedRuntimeVar(name string) bool {
	switch name {
	case reservedWorkspaceVar, reservedWorkspaceRootVar, reservedTaskIDVar, reservedUserEmailVar, reservedUserNameVar:
		return true
	}
	return false
}

// expandExpr resolves the body of a single ${...} expression (the text between
// the braces) into a replacement, implementing the ${VAR}, ${VAR:-default} and
// ${VAR:?message} forms.
func expandExpr(expr, manifestPath string) (expandResult, error) {
	// Reserved spawn-time tokens admit ONLY the bare spelling. Any
	// colon-suffixed form (:-, :?, or a typo'd op) would resolve the token from
	// the process env or a literal default — silently defeating the launch-time
	// substitution — so it fails the load.
	if idx := strings.IndexByte(expr, ':'); idx >= 0 {
		if name := strings.TrimSpace(expr[:idx]); reservedRuntimeVar(name) {
			return expandResult{}, fmt.Errorf(
				"client config manifest %s: ${%s}: %s is a RESERVED runtime token that fleet substitutes at MCP launch; only the bare ${%s} spelling is supported — it is never read from the process env and takes no default (see docs/MCP-BUNDLE-ENV.md)",
				manifestPath, expr, name, name)
		}
	}
	// Find the first ":-" or ":?" operator at the TOP of the expression. The var
	// name itself never contains ':', so the first ':' (if any) starts the op.
	if idx := strings.IndexByte(expr, ':'); idx >= 0 && idx+1 < len(expr) {
		name := expr[:idx]
		op := expr[idx+1]
		body := expr[idx+2:]
		switch op {
		case '-': // ${VAR:-default}
			if v, ok := lookupNonEmpty(name); ok {
				return expandResult{text: v, fromEnv: true}, nil
			}
			return expandResult{text: body}, nil
		case '?': // ${VAR:?message}
			if v, ok := lookupNonEmpty(name); ok {
				return expandResult{text: v, fromEnv: true}, nil
			}
			msg := strings.TrimSpace(body)
			if msg == "" {
				msg = "required value is unset or empty"
			}
			return expandResult{}, fmt.Errorf("client config manifest %s: ${%s:?...}: %s", manifestPath, strings.TrimSpace(name), msg)
		}
		// Any other ':X' is not a form we support; fall through and treat the
		// whole expression as a bare name (which will almost certainly be unset,
		// hence deferred) rather than silently mangling it.
	}
	name := strings.TrimSpace(expr)
	if reservedRuntimeVar(name) {
		// Reserved spawn-time token: always deferred, never read from the
		// process env (an operator-exported FLEET_WORKSPACE must not hijack it).
		return expandResult{deferred: true}, nil
	}
	if v, ok := lookupNonEmpty(name); ok {
		return expandResult{text: v, fromEnv: true}, nil
	}
	// Unset bare ${VAR}: defer to spawn-time resolution.
	return expandResult{deferred: true}, nil
}

// lookupNonEmpty reports the trimmed process-env value for name and whether it is
// set AND non-empty (empty env counts as unset, matching POSIX ${VAR:-default}
// and the legacy getEnvOrDefault, which treated an empty value as "use default").
//
// It is the one read behind every ${VAR} the manifest resolves — at load
// (expandExpr) and at catalog-build time (resolveSpawnExpr) — so it is also
// where the signed-in-user token's provenance is enforced (ADR-0087): a
// process value that names ${FLEET_USER_EMAIL} (or FLEET_USER_NAME) in any
// spelling counts as UNSET. Otherwise `OWNER_ID: "${OWNER_ID}"` with
// OWNER_ID=${FLEET_USER_EMAIL} exported would pass every load check (the
// manifest shows only ${OWNER_ID}) and then be filled with whoever is at the
// keyboard — in an identity_env key, a seat or money would follow the user.
// The token is honoured only where the bundle itself writes it.
func lookupNonEmpty(name string) (string, bool) {
	name = strings.TrimSpace(name)
	v := strings.TrimSpace(envreg.Unregistered(name))
	if v == "" {
		return "", false
	}
	if usertoken.Mentions(v) {
		warnUserTokenFromEnv(name)
		return "", false
	}
	return v, true
}

// userTokenEnvWarned dedupes warnUserTokenFromEnv per variable name.
var userTokenEnvWarned sync.Map

// warnUserTokenFromEnv logs, once per variable, that a process value naming a
// user token was ignored. The value itself is never logged.
func warnUserTokenFromEnv(name string) {
	if _, seen := userTokenEnvWarned.LoadOrStore(name, true); seen {
		return
	}
	slog.Warn("clientconfig: warning: a process env value names the reserved ${FLEET_USER_EMAIL}/FLEET_USER_NAME token; fleet substitutes the signed-in user only where the bundle manifest itself writes the token, so the variable is treated as unset (see docs/MCP-BUNDLE-ENV.md)", "name", name)
}

// matchBrace returns the index of the '}' that closes the '{' at position open
// (s[open] must be '{'), tracking nested '{' '}' so a brace inside a default body
// is balanced rather than terminating the expression.
func matchBrace(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// interpolate resolves the ${...} references in an MCP env/header value
// against the process env at catalog-build time — AFTER config.Load has
// applied the .env file, which is why those values keep their raw manifest
// text through Load (see the re-source step there). It is the single
// substituting pass for those fields and supports the same forms as
// interpolateManifest: "$${" emits a literal "${" without expanding, ${VAR}
// substitutes the value (empty when unset — resolveEnvMap's optional_env drop
// keys on that), ${VAR:-default} falls back to the default, and ${VAR:?msg}
// substitutes the value (an unset var cannot reach here: interpolateManifest
// already failed the load). The reserved runtime tokens (reservedRuntimeVar)
// are preserved verbatim — never env-resolved, never blanked — for the MCP
// spawn paths to substitute. An unterminated "${" is copied through verbatim.
//
// It also checks the user token's provenance (ADR-0087): it records the byte
// ranges of the output that came from the process env, and if any user-token
// spelling in the assembled value overlaps one of them — "${A}${B}" with
// A="${FLEET_USER_" and B="EMAIL}", or an env fragment completing manifest
// text — the token was not written by the bundle, and assembled is true (the
// result is then ""). Tokens the manifest wrote, verbatim or in a default
// body, are untouched.
func interpolate(v string) (out string, assembled bool) {
	if !strings.Contains(v, "${") {
		return v, false
	}
	var fromEnv [][2]int
	var sb strings.Builder
	sb.Grow(len(v))
	for i := 0; i < len(v); {
		// Escape: "$${" -> literal "${" (consume one leading '$').
		if strings.HasPrefix(v[i:], "$${") {
			sb.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(v[i:], "${") {
			sb.WriteByte(v[i])
			i++
			continue
		}
		end, ok := matchBrace(v, i+1)
		if !ok {
			sb.WriteString(v[i:])
			break
		}
		text, env := resolveSpawnExpr(v[i+2:end], v[i:end+1])
		if env && text != "" {
			fromEnv = append(fromEnv, [2]int{sb.Len(), sb.Len() + len(text)})
		}
		sb.WriteString(text)
		i = end + 1
	}
	out = sb.String()
	if usertoken.AssembledFrom(out, fromEnv) {
		return "", true
	}
	registerHostValues(out, fromEnv)
	return out, false
}

// registerHostValues hands every fragment of a connector env or header value
// that came from the host process env — a ${VAR} the manifest interpolates,
// whatever VAR is called — to the log redactor as a secret literal, before
// any connector is spawned with it. The secret-NAME heuristic misses
// credentials under names like AUTH or SESSION or a vendor's own; provenance
// does not: a value the bundle writes literally (REGION: us-east-1) lives in
// the bundle repo and is not a secret, while one interpolated from the host
// is the operator's to protect. The redactor's minimum literal length keeps
// "1" or "true" out.
func registerHostValues(out string, fromEnv [][2]int) {
	if len(fromEnv) == 0 {
		return
	}
	values := make([]string, 0, len(fromEnv)+1)
	for _, r := range fromEnv {
		values = append(values, out[r[0]:r[1]])
	}
	// The assembled value too: ${AUTH_A}${AUTH_B} built from fragments each
	// below the redactor's minimum literal length registers nothing piece by
	// piece, yet the whole is the credential the connector is sent.
	values = append(values, out)
	logging.RegisterSecretLiterals("", false, values...)
}

// warnUserTokenAssembled logs, once per env key, that a value assembling a
// user token from resolved fragments was dropped. No value is logged.
func warnUserTokenAssembled(key string) {
	if _, seen := userTokenEnvWarned.LoadOrStore("assembled:"+key, true); seen {
		return
	}
	slog.Warn("clientconfig: warning: MCP env/header resolves to a reserved ${FLEET_USER_EMAIL}/FLEET_USER_NAME spelling the manifest did not write (assembled from environment values); the key is dropped (see docs/MCP-BUNDLE-ENV.md)", "key", key)
}

// resolveSpawnExpr resolves one ${...} expression at catalog-build time (expr
// is the text between the braces, token the original "${...}" spelling);
// fromEnv reports that the text came from the process env (not the manifest),
// for interpolate's provenance check. It
// mirrors expandExpr with the two load/spawn differences: an unset bare ${VAR}
// resolves to "" (there is no later pass to defer to), and a reserved runtime
// token is preserved verbatim in ANY spelling rather than erroring — the load
// pass already rejected non-bare reserved forms, so this is belt-and-braces
// against a value that never went through it.
func resolveSpawnExpr(expr, token string) (text string, fromEnv bool) {
	head := expr
	if idx := strings.IndexByte(expr, ':'); idx >= 0 {
		head = expr[:idx]
	}
	if reservedRuntimeVar(strings.TrimSpace(head)) {
		return token, false
	}
	if idx := strings.IndexByte(expr, ':'); idx >= 0 && idx+1 < len(expr) {
		name := expr[:idx]
		op := expr[idx+1]
		body := expr[idx+2:]
		switch op {
		case '-': // ${VAR:-default}
			if v, ok := lookupNonEmpty(name); ok {
				return v, true
			}
			return body, false
		case '?': // ${VAR:?message}
			v, _ := lookupNonEmpty(name)
			return v, true
		}
		// Any other ':X' is not a form we support; treat the whole expression
		// as a bare name, matching expandExpr.
	}
	v, _ := lookupNonEmpty(expr)
	return v, true
}
