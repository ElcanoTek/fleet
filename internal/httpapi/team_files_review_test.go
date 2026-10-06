package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// filterLikeDiscoveryStore is LoadDiscoveryHistory in memory: the rows it
// selects (with its content narrowing and summary mapping), newest first,
// stopped by the cutoff, back in ascending order.
func filterLikeDiscoveryStore(history []agent.HistoryEntry, more func(agent.HistoryEntry) bool) []agent.HistoryEntry {
	var newestFirst []agent.HistoryEntry
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		switch {
		case e.Type == "text" && e.Role == "assistant":
		case e.Type == "text" && e.Role == "user":
			e.Content = json.RawMessage(`{}`)
		case e.Type == "summary" || e.Type == agent.EntryTypeSummaryBoundary:
			e = agent.HistoryEntry{ID: e.ID, Role: e.Role, Type: agent.EntryTypeSummaryBoundary, Content: json.RawMessage(`{}`)}
		default:
			continue
		}
		newestFirst = append(newestFirst, e)
		if !more(e) {
			break
		}
	}
	out := make([]agent.HistoryEntry, len(newestFirst))
	for i, e := range newestFirst {
		out[len(newestFirst)-1-i] = e
	}
	return out
}

// The narrow read must not change what discovery finds: for random histories
// and small budgets, recentRenderedReplies over the narrowed, cut-off rows
// answers exactly what it answers over the full history — replies and the
// truncated flag alike.
func TestDiscoveryCutoffMatchesTheFullWalk(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	kinds := []struct{ role, typ string }{
		{"user", "text"}, {"assistant", "text"}, {"assistant", "text"}, {"assistant", "text"},
		{"assistant", "tool_call"}, {"tool", "tool_result"}, {"assistant", "reasoning"},
		{"user", "summary"}, {"assistant", "text"}, {"system", "text"},
	}
	for iter := range 3000 {
		n := rng.Intn(30)
		history := make([]agent.HistoryEntry, 0, n)
		for i := range n {
			k := kinds[rng.Intn(len(kinds))]
			var content json.RawMessage
			switch rng.Intn(5) {
			case 0:
				content = json.RawMessage(`{"text":""}`) // an empty reply part
			case 1:
				content = json.RawMessage(`not json`)
			default:
				raw, _ := json.Marshal(agent.TextContent{Text: fmt.Sprintf("[f%d](f%d.csv) %s", i, i, strings.Repeat("x", rng.Intn(40)))})
				content = raw
			}
			history = append(history, agent.HistoryEntry{ID: int64(i + 1), Role: k.role, Type: k.typ, Content: content})
		}
		maxReplies, maxBytes := 1+rng.Intn(5), 20+rng.Intn(300)
		wantReplies, wantTrunc := recentRenderedReplies(history, maxReplies, maxBytes)
		cut := discoveryCutoff{maxReplies: maxReplies, maxBytes: maxBytes}
		narrow := filterLikeDiscoveryStore(history, cut.more)
		gotReplies, gotTrunc := recentRenderedReplies(narrow, maxReplies, maxBytes)
		if !reflect.DeepEqual(gotReplies, wantReplies) || gotTrunc != wantTrunc {
			t.Fatalf("iter %d (replies %d, bytes %d): narrow = %q/%v, full = %q/%v", iter, maxReplies, maxBytes,
				gotReplies, gotTrunc, wantReplies, wantTrunc)
		}
	}
}

// Against the real store, with paging forced small: discovery over the
// narrow read finds the same paths (and truncated flag) as over LoadHistory,
// for a representative chat — tool rows, a compaction summary, a reply split
// across entries, user links that are not chips.
func TestDiscoveryHistoryMatchesLoadHistory(t *testing.T) {
	f := newFilesFixture(t)
	entries := []agent.HistoryEntry{
		textEntry("user", "please use [mine](user-linked.csv)"),
		{Role: "assistant", Type: "tool_call", Content: json.RawMessage(`{"tool":"bash","args":"[t](tool.csv)"}`)},
		{Role: "tool", Type: "tool_result", Content: json.RawMessage(`{"output":"[r](result.csv)"}`)},
		textEntry("assistant", "```\n[in a fence](fenced.csv)\n"),
		textEntry("assistant", "```\nafter: [real](after-fence.csv)"),
		{Role: "user", Type: "summary", Content: json.RawMessage(`{"text":"[s](summary.csv)"}`)},
		textEntry("assistant", "![chart](chart.png) and [dup](out/report.csv)"),
	}
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, entries); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.SetDiscoveryHistoryPageForTest(2))
	full, err := f.st.LoadHistory(f.ctx, f.chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := f.srv.discoveryHistory(f.ctx, f.chat.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, include := range []bool{false, true} {
		wantP, wantT := boundedPresentedPaths(full, maxOutputReferences, include)
		gotP, gotT := boundedPresentedPaths(narrow, maxOutputReferences, include)
		if !reflect.DeepEqual(gotP, wantP) || gotT != wantT {
			t.Errorf("includeUploads=%v: narrow %v/%v, full %v/%v", include, gotP, gotT, wantP, wantT)
		}
	}
	wantOuts, _ := conversationOutputs(f.chat.ID, full, map[string]bool{"held.json": true})
	gotOuts, _, err := f.srv.ownerOutputs(f.ctx, f.chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOuts, wantOuts) {
		t.Errorf("ownerOutputs = %+v, want %+v", gotOuts, wantOuts)
	}
	// Through a point: the same as the full history cut there.
	through := full[3].ID
	narrow, err = f.srv.discoveryHistory(f.ctx, f.chat.ID, through)
	if err != nil {
		t.Fatal(err)
	}
	var cut []agent.HistoryEntry
	for _, e := range full {
		if e.ID <= through {
			cut = append(cut, e)
		}
	}
	wantP, _ := boundedPresentedPaths(cut, maxOutputReferences, true)
	gotP, _ := boundedPresentedPaths(narrow, maxOutputReferences, true)
	if !reflect.DeepEqual(gotP, wantP) {
		t.Errorf("through: narrow %v, full %v", gotP, wantP)
	}
}

// HEAD reaches the team-files handler (it was 405'd by the router): same
// gate, headers only.
func TestTeamFilesHead(t *testing.T) {
	f := newFilesFixture(t)
	w := convSub(t, f.srv, "HEAD", "bob@x.com", f.chat.ID, "team-files/out/report.csv", "")
	if w.Code != 200 || w.Header().Get("Content-Length") != "8" || w.Body.Len() != 0 {
		t.Errorf("HEAD shared: %d len=%q body=%d", w.Code, w.Header().Get("Content-Length"), w.Body.Len())
	}
	for _, c := range []struct{ user, rel string }{
		{"bob@x.com", "held.json"}, {"zoe@x.com", "out/report.csv"}, {"bob@x.com", "attachments/u1/in.pdf"},
	} {
		if w := convSub(t, f.srv, "HEAD", c.user, f.chat.ID, "team-files/"+c.rel, ""); w.Code != 404 {
			t.Errorf("HEAD %s as %s: %d, want 404", c.rel, c.user, w.Code)
		}
	}
	if w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "team-files/out/report.csv", ""); w.Code != 405 {
		t.Errorf("POST: %d, want 405", w.Code)
	}
}

// The branch copy re-checks the team gate before every file: an owner who
// stops sharing mid-copy closes the door on the files not yet copied.
func TestTeammateBranchStopsCopyingWhenUnsharedMidCopy(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	copies := 0
	branchCopyAfterStat = func(string) {
		copies++
		if copies == 1 {
			if _, err := f.st.SetConversationTeamVisible(f.ctx, "alice@x.com", f.chat.ID, false); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { branchCopyAfterStat = nil })
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	branchCopyAfterStat = nil
	if w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	o := decode[store.Conversation](t, w).BranchOrigin
	if o == nil {
		t.Fatal("no origin")
	}
	if copies != 1 || len(o.CopiedFiles) != 1 {
		t.Fatalf("copied %d files (%d copy attempts), want only the one in flight when the owner unshared", len(o.CopiedFiles), copies)
	}
	withheld := map[string]bool{}
	for _, p := range o.WithheldFiles {
		withheld[p] = true
	}
	for _, p := range []string{"out/report.csv", "chart.png", "page.html"} {
		if p != o.CopiedFiles[0].Path && !withheld[p] {
			t.Errorf("%s neither copied nor withheld: %+v", p, o)
		}
	}
}

// A request cancelled once the branch exists still gets its files: the
// discovery and the copy run detached from the request's cancellation.
func TestCarrySharedFilesIgnoresRequestCancellation(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	point := msgs[len(msgs)-1].ID
	branch, err := f.st.BranchConversation(f.ctx, "bob@x.com", f.chat.ID, point, "b")
	if err != nil {
		t.Fatal(err)
	}
	src, err := f.st.GetTeamVisibleConversationMeta(f.ctx, "bob@x.com", f.chat.ID)
	if err != nil || src == nil {
		t.Fatalf("meta: %v %v", src, err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	o := f.srv.carrySharedFilesIntoBranch(ctx, src, branch, point)
	if len(o.CopiedFiles) != 3 {
		t.Errorf("copied = %+v, want all three shared outputs despite the cancelled request", o.CopiedFiles)
	}
}

// A FIFO swapped in for a shared name between the check and the open must
// not hang the reader: the open is non-blocking and the identity check
// refuses it. Regular files still read normally.
func TestOpenWorkspaceFileNoFollowFIFODoesNotHang(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "out.csv"), []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _, err := openWorkspaceFileNoFollow(ws, "out.csv")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	n, _ := f.Read(b)
	_ = f.Close()
	if string(b[:n]) != "a,b\n" {
		t.Fatalf("regular read = %q", b[:n])
	}

	workspaceOpenAfterLstat = func(p string) {
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Errorf("mkfifo: %v", err)
		}
	}
	t.Cleanup(func() { workspaceOpenAfterLstat = nil })
	done := make(chan error, 1)
	go func() {
		f, _, err := openWorkspaceFileNoFollow(ws, "out.csv")
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened as a workspace file")
		}
	case <-time.After(5 * time.Second):
		// Unblock the stuck open so the test process can exit.
		if w, err := os.OpenFile(filepath.Join(ws, "out.csv"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("open of a swapped-in FIFO blocked")
	}
}

// The owner's private skills, materialized into every workspace, are never
// outputs: not listed, not shared, refused by the download gate, not copied
// into a branch, and not in Sources.
func TestUserSkillsAreNeverOutputs(t *testing.T) {
	f := newFilesFixture(t)
	ws := filepath.Join(f.root, f.chat.ID)
	p := filepath.Join(ws, userSkillsRoot, "pricing", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("my private playbook"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{
		textEntry("assistant", "I followed [your skill](user-skills/pricing/SKILL.md)."),
	}); err != nil {
		t.Fatal(err)
	}
	const rel = "user-skills/pricing/SKILL.md"
	got := decode[outputsBody](t, convSub(t, f.srv, "GET", "alice@x.com", f.chat.ID, "outputs", ""))
	for _, o := range got.Outputs {
		if o.Path == rel {
			t.Error("a private skill is listed as an output")
		}
	}
	if w := teamFile(t, f, "bob@x.com", rel); w.Code != 404 || strings.Contains(w.Body.String(), "playbook") {
		t.Errorf("team-file of a private skill: %d", w.Code)
	}
	if w := teamFile(t, f, "alice@x.com", rel); w.Code != 404 {
		t.Errorf("even the owner's team door refuses it: %d", w.Code)
	}

	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: %d", w.Code)
	}
	br := decode[store.Conversation](t, w)
	if _, err := os.Stat(filepath.Join(f.root, br.ID, filepath.FromSlash(rel))); err == nil {
		t.Error("a private skill reached the teammate's branch")
	}
	withheld := false
	for _, w := range br.BranchOrigin.WithheldFiles {
		withheld = withheld || w == rel
	}
	if !withheld {
		t.Errorf("the branch must name the skill as withheld: %v", br.BranchOrigin.WithheldFiles)
	}

	files, _ := walkWorkspaceFiles(f.chat.ID, maxProjectFiles)
	for _, fl := range files {
		if strings.HasPrefix(fl.Path, userSkillsRoot+"/") {
			t.Errorf("Sources walk lists %s", fl.Path)
		}
	}
	// Only the top-level dir is private.
	if isPrivateWorkspacePath("out/user-skills/x.md") || !isPrivateWorkspacePath(userSkillsRoot) {
		t.Error("isPrivateWorkspacePath scope")
	}
}

// Sources lists at most maxSourcesGroups chats per half — the most recently
// active ones with files — and says it left some out.
func TestProjectFilesCapsGroupsPerHalf(t *testing.T) {
	f := newFilesFixture(t)
	// A second shared chat of alice's in the project, with one output.
	c2, err := f.st.CreateConversation(f.ctx, "alice@x.com", "Second", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetConversationProject(f.ctx, "alice@x.com", c2.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, c2.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, c2.ID, "two.csv"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AppendHistory(f.ctx, c2.ID, []agent.HistoryEntry{textEntry("assistant", "[two](two.csv)")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConversationTeamVisible(f.ctx, "alice@x.com", c2.ID, true); err != nil {
		t.Fatal(err)
	}
	type body struct {
		Groups []struct {
			ConversationID string `json:"conversation_id"`
			Mine           bool   `json:"mine"`
		} `json:"groups"`
		Truncated       bool `json:"truncated"`
		GroupsTruncated bool `json:"groups_truncated"`
	}
	// Uncapped: two groups for each of them.
	for _, who := range []string{"alice@x.com", "bob@x.com"} {
		got := decode[body](t, projectSub(t, f.srv, "GET", who, f.project.ID+"/files", ""))
		if len(got.Groups) != 2 || got.GroupsTruncated {
			t.Fatalf("%s uncapped: %+v", who, got)
		}
	}
	old := maxSourcesGroups
	maxSourcesGroups = 1
	t.Cleanup(func() { maxSourcesGroups = old })
	convs, err := f.st.ListProjectConversationsForUser(f.ctx, "alice@x.com", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	sortConversationsRecentFirst(convs)
	newest := convs[0].ID
	for _, who := range []string{"alice@x.com", "bob@x.com"} {
		got := decode[body](t, projectSub(t, f.srv, "GET", who, f.project.ID+"/files", ""))
		if len(got.Groups) != 1 || !got.GroupsTruncated || !got.Truncated || got.Groups[0].ConversationID != newest {
			t.Errorf("%s capped: %+v, want only %s", who, got, newest)
		}
	}
	// The scan bound: one chat examined, the rest reported.
	maxSourcesGroups = old
	oldScan := maxSourcesChatsScanned
	maxSourcesChatsScanned = 1
	t.Cleanup(func() { maxSourcesChatsScanned = oldScan })
	got := decode[body](t, projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/files", ""))
	if len(got.Groups) != 1 || !got.GroupsTruncated {
		t.Errorf("scan-capped: %+v", got)
	}
}
