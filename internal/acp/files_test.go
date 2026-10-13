package acp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
)

// fakeReader answers fs/read_text_file the way a client does, from files
// (path → text; a path not in it is refused), and records each request.
// block, when set, holds every read until it is closed or the read's
// context ends.
type fakeReader struct {
	mu    sync.Mutex
	files map[string]string
	errs  map[string]error
	reqs  []acpsdk.ReadTextFileRequest
	block chan struct{}
}

func (f *fakeReader) ReadTextFile(ctx context.Context, p acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, p)
	text, ok := f.files[p.Path]
	err := f.errs[p.Path]
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return acpsdk.ReadTextFileResponse{}, ctx.Err()
		}
	}
	if err != nil {
		return acpsdk.ReadTextFileResponse{}, err
	}
	if !ok {
		return acpsdk.ReadTextFileResponse{}, &acpsdk.RequestError{Code: -32002, Message: "no such file"}
	}
	return acpsdk.ReadTextFileResponse{Content: text}, nil
}

// asked is the paths read so far, in order.
func (f *fakeReader) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		paths = append(paths, r.Path)
	}
	return paths
}

// readingAgent is an Agent whose client answers reads from r.
func readingAgent(r *fakeReader) *Agent {
	a := NewAgent(nil, nil, "", 0, "test")
	a.files = r
	a.clientReads.Store(true)
	return a
}

func fileURI(path string) string { return "file://" + path }

// sizedLink is a link to uri whose client gives the file's size.
func sizedLink(name, uri string, size int) acpsdk.ContentBlock {
	b := acpsdk.ResourceLinkBlock(name, uri)
	b.ResourceLink.Size = &size
	return b
}

// submit is what a prompt of blocks submits to fleet through a.
func submit(t *testing.T, a *Agent, blocks ...acpsdk.ContentBlock) string {
	t.Helper()
	message, err := promptText(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return a.withLinkedFiles(context.Background(), "s1", blocks, message)
}

// rows is n lines of width bytes each, line break included.
func rows(n, width int) string { return strings.Repeat(strings.Repeat("x", width-1)+"\n", n) }

// lines is n lines of 64 bytes.
func lines(n int) string { return rows(n, 64) }

// A link to a local file carries the file's text as the client holds it
// (here, unsaved edits that differ from the disk). The same link again is
// dropped; another link to the same file says so; links to anything else
// stay links, unread.
func TestLinkedFileTextReplacesItsLink(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.py")
	if err := os.WriteFile(main, []byte("print('saved')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeReader{files: map[string]string{main: "print('unsaved')\n"}}
	got := submit(t, readingAgent(r),
		acpsdk.TextBlock("What does this do?"),
		acpsdk.ResourceLinkBlock("main.py", fileURI(main)),       // the file open in the editor
		acpsdk.ResourceLinkBlock("main.py", fileURI(main)),       // the same file, attached
		acpsdk.ResourceLinkBlock("main.py", fileURI(main)+"#L1"), // its first line
		acpsdk.ResourceLinkBlock("spec", "https://example.com/spec"),
		acpsdk.ResourceLinkBlock("hosts", "file://otherhost/etc/hosts"),
		acpsdk.ResourceLinkBlock("rel", "file:relative/path"),
	)
	want := "What does this do?\n\n" +
		"Contents of " + fileURI(main) + ":\n````\nprint('unsaved')\n\n````\n\n" +
		"[main.py](" + fileURI(main) + "#L1) (the same file as a link above)\n\n" +
		"[spec](https://example.com/spec)\n\n" +
		"[hosts](file://otherhost/etc/hosts)\n\n" +
		"[rel](file:relative/path)"
	if got != want {
		t.Errorf("submitted\n%q\nwant\n%q", got, want)
	}
	if asked := r.asked(); len(asked) != 1 || asked[0] != main {
		t.Errorf("read %q, want only %q", asked, main)
	}
}

// The forms a client writes a local file in: localhost as the host, a
// percent-encoded path, a fragment naming lines. The text's heading names
// the whole file, since the whole file follows, and keeps the link as sent,
// so the model knows which lines the user pointed at.
func TestLinkedFileURIForms(t *testing.T) {
	r := &fakeReader{files: map[string]string{"/v/my file.go": "package a\n", "/v/b.go": "package b\n"}}
	got := submit(t, readingAgent(r),
		acpsdk.ResourceLinkBlock("my file.go", "file://localhost/v/my%20file.go"),
		acpsdk.ResourceLinkBlock("b.go (lines 5-9)", "file:///v/b.go#L5:9"),
	)
	want := "Contents of file://localhost/v/my%20file.go:\n````\npackage a\n\n````\n\n" +
		"Contents of file:///v/b.go (linked as [b.go (lines 5-9)](file:///v/b.go#L5:9)):\n````\npackage b\n\n````"
	if got != want {
		t.Errorf("submitted\n%q\nwant\n%q", got, want)
	}
	if asked := r.asked(); len(asked) != 2 || asked[0] != "/v/my file.go" || asked[1] != "/v/b.go" {
		t.Errorf("read %q", asked)
	}
}

// Without a client that said it reads files, or without a connection that
// can ask it, the prompt is submitted as the client sent it.
func TestLinkedFilesNeedAClientThatReads(t *testing.T) {
	r := &fakeReader{files: map[string]string{"/x/a.go": "package a\n"}}
	blocks := []acpsdk.ContentBlock{acpsdk.TextBlock("look"), acpsdk.ResourceLinkBlock("a.go", "file:///x/a.go")}
	message, err := promptText(blocks)
	if err != nil {
		t.Fatal(err)
	}
	noCapability := readingAgent(r)
	noCapability.clientReads.Store(false)
	noConnection := readingAgent(r)
	noConnection.files = nil
	for name, a := range map[string]*Agent{"no fs.readTextFile": noCapability, "no reader": noConnection} {
		if got := a.withLinkedFiles(context.Background(), "s1", blocks, message); got != message {
			t.Errorf("%s: submitted %q, want the prompt as sent %q", name, got, message)
		}
	}
	if asked := r.asked(); len(asked) != 0 {
		t.Errorf("read %q without a client that reads", asked)
	}
}

// A file that cannot or should not be sent stays a link and says why; a
// long one is cut back to a line break and says how much of it is included.
func TestLinkedFileLimits(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(3 << 19); err != nil { // 1.5 MB, sparse
		t.Fatal(err)
	}
	_ = f.Close()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeReader{
		files: map[string]string{
			big:          "never read",
			fifo:         "never read",
			"/v/huge":    "never read",
			"/v/long.go": rows(1800, 90), // 159 KB; the limit falls mid-line
			"/v/img.png": "\x89PNG\r\n\x1a\n\x00\x00",
		},
		errs: map[string]error{
			"/v/secret": &acpsdk.RequestError{Code: -32603, Message: "path is outside the project"},
			"/v/denied": &acpsdk.RequestError{Code: -32603, Message: "Internal error", Data: map[string]any{"error": "permission denied"}},
		},
	}
	a := readingAgent(r)
	for _, tc := range []struct {
		name string
		link acpsdk.ContentBlock
		want string // the submitted text
		read bool
	}{
		{"too large on disk", acpsdk.ResourceLinkBlock("x", fileURI(big)),
			"[x](" + fileURI(big) + ") (not included: it is 1.5 MB, and fleet acp reads linked files of up to 1 MB)", false},
		{"too large by its link", sizedLink("x", "file:///v/huge", 2<<20),
			"[x](file:///v/huge) (not included: it is 2 MB, and fleet acp reads linked files of up to 1 MB)", false},
		{"folder", acpsdk.ResourceLinkBlock("x", fileURI(dir)),
			"[x](" + fileURI(dir) + ") (not included: it is a folder; fleet acp includes linked files, not folders)", false},
		{"not a regular file", acpsdk.ResourceLinkBlock("x", fileURI(fifo)),
			"[x](" + fileURI(fifo) + ") (not included: it is not a regular file)", false},
		{"cut at a line", acpsdk.ResourceLinkBlock("x", "file:///v/long.go"),
			"Contents of file:///v/long.go (the first 100 KB of 159 KB: fleet acp includes up to 100 KB of a linked file):\n````\n" + rows(1137, 90) + "\n````", true},
		{"not text", acpsdk.ResourceLinkBlock("x", "file:///v/img.png"),
			"[x](file:///v/img.png) (not included: it is not a text file)", true},
		{"refused", acpsdk.ResourceLinkBlock("x", "file:///v/secret"),
			"[x](file:///v/secret) (not included: the editor could not read it: path is outside the project)", true},
		{"refused, with detail", acpsdk.ResourceLinkBlock("x", "file:///v/denied"),
			"[x](file:///v/denied) (not included: the editor could not read it: Internal error: permission denied)", true},
		{"missing", acpsdk.ResourceLinkBlock("x", "file:///v/gone"),
			"[x](file:///v/gone) (not included: the editor could not read it: no such file)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(r.asked())
			got := submit(t, a, tc.link)
			if got != tc.want {
				t.Errorf("submitted\n%.300q\nwant\n%.300q", got, tc.want)
			}
			if read := len(r.asked()) > before; read != tc.read {
				t.Errorf("read = %v, want %v", read, tc.read)
			}
		})
	}
}

func TestCutAt(t *testing.T) {
	euros := strings.Repeat("€", 50000) // 3 bytes each: 102400 falls inside one
	shortFirstLine := "x\n" + strings.Repeat("y", 200<<10)
	for _, tc := range []struct {
		name, text string
		limit      int
		want       string
	}{
		{"fits", "abc\n", 10, "abc\n"},
		{"back to a line break", "aaaa\nbbbb\ncccc\n", 12, "aaaa\nbbbb\n"},
		{"a line break at the limit", "aaaa\nbbbb\n", 5, "aaaa\n"},
		{"no line break: between characters", euros, 102400, euros[:102399]},
		{"a line break too early: between characters", shortFirstLine, 102400, shortFirstLine[:102400]},
	} {
		got := cutAt(tc.text, tc.limit)
		if got != tc.want {
			t.Errorf("%s: cut to %d bytes, want %d", tc.name, len(got), len(tc.want))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: cut inside a character", tc.name)
		}
	}
}

// The files of one message share linkedFilesBudget. A file that fits what
// is left goes whole, however little that is; the one that reaches the
// limit is cut, with a note naming the shared limit; and once too little is
// left to cut to, a file that does not fit is left out: after it is read,
// or, when its size is known, without reading it.
func TestLinkedFilesShareOneBudget(t *testing.T) {
	r := &fakeReader{files: map[string]string{"/v/small": "small", "/v/tiny": "tiny!"}}
	names := []string{"a", "b", "c", "small", "d", "tiny", "e"}
	blocks := make([]acpsdk.ContentBlock, 0, len(names))
	for _, name := range names {
		if _, ok := r.files["/v/"+name]; !ok {
			r.files["/v/"+name] = lines(1584) // 99 KB
		}
		blocks = append(blocks, acpsdk.ResourceLinkBlock(name, "file:///v/"+name))
	}
	blocks = append(blocks, sizedLink("f", "file:///v/f", 2000))
	got := submit(t, readingAgent(r), blocks...)
	for _, want := range []string{
		"Contents of file:///v/a:\n````\n" + lines(1584) + "\n````",
		"Contents of file:///v/c:\n````\n" + lines(1584) + "\n````",
		"Contents of file:///v/small:\n````\nsmall\n````", // 3 KB left
		"Contents of file:///v/d (the first 3 KB of 99 KB: fleet acp includes up to 300 KB of linked files in one message):\n````\n" + lines(47) + "\n````",
		"Contents of file:///v/tiny:\n````\ntiny!\n````", // 59 bytes left
		"[e](file:///v/e) (not included: this message already carries 300 KB of linked files, the most fleet acp sends)",
		"[f](file:///v/f) (not included: this message already carries 300 KB of linked files, the most fleet acp sends)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("submitted text is missing %.200q", want)
		}
	}
	for _, path := range r.asked() {
		if path == "/v/f" {
			t.Error("f was read although its size showed it could not fit")
		}
	}
}

// A file whose size fleet acp does not know (not on disk, no size on its
// link) is asked for a bounded number of lines; one it knows is asked for
// whole. When the answer may stop short of the file's end, the note does
// not claim the file's size.
func TestUnknownSizeAsksForBoundedLines(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(onDisk, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeReader{files: map[string]string{
		onDisk:     "a\n",
		"/v/sized": "b\n",
		// As many lines as were asked for, the last one's line break
		// dropped: the answer may stop short of the file's end.
		"/v/buffer": strings.TrimSuffix(rows(linkedFileLines, 2), "\n"),
		"/v/zero":   "c\n",
	}}
	got := submit(t, readingAgent(r),
		acpsdk.ResourceLinkBlock("a", fileURI(onDisk)),
		sizedLink("sized", "file:///v/sized", 2),
		acpsdk.ResourceLinkBlock("buffer", "file:///v/buffer"),
		sizedLink("zero", "file:///v/zero", 0), // a size of 0 is no size
	)
	r.mu.Lock()
	reqs := append([]acpsdk.ReadTextFileRequest(nil), r.reqs...)
	r.mu.Unlock()
	if len(reqs) != 4 {
		t.Fatalf("%d reads, want 4", len(reqs))
	}
	bounded := func() *int { n := linkedFileLines; return &n }()
	for i, wantLimit := range []*int{nil, nil, bounded, bounded} {
		switch lim := reqs[i].Limit; {
		case wantLimit == nil && lim != nil:
			t.Errorf("read of %s asked for %d lines, want the whole file", reqs[i].Path, *lim)
		case wantLimit != nil && (lim == nil || *lim != *wantLimit):
			t.Errorf("read of %s asked for %v lines, want %d", reqs[i].Path, lim, *wantLimit)
		}
	}
	want := "Contents of file:///v/buffer (the first 100 KB of a longer file: fleet acp includes up to 100 KB of a linked file):"
	if !strings.Contains(got, want) {
		t.Errorf("submitted text is missing %q", want)
	}
}

// A client that does not answer holds the message for linkedFilesWait in
// all: once it has run out, the files after it are not asked for, and each
// says why it is missing.
func TestLinkedFileReadsGiveUp(t *testing.T) {
	prev := linkedFilesWait
	linkedFilesWait = 50 * time.Millisecond
	t.Cleanup(func() { linkedFilesWait = prev })
	r := &fakeReader{files: map[string]string{"/v/a": "a", "/v/b": "b"}, block: make(chan struct{})}
	got := submit(t, readingAgent(r),
		acpsdk.ResourceLinkBlock("a", "file:///v/a"), acpsdk.ResourceLinkBlock("b", "file:///v/b"))
	for _, name := range []string{"a", "b"} {
		want := "[" + name + "](file:///v/" + name + ") (not included: the 50ms fleet acp waits for a message's linked files ran out before it arrived)"
		if !strings.Contains(got, want) {
			t.Errorf("submitted %q, want %q", got, want)
		}
	}
	if asked := r.asked(); len(asked) != 1 || asked[0] != "/v/a" {
		t.Errorf("read %q, want only /v/a: the wait had run out before b", asked)
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int]string{
		0: "0 bytes", 1023: "1023 bytes", 1024: "1 KB", 1025: "2 KB", 100 << 10: "100 KB",
		1023 << 10: "1023 KB", 1023<<10 + 1: "1 MB", 1 << 20: "1 MB", 1<<20 + 1: "1.1 MB",
		3 << 19: "1.5 MB", 10 << 20: "10 MB",
	} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// newReadingSession opens a session from a client that said in initialize
// that it answers fs/read_text_file, through h.client.read.
func (h *harness) newReadingSession(t *testing.T) acpsdk.SessionId {
	t.Helper()
	ctx := context.Background()
	caps := acpsdk.ClientCapabilities{Fs: acpsdk.FileSystemCapabilities{ReadTextFile: true}}
	if _, err := h.conn.Initialize(ctx, acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber, ClientCapabilities: caps}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	s, err := h.conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: "/work", McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	return s.SessionId
}

// promptBlocks sends a prompt of blocks without waiting for its answer.
func (h *harness) promptBlocks(sid acpsdk.SessionId, blocks ...acpsdk.ContentBlock) <-chan promptResult {
	done := make(chan promptResult, 1)
	go func() {
		r, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: blocks})
		done <- promptResult{r, err}
	}()
	return done
}

// readCount is how many fs/read_text_file requests the client has had.
func (h *harness) readCount() int {
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	return len(h.client.reads)
}

// waitReads waits until the client has had n reads.
func (h *harness) waitReads(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.readCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d reads, want %d", h.readCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockReads makes the client hold every read until the test ends or the
// read is given up on.
func (h *harness) blockReads(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.client.read = func(ctx context.Context, _ acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return acpsdk.ReadTextFileResponse{Content: "late"}, nil
	}
}

// End to end over the SDK: the client's initialize turns reading on, the
// read names the prompt's session, and fleet gets the file's text.
func TestPromptSendsLinkedFileTextToFleet(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.client.read = func(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
		return acpsdk.ReadTextFileResponse{Content: "def hi():\n    return 1\n"}, nil
	}
	sid := h.newReadingSession(t)
	if _, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{
		acpsdk.TextBlock("What does hi return?"), acpsdk.ResourceLinkBlock("main.py", "file:///nonexistent/main.py"),
	}}); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	want := "What does hi return?\n\nContents of file:///nonexistent/main.py:\n````\ndef hi():\n    return 1\n\n````"
	if len(h.fleet.chats) != 1 || h.fleet.chats[0].Message != want {
		t.Fatalf("fleet got %+v, want one message %q", h.fleet.chats, want)
	}
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	if len(h.client.reads) != 1 || h.client.reads[0].SessionId != sid || h.client.reads[0].Path != "/nonexistent/main.py" {
		t.Errorf("reads = %+v, want one of /nonexistent/main.py on %s", h.client.reads, sid)
	}
}

// A session/cancel while the linked files are read ends the prompt
// cancelled: the files after the one being read are not asked for, and
// nothing reaches fleet.
func TestCancelWhileReadingLinkedFilesSubmitsNothing(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.blockReads(t)
	sid := h.newReadingSession(t)
	done := h.promptBlocks(sid, acpsdk.TextBlock("explain"),
		acpsdk.ResourceLinkBlock("a.go", "file:///nonexistent/a.go"),
		acpsdk.ResourceLinkBlock("b.go", "file:///nonexistent/b.go"))
	h.waitReads(t, 1)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	r := await(t, done, "the cancelled prompt")
	if r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v; want cancelled", r.resp, r.err)
	}
	if n := h.readCount(); n != 1 {
		t.Errorf("%d reads, want 1: b.go was asked for after the cancel", n)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 0 {
		t.Errorf("fleet got %d turns, want none", len(h.fleet.chats))
	}
}

// A prompt cancelled while it waits behind another for the session reads
// none of its files.
func TestPromptCancelledWhileWaitingReadsNothing(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, harnessOpts{turn: blockingTurn(started)})
	h.blockReads(t)
	sid := h.newReadingSession(t)
	first := h.promptBlocks(sid, acpsdk.TextBlock("long job"))
	<-started
	second := h.promptBlocks(sid, acpsdk.TextBlock("explain"), acpsdk.ResourceLinkBlock("a.go", "file:///nonexistent/a.go"))
	h.waitTracked(t, sid, 2)
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	for what, done := range map[string]<-chan promptResult{"the running prompt": first, "the waiting prompt": second} {
		if r := await(t, done, what); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
			t.Errorf("%s = %+v, %v; want cancelled", what, r.resp, r.err)
		}
	}
	if n := h.readCount(); n != 0 {
		t.Errorf("%d reads, want none", n)
	}
}

// The client going away while the linked files are read stops the prompt
// before it is submitted.
func TestHangUpWhileReadingLinkedFilesSubmitsNothing(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.blockReads(t)
	sid := h.newReadingSession(t)
	h.promptBlocks(sid, acpsdk.TextBlock("explain"), acpsdk.ResourceLinkBlock("a.go", "file:///nonexistent/a.go"))
	h.waitReads(t, 1)
	h.hangUp()
	idle, _ := h.agent.inFlight()
	select {
	case <-idle:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt was still in flight after the hang-up")
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 0 {
		t.Errorf("fleet got %d turns, want none", len(h.fleet.chats))
	}
}

// A resend keeps its key however the linked file changed in between: it is
// the same message, keyed by the prompt as sent, not by the files' text.
func TestResendKeyIgnoresLinkedFileText(t *testing.T) {
	calls := 0
	h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.w.WriteHeader(http.StatusOK) // accepted, then the stream is lost
			return
		}
		w.emit("conversation", map[string]any{"id": "c"})
		w.emit("turn.completed", map[string]any{})
	}})
	version := 0
	h.client.read = func(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
		version++
		return acpsdk.ReadTextFileResponse{Content: strings.Repeat("v", version)}, nil
	}
	sid := h.newReadingSession(t)
	prompt := func() error {
		_, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{
			acpsdk.TextBlock("book it"), acpsdk.ResourceLinkBlock("plan.md", "file:///nonexistent/plan.md"),
		}})
		return err
	}
	if err := prompt(); err == nil {
		t.Fatal("want the lost-stream error")
	}
	if err := prompt(); err != nil {
		t.Fatal(err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	if len(h.fleet.chats) != 2 {
		t.Fatalf("fleet got %d turns, want 2", len(h.fleet.chats))
	}
	if k0, k1 := h.fleet.chats[0].InputID, h.fleet.chats[1].InputID; k0 == "" || k0 != k1 {
		t.Errorf("resend key %q != original %q", k1, k0)
	}
	if !strings.Contains(h.fleet.chats[1].Message, "````\nvv\n````") {
		t.Errorf("resend carried %q, want the file as it is now", h.fleet.chats[1].Message)
	}
}
