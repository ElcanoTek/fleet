package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// Scheduled-mode session log (the captain's-log JSON substrate). The in-memory
// model + usage accumulators live in agentcore (shared with the resilience /
// orchestration layers); this file re-exports those types into the agent
// package under the names cutlass's tests use and adds the scheduled Observer's
// file-write / truncation / redaction machinery that agentcore deliberately
// left to the driver (it depends on a file path + size cap, not just the
// in-memory model).

// LogSession / LogMessage / LogToolCall are the agentcore session-log types,
// re-exported so the scheduled driver + its ported tests refer to them in the
// agent package (cutlass kept them here).
type (
	// LogSession is the structured session log (alias of agentcore.LogSession).
	LogSession = agentcore.LogSession
	// LogMessage is one logged message (alias of agentcore.LogMessage).
	LogMessage = agentcore.LogMessage
	// LogToolCall is a structured tool call in the log (alias).
	LogToolCall = agentcore.LogToolCall
)

// NewLogSession constructs a fresh session log.
func NewLogSession() *LogSession { return agentcore.NewLogSession() }

// Roles written into the session log (mirrors agentcore's unexported set).
const (
	roleUser      = "user"
	roleTool      = "tool"
	roleAssistant = "assistant"
)

// session-log file sizing (cutlass values).
const (
	maxSessionLogFileSize      = 10 * 1024 * 1024
	maxSessionLogFileSizeLabel = "10 MB"
)

// marshalLogSession renders a LogSession to indented JSON.
func marshalLogSession(session *LogSession) ([]byte, error) {
	return json.MarshalIndent(session, "", "  ")
}

// validateLogSessionJSON confirms data round-trips as a LogSession (so a
// truncation bug can never write a half-object that breaks downstream readers).
func validateLogSessionJSON(data []byte) error {
	var probe LogSession
	return json.Unmarshal(data, &probe)
}

// writeValidatedLogFile writes data to path only after validating it parses as
// a LogSession. The file is 0600 — session logs may carry redacted-but-still-
// sensitive context.
func writeValidatedLogFile(path string, data []byte) error {
	if err := validateLogSessionJSON(data); err != nil {
		return fmt.Errorf("refusing to write invalid session-log JSON: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// logAuxUsage emits the one-line host log for an auxiliary model call (#1118).
// It is the visibility floor: ledger-backed calls (verifier/reviewer) log it in
// addition to their session-log record, and calls whose run session is no
// longer reachable (error analysis, recurring-task synthesis) have only this
// line — see docs/AUX-MODEL-CALL-METERING.md.
func logAuxUsage(rec agentcore.AuxUsageRecord) {
	log.Printf("aux model call: label=%s model=%s prompt=%d completion=%d cost=$%.4f",
		rec.Label, rec.Model, rec.PromptTokens, rec.CompletionTokens, rec.CostUSD)
}

// oneShotCall is one auxiliary, tool-less model call: a system prompt, a single
// user message, and the call's sampling, output and time budgets. AuxLabel is
// the agentcore.AuxUsage* label the call is metered under via logAuxUsage; ""
// leaves it unmetered (docs/AUX-MODEL-CALL-METERING.md lists which are).
type oneShotCall struct {
	System      string
	Prompt      string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
	AuxLabel    string
}

// generateOnce runs call against model the way every short-lived host-side
// helper (titles, memory extraction, error analysis, the synthesizers) does: a
// fresh fantasy agent with the stable-prefix prompt-caching step and the
// routing provider options, bounded by call.Timeout. A metered call is logged
// before the caller validates anything, because a non-conforming answer still
// cost money. text concatenates every text part of the response; res is the
// raw result, nil on error.
func generateOnce(ctx context.Context, model fantasy.LanguageModel, call oneShotCall) (text string, res *fantasy.AgentResult, err error) {
	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(call.System),
		fantasy.WithTemperature(call.Temperature),
		fantasy.WithMaxOutputTokens(int64(call.MaxTokens)),
		fantasy.WithPrepareStep(agentcore.PromptCachingStep(model.Model(), agentcore.WithStablePrefixOnly())),
	)
	ctx, cancel := context.WithTimeout(ctx, call.Timeout)
	defer cancel()

	maxTokens := int64(call.MaxTokens)
	res, err = ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(call.Prompt)},
		MaxOutputTokens: &maxTokens,
		ProviderOptions: agentcore.RoutingProviderOptions(model.Model()),
	})
	if err != nil {
		return "", nil, err
	}
	if call.AuxLabel != "" {
		logAuxUsage(agentcore.NewAuxUsageRecord(call.AuxLabel, model.Model(), res))
	}
	var out strings.Builder
	for _, c := range res.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return out.String(), res, nil
}

// redactLogSession returns a copy of session with secrets scrubbed from text,
// reasoning, and tool-call arguments.
func redactLogSession(session *LogSession) *LogSession {
	if session == nil {
		return nil
	}
	msgs := session.SnapshotMessages()
	redacted := &LogSession{
		ID:                  session.ID,
		Title:               agentcore.RedactSecrets(session.Title),
		ParentTaskID:        session.ParentTaskID,
		PromptTokens:        session.PromptTokens,
		CompletionTokens:    session.CompletionTokens,
		CachedTokens:        session.CachedTokens,
		CacheCreationTokens: session.CacheCreationTokens,
		Cost:                session.Cost,
		CreatedAt:           session.CreatedAt,
		UpdatedAt:           session.UpdatedAt,
		Messages:            make([]LogMessage, len(msgs)),
		// Aux-usage ledger (#1118): carried as-is — records hold only
		// label/model/token counts/cost, nothing redactable.
		AuxUsage: session.SnapshotAuxUsage(),
	}
	for i, msg := range msgs {
		msgCopy := msg
		msgCopy.Content = agentcore.RedactSecrets(msgCopy.Content)
		msgCopy.Reasoning = agentcore.RedactSecrets(msgCopy.Reasoning)
		if len(msgCopy.ToolCalls) > 0 {
			msgCopy.ToolCalls = append([]LogToolCall(nil), msgCopy.ToolCalls...)
			for j := range msgCopy.ToolCalls {
				msgCopy.ToolCalls[j].Arguments = agentcore.RedactSecrets(msgCopy.ToolCalls[j].Arguments)
			}
		}
		redacted.Messages[i] = msgCopy
	}
	return redacted
}

// truncateLogSession drops middle messages until the marshaled session fits
// maxSize, keeping the first message + a truncation notice + as many trailing
// messages as fit (cutlass's binary-search-from-the-end strategy).
func truncateLogSession(session *LogSession, maxSize int) []byte {
	original := session.SnapshotMessages()
	if len(original) <= 2 {
		data, _ := marshalLogSession(session)
		return data
	}

	// Aux-usage records (#1118) survive truncation: they are tiny fixed-size
	// accounting rows, not transcript bulk, and dropping them would silently
	// unmeter the run's overhead exactly on the biggest (truncated) runs.
	auxUsage := session.SnapshotAuxUsage()
	clone := func(msgs []LogMessage) *LogSession {
		return &LogSession{
			ID:                  session.ID,
			Title:               session.Title,
			ParentTaskID:        session.ParentTaskID,
			PromptTokens:        session.PromptTokens,
			CompletionTokens:    session.CompletionTokens,
			CachedTokens:        session.CachedTokens,
			CacheCreationTokens: session.CacheCreationTokens,
			Cost:                session.Cost,
			CreatedAt:           session.CreatedAt,
			UpdatedAt:           session.UpdatedAt,
			Messages:            msgs,
			AuxUsage:            auxUsage,
		}
	}

	for endCount := len(original) - 1; endCount > 0; endCount-- {
		msgs := make([]LogMessage, 0, endCount+2)
		msgs = append(msgs, original[0])
		if endCount < len(original)-1 {
			skipped := len(original) - endCount - 1
			msgs = append(msgs, LogMessage{
				ID:        fmt.Sprintf("msg-truncated-%d", time.Now().Unix()),
				Role:      "system",
				Content:   fmt.Sprintf("[Log truncated: %d messages omitted to stay within %s limit]", skipped, maxSessionLogFileSizeLabel),
				CreatedAt: time.Now().Unix(),
			})
		}
		msgs = append(msgs, original[len(original)-endCount:]...)
		data, err := marshalLogSession(clone(msgs))
		if err != nil {
			continue
		}
		if len(data) <= maxSize {
			log.Printf("Truncated log to %d messages (kept first + last %d, skipped %d)",
				len(msgs), endCount, len(original)-endCount-1)
			return data
		}
	}

	minimal := clone([]LogMessage{
		original[0],
		{
			ID:        fmt.Sprintf("msg-truncated-%d", time.Now().Unix()),
			Role:      "system",
			Content:   fmt.Sprintf("[Log truncated: %d messages omitted to stay within %s limit]", len(original)-2, maxSessionLogFileSizeLabel),
			CreatedAt: time.Now().Unix(),
		},
		original[len(original)-1],
	})
	data, _ := marshalLogSession(minimal)
	log.Printf("Truncated log to minimal (first + last message only, skipped %d)", len(original)-2)
	return data
}

// writeLogFile persists session to logFile (defaulting to FLEET_LOG_FILE or
// fleet-session.json), redacting secrets and truncating to the size cap. All
// failures are logged but non-fatal — a successful run must not be turned into
// a failure because the side-channel log could not be written.
func writeLogFile(session *LogSession, logFile string) {
	if logFile == "" {
		logFile = envreg.Get("FLEET_LOG_FILE")
		if logFile == "" {
			logFile = envreg.Get("CUTLASS_LOG_FILE")
		}
		if logFile == "" {
			logFile = "fleet-session.json"
		}
	}
	redacted := redactLogSession(session)
	data, err := marshalLogSession(redacted)
	if err != nil {
		log.Printf("Warning: Failed to marshal log session: %v", err)
		return
	}
	if len(data) > maxSessionLogFileSize {
		log.Printf("Log size (%d bytes) exceeds limit (%d bytes), truncating middle messages...", len(data), maxSessionLogFileSize)
		data = truncateLogSession(redacted, maxSessionLogFileSize)
	}
	if err := writeValidatedLogFile(logFile, data); err != nil {
		log.Printf("Warning: Failed to write log file: %v", err)
		return
	}
	log.Printf("Session summary: %d prompt + %d completion tokens, $%.4f total cost, cache: %d read / %d created (%.1f%% hit rate)",
		session.PromptTokens, session.CompletionTokens, session.Cost,
		session.CachedTokens, session.CacheCreationTokens, session.CumulativeCacheHitRate())
	log.Printf("Session log written to: %q (%d bytes)", logFile, len(data))
}

// summarizeForConsole clamps text to a single-line preview (verifier logging).
func summarizeForConsole(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "<empty>"
	}
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	if maxLen < 32 {
		maxLen = 32
	}
	return text[:maxLen-3] + "..."
}
