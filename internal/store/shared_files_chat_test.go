package store

// Load-bearing assertions for chat-written shared files (migration 0205):
// create needs a description and records the writer; append starts a new line
// and never loses a concurrent writer's text; replace needs the sha of the
// version read (compare-and-swap) and keeps the displaced text for one undo;
// admin uploads stay read-only to chats; the per-file and library caps refuse
// before anything is written; and one folder holds at most one index file.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func chatWrite(mode, folder, name, text string) ChatSharedFileWrite {
	return ChatSharedFileWrite{
		Folder: folder, Name: name, Mode: mode, Text: text,
		Actor: "trader@client.com", ConversationID: "conv-1",
		MaxFileBytes: 1 << 20,
	}
}

func TestChatSharedFileCreateAppendReplaceUndo(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	// Create without a description is refused: the description is how every
	// other chat learns what the file is.
	w := chatWrite(SharedFileWriteCreate, "optimizations", "log.csv", "a,b\n")
	w.NewID = "c1"
	if _, err := st.WriteChatSharedFile(ctx, w); !errors.Is(err, ErrSharedFileDescriptionRequired) {
		t.Fatalf("create without description err = %v", err)
	}
	w.Description = strp("  Optimization log  ")
	res, err := st.WriteChatSharedFile(ctx, w)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Created || res.File.Origin != SharedFileOriginChat || res.File.Description != "Optimization log" ||
		res.File.UpdatedBy != "trader@client.com" || res.File.UploadedBy != "trader@client.com" ||
		res.File.UpdatedConversationID != "conv-1" || res.File.SizeBytes != 4 || res.File.SHA256 != sha256Hex("a,b\n") {
		t.Fatalf("created row = %+v", res.File)
	}
	if res.File.HasPrevious {
		t.Fatal("a new file has no previous version")
	}

	// Creating it again is refused (append or replace instead).
	w.NewID = "c2"
	if _, err := st.WriteChatSharedFile(ctx, w); !errors.Is(err, ErrSharedFileExists) {
		t.Fatalf("second create err = %v", err)
	}

	// Append adds text; a file without a trailing newline gets one first.
	a := chatWrite(SharedFileWriteAppend, "optimizations", "log.csv", "1,2")
	if _, err := st.WriteChatSharedFile(ctx, a); err != nil {
		t.Fatalf("append: %v", err)
	}
	a.Text = "3,4\n"
	a.ConversationID = "conv-2"
	res, err = st.WriteChatSharedFile(ctx, a)
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	content, sha, ok, err := st.SharedFileContent(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("content: ok=%v err=%v", ok, err)
	}
	if content != "a,b\n1,2\n3,4\n" {
		t.Fatalf("content after appends = %q", content)
	}
	if sha != res.File.SHA256 || res.File.UpdatedConversationID != "conv-2" {
		t.Fatalf("append row = %+v sha=%s", res.File, sha)
	}

	// Replace without expected_sha256 is refused; with a stale one it reports
	// the current sha so the writer can re-read and merge.
	r := chatWrite(SharedFileWriteReplace, "optimizations", "log.csv", "fresh\n")
	if _, err := st.WriteChatSharedFile(ctx, r); !errors.Is(err, ErrSharedFileExpectedHashRequired) {
		t.Fatalf("replace without hash err = %v", err)
	}
	r.ExpectedSHA256 = sha256Hex("something else")
	var stale *SharedFileStaleError
	if _, err := st.WriteChatSharedFile(ctx, r); !errors.As(err, &stale) || stale.Current != sha {
		t.Fatalf("stale replace err = %v", err)
	}
	r.ExpectedSHA256 = strings.ToUpper(sha) // hashes compare case-insensitively
	res, err = st.WriteChatSharedFile(ctx, r)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if res.PreviousSHA256 != sha || !res.File.HasPrevious {
		t.Fatalf("replace result = %+v", res)
	}

	// An append after a replace keeps the pre-replace text restorable.
	if _, err := st.WriteChatSharedFile(ctx, chatWrite(SharedFileWriteAppend, "optimizations", "log.csv", "more\n")); err != nil {
		t.Fatalf("append after replace: %v", err)
	}

	// Undo restores the displaced version, and undo again swaps back.
	if _, err := st.WriteChatSharedFile(ctx, chatWrite(SharedFileWriteUndo, "optimizations", "log.csv", "")); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got, _, _, _ := st.SharedFileContent(ctx, "c1"); got != "a,b\n1,2\n3,4\n" {
		t.Fatalf("after undo = %q", got)
	}
	if _, err := st.WriteChatSharedFile(ctx, chatWrite(SharedFileWriteUndo, "optimizations", "log.csv", "")); err != nil {
		t.Fatalf("redo: %v", err)
	}
	if got, _, _, _ := st.SharedFileContent(ctx, "c1"); got != "fresh\nmore\n" {
		t.Fatalf("after second undo = %q", got)
	}

	// Replace and undo on a missing file are not-found, not creates.
	if _, err := st.WriteChatSharedFile(ctx, chatWrite(SharedFileWriteUndo, "optimizations", "nope.csv", "")); !errors.Is(err, ErrSharedFileNotFound) {
		t.Fatalf("undo missing err = %v", err)
	}
	// Undo with nothing displaced.
	n := chatWrite(SharedFileWriteAppend, "", "notes.md", "x")
	n.NewID, n.Description = "c3", strp("notes")
	if _, err := st.WriteChatSharedFile(ctx, n); err != nil {
		t.Fatalf("append-create: %v", err)
	}
	if _, err := st.WriteChatSharedFile(ctx, chatWrite(SharedFileWriteUndo, "", "notes.md", "")); !errors.Is(err, ErrSharedFileNoPrevious) {
		t.Fatalf("undo without previous err = %v", err)
	}
}

func TestChatSharedFileAdminUploadsStayReadOnly(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "adm", Name: "prices.csv", Folder: "ref", SizeBytes: 3, SHA256: "x"}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	got, err := st.GetSharedFile(ctx, "adm")
	if err != nil || got.Origin != SharedFileOriginAdmin {
		t.Fatalf("upload origin = %q err=%v", got.Origin, err)
	}
	for _, mode := range []string{SharedFileWriteAppend, SharedFileWriteReplace, SharedFileWriteUndo} {
		w := chatWrite(mode, "ref", "prices.csv", "y")
		w.ExpectedSHA256 = "x"
		if _, err := st.WriteChatSharedFile(ctx, w); !errors.Is(err, ErrSharedFileAdminOwned) {
			t.Fatalf("%s on admin file err = %v", mode, err)
		}
	}
	if _, _, ok, err := st.SharedFileContent(ctx, "adm"); ok || err != nil {
		t.Fatalf("admin content ok=%v err=%v", ok, err)
	}
	// A chat file cannot shadow a folder name either.
	w := chatWrite(SharedFileWriteCreate, "", "ref", "z")
	w.NewID, w.Description = "c9", strp("d")
	if _, err := st.WriteChatSharedFile(ctx, w); !errors.Is(err, ErrSharedFileNameIsFolder) {
		t.Fatalf("file named like a folder err = %v", err)
	}
}

func TestChatSharedFileCaps(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	w := chatWrite(SharedFileWriteCreate, "", "big.txt", strings.Repeat("x", 11))
	w.NewID, w.Description, w.MaxFileBytes = "b1", strp("big"), 10
	var tooBig *SharedFileTooLargeError
	if _, err := st.WriteChatSharedFile(ctx, w); !errors.As(err, &tooBig) || tooBig.Size != 11 || tooBig.Limit != 10 {
		t.Fatalf("over per-file cap err = %v", err)
	}
	w.Text = "0123456789"
	if _, err := st.WriteChatSharedFile(ctx, w); err != nil {
		t.Fatalf("at cap: %v", err)
	}
	a := chatWrite(SharedFileWriteAppend, "", "big.txt", "!")
	a.MaxFileBytes = 10
	if _, err := st.WriteChatSharedFile(ctx, a); !errors.As(err, &tooBig) {
		t.Fatalf("append over cap err = %v", err)
	}

	// Library total: an upload of 90 bytes plus a 10-byte chat file fill a
	// 100-byte library; one more byte is refused, a shrinking replace is not.
	if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "u1", Name: "u.bin", SizeBytes: 90}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	a = chatWrite(SharedFileWriteAppend, "", "big.txt", "!")
	a.MaxTotalBytes = 100
	var full *SharedFileLibraryFullError
	if _, err := st.WriteChatSharedFile(ctx, a); !errors.As(err, &full) || full.Limit != 100 {
		t.Fatalf("library full err = %v", err)
	}
	r := chatWrite(SharedFileWriteReplace, "", "big.txt", "small")
	r.ExpectedSHA256, r.MaxTotalBytes = sha256Hex("0123456789"), 100
	if _, err := st.WriteChatSharedFile(ctx, r); err != nil {
		t.Fatalf("shrinking replace in a full library: %v", err)
	}
	// Nothing was written by the refused calls.
	if got, _, _, _ := st.SharedFileContent(ctx, "b1"); got != "small" {
		t.Fatalf("content = %q", got)
	}
}

func TestChatSharedFileFolderIndex(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()
	mk := func(id, name string, index bool) SharedFile {
		t.Helper()
		w := chatWrite(SharedFileWriteCreate, "opt", name, "x")
		w.NewID, w.Description, w.FolderIndex = id, strp(name), boolp(index)
		res, err := st.WriteChatSharedFile(ctx, w)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return res.File
	}
	if f := mk("i1", "Registry.yaml", true); !f.FolderIndex {
		t.Fatal("index not set on create")
	}
	mk("i2", "a.csv", false)
	// Marking another file moves the index; the old one is cleared.
	a := chatWrite(SharedFileWriteAppend, "opt", "a.csv", "y")
	a.FolderIndex = boolp(true)
	if res, err := st.WriteChatSharedFile(ctx, a); err != nil || !res.File.FolderIndex {
		t.Fatalf("move index: %+v %v", res.File, err)
	}
	old, _ := st.GetSharedFile(ctx, "i1")
	if old.FolderIndex {
		t.Fatal("old index still set")
	}
	// A new file marked as index in the same folder also moves it.
	if f := mk("i3", "README.md", true); !f.FolderIndex {
		t.Fatal("index not set on second create")
	}
	if got, _ := st.GetSharedFile(ctx, "i2"); got.FolderIndex {
		t.Fatal("index not moved off a.csv")
	}
	// Description changes only when a non-empty one is passed.
	d := chatWrite(SharedFileWriteAppend, "opt", "README.md", "z")
	d.Description = strp("   ")
	if res, _ := st.WriteChatSharedFile(ctx, d); res.File.Description != "README.md" {
		t.Fatalf("blank description replaced the old one: %q", res.File.Description)
	}
	d.Description = strp("Start here")
	if res, _ := st.WriteChatSharedFile(ctx, d); res.File.Description != "Start here" {
		t.Fatalf("description not updated: %q", res.File.Description)
	}
}

// Concurrent appends from many writers all land: the row lock serializes them
// in the database, so no writer's line is lost to a read-modify-write race.
func TestChatSharedFileConcurrentAppends(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := chatWrite(SharedFileWriteAppend, "opt", "log.csv", fmt.Sprintf("row-%02d\n", i))
			w.NewID, w.Description = fmt.Sprintf("n%02d", i), strp("log")
			// The first writers race to create the file; a loser retries
			// once, exactly as the tool handler does.
			_, err := st.WriteChatSharedFile(ctx, w)
			if errors.Is(err, ErrSharedFileWriteConflict) {
				_, err = st.WriteChatSharedFile(ctx, w)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	files, err := st.ListSharedFiles(ctx)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %d err=%v", len(files), err)
	}
	content, _, _, _ := st.SharedFileContent(ctx, files[0].ID)
	for i := 0; i < writers; i++ {
		if !strings.Contains(content, fmt.Sprintf("row-%02d\n", i)) {
			t.Fatalf("row %d lost; content:\n%s", i, content)
		}
	}
	if got := strings.Count(content, "\n"); got != writers {
		t.Fatalf("lines = %d, want %d", got, writers)
	}
}
