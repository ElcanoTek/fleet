package httpapi

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	note := f.srv.appendBranchFilesBlock(f.ctx, "", br.ID)
	if !strings.Contains(note, "`out/report.csv`") || strings.Contains(note, "held.json") || !strings.Contains(note, "NOT in this workspace") {
		t.Errorf("note = %q", note)
	}
	if again := f.srv.appendBranchFilesBlock(f.ctx, "x", br.ID); again != "x" {
		t.Errorf("second note = %q", again)
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

// The branch's "seen" high-water mark is read before the copy: a source
// message that lands WHILE the files are being copied is not in the branch,
// so the viewer is told the source has added messages since.
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

	n, err := copyOneOutput(src, dst, "report.csv", 1<<20)
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
	if n, err := copyOneOutput(src, dst, "ok.csv", 1<<20); err != nil || n != 4 {
		t.Fatalf("full copy = (%d, %v), want (4, nil)", n, err)
	}
}
