package agentcore

import (
	"log"
	"sync"

	"github.com/ElcanoTek/fleet/internal/piiredact"
)

// PII redaction (#450) is an OPTIONAL, provider-neutral pass layered on top of
// the unconditional secret scrubber (toolRedactor). It runs at the SAME choke
// point — tool OUTPUT, where external data first enters the model context — so
// it inherits the secret scrubber's cache-safe, structure-safe placement (plain
// text only; never the cacheable system-prompt prefix or tool-call JSON args).
//
// It is DEFAULT OFF: cmd/fleet installs a redactor at boot only when
// FLEET_PII_REDACTION_ENABLED is set. A nil redactor makes redactPII a
// pass-through no-op, so an unconfigured deployment is byte-for-byte unchanged.
// The Redactor INTERFACE lets an external ONNX/HTTP classifier (Rampart) replace
// the built-in deterministic PatternRedactor as a follow-on with no call-site
// change here.
var (
	piiMu       sync.RWMutex
	piiRedactor piiredact.Redactor
)

// SetPIIRedactor installs (nil clears) the process-wide PII redactor. Called
// once at boot before any run; safe for concurrent reads afterward.
func SetPIIRedactor(r piiredact.Redactor) {
	piiMu.Lock()
	defer piiMu.Unlock()
	piiRedactor = r
}

func currentPIIRedactor() piiredact.Redactor {
	piiMu.RLock()
	defer piiMu.RUnlock()
	return piiRedactor
}

// redactPII applies the optional PII pass to tool output. It returns the
// possibly-redacted (or, in block mode, withheld) text and whether the content
// was BLOCKED, so the caller can flag the tool result as an error. A nil/off
// redactor is a pass-through no-op. Findings are audit-logged by TYPE and COUNT
// only — never the raw value (surfacing it would defeat the redaction).
func redactPII(toolName, text string) (out string, blocked bool) {
	r := currentPIIRedactor()
	if r == nil || text == "" {
		return text, false
	}
	res := r.Redact(text)
	if res.Found() {
		log.Printf("piiredact: %s output — %s (mode=%s)", toolName, res.Summary(), r.Mode())
	}
	return res.Text, res.Blocked
}
