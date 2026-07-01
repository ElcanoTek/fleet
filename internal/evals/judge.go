package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// The LLM-judge: one rubric-driven grading call whose verdict is validated
// against a fixed schema (the AnalyzeTaskFailure #317 pattern — a short-lived
// fantasy agent through the SAME host-side resolver the runs use, so the
// operator's key stays host-side and the rubric + graded output never leave
// the box). It is deliberately NOT a full agentcore.Run: the judge is a single
// bounded tool-less completion, governed by construction (no tools, fixed
// timeout, temperature 0, schema-validated output) exactly like SuggestTitle /
// AnalyzeTaskFailure / the loop's YES-NO verifier. The REPLAY under judgment
// is what goes through agentcore.Run.
//
// #502's noted risk — "the known structuredoutput no-retry gap would error a
// malformed judge verdict" — is handled explicitly here: one bounded retry
// feeds the validation error back to the model before the judge gives up.

// judgeVerdictSchema is the fixed draft-07 schema every judge verdict must
// conform to. additionalProperties:false so the model can't smuggle freeform
// keys past the gate.
const judgeVerdictSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["pass", "score"],
  "properties": {
    "pass": { "type": "boolean" },
    "score": { "type": "number", "minimum": 0, "maximum": 1 },
    "reasoning": { "type": "string", "maxLength": 2000 }
  }
}`

const (
	judgeTimeout   = 90 * time.Second
	judgeMaxTokens = 1024
	// Input caps so a pathological transcript can't blow the judge's context.
	judgeMaxRubricChars   = 2000
	judgeMaxPromptChars   = 4000
	judgeMaxExpectedChars = 8000
	judgeMaxActualChars   = 16000
)

// Verdict is the judge's validated grading of one case output.
type Verdict struct {
	Pass      bool    `json:"pass"`
	Score     float64 `json:"score"`
	Reasoning string  `json:"reasoning,omitempty"`
}

// ModelResolver resolves a model slug to a usable handle — satisfied by
// *agent.Manager (and by agentcore.ModelResolver via a thin wrapper in tests).
type ModelResolver interface {
	Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error)
}

// RunJudge grades actual against the rubric (and optional expected reference)
// on the given judge model, returning a schema-validated Verdict. It returns
// an error — never a partial verdict — when the model can't be resolved, the
// call fails, or the output stays invalid after one corrective retry. Callers
// treat a judge error as a FAILED scorer (fail-closed): a gate must not pass
// on a grade it never got.
func RunJudge(ctx context.Context, resolver ModelResolver, slug, rubric, prompt, expected, actual string) (*Verdict, error) {
	if strings.TrimSpace(slug) == "" {
		return nil, fmt.Errorf("llm_judge: no judge model configured")
	}
	model, err := resolver.Resolve(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("resolve judge model %q: %w", slug, err)
	}

	sys := "You are a strict evaluation judge grading an AI agent's answer against a rubric. " +
		"Score 1.0 only when the answer fully satisfies the rubric; score 0.0 when it clearly fails; " +
		"use intermediate values for partial credit. Set pass=true only when the rubric is satisfied. " +
		"Judge ONLY the answer given — do not solve the task yourself." +
		structuredoutput.PromptAugmentation(json.RawMessage(judgeVerdictSchema))

	var user strings.Builder
	fmt.Fprintf(&user, "Rubric:\n%s\n\nOriginal prompt:\n%s\n\n", truncate(rubric, judgeMaxRubricChars), truncate(prompt, judgeMaxPromptChars))
	if strings.TrimSpace(expected) != "" {
		fmt.Fprintf(&user, "Reference answer (a known-good output for comparison):\n%s\n\n", truncate(expected, judgeMaxExpectedChars))
	}
	fmt.Fprintf(&user, "Answer under evaluation:\n%s", truncate(actual, judgeMaxActualChars))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.0),
		fantasy.WithMaxOutputTokens(judgeMaxTokens),
	)

	ctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()

	messages := []fantasy.Message{fantasy.NewUserMessage(user.String())}
	reply, err := judgeGenerate(ctx, ag, messages)
	if err != nil {
		return nil, fmt.Errorf("judge generate: %w", err)
	}
	validated, verr := structuredoutput.ValidateOutput(reply, json.RawMessage(judgeVerdictSchema))
	if verr != nil {
		// One corrective retry with the validation error fed back — the explicit
		// handling #502 asks for around structuredoutput's no-retry gap.
		messages = append(messages,
			fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: reply}},
			},
			fantasy.NewUserMessage(fmt.Sprintf(
				"Your reply was not a valid verdict: %v. Reply again with ONLY the JSON object conforming to the schema — no prose, no fences.", verr)),
		)
		reply, err = judgeGenerate(ctx, ag, messages)
		if err != nil {
			return nil, fmt.Errorf("judge retry generate: %w", err)
		}
		validated, verr = structuredoutput.ValidateOutput(reply, json.RawMessage(judgeVerdictSchema))
		if verr != nil {
			return nil, fmt.Errorf("judge verdict failed schema validation after retry: %w", verr)
		}
	}

	var v Verdict
	if err := json.Unmarshal(validated, &v); err != nil {
		return nil, fmt.Errorf("decode judge verdict: %w", err)
	}
	return &v, nil
}

// judgeGenerate makes one bounded tool-less call and concatenates the text parts.
func judgeGenerate(ctx context.Context, ag fantasy.Agent, messages []fantasy.Message) (string, error) {
	maxTokens := int64(judgeMaxTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        messages,
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return out.String(), nil
}

// truncate clamps s to max chars with an ellipsis marker (rune-safe enough for
// prompt material — clamping mid-rune only risks one mangled character in a
// truncation notice).
func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n…[truncated]"
}
