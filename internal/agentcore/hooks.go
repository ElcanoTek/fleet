package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// Governed lifecycle hooks (#788).
//
// Operator bundles may declare hooks that run at fixed agent-run lifecycle
// points — user_prompt_submit, pre_tool_use, post_tool_use, turn_end — as
// commands executed INSIDE the per-turn sandbox via the Executor seam (never a
// host exec.Command; that would recreate the #784 hole). A hook receives a
// bounded, redacted, credential-free JSON payload on stdin and prints a JSON
// decision on stdout. It can only OBSERVE or NARROW: continue, block-with-reason,
// or attach a small bounded context fragment. It cannot add tools, credentials,
// network, budget, or approval authority, and fleet's existing policy/approval/
// audit gates evaluate AFTER hooks on the same unmodified input, so a hook can
// never widen authority.
//
// This lives inside agentcore.Run / buildFantasyTools, so both interactive and
// scheduled runs (and spawned sub-agents) inherit hooks with zero driver
// changes — one governed core. It is nil-safe and zero-overhead when no hooks
// are configured.

// HookEvent is a lifecycle point a hook can attach to. Values mirror the
// clientconfig HookEvent* strings.
type HookEvent string

const (
	HookUserPromptSubmit HookEvent = "user_prompt_submit"
	HookPreToolUse       HookEvent = "pre_tool_use"
	HookPostToolUse      HookEvent = "post_tool_use"
	HookTurnEnd          HookEvent = "turn_end"
)

// LifecycleHook is the engine-side form of a bundle HookDef (translated by
// cmd/fleet at startup so agentcore does not import clientconfig).
type LifecycleHook struct {
	ID      string
	Event   HookEvent
	Matcher string
	Command string
	Timeout time.Duration
	Enforce bool
}

// ErrBlockedByHook is wrapped into the Run error when a user_prompt_submit hook
// blocks a turn (mirrors ErrGuardrailBlocked's ingress-block path).
var ErrBlockedByHook = errors.New("blocked by lifecycle hook")

// Payload/output caps. Hooks are advisory plumbing, not a data channel.
const (
	hookAPIVersion       = 1
	hookInputCap         = 8 * 1024
	hookResultPreviewCap = 4 * 1024
	hookReasonCap        = 1 * 1024
	hookContextFragCap   = 4 * 1024
	hookRunContextBudget = 32 * 1024
	hookStdoutReadCap    = 16 * 1024
	hookTimeoutGrace     = 5 * time.Second
)

var (
	lifecycleHooksMu sync.Mutex
	lifecycleHooks   []LifecycleHook
)

// ConfigureLifecycleHooks installs the bundle's hooks. Call once at startup
// (cmd/fleet) before any turn runs. Idempotent full-replace, mirroring
// ConfigureAgentPolicy. Passing nil disables hooks (the generic-bundle default).
func ConfigureLifecycleHooks(hooks []LifecycleHook) {
	lifecycleHooksMu.Lock()
	defer lifecycleHooksMu.Unlock()
	lifecycleHooks = append([]LifecycleHook(nil), hooks...)
}

func activeLifecycleHooks() []LifecycleHook {
	lifecycleHooksMu.Lock()
	defer lifecycleHooksMu.Unlock()
	return append([]LifecycleHook(nil), lifecycleHooks...)
}

// hookEngine runs the configured hooks for a single run. Constructed per run;
// newHookEngine returns nil when no hooks are configured, so every call site is
// a nil-safe no-op with zero overhead in the default (no-hooks) deployment.
type hookEngine struct {
	hooks    []LifecycleHook
	exec     Executor
	observer Observer
	mode     string // stringified once here — never branched on (TestSeamPurity)
	label    string

	mu           sync.Mutex
	ctxBytesUsed int
}

// newHookEngine builds the per-run engine, or nil when there are no hooks (or
// no executor to run them in — hooks MUST run in the sandbox).
func newHookEngine(mode Mode, exec Executor, obs Observer, label string) *hookEngine {
	hooks := activeLifecycleHooks()
	if len(hooks) == 0 || exec == nil {
		return nil
	}
	return &hookEngine{hooks: hooks, exec: exec, observer: obs, mode: mode.String(), label: label}
}

// hookDecision is the JSON contract a hook prints on stdout.
type hookDecision struct {
	Decision          string `json:"decision"` // "block" | "continue" (default)
	Reason            string `json:"reason"`
	AdditionalContext string `json:"additional_context"`
}

// hookOutcome is the engine-internal result of one invocation.
type hookOutcome struct {
	blocked bool
	reason  string
	context string
}

// matchesTool reports whether a matcher selects toolName. "" or "*" = all; a
// trailing "*" is a prefix glob; otherwise exact.
func matchesTool(matcher, toolName string) bool {
	m := strings.TrimSpace(matcher)
	if m == "" || m == "*" {
		return true
	}
	if strings.HasSuffix(m, "*") {
		return strings.HasPrefix(toolName, strings.TrimSuffix(m, "*"))
	}
	return m == toolName
}

// preToolUse runs the matching pre_tool_use hooks in order; the first block
// wins (later hooks skipped). Returns (blocked, reason).
func (e *hookEngine) preToolUse(ctx context.Context, toolName, callID, rawInput string) (bool, string) {
	if e == nil {
		return false, ""
	}
	payload := e.basePayload(HookPreToolUse, map[string]any{
		"tool_name":    toolName,
		"tool_call_id": callID,
	})
	e.addTruncated(payload, "tool_input", "tool_input_truncated", rawInput, hookInputCap)
	for _, h := range e.hooks {
		if h.Event != HookPreToolUse || !matchesTool(h.Matcher, toolName) {
			continue
		}
		out := e.invoke(ctx, h, payload, toolName, callID)
		if out.blocked {
			return true, out.reason
		}
	}
	return false, ""
}

// postToolUse runs the matching post_tool_use hooks; returns any bounded
// additional-context fragments joined for appending to the tool result.
func (e *hookEngine) postToolUse(ctx context.Context, toolName, callID, rawInput, result string, isError bool) string {
	if e == nil {
		return ""
	}
	payload := e.basePayload(HookPostToolUse, map[string]any{
		"tool_name":            toolName,
		"tool_call_id":         callID,
		"tool_result_is_error": isError,
	})
	e.addTruncated(payload, "tool_input", "tool_input_truncated", rawInput, hookInputCap)
	e.addTruncated(payload, "tool_result_preview", "tool_result_truncated", result, hookResultPreviewCap)
	var frags []string
	for _, h := range e.hooks {
		if h.Event != HookPostToolUse || !matchesTool(h.Matcher, toolName) {
			continue
		}
		out := e.invoke(ctx, h, payload, toolName, callID)
		if out.context != "" {
			frags = append(frags, "[hook:"+h.ID+"] "+out.context)
		}
	}
	return strings.Join(frags, "\n\n")
}

// userPromptSubmit runs user_prompt_submit hooks over the incoming messages.
// Returns (blocked, reason, additionalContext).
func (e *hookEngine) userPromptSubmit(ctx context.Context, messages []fantasy.Message) (bool, string, string) {
	if e == nil {
		return false, "", ""
	}
	last := ""
	if n := len(messages); n > 0 {
		last = messageText(messages[n-1])
	}
	payload := e.basePayload(HookUserPromptSubmit, nil)
	e.addTruncated(payload, "prompt_preview", "prompt_truncated", last, hookInputCap)
	var frags []string
	for _, h := range e.hooks {
		if h.Event != HookUserPromptSubmit {
			continue
		}
		out := e.invoke(ctx, h, payload, "", "")
		if out.blocked {
			return true, out.reason, ""
		}
		if out.context != "" {
			frags = append(frags, out.context)
		}
	}
	return false, "", strings.Join(frags, "\n\n")
}

// applyUserPromptSubmitHooks runs the user_prompt_submit hooks and applies the
// outcome: a block becomes a Run error (mirroring ErrGuardrailBlocked's ingress
// path), additional context is appended (append-only, so the prompt-cache
// prefix stays stable). nil engine → returns messages unchanged. Extracted from
// Run to keep its cyclomatic complexity flat.
func applyUserPromptSubmitHooks(ctx context.Context, e *hookEngine, messages []fantasy.Message) ([]fantasy.Message, error) {
	blocked, reason, extra := e.userPromptSubmit(ctx, messages)
	if blocked {
		return nil, fmt.Errorf("%w: %s", ErrBlockedByHook, reason)
	}
	if extra != "" {
		messages = append(messages, fantasy.NewUserMessage(extra))
	}
	return messages, nil
}

// turnEnd runs turn_end hooks observationally (their decision is audited but not
// enforced — a completed turn is not undone).
func (e *hookEngine) turnEnd(ctx context.Context, finalText string, rounds int) {
	if e == nil {
		return
	}
	payload := e.basePayload(HookTurnEnd, map[string]any{"rounds": rounds})
	e.addTruncated(payload, "final_text_preview", "final_text_truncated", finalText, hookResultPreviewCap)
	for _, h := range e.hooks {
		if h.Event != HookTurnEnd {
			continue
		}
		e.invoke(ctx, h, payload, "", "")
	}
}

// basePayload builds the versioned payload common fields.
func (e *hookEngine) basePayload(event HookEvent, extra map[string]any) map[string]any {
	p := map[string]any{
		"hook_api_version": hookAPIVersion,
		"event":            string(event),
		"mode":             e.mode,
		"label":            e.label,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// addTruncated redacts + byte-caps text into the payload under key, setting the
// truncation flag when it was cut.
func (e *hookEngine) addTruncated(payload map[string]any, key, truncKey, text string, limit int) {
	red := toolRedactor().Redact(text)
	if len(red) > limit {
		red = red[:limit]
		payload[truncKey] = true
	}
	payload[key] = red
}

// invoke runs one hook in the sandbox and maps its result to a hookOutcome. All
// failure modes (nonzero exit, timeout, malformed output, executor error,
// panic) are contained: an ENFORCE hook blocks; an advisory hook continues.
// Every invocation emits a hook.decision audit event.
func (e *hookEngine) invoke(ctx context.Context, h LifecycleHook, payload map[string]any, toolName, callID string) (outcome hookOutcome) {
	start := time.Now()
	errClass := ""
	decision := "continue"
	defer func() {
		if r := recover(); r != nil {
			safe.EmitPanic("agentcore.hook."+h.ID, r, debug.Stack())
			errClass = "panic"
			outcome = e.onFailure(h, "hook panicked")
			decision = decisionOf(outcome)
		}
		e.emit(h, toolName, callID, decision, outcome.reason, time.Since(start), errClass)
	}()

	body, err := json.Marshal(payload)
	if err != nil {
		errClass = "marshal"
		outcome = e.onFailure(h, "hook payload marshal failed")
		decision = decisionOf(outcome)
		return outcome
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout+hookTimeoutGrace)
	defer cancel()

	// Pipe the payload to the hook on stdin and bound the in-container run with
	// `timeout` (the process-leak window mitigation, #796). Everything is
	// single-quote escaped so neither the JSON nor the operator command can
	// break out of the wrapper.
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	wrapped := fmt.Sprintf("printf '%%s' %s | timeout %ds bash -c %s",
		shellSingleQuote(string(body)), secs, shellSingleQuote(h.Command))

	out, runErr := e.exec.RunBash(hctx, wrapped)
	if runErr != nil {
		errClass = "exec_error"
		if hctx.Err() != nil {
			errClass = "timeout"
		}
		outcome = e.onFailure(h, "hook "+errClass)
		decision = decisionOf(outcome)
		return outcome
	}

	dec, ok := parseHookDecision(out)
	if !ok {
		errClass = "malformed"
		outcome = e.onFailure(h, "hook produced no valid JSON decision")
		decision = decisionOf(outcome)
		return outcome
	}
	if dec.Decision == "block" {
		outcome = hookOutcome{blocked: true, reason: capString(dec.Reason, hookReasonCap)}
		decision = "block"
		return outcome
	}
	if frag := capString(dec.AdditionalContext, hookContextFragCap); frag != "" {
		outcome.context = e.chargeContext(frag)
	}
	decision = "continue"
	return outcome
}

// onFailure maps a hook failure to fail-closed (enforce) or fail-observable.
func (e *hookEngine) onFailure(h LifecycleHook, reason string) hookOutcome {
	if h.Enforce {
		return hookOutcome{blocked: true, reason: fmt.Sprintf("hook %q failed: %s", h.ID, reason)}
	}
	return hookOutcome{}
}

// chargeContext debits the per-run context budget; an over-budget fragment is
// dropped (and its drop is captured by the caller's audit event).
func (e *hookEngine) chargeContext(frag string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctxBytesUsed+len(frag) > hookRunContextBudget {
		return ""
	}
	e.ctxBytesUsed += len(frag)
	return frag
}

func (e *hookEngine) emit(h LifecycleHook, toolName, callID, decision, reason string, dur time.Duration, errClass string) {
	if e.observer == nil {
		return
	}
	e.observer.Observe("hook.decision", map[string]any{
		"hook_id":      h.ID,
		"event":        string(h.Event),
		"tool_name":    toolName,
		"tool_call_id": callID,
		"decision":     decision,
		"reason":       reason,
		"duration_ms":  dur.Milliseconds(),
		"error_class":  errClass,
		"enforce":      h.Enforce,
	})
}

func decisionOf(o hookOutcome) string {
	if o.blocked {
		return "block"
	}
	return "continue"
}

// parseHookDecision scans the output for the LAST parseable JSON object line
// (so a hook that prints diagnostics before its decision still works).
func parseHookDecision(out string) (hookDecision, bool) {
	if len(out) > hookStdoutReadCap {
		out = out[:hookStdoutReadCap]
	}
	var found hookDecision
	ok := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var d hookDecision
		if json.Unmarshal([]byte(line), &d) == nil {
			found, ok = d, true
		}
	}
	return found, ok
}

func capString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes as
// the standard '\” sequence, so it is a safe single shell word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// messageText flattens a fantasy.Message's text parts.
func messageText(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}
