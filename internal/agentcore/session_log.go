package agentcore

import (
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"
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

// RedactSecrets scrubs recoverable secrets from text before it is persisted to
// the session log or re-enters the model context. Backed by the shared
// internal/redact Redactor (see redact.go) — the same scrubber the tool wrappers
// and stream sink apply — so the scheduled and interactive paths agree. It now
// covers vendor key prefixes (sk-/sk-ant-/sk-or-/ghp_/glpat-/AKIA), PEM blocks,
// the JSON-quoted marker form ({"api_key":"…"}), and registered env-secret
// literals, not just the old marker=value regex.
func RedactSecrets(text string) string {
	return toolRedactor().Redact(text)
}

// LogSession tracks the execution session for logging.
//
// Token semantics:
//   - PromptTokens / CompletionTokens / CachedTokens / CacheCreationTokens are
//     CUMULATIVE across every API call in the session. They are billing/display
//     numbers; do not use them to reason about the size of the next API call.
//     PromptTokens INCLUDES cache reads and CachedTokens is that cached subset
//     (so CumulativeCacheHitRate ≤ 100% and "PromptTokens - CachedTokens" is
//     the uncached spend checkCeilings/budgetState charge against ceilings) —
//     updateUsage normalizes every provider's reporting onto this convention.
//   - LastStepPromptTokens is OVERWRITTEN on every call with that call's input
//     size (fresh input + cache-read input). This is the value compaction
//     compares against the model's context window — cumulative growth must NOT
//     drive compaction or the trigger ratchets up every step into a spiral.
type LogSession struct {
	mu    sync.Mutex `json:"-"`
	ID    string     `json:"id"`
	Title string     `json:"title"`
	// ParentTaskID links a sub-agent's child run back to the scheduled task that
	// spawned it (#264) for traceability. Empty for an ordinary (root) run, so it
	// is omitted from the JSON and a non-delegating run's log is byte-for-byte
	// unchanged. Set by the spawn_subagent tool on the child's session.
	ParentTaskID         string       `json:"parent_task_id,omitempty"`
	PromptTokens         int          `json:"prompt_tokens"`
	CompletionTokens     int          `json:"completion_tokens"`
	CachedTokens         int          `json:"cached_tokens,omitempty"`
	CacheCreationTokens  int          `json:"cache_creation_tokens,omitempty"`
	LastStepPromptTokens int          `json:"last_step_prompt_tokens,omitempty"`
	Cost                 float64      `json:"cost"`
	CreatedAt            int64        `json:"created_at"`
	UpdatedAt            int64        `json:"updated_at"`
	Messages             []LogMessage `json:"messages"`
	// OutputJSON is the schema-validated terminal structured output (#797),
	// set by the scheduled driver from Result.OutputJSON. It is the exact
	// bytes agentcore validated — the runner commits THESE (post-redaction),
	// never a re-parse of the redacted final message text.
	OutputJSON string `json:"output_json,omitempty"`
	// AuxUsage is the labeled ledger of host-side auxiliary model calls made on
	// behalf of the run but OUTSIDE its governed loop's step accounting (#1118):
	// the end-of-run verifier, the phone-a-friend reviewer, and the scheduled
	// loop's llm exit-condition verifier. These records are deliberately NOT
	// added to the headline PromptTokens/CompletionTokens/Cost totals and do
	// NOT debit the run's cost/token ceilings — the loop verifier's accounting
	// model (#179) explicitly excludes them from the across-iteration
	// MaxCostUSD ceiling, and the verifier/reviewer are documented host-side
	// extras layered AROUND the run — but they must not vanish either: this is
	// where that spend is visible, per call, with a distinguishing label.
	// omitempty keeps a run with no aux calls byte-identical to before.
	AuxUsage []AuxUsageRecord `json:"aux_usage,omitempty"`
}

// Labels for the AuxUsage ledger (#1118), exported so the recording call sites
// and any reader agree on the vocabulary.
const (
	// AuxUsageEndOfRunVerifier is the scheduled end-of-run completeness check
	// (internal/agent/verifier.go).
	AuxUsageEndOfRunVerifier = "end_of_run_verifier"
	// AuxUsagePhoneAFriend is the optional super-LLM quality review
	// (internal/agent/reviewer.go).
	AuxUsagePhoneAFriend = "phone_a_friend_review"
	// AuxUsageLoopExitVerifier is the scheduled loop's llm exit-condition
	// YES/NO verifier (internal/scheduledrun/loop.go).
	AuxUsageLoopExitVerifier = "loop_exit_verifier"
	// AuxUsageErrorAnalysis is the post-failure diagnosis of one terminal task
	// failure (#317, internal/agent/erroranalysis.go). Log-line only: it runs
	// after the failed run's session was persisted, so there is no live ledger
	// to append to (see docs/AUX-MODEL-CALL-METERING.md).
	AuxUsageErrorAnalysis = "error_analysis"
	// AuxUsageRecurringTaskSynthesis is the chat→recurring-task proposal
	// synthesizer (#455, internal/agent/recurring_task.go). Log-line only: it
	// is a conversation-level user action with no run session at all.
	AuxUsageRecurringTaskSynthesis = "recurring_task_synthesis"
	// AuxUsageLibraryPromptSynthesis is the chat→workflow-template
	// synthesizer (internal/agent/library_prompt.go). Same shape as the
	// recurring-task one next door: a conversation-level user action with no
	// run session, so the log line is the whole record.
	AuxUsageLibraryPromptSynthesis = "library_prompt_synthesis"
)

// AuxUsageRecord is one host-side auxiliary model call's metered spend. Token
// semantics follow the LogSession convention: PromptTokens includes cache
// reads.
type AuxUsageRecord struct {
	Label            string  `json:"label"`
	Model            string  `json:"model,omitempty"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// NewAuxUsageRecord prices one host-side auxiliary Generate call (a single
// tool-less completion — the verifier/reviewer shape) into an AuxUsageRecord.
// Cost resolves through the SAME pricing policy the run loop uses
// (ResolveStepCost), so a per-model override (#297) applies to aux calls too;
// with no overrides it is the OpenRouter-returned cost.
func NewAuxUsageRecord(label, modelSlug string, res *fantasy.AgentResult) AuxUsageRecord {
	rec := AuxUsageRecord{Label: label, Model: modelSlug}
	if res == nil {
		return rec
	}
	rec.PromptTokens = int(res.TotalUsage.InputTokens + res.TotalUsage.CacheReadTokens)
	rec.CompletionTokens = int(res.TotalUsage.OutputTokens)
	rec.CostUSD = ResolveStepCost(modelSlug, res.TotalUsage, openrouterCost(res.Response.ProviderMetadata))
	return rec
}

// AddAuxUsage appends one auxiliary model call's record to the session's
// labeled overhead ledger (#1118). See LogSession.AuxUsage for why these are
// kept out of the headline totals.
func (ls *LogSession) AddAuxUsage(rec AuxUsageRecord) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.AuxUsage = append(ls.AuxUsage, rec)
	ls.UpdatedAt = time.Now().Unix()
}

// SnapshotAuxUsage returns a copy of the aux-usage ledger taken under lock.
func (ls *LogSession) SnapshotAuxUsage() []AuxUsageRecord {
	if ls == nil {
		return nil
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]AuxUsageRecord, len(ls.AuxUsage))
	copy(out, ls.AuxUsage)
	return out
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
	ToolName    string        `json:"tool_name,omitempty"`
	IsError     bool          `json:"is_error,omitempty"`
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

// SetOutputJSON records the validated terminal structured output (#797).
func (ls *LogSession) SetOutputJSON(v string) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.OutputJSON = v
}

// SnapshotOutputJSON returns the validated terminal structured output.
func (ls *LogSession) SnapshotOutputJSON() string {
	if ls == nil {
		return ""
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.OutputJSON
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

// AddToolCall records the structured assistant invocation in the scheduled
// session log. Keeping this separate from the text accumulator makes persisted
// logs replayable after the short-lived live SSE buffer expires.
func (ls *LogSession) AddToolCall(id, name, arguments string) {
	ls.AddMessageWithMetadata(roleAssistant, "", nil, nil, nil, []LogToolCall{{
		ID: id, Name: name, Arguments: arguments,
	}}, nil, "")
}

// AddToolResult records the matching tool response, including its explicit
// error bit. Error text is not reliably self-describing (the Fast.io schema
// failure was plain "invalid arguments: ..."), so inferring status from content
// would make failed calls look successful to stored-log replay and verification.
func (ls *LogSession) AddToolResult(id, name, content string, isError bool) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := time.Now().Unix()
	ls.Messages = append(ls.Messages, LogMessage{
		ID:         fmt.Sprintf("msg-%d-%d", now, len(ls.Messages)),
		Role:       roleTool,
		Content:    content,
		CreatedAt:  now,
		ToolCallID: &id,
		ToolName:   name,
		IsError:    isError,
	})
	ls.UpdatedAt = now
}
