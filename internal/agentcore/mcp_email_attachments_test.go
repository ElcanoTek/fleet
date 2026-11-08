package agentcore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// attachmentReadingBroker records the attachment path an email call was
// dispatched with and reads the file there during the call, the way a
// connector would.
type attachmentReadingBroker struct {
	calls   int
	path    string
	content string
}

func (b *attachmentReadingBroker) CallMCP(_ context.Context, _, _ string, args map[string]any) (string, bool, error) {
	b.calls++
	if list, ok := args["attachments"].([]any); ok && len(list) > 0 {
		if obj, ok := list[0].(map[string]any); ok {
			b.path, _ = obj["path"].(string)
			data, err := os.ReadFile(b.path)
			if err == nil {
				b.content = string(data)
			}
		}
	}
	return "sent", false, nil
}

// An email call that runs without an approval card (session pre-approval,
// conversation auto-approve, a patrol email) gets the same attachment rule
// as an approved card: the path is resolved through the conversation's
// workspace, and the connector reads a private copy that is removed after.
func TestMCPTool_EmailAttachmentsPreparedBeforeDispatch(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	conv := "conv-a"
	if err := os.MkdirAll(filepath.Join(wsRoot, conv), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, conv, "report.csv"), []byte("a,b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(wsRoot, "conv-b")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "outbound")

	newEmailTool := func(b MCPBroker) *mcpTool {
		return &mcpTool{
			serverName: "sendgrid",
			tool:       mcp.Tool{Name: "send_email", Description: "d"},
			broker:     b,
			prepareEmail: func(args map[string]any) (func(), error) {
				return tools.SnapshotEmailAttachments(conv, args, staging)
			},
		}
	}
	input := func(path string) string {
		return `{"to_email":"x@example.com","subject":"s","content":"c","attachments":[{"path":"` + path + `"}]}`
	}

	for _, path := range []string{outside, "../conv-b/notes.txt", filepath.Join(other, "notes.txt")} {
		b := &attachmentReadingBroker{}
		resp, err := newEmailTool(b).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: input(path)})
		if err != nil {
			t.Fatalf("%s: Go error %v", path, err)
		}
		if !resp.IsError || b.calls != 0 {
			t.Fatalf("%s: want a refused call with no dispatch; isError=%v calls=%d text=%q", path, resp.IsError, b.calls, resp.Content)
		}
	}

	b := &attachmentReadingBroker{}
	resp, err := newEmailTool(b).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: input("report.csv")})
	if err != nil || resp.IsError {
		t.Fatalf("workspace attachment: err=%v resp=%q", err, resp.Content)
	}
	if b.calls != 1 || b.content != "a,b\n" {
		t.Fatalf("dispatch: calls=%d content=%q", b.calls, b.content)
	}
	if !strings.HasPrefix(b.path, staging+string(filepath.Separator)) || filepath.Base(b.path) != "report.csv" {
		t.Fatalf("connector got %q, want a copy named report.csv under %s", b.path, staging)
	}
	if _, err := os.Stat(b.path); !os.IsNotExist(err) {
		t.Fatalf("copy %s still present after the call (stat err %v)", b.path, err)
	}

	// Without the seam (a scheduled task run) the arguments pass unchanged.
	plain := &attachmentReadingBroker{}
	tool := newEmailTool(plain)
	tool.prepareEmail = nil
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: input("/data/x.csv")}); err != nil {
		t.Fatal(err)
	}
	if plain.calls != 1 || plain.path != "/data/x.csv" {
		t.Fatalf("no seam: calls=%d path=%q", plain.calls, plain.path)
	}
}

// The seam applies only to email tools.
func TestMCPTool_PrepareEmailOnlyForEmailTools(t *testing.T) {
	called := false
	b := &recordingBroker{text: "ok"}
	tool := newTestMCPTool(b, nil)
	tool.prepareEmail = func(map[string]any) (func(), error) { called = true; return nil, nil }
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: `{"attachments":[{"path":"/x"}]}`}); err != nil {
		t.Fatal(err)
	}
	if called || b.calls != 1 {
		t.Fatalf("non-email tool: prepare called=%v broker calls=%d", called, b.calls)
	}
}
