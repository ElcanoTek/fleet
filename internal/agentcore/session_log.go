package agentcore

import (
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Session log (lifted from cutlass session_log.go).
//
// The structured JSON session log is the scheduled mode's "captain's-log"
// observer substrate (Observer.Observe writes through it) and the accumulator
// the resilience/orchestration layers report token + cost usage into. The chat
// (interactive) Observer streams SSE instead and uses only the usage counters.
// The full file-write / truncation machinery is a P3 Observer concern; what
// lives here is the in-memory model + the redaction helper the parity tests and
// the retry logger exercise.

// Roles + message-type tags written into the log.
const (
	roleUser      = "user"
	roleTool      = "tool"
	roleAssistant = "assistant"

	messageTypeSystemEnforcement = "system_enforcement"
	messageTypeSystemCompaction  = "system_compaction"
	messageTypeSystemRetry       = "system_retry"

	statusUnknown = "unknown"
)

// secretPattern matches common secret patterns in free text
// (KEY=value, TOKEN: value, Bearer token, …).
var secretPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password|authorization)[:\s=]+(?:bearer\s+)?)([^\s"',}{]{8,})`)

// redactSecrets replaces secret values in text with [REDACTED].
func redactSecrets(text string) string {
	return secretPattern.ReplaceAllString(text, "${1}[REDACTED]")
}

// RedactSecrets is the exported form the scheduled driver's log writer uses to
// scrub recoverable secrets before persisting the session log.
func RedactSecrets(text string) string {
	return redactSecrets(text)
}

// LogSession tracks the execution session for logging.
//
// Token semantics:
//   - PromptTokens / CompletionTokens / CachedTokens / CacheCreationTokens are
//     CUMULATIVE across every API call in the session. They are billing/display
//     numbers; do not use them to reason about the size of the next API call.
//   - LastStepPromptTokens is OVERWRITTEN on every call with that call's input
//     size (fresh input + cache-read input). This is the value compaction
//     compares against the model's context window — cumulative growth must NOT
//     drive compaction or the trigger ratchets up every step into a spiral.
type LogSession struct {
	mu                   sync.Mutex   `json:"-"`
	ID                   string       `json:"id"`
	Title                string       `json:"title"`
	PromptTokens         int          `json:"prompt_tokens"`
	CompletionTokens     int          `json:"completion_tokens"`
	CachedTokens         int          `json:"cached_tokens,omitempty"`
	CacheCreationTokens  int          `json:"cache_creation_tokens,omitempty"`
	LastStepPromptTokens int          `json:"last_step_prompt_tokens,omitempty"`
	Cost                 float64      `json:"cost"`
	CreatedAt            int64        `json:"created_at"`
	UpdatedAt            int64        `json:"updated_at"`
	Messages             []LogMessage `json:"messages"`
}

// LogToolCall represents a structured tool call in logs.
type LogToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LogMessage represents a single message in the session.
type LogMessage struct {
	ID          string        `json:"id"`
	Role        string        `json:"role"`
	Content     string        `json:"content"`
	Reasoning   string        `json:"reasoning,omitempty"`
	Model       *string       `json:"model,omitempty"`
	Provider    *string       `json:"provider,omitempty"`
	CreatedAt   int64         `json:"created_at"`
	FinishedAt  *int64        `json:"finished_at,omitempty"`
	MessageType *string       `json:"message_type,omitempty"`
	ToolCalls   []LogToolCall `json:"tool_calls,omitempty"`
	ToolCallID  *string       `json:"tool_call_id,omitempty"`
}

// SnapshotMessages returns a copy of the session's messages taken under lock,
// so callers in other packages (e.g. the scheduled driver's verifier) can scan
// the log without touching the unexported mutex.
func (ls *LogSession) SnapshotMessages() []LogMessage {
	if ls == nil {
		return nil
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]LogMessage, len(ls.Messages))
	copy(out, ls.Messages)
	return out
}

// CumulativeCacheHitRate returns the session-wide cache hit rate as a percentage.
func (ls *LogSession) CumulativeCacheHitRate() float64 {
	if ls.PromptTokens <= 0 {
		return 0
	}
	return float64(ls.CachedTokens) / float64(ls.PromptTokens) * 100.0
}

// NewLogSession creates a new log session.
func NewLogSession() *LogSession {
	now := time.Now().Unix()
	return &LogSession{
		ID:        fmt.Sprintf("session-%d", now),
		Title:     "Task Execution",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  make([]LogMessage, 0),
	}
}

// AddMessage adds a message to the log session.
func (ls *LogSession) AddMessage(role, content string, model, provider *string) {
	ls.AddMessageWithMetadata(role, content, model, provider, nil, nil, nil, "")
}

// AddMessageWithMetadata adds a message with enhanced metadata to the log
// session.
func (ls *LogSession) AddMessageWithMetadata(role, content string, model, provider *string, messageType *string, toolCalls []LogToolCall, toolCallID *string, reasoning string) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := time.Now().Unix()
	msg := LogMessage{
		ID:          fmt.Sprintf("msg-%d-%d", now, len(ls.Messages)),
		Role:        role,
		Content:     content,
		Reasoning:   reasoning,
		Model:       model,
		Provider:    provider,
		CreatedAt:   now,
		MessageType: messageType,
		ToolCalls:   toolCalls,
		ToolCallID:  toolCallID,
	}
	ls.Messages = append(ls.Messages, msg)
	ls.UpdatedAt = now
}
