package httpapi

import (
	"encoding/json"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/redact"
	"github.com/ElcanoTek/fleet/internal/store"
)

// auditSummaryMaxBytes caps the redacted args/result text stored per audit row.
// The ledger is for "what ran + did it error", not a full transcript (that lives
// in turn_events / messages); a generous cap keeps a row useful for triage while
// bounding table growth on a chatty turn.
const auditSummaryMaxBytes = 4000

// auditRedactor is the secret scrubber applied to tool ARGS before they enter the
// audit ledger. Tool RESULTS are already redacted upstream (streamSink.onToolResult
// runs the shared redactor before the result is recorded/persisted), but tool args
// flow verbatim from the model and have no prior redaction pass — so they MUST be
// scrubbed here to honor the host-side-credentials invariant: a user pasting an API
// key into a prompt that the model echoes into a tool call must never land as
// plaintext in this table. Built once; Redact is concurrency-safe after construction.
var auditRedactor = sync.OnceValue(func() *redact.Redactor { return redact.NewRedactor(nil) })

// deriveToolCallEntries pairs the turn's tool_call history entries with their
// matching tool_result entries (by tool-call id) into audit-ledger rows. It taps
// the EXISTING accumulated history (res.NewHistory) rather than adding a new
// instrumentation point in the hot loop: every tool the turn ran is already
// recorded there as a tool_call entry (args) followed by a tool_result entry
// (outcome).
//
// A tool_call with no matching result (an interrupted/cancelled turn) still
// produces a row, flagged IsError with an empty result and no duration — so the
// ledger never silently drops a call that started.
//
// startedAt is the turn's start time; per-call timing is not available from the
// history (the SDK does not propagate per-call token/latency, see the issue's
// out-of-scope note), so DurationMS is left nil here. The column exists so a
// later change that does thread per-call timing can populate it without a
// migration.
func deriveToolCallEntries(history []agent.HistoryEntry, convID, turnID, userEmail string, startedAt int64) []store.ToolCallEntry {
	// First pass: collect tool_result text/outcome keyed by tool-call id.
	type result struct {
		text  string
		isErr bool
		seen  bool
	}
	results := make(map[string]result)
	for _, h := range history {
		if h.Role == "tool" && h.Type == "tool_result" {
			var c agent.ToolResultContent
			if err := json.Unmarshal(h.Content, &c); err != nil {
				continue
			}
			results[c.ID] = result{text: c.Text, isErr: c.IsErr, seen: true}
		}
	}

	// Second pass: one entry per tool_call, in call order.
	var entries []store.ToolCallEntry
	for _, h := range history {
		if h.Role != "assistant" || h.Type != "tool_call" {
			continue
		}
		var c agent.ToolCallContent
		if err := json.Unmarshal(h.Content, &c); err != nil {
			continue
		}
		entry := store.ToolCallEntry{
			ConversationID: convID,
			TurnID:         turnID,
			UserEmail:      userEmail,
			ToolName:       c.Name,
			// Args flow verbatim from the model — scrub before storing.
			ArgsSummary: capBytes(auditRedactor().Redact(c.Input), auditSummaryMaxBytes),
			StartedAt:   startedAt,
		}
		if r, ok := results[c.ID]; ok && r.seen {
			// Result text is already redacted upstream; re-run defensively
			// (Redact is idempotent) so a pre-gated tool that bypassed the
			// stream redactor can't leak here either.
			entry.ResultSummary = capBytes(auditRedactor().Redact(r.text), auditSummaryMaxBytes)
			entry.IsError = r.isErr
		} else {
			// Call started but never produced a result — interrupted/cancelled.
			entry.IsError = true
		}
		entries = append(entries, entry)
	}
	return entries
}

// capBytes truncates s to at most n bytes, backing off to the previous rune
// boundary so the result stays valid UTF-8 (otherwise a multi-byte rune split at
// the cap would produce invalid bytes that the JSON response encoder mangles).
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Walk back to a UTF-8 start byte (0xxxxxxx or 11xxxxxx, i.e. not a 10xxxxxx
	// continuation byte) so we never cut a rune in half.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
