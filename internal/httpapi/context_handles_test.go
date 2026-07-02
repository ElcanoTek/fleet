package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandContextHandles_File(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("Q3 revenue up 12%"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, notices := expandContextHandles(context.Background(), `summarize @file:"report.txt" please`, dir)
	if len(notices) != 0 {
		t.Fatalf("unexpected notices: %v", notices)
	}
	if len(blocks) != 1 || !strings.Contains(blocks[0], "Q3 revenue up 12%") {
		t.Fatalf("expected file content block, got %v", blocks)
	}
	if !strings.Contains(blocks[0], `report.txt`) {
		t.Errorf("block should name the file: %q", blocks[0])
	}
}

func TestExpandContextHandles_FileTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	// A path escaping the workspace must be rejected by SafeWorkspaceJoin.
	blocks, notices := expandContextHandles(context.Background(), `@file:"../../etc/passwd"`, dir)
	if len(blocks) != 0 {
		t.Errorf("traversal must not produce a block: %v", blocks)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "not allowed") {
		t.Errorf("expected a path-not-allowed notice, got %v", notices)
	}
}

func TestExpandContextHandles_FileMissing(t *testing.T) {
	dir := t.TempDir()
	blocks, notices := expandContextHandles(context.Background(), `@file:"nope.txt"`, dir)
	if len(blocks) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "not found") {
		t.Errorf("expected not-found notice, got blocks=%v notices=%v", blocks, notices)
	}
}

func TestExpandContextHandles_FileNoWorkspace(t *testing.T) {
	blocks, notices := expandContextHandles(context.Background(), `@file:"report.txt"`, "")
	if len(blocks) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "unavailable") {
		t.Errorf("empty workspace should disable @file with a notice, got blocks=%v notices=%v", blocks, notices)
	}
}

func TestExpandContextHandles_FileTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxContextFileBytes+1000)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, _ := expandContextHandles(context.Background(), `@file:"big.txt"`, dir)
	if len(blocks) != 1 || !strings.Contains(blocks[0], "_(truncated)_") {
		t.Fatalf("oversized file should be truncated with a marker")
	}
}

func TestExpandContextHandles_URLSSRFNotice(t *testing.T) {
	// A loopback httptest URL is refused by the SSRF-guarded fetcher → notice.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "internal")
	}))
	defer srv.Close()

	blocks, notices := expandContextHandles(context.Background(), "check @url:"+srv.URL+" now", "")
	if len(blocks) != 0 {
		t.Errorf("SSRF-blocked URL must not produce a block: %v", blocks)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "could not fetch") {
		t.Errorf("expected a fetch-failed notice, got %v", notices)
	}
}

func TestExpandContextHandles_None(t *testing.T) {
	blocks, notices := expandContextHandles(context.Background(), "just a normal message with an email a@b.com", "/tmp")
	if blocks != nil || notices != nil {
		t.Errorf("no handles should expand to nothing, got blocks=%v notices=%v", blocks, notices)
	}
}

func TestExpandContextHandles_Cap(t *testing.T) {
	dir := t.TempDir()
	var msg strings.Builder
	for i := 0; i < maxContextHandles+3; i++ {
		fmt.Fprintf(&msg, ` @file:"f%d.txt"`, i)
	}
	_, notices := expandContextHandles(context.Background(), msg.String(), dir)
	found := false
	for _, n := range notices {
		if strings.Contains(n, "ignored (max") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an over-cap notice, got %v", notices)
	}
}

func TestAppendContextHandleBlocks(t *testing.T) {
	// No-op when nothing to append.
	if got := appendContextHandleBlocks("hi", nil, nil); got != "hi" {
		t.Errorf("expected no-op, got %q", got)
	}
	got := appendContextHandleBlocks("base", []string{"BLOCK"}, []string{"a notice"})
	if !strings.Contains(got, "base") || !strings.Contains(got, "BLOCK") || !strings.Contains(got, "a notice") {
		t.Errorf("append dropped content: %q", got)
	}
}
