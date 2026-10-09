package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// promptCapturingModel records the system prompt of each Generate call so a
// test can see what the summarizer asked for.
type promptCapturingModel struct {
	itMockModel
	pmu     sync.Mutex
	systems []string
}

func (m *promptCapturingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	for _, msg := range call.Prompt {
		if msg.Role == fantasy.MessageRoleSystem {
			m.pmu.Lock()
			m.systems = append(m.systems, msgTextOf(msg))
			m.pmu.Unlock()
		}
	}
	return m.itMockModel.Generate(ctx, call)
}

func msgTextOf(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

func TestBuildScheduledCompactionSummarizer_UsesTheModelWithTheUnattendedAddendum(t *testing.T) {
	model := &promptCapturingModel{itMockModel: itMockModel{generateText: "condensed run state"}}
	var metered int
	in := agentcore.CompactionSummarizeInput{
		Droppable:   []fantasy.Message{fantasy.NewUserMessage("step 1 result: wrote /w/report.csv"), fantasy.NewUserMessage("step 2 result: version 859 published")},
		RecordUsage: func(fantasy.Usage, fantasy.ProviderMetadata) { metered++ },
	}
	msg := buildScheduledCompactionSummarizer(model)(context.Background(), in)
	text := msgTextOf(msg)
	if !strings.HasPrefix(text, compactionSummaryPrefix) || !strings.Contains(text, "condensed run state") {
		t.Fatalf("summary message = %q, want the tagged model summary", text)
	}
	if metered != 1 {
		t.Errorf("summarizer usage metered %d times, want 1", metered)
	}
	model.pmu.Lock()
	defer model.pmu.Unlock()
	if len(model.systems) != 1 {
		t.Fatalf("summarizer made %d model calls, want 1", len(model.systems))
	}
	sys := model.systems[0]
	for _, want := range []string{"Critical Context", "UNATTENDED scheduled task", "Record which tool calls succeeded"} {
		if !strings.Contains(sys, want) {
			t.Errorf("scheduled summarizer system prompt lacks %q", want)
		}
	}
}

// The summarizer never sees the pinned task prompt, so it must not report on
// it — and the agent must read the summary as subordinate to it. TWC task
// b50b6d78 (2026-10-08) skipped its mandatory send because a summary called the
// prompt's own recipient list "not confirmed".
func TestScheduledCompactionSummary_CannotOverrideTheTaskPrompt(t *testing.T) {
	model := &promptCapturingModel{itMockModel: itMockModel{generateText: "## Progress\nreport validated"}}
	in := agentcore.CompactionSummarizeInput{Droppable: []fantasy.Message{fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b")}}
	text := msgTextOf(buildScheduledCompactionSummarizer(model)(context.Background(), in))

	preamble := strings.Index(text, scheduledSummaryPreamble)
	body := strings.Index(text, "report validated")
	if !strings.HasPrefix(text, compactionSummaryPrefix) || preamble < 0 || body < preamble {
		t.Fatalf("summary message = %q, want the tag, then the task-prompt preamble, then the summary", text)
	}

	model.pmu.Lock()
	defer model.pmu.Unlock()
	sys := model.systems[0]
	for _, want := range []string{
		"you are not shown it",
		"never write that an input, recipient, approval, permission or instruction is missing, unknown, unconfirmed or unauthorized",
		"do not restate, doubt or mark any part of it as pending confirmation",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("scheduled summarizer system prompt lacks %q", want)
		}
	}
}

// Chat keeps the summary unranked: a later user turn may overrule the first.
func TestInteractiveCompactionSummary_HasNoTaskPromptPreamble(t *testing.T) {
	model := &promptCapturingModel{itMockModel: itMockModel{generateText: "condensed"}}
	in := agentcore.CompactionSummarizeInput{Droppable: []fantasy.Message{fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b")}}
	text := msgTextOf(buildInteractiveCompactionSummarizer(TurnConfig{Model: model})(context.Background(), in))
	if strings.Contains(text, scheduledSummaryPreamble) {
		t.Fatalf("interactive summary = %q, must not carry the scheduled preamble", text)
	}
}

func TestBuildScheduledCompactionSummarizer_NilModelDegradesToPlaceholder(t *testing.T) {
	in := agentcore.CompactionSummarizeInput{Droppable: []fantasy.Message{fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b")}}
	text := msgTextOf(buildScheduledCompactionSummarizer(nil)(context.Background(), in))
	if !strings.HasPrefix(text, compactionSummaryPrefix) || !strings.Contains(text, "2 messages compacted") {
		t.Fatalf("nil-model summary = %q, want the tagged placeholder", text)
	}
}

func TestScheduledCompactionSummarizer_FollowsTheActiveModel(t *testing.T) {
	configured := &promptCapturingModel{itMockModel: itMockModel{generateText: "from primary"}}
	active := &promptCapturingModel{itMockModel: itMockModel{generateText: "from fallback"}}
	in := agentcore.CompactionSummarizeInput{
		Droppable: []fantasy.Message{fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b")},
		Model:     active,
	}
	text := msgTextOf(buildScheduledCompactionSummarizer(configured)(context.Background(), in))
	if !strings.Contains(text, "from fallback") {
		t.Fatalf("summary = %q, want the ACTIVE model's text after a swap", text)
	}
	configured.mu.Lock()
	n := configured.generateCount
	configured.mu.Unlock()
	if n != 0 {
		t.Errorf("configured model was called %d times; a swapped run must not summarize on it", n)
	}
}
