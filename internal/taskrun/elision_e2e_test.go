package taskrun

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// #1742's acceptance case, end to end: a scheduled task, through the
// OpenRouter adapter against the fake LLM, whose middle is dominated by large
// tool results. Its resend-budget checkpoint fires (the fake reports a prompt
// size that grows with the request, SizedPromptTokens), the run elides the
// stale results, and it gets back under the budget with zero summarizer
// invocations, then finishes.
func TestElision_LargeToolResultsNeedNoSummary_FakeLLM(t *testing.T) {
	fake := startSubagentFakeLLM(t)
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "40000")
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	// The bash tool appends to an audit log, under ./data/audit unless told
	// otherwise: keep it out of the package directory.
	t.Setenv("FLEET_AUDIT_DIR", t.TempDir())

	const pages = 12
	steps := make([]fakellm.Step, 0, pages+2)
	for i := 1; i <= pages; i++ {
		// ~20 KB of output each, a distinct command so the loop guard does
		// not see a repeated call.
		steps = append(steps, fakellm.ToolStep(fakellm.ToolCall{
			ID: fmt.Sprintf("page-%d", i), Name: "bash",
			Arguments: fmt.Sprintf(`{"command":"yes page%02d | head -c 20000"}`, i),
		}))
	}
	steps = append(steps, auditStep("audit-elide"), fakellm.TextStep("ELIDE-DONE"))
	fake.Scenario("elide", fakellm.Scenario{SizedPromptTokens: true, Steps: steps})

	session := runTaskToLog(t, writeTaskFileLines(t,
		`prompt: "[[scenario:elide]] refresh every page"`,
		`model: anthropic/claude-opus-4.8`))

	var elided, checkpoints []string
	joined := ""
	for _, m := range session.Messages {
		joined += m.Content + "\n"
		switch {
		case strings.HasPrefix(m.Content, "[context_elided] "):
			elided = append(elided, m.Content)
		case strings.HasPrefix(m.Content, "[context_checkpoint] "):
			checkpoints = append(checkpoints, m.Content)
		case strings.HasPrefix(m.Content, "[context_compacted] "),
			m.MessageType != nil && *m.MessageType == "compaction_summary":
			t.Fatalf("a summary was bought: %.200q", m.Content)
		}
	}
	if len(checkpoints) == 0 || len(elided) != len(checkpoints) {
		t.Fatalf("checkpoints %d, elisions %d: want the budget reached and one elision per pause\n%s", len(checkpoints), len(elided), strings.Join(checkpoints, "\n"))
	}
	for _, line := range elided {
		if !strings.HasSuffix(line, "that freed enough, so no summary was needed") {
			t.Fatalf("an elision did not get back under the budget: %q", line)
		}
	}
	t.Logf("%d checkpoint(s); elisions:\n%s", len(checkpoints), strings.Join(elided, "\n"))
	if !strings.Contains(joined, "ELIDE-DONE") {
		t.Fatal("the run did not finish with its answer")
	}
	if got := fake.Hits("elide"); got < pages+2 {
		t.Fatalf("the scenario served %d requests, want every step (%d) once at least", got, pages+2)
	}
}
