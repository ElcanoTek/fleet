package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/config"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// mailBroker records each send_email dispatch's attachment path and reads
// the file there during the call, the way a connector would.
type mailBroker struct {
	mu       sync.Mutex
	sends    int
	refused  int // sends the attachment step refused
	paths    []string
	contents []string
}

func (b *mailBroker) CallMCP(_ context.Context, _, tool string, args map[string]any) (string, bool, error) {
	if tool != "send_email" {
		return "ok", false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends++
	if list, ok := args["attachments"].([]any); ok && len(list) > 0 {
		if obj, ok := list[0].(map[string]any); ok {
			p, _ := obj["path"].(string)
			data, _ := os.ReadFile(p)
			b.paths = append(b.paths, p)
			b.contents = append(b.contents, string(data))
		}
	}
	return `{"status":"sent"}`, false, nil
}

// runScheduledEmail drives one scheduled Execute whose model sends an email
// with the given attachment path, with the run's attachment step bound to
// jobDir under wsRoot.
func runScheduledEmail(t *testing.T, wsRoot, jobDir, staging, attachment string) *mailBroker {
	t.Helper()
	input := `{"to_email":"ops@example.com","subject":"report","content":"see attached","attachments":[{"path":"` + attachment + `"}]}`
	steps := []struct{ tool, input string }{
		{"confirm_audit", `{"success":true,"critical_actions":[{"tool":"mcp_mail_send_email"}],"reasoning":"Checked the report","artifacts_checked":["report.csv"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":["report.csv"],"remaining_risks":[]}`},
		{"mcp_mail_send_email", input},
	}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			if step < len(steps) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: fmt.Sprint(step), ToolCallName: steps[step].tool, ToolCallInput: steps[step].input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Sent."})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	broker := &mailBroker{}
	a.mcpBroker = broker
	a.mcpCatalog = []mcp.ServerTool{{ServerName: "mail", Tool: mcp.Tool{Name: "send_email", Description: "Send an email", InputSchema: map[string]any{"type": "object"}}}}
	a.prepareEmailAttachments = func(args map[string]any) (func(), error) {
		cleanup, err := tools.SnapshotEmailAttachmentsInDir(wsRoot, jobDir, args, staging)
		if err != nil {
			broker.mu.Lock()
			broker.refused++
			broker.mu.Unlock()
		}
		return cleanup, err
	}
	_ = a.Execute(context.Background(), "Email the report.")
	return broker
}

// A scheduled run's email attachments resolve through its own job directory:
// one there is copied, sent and removed afterwards; one in another job's
// directory, or reached through a link out of the job directory, is refused
// before the connector is called.
func TestScheduledRun_EmailAttachmentsFromTheJobDirectory(t *testing.T) {
	wsRoot := t.TempDir()
	jobDir := filepath.Join(wsRoot, "tasks", "job-a")
	otherJob := filepath.Join(wsRoot, "tasks", "job-b")
	for _, d := range []string{jobDir, otherJob} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(jobDir, "report.csv"), []byte("a,b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherJob, "data.csv"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(otherJob, "data.csv"), filepath.Join(jobDir, "link.csv")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "outbound")

	for _, path := range []string{filepath.Join(otherJob, "data.csv"), "../job-b/data.csv", "link.csv"} {
		if b := runScheduledEmail(t, wsRoot, jobDir, staging, path); b.sends != 0 || b.refused == 0 {
			t.Fatalf("%s: sends=%d refused=%d, want the attachment step to refuse it", path, b.sends, b.refused)
		}
	}

	b := runScheduledEmail(t, wsRoot, jobDir, staging, "report.csv")
	if b.sends != 1 || len(b.contents) != 1 || b.contents[0] != "a,b\n" {
		t.Fatalf("job-dir attachment: sends=%d contents=%q", b.sends, b.contents)
	}
	if !strings.HasPrefix(b.paths[0], staging+string(filepath.Separator)) || filepath.Base(b.paths[0]) != "report.csv" {
		t.Fatalf("connector got %q, want a copy named report.csv under %s", b.paths[0], staging)
	}
	if _, err := os.Stat(b.paths[0]); !os.IsNotExist(err) {
		t.Fatalf("copy %s still present after the run (stat err %v)", b.paths[0], err)
	}
}

// A scheduled run's sub-agents, at every depth, carry the run's attachment
// step.
func TestScheduledRun_SubagentsInheritTheEmailAttachmentStep(t *testing.T) {
	parent := NewAgent(Options{
		Config:                  &config.Config{MaxIterations: 10},
		SystemPrompt:            "task",
		Subagent:                SubagentOptions{Enabled: true, MaxDepth: 2, MaxChildren: 5},
		PrepareEmailAttachments: func(map[string]any) (func(), error) { return func() {}, nil },
	})
	child := parent.buildChild(SubagentRoleWorker, nil, nil, nil, 0, 0, 0)
	grandchild := child.buildChild(SubagentRoleExplore, nil, nil, nil, 0, 0, 0)
	if child.prepareEmailAttachments == nil || grandchild.prepareEmailAttachments == nil {
		t.Fatal("a scheduled run's sub-agent carries no email attachment step")
	}
}
