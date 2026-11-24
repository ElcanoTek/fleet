package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/migrations"
)

const uploadsTTL = 14 * 24 * time.Hour

// agedFile writes path (and its parents) and ages it 30 days, past the window.
func agedFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustTouch(t, path, "x", time.Now().Add(-30*24*time.Hour))
	return path
}

// imageEntry is a user message that attached the images at paths, stored the
// way a turn stores it.
func imageEntry(t *testing.T, paths ...string) agent.HistoryEntry {
	t.Helper()
	tc := agent.TextContent{Text: "look at this"}
	for _, p := range paths {
		tc.Images = append(tc.Images, agent.ImageRefMeta{Path: p, MediaType: "image/png", Name: filepath.Base(p)})
	}
	raw, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	return agent.HistoryEntry{Role: "user", Type: "text", Content: raw}
}

func onDisk(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", p, err)
	}
	return err == nil
}

// completeUploadsBackfill runs the record of earlier messages' uploads to the
// end, as the first server of a release does at boot; until it has, the
// cleanup keeps every upload.
func completeUploadsBackfill(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.BackfillMessageUploads(context.Background()); err != nil {
		t.Fatalf("BackfillMessageUploads: %v", err)
	}
}

func sweepAttachmentTree(t *testing.T, s *Store, attachDir string) {
	t.Helper()
	if _, err := SweepAttachments(attachDir, uploadsTTL, AttachmentKeeper(context.Background(), s, attachDir)); err != nil {
		t.Fatalf("SweepAttachments: %v", err)
	}
}

// TestAttachmentKeeperKeepsUploadsWhileAMessageRefersToThem: every way a
// message reaches the store records the uploads it refers to, the cleanup
// keeps those however old they are, and removing the messages (with their
// conversation, or by rewinding the chat) releases them.
func TestAttachmentKeeperKeepsUploadsWhileAMessageRefersToThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	completeUploadsBackfill(t, s)
	ctx := context.Background()
	const user = "owner@x.com"
	if _, err := s.CreateUser(ctx, user, "password123"); err != nil {
		t.Fatal(err)
	}
	attachDir := t.TempDir()
	up := func(token, name string) string {
		return agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "owner", token, name))
	}

	// A live turn's user message (CommitUserMessage, the turn projection).
	turnConv, err := s.CreateConversation(ctx, user, "turn", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	turnImg := up("turn", "a.png")
	if _, err := s.CommitUserMessage(ctx, turnConv.ID, "turn-1", imageEntry(t, turnImg)); err != nil {
		t.Fatal(err)
	}

	// A plain append, then a branch of it: the owner's branch keeps the image,
	// so deleting the parent does not release it.
	parent, err := s.CreateConversation(ctx, user, "parent", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	branchedImg := up("branched", "b.png")
	if _, err := s.AppendHistory(ctx, parent.ID, []agent.HistoryEntry{imageEntry(t, branchedImg)}); err != nil {
		t.Fatal(err)
	}
	hist, err := s.LoadHistory(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.BranchConversation(ctx, user, parent.ID, hist[len(hist)-1].ID, "fork")
	if err != nil {
		t.Fatal(err)
	}

	// A conversation brought in by the legacy importer.
	importedID := uuid.NewString()
	importedImg := up("imported", "c.png")
	now := time.Now().Unix()
	if _, err := s.ImportConversation(ctx, ImportedConversation{
		ID: importedID, UserEmail: user, Title: "imported", Persona: "victoria", CreatedAt: now, UpdatedAt: now,
		Messages: []ImportedMessage{{Role: "user", Type: "text", Content: imageEntry(t, importedImg).Content, CreatedAt: now}},
	}); err != nil {
		t.Fatal(err)
	}

	// A message the user later rewound away.
	rewound, err := s.CreateConversation(ctx, user, "rewound", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendHistory(ctx, rewound.ID, []agent.HistoryEntry{userEntryWithInjected(t, "first", "")}); err != nil {
		t.Fatal(err)
	}
	rewoundHist, err := s.LoadHistory(ctx, rewound.ID)
	if err != nil {
		t.Fatal(err)
	}
	rewoundImg := up("rewound", "d.png")
	if _, err := s.AppendHistory(ctx, rewound.ID, []agent.HistoryEntry{imageEntry(t, rewoundImg)}); err != nil {
		t.Fatal(err)
	}
	if err := s.TruncateAfter(ctx, user, rewound.ID, rewoundHist[0].ID); err != nil {
		t.Fatal(err)
	}

	neverSent := up("unsent", "e.png")
	stashed := agedFile(t, filepath.Join(attachDir, AttachmentPrivateStashDir, turnConv.ID, "participants", "p", "attachments", "f.csv"))
	outbound := agedFile(t, filepath.Join(attachDir, "outbound", "outbound-123", "g.pdf"))

	if err := s.Delete(ctx, user, parent.ID); err != nil {
		t.Fatal(err)
	}
	sweepAttachmentTree(t, s, attachDir)

	for name, p := range map[string]string{
		"image of a live turn":                  turnImg,
		"image a branch still carries":          branchedImg,
		"image of an imported conversation":     importedImg,
		"Multiplayer private stash (own sweep)": stashed,
	} {
		if !onDisk(t, p) {
			t.Errorf("%s was removed by the cleanup", name)
		}
	}
	for name, p := range map[string]string{
		"upload never sent":                    neverSent,
		"image of a rewound message":           rewoundImg,
		"outbound copy left behind by a crash": outbound,
	} {
		if onDisk(t, p) {
			t.Errorf("%s outlived the window", name)
		}
	}

	for _, id := range []string{turnConv.ID, branch.ID, importedID} {
		if err := s.Delete(ctx, user, id); err != nil {
			t.Fatal(err)
		}
	}
	sweepAttachmentTree(t, s, attachDir)
	for name, p := range map[string]string{"turn": turnImg, "branch": branchedImg, "imported": importedImg} {
		if onDisk(t, p) {
			t.Errorf("the %s image is still on disk after its conversation was deleted", name)
		}
	}
}

// TestAttachmentKeeperKeepsUploadsOfASoftDeletedConversation: a tombstoned
// chat keeps its rows for 30 days, and its images with them; the purge
// releases them.
func TestAttachmentKeeperKeepsUploadsOfASoftDeletedConversation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	completeUploadsBackfill(t, s)
	s.SetSoftDelete(true)
	ctx := context.Background()
	const user = "soft@x.com"
	if _, err := s.CreateUser(ctx, user, "password123"); err != nil {
		t.Fatal(err)
	}
	attachDir := t.TempDir()
	conv, err := s.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	img := agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "soft", "tok", "a.png"))
	if _, err := s.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{imageEntry(t, img)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, user, conv.ID); err != nil {
		t.Fatal(err)
	}
	sweepAttachmentTree(t, s, attachDir)
	if !onDisk(t, img) {
		t.Fatal("the image of a soft-deleted chat was removed before the purge")
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET deleted_at = NOW() - INTERVAL '31 days' WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SweepExpired(ctx, uploadsTTL, 0); err != nil {
		t.Fatal(err)
	}
	sweepAttachmentTree(t, s, attachDir)
	if onDisk(t, img) {
		t.Error("the image of a purged chat is still on disk")
	}
}

// TestSweepAttachmentsKeepsAFileWhoseUseCannotBeChecked: a failed lookup is
// not a "not in use"; the file stays and the sweep reports why.
func TestSweepAttachmentsKeepsAFileWhoseUseCannotBeChecked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := agedFile(t, filepath.Join(dir, AttachmentUploadsDir, "u", "tok", "a.png"))
	lookupErr := errors.New("database unavailable")
	_, err := SweepAttachments(dir, uploadsTTL, func(string) (bool, error) { return false, lookupErr })
	if !errors.Is(err, lookupErr) {
		t.Errorf("err = %v, want the lookup failure", err)
	}
	if !onDisk(t, p) {
		t.Error("a file whose use could not be checked was removed")
	}
}

// TestAttachmentKeeperWithoutAStoreKeepsUploads: with nothing to ask, no
// upload is treated as unused.
func TestAttachmentKeeperWithoutAStoreKeepsUploads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	upload := agedFile(t, filepath.Join(dir, AttachmentUploadsDir, "u", "tok", "a.png"))
	other := agedFile(t, filepath.Join(dir, "outbound", "x", "b.pdf"))
	if _, err := SweepAttachments(dir, uploadsTTL, AttachmentKeeper(context.Background(), nil, dir)); err != nil {
		t.Fatal(err)
	}
	if !onDisk(t, upload) {
		t.Error("an upload was removed with no way to tell whether a chat uses it")
	}
	if onDisk(t, other) {
		t.Error("an aged file outside uploads/ was kept")
	}
}

// TestMessageUploadsBackfillRecordsEarlierMessages: on upgrade, the migration
// only creates the table; until the server has recorded the images of the
// messages written before it, the cleanup keeps every upload. The backfill
// then records them — including a message whose text holds a \u0000 escape,
// which the database's own JSON type rejects, and leaving out an image path
// that holds U+0000 — resumes from its saved position, and marks the record
// complete, after which an upload no message names is removed.
func TestMessageUploadsBackfillRecordsEarlierMessages(t *testing.T) {
	t.Parallel()
	dsn := emptyTestDSN(t)
	ctx := context.Background()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := migrations.UpTo(ctx, raw, versionBeforeMessageUploads(t)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := raw.Exec(`INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ('old@x.com', 'x', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('old-chat', 'old@x.com', 't', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	attachDir := t.TempDir()
	up := func(token, name string) string {
		return agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "u", token, name))
	}
	img1, img2, nulImg := up("t1", "a.png"), up("t2", "b.png"), up("t3", "c.png")
	unsent := up("t4", "d.png")
	contents := []string{
		`{"text":"two","images":[{"path":"` + img1 + `","media_type":"image/png"},{"path":"` + img2 + `","media_type":"image/png"}]}`,
		`{"text":"no images here"}`,
		`not json, but it mentions "images"`,
		`{"text":"odd","images":"not a list"}`,
		`{"text":"a\u0000b","images":[{"path":"` + nulImg + `","media_type":"image/png"}]}`,
		// A path no upload can have: skipped, and the backfill goes on.
		`{"text":"bad path","images":[{"path":"` + filepath.Join(attachDir, AttachmentUploadsDir, "u", "t5") + `/e\u0000.png","media_type":"image/png"}]}`,
	}
	for _, content := range contents {
		if _, err := raw.Exec(`INSERT INTO chat.messages (conversation_id, role, type, content, created_at) VALUES ('old-chat', 'user', 'text', $1, $2)`, content, now); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(dsn, testPool()) // migrates through 0211: table only
	if err != nil {
		t.Fatalf("Open (migrating through 0211): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Before the backfill: nothing is recorded yet, so everything stays.
	sweepAttachmentTree(t, s, attachDir)
	for _, p := range []string{img1, img2, nulImg, unsent} {
		if !onDisk(t, p) {
			t.Errorf("%s was removed before the record of references was complete", p)
		}
	}

	// Part of the way, then resumed: the position is saved per range.
	if _, err := s.backfillMessageUploadsRange(ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	var through int64
	if err := raw.QueryRow(`SELECT through_id FROM chat.message_uploads_backfill`).Scan(&through); err != nil || through != 1 {
		t.Fatalf("saved position = %d (%v), want 1", through, err)
	}
	n, err := s.backfillMessageUploads(ctx, 2)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 { // the first message's two were recorded by the first range
		t.Errorf("resumed backfill recorded %d references, want 1 (the \\u0000 message's)", n)
	}
	for _, p := range []string{img1, img2, nulImg} {
		if ok, err := s.UploadReferenced(ctx, p); err != nil || !ok {
			t.Errorf("UploadReferenced(%s) = %v, %v; want true", p, ok, err)
		}
	}
	if n, err := s.BackfillMessageUploads(ctx); err != nil || n != 0 {
		t.Errorf("a later start recorded %d references again (%v); want 0", n, err)
	}

	sweepAttachmentTree(t, s, attachDir)
	for _, p := range []string{img1, img2, nulImg} {
		if !onDisk(t, p) {
			t.Errorf("%s was removed although an earlier message names it", p)
		}
	}
	if onDisk(t, unsent) {
		t.Error("an upload no message names was kept after the record was complete")
	}
}

// versionBeforeMessageUploads is the migration just before the one that adds
// chat.message_uploads, found by name so a renumbering at rebase needs no edit.
func versionBeforeMessageUploads(t *testing.T) int {
	t.Helper()
	set, err := migrations.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	prev := 0
	for _, m := range set.All() {
		if strings.HasSuffix(m.Name, "_message_uploads") {
			if prev == 0 {
				t.Fatal("the message_uploads migration has no predecessor")
			}
			return prev
		}
		prev = m.Version
	}
	t.Fatal("no message_uploads migration in the embedded set")
	return 0
}

// TestEachStartRecordsMessagesWrittenWithoutReferences: after the record is
// complete, a writer that does not record references — a server of an earlier
// release still running against the same database — can add a message naming
// an image. The next server start keeps every upload until it has scanned the
// messages added since the previous start, records that one, and only then
// lets the cleanup remove what nothing refers to.
func TestEachStartRecordsMessagesWrittenWithoutReferences(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	completeUploadsBackfill(t, s)
	seedAccount(t, s, "earlier@x.com")
	conv, err := s.CreateConversation(ctx, "earlier@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	attachDir := t.TempDir()
	img := agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "u", "earlier", "a.png"))
	unsent := agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "u", "unsent", "b.png"))
	// The way a writer of an earlier release stores it: no reference row.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'user', 'text', $2, $3)`,
		conv.ID, string(imageEntry(t, img).Content), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	// The next start: a new process on the same database.
	next, err := Open(dsn, testPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if st, err := next.MessageUploadsBackfillState(ctx); err != nil || st.Complete {
		t.Fatalf("state at the next start before its scan = %+v, %v; want not complete", st, err)
	}
	sweepAttachmentTree(t, next, attachDir)
	if !onDisk(t, img) || !onDisk(t, unsent) {
		t.Fatal("an upload was removed before this start had scanned the messages added since the last one")
	}

	n, err := next.BackfillMessageUploads(ctx)
	if err != nil || n != 1 {
		t.Fatalf("the next start's scan recorded %d, %v; want 1", n, err)
	}
	sweepAttachmentTree(t, next, attachDir)
	if !onDisk(t, img) {
		t.Error("the image of a message written without a reference was removed after the next start's scan")
	}
	if onDisk(t, unsent) {
		t.Error("an upload no message names was kept after the next start's scan")
	}
}

// countingReferences is an UploadReferences that counts its lookups.
type countingReferences struct {
	referenced map[string]struct{}
	complete   bool
	snapshots  int
	singles    int
}

func (c *countingReferences) ReferencedUploads(context.Context) (map[string]struct{}, bool, error) {
	c.snapshots++
	return c.referenced, c.complete, nil
}

func (c *countingReferences) UploadReferenced(_ context.Context, p string) (bool, error) {
	c.singles++
	_, ok := c.referenced[p]
	return ok, nil
}

// TestAttachmentKeeperLookupsDoNotGrowWithKeptFiles: however many aged
// uploads chats still use, one sweep loads the references once and asks
// about an upload on its own only when it is about to be removed.
func TestAttachmentKeeperLookupsDoNotGrowWithKeptFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	refs := &countingReferences{referenced: map[string]struct{}{}, complete: true}
	for i := range 200 {
		p := agedFile(t, filepath.Join(dir, AttachmentUploadsDir, "u", fmt.Sprintf("tok%03d", i), "a.png"))
		refs.referenced[normalizeUploadPath(p)] = struct{}{}
	}
	for i := range 3 {
		agedFile(t, filepath.Join(dir, AttachmentUploadsDir, "u", fmt.Sprintf("unsent%d", i), "b.png"))
	}
	removed, err := SweepAttachments(dir, uploadsTTL, AttachmentKeeper(context.Background(), refs, dir))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed %d, want the 3 unsent uploads", removed)
	}
	if refs.snapshots != 1 || refs.singles != 3 {
		t.Errorf("lookups: %d snapshots and %d single-upload lookups for 200 kept and 3 removable uploads; want 1 and 3", refs.snapshots, refs.singles)
	}
}

// TestAttachmentKeeperKeepsHeldUploadTrees: an upload tree an account
// deletion holds (its outcome unconfirmed) is kept whole, however old and
// unreferenced its files, until the maintenance pass puts it back or removes
// it; the rest of the uploads are swept as usual.
func TestAttachmentKeeperKeepsHeldUploadTrees(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	refs := &countingReferences{referenced: map[string]struct{}{}, complete: true}
	held := ".held-" + strings.Repeat("a", 32) + "-" + hex.EncodeToString([]byte("acct-1")) + "-0123456789abcdef"
	kept := agedFile(t, filepath.Join(dir, AttachmentUploadsDir, held, "tok", "a.png"))
	unsent := agedFile(t, filepath.Join(dir, AttachmentUploadsDir, "u", "tok", "b.png"))
	if _, err := SweepAttachments(dir, uploadsTTL, AttachmentKeeper(context.Background(), refs, dir)); err != nil {
		t.Fatal(err)
	}
	if !onDisk(t, kept) {
		t.Error("a file of a held upload tree was removed by age")
	}
	if onDisk(t, unsent) {
		t.Error("an unsent upload outside the held tree was kept")
	}
}

// TestAttachmentKeeperKeepsEveryUploadUntilTheRecordIsComplete: until a
// server of this release has finished recording earlier messages' uploads —
// on a new, empty database too — no upload counts as unused.
func TestAttachmentKeeperKeepsEveryUploadUntilTheRecordIsComplete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	attachDir := t.TempDir()
	unsent := agedFile(t, filepath.Join(attachDir, AttachmentUploadsDir, "u", "tok", "a.png"))
	sweepAttachmentTree(t, s, attachDir)
	if !onDisk(t, unsent) {
		t.Fatal("an upload was removed before the record of references was complete")
	}
	completeUploadsBackfill(t, s)
	sweepAttachmentTree(t, s, attachDir)
	if onDisk(t, unsent) {
		t.Error("an upload no message names was kept after the record was complete")
	}
}

// TestUnstorableImagePathsAreLeftOut: an image path that holds U+0000 or
// another control character, or is far longer than any upload path, names no
// upload and cannot be stored. A message naming one is still written — by a
// turn, an append or a legacy import — with its other images recorded.
func TestUnstorableImagePathsAreLeftOut(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	completeUploadsBackfill(t, s)
	ctx := context.Background()
	const user = "odd@x.com"
	good := "/data/attachments/uploads/u/tok/good.png"
	bad := []string{
		"/data/attachments/uploads/u/tok/a\x00b.png",
		"/data/attachments/uploads/u/tok/line\nbreak.png",
		"/data/attachments/uploads/u/tok/" + strings.Repeat("x", 3000) + ".png",
	}
	paths := append([]string{good}, bad...)

	seedAccount(t, s, user)
	conv, err := s.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{imageEntry(t, paths...)}); err != nil {
		t.Fatalf("AppendHistory with an unstorable image path: %v", err)
	}
	turnConv, err := s.CreateConversation(ctx, user, "turn", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, turnConv.ID, "turn-odd", imageEntry(t, paths...)); err != nil {
		t.Fatalf("CommitUserMessage with an unstorable image path: %v", err)
	}
	now := time.Now().Unix()
	if _, err := s.ImportConversation(ctx, ImportedConversation{
		ID: uuid.NewString(), UserEmail: user, Title: "imported", Persona: "victoria", CreatedAt: now, UpdatedAt: now,
		Messages: []ImportedMessage{{Role: "user", Type: "text", Content: imageEntry(t, paths...).Content, CreatedAt: now}},
	}); err != nil {
		t.Fatalf("ImportConversation with an unstorable image path: %v", err)
	}

	var recorded []string
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT upload_path FROM message_uploads`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, p)
	}
	if len(recorded) != 1 || recorded[0] != good {
		t.Errorf("recorded %q, want only %q", recorded, good)
	}
}

// backfillSkipFixture is a store with three messages naming one image each
// and no references recorded yet, as after the upgrade, plus the paths.
func backfillSkipFixture(t *testing.T) (*Store, [3]string) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "skip@x.com")
	conv, err := s.CreateConversation(ctx, "skip@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	paths := [3]string{"/data/attachments/uploads/u/t1/a.png", "/data/attachments/uploads/u/t2/b.png", "/data/attachments/uploads/u/t3/c.png"}
	for _, p := range paths {
		if _, err := s.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{imageEntry(t, p)}); err != nil {
			t.Fatal(err)
		}
	}
	// Recorded as written: forget them, so the backfill has to.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM message_uploads`); err != nil {
		t.Fatal(err)
	}
	return s, paths
}

// failBackfillInsert swaps the backfillInsert seam for one that answers
// fail(refs) before inserting.
func failBackfillInsert(t *testing.T, fail func(refs []messageUploadRef) error) {
	t.Helper()
	prev := backfillInsert
	backfillInsert = func(ctx context.Context, tx *sql.Tx, refs []messageUploadRef) error {
		if err := fail(refs); err != nil {
			return err
		}
		return prev(ctx, tx, refs)
	}
	t.Cleanup(func() { backfillInsert = prev })
}

func namesPath(refs []messageUploadRef, p string) bool {
	for _, r := range refs {
		if r.path == p {
			return true
		}
	}
	return false
}

// TestMessageUploadsBackfillSkipsAMessageItCannotRecord: a message whose
// uploads Postgres refuses as data is logged and skipped; the rest of its
// range is recorded one message at a time and the record completes.
func TestMessageUploadsBackfillSkipsAMessageItCannotRecord(t *testing.T) {
	// Not parallel: it swaps the package-level backfillInsert seam.
	s, paths := backfillSkipFixture(t)
	ctx := context.Background()
	first, broken, last := paths[0], paths[1], paths[2]
	failBackfillInsert(t, func(refs []messageUploadRef) error {
		if namesPath(refs, broken) {
			return &pgconn.PgError{Code: "22021", Message: "invalid byte sequence for encoding \"UTF8\": 0x00"}
		}
		return nil
	})

	n, err := s.BackfillMessageUploads(ctx)
	if err != nil {
		t.Fatalf("backfill stopped on one bad message: %v", err)
	}
	if n != 2 {
		t.Errorf("recorded %d references, want 2", n)
	}
	for p, want := range map[string]bool{first: true, broken: false, last: true} {
		if ok, err := s.UploadReferenced(ctx, p); err != nil || ok != want {
			t.Errorf("UploadReferenced(%s) = %v, %v; want %v", p, ok, err, want)
		}
	}
	if st, err := s.MessageUploadsBackfillState(ctx); err != nil || !st.Complete {
		t.Errorf("state = %+v, %v; want complete", st, err)
	}
}

// TestMessageUploadsBackfillRetriesAFailedAttempt: a failure that is about
// the attempt, not the data — a cancel, a full disk, a lost connection, in the
// one-statement insert or in the message-by-message one — skips nothing: the
// range rolls back, its position is not saved, nothing is marked complete, and
// a later attempt records every message.
func TestMessageUploadsBackfillRetriesAFailedAttempt(t *testing.T) {
	// Not parallel: it swaps the package-level backfillInsert seam.
	cases := []struct {
		name string
		fail func(paths [3]string) func([]messageUploadRef) error
	}{
		{"query cancelled", func([3]string) func([]messageUploadRef) error {
			return func([]messageUploadRef) error {
				return &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
			}
		}},
		{"disk full", func([3]string) func([]messageUploadRef) error {
			return func([]messageUploadRef) error {
				return &pgconn.PgError{Code: "53100", Message: "could not extend file"}
			}
		}},
		{"connection lost", func([3]string) func([]messageUploadRef) error {
			return func([]messageUploadRef) error { return errors.New("conn closed") }
		}},
		{"timeout while recording one message at a time", func(paths [3]string) func([]messageUploadRef) error {
			return func(refs []messageUploadRef) error {
				switch {
				case len(refs) > 1:
					return &pgconn.PgError{Code: "22021", Message: "invalid byte sequence"}
				case namesPath(refs, paths[2]):
					return &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
				}
				return nil
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, paths := backfillSkipFixture(t)
			ctx := context.Background()
			failing := true
			fail := c.fail(paths)
			failBackfillInsert(t, func(refs []messageUploadRef) error {
				if !failing {
					return nil
				}
				return fail(refs)
			})
			if _, err := s.BackfillMessageUploads(ctx); err == nil {
				t.Fatal("the attempt reported success")
			}
			st, err := s.MessageUploadsBackfillState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.Complete || st.ThroughID != 0 {
				t.Errorf("state after a failed attempt = %+v; want position 0, not complete", st)
			}
			for _, p := range paths {
				if ok, _ := s.UploadReferenced(ctx, p); ok {
					t.Errorf("%s was recorded by an attempt that rolled back", p)
				}
			}

			failing = false
			if n, err := s.BackfillMessageUploads(ctx); err != nil || n != 3 {
				t.Fatalf("retry recorded %d, %v; want 3, nil", n, err)
			}
			if st, err := s.MessageUploadsBackfillState(ctx); err != nil || !st.Complete {
				t.Errorf("state after the retry = %+v, %v; want complete", st, err)
			}
		})
	}
}

// TestMessageUploadsBackfillCoversMessagesWrittenAfterTheMigration: a server
// of the previous release can keep writing messages after a command of this
// release migrated the database. The backfill raises its upper bound when it
// starts, so those are recorded too.
func TestMessageUploadsBackfillCoversMessagesWrittenAfterTheMigration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "late@x.com")
	conv, err := s.CreateConversation(ctx, "late@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	const late = "/data/attachments/uploads/u/late/a.png"
	// The way the previous release writes a message: no reference row.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'user', 'text', $2, $3)`,
		conv.ID, string(imageEntry(t, late).Content), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	before, err := s.MessageUploadsBackfillState(ctx)
	if err != nil || before.Complete {
		t.Fatalf("state before = %+v, %v; want incomplete", before, err)
	}
	completeUploadsBackfill(t, s)
	if ok, err := s.UploadReferenced(ctx, late); err != nil || !ok {
		t.Errorf("a message written after the migration was not recorded (%v, %v)", ok, err)
	}
	after, err := s.MessageUploadsBackfillState(ctx)
	if err != nil || !after.Complete || after.UptoID <= before.UptoID {
		t.Errorf("state after = %+v (before %+v), %v; want complete with a raised bound", after, before, err)
	}
}

// TestImportRecordsManyUploadsInOneStatement: a large legacy import (5000
// messages with 8 images each, 40000 references) records every reference.
func TestImportRecordsManyUploadsInOneStatement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	msgs := make([]ImportedMessage, 5000)
	for i := range msgs {
		paths := make([]string, 8)
		for j := range paths {
			paths[j] = fmt.Sprintf("/data/attachments/uploads/u/m%04d/i%d.png", i, j)
		}
		msgs[i] = ImportedMessage{Role: "user", Type: "text", Content: imageEntry(t, paths...).Content, CreatedAt: now}
	}
	if _, err := s.ImportConversation(ctx, ImportedConversation{
		ID: uuid.NewString(), UserEmail: "bulk@x.com", Title: "bulk", Persona: "victoria", CreatedAt: now, UpdatedAt: now, Messages: msgs,
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM message_uploads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 40000 {
		t.Errorf("recorded %d references, want 40000", n)
	}
}

// TestSweepOrphanWorkspacesSparesAConversationCreatedDuringTheSweep: the live
// set is read before the directory listing; a conversation created in between
// keeps its directory.
func TestSweepOrphanWorkspacesSparesAConversationCreatedDuringTheSweep(t *testing.T) {
	// Not parallel: it sets a package-level hook.
	s := newTestStore(t)
	ctx := context.Background()
	const user = "late@x.com"
	if _, err := s.CreateUser(ctx, user, "password123"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var lateDir string
	afterLiveSnapshot = func() {
		conv, err := s.CreateConversation(ctx, user, "late", "victoria", "", false)
		if err != nil {
			t.Error(err)
			return
		}
		lateDir = filepath.Join(root, conv.ID)
		if err := os.MkdirAll(lateDir, 0o750); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterLiveSnapshot = nil })
	orphan := filepath.Join(root, "ff638dee-ffcb-497d-ba91-e84fd6f94ae2")
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatal(err)
	}

	removed, err := s.SweepOrphanWorkspaces(ctx, root)
	if err != nil {
		t.Fatalf("SweepOrphanWorkspaces: %v", err)
	}
	if lateDir == "" {
		t.Fatal("the hook did not run")
	}
	if !onDisk(t, lateDir) {
		t.Error("the directory of a conversation created during the sweep was removed")
	}
	if onDisk(t, orphan) || removed != 1 {
		t.Errorf("orphan removed=%d, still on disk=%v; want 1, false", removed, onDisk(t, orphan))
	}
}

// TestDeleteUnpinnedOlderThanSparesAChatWithAPatrol: the admin "Clean up now"
// removes exactly what the retention sweep would — a chat whose patrol is
// armed is kept by both, and the panel's preview does not count it.
func TestDeleteUnpinnedOlderThanSparesAChatWithAPatrol(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const user = "patrol@x.com"
	seedAccount(t, s, user)
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()

	patrolled, err := s.CreateConversation(ctx, user, "patrolled", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: patrolled.ID, UserEmail: user, Title: "daily check",
		Prompt: "check pacing", CronExpr: "0 9 * * *", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	backdateConversation(t, s, patrolled.ID, old)
	stale, err := s.CreateConversation(ctx, user, "stale", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	backdateConversation(t, s, stale.ID, old)

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	stats, err := s.StorageConversationStats(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Protected != 1 || stats.ReclaimableAtCutoff != 1 {
		t.Errorf("stats = %+v, want Protected 1 (the patrolled chat) and ReclaimableAtCutoff 1", stats)
	}
	n, err := s.DeleteUnpinnedOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d, want 1", n)
	}
	if c, _ := s.Get(ctx, user, patrolled.ID); c == nil {
		t.Error("the admin cleanup deleted a chat with an armed patrol")
	}
	if c, _ := s.Get(ctx, user, stale.ID); c != nil {
		t.Error("the stale temporary chat survived the cleanup")
	}
}

// TestUploadReferencesLandOnTheMessageThatNamesThem: in a multi-message write
// — a legacy import, an append, a turn's projection — each upload reference is
// attached to the very message that names its path, the returned ids are the
// inputs' own in input order, and history reads back in that order.
func TestUploadReferencesLandOnTheMessageThatNamesThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const user = "order@x.com"
	seedAccount(t, s, user)
	path := func(kind string, i int) string {
		return fmt.Sprintf("/data/attachments/uploads/u/%s%03d/img.png", kind, i)
	}
	// Images on every third message, plain text in between, so a shifted
	// pairing would move a reference onto a message that names nothing.
	batch := func(kind string, n int) []agent.HistoryEntry {
		out := make([]agent.HistoryEntry, n)
		for i := range out {
			if i%3 == 0 {
				out[i] = imageEntry(t, path(kind, i))
			} else {
				out[i] = userEntryWithInjected(t, fmt.Sprintf("%s plain %d", kind, i), "")
			}
		}
		return out
	}

	now := time.Now().Unix()
	importedID := uuid.NewString()
	imported := batch("imp", 300)
	msgs := make([]ImportedMessage, len(imported))
	for i, e := range imported {
		msgs[i] = ImportedMessage{Role: e.Role, Type: e.Type, Content: e.Content, CreatedAt: now}
	}
	if _, err := s.ImportConversation(ctx, ImportedConversation{
		ID: importedID, UserEmail: user, Title: "imported", Persona: "victoria", CreatedAt: now, UpdatedAt: now, Messages: msgs,
	}); err != nil {
		t.Fatal(err)
	}

	appended, err := s.CreateConversation(ctx, user, "appended", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	appendedEntries := batch("app", 60)
	ids, err := s.AppendHistory(ctx, appended.ID, appendedEntries)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("AppendHistory ids not rising in input order: %v", ids)
		}
	}

	turnConv, err := s.CreateConversation(ctx, user, "turn", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTurn(ctx, "order-turn", turnConv.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, turnConv.ID, "order-turn", userEntryWithInjected(t, "go", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTurnHistory(ctx, turnConv.ID, "order-turn", batch("turn", 30)); err != nil {
		t.Fatal(err)
	}

	// Every reference sits on a message whose content names its path.
	var misplaced, total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE position(mu.upload_path IN m.content) = 0), count(*)
		  FROM message_uploads mu JOIN messages m ON m.id = mu.message_id`,
	).Scan(&misplaced, &total); err != nil {
		t.Fatal(err)
	}
	if want := 100 + 20 + 10; total != want || misplaced != 0 {
		t.Errorf("references: %d total (want %d), %d on a message that does not name their path", total, want, misplaced)
	}

	// History reads back in input order, ids included.
	hist, err := s.LoadHistory(ctx, appended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != len(appendedEntries) {
		t.Fatalf("history has %d entries, want %d", len(hist), len(appendedEntries))
	}
	for i, e := range hist {
		if e.ID != ids[i] || string(e.Content) != string(appendedEntries[i].Content) {
			t.Fatalf("history entry %d = id %d %s; want id %d %s", i, e.ID, e.Content, ids[i], appendedEntries[i].Content)
		}
	}
}
