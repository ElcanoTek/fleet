package agentcore

import (
	"os"
	"sync"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// toolRedactor returns the process-wide secret scrubber applied to tool output
// (in the tool wrappers + stream sink) and to the persisted session log. Built
// once: the canonical pattern set plus literal redaction of secret-named env
// values so a novel key format is still scrubbed by value. See internal/redact.
//
// NOTE ON WHAT THE ENV SNAPSHOT DOES AND DOES NOT COVER. This is lazy — the
// Once fires on the first tool output — and by then the parent has already
// divested its connector credentials: the MCP broker's boot path
// os.Unsetenv's every connector environment key (scrubParentConnectorState)
// long before any turn runs. So os.Environ() here no longer contains connector
// values, and this call alone registers only what survives the scrub, e.g.
// OPENROUTER_API_KEY.
//
// That divestment is the point of the broker boundary and is not being undone.
// But it also meant the literal set was EMPTY of connector secrets, so a
// connector echoing its own credential back in a tool result was caught only if
// the value happened to match one of internal/redact's shape patterns (sk-*,
// ghp_*, AKIA*, …) — a novel bare token would have reached the model context,
// the SSE stream and the session log. RegisterSecretLiteral below is how the
// boot path hands those values over BEFORE unsetting them, so defense-in-depth
// against an upstream echoing a credential does not depend on its format.
func toolRedactor() *redact.Redactor {
	redactorOnce.Do(func() {
		r := redact.NewRedactor(nil)
		r.RegisterEnvLiterals(os.Environ())
		// Publish under pendingMu, and drain under the same lock: that is what
		// makes RegisterSecretLiteral's nil-check safe against a racing
		// construction, so a value offered concurrently is either buffered here
		// and drained, or added directly — never dropped between the two.
		pendingMu.Lock()
		for _, p := range pendingLiterals {
			r.RegisterSecrets(p.scope, p.rotated, p.values...)
		}
		pendingLiterals = nil
		sharedRedactor = r
		pendingMu.Unlock()
	})
	// Safe unlocked: sync.Once establishes happens-before for every caller that
	// returns from Do, so the write above is visible here.
	return sharedRedactor
}

// RegisterSecretLiteral adds one secret VALUE to the process-wide tool-output
// scrubber, so it is redacted by exact match regardless of format.
//
// Call this with a value that is about to become unreachable — the boot path
// uses it for each connector credential immediately before os.Unsetenv removes
// it from the environment. Safe before or after the redactor is built: earlier
// calls are buffered and drained at construction, later ones go straight in
// (redact.Redactor.AddLiteral is mutex-guarded). Values shorter than the
// redactor's floor are ignored by AddLiteral, so a short or empty setting cannot
// turn the scrubber into a match-everything.
//
// This never widens what the parent can READ — the value is stored only as a
// scrub target and is never emitted. It is not a substitute for the broker
// boundary; it is the backstop for output that comes back from the other side of
// it.
func RegisterSecretLiteral(value string) {
	RegisterSecretLiterals("", false, value)
}

// RegisterSecretLiterals is the scope-aware form, and the function the
// hosted-MCP control plane's runtime secret observer is wired to (#1274).
//
// scope names a ROTATING credential set — one hosted-MCP server row, whose
// OAuth access+refresh pair is replaced wholesale on every refresh; rotated
// says these values REPLACE that scope's previous generation, which the
// redactor then retires after its grace window instead of keeping forever. An
// empty scope means "does not rotate on a clock" and registers permanently,
// exactly like RegisterSecretLiteral. Everything RegisterSecretLiteral
// promises still holds: values are kept only as scrub targets, never emitted,
// and calls before the redactor exists are buffered and drained at
// construction (in order, so a rotation cannot be replayed out of sequence).
func RegisterSecretLiterals(scope string, rotated bool, values ...string) {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 && !rotated {
		return
	}
	pendingMu.Lock()
	if sharedRedactor == nil {
		pendingLiterals = append(pendingLiterals, pendingLiteral{scope: scope, rotated: rotated, values: kept})
		pendingMu.Unlock()
		return
	}
	pendingMu.Unlock()
	sharedRedactor.RegisterSecrets(scope, rotated, kept...)
}

// pendingLiteral is one buffered registration (see RegisterSecretLiterals):
// the rotation semantics are order-sensitive, so the buffer keeps whole calls
// rather than a flat value list.
type pendingLiteral struct {
	scope   string
	rotated bool
	values  []string
}

var (
	redactorOnce   sync.Once
	sharedRedactor *redact.Redactor

	// pendingMu guards literals registered before the redactor is built. It
	// also guards the sharedRedactor nil-check in RegisterSecretLiterals so a
	// value cannot be dropped by racing construction.
	pendingMu       sync.Mutex
	pendingLiterals []pendingLiteral
)
