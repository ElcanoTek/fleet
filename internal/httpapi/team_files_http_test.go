package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/store"
)

// filesFixture is the team fixture plus a workspace on disk: alice's shared
// chat presented three outputs (one of them html), linked an upload, and
// wrote a file it never presented.
type filesFixture struct {
	teamHTTPFixture
	root string
}

func newFilesFixture(t *testing.T) filesFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	f := newTeamHTTPFixture(t)
	ws := filepath.Join(root, f.chat.ID)
	for rel, body := range map[string]string{
		"out/report.csv":        "a,b\n1,2\n",
		"chart.png":             "png",
		"page.html":             "<script>alert(1)</script>",
		"held.json":             `{"secret":true}`,
		"attachments/u1/in.pdf": "upload",
		"scratch.csv":           "never presented",
	} {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("assistant",
		"Here: [report](out/report.csv), ![chart](chart.png), [page](page.html), "+
			"[held](held.json) and your upload [in](attachments/u1/in.pdf).")}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, "held.json", false); err != nil {
		t.Fatal(err)
	}
	return filesFixture{teamHTTPFixture: f, root: root}
}

type outputsBody struct {
	Outputs []struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Shared bool   `json:"shared"`
	} `json:"outputs"`
	Total       int  `json:"total"`
	SharedCount int  `json:"shared_count"`
	TeamVisible bool `json:"team_visible"`
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("json: %v (body %s)", err, w.Body.String())
	}
	return v
}

// GET/POST outputs: the owner's list is outputs only (presented, on disk, not
// uploads), with each file's own share state; nobody else can read it.
func TestConversationOutputsEndpoint(t *testing.T) {
	f := newFilesFixture(t)
	w := convSub(t, f.srv, "GET", "alice@x.com", f.chat.ID, "outputs", "")
	if w.Code != 200 {
		t.Fatalf("outputs: %d %s", w.Code, w.Body.String())
	}
	got := decode[outputsBody](t, w)
	if got.Total != 4 || got.SharedCount != 3 || !got.TeamVisible {
		t.Errorf("counts = total %d shared %d visible %v, want 4/3/true", got.Total, got.SharedCount, got.TeamVisible)
	}
	for _, o := range got.Outputs {
		if strings.HasPrefix(o.Path, "attachments/") || o.Path == "scratch.csv" {
			t.Errorf("non-output listed: %s", o.Path)
		}
		if o.Path == "held.json" && o.Shared {
			t.Error("held.json is excluded")
		}
	}

	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "outputs/share", `{"path":"held.json","shared":true}`)
	if w.Code != 200 || decode[outputsBody](t, w).SharedCount != 4 {
		t.Fatalf("share one: %d %s", w.Code, w.Body.String())
	}
	if w := convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "outputs/share", `{"path":"held.json"}`); w.Code != 400 {
		t.Errorf("missing shared: %d, want 400", w.Code)
	}
	if w := convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "outputs/share", `{"path":"../x","shared":false}`); w.Code != 400 {
		t.Errorf("bad path: %d, want 400", w.Code)
	}
	for _, who := range []string{"bob@x.com", "zoe@x.com"} {
		if w := convSub(t, f.srv, "GET", who, f.chat.ID, "outputs", ""); w.Code != 404 {
			t.Errorf("%s outputs: %d, want 404", who, w.Code)
		}
		if w := convSub(t, f.srv, "POST", who, f.chat.ID, "outputs/share", `{"path":"chart.png","shared":false}`); w.Code != 404 {
			t.Errorf("%s toggle: %d, want 404", who, w.Code)
		}
	}
}

// share-with-team: unshared_paths replaces the checklist, the counts are
// outputs only, an unshare reports what stopped, and the share retires the
// owner's getting-started card in the project.
func TestShareWithTeamCarriesFiles(t *testing.T) {
	f := newFilesFixture(t)
	type resp struct {
		TeamVisible bool `json:"team_visible"`
		SharedFiles int  `json:"shared_files"`
		TotalFiles  int  `json:"total_files"`
	}
	w := convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team",
		`{"visible":true,"unshared_paths":["chart.png","page.html"]}`)
	if w.Code != 200 {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	if got := decode[resp](t, w); !got.TeamVisible || got.SharedFiles != 2 || got.TotalFiles != 4 {
		t.Errorf("share = %+v, want visible 2/4", got)
	}
	// Omitted unshared_paths keeps the owner's choices (the fast paths).
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", `{"visible":true}`)
	if got := decode[resp](t, w); got.SharedFiles != 2 {
		t.Errorf("fast-path share = %+v, want the earlier choices kept", got)
	}
	// The checklist decides only the paths it listed. An exclusion for a file
	// the dialog never showed — here an output missing on disk right now —
	// survives an all-checked re-share, with listed_paths and without it.
	if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, "out/ghost.csv", false); err != nil {
		t.Fatal(err)
	}
	for _, req := range []string{
		`{"visible":true,"unshared_paths":[],"listed_paths":["out/report.csv","chart.png","page.html","held.json"]}`,
		`{"visible":true,"unshared_paths":[]}`,
	} {
		w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", req)
		if got := decode[resp](t, w); w.Code != 200 || got.SharedFiles != 4 {
			t.Errorf("%s: share = %d %+v, want every listed file shared", req, w.Code, got)
		}
		if ex, _ := f.st.ListOutputExclusions(f.ctx, f.chat.ID); !ex["out/ghost.csv"] || len(ex) != 1 {
			t.Errorf("%s: exclusions = %v, want only the unlisted out/ghost.csv kept", req, ex)
		}
	}
	// Back to the earlier choices for the rest of the test.
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team",
		`{"visible":true,"unshared_paths":["chart.png","page.html"],"listed_paths":["chart.png","page.html"]}`)
	if got := decode[resp](t, w); got.SharedFiles != 2 {
		t.Errorf("re-share = %+v, want 2", got)
	}
	st, _ := f.st.GetProjectUserState(f.ctx, f.project.ID, "alice@x.com")
	if !st.HasSharedChat {
		t.Error("a successful share must record has_shared_chat")
	}
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", `{"visible":false}`)
	if got := decode[resp](t, w); got.TeamVisible || got.SharedFiles != 2 || got.TotalFiles != 4 {
		t.Errorf("unshare = %+v, want 2 files stopped", got)
	}
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", `{"visible":false}`)
	if got := decode[resp](t, w); got.SharedFiles != 0 {
		t.Errorf("unsharing an unshared chat stops nothing: %+v", got)
	}
	if w := convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team",
		`{"visible":true,"unshared_paths":["../etc/passwd"]}`); w.Code != 400 {
		t.Errorf("bad unshared_paths: %d, want 400", w.Code)
	}
	if w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "share-with-team", `{"visible":false}`); w.Code != 404 {
		t.Errorf("teammate share-with-team: %d, want 404", w.Code)
	}
	// An archived chat cannot be shared: 409 with the sentence that says why.
	if err := f.st.SetArchived(f.ctx, "alice@x.com", f.chat.ID, true); err != nil {
		t.Fatal(err)
	}
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", `{"visible":true}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "unarchive") {
		t.Errorf("share while archived: %d %q, want 409 naming unarchive", w.Code, w.Body.String())
	}
}

// team-view lists every output with its state (a withheld one as a name
// only), names the project, and reports the viewer's branch.
func TestTeamViewListsFiles(t *testing.T) {
	f := newFilesFixture(t)
	w := convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", "")
	if w.Code != 200 {
		t.Fatalf("team-view: %d %s", w.Code, w.Body.String())
	}
	got := decode[struct {
		ProjectID    string          `json:"project_id"`
		ProjectName  string          `json:"project_name"`
		Files        []outputFile    `json:"files"`
		ViewerBranch json.RawMessage `json:"viewer_branch"`
	}](t, w)
	if got.ProjectID != f.project.ID || got.ProjectName != "Quant" {
		t.Errorf("project = %q/%q", got.ProjectID, got.ProjectName)
	}
	if len(got.Files) != 4 {
		t.Fatalf("files = %+v, want the 4 outputs", got.Files)
	}
	for _, o := range got.Files {
		if o.Path == "held.json" && (o.Shared || o.Size != 0) {
			t.Errorf("withheld file leaks more than its name: %+v", o)
		}
		if strings.HasPrefix(o.Path, "attachments/") {
			t.Error("uploads are never listed")
		}
	}
	if string(got.ViewerBranch) != "null" {
		t.Errorf("viewer_branch = %s, want null before branching", got.ViewerBranch)
	}
}

func teamFile(t *testing.T, f filesFixture, user, rel string) *httptest.ResponseRecorder {
	t.Helper()
	return convSub(t, f.srv, "GET", user, f.chat.ID, "team-files/"+rel, "")
}

// The first cross-user file read: every gate, every refusal a 404.
func TestTeamFilesGate(t *testing.T) {
	f := newFilesFixture(t)

	w := teamFile(t, f, "bob@x.com", "out/report.csv")
	if w.Code != 200 || w.Body.String() != "a,b\n1,2\n" {
		t.Fatalf("shared output: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("headers = %v", w.Header())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("a csv need not be forced to download: %q", cd)
	}
	w = teamFile(t, f, "bob@x.com", "page.html")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("html must download, not render: %d %v", w.Code, w.Header())
	}

	// The owner reaches it through the same door.
	if w := teamFile(t, f, "alice@x.com", "chart.png"); w.Code != 200 {
		t.Errorf("owner team-file: %d", w.Code)
	}

	for _, rel := range []string{
		"held.json",                      // excluded by the owner
		"attachments/u1/in.pdf",          // an upload, even though the reply linked it
		"scratch.csv",                    // on disk but never presented
		"missing.csv",                    // not on disk
		"../" + f.chat.ID + "/chart.png", // traversal
		"out/../chart.png",
		"out",
	} {
		if w := teamFile(t, f, "bob@x.com", rel); w.Code != 404 {
			t.Errorf("%s: %d, want 404", rel, w.Code)
		}
	}
	if w := teamFile(t, f, "zoe@x.com", "out/report.csv"); w.Code != 404 {
		t.Errorf("other team: %d, want 404", w.Code)
	}

	// A shared name swapped for a symlink to an upload (the owner's sandbox
	// can write here) must not hand the teammate the upload.
	ws := filepath.Join(f.root, f.chat.ID)
	if err := os.Remove(filepath.Join(ws, "chart.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "attachments", "u1", "in.pdf"), filepath.Join(ws, "chart.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if w := teamFile(t, f, "bob@x.com", "chart.png"); w.Code != 404 || strings.Contains(w.Body.String(), "upload") {
		t.Errorf("symlinked shared name: %d %q, want 404", w.Code, w.Body.String())
	}
	// Same for a directory swapped for a link to the uploads dir.
	if err := os.RemoveAll(filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "attachments", "u1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "attachments", "report.csv"), []byte("upload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "attachments"), filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if w := teamFile(t, f, "bob@x.com", "out/report.csv"); w.Code != 404 {
		t.Errorf("symlinked directory: %d, want 404", w.Code)
	}

	// Stop sharing / archive close the door for every file.
	if w := teamFile(t, f, "bob@x.com", "page.html"); w.Code != 200 {
		t.Fatalf("still shared: %d", w.Code)
	}
	if err := f.st.SetArchived(f.ctx, "alice@x.com", f.chat.ID, true); err != nil {
		t.Fatal(err)
	}
	if w := teamFile(t, f, "bob@x.com", "page.html"); w.Code != 404 {
		t.Errorf("archived: %d, want 404", w.Code)
	}
}

// The public link stays transcript-only: no files, no ids.
func TestPublicShareSnapshotCarriesNoFiles(t *testing.T) {
	f := newFilesFixture(t)
	if err := f.st.SetShareToken(f.ctx, "alice@x.com", f.chat.ID, "tok-public-files", nil); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.srv.shareRL = ratelimit.New(100, 0)
	f.srv.handleSharedConversation(w, httptest.NewRequest("GET", "/shared/tok-public-files", nil))
	if w.Code != 200 {
		t.Fatalf("shared: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, k := range []string{`"files"`, `"outputs"`, `"project_id"`, `"team-files"`} {
		if strings.Contains(body, k) {
			t.Errorf("public snapshot carries %s", k)
		}
	}
}

func TestTeamLinkEndpoint(t *testing.T) {
	f := newFilesFixture(t)
	type link struct {
		Status      string `json:"status"`
		TeamID      string `json:"team_id"`
		ViewerEmail string `json:"viewer_email"`
		Project     *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	for who, want := range map[string]string{"alice@x.com": "owner", "bob@x.com": "open", "zoe@x.com": "not_on_team"} {
		w := convSub(t, f.srv, "GET", who, f.chat.ID, "team-link", "")
		got := decode[link](t, w)
		if w.Code != 200 || got.Status != want || got.ViewerEmail != who {
			t.Errorf("%s: %d %+v, want %s", who, w.Code, got, want)
		}
		if who == "zoe@x.com" && (got.TeamID != "quant" || got.Project != nil || strings.Contains(w.Body.String(), "Spread")) {
			t.Errorf("not_on_team reveals too much: %s", w.Body.String())
		}
	}
	got := decode[link](t, convSub(t, f.srv, "GET", "bob@x.com", "nope", "team-link", ""))
	if got.Status != "not_shared" || got.Project != nil {
		t.Errorf("unknown id: %+v", got)
	}
}

// A teammate's branch carries the SHARED outputs as its own copies at the
// same paths; withheld ones are named, not copied; the origin is reported on
// the branch response and the conversation read; the agent is told once.
func TestTeammateBranchCopiesSharedFiles(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	point := msgs[len(msgs)-1].ID
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": point})

	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	br := decode[store.Conversation](t, w)
	if br.BranchOrigin == nil {
		t.Fatal("branch response must carry branch_origin")
	}
	o := br.BranchOrigin
	if o.SourceOwnerEmail != "alice@x.com" || o.SourceTitle != "Spread study" || !o.SourceStillShared {
		t.Errorf("origin = %+v", o)
	}
	copied := map[string]bool{}
	for _, c := range o.CopiedFiles {
		copied[c.Path] = true
	}
	if len(copied) != 3 || !copied["out/report.csv"] || !copied["chart.png"] || !copied["page.html"] {
		t.Errorf("copied = %+v", o.CopiedFiles)
	}
	// Withheld: the unchecked output AND the upload the transcript links —
	// uploads are never copied, so the branch must render it locked, not as a
	// live link that 404s.
	if strings.Join(o.WithheldFiles, ",") != "held.json,attachments/u1/in.pdf" {
		t.Errorf("withheld = %v", o.WithheldFiles)
	}
	bws := filepath.Join(f.root, br.ID)
	if b, err := os.ReadFile(filepath.Join(bws, "out", "report.csv")); err != nil || string(b) != "a,b\n1,2\n" {
		t.Errorf("copy at same path: %q %v", b, err)
	}
	for _, rel := range []string{"held.json", "attachments/u1/in.pdf", "scratch.csv"} {
		if _, err := os.Stat(filepath.Join(bws, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s must not reach the branch", rel)
		}
	}
	// The copy is the brancher's: the owner deleting the source file later
	// does not touch it.
	if err := os.Remove(filepath.Join(f.root, f.chat.ID, "out", "report.csv")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bws, "out", "report.csv")); err != nil {
		t.Errorf("branch copy must survive the source's deletion: %v", err)
	}

	// The conversation read carries the origin.
	g := convSub(t, f.srv, "GET", "bob@x.com", br.ID, "", "")
	if g.Code != 200 {
		t.Fatalf("get: %d", g.Code)
	}
	gb := decode[struct {
		BranchOrigin *store.BranchOrigin `json:"branch_origin"`
	}](t, g)
	if gb.BranchOrigin == nil || len(gb.BranchOrigin.CopiedFiles) != 3 {
		t.Errorf("GET branch_origin = %+v", gb.BranchOrigin)
	}

	// The viewer now has a branch, in team-view and the Team section.
	tv := decode[struct {
		ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
	}](t, convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", ""))
	if tv.ViewerBranch == nil || tv.ViewerBranch.ConversationID != br.ID {
		t.Errorf("team-view viewer_branch = %+v", tv.ViewerBranch)
	}
	tc := decode[struct {
		Conversations []struct {
			ID           string              `json:"id"`
			ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
		} `json:"conversations"`
	}](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/team-conversations", ""))
	if len(tc.Conversations) != 1 || tc.Conversations[0].ViewerBranch == nil || tc.Conversations[0].ViewerBranch.ConversationID != br.ID {
		t.Errorf("team-conversations = %+v", tc.Conversations)
	}

	// First turn: the note names the copies; then never again.
	note, claimed := f.srv.appendBranchFilesBlock(f.ctx, "", br.ID)
	if !claimed || !strings.Contains(note, "`out/report.csv`") || strings.Contains(note, "held.json") || !strings.Contains(note, "NOT in this workspace") {
		t.Errorf("note = %q (claimed=%v)", note, claimed)
	}
	if again, claimed := f.srv.appendBranchFilesBlock(f.ctx, "x", br.ID); again != "x" || claimed {
		t.Errorf("second note = %q (claimed=%v)", again, claimed)
	}

	// The owner's own branch copies nothing and has no origin.
	w = convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("owner branch: %d", w.Code)
	}
	own := decode[store.Conversation](t, w)
	if own.BranchOrigin != nil {
		t.Error("the owner's own branch has no origin")
	}
	if _, err := os.Stat(filepath.Join(f.root, own.ID, "chart.png")); err == nil {
		t.Error("the owner's own branch must not copy files")
	}
}

// The branch's "seen" high-water mark is the branch point: a source message
// that lands WHILE the files are being copied is not in the branch, so the
// viewer is told the source has added messages since.
func TestTeammateBranchMidCopyMessageReportsChanged(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	appended := false
	branchCopyAfterStat = func(string) {
		if appended {
			return
		}
		appended = true
		if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("assistant", "late")}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { branchCopyAfterStat = nil })
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	branchCopyAfterStat = nil
	if w.Code != 201 || !appended {
		t.Fatalf("branch: %d %s (appended=%v)", w.Code, w.Body.String(), appended)
	}
	tv := decode[struct {
		ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
	}](t, convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", ""))
	if tv.ViewerBranch == nil || !tv.ViewerBranch.ChangedSince {
		t.Errorf("viewer_branch = %+v, want changed_since after a mid-copy message", tv.ViewerBranch)
	}
}

// The branch is committed before its files are copied; a request canceled
// during that copy (the client gave up, a proxy timed out) must still leave
// the branch with its recorded origin — never copied files with no banner and
// no locked names.
func TestTeammateBranchRecordsOriginAfterRequestCancel(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	reqCtx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKeyUser, "bob@x.com"))
	defer cancel()
	canceled := false
	branchCopyAfterStat = func(string) {
		canceled = true
		cancel()
	}
	t.Cleanup(func() { branchCopyAfterStat = nil })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/conversations/"+f.chat.ID+"/branch", strings.NewReader(string(body))).WithContext(reqCtx)
	f.srv.conversationByID(w, req)
	branchCopyAfterStat = nil
	if w.Code != 201 || !canceled {
		t.Fatalf("branch: %d %s (canceled=%v)", w.Code, w.Body.String(), canceled)
	}
	br := decode[store.Conversation](t, w)
	origins, err := f.st.BranchOriginsFor(f.ctx, []string{br.ID})
	if err != nil {
		t.Fatal(err)
	}
	o := origins[br.ID]
	if o == nil {
		t.Fatal("a canceled request left the committed branch without its origin")
	}
	if o.SourceConversationID != f.chat.ID || len(o.CopiedFiles) == 0 {
		t.Errorf("origin = %+v, want the source and its copied files", o)
	}
}

// The teammate branches at the message they last saw (their last poll); a
// source message sent after that but BEFORE the branch POST is not in the
// branch either, so it must report changed_since rather than count as seen.
func TestTeammateBranchAtOlderPointReportsChanged(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	point := msgs[len(msgs)-1].ID
	// The owner replies after the teammate's last poll, before they branch.
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("assistant", "newer")}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": point})
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	tv := decode[struct {
		ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
	}](t, convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", ""))
	if tv.ViewerBranch == nil || !tv.ViewerBranch.ChangedSince {
		t.Errorf("viewer_branch = %+v, want changed_since for a message past the branch point", tv.ViewerBranch)
	}
}

// Branching at the newest message with nothing after it reports no change.
func TestTeammateBranchAtLatestReportsUnchanged(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	if w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body)); w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	tv := decode[struct {
		ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
	}](t, convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", ""))
	if tv.ViewerBranch == nil || tv.ViewerBranch.ChangedSince {
		t.Errorf("viewer_branch = %+v, want unchanged", tv.ViewerBranch)
	}
}

// Every workspace reference in the branched transcript that the branch did
// not receive is withheld — a presented file missing on disk and an upload
// included — and nothing is withheld twice.
func TestTeammateBranchWithholdsEveryUncopiedReference(t *testing.T) {
	f := newFilesFixture(t)
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("assistant",
		"Also [gone](out/gone.csv), [again](attachments/u1/in.pdf) and [report](out/report.csv).")}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	o := decode[store.Conversation](t, w).BranchOrigin
	if o == nil {
		t.Fatal("no branch_origin")
	}
	got := strings.Join(o.WithheldFiles, ",")
	if got != "held.json,attachments/u1/in.pdf,out/gone.csv" {
		t.Errorf("withheld = %s", got)
	}
	for _, c := range o.CopiedFiles {
		if strings.HasPrefix(c.Path, "attachments/") {
			t.Errorf("upload copied: %s", c.Path)
		}
	}
}

// The branch copies messages only through the branch point, so its files are
// only those: an output presented in a reply AFTER the branch point is neither
// copied into the branch nor named as withheld in it.
func TestTeammateBranchIgnoresRepliesPastTheBranchPoint(t *testing.T) {
	f := newFilesFixture(t)
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	point := msgs[len(msgs)-1].ID
	if err := os.WriteFile(filepath.Join(f.root, f.chat.ID, "later.csv"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{
		textEntry("user", "one more"),
		textEntry("assistant", "Here is [later](later.csv) and [gone later](gone-later.csv)."),
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"branch_point_message_id": point})
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	br := decode[store.Conversation](t, w)
	o := br.BranchOrigin
	if o == nil {
		t.Fatal("no branch_origin")
	}
	for _, c := range o.CopiedFiles {
		if c.Path == "later.csv" {
			t.Error("an output past the branch point must not be copied")
		}
	}
	for _, p := range o.WithheldFiles {
		if p == "later.csv" || p == "gone-later.csv" {
			t.Errorf("a reference past the branch point must not be withheld: %v", o.WithheldFiles)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, br.ID, "later.csv")); err == nil {
		t.Error("later.csv reached the branch workspace")
	}
	if o.WithheldTruncated {
		t.Error("a short transcript is not truncated")
	}
}

// The withheld list is bounded like discovery: past maxOutputReferences
// distinct references only the most recent are recorded, and the origin says
// the list is truncated so the web treats unknown references as withheld.
func TestWithholdUncopiedReferencesIsBounded(t *testing.T) {
	history := make([]agent.HistoryEntry, 0, maxOutputReferences+3)
	for i := range maxOutputReferences + 3 {
		history = append(history, textEntry("user", "q"), textEntry("assistant", fmt.Sprintf("[f](f%04d.csv)", i)))
	}
	withheld, truncated := withholdUncopiedReferences(history, nil, nil)
	if !truncated || len(withheld) != maxOutputReferences {
		t.Fatalf("withheld = %d (truncated=%v), want %d truncated", len(withheld), truncated, maxOutputReferences)
	}
	if withheld[0] != fmt.Sprintf("f%04d.csv", maxOutputReferences+2) {
		t.Errorf("the most recent reference must be kept first, got %s", withheld[0])
	}
	if _, truncated := withholdUncopiedReferences(history[6:], nil, nil); truncated {
		t.Error("exactly maxOutputReferences references is not truncated")
	}
}

// Sources, grouped: my chats with every non-upload file (flagged), the
// teammate's shared chat with its SHARED outputs only.
func TestProjectFilesGrouped(t *testing.T) {
	f := newFilesFixture(t)
	type file struct {
		Path     string `json:"path"`
		Shared   bool   `json:"shared"`
		Output   bool   `json:"output"`
		YourCopy bool   `json:"your_copy"`
	}
	type group struct {
		ConversationID string `json:"conversation_id"`
		OwnerEmail     string `json:"owner_email"`
		Mine           bool   `json:"mine"`
		TeamVisible    bool   `json:"team_visible"`
		IsBranch       bool   `json:"is_branch"`
		FileCount      int    `json:"file_count"`
		SharedCount    int    `json:"shared_count"`
		Files          []file `json:"files"`
	}
	type body struct {
		Groups []group `json:"groups"`
		Files  []struct {
			Path string `json:"path"`
		} `json:"files"`
	}

	// The owner: one group, hers, with the non-output scratch file download-only.
	got := decode[body](t, projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/files", ""))
	if len(got.Groups) != 1 || !got.Groups[0].Mine || got.Groups[0].FileCount != 4 || got.Groups[0].SharedCount != 3 {
		t.Fatalf("owner groups = %+v", got.Groups)
	}
	for _, fl := range got.Groups[0].Files {
		if strings.HasPrefix(fl.Path, "attachments/") {
			t.Error("uploads are never listed")
		}
		if fl.Path == "scratch.csv" && (fl.Output || fl.Shared) {
			t.Errorf("scratch.csv is not an output: %+v", fl)
		}
	}
	if len(got.Groups[0].Files) != 5 || len(got.Files) != 5 {
		t.Errorf("files = %d grouped / %d legacy, want 5", len(got.Groups[0].Files), len(got.Files))
	}

	// The teammate: alice's chat appears with its three shared outputs only.
	got = decode[body](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/files", ""))
	if len(got.Groups) != 1 || got.Groups[0].Mine || got.Groups[0].OwnerEmail != "alice@x.com" || got.Groups[0].FileCount != 3 {
		t.Fatalf("teammate groups = %+v", got.Groups)
	}
	for _, fl := range got.Groups[0].Files {
		if fl.Path == "held.json" || fl.Path == "scratch.csv" || !fl.Shared {
			t.Errorf("teammate sees %+v", fl)
		}
	}

	// After branching, bob also has his own group, labelled as a branch.
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	b, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	if w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(b)); w.Code != 201 {
		t.Fatalf("branch: %d", w.Code)
	}
	got = decode[body](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/files", ""))
	var mine *group
	for i := range got.Groups {
		if got.Groups[i].Mine {
			mine = &got.Groups[i]
		}
	}
	if mine == nil || !mine.IsBranch || len(mine.Files) != 3 {
		t.Fatalf("branch group = %+v", mine)
	}
	for _, fl := range mine.Files {
		if !fl.YourCopy {
			t.Errorf("branch file not labelled as a copy: %+v", fl)
		}
	}

	// Unsharing the source removes the teammate group.
	if _, err := f.st.SetConversationTeamVisible(f.ctx, "alice@x.com", f.chat.ID, false); err != nil {
		t.Fatal(err)
	}
	got = decode[body](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/files", ""))
	for _, g := range got.Groups {
		if !g.Mine {
			t.Errorf("an unshared chat's files must disappear for teammates: %+v", g)
		}
	}
}

// Sources' workspace walk keeps only the newest maxProjectFiles files, but
// every current output is counted — so every output must also have its row
// (and toggle), however many newer scratch files bury it.
func TestProjectFilesListsOlderOutputsPastTheWalkBound(t *testing.T) {
	f := newFilesFixture(t)
	ws := filepath.Join(f.root, f.chat.ID)
	old := time.Now().Add(-48 * time.Hour)
	for _, rel := range []string{"out/report.csv", "chart.png", "page.html", "held.json"} {
		if err := os.Chtimes(filepath.Join(ws, filepath.FromSlash(rel)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := range maxProjectFiles + 20 {
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("tmp%04d.log", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type body struct {
		Groups []struct {
			FileCount   int `json:"file_count"`
			SharedCount int `json:"shared_count"`
			Files       []struct {
				Path   string `json:"path"`
				Shared bool   `json:"shared"`
				Output bool   `json:"output"`
			} `json:"files"`
		} `json:"groups"`
		Truncated bool `json:"truncated"`
	}
	got := decode[body](t, projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/files", ""))
	if len(got.Groups) != 1 {
		t.Fatalf("groups = %+v", got.Groups)
	}
	g := got.Groups[0]
	if g.FileCount != 4 || g.SharedCount != 3 {
		t.Fatalf("counts = %d/%d, want 4/3", g.FileCount, g.SharedCount)
	}
	outputs, shared := 0, 0
	for _, fl := range g.Files {
		if fl.Output {
			outputs++
			if fl.Shared {
				shared++
			}
		}
	}
	if outputs != g.FileCount || shared != g.SharedCount {
		t.Errorf("rows disagree with counts: %d outputs (%d shared) listed, counts %d/%d",
			outputs, shared, g.FileCount, g.SharedCount)
	}
	if len(g.Files) != maxProjectFiles || !got.Truncated {
		t.Errorf("files = %d (truncated=%v), want the cap %d, truncated", len(g.Files), got.Truncated, maxProjectFiles)
	}
}

// The walk's visit budget can be spent before it reaches a single file (a
// tree of empty directories that sorts first). That must not hide the chat's
// current outputs: they are resolved independently of the walk, so the group
// still lists every output with its toggle, and the listing says truncated.
func TestProjectFilesListsOutputsWhenWalkFindsNoFiles(t *testing.T) {
	f := newFilesFixture(t)
	ws := filepath.Join(f.root, f.chat.ID)
	for i := range 40 {
		if err := os.MkdirAll(filepath.Join(ws, "aaa", fmt.Sprintf("d%03d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := maxWorkspaceWalkEntries
	maxWorkspaceWalkEntries = 20
	t.Cleanup(func() { maxWorkspaceWalkEntries = old })
	if all, _ := walkWorkspaceFiles(f.chat.ID, maxProjectFiles); len(all) != 0 {
		t.Fatalf("fixture: the walk should find no files, got %d", len(all))
	}
	type body struct {
		Groups []struct {
			ConversationID string `json:"conversation_id"`
			FileCount      int    `json:"file_count"`
			SharedCount    int    `json:"shared_count"`
			Files          []struct {
				Path   string `json:"path"`
				Shared bool   `json:"shared"`
				Output bool   `json:"output"`
			} `json:"files"`
		} `json:"groups"`
		Truncated bool `json:"truncated"`
	}
	got := decode[body](t, projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/files", ""))
	if len(got.Groups) != 1 || got.Groups[0].ConversationID != f.chat.ID {
		t.Fatalf("groups = %+v, want alice's chat with its outputs", got.Groups)
	}
	g := got.Groups[0]
	if g.FileCount != 4 || g.SharedCount != 3 || len(g.Files) != 4 || !got.Truncated {
		t.Errorf("group = %+v truncated=%v, want 4 output rows (3 shared), truncated", g, got.Truncated)
	}
	for _, fl := range g.Files {
		if !fl.Output {
			t.Errorf("non-output row without a walk: %+v", fl)
		}
	}
}

func TestProjectMyStateEndpoint(t *testing.T) {
	f := newTeamHTTPFixture(t)
	// sources_open keys are chat ids in the project; keys naming no live chat
	// here are pruned on write.
	w := projectSub(t, f.srv, "PUT", "bob@x.com", f.project.ID+"/my-state",
		`{"kept_personal":true,"sources_open":{"`+f.chat.ID+`":true,"no-such-chat":true}}`)
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	got := decode[store.ProjectUserState](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/my-state", ""))
	if _, stale := got.SourcesOpen["no-such-chat"]; !got.KeptPersonal || got.HasSharedChat || !got.SourcesOpen[f.chat.ID] || stale {
		t.Errorf("state = %+v", got)
	}
	// has_shared_chat is not client-writable.
	projectSub(t, f.srv, "PUT", "bob@x.com", f.project.ID+"/my-state", `{"has_shared_chat":true}`)
	if got := decode[store.ProjectUserState](t, projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/my-state", "")); got.HasSharedChat {
		t.Error("has_shared_chat must only flip on a real share")
	}
	if w := projectSub(t, f.srv, "GET", "zoe@x.com", f.project.ID+"/my-state", ""); w.Code != 404 {
		t.Errorf("non-member: %d, want 404", w.Code)
	}
}

// TestCopyOneOutputRejectsShortCopy: a source truncated between the stat and
// the copy must not land in the branch as a silently shorter file. The
// LimitReader-bounded io.Copy reports the early EOF as success, so the copy
// checks the byte count against the stat'd size, removes the partial file and
// errors (the caller then withholds it).
func TestCopyOneOutputRejectsShortCopy(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "report.csv"), []byte("a,b\n1,2\n3,4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstDir := t.TempDir()
	dst, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	branchCopyAfterStat = func(p string) {
		if err := os.Truncate(p, 3); err != nil {
			t.Errorf("truncate: %v", err)
		}
	}
	t.Cleanup(func() { branchCopyAfterStat = nil })

	n, err := copyOneOutput(context.Background(), src, dst, "report.csv", 1<<20)
	if !errors.Is(err, errBranchCopyShort) {
		t.Fatalf("copyOneOutput = (%d, %v), want errBranchCopyShort", n, err)
	}
	if _, statErr := os.Stat(filepath.Join(dstDir, "report.csv")); !os.IsNotExist(statErr) {
		t.Fatalf("partial copy left behind: %v", statErr)
	}

	// Untouched source copies in full.
	branchCopyAfterStat = nil
	if err := os.WriteFile(filepath.Join(src, "ok.csv"), []byte("full"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := copyOneOutput(context.Background(), src, dst, "ok.csv", 1<<20); err != nil || n != 4 {
		t.Fatalf("full copy = (%d, %v), want (4, nil)", n, err)
	}
}

// A same-size in-place rewrite during the copy passes the length check but
// can leave a file mixing two versions; the post-copy re-stat of the open
// descriptor (size + mtime) catches it and the copy is withheld.
func TestCopyOneOutputRejectsSameSizeRewrite(t *testing.T) {
	src := t.TempDir()
	p := filepath.Join(src, "report.csv")
	if err := os.WriteFile(p, []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	dstDir := t.TempDir()
	dst, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	branchCopyAfterStat = func(p string) {
		// Same length, different bytes, later mtime — same inode.
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		if _, err := f.WriteAt([]byte("x,y\n9,9\n"), 0); err != nil {
			t.Errorf("rewrite: %v", err)
		}
		_ = f.Close()
		later := old.Add(time.Minute)
		if err := os.Chtimes(p, later, later); err != nil {
			t.Errorf("chtimes: %v", err)
		}
	}
	t.Cleanup(func() { branchCopyAfterStat = nil })

	n, err := copyOneOutput(context.Background(), src, dst, "report.csv", 1<<20)
	if !errors.Is(err, errBranchCopyShort) {
		t.Fatalf("copyOneOutput = (%d, %v), want errBranchCopyShort", n, err)
	}
	if _, statErr := os.Stat(filepath.Join(dstDir, "report.csv")); !os.IsNotExist(statErr) {
		t.Fatalf("mixed-version copy left behind: %v", statErr)
	}
}

// Past the discovery bound the listings say truncated and the gate refuses an
// older reference exactly like a non-output: the bound narrows downloads,
// never widens them, and the newest references stay reachable.
func TestOutputDiscoveryBoundAcrossRoutes(t *testing.T) {
	f := newFilesFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, f.chat.ID, "new.csv"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range maxOutputReferences {
		fmt.Fprintf(&b, "[f%d](gone/f%d.csv) ", i, i)
	}
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{
		textEntry("assistant", b.String()),
		textEntry("assistant", "Latest: [new](new.csv)"),
	}); err != nil {
		t.Fatal(err)
	}

	w := convSub(t, f.srv, "GET", "alice@x.com", f.chat.ID, "outputs", "")
	got := decode[struct {
		Outputs   []outputFile `json:"outputs"`
		Truncated bool         `json:"truncated"`
	}](t, w)
	if !got.Truncated || len(got.Outputs) != 1 || got.Outputs[0].Path != "new.csv" {
		t.Errorf("outputs = %+v truncated=%v, want only new.csv and truncated", got.Outputs, got.Truncated)
	}
	tv := decode[struct {
		Files          []outputFile `json:"files"`
		FilesTruncated bool         `json:"files_truncated"`
	}](t, convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", ""))
	if !tv.FilesTruncated || len(tv.Files) != 1 {
		t.Errorf("team-view files = %+v truncated=%v", tv.Files, tv.FilesTruncated)
	}
	if w := teamFile(t, f, "bob@x.com", "new.csv"); w.Code != 200 {
		t.Errorf("newest reference: %d, want 200", w.Code)
	}
	if w := teamFile(t, f, "bob@x.com", "out/report.csv"); w.Code != 404 {
		t.Errorf("a reference beyond the bound: %d, want 404", w.Code)
	}
}

// noteEngine records each turn's injected context; while failPreCommit is
// set it fails BEFORE committing the user entry, like a provider preflight or
// a lost DB write would.
type noteEngine struct {
	fakeEngine
	failPreCommit atomic.Bool
	mu2           sync.Mutex
	injected      []string
}

func (e *noteEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	e.mu2.Lock()
	e.injected = append(e.injected, in.InjectedContext)
	e.mu2.Unlock()
	if e.failPreCommit.Load() {
		return nil, errors.New("preflight failed")
	}
	return e.fakeEngine.RunTurn(ctx, in, sink)
}

// The branch-files note is one-shot, but only once it is DURABLE: a first
// turn that fails before its user message commits hands the latch back, and
// the next turn carries the note. A committed turn keeps it claimed.
func TestBranchFilesNoteSurvivesAFailedFirstTurn(t *testing.T) {
	f := newFilesFixture(t)
	br, err := f.st.CreateConversation(f.ctx, "bob@x.com", "branch", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordBranchOrigin(f.ctx, br.ID, store.BranchOrigin{
		SourceConversationID: f.chat.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
		BranchedAt: 1, CopiedFiles: []store.BranchFile{{Path: "out/report.csv", Name: "report.csv", Size: 8}},
	}); err != nil {
		t.Fatal(err)
	}
	eng := &noteEngine{}
	f.srv.agent = eng
	turn := func(msg string) string {
		t.Helper()
		postChatJSON(t, f.srv, "bob@x.com", map[string]any{"message": msg, "conversation_id": br.ID})
		waitFor(t, "turn goroutine finished", func() bool { return f.srv.activeTurnCount.Load() == 0 })
		eng.mu2.Lock()
		defer eng.mu2.Unlock()
		return eng.injected[len(eng.injected)-1]
	}

	eng.failPreCommit.Store(true)
	if got := turn("first"); !strings.Contains(got, "`out/report.csv`") {
		t.Fatalf("first turn must carry the note: %q", got)
	}
	eng.failPreCommit.Store(false)
	if got := turn("again"); !strings.Contains(got, "`out/report.csv`") {
		t.Fatalf("a failed pre-commit turn must release the note to the next turn: %q", got)
	}
	if got := turn("third"); strings.Contains(got, "out/report.csv") {
		t.Fatalf("a committed turn keeps the note claimed: %q", got)
	}
}

// teamViewIf issues GET team-view as user with If-None-Match: etag.
func teamViewIf(t *testing.T, srv *Server, user, convID, etag string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/conversations/"+convID+"/team-view", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	srv.conversationByID(w, req)
	return w
}

// The ETag on a 200 is the fingerprint of what that body was BUILT from. A
// project renamed A→B after the version read (so the body says B) and back
// to A before the next poll must not let that poll 304 — the client holds B,
// the state is A. And a body built in a settled state 304s on the next poll,
// even with exclusions whose order or lengths a naive aggregate would get
// wrong (unicode, case, a newline), and with the viewer's own branch present.
func TestTeamViewETagIsTheServedBodysFingerprint(t *testing.T) {
	f := newFilesFixture(t)
	rename := func(name string) {
		t.Helper()
		if _, err := f.st.UpdateProject(f.ctx, "alice@x.com", f.project.ID, store.ProjectPatch{Name: &name}); err != nil {
			t.Fatal(err)
		}
	}
	teamViewAfterVersion = func() {
		teamViewAfterVersion = nil
		rename("Renamed")
	}
	t.Cleanup(func() { teamViewAfterVersion = nil })
	w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, "")
	tag := w.Header().Get("ETag")
	if w.Code != 200 || tag == "" || !strings.Contains(w.Body.String(), `"project_name":"Renamed"`) {
		t.Fatalf("mid-build rename: %d etag=%q body=%s", w.Code, tag, w.Body.String())
	}
	rename("Quant") // back to the name the version read saw
	if w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, tag); w.Code != 200 {
		t.Fatalf("poll after the rename was undone: %d, want 200 (the client holds the Renamed body)", w.Code)
	}

	// Settled, with awkward exclusions and a branch of bob's: 200 then 304.
	for _, p := range []string{"Zeta.csv", "alpha.csv", "é.csv", "a\nb.csv"} {
		if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, p, false); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	br, err := f.st.BranchConversation(f.ctx, "bob@x.com", f.chat.ID, msgs[len(msgs)-1].ID, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordBranchOrigin(f.ctx, br.ID, store.BranchOrigin{
		SourceConversationID: f.chat.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "t",
		BranchedAt: br.CreatedAt, SourceMaxMessageID: msgs[len(msgs)-1].ID,
	}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"bob@x.com", "alice@x.com"} {
		w := teamViewIf(t, f.srv, who, f.chat.ID, "")
		tag := w.Header().Get("ETag")
		if w.Code != 200 || tag == "" {
			t.Fatalf("%s settled read: %d etag=%q", who, w.Code, tag)
		}
		if w := teamViewIf(t, f.srv, who, f.chat.ID, tag); w.Code != 304 {
			t.Errorf("%s: poll with the served tag = %d, want 304 (served fingerprint must equal the cheap one)", who, w.Code)
		}
	}
}

// Each listed file carries a revision precise enough to tell two writes in
// the same second apart (the viewer versions inline URLs by it); a file the
// owner held back carries none for a teammate, like its size and date.
func TestTeamViewFileRevSeesSameSecondRewrite(t *testing.T) {
	f := newFilesFixture(t)
	revOf := func(path string) string {
		t.Helper()
		w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, "")
		var body struct {
			Files []struct {
				Path string `json:"path"`
				Rev  string `json:"rev"`
			} `json:"files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, fl := range body.Files {
			if fl.Path == path {
				return fl.Rev
			}
		}
		t.Fatalf("%s not listed", path)
		return ""
	}
	p := filepath.Join(f.root, f.chat.ID, "chart.png")
	sec := time.Unix(1767225600, 100)
	if err := os.Chtimes(p, sec, sec); err != nil {
		t.Fatal(err)
	}
	before := revOf("chart.png")
	// Same second, same size, rewritten.
	later := time.Unix(1767225600, 900)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if after := revOf("chart.png"); after == "" || after == before {
		t.Errorf("rev %q -> %q: a same-second rewrite must change it", before, after)
	}
	if held := revOf("held.json"); held != "" {
		t.Errorf("held-back file rev = %q for a teammate, want none", held)
	}
}

// The live view's poll is conditional: an unchanged chat answers 304 with no
// body; a new message, an exclusion change and the viewer's own new branch
// each move the ETag; and a caller who may not read the chat gets 404 —
// never 304 — whatever If-None-Match they send.
func TestTeamViewConditionalPoll(t *testing.T) {
	f := newFilesFixture(t)
	w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, "")
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" {
		t.Fatalf("first read: %d etag=%q", w.Code, etag)
	}
	notModified := func(step, tag string) {
		t.Helper()
		w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, tag)
		if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != tag {
			t.Fatalf("%s: %d etag=%q body=%q, want 304 with the same etag and no body",
				step, w.Code, w.Header().Get("ETag"), w.Body.String())
		}
	}
	changed := func(step, tag string) string {
		t.Helper()
		w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, tag)
		next := w.Header().Get("ETag")
		if w.Code != 200 || next == "" || next == tag || w.Body.Len() == 0 {
			t.Fatalf("%s: %d etag=%q (was %q), want 200 with a new etag", step, w.Code, next, tag)
		}
		return next
	}
	notModified("unchanged", etag)
	// A list containing it matches too (weak comparison).
	if w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, `"other", `+etag); w.Code != 304 {
		t.Errorf("etag list: %d, want 304", w.Code)
	}
	// The owner's body differs (withheld sizes are not zeroed): never the
	// teammate's version.
	if w := teamViewIf(t, f.srv, "alice@x.com", f.chat.ID, etag); w.Code != 200 {
		t.Errorf("owner with the teammate's etag: %d, want 200", w.Code)
	}

	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("assistant", "next reply")}); err != nil {
		t.Fatal(err)
	}
	etag = changed("new message", etag)
	notModified("after message", etag)

	if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, "chart.png", false); err != nil {
		t.Fatal(err)
	}
	etag = changed("exclusion added", etag)
	// Swap which file is held back: same count, different set.
	if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, "chart.png", true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetOutputShared(f.ctx, "alice@x.com", f.chat.ID, "page.html", false); err != nil {
		t.Fatal(err)
	}
	etag = changed("exclusion swapped", etag)

	msgs, _ := f.st.LoadHistory(f.ctx, f.chat.ID)
	b, _ := json.Marshal(map[string]any{"branch_point_message_id": msgs[len(msgs)-1].ID})
	if w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(b)); w.Code != 201 {
		t.Fatalf("branch: %d %s", w.Code, w.Body.String())
	}
	etag = changed("viewer branched", etag)
	notModified("after branch", etag)
	// A message after the branch flips changed_since — and the etag.
	if _, err := f.st.AppendHistory(f.ctx, f.chat.ID, []agent.HistoryEntry{textEntry("user", "more")}); err != nil {
		t.Fatal(err)
	}
	etag = changed("message after branch", etag)

	// No read access: 404, not 304, with the CURRENT etag in hand.
	if w := teamViewIf(t, f.srv, "zoe@x.com", f.chat.ID, etag); w.Code != 404 || w.Header().Get("ETag") != "" {
		t.Errorf("other team with a matching etag: %d etag=%q, want 404 and no etag", w.Code, w.Header().Get("ETag"))
	}
	if w := teamViewIf(t, f.srv, "zoe@x.com", f.chat.ID, "*"); w.Code != 404 {
		t.Errorf("other team with *: %d, want 404", w.Code)
	}
	if _, err := f.st.SetConversationTeamVisible(f.ctx, "alice@x.com", f.chat.ID, false); err != nil {
		t.Fatal(err)
	}
	if w := teamViewIf(t, f.srv, "bob@x.com", f.chat.ID, etag); w.Code != 404 || w.Header().Get("ETag") != "" {
		t.Errorf("unshared with a matching etag: %d etag=%q, want 404 and no etag", w.Code, w.Header().Get("ETag"))
	}
}
