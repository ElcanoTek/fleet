package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// errorAnalysisSchema is the fixed draft-07 JSON Schema the post-failure
// diagnosis (#317) must conform to. A closed taxonomy keeps `category` machine-
// filterable; `remediation` is 1–3 concrete steps. additionalProperties:false so
// the model can't smuggle freeform keys past the schema.
const errorAnalysisSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["category", "summary"],
  "properties": {
    "category": {
      "type": "string",
      "enum": [
        "configuration",
        "credentials",
        "network",
        "rate_limit",
        "timeout",
        "tool_error",
        "model_refusal",
        "resource_exhausted",
        "logic_error",
        "unknown"
      ]
    },
    "summary": { "type": "string", "maxLength": 600 },
    "remediation": {
      "type": "array",
      "items": { "type": "string", "maxLength": 400 },
      "maxItems": 3
    }
  }
}`

const (
	errorAnalysisTimeout   = 30 * time.Second
	errorAnalysisMaxTokens = 1024
	// Input caps so a pathological prompt/log can't blow the cheap model's context.
	errorAnalysisMaxPromptChars  = 1000
	errorAnalysisMaxErrChars     = 1500
	errorAnalysisMaxSessionChars = 6000
)

// AnalyzeTaskFailure runs the post-failure LLM diagnosis (#317): it asks the
// operator-configured cheap model (config.ErrorAnalysisModel) to classify a
// TERMINAL task failure and propose remediation, returning JSON validated against
// errorAnalysisSchema. It mirrors SuggestTitle — a short-lived fantasy.NewAgent
// call through the SAME host-side resolver, with a tight prompt + timeout, so the
// operator's key stays host-side and the call rides the shared governed provider.
//
// It satisfies runner.ErrorAnalyzer (primitive params, no shared types, so the
// runner stays decoupled from this package). Returns an error — never a partial /
// unvalidated result — on resolve/generate/validation failure; the caller logs it
// and persists nothing (best-effort diagnosis).
func (m *Manager) AnalyzeTaskFailure(ctx context.Context, taskPrompt, errMsg, sessionTail string) (json.RawMessage, error) {
	if strings.TrimSpace(errMsg) == "" {
		return nil, fmt.Errorf("error-analysis: empty error message")
	}
	modelSlug := m.config.ErrorAnalysisModel
	model, err := m.resolver.Resolve(ctx, modelSlug)
	if err != nil {
		return nil, fmt.Errorf("resolve error-analysis model %q: %w", modelSlug, err)
	}

	sys := "You are a Fleet task-failure diagnostician. A scheduled agent task failed terminally. " +
		"Given the task instructions, the terminal error, and the tail of the session log, classify the " +
		"failure using ONLY the allowed category values and propose 1–3 specific, concrete remediation steps " +
		"an operator could take. Be precise and actionable; do not speculate beyond the evidence." +
		structuredoutput.PromptAugmentation(json.RawMessage(errorAnalysisSchema))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.1),
		fantasy.WithMaxOutputTokens(errorAnalysisMaxTokens),
	)

	prompt := fmt.Sprintf("Task instructions:\n%s\n\nTerminal error:\n%s\n\nSession log tail:\n%s",
		truncate(taskPrompt, errorAnalysisMaxPromptChars),
		truncate(errMsg, errorAnalysisMaxErrChars),
		truncate(sessionTail, errorAnalysisMaxSessionChars),
	)

	ctx, cancel := context.WithTimeout(ctx, errorAnalysisTimeout)
	defer cancel()

	maxTokens := int64(errorAnalysisMaxTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(prompt)},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("error-analysis generate: %w", err)
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}

	validated, err := structuredoutput.ValidateOutput(out.String(), json.RawMessage(errorAnalysisSchema))
	if err != nil {
		return nil, fmt.Errorf("error-analysis output failed schema validation: %w", err)
	}
	return validated, nil
}
