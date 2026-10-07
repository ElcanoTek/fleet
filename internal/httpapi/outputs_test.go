package httpapi

import (
	"encoding/json"
	"fmt"
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
		"Escapes everywhere: [a](<a\\_b.csv>) [r][esc]\n" +
		"\n[deck]: deck.pptx\n" +
		"[notes]: <notes file.md>\n" +
		"[esc]: ref\\_\\#1.csv\n" +
		"[orphan]: orphan.csv\n"
	var got []string
	for _, d := range markdownDestinations(md) {
		if p, ok := resolveWorkspaceRelPath(d); ok {
			got = append(got, p)
		}
	}
	want := []string{
		"out/report.xlsx",
		"chart.png",
		"full.png", // document order: the outer link opens before its image
		"thumb.png",
		"My File.csv",
		"my_file.csv",
		"deck.pptx",
		"notes file.md",
		"after.csv",
		"a_b.csv",    // escapes are dropped inside <…> too
		"ref_#1.csv", // and in a reference definition
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("destinations =\n %q\nwant\n %q", got, want)
	}
}

// Discovery matches only what the chat RENDERS as a link or image. Every
// construct below contains link-shaped source text that CommonMark (and the
// web's react-markdown + remark-gfm) renders as no link at all — so the file
// was never a chip, and must never become a shared output.
func TestMarkdownDestinationsOnlyRenderedLinks(t *testing.T) {
	notLinks := map[string]string{
		"backslash-escaped bracket": `See \[x](secret.csv) here.`,
		"escaped image bracket":     `See !\[x](secret.png) here.`,
		"inline HTML comment":       `Done <!-- [x](secret.csv) --> ok`,
		"block HTML comment":        "<!--\n[x](secret.csv)\n-->\n\nafter",
		"indented code block":       "Intro.\n\n    [x](secret.csv)\n\nOutro.",
		"fenced code block":         "```\n[x](secret.csv)\n```",
		"tilde fence":               "~~~\n[x](secret.csv)\n~~~",
		"inline code span":          "Use `[x](secret.csv)` literally.",
		"double-backtick span":      "Use ``a ` [x](secret.csv)`` literally.",
		"raw HTML block":            "<div>\n[x](secret.csv)\n</div>",
		"raw HTML anchor":           `<a href="secret.csv">x</a>`,
		"link in image alt":         `![see [x](secret.csv)](https://example.com/p.png)`,
		"unfenced HTML document":    "<!DOCTYPE html>\n<html>\n<body>\n\n[x](secret.csv)\n\n</body>\n</html>",
		"email autolink":            "Write to <secret@report.csv> or secret@report.csv.",
		"definition never used":     "[x]: secret.csv",
	}
	for name, md := range notLinks {
		for _, d := range markdownDestinations(md) {
			if p, ok := resolveWorkspaceRelPath(d); ok {
				t.Errorf("%s: %q yielded workspace path %q, want none", name, md, p)
			}
		}
	}

	// And the shapes that DO render as links still count.
	links := map[string]string{
		"escaped backslash before bracket": `See \\[x](a.csv)`, // \\ is a literal backslash; the [ is live
		"list continuation, 4 spaces":      "- item\n\n    [x](a.csv)",
		"label-colon code span":            "File: `[x](a.csv)`", // the renderer un-codes this value
		"angle autolink to sandbox path":   "<sandbox:/opt/chat/workspace/a.csv>",
		"footnote body":                    "Note[^1].\n\n[^1]: See [x](a.csv).",
		"table cell":                       "| f |\n|---|\n| [x](a.csv) |",
		"first definition wins":            "[x]\n\n[x]: a.csv\n[x]: https://example.com/",
		"entity in destination":            "[x](a&#46;csv)",
	}
	for name, md := range links {
		var got []string
		for _, d := range markdownDestinations(md) {
			if p, ok := resolveWorkspaceRelPath(d); ok {
				got = append(got, p)
			}
		}
		if !reflect.DeepEqual(got, []string{"a.csv"}) {
			t.Errorf("%s: %q yielded %q, want [a.csv]", name, md, got)
		}
	}
	// A later definition never overrides the first, even when the first is
	// external: the label renders as the external link, no chip.
	for _, d := range markdownDestinations("[x]\n\n[x]: https://example.com/\n[x]: secret.csv") {
		if p, ok := resolveWorkspaceRelPath(d); ok {
			t.Errorf("second definition took over: %q", p)
		}
	}
}

// The chat renders consecutive assistant text entries (across tool calls,
// until the next user message or compaction summary) as ONE message, so
// discovery parses them as one: a fence opened in one entry and closed in the
// next is one code block on screen, and a link inside it is not a chip.
func TestPresentedWorkspacePathsGroupsLikeTheChat(t *testing.T) {
	history := []agent.HistoryEntry{
		textEntry("assistant", "Here is the raw output:\n```\n"),
		{Role: "assistant", Type: "tool_call", Content: json.RawMessage(`{"id":"1","name":"bash","input":"{}"}`)},
		textEntry("assistant", "[x](secret.csv)\n```\nand the [real](real.csv) file."),
		textEntry("user", "thanks"),
		// A new message: an unclosed fence above does not swallow this.
		textEntry("assistant", "[next](next.csv)"),
	}
	got := presentedWorkspacePaths(history)
	if want := []string{"real.csv", "next.csv"}; !reflect.DeepEqual(got, want) {
		t.Errorf("presented = %q, want %q", got, want)
	}

	// A compaction summary starts a new message too.
	history = []agent.HistoryEntry{
		textEntry("assistant", "```\n"),
		{Role: "assistant", Type: "summary", Content: json.RawMessage(`{"text":"s"}`)},
		textEntry("assistant", "[after](after.csv)"),
	}
	if got := presentedWorkspacePaths(history); !reflect.DeepEqual(got, []string{"after.csv"}) {
		t.Errorf("presented across a summary = %q, want [after.csv]", got)
	}
}

// The content-free boundary a teammate's view and branch carry in place of a
// summary splits exactly as the summary does: an unclosed fence before it does
// not swallow a link after it.
func TestPresentedWorkspacePathsSplitsAtSummaryBoundary(t *testing.T) {
	history := []agent.HistoryEntry{
		textEntry("assistant", "```\n[hidden](inside.csv)"),
		{Role: "assistant", Type: agent.EntryTypeSummaryBoundary, Content: json.RawMessage(`{}`)},
		textEntry("assistant", "[after](after.csv)"),
	}
	if got := presentedWorkspacePaths(history); !reflect.DeepEqual(got, []string{"after.csv"}) {
		t.Errorf("presented across a boundary = %q, want [after.csv]", got)
	}
}

// The visit budget bounds what discovery READS, not just what it keeps: past
// maxDiscoveryReplies replies (or maxDiscoveryBytes of reply text) the walk
// stops, reports truncated, and keeps only the newest whole replies — a reply
// the byte budget would cut is dropped, never parsed headless.
func TestRecentRenderedRepliesBudget(t *testing.T) {
	var history []agent.HistoryEntry
	for i := 0; i < 5; i++ {
		history = append(history, textEntry("user", "q"), textEntry("assistant", fmt.Sprintf("[f](f%d.csv)", i)))
	}
	replies, truncated := recentRenderedReplies(history, 3, 1<<20)
	if !truncated || len(replies) != 3 || replies[0] != "[f](f4.csv)" || replies[2] != "[f](f2.csv)" {
		t.Errorf("reply budget: replies=%q truncated=%v, want the 3 newest and truncated", replies, truncated)
	}
	if replies, truncated := recentRenderedReplies(history, 5, 1<<20); truncated || len(replies) != 5 {
		t.Errorf("within budget: replies=%d truncated=%v, want 5 and not truncated", len(replies), truncated)
	}

	// Byte budget: an older reply whose head would fall past the budget is
	// dropped whole, so its opening fence can never be lost while its link
	// is kept.
	history = []agent.HistoryEntry{
		textEntry("user", "q"),
		textEntry("assistant", "```\n"),
		textEntry("assistant", "[x](inside.csv)\n"),
		textEntry("user", "q2"),
		textEntry("assistant", "[new](new.csv)"),
	}
	newest := len(history[4].Content)
	replies, truncated = recentRenderedReplies(history, 100, newest+len(history[2].Content))
	if !truncated || !reflect.DeepEqual(replies, []string{"[new](new.csv)"}) {
		t.Errorf("byte budget: replies=%q truncated=%v, want only the newest whole reply", replies, truncated)
	}

	// The truncation surfaces through the bounded path walk's flag.
	var long []agent.HistoryEntry
	for i := 0; i < maxDiscoveryReplies+1; i++ {
		long = append(long, textEntry("user", "q"), textEntry("assistant", "no links here"))
	}
	if paths, truncated := recentPresentedPaths(long, maxOutputReferences); len(paths) != 0 || !truncated {
		t.Errorf("a linkless chat past the reply budget: paths=%q truncated=%v, want none and truncated", paths, truncated)
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

	outs, truncated := conversationOutputs(conv, history, map[string]bool{"out/chart.png": true})
	if truncated {
		t.Error("a short transcript is not truncated")
	}
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

// Discovery is bounded: past maxOutputReferences distinct references only
// the most recent are considered (and stat'ed), the result says so, and an
// older reference is not an output for anyone — the team-files gate matches
// against this same bounded list. Uploads never count toward the bound.
func TestConversationOutputsBoundsReferences(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	const conv = "conv-cap"
	ws := filepath.Join(root, conv)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const extra = 7
	history := make([]agent.HistoryEntry, 0, maxOutputReferences+extra)
	for i := range maxOutputReferences + extra {
		name := fmt.Sprintf("f%04d.csv", i)
		if err := os.WriteFile(filepath.Join(ws, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// One upload reference per message: never an output, never counted.
		history = append(history, textEntry("assistant",
			fmt.Sprintf("[f](%s) [u](attachments/u%d.pdf)", name, i)))
	}
	outs, truncated := conversationOutputs(conv, history, nil)
	if !truncated {
		t.Error("more than maxOutputReferences references must report truncated")
	}
	if len(outs) != maxOutputReferences {
		t.Fatalf("outputs = %d, want %d", len(outs), maxOutputReferences)
	}
	got := map[string]bool{}
	for _, o := range outs {
		got[o.Path] = true
	}
	for i := range extra {
		if old := fmt.Sprintf("f%04d.csv", i); got[old] {
			t.Errorf("%s is among the oldest references and must be dropped", old)
		}
	}
	if newest := fmt.Sprintf("f%04d.csv", maxOutputReferences+extra-1); !got[newest] {
		t.Errorf("the newest reference %s must be kept", newest)
	}

	// Exactly at the bound: everything considered, nothing truncated.
	if _, truncated := conversationOutputs(conv, history[extra:], nil); truncated {
		t.Error("exactly maxOutputReferences references is not truncated")
	}
}
