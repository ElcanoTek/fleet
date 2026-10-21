package taskrun

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// #1826 end to end: a scheduled task, through the OpenRouter adapter against
// the fake LLM, whose model answers before the self-audit and closes the audit
// round with only a pointer back to that answer. The run's final assistant
// message, which the runner stores as the task result and the Operations
// Center shows as the answer, must carry the answer, not the pointer alone.
func TestAnswerBeforeAuditIsStored_FakeLLM(t *testing.T) {
	fake := startSubagentFakeLLM(t)
	const table, pointer = "TABLE-1826: seed 1 = 42, seed 2 = 17", "Audit ok, the table is in my previous message."
	fake.Scenario("answer-first", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep(table),
		auditStep("audit-1826"),
		fakellm.TextStep(pointer),
	}})

	session := runTaskToLog(t, writeTaskFileLines(t,
		`prompt: "[[scenario:answer-first]] compute every seed and answer with a table"`,
		`model: anthropic/claude-opus-4.8`))

	final, audited := "", false
	for _, m := range session.Messages {
		if m.MessageType != nil && *m.MessageType == "system_enforcement" && strings.HasPrefix(m.Content, "Before finishing: ") {
			audited = true
		}
		if m.Role == "assistant" && m.Content != "" {
			final = m.Content
		}
	}
	if !audited {
		t.Fatal("the run was never asked for its self-audit, so this is not the #1826 shape")
	}
	if want := table + "\n\n" + pointer; final != want {
		t.Fatalf("final assistant message = %q, want the answer followed by the audit's closing text %q", final, want)
	}
	if got := fake.Hits("answer-first"); got != 3 {
		t.Fatalf("the scenario served %d requests, want 3 (answer, audit, closing)", got)
	}
}
