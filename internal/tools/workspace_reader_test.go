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
	"sync"
	"testing"
	"time"
)

// readNow adapts a reader for tests that read one window and do not hold
// it: it releases the lease at once.
func readNow(r WorkspaceFileReadFunc) func(context.Context, string, string, int64, int64, int64) ([]byte, error) {
	return func(ctx context.Context, path, sum string, offset, length, maxFileBytes int64) ([]byte, error) {
		windows, release, err := r(ctx, []WorkspaceFileRef{{Path: path, SHA256: sum, Offset: offset, Length: length}}, maxFileBytes)
		if err != nil {
			return nil, err
		}
		release()
		return windows[0], nil
	}
}

// readHeld reads one window and returns its release.
func readHeld(ctx context.Context, r WorkspaceFileReadFunc, path, sum string, offset, length, maxFileBytes int64) (func(), error) {
	_, release, err := r(ctx, []WorkspaceFileRef{{Path: path, SHA256: sum, Offset: offset, Length: length}}, maxFileBytes)
	return release, err
}

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
	reader := readNow(WorkspaceFileReader(sb))
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
	if _, err := readNow(WorkspaceFileReader(sb))(ctx, "payload", sum, 0, 1, 10); err == nil || !strings.Contains(err.Error(), "exceeding the 10-byte cap") {
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
	if _, err := readNow(WorkspaceFileReader(nil))(ctx, "payload", sum, 0, 1, 100); err == nil {
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
	reader := readNow(WorkspaceFileReader(sb))
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

// fakeSource serves files from a map. It counts reads and can block them on
// gate (closed = never block) so tests can hold reads in flight.
type fakeSource struct {
	mu       sync.Mutex
	files    map[string][]byte
	reads    map[string]int
	probes   int
	gate     chan struct{}
	started  chan string
	override func(path string) []byte // bytes a compromised helper returns
}

func newFakeSource(files map[string][]byte) *fakeSource {
	gate := make(chan struct{})
	close(gate)
	return &fakeSource{files: files, reads: map[string]int{}, gate: gate, started: make(chan string, 64)}
}

func (f *fakeSource) source() *workspaceFileSource {
	return &workspaceFileSource{
		size: func(_ context.Context, _, path string) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.probes++
			return int64(len(f.files[path])), nil
		},
		read: func(ctx context.Context, _, path string, maxBytes int64) ([]byte, error) {
			f.mu.Lock()
			f.reads[path]++
			gate, started := f.gate, f.started
			f.mu.Unlock()
			started <- path
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if f.override != nil {
				return f.override(path), nil
			}
			data := f.files[path]
			if int64(len(data)) > maxBytes {
				return nil, errors.New("too large")
			}
			return data, nil
		},
	}
}

func (f *fakeSource) readCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[path]
}

func testStore(budget int64) *workspaceFileStore {
	return newWorkspaceFileStore(budget, budget, 2, time.Minute, time.Now)
}

// TestWorkspaceFileReaderRejectsForgedHelperDigest simulates a compromised
// sandbox helper (a planted hashlib.py, say) that reports the approved hash
// while returning altered bytes. Fleet ignores anything the helper says about
// the bytes and hashes them itself, so nothing is returned.
func TestWorkspaceFileReaderRejectsForgedHelperDigest(t *testing.T) {
	approved := []byte("approved contents")
	src := newFakeSource(map[string][]byte{"f": approved})
	src.override = func(string) []byte { return []byte("altered contents!") } // helper would claim hexSum(approved)
	store := testStore(1 << 20)
	reader := readNow(newVerifiedWorkspaceReader(src.source(), store))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	for offset := int64(0); offset < 3; offset++ {
		if _, err := reader(ctx, "f", hexSum(approved), offset, 4, 1<<20); !errors.Is(err, ErrWorkspaceFileHashMismatch) {
			t.Fatalf("forged digest let altered bytes through: %v", err)
		}
	}
	if src.readCount("f") != 3 || store.order.Len() != 0 || store.reserved != 0 {
		t.Fatalf("a refused file was cached or its reservation leaked: %d reads, %d entries, %d reserved",
			src.readCount("f"), store.order.Len(), store.reserved)
	}
}

func TestVerifiedWorkspaceFileCache(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 400), "b": bytes.Repeat([]byte("b"), 400), "c": bytes.Repeat([]byte("c"), 400)}
	src := newFakeSource(files)
	now := time.Unix(0, 0)
	store := newWorkspaceFileStore(1<<20, 1000, 2, time.Minute, func() time.Time { return now })
	reader := readNow(newVerifiedWorkspaceReader(src.source(), store))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	read := func(r func(context.Context, string, string, int64, int64, int64) ([]byte, error), path string, offset int64) []byte {
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
	if src.readCount("a") != 1 {
		t.Fatalf("hit re-read the file: %d reads", src.readCount("a"))
	}
	// Miss: another reader (another run) never sees this run's entry.
	read(readNow(newVerifiedWorkspaceReader(src.source(), store)), "a", 0)
	if src.readCount("a") != 2 {
		t.Fatalf("cache shared across readers: %d reads", src.readCount("a"))
	}
	// Eviction: entry and byte bounds evict least-recently used first.
	read(reader, "b", 0)
	read(reader, "c", 0)
	if store.order.Len() > 2 || store.cached > 1000 {
		t.Fatalf("cache over bounds: %d entries, %d bytes", store.order.Len(), store.cached)
	}
	before := src.readCount("b")
	read(reader, "b", 0) // most recent two are b and c
	if src.readCount("b") != before {
		t.Fatal("recent entry was evicted")
	}
	before = src.readCount("a")
	read(reader, "a", 0)
	if src.readCount("a") != before+1 {
		t.Fatal("least recently used entry was not evicted")
	}
	// TTL: an expired entry is a miss and is re-verified.
	now = now.Add(2 * time.Minute)
	before = src.readCount("a")
	read(reader, "a", 0)
	if src.readCount("a") != before+1 {
		t.Fatal("expired entry served")
	}
	// A file larger than the cache's byte bound is never cached.
	files["big"] = bytes.Repeat([]byte("z"), 1001)
	read(reader, "big", 0)
	read(reader, "big", 0)
	if src.readCount("big") != 2 {
		t.Fatalf("over-budget file cached: %d reads", src.readCount("big"))
	}
	if store.reserved != 0 {
		t.Fatalf("reservations leaked: %d", store.reserved)
	}
}

// TestWorkspaceFileReaderConcurrentIdenticalMisses: concurrent references to
// the same file share one read.
func TestWorkspaceFileReaderConcurrentIdenticalMisses(t *testing.T) {
	body := bytes.Repeat([]byte("p"), 64<<10)
	src := newFakeSource(map[string][]byte{"f": body})
	src.gate = make(chan struct{})
	reader := readNow(newVerifiedWorkspaceReader(src.source(), testStore(8<<20)))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			window, err := reader(ctx, "f", hexSum(body), int64(i)*1024, 1024, 1<<20)
			if err == nil && !bytes.Equal(window, body[i*1024:(i+1)*1024]) {
				err = errors.New("wrong window")
			}
			errs <- err
		})
	}
	<-src.started
	time.Sleep(50 * time.Millisecond) // let the others queue on the flight
	close(src.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := src.readCount("f"); got != 1 {
		t.Fatalf("identical concurrent misses read %d times, want 1", got)
	}
}

// TestWorkspaceFileReaderBudget: concurrent misses on different large files
// never hold more than the budget (cached + in-flight reservations), waiting
// misses honor cancellation, and a file whose read could never fit fails at
// once instead of waiting forever.
func TestWorkspaceFileReaderBudget(t *testing.T) {
	const budget = 1 << 20
	files := map[string][]byte{}
	for i := range 6 {
		files[string(rune('a'+i))] = bytes.Repeat([]byte{byte(i)}, 150<<10) // a read reserves 2.5*150K + 192K: one fits at a time
	}
	files["huge"] = bytes.Repeat([]byte("h"), 400<<10) // read costs 2.5*400K + 192K > budget
	src := newFakeSource(files)
	store := testStore(budget)
	reader := readNow(newVerifiedWorkspaceReader(src.source(), store))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		name := string(rune('a' + i))
		wg.Go(func() {
			_, err := reader(ctx, name, hexSum(files[name]), 0, 10, 1<<20)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if store.peak > budget {
		t.Fatalf("peak reserved+cached %d exceeds budget %d", store.peak, budget)
	}
	if store.reserved != 0 || store.cached > budget {
		t.Fatalf("after settling: reserved %d, cached %d", store.reserved, store.cached)
	}

	// Oversize: fails fast, without reading.
	start := time.Now()
	if _, err := reader(ctx, "huge", hexSum(files["huge"]), 0, 10, 1<<20); !errors.Is(err, ErrWorkspaceFileBudget) {
		t.Fatalf("oversize read: %v", err)
	}
	if src.readCount("huge") != 0 || time.Since(start) > time.Second {
		t.Fatal("oversize read was attempted or waited")
	}

	// Cancellation while waiting: hold one read in flight, then cancel a
	// second miss that cannot be admitted beside it.
	src.mu.Lock()
	src.gate = make(chan struct{})
	src.started = make(chan string, 64) // drop the batch's start signals
	src.mu.Unlock()
	holder := readNow(newVerifiedWorkspaceReader(src.source(), store)) // fresh reader: a miss
	held := make(chan error, 1)
	go func() {
		_, err := holder(ctx, "a", hexSum(files["a"]), 0, 10, 1<<20)
		held <- err
	}()
	<-src.started // the holder's read is in flight
	waitCtx, cancel := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() {
		_, err := holder(waitCtx, "b", hexSum(files["b"]), 0, 10, 1<<20)
		waited <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if src.readCount("b") != 1 { // b's only read so far is from the batch above
		t.Fatal("second miss was admitted beside the first")
	}
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting miss ignored cancellation: %v", err)
	}
	src.mu.Lock()
	close(src.gate)
	src.mu.Unlock()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if store.peak > budget || store.reserved != 0 {
		t.Fatalf("budget broken: peak %d, reserved %d", store.peak, store.reserved)
	}
}

// TestWorkspaceFileLeases: a window stays charged — the file it is cut from
// plus its dispatch charge — from acquisition until release, on cache hits as
// well as misses; a pinned file is never evicted from under a lease; a lease
// that does not fit waits for a release; release is idempotent.
func TestWorkspaceFileLeases(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 100<<10), "b": bytes.Repeat([]byte("b"), 10<<10)}
	src := newFakeSource(files)
	wc := WorkspaceWindowCharge(10)
	// Room for "a" and two leases, not three; and for a's file and one lease
	// beside b's miss reservation (max(4*10K+128K, 10K+wc)).
	budget := 100<<10 + 2*wc + wc/2
	store := newWorkspaceFileStore(budget, budget, 1, time.Minute, time.Now)
	reader := newVerifiedWorkspaceReader(src.source(), store)
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	charged := func() int64 {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.cached + store.reserved
	}
	acquire := func(c context.Context, path string) (func(), error) {
		return readHeld(c, reader, path, hexSum(files[path]), 0, 10, 1<<20)
	}

	releaseMiss, err := acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := charged(), int64(100<<10)+wc; got != want {
		t.Fatalf("miss lease charged %d, want %d", got, want)
	}
	releaseHit, err := acquire(ctx, "a") // cache hit
	if err != nil {
		t.Fatal(err)
	}
	if got, want := charged(), int64(100<<10)+2*wc; got != want || src.readCount("a") != 1 {
		t.Fatalf("hit lease charged %d, want %d (reads %d)", got, want, src.readCount("a"))
	}
	// A third lease does not fit: it waits, and honors cancellation.
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if _, err := acquire(waitCtx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("over-budget lease: %v", err)
	}
	cancel()
	third := make(chan error, 1)
	go func() {
		release, err := acquire(ctx, "a")
		if err == nil {
			release()
		}
		third <- err
	}()
	time.Sleep(20 * time.Millisecond)
	releaseHit()
	releaseHit() // idempotent
	if err := <-third; err != nil {
		t.Fatalf("waiting lease not admitted after a release: %v", err)
	}
	// maxEntries is 1, but "a" is pinned: reading "b" must not evict it.
	releaseB, err := acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	entries := store.order.Len()
	store.mu.Unlock()
	if entries != 2 {
		t.Fatalf("pinned file evicted: %d entries", entries)
	}
	releaseMiss()
	releaseB()
	if got := charged(); got > int64(100<<10) || store.reserved != 0 {
		t.Fatalf("after releasing every lease %d bytes stay charged; want at most one cached file", got)
	}
	if store.peak > budget {
		t.Fatalf("peak %d exceeds budget %d", store.peak, budget)
	}
}

// TestWorkspaceFileMultiReferenceAdmission: all references of one call are
// admitted in one step. Two 64 MiB files with 8 MiB windows — the shape that
// deadlocked when each reference was admitted while holding the previous
// one — fit together under the real budget. Calls that each need most of the
// budget run one after another instead of deadlocking on each other, a call
// that could never fit fails at once, and a file named twice is read once.
func TestWorkspaceFileMultiReferenceAdmission(t *testing.T) {
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	two := func(a, b string, files map[string][]byte, n int64) []WorkspaceFileRef {
		return []WorkspaceFileRef{{Path: a, SHA256: hexSum(files[a]), Length: n}, {Path: b, SHA256: hexSum(files[b]), Offset: 1, Length: n}}
	}

	big := map[string][]byte{"x": bytes.Repeat([]byte("x"), 64<<20), "y": bytes.Repeat([]byte("y"), 64<<20)}
	store := newWorkspaceFileStore(workspaceFileBudgetBytes, workspaceFileCacheMaxBytes, workspaceFileCacheMaxEntries, time.Minute, time.Now)
	reader := newVerifiedWorkspaceReader(newFakeSource(big).source(), store)
	windows, release, err := reader(ctx, two("x", "y", big, 8<<20), 64<<20)
	if err != nil {
		t.Fatalf("two 64 MiB files with 8 MiB windows: %v", err)
	}
	if !bytes.Equal(windows[0], big["x"][:8<<20]) || !bytes.Equal(windows[1], big["y"][1:1+8<<20]) {
		t.Fatal("wrong windows")
	}
	if store.peak > store.budget {
		t.Fatalf("peak %d exceeds budget %d", store.peak, store.budget)
	}
	release()

	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 100<<10), "b": bytes.Repeat([]byte("b"), 100<<10)}
	src := newFakeSource(files)
	// One call needs both files plus two leases (and one read's excess);
	// two such calls do not fit at once.
	one := 2*(100<<10+WorkspaceWindowCharge(10)) + (workspaceFileMissCost(100<<10) - (100<<10 + WorkspaceWindowCharge(10)))
	small := newWorkspaceFileStore(one+one/2, one+one/2, 2, time.Minute, time.Now)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		r := newVerifiedWorkspaceReader(src.source(), small) // separate runs: separate keys
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, release, err := r(cctx, two(order[0], order[1], files, 10), 1<<20)
			if err == nil {
				time.Sleep(20 * time.Millisecond)
				release()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent multi-reference calls: %v", err)
		}
	}
	if small.peak > small.budget || small.reserved != 0 {
		t.Fatalf("peak %d (budget %d), reserved %d after release", small.peak, small.budget, small.reserved)
	}

	// Never fits: fails fast, reads nothing.
	tiny := newWorkspaceFileStore(one-1, one, 2, time.Minute, time.Now)
	before := src.readCount("a") + src.readCount("b")
	start := time.Now()
	if _, _, err := newVerifiedWorkspaceReader(src.source(), tiny)(ctx, two("a", "b", files, 10), 1<<20); !errors.Is(err, ErrWorkspaceFileBudget) {
		t.Fatalf("call over the whole budget: %v", err)
	}
	if src.readCount("a")+src.readCount("b") != before || time.Since(start) > time.Second {
		t.Fatal("an impossible call read or waited")
	}

	// The same file twice in one call: one read, both windows, two pins.
	r := newVerifiedWorkspaceReader(src.source(), small)
	before = src.readCount("a")
	windows, release, err = r(ctx, two("a", "a", files, 10), 1<<20)
	if err != nil || src.readCount("a") != before+1 || !bytes.Equal(windows[1], files["a"][1:11]) {
		t.Fatalf("same file twice: %v (reads %d)", err, src.readCount("a")-before)
	}
	release()
	release() // idempotent
	if small.reserved != 0 {
		t.Fatalf("reserved %d after release", small.reserved)
	}
}

// TestWorkspaceFileReadPanicReleases: a panic while reading leaves nothing
// reserved, pinned, or in flight, and a later call reads normally.
func TestWorkspaceFileReadPanicReleases(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 1000)}
	src := newFakeSource(files)
	panicking := src.source()
	panicking.read = func(context.Context, string, string, int64) ([]byte, error) { panic("read blew up") }
	store := testStore(4 << 20)
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_, _, _ = newVerifiedWorkspaceReader(panicking, store)(ctx, []WorkspaceFileRef{{Path: "a", SHA256: hexSum(files["a"]), Length: 10}}, 1<<20)
	}()
	if store.reserved != 0 || len(store.flights) != 0 {
		t.Fatalf("panic leaked: reserved %d, flights %d", store.reserved, len(store.flights))
	}
	if _, err := readNow(newVerifiedWorkspaceReader(src.source(), store))(ctx, "a", hexSum(files["a"]), 0, 10, 1<<20); err != nil {
		t.Fatal(err)
	}
}

// TestVerifiedWorkspaceFileCacheHitIsAUse: a cache hit moves the file to the
// front of the LRU, so after A, B, A the next insert evicts B, not the file an
// interleaved upload is still sending.
func TestVerifiedWorkspaceFileCacheHitIsAUse(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 100), "b": bytes.Repeat([]byte("b"), 100), "c": bytes.Repeat([]byte("c"), 100)}
	src := newFakeSource(files)
	store := newWorkspaceFileStore(1<<20, 1<<20, 2, time.Minute, time.Now)
	reader := readNow(newVerifiedWorkspaceReader(src.source(), store))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	read := func(path string) {
		t.Helper()
		if _, err := reader(ctx, path, hexSum(files[path]), 0, 10, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	read("a")
	read("b")
	read("a") // a hit: a is now the most recently used
	read("c") // evicts b
	read("a")
	if got := src.readCount("a"); got != 1 {
		t.Fatalf("recently used file was evicted and re-read: %d reads", got)
	}
	read("b")
	if got := src.readCount("b"); got != 2 {
		t.Fatalf("least recently used file was not the one evicted: %d reads of b", got)
	}
}

// TestWorkspaceFileReaderFailedFlightIsShared: when the shared read of a key
// fails on its own terms (here a hash mismatch), every waiter gets that
// failure instead of repeating the whole-file read in turn.
func TestWorkspaceFileReaderFailedFlightIsShared(t *testing.T) {
	approved := bytes.Repeat([]byte("q"), 32<<10)
	src := newFakeSource(map[string][]byte{"f": approved})
	src.override = func(string) []byte { return bytes.Repeat([]byte("x"), 32<<10) }
	src.gate = make(chan struct{})
	reader := readNow(newVerifiedWorkspaceReader(src.source(), testStore(8<<20)))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := reader(ctx, "f", hexSum(approved), 0, 1024, 1<<20)
			errs <- err
		})
	}
	<-src.started
	time.Sleep(50 * time.Millisecond) // let the others queue on the flight
	close(src.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrWorkspaceFileHashMismatch) {
			t.Fatalf("waiter did not get the shared failure: %v", err)
		}
	}
	if got := src.readCount("f"); got != 1 {
		t.Fatalf("a failed read was repeated by waiters: %d reads, want 1", got)
	}
}

// TestWorkspaceFileHitFootprintFailsFast: when every reference is a cache
// hit, the impossibility check counts the hit file the call must keep pinned,
// so a call that can never fit fails at once instead of waiting for capacity
// that cannot appear.
func TestWorkspaceFileHitFootprintFailsFast(t *testing.T) {
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	files := map[string][]byte{"x": bytes.Repeat([]byte("x"), 64<<20)}
	src := newFakeSource(files)
	store := newWorkspaceFileStore(workspaceFileBudgetBytes, workspaceFileCacheMaxBytes, workspaceFileCacheMaxEntries, time.Minute, time.Now)
	reader := newVerifiedWorkspaceReader(src.source(), store)
	refs := func(n int) []WorkspaceFileRef {
		out := make([]WorkspaceFileRef, n)
		for i := range out {
			out[i] = WorkspaceFileRef{Path: "x", SHA256: hexSum(files["x"]), Offset: int64(i) << 23, Length: 8 << 20}
		}
		return out
	}
	// Cache the file, then a call that fits alongside it is still admitted.
	_, release, err := reader(ctx, refs(1), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, release, err = reader(ctx, refs(2), 64<<20); err != nil {
		t.Fatalf("two windows of a cached 64 MiB file should fit: %v", err)
	}
	release()
	// Three 8 MiB windows plus the pinned 64 MiB file exceed the budget.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := reader(cctx, refs(3), 64<<20); !errors.Is(err, ErrWorkspaceFileBudget) {
		t.Fatalf("an impossible all-hit call: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("an impossible all-hit call waited instead of failing fast")
	}
	if store.reserved != 0 {
		t.Fatalf("reserved %d after the refused call", store.reserved)
	}
	if got := src.readCount("x"); got != 1 {
		t.Fatalf("cached file re-read: %d reads", got)
	}
}

// TestWorkspaceFileReaderAbandonedFlightIsRetried: a call that reserves two
// misses and fails on the first leaves the second unread; a waiter on that
// second file must read it itself rather than inherit the first file's error.
func TestWorkspaceFileReaderAbandonedFlightIsRetried(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 32<<10), "b": bytes.Repeat([]byte("b"), 32<<10)}
	src := newFakeSource(files)
	src.override = func(path string) []byte {
		if path == "a" {
			return bytes.Repeat([]byte("x"), 32<<10)
		}
		return files[path]
	}
	src.gate = make(chan struct{})
	reader := newVerifiedWorkspaceReader(src.source(), testStore(8<<20))
	ctx := WithForcedWorkingDir(context.Background(), "/ws")
	first := make(chan error, 1)
	go func() {
		_, release, err := reader(ctx, []WorkspaceFileRef{
			{Path: "a", SHA256: hexSum(files["a"]), Length: 1024},
			{Path: "b", SHA256: hexSum(files["b"]), Length: 1024},
		}, 1<<20)
		if err == nil {
			release()
		}
		first <- err
	}()
	if got := <-src.started; got != "a" {
		t.Fatalf("first read was %q, want a", got)
	}
	second := make(chan error, 1)
	go func() {
		_, release, err := reader(ctx, []WorkspaceFileRef{{Path: "b", SHA256: hexSum(files["b"]), Length: 1024}}, 1<<20)
		if err == nil {
			release()
		}
		second <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the second call queue on b's flight
	close(src.gate)
	if err := <-first; !errors.Is(err, ErrWorkspaceFileHashMismatch) {
		t.Fatalf("first call: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("a waiter on an unread file inherited another file's failure: %v", err)
	}
}
