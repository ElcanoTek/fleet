package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"
)

// "Phone a friend" super-LLM review (scheduled-only; part of #175).
//
// This is the same SHAPE as the end-of-run verifier (verifier.go): a one-time,
// host-side LLM call layered onto the scheduled policy's CanFinish seam. The
// difference is the role of the second model and what it returns.
//
//   - The verifier (runEndOfRunVerifier) is a *completeness* check on the
//     fallback model: did the run attempt every action the task demanded?
//   - phone_a_friend is a *quality* review by a configurable, typically STRONGER
//     "reviewer"/"super" model (inspired by Brad's lifeline MCP — a one-time
//     review by a more capable model). It critiques the agent's actual answer/
//     work and, when it finds material problems, feeds the critique back into
//     ONE more enforcement round so the agent can address it before finishing.
//
// Both run host-side (the reviewer's model call is just another host LLM call,
// so its credentials never enter the sandbox or the agent's model context) and
// both degrade gracefully: a reviewer error fails OPEN (allow finish) exactly
// like the verifier, so a flaky reviewer never blocks an otherwise-complete run.
//
// Deliberately NOT a built-in tool and NOT an MCP server:
//   - A built-in tool would let the agent invoke the reviewer at will (any
//     number of times, unbudgeted) and would surface the reviewer in the
//     sandboxed tool roster — but the reviewer call is a host-side LanguageModel
//     call whose credentials stay host-side, so it belongs at the host finish
//     seam, not in the tool surface. It would also create a second model-
//     invocation path the policy does not govern.
//   - An MCP server (like lifeline) would push the reviewer toward the broker /
//     sandbox boundary and add an external dependency.
//
// Wiring it into CanFinish reuses the verifier machinery and keeps governance as
// ONE core: the critique re-enters the run through the SAME enforcement-round
// channel the verifier already uses (a non-empty issue list blocks finishing),
// so no second governance path is created.

const reviewerTimeout = 3 * time.Minute

const reviewerMaxTaskChars = 12000

const reviewerMaxAnswerChars = 16000

// Host-log preview clamps. A parse failure logs a short raw preview; a successful
// verdict logs a longer reasoning preview (the reasoning is the useful signal).
const (
	reviewerRawPreviewChars       = 200
	reviewerReasoningPreviewChars = 400
)

// reviewResult is the reviewer model's structured verdict. NeedsRevision gates
// whether the critique re-enters the loop; Issues are the concrete, actionable
// problems fed back as the final enforcement round.
type reviewResult struct {
	NeedsRevision bool     `json:"needs_revision"`
	Issues        []string `json:"issues"`
	Reasoning     string   `json:"reasoning"`
}

func truncateForReviewer(s string, maxChars int) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) <= maxChars {
		return trimmed
	}
	return trimmed[:maxChars] + "\n…[truncated for reviewer]"
}

// latestAssistantText returns the most recent non-empty assistant message text
// in the session — the answer/work the reviewer critiques. Reads the log via
// SnapshotMessages so it never touches the session's unexported mutex.
func latestAssistantText(session *LogSession) string {
	if session == nil {
		return ""
	}
	messages := session.SnapshotMessages()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != roleAssistant {
			continue
		}
		if text := strings.TrimSpace(messages[i].Content); text != "" {
			return text
		}
	}
	return ""
}

// runPhoneAFriendReview asks the configured reviewer model to critique the
// agent's answer/work against the original task and report material issues that
// must be addressed before finishing. It returns the issue list only when the
// reviewer flags the work as needing revision; an empty list means "ship it".
//
// The reviewer model is passed in explicitly (rather than read off the Agent)
// so the caller controls the configurable reviewer selection and so this
// function never has to know how the model was resolved — keeping credentials
// out of sight here.
func (a *Agent) runPhoneAFriendReview(ctx context.Context, reviewer fantasy.LanguageModel, task, answer string, records []toolExecRecord) ([]string, error) {
	if reviewer == nil {
		return nil, fmt.Errorf("no reviewer model configured for phone_a_friend")
	}

	recordsJSON, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("marshal tool records: %w", err)
	}

	systemPrompt := `You are a senior reviewer ("phone a friend") for an automated agent: ` +
		`a one-time, high-quality second opinion from a stronger model. ` +
		`Given the agent's original task, its final answer/work, and the list of ` +
		`tool calls it executed, judge whether the work is correct, complete, and ` +
		`safe to ship as-is. ` +
		`Flag only MATERIAL problems the agent can act on: wrong or unsupported ` +
		`conclusions, missed requirements, unsafe or risky actions, and clear ` +
		`quality defects. Do not nitpick style or invent requirements the task ` +
		`did not state; when the work is good enough, say so. ` +
		"\n\n" +
		`Respond with a single JSON object and no other text, matching: ` +
		`{"needs_revision": bool, "issues": [string, ...], "reasoning": string}. ` +
		`Set needs_revision to false and issues to [] when the work is acceptable. ` +
		`Each issue must be a concise, actionable instruction the agent can follow ` +
		`to fix the problem (e.g. "recompute the Q3 totals; the sum excludes ` +
		`refunds", "the email omits the agreed deadline — add it before sending").`

	userPrompt := fmt.Sprintf(
		"ORIGINAL TASK (possibly truncated):\n---\n%s\n---\n\n"+
			"AGENT'S FINAL ANSWER / WORK (possibly truncated):\n---\n%s\n---\n\n"+
			"TOOL EXECUTIONS (JSON):\n%s",
		truncateForReviewer(task, reviewerMaxTaskChars),
		truncateForReviewer(answer, reviewerMaxAnswerChars),
		string(recordsJSON),
	)

	reviewCtx, cancel := context.WithTimeout(ctx, reviewerTimeout)
	defer cancel()

	reviewAgent := fantasy.NewAgent(reviewer, fantasy.WithSystemPrompt(systemPrompt))
	out, err := reviewAgent.Generate(reviewCtx, fantasy.AgentCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage(userPrompt)},
	})
	if err != nil {
		return nil, fmt.Errorf("reviewer call failed: %w", err)
	}
	raw := strings.TrimSpace(out.Response.Content.Text())
	if raw == "" {
		return nil, fmt.Errorf("reviewer returned empty response")
	}

	parsed, err := parseReviewResult(raw)
	if err != nil {
		// Clamp the raw model output to a short single-line preview so a
		// malformed verdict cannot flood the host log with the full response.
		return nil, fmt.Errorf("reviewer output parse: %w (raw=%q)", err, summarizeForConsole(raw, reviewerRawPreviewChars))
	}
	log.Printf("phone_a_friend: needs_revision=%v issues=%v reasoning=%q",
		parsed.NeedsRevision, parsed.Issues, summarizeForConsole(parsed.Reasoning, reviewerReasoningPreviewChars))
	if !parsed.NeedsRevision {
		return nil, nil
	}
	return parsed.Issues, nil
}

func parseReviewResult(raw string) (reviewResult, error) {
	candidate := strings.TrimSpace(raw)
	candidate = strings.TrimPrefix(candidate, "```json")
	candidate = strings.TrimPrefix(candidate, "```")
	candidate = strings.TrimSuffix(candidate, "```")
	candidate = strings.TrimSpace(candidate)

	start := strings.Index(candidate, "{")
	end := strings.LastIndex(candidate, "}")
	if start < 0 || end <= start {
		return reviewResult{}, fmt.Errorf("no JSON object found")
	}
	candidate = candidate[start : end+1]

	var result reviewResult
	if err := json.Unmarshal([]byte(candidate), &result); err != nil {
		return reviewResult{}, err
	}
	cleaned := result.Issues[:0]
	for _, m := range result.Issues {
		if s := strings.TrimSpace(m); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	result.Issues = cleaned
	return result, nil
}
