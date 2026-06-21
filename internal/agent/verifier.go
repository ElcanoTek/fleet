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

// End-of-run verifier (scheduled-only; ported from cutlass verifier.go).
//
// After a scheduled run finishes (audit cleared, loop terminated), the verifier
// makes one fallback-model pass over the original task + the executed tool
// summary and reports any user-visible deliverable the task demanded that was
// never successfully attempted. The scheduled driver feeds the result back into
// the enforcement loop (a non-empty Missing list blocks finishing).

const verifierTimeout = 2 * time.Minute

const verifierMaxTaskChars = 12000

type verifierResult struct {
	Missing   []string `json:"missing_actions"`
	Reasoning string   `json:"reasoning"`
}

type toolExecRecord struct {
	Name      string `json:"name"`
	Succeeded bool   `json:"succeeded"`
}

// buildToolExecSummary pairs each tool call in the session log with its result,
// classifying success/failure. Tool calls without a result count as failed so
// the verifier treats them as incomplete. Reads the log via SnapshotMessages so
// it never touches the session's unexported mutex.
func buildToolExecSummary(session *LogSession) []toolExecRecord {
	if session == nil {
		return nil
	}
	messages := session.SnapshotMessages()

	type pendingCall struct {
		id   string
		name string
	}
	records := make([]toolExecRecord, 0, len(messages))
	calls := make(map[string]pendingCall)

	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			calls[tc.ID] = pendingCall{id: tc.ID, name: tc.Name}
		}
		if msg.Role == roleTool && msg.ToolCallID != nil {
			pc, ok := calls[*msg.ToolCallID]
			if !ok {
				continue
			}
			delete(calls, *msg.ToolCallID)
			records = append(records, toolExecRecord{
				Name:      pc.name,
				Succeeded: !toolResultLooksFailed(msg.Content),
			})
		}
	}
	for _, pc := range calls {
		records = append(records, toolExecRecord{Name: pc.name, Succeeded: false})
	}
	return records
}

// toolResultLooksFailed detects failed tool result patterns: the [tool error]
// prefix, enforcement blocks, and {"status":"error"} JSON bodies.
func toolResultLooksFailed(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[tool error]") {
		return true
	}
	for _, prefix := range []string{"LOOP_GUARD", "BLOCKED:", "Safety Limit:", "Safety Guard:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var probe struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
			return strings.EqualFold(probe.Status, "error")
		}
		return strings.HasPrefix(trimmed, `{"status":"error"`) || strings.HasPrefix(trimmed, `{"status": "error"`)
	}
	return false
}

func truncateTaskForVerifier(task string) string {
	trimmed := strings.TrimSpace(task)
	if len(trimmed) <= verifierMaxTaskChars {
		return trimmed
	}
	return trimmed[:verifierMaxTaskChars] + "\n…[truncated for verifier]"
}

// runEndOfRunVerifier asks the fallback model whether every action the task
// demanded was successfully attempted, returning the list of missing actions.
func (a *Agent) runEndOfRunVerifier(ctx context.Context, task string, records []toolExecRecord) ([]string, error) {
	if a.fallbackModel == nil {
		return nil, fmt.Errorf("no fallback model configured for verifier")
	}

	recordsJSON, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("marshal tool records: %w", err)
	}

	systemPrompt := `You are a strict end-of-run verifier for an automated agent. ` +
		`Given the agent's original task and the list of tool calls it executed, ` +
		`decide whether every action the task explicitly required was actually ` +
		`attempted with a successful result. ` +
		`Focus on user-visible deliverables the task demands (sending emails, ` +
		`generating presentations, creating deals, writing reports to named ` +
		`recipients, etc.), not on internal planning steps. ` +
		"\n\n" +
		`Respond with a single JSON object and no other text, matching: ` +
		`{"missing_actions": [string, ...], "reasoning": string}. ` +
		`Use [] when the task is complete. ` +
		`Each missing action should be a concise imperative phrase naming the ` +
		`tool or deliverable that is missing (e.g. "send_email to trading team", ` +
		`"generate_wrap_up_presentation"). ` +
		`Do not invent requirements the task did not state.`

	userPrompt := fmt.Sprintf(
		"ORIGINAL TASK (possibly truncated):\n---\n%s\n---\n\nTOOL EXECUTIONS (JSON):\n%s",
		truncateTaskForVerifier(task),
		string(recordsJSON),
	)

	verifyCtx, cancel := context.WithTimeout(ctx, verifierTimeout)
	defer cancel()

	verifyAgent := fantasy.NewAgent(a.fallbackModel, fantasy.WithSystemPrompt(systemPrompt))
	out, err := verifyAgent.Generate(verifyCtx, fantasy.AgentCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage(userPrompt)},
	})
	if err != nil {
		return nil, fmt.Errorf("verifier call failed: %w", err)
	}
	raw := strings.TrimSpace(out.Response.Content.Text())
	if raw == "" {
		return nil, fmt.Errorf("verifier returned empty response")
	}

	parsed, err := parseVerifierResult(raw)
	if err != nil {
		return nil, fmt.Errorf("verifier output parse: %w (raw=%q)", err, summarizeForConsole(raw, 200))
	}
	log.Printf("Verifier: missing=%v reasoning=%q", parsed.Missing, summarizeForConsole(parsed.Reasoning, 200))
	return parsed.Missing, nil
}

func parseVerifierResult(raw string) (verifierResult, error) {
	candidate := strings.TrimSpace(raw)
	candidate = strings.TrimPrefix(candidate, "```json")
	candidate = strings.TrimPrefix(candidate, "```")
	candidate = strings.TrimSuffix(candidate, "```")
	candidate = strings.TrimSpace(candidate)

	start := strings.Index(candidate, "{")
	end := strings.LastIndex(candidate, "}")
	if start < 0 || end <= start {
		return verifierResult{}, fmt.Errorf("no JSON object found")
	}
	candidate = candidate[start : end+1]

	var result verifierResult
	if err := json.Unmarshal([]byte(candidate), &result); err != nil {
		return verifierResult{}, err
	}
	cleaned := result.Missing[:0]
	for _, m := range result.Missing {
		if s := strings.TrimSpace(m); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	result.Missing = cleaned
	return result, nil
}
