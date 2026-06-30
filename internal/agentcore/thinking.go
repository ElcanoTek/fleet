package agentcore

import "strings"

// ThinkingConfig controls Claude extended thinking for a run (#220). Claude 4
// models can run an internal chain-of-thought before the visible answer, which
// substantially improves complex reasoning; OpenRouter activates it via a
// `thinking` extra-body parameter. The zero value (Enabled=false) leaves the
// request untouched — byte-for-byte the prior behavior.
type ThinkingConfig struct {
	Enabled bool `json:"enabled"`
	// BudgetTokens is the thinking budget. Claude's documented range is
	// [MinThinkingBudgetTokens, MaxThinkingBudgetTokens]; the producer clamps to
	// that window, so an out-of-range value here is never sent verbatim.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

const (
	// MinThinkingBudgetTokens / MaxThinkingBudgetTokens bound the thinking budget
	// per Claude's API contract. A request below the minimum is rejected by the
	// provider; above the maximum is wasteful. The producer clamps into this
	// window rather than erroring, so a generous operator default still works.
	MinThinkingBudgetTokens = 1024
	MaxThinkingBudgetTokens = 100_000
)

// ClampThinkingBudget clamps a requested budget into Claude's accepted window.
// A non-positive budget clamps up to the minimum (callers gate on Enabled first,
// so this is only reached for an enabled config).
func ClampThinkingBudget(budget int) int {
	if budget < MinThinkingBudgetTokens {
		return MinThinkingBudgetTokens
	}
	if budget > MaxThinkingBudgetTokens {
		return MaxThinkingBudgetTokens
	}
	return budget
}

// ThinkingConfigForBudget builds an enabled ThinkingConfig from a global default
// budget (FLEET_DEFAULT_THINKING_BUDGET_TOKENS). A non-positive budget returns
// nil — thinking stays off when no default is configured. The drivers use this
// as the fallback when a per-conversation/task config is absent.
func ThinkingConfigForBudget(budget int) *ThinkingConfig {
	if budget <= 0 {
		return nil
	}
	return &ThinkingConfig{Enabled: true, BudgetTokens: ClampThinkingBudget(budget)}
}

// supportsExtendedThinking reports whether slug is a Claude model that supports
// extended thinking. Only the Claude 4 family (opus-4.x / sonnet-4.x) qualifies;
// a leading `~` floating alias is excluded because fantasy drops the thinking
// signature for aliased slugs (mirroring upstreamPinFor's `~` handling), which
// would make the provider reject the follow-up tool-use turn. Non-Claude models
// (Gemini, OpenAI) never qualify, so enabling thinking on them is a silent no-op
// rather than a provider error.
func supportsExtendedThinking(slug string) bool {
	s := strings.ToLower(strings.TrimSpace(slug))
	if strings.HasPrefix(s, "~") {
		return false
	}
	return strings.Contains(s, "claude-opus-4") || strings.Contains(s, "claude-sonnet-4")
}
