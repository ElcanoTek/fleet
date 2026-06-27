package agentcore

import (
	"os"
	"sync"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// toolRedactor returns the process-wide secret scrubber applied to tool output
// (in the tool wrappers + stream sink) and to the persisted session log. Built
// once: the canonical pattern set plus literal redaction of secret-named env
// values (OPENROUTER_API_KEY, connector credentials, …) so a novel key format
// is still scrubbed by value. See internal/redact.
func toolRedactor() *redact.Redactor {
	redactorOnce.Do(func() {
		r := redact.NewRedactor(nil)
		r.RegisterEnvLiterals(os.Environ())
		sharedRedactor = r
	})
	return sharedRedactor
}

var (
	redactorOnce   sync.Once
	sharedRedactor *redact.Redactor
)
