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

func TestBuildScheduledCompactionSummarizer_NilModelDegradesToPlaceholder(t *testing.T) {
	in := agentcore.CompactionSummarizeInput{Droppable: []fantasy.Message{fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b")}}
	text := msgTextOf(buildScheduledCompactionSummarizer(nil)(context.Background(), in))
	if !strings.HasPrefix(text, compactionSummaryPrefix) || !strings.Contains(text, "2 messages compacted") {
		t.Fatalf("nil-model summary = %q, want the tagged placeholder", text)
	}
}
