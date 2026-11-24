package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A conversation directory that the conversation's own sandbox renamed and
// replaced with a symbolic link to a directory outside the workspace root.
// Every host-side path in this package that reads, lists, writes, moves,
// chmods or removes inside a conversation's workspace must refuse it and
// leave the outside directory exactly as it was.

const linkTestMarker = "OUTSIDE-MARKER-7d21"

// linkOutsideTree builds the directory the link points at: a marker file plus
// the shapes the host code touches (attachments/, user-skills/, out/), all
// 0o600/0o700 so a chmod to the workspace's 0o755 shows in the snapshot.
func linkOutsideTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"marker.txt":                linkTestMarker,
		"out/report.csv":            linkTestMarker,
		"attachments/keep/keep.txt": linkTestMarker,
		"user-skills/keep/SKILL.md": linkTestMarker,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// linkTreeSnapshot lists every entry under dir with its mode and a content
// hash for regular files.
func linkTreeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		entry := rel + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(b)
			entry += " " + hex.EncodeToString(sum[:8])
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// linkCase is one sink's conversation: its id and its directory path, which
// is a symbolic link to an outside tree.
type linkCase struct {
	t       *testing.T
	convID  string
	convDir string
}

func TestConversationDirSymlinkIsNeverFollowed(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "owner@example.com"
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)

	newConversation := func(t *testing.T) string {
		t.Helper()
		w := do(t, h, http.MethodPost, "/conversations", map[string]any{"persona": "victoria"}, owner)
		if w.Code != http.StatusOK {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.ID == "" {
			t.Fatalf("create: %v %s", err, w.Body.String())
		}
		return resp.ID
	}
	// A real source conversation for the branch copy.
	srcID := newConversation(t)
	if err := os.MkdirAll(filepath.Join(wsRoot, srcID, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, srcID, "out", "a.csv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An upload to stage.
	uploads := userUploadsRoot(t.TempDir(), owner)
	upload := filepath.Join(uploads, "tok", "doc.txt")
	if err := os.MkdirAll(filepath.Dir(upload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upload, []byte("doc"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	sinks := []linkSink{
		{"GET workspace file (owner)", func(c linkCase) string {
			w := do(c.t, h, http.MethodGet, "/conversations/"+c.convID+"/workspace/marker.txt", nil, owner)
			if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != "file not found" {
				c.t.Errorf("status %d body %q, want the missing-file 404", w.Code, w.Body.String())
			}
			return w.Body.String()
		}},
		{"GET nested workspace file (owner)", func(c linkCase) string {
			w := do(c.t, h, http.MethodGet, "/conversations/"+c.convID+"/workspace/out/report.csv", nil, owner)
			if w.Code != http.StatusNotFound {
				c.t.Errorf("status %d, want 404", w.Code)
			}
			return w.Body.String()
		}},
		{"@file context handle", func(c linkCase) string {
			blocks, notices := expandContextHandles(ctx, `@file:"marker.txt"`, c.convDir)
			if len(blocks) != 0 || len(notices) != 1 {
				c.t.Errorf("blocks %v notices %v, want one notice and nothing expanded", blocks, notices)
			}
			return strings.Join(blocks, "\n") // the notice echoes the requested name by design
		}},
		{"workspace inventory block", func(c linkCase) string {
			return appendWorkspaceInventoryBlock("hi", c.convDir)
		}},
		{"email preview cid images", func(c linkCase) string {
			// Inlined bytes would be base64 in a data: URL, which the marker
			// check below cannot see: require the document unchanged.
			const doc = `<img src="cid:m">`
			args := map[string]any{"inline_attachments": []any{map[string]any{"cid": "m", "path": "marker.txt"}}}
			out := expandCidImagesToDataURLs(doc, args, c.convID)
			if out != doc {
				c.t.Errorf("preview inlined something through the link: %.120q", out)
			}
			return out
		}},
		{"attachment staging", func(c linkCase) string {
			out := stageAttachmentsIntoWorkspace(ctx, uploads, c.convID, []chatAttachment{{Name: "doc.txt", Path: upload, Size: 3}})
			if len(out) != 1 || out[0].Path != upload {
				c.t.Errorf("staged = %+v, want the attachment left unstaged", out)
			}
			return ""
		}},
		{"shared output read (team files)", func(c linkCase) string {
			f, _, err := openWorkspaceFileNoFollow(ctx, c.convDir, "out/report.csv")
			if err == nil {
				defer f.Close()
				b := make([]byte, 64)
				n, _ := f.Read(b)
				return string(b[:n])
			}
			return ""
		}},
		{"project Sources walk", func(c linkCase) string {
			files, _ := walkWorkspaceFiles(ctx, c.convID, 50)
			names := make([]string, 0, len(files))
			for _, f := range files {
				names = append(names, f.Path)
			}
			return strings.Join(names, "\n")
		}},
		{"user skill materialization", func(c linkCase) string {
			_ = s.materializeUserSkills(ctx, owner, c.convID)
			return ""
		}},
		{"branch copy into the linked conversation", func(c linkCase) string {
			copied, _ := copySharedOutputsIntoBranch(ctx, srcID, c.convID, []outputFile{{Path: "out/a.csv", Shared: true}}, nil)
			if len(copied) != 0 {
				c.t.Errorf("copied %v", copied)
			}
			return ""
		}},
		{"participant turn: private workspace swap", func(c linkCase) string {
			conv := &store.Conversation{ID: c.convID, UserEmail: owner}
			if err := s.activateTurnPrivateWorkspace(conv, "bob@example.com"); err == nil {
				c.t.Error("a participant turn started over a conversation directory that is a symbolic link")
			}
			s.restoreTurnPrivateWorkspace(conv, "bob@example.com")
			return ""
		}},
	}
	runLinkSinks(t, wsRoot, newConversation, sinks)

	// A real conversation directory still works through the same paths.
	convID := newConversation(t)
	if err := os.Mkdir(filepath.Join(wsRoot, convID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, convID, "mine.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, http.MethodGet, "/conversations/"+convID+"/workspace/mine.txt", nil, owner)
	if w.Code != http.StatusOK || w.Body.String() != "mine" {
		t.Fatalf("a real conversation directory: %d %q", w.Code, w.Body.String())
	}
	// Positive control for the cid sub-case: a real directory's file is
	// inlined as a data: URL.
	args := map[string]any{"inline_attachments": []any{map[string]any{"cid": "m", "path": "mine.txt"}}}
	if out := expandCidImagesToDataURLs(`<img src="cid:m">`, args, convID); !strings.Contains(out, "data:") {
		t.Fatalf("a real conversation directory's cid image was not inlined: %q", out)
	}
}

// After a participant turn the owner's stash directory is removed once it is
// empty, and a recovery check with nothing to recover does not open (or
// create) the conversation directory at all.
func TestPrivateStashLeavesNoEmptyOwnerDir(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	stash := t.TempDir()
	const convID = "conv-stash"

	// Rollback moves the owner's files back and removes the emptied owner/.
	if err := os.MkdirAll(filepath.Join(stash, "owner", uploadsDir, "u1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wsRoot, convID), 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := tools.OpenConversationDir(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackOwnerStash(ws.Root, stash); err != nil {
		t.Fatal(err)
	}
	_ = ws.Close()
	if _, err := os.Stat(filepath.Join(wsRoot, convID, uploadsDir, "u1")); err != nil {
		t.Fatalf("owner files not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stash, "owner")); !os.IsNotExist(err) {
		t.Fatalf("an empty owner/ was left in the stash: %v", err)
	}

	// An empty owner/ (left by an earlier version) and no phase marker:
	// nothing to recover, so the conversation directory is not created.
	other := "conv-idle"
	if err := os.Mkdir(filepath.Join(stash, "owner"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedParticipantWorkspace(other, stash, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(wsRoot, other)); !os.IsNotExist(err) {
		t.Fatalf("recovery with nothing to recover created the conversation directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stash, "owner")); !os.IsNotExist(err) {
		t.Fatalf("the empty owner/ was not removed: %v", err)
	}
}

// linkSink is one host-side path checked against a conversation directory
// that is a symbolic link: run exercises it and returns anything it would
// show a user or the model.
type linkSink struct {
	name string
	run  func(c linkCase) string
}

// runLinkSinks gives each sink a conversation of its own whose directory is a
// link to a fresh outside tree, so every refusal is checked on its own: no
// output may show the outside tree, the tree must be left exactly as it was,
// and the link must still be a link.
func runLinkSinks(t *testing.T, wsRoot string, newConversation func(*testing.T) string, sinks []linkSink) {
	t.Helper()
	for _, sk := range sinks {
		t.Run(sk.name, func(t *testing.T) {
			convID := newConversation(t)
			outside := linkOutsideTree(t)
			if err := os.Symlink(outside, filepath.Join(wsRoot, convID)); err != nil {
				t.Fatal(err)
			}
			before := linkTreeSnapshot(t, outside)
			out := sk.run(linkCase{t: t, convID: convID, convDir: tools.WorkspaceDirForConversation(convID)})
			if strings.Contains(out, linkTestMarker) || strings.Contains(out, "marker.txt") || strings.Contains(out, "report.csv") {
				t.Errorf("%s: output shows the outside directory: %q", sk.name, out)
			}
			if after := linkTreeSnapshot(t, outside); after != before {
				t.Fatalf("%s changed the outside directory:\nbefore\n%s\nafter\n%s", sk.name, before, after)
			}
			if info, err := os.Lstat(filepath.Join(wsRoot, convID)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("%s replaced or removed the conversation link: %v %v", sk.name, info, err)
			}
		})
	}
}

// The chat transfer's two workspace steps — purging the previous owner's
// skills, and moving the new owner's participant attachments in — refuse a
// conversation directory that is a symbolic link and touch nothing it points
// at.
func TestTransferStepsNeverFollowAConversationDirLink(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "owner@example.com"
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	newConversation := func(t *testing.T) string {
		t.Helper()
		w := do(t, h, http.MethodPost, "/conversations", map[string]any{"persona": "victoria"}, owner)
		var resp struct {
			ID string `json:"id"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.ID == "" {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		return resp.ID
	}
	ctx := context.Background()
	runLinkSinks(t, wsRoot, newConversation, []linkSink{
		{"the previous owner's skills are purged", func(c linkCase) string {
			if err := s.purgeOwnerSkills(ctx, c.convID); !errors.Is(err, tools.ErrWorkspaceDirLink) {
				c.t.Errorf("purge err = %v, want the refusal of a conversation directory that is a symbolic link", err)
			}
			return ""
		}},
		{"the new owner's participant attachments move in", func(c linkCase) string {
			const next = "next@example.com"
			stash, err := s.privateWorkspaceStashDir(c.convID)
			if err != nil {
				c.t.Fatal(err)
			}
			theirs := filepath.Join(stash, "participants", participantDigest(next), uploadsDir)
			if err := os.MkdirAll(theirs, 0o755); err != nil {
				c.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(theirs, "brief.csv"), []byte("a,b\n"), 0o600); err != nil {
				c.t.Fatal(err)
			}
			s.promoteParticipantAttachments(ctx, c.convID, next)
			if _, err := os.Stat(filepath.Join(theirs, "brief.csv")); err != nil {
				c.t.Errorf("the attachment left the stash for a conversation directory that is a symbolic link: %v", err)
			}
			return ""
		}},
	})
}

// A participant's chat turn in a conversation whose directory is a symbolic
// link fails with a fixed message: the detail, which names host paths, is
// logged, never sent to the browser.
func TestParticipantTurnOverLinkedConversationDirGetsAFixedMessage(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	engine := &fakeEngine{}
	srv := newDefaultChatServer(t, engine, newFakeChatStore())
	w := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations", "alice@reklaim.com", map[string]any{"title": "linked"})
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}
	if w := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", "alice@reklaim.com", map[string]any{"multiplayer": true}); w.Code != http.StatusOK {
		t.Fatalf("enable multiplayer: %d %s", w.Code, w.Body.String())
	}
	if err := os.Symlink(linkOutsideTree(t), filepath.Join(wsRoot, conv.ID)); err != nil {
		t.Fatal(err)
	}
	w = doAuthedJSONRequest(t, srv, http.MethodPost, "/chat", "bob@reklaim.com", map[string]any{
		"conversation_id": conv.ID,
		"message":         "hello",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "workspace unavailable") || strings.Contains(body, wsRoot) || strings.Contains(body, conv.ID+":") {
		t.Fatalf("body %q: want the fixed message and no host path", body)
	}
}

// A directory whose name is not valid UTF-8 is sized and copied like any
// other: the walks go through descriptors (tools.WalkNoFollow), not Root.FS,
// which refuses such names.
func TestNoFollowWalksHandleNonUTF8Names(t *testing.T) {
	const odd = "dir-\xff\xfe"
	root := t.TempDir()
	conv := filepath.Join(root, "conv-odd")
	if err := os.MkdirAll(filepath.Join(conv, uploadsDir, odd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, uploadsDir, odd, "f.txt"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("admin storage sizing", func(t *testing.T) {
		got := duWorkspaceDir(context.Background(), root, "conv-odd")
		if got.Files != 2 || got.Bytes != 8 {
			t.Fatalf("sized %d files / %d bytes, want 2 / 8", got.Files, got.Bytes)
		}
	})

	t.Run("private directory copy", func(t *testing.T) {
		src, err := os.OpenRoot(conv)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		dstDir := t.TempDir()
		dst, err := os.OpenRoot(dstDir)
		if err != nil {
			t.Fatal(err)
		}
		defer dst.Close()
		if err := copyDirBetween(src, uploadsDir, dst, "copy"); err != nil {
			t.Fatalf("copyDirBetween: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(dstDir, "copy", odd, "f.txt")); err != nil || string(b) != "12345" {
			t.Fatalf("copied file = %q, %v", b, err)
		}
	})

	t.Run("an unreadable directory fails the copy instead of being dropped", func(t *testing.T) {
		locked := filepath.Join(conv, uploadsDir, "locked")
		if err := os.Mkdir(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "keep.txt"), []byte("k"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		src, err := os.OpenRoot(conv)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		dst, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer dst.Close()
		if err := copyDirBetween(src, uploadsDir, dst, "copy"); err == nil {
			t.Fatal("a directory that could not be read was silently left out of the copy")
		}
	})
}

// A transfer of a chat whose conversation directory is a symbolic link is
// refused before it commits: the previous owner's skills cannot be purged
// through the link, so the chat stays where it is and nothing the link points
// at is touched.
func TestTransferOverLinkedConversationDirIsRefused(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	st := newScheduleFakeStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	defer srv.StopBackground()
	h := srv.Routes()
	ctx := context.Background()
	const owner, next = "jeanne@elcanotek.com", "client@reklaim.com"
	conv, err := st.CreateConversation(ctx, owner, "linked", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	outside := linkOutsideTree(t)
	if err := os.Symlink(outside, filepath.Join(wsRoot, conv.ID)); err != nil {
		t.Fatal(err)
	}
	before := linkTreeSnapshot(t, outside)
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/transfer", strings.NewReader(`{"to_email":"`+next+`"}`))
	req = req.WithContext(asUser(ctx, owner))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != "transfer failed" {
		t.Errorf("transfer = %d %q, want 500 transfer failed", rec.Code, rec.Body.String())
	}
	if c, _ := st.Get(ctx, owner, conv.ID); c == nil {
		t.Error("the chat changed hands although its workspace could not be prepared")
	}
	if after := linkTreeSnapshot(t, outside); after != before {
		t.Fatalf("the transfer changed the outside directory:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// A symbolic link at attachments/ inside a real conversation directory holds
// no attachment data: the transfer replaces it with a real directory and
// moves the new owner's participant attachments there, writing nothing
// through the link — whether it points inside the conversation or out of the
// workspace.
func TestTransferReplacesALinkedAttachmentsDirectory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T, conv string) string
	}{
		{"a link to another directory of the conversation", func(t *testing.T, conv string) string {
			old := filepath.Join(conv, "old")
			if err := os.Mkdir(old, 0o755); err != nil {
				t.Fatal(err)
			}
			return old
		}},
		{"a link out of the workspace", func(t *testing.T, _ string) string { return linkOutsideTree(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
			srv := newDefaultChatServer(t, &fakeEngine{}, newScheduleFakeStore())
			defer srv.StopBackground()
			const convID, next = "conv-att", "next@example.com"
			conv := filepath.Join(wsRoot, convID)
			if err := os.Mkdir(conv, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target(t, conv)
			before := linkTreeSnapshot(t, target)
			if err := os.Symlink(target, filepath.Join(conv, uploadsDir)); err != nil {
				t.Fatal(err)
			}
			stash, err := srv.privateWorkspaceStashDir(convID)
			if err != nil {
				t.Fatal(err)
			}
			theirs := filepath.Join(stash, "participants", participantDigest(next), uploadsDir)
			if err := os.MkdirAll(theirs, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(theirs, "brief.csv"), []byte("a,b\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			srv.promoteParticipantAttachments(context.Background(), convID, next)

			info, err := os.Lstat(filepath.Join(conv, uploadsDir))
			if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
				t.Fatalf("attachments/ is not a real directory after the move: %v %v", info, err)
			}
			if b, err := os.ReadFile(filepath.Join(conv, uploadsDir, "brief.csv")); err != nil || string(b) != "a,b\n" {
				t.Errorf("the attachment did not move into attachments/: %q %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(theirs, "brief.csv")); !os.IsNotExist(err) {
				t.Errorf("the attachment is still in the stash: %v", err)
			}
			if after := linkTreeSnapshot(t, target); after != before {
				t.Fatalf("something was written through the link:\nbefore\n%s\nafter\n%s", before, after)
			}
		})
	}
}

// A transfer whose recovery finds a participant swap a crashed process left
// half done, over a conversation directory that is a symbolic link, is
// refused by the recovery before anything moves: the chat stays with its
// owner, the owner's files stay in the stash, and nothing the link points at
// is touched.
func TestTransferRecoveryRefusesALinkedConversationDir(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	st := newScheduleFakeStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	defer srv.StopBackground()
	ctx := context.Background()
	const owner, next, mate = "jeanne@elcanotek.com", "client@reklaim.com", "mate@elcanotek.com"
	conv, err := st.CreateConversation(ctx, owner, "linked", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	stash, err := srv.privateWorkspaceStashDir(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownerFile := filepath.Join(stash, "owner", uploadsDir, "u1", "brief.csv")
	if err := os.MkdirAll(filepath.Dir(ownerFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerFile, []byte("owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stash, "active-participant"), []byte(mate), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := linkOutsideTree(t)
	if err := os.Symlink(outside, filepath.Join(wsRoot, conv.ID)); err != nil {
		t.Fatal(err)
	}
	before := linkTreeSnapshot(t, outside)
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/transfer", strings.NewReader(`{"to_email":"`+next+`"}`))
	req = req.WithContext(asUser(ctx, owner))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != "transfer failed" {
		t.Errorf("transfer = %d %q, want 500 transfer failed", rec.Code, rec.Body.String())
	}
	if c, _ := st.Get(ctx, owner, conv.ID); c == nil {
		t.Error("the chat changed hands although its workspace could not be recovered")
	}
	if b, err := os.ReadFile(ownerFile); err != nil || string(b) != "owner" {
		t.Errorf("the owner's stashed file was not kept: %q %v", b, err)
	}
	if after := linkTreeSnapshot(t, outside); after != before {
		t.Fatalf("the recovery changed the outside directory:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// A recovery check that cannot read the owner's stash directory does not
// treat it as empty: it goes on to the rollback, which reports the failure,
// so a transfer is not made with the owner's files still in the stash.
func TestRecoveryDoesNotTreatAnUnreadableOwnerStashAsEmpty(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	stash := t.TempDir()
	ownerDir := filepath.Join(stash, "owner")
	if err := os.MkdirAll(filepath.Join(ownerDir, uploadsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ownerDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ownerDir, 0o750) })
	if err := recoverInterruptedParticipantWorkspace("conv-unreadable", stash, nil); err == nil {
		t.Fatal("recovery reported nothing to do over an owner stash it could not read")
	}
}
