package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// resolveWorkspaceRelPath must agree with web/src/app/chat/ui/workspaceHref.ts
// (resolveScopedWorkspaceHref) on every shape: what the owner sees as a file
// chip is exactly what can become a shared output, and nothing else.
func TestResolveWorkspaceRelPath(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	cases := []struct {
		raw  string
		want string // "" = not a workspace path
	}{
		{"report.xlsx", "report.xlsx"},
		{"out/chart.png", "out/chart.png"},
		{"./report.xlsx", ""}, // a "." segment is refused, like the TS
		{"My%20File.csv", "My File.csv"},
		{"My File.csv", "My File.csv"},
		{"100%.png", "100%.png"},                   // an invalid escape keeps the raw segment
		{"rate%2520final.csv", "rate%20final.csv"}, // decoded ONCE for the path
		{"sandbox:/opt/chat/workspace/" + id + "/q3.xlsx", "q3.xlsx"},
		{"sandbox:q3.xlsx", "q3.xlsx"},
		{"/opt/chat/workspace/" + id + "/out/q3.xlsx", "out/q3.xlsx"},
		{"opt/chat/workspace/q3.xlsx", "q3.xlsx"},
		{"SANDBOX:/OPT/CHAT/WORKSPACE/q3.xlsx", "q3.xlsx"},
		// Absolute / non-workspace references never resolve.
		{"https://example.com/a.png", ""},
		{"HTTP://example.com/a.png", ""},
		{"mailto:a@b.c", ""},
		{"data:image/png;base64,AAAA", ""},
		{"javascript:alert(1)", ""},
		{"//evil.example/x.png", ""},
		{"/api/auth/elcano-login", ""},
		{"#section", ""},
		{"?q=1", ""},
		{"", ""},
		{"/", ""},
		// Traversal, plain and encoded, at any depth.
		{"../secret", ""},
		{"a/../../etc/passwd", ""},
		{"%2e%2e/secret", ""},
		{"%252e%252e/secret", ""},
		{"a/%2E/b", ""},
		{"a%2F..%2Fb", ""}, // a decoded segment carrying "/.." is refused too
		{"a%00b", ""},
		{`a\b`, ""},
		// Empty segments collapse, as the TS filter does.
		{"out//chart.png", "out/chart.png"},
		{"attachments/x/upload.pdf", "attachments/x/upload.pdf"}, // resolved; filtered later as an upload
	}
	for _, c := range cases {
		got, ok := resolveWorkspaceRelPath(c.raw)
		if c.want == "" {
			if ok {
				t.Errorf("resolve(%q) = %q, want not a workspace path", c.raw, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("resolve(%q) = (%q, %v), want %q", c.raw, got, ok, c.want)
		}
	}
}

func TestMarkdownDestinations(t *testing.T) {
	md := "Here is the [report](out/report.xlsx \"Q3\") and a chart:\n" +
		"![chart](chart.png)\n" +
		"[![thumb](thumb.png)](full.png)\n" +
		"Angle: [spaced](<My File.csv>) escaped: [e](my\\_file.csv)\n" +
		"Ref: [the deck][deck] and [notes] and [unused-ref-usage][missing]\n" +
		"Inline code `[x](code.csv)` is not a chip.\n" +
		"```\n[y](fenced.csv)\n```\n" +
		"````\n```\n[z](nested.csv)\n```\n````\n" +
		"After fences: [after](after.csv)\n" +
		"\n[deck]: deck.pptx\n" +
		"[notes]: <notes file.md>\n" +
		"[orphan]: orphan.csv\n"
	got := markdownDestinations(md)
	want := []string{
		"out/report.xlsx",
		"chart.png",
		"thumb.png",
		"full.png",
		"My File.csv",
		"my_file.csv",
		"deck.pptx",
		"notes file.md",
		"after.csv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("destinations =\n %q\nwant\n %q", got, want)
	}
}

func textEntry(role, text string) agent.HistoryEntry {
	raw, _ := json.Marshal(agent.TextContent{Text: text})
	return agent.HistoryEntry{Role: role, Type: "text", Content: raw}
}

// Only the assistant's TEXT replies present files: a link in the user's own
// message, a tool result or reasoning is not a chip.
func TestPresentedWorkspacePathsOnlyAssistantText(t *testing.T) {
	history := []agent.HistoryEntry{
		textEntry("user", "look at [mine](user.csv)"),
		{Role: "assistant", Type: "tool_result", Content: json.RawMessage(`{"text":"[t](tool.csv)"}`)},
		{Role: "assistant", Type: "reasoning", Content: json.RawMessage(`{"text":"[r](reason.csv)"}`)},
		textEntry("assistant", "Saved [a](a.csv) and [a again](a.csv) plus [web](https://x.y/z.csv)"),
		textEntry("assistant", "and ![b](sandbox:/opt/chat/workspace/b.png)"),
	}
	got := presentedWorkspacePaths(history)
	if want := []string{"a.csv", "b.png"}; !reflect.DeepEqual(got, want) {
		t.Errorf("presented = %q, want %q", got, want)
	}
}

// conversationOutputs keeps presented paths that exist as regular files now,
// drops uploads, missing files, directories and anything reached through a
// symlink, and applies exclusions.
func TestConversationOutputsFiltersOnDisk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	const conv = "conv-1"
	ws := filepath.Join(root, conv)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("report.xlsx", "r")
	write("out/chart.png", "c")
	write("attachments/abc/upload.pdf", "u")
	write("unlinked.csv", "never presented")
	if err := os.MkdirAll(filepath.Join(ws, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "attachments", "abc", "upload.pdf"), filepath.Join(ws, "alias.pdf")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, "attachments"), filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	history := []agent.HistoryEntry{textEntry("assistant",
		"[r](report.xlsx) ![c](out/chart.png) [u](attachments/abc/upload.pdf) [gone](missing.csv) "+
			"[d](adir) [alias](alias.pdf) [via](linkdir/abc/upload.pdf)")}

	outs := conversationOutputs(conv, history, map[string]bool{"out/chart.png": true})
	got := map[string]bool{}
	for _, o := range outs {
		got[o.Path] = o.Shared
	}
	want := map[string]bool{"report.xlsx": true, "out/chart.png": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("outputs = %v, want %v", got, want)
	}
	if countShared(outs) != 1 {
		t.Errorf("shared = %d, want 1", countShared(outs))
	}
}

// The strict opener refuses a symlink at ANY component — including one that
// stays inside the workspace, which os.Root alone would follow.
func TestOpenWorkspaceFileNoFollow(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out", "ok.csv"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape.csv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, "out", "ok.csv"), filepath.Join(ws, "inner.csv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "out"), filepath.Join(ws, "dirlink")); err != nil {
		t.Fatal(err)
	}

	f, info, err := openWorkspaceFileNoFollow(ws, "out/ok.csv")
	if err != nil {
		t.Fatalf("regular file: %v", err)
	}
	_ = f.Close()
	if info.Size() != 2 {
		t.Errorf("size = %d", info.Size())
	}
	for _, rel := range []string{"escape.csv", "inner.csv", "dirlink/ok.csv", "out", "../x", "out/../out/ok.csv", "missing"} {
		if f, _, err := openWorkspaceFileNoFollow(ws, rel); err == nil {
			_ = f.Close()
			t.Errorf("open(%q) succeeded, want refusal", rel)
		}
	}
}
