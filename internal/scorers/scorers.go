// Package scorers holds the DETERMINISTIC output scorers shared by the
// scheduled loop's exit conditions (#179, internal/scheduledrun/loop.go) and the
// eval & regression harness (#502, internal/evals). Extracting them here — as
// #502 asks — gives both consumers one implementation, so a loop exit-condition
// and an eval scorer can never drift in how they judge the same output.
//
// Every scorer returns (passed bool, label string). The label is a short
// machine-readable verdict ("regex:matched", "shell:exit_2", …) persisted
// verbatim as TaskIteration.ExitConditionResult by the loop and as a per-case
// scorer verdict by the eval harness — the string contract predates this
// package and must stay stable.
//
// The LLM-judge (rubric-scored, schema-validated) deliberately does NOT live
// here: it is a model call, not a deterministic function. See
// internal/evals/judge.go and the loop's evalLLMExit.
package scorers

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// BashRunner is the one sandbox capability the shell scorer needs. It is
// satisfied by *sandbox.Sandbox; tests substitute a fake so the scorer is
// unit-testable without podman. The command ALWAYS runs inside the mandatory
// sandbox — a scorer never grows a host-side shell.
type BashRunner interface {
	RunBash(ctx context.Context, req sandbox.BashRequest) (sandbox.BashResult, error)
}

// Regex reports whether pattern matches text. An invalid pattern is a fail
// (never a pass), labelled "regex:invalid" and logged — matching the loop's
// historical behavior for a malformed exit_condition.
func Regex(pattern, text string) (bool, string) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Printf("scorers: invalid regex %q: %v", pattern, err)
		return false, "regex:invalid"
	}
	if re.MatchString(text) {
		return true, "regex:matched"
	}
	return false, "regex:no_match"
}

// Contains reports whether text contains needle (case-sensitive). An empty
// needle is a configuration error, not a trivially-true scorer.
func Contains(needle, text string) (bool, string) {
	if needle == "" {
		return false, "contains:empty_needle"
	}
	if strings.Contains(text, needle) {
		return true, "contains:matched"
	}
	return false, "contains:no_match"
}

// Equals reports whether text equals want after trimming surrounding
// whitespace on both sides (a trailing newline must not fail an exact-match
// golden).
func Equals(want, text string) (bool, string) {
	if strings.TrimSpace(text) == strings.TrimSpace(want) {
		return true, "equals:matched"
	}
	return false, "equals:no_match"
}

// Shell runs cmd inside the run's sandbox and scores exit code 0 as a pass.
// The caller supplies the timeout (the loop uses 2 minutes) and is responsible
// for the nil-sandbox case — pass a non-nil runner or report "shell:no_sandbox"
// yourself, since a typed-nil pointer inside the interface would defeat a nil
// check here.
func Shell(ctx context.Context, sb BashRunner, cmd string, timeout time.Duration) (bool, string) {
	res, err := sb.RunBash(ctx, sandbox.BashRequest{Command: cmd, Timeout: timeout})
	if err != nil {
		log.Printf("scorers: shell check failed: %v", err)
		return false, "shell:error"
	}
	if res.TimedOut {
		return false, "shell:timeout"
	}
	if res.ExitCode == 0 {
		return true, "shell:passed"
	}
	return false, fmt.Sprintf("shell:exit_%d", res.ExitCode)
}

// FirstWordIsYes reports whether an LLM verifier's reply begins with YES
// (ignoring case, leading whitespace, and a leading markdown/quote/backtick
// character) — the deterministic half of a YES/NO verifier call.
func FirstWordIsYes(s string) bool {
	s = strings.TrimLeft(strings.TrimSpace(s), "*_`\"'> ")
	return strings.HasPrefix(strings.ToUpper(s), "YES")
}

// LastAssistantMessage returns the text of a session's final assistant
// message — the output a regex/contains/judge scorer evaluates and the loop
// feeds forward between iterations. nil-safe.
func LastAssistantMessage(session *models.LogSession) string {
	if session == nil {
		return ""
	}
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == "assistant" {
			return session.Messages[i].Content
		}
	}
	return ""
}
