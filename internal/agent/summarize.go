package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

// summarizeSystemPrompt drives the summarize-and-continue compaction call. Kept
// short + explicit so output is stable across providers. The result is treated
// as the model's first assistant turn after the summary marker (replayHistory).
const summarizeSystemPrompt = `You are condensing a chat between a user and an assistant so the conversation can continue with a smaller context.

Produce a structured plain-text summary covering:

1. **What the user is trying to accomplish** — the overall goal and any sub-goals still in flight.
2. **Decisions made** — anything the user explicitly approved, rejected, or chose between.
3. **Findings** — concrete facts, numbers, file paths, or analysis results that future turns will need to reference.
4. **Open threads** — questions you've asked the user, things you said you'd do next, anything left unfinished.
5. **Working artifacts** — files in the workspace, drafts in progress, attachments the user uploaded that future turns may want to reference.

Rules:
- Be specific. Include exact file paths, exact numbers, exact metric names. A future turn will read your summary in place of the scroll, so vague summaries become real information loss.
- Do NOT speculate or add information that isn't in the conversation. If something is uncertain, say so.
- Do NOT roleplay or address the user — produce a neutral brief.
- Aim for 200–600 words. Longer is fine if the conversation was complex; shorter is fine if it was simple.

Return only the summary text, no preamble.`

// Summarize runs a single streaming completion condensing the supplied history
// into a structured brief. No tools / MCP / sandbox — one job, minimal cost.
func (m *Manager) Summarize(ctx context.Context, in SummarizeInput) (*SummarizeResult, error) {
	if len(in.History) == 0 {
		return nil, fmt.Errorf("summarize: history is empty")
	}
	if in.Lockdown && in.Model != "" && !m.config.LockdownAllows(in.Model) {
		return nil, fmt.Errorf("model %q not allowed in lockdown mode", in.Model)
	}

	model, err := m.resolver.Resolve(ctx, in.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve summarize model: %w", err)
	}
	modelSlug := model.Model()

	messages, err := replayHistory(in.History)
	if err != nil {
		return nil, fmt.Errorf("replay history for summarize: %w", err)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("summarize: history yielded no replayable messages")
	}

	const summarizeMaxOutputTokens int64 = 4096
	maxTokens := summarizeMaxOutputTokens
	temp := m.config.Temperature

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(summarizeSystemPrompt),
		fantasy.WithMaxOutputTokens(maxTokens),
	)

	messages = append(messages, fantasy.NewUserMessage("Produce the summary as instructed above."))

	startedAt := time.Now()
	streamCall := fantasy.AgentStreamCall{
		Messages:        messages,
		MaxOutputTokens: &maxTokens,
		Temperature:     &temp,
	}
	if in.OnTextDelta != nil {
		streamCall.OnTextDelta = func(_ string, text string) error {
			in.OnTextDelta(text)
			return nil
		}
	}
	result, err := ag.Stream(ctx, streamCall)
	if err != nil {
		return nil, fmt.Errorf("summarize generate: %w", err)
	}

	text := strings.TrimSpace(result.Response.Content.Text())
	if text == "" {
		return nil, fmt.Errorf("summarize: model returned empty text after %s", time.Since(startedAt))
	}

	cost := 0.0
	if c := openrouterCost(result.Response.ProviderMetadata); c != nil {
		cost = *c
	}

	return &SummarizeResult{
		Text:             text,
		Model:            modelSlug,
		PromptTokens:     int(result.TotalUsage.InputTokens),
		CompletionTokens: int(result.TotalUsage.OutputTokens),
		CostUSD:          cost,
	}, nil
}
