package mcpbroker

import (
	"log"
	"os"
	"sync"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// logMasked records host-side WHY a broker reply was masked, then leaves the
// masking itself alone.
//
// Every operational failure the credential owner hits is replaced with a fixed
// errBroker* string before it crosses back to the parent, because the real error
// can embed connector stderr, resolved URLs, or Authorization headers. That is
// correct — but it used to be the ONLY thing that happened to the error, so the
// detail existed nowhere: not in the parent, not in this process, not for the
// operator. A connector returning "Unknown tool: x" and a revoked credential
// both surfaced to the agent as the same sentence, and the difference was
// unrecoverable. Diagnosing it meant reading the UPSTREAM's logs, if it had any.
//
// The error is scrubbed before it reaches the log, not merely trusted to be
// clean: internal/logging's slog handler only blanks attributes whose KEY looks
// secret, and fleet's convention here is stdlib log.Printf, whose message text
// that handler does not touch. brokerRedactor's env literals matter most — this
// process is the one that holds the connector credentials, so the exact bearer
// token that might appear inside a failed request URL is registered by value and
// replaced even when its shape matches no known vendor pattern.
//
// Credentials-never-in-logs (AGENTS.md) is therefore still honored: what lands
// in the log is the redacted shape of the failure, which is what an operator
// needs, and not the secret that failure may have quoted.
func logMasked(op, detail string, err error) {
	if err == nil {
		return
	}
	scrubbed := brokerRedactor().Redact(err.Error())
	if detail == "" {
		log.Printf("mcpbroker: %s failed (masked to the caller): %s", op, scrubbed)
		return
	}
	log.Printf("mcpbroker: %s failed (masked to the caller): %s: %s", op, detail, scrubbed)
}

// RegisterSecretLiteral adds a runtime-acquired secret — a per-user OAuth
// bearer, a rotated refresh token, a sealed connector API key — to the
// broker's literal redactor (#1124). Boot-time RegisterEnvLiterals can only
// know env-file secrets; tokens minted or refreshed while the broker is
// serving were previously scrubbed by shape patterns alone, so a connector
// echoing its own bare token into an error string would have leaked it into
// the host log. Safe for concurrent use (the Redactor guards its literal set);
// duplicates are ignored, so re-registering on every acquisition is free.
// The value is retained only inside the redactor and is never logged.
func RegisterSecretLiteral(value string) {
	brokerRedactor().AddLiteral(value)
}

// brokerRedactor returns the process-wide scrubber for masked-error logging: the
// canonical pattern set plus literal redaction of secret-named env values, which
// in the credential owner are the connector credentials themselves. Mirrors
// agentcore.toolRedactor; built once so every caller — boot-time env
// registration, runtime RegisterSecretLiteral, and logMasked's Redact — shares
// one literal set (AddLiteral/Redact are concurrency-safe, #1124).
func brokerRedactor() *redact.Redactor {
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
