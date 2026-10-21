//go:build fleet_host_executor

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestWorkspaceFileReaderConfinement(t *testing.T) {
	root := t.TempDir()
	sb := fsTestSandbox(t)
	if err := sb.BindFileOpRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ctx := WithForcedWorkingDir(context.Background(), root)
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("exact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := hexSum([]byte("exact bytes"))
	reader := WorkspaceFileReader(sb)
	window, err := reader(ctx, "payload", sum, 0, 11, 100)
	if err != nil || string(window) != "exact bytes" {
		t.Fatalf("read failed: %q %v", window, err)
	}
	window, err = reader(ctx, "payload", sum, 6, 5, 100)
	if err != nil || string(window) != "bytes" {
		t.Fatalf("window read failed: %q %v", window, err)
	}
	if _, err := reader(ctx, "payload", strings.Repeat("0", 64), 0, 1, 100); !errors.Is(err, ErrWorkspaceFileHashMismatch) {
		t.Fatalf("wrong hash accepted: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	outsideSum := hexSum([]byte("outside"))
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "escape", "missing", ""} {
		if _, err := reader(ctx, path, outsideSum, 0, 1, 100); err == nil {
			t.Fatalf("read permitted for %q", path)
		}
	}
	// The cap is on the whole file, so even a 1-byte window of it is refused.
	if _, err := WorkspaceFileReader(sb)(ctx, "payload", sum, 0, 1, 10); err == nil || !strings.Contains(err.Error(), "exceeding the 10-byte cap") {
		t.Fatalf("oversized file accepted: %v", err)
	}
	for _, r := range [][3]int64{{-1, 1, 100}, {0, 0, 100}, {0, 101, 100}, {100, 1, 100}, {0, 1, 0}} {
		if _, err := reader(ctx, "payload", sum, r[0], r[1], r[2]); err == nil {
			t.Fatalf("invalid range %v accepted", r)
		}
	}
	if _, err := reader(context.Background(), "payload", sum, 0, 1, 100); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := WorkspaceFileReader(nil)(ctx, "payload", sum, 0, 1, 100); err == nil {
		t.Fatal("nil sandbox accepted")
	}
}

// TestWorkspaceFileReaderChunkedLargeFile: a 3 MiB file (over the old 2 MiB
// cap) sent in 1 MiB windows through the real sandbox comes back byte-exact,
// and later windows come from the verified bytes even after the file changes
// on disk — the dispatched bytes are always the hashed version.
func TestWorkspaceFileReaderChunkedLargeFile(t *testing.T) {
	root := t.TempDir()
	sb := fsTestSandbox(t)
	if err := sb.BindFileOpRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ctx := WithForcedWorkingDir(context.Background(), root)
	body := make([]byte, 3<<20)
	for i := range body {
		body[i] = byte(i*31 + i>>13)
	}
	path := filepath.Join(root, "site.zip")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	reader := WorkspaceFileReader(sb)
	var got []byte
	for offset := int64(0); offset < int64(len(body)); offset += 1 << 20 {
		window, err := reader(ctx, "site.zip", hexSum(body), offset, 1<<20, 64<<20)
		if err != nil {
			t.Fatalf("window at %d: %v", offset, err)
		}
		got = append(got, window...)
		if offset == 0 {
			// Rewriting the file mid-upload cannot change what is sent.
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatal("chunked transfer was not byte-exact")
	}
}

// TestWorkspaceFileReaderRejectsForgedHelperDigest simulates a compromised
// sandbox helper (a planted hashlib.py, say) that reports the approved hash
// while returning altered bytes. Fleet ignores anything the helper says about
// the bytes and hashes them itself, so nothing is returned.
func TestWorkspaceFileReaderRejectsForgedHelperDigest(t *testing.T) {
	approved := []byte("approved contents")
	reads := 0
	raw := func(context.Context, string, string, int64) ([]byte, error) {
		reads++
		return []byte("altered contents!"), nil // helper would claim hexSum(approved)
	}
	reader := newVerifiedWorkspaceReader(raw, newVerifiedFileCache(1<<20, 2, time.Minute, time.Now))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	for offset := int64(0); offset < 3; offset++ {
		if _, err := reader(ctx, "f", hexSum(approved), offset, 4, 1<<20); !errors.Is(err, ErrWorkspaceFileHashMismatch) {
			t.Fatalf("forged digest let altered bytes through: %v", err)
		}
	}
	if reads != 3 {
		t.Fatalf("a refused file was cached: %d reads", reads)
	}
}

func TestVerifiedWorkspaceFileCache(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 400), "b": bytes.Repeat([]byte("b"), 400), "c": bytes.Repeat([]byte("c"), 400)}
	reads := map[string]int{}
	raw := func(_ context.Context, _ string, path string, _ int64) ([]byte, error) {
		reads[path]++
		return files[path], nil
	}
	now := time.Unix(0, 0)
	cache := newVerifiedFileCache(1000, 2, time.Minute, func() time.Time { return now })
	reader := newVerifiedWorkspaceReader(raw, cache)
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	read := func(r WorkspaceFileReadFunc, path string, offset int64) []byte {
		t.Helper()
		window, err := r(ctx, path, hexSum(files[path]), offset, 100, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return window
	}

	// Hit: four windows of one reference, one read.
	for offset := int64(0); offset < 400; offset += 100 {
		if !bytes.Equal(read(reader, "a", offset), files["a"][offset:offset+100]) {
			t.Fatal("window not sliced from verified bytes")
		}
	}
	if reads["a"] != 1 {
		t.Fatalf("hit re-read the file: %d reads", reads["a"])
	}
	// Miss: another reader (another run) never sees this run's entry.
	read(newVerifiedWorkspaceReader(raw, cache), "a", 0)
	if reads["a"] != 2 {
		t.Fatalf("cache shared across readers: %d reads", reads["a"])
	}
	// Eviction: entry and byte bounds evict least-recently used first.
	read(reader, "b", 0)
	read(reader, "c", 0)
	if cache.order.Len() > 2 || cache.bytes > 1000 {
		t.Fatalf("cache over bounds: %d entries, %d bytes", cache.order.Len(), cache.bytes)
	}
	before := reads["b"]
	read(reader, "b", 0) // most recent two are b and c
	if reads["b"] != before {
		t.Fatal("recent entry was evicted")
	}
	before = reads["a"]
	read(reader, "a", 0)
	if reads["a"] != before+1 {
		t.Fatal("least recently used entry was not evicted")
	}
	// TTL: an expired entry is a miss and is re-verified.
	now = now.Add(2 * time.Minute)
	before = reads["a"]
	read(reader, "a", 0)
	if reads["a"] != before+1 {
		t.Fatal("expired entry served")
	}
	// A file larger than the whole budget is never cached.
	files["big"] = bytes.Repeat([]byte("z"), 1001)
	read(reader, "big", 0)
	read(reader, "big", 0)
	if reads["big"] != 2 {
		t.Fatalf("over-budget file cached: %d reads", reads["big"])
	}
}
