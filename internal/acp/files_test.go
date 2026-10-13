package acp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
)

// fakeReader answers fs/read_text_file the way a client does, from files
// (path → text; a path not in it is refused), and records each path asked
// for. block, when set, holds every read until it is closed or the read's
// context ends.
type fakeReader struct {
	mu    sync.Mutex
	files map[string]string
	errs  map[string]error
	paths []string
	block chan struct{}
}

func (f *fakeReader) ReadTextFile(ctx context.Context, p acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	f.mu.Lock()
	f.paths = append(f.paths, p.Path)
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

func (f *fakeReader) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// readingAgent is an Agent whose client answers reads from r.
func readingAgent(r *fakeReader) *Agent {
	a := NewAgent(nil, nil, "", 0, "test")
	a.files = r
	a.clientReads.Store(true)
	return a
}

func fileURI(path string) string { return "file://" + path }

// submit is what a prompt of blocks submits to fleet through a.
func submit(t *testing.T, a *Agent, blocks ...acpsdk.ContentBlock) string {
	t.Helper()
	message, err := promptText(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return a.withLinkedFiles(context.Background(), "s1", blocks, message)
}

// A link to a local file carries the file's text as the client holds it
// (here, unsaved edits that differ from the disk), once however often it is
// linked; links to anything else stay links, unread.
func TestLinkedFileTextReplacesItsLink(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.py")
	if err := os.WriteFile(main, []byte("print('saved')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeReader{files: map[string]string{main: "print('unsaved')\n"}}
	got := submit(t, readingAgent(r),
		acpsdk.TextBlock("What does this do?"),
		acpsdk.ResourceLinkBlock("main.py", fileURI(main)), // the file open in the editor
		acpsdk.ResourceLinkBlock("main.py", fileURI(main)), // the same file, attached
		acpsdk.ResourceLinkBlock("spec", "https://example.com/spec"),
		acpsdk.ResourceLinkBlock("hosts", "file://otherhost/etc/hosts"),
		acpsdk.ResourceLinkBlock("rel", "file:relative/path"),
	)
	want := "What does this do?\n\n" +
		"Contents of " + fileURI(main) + ":\n````\nprint('unsaved')\n\n````\n\n" +
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

// lines is n lines of 64 bytes each.
func lines(n int) string { return strings.Repeat(strings.Repeat("x", 63)+"\n", n) }

// A file that cannot or should not be sent stays a link and says why; a
// long one is cut at a line break and says how much of it is included.
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
	long := lines(2400) // 150 KB
	oneLine := strings.Repeat("é", 75<<10)
	r := &fakeReader{
		files: map[string]string{
			big:          "never read",
			"/v/long.go": long,
			"/v/one.min": oneLine,
			"/v/img.png": "\x89PNG\r\n\x1a\n\x00\x00",
		},
		errs: map[string]error{"/v/secret": &acpsdk.RequestError{Code: -32603, Message: "path is outside the project"}},
	}
	a := readingAgent(r)
	for _, tc := range []struct {
		name, path string
		want       string // in the submitted text
		read       bool
	}{
		{"too large on disk", big, "[x](" + fileURI(big) + ") (not included: it is 1.5 MB, and fleet acp reads linked files of up to 1 MB)", false},
		{"folder", dir, "[x](" + fileURI(dir) + ") (not included: it is a folder; fleet acp includes linked files, not folders)", false},
		{"cut at a line", "/v/long.go", "Contents of file:///v/long.go (the first 100 KB of 150 KB: fleet acp includes up to 100 KB of a linked file):\n````\n" + lines(1600) + "\n````", true},
		{"not text", "/v/img.png", "[x](file:///v/img.png) (not included: it is not a text file)", true},
		{"refused", "/v/secret", "[x](file:///v/secret) (not included: the editor could not read it: path is outside the project)", true},
		{"missing", "/v/gone", "[x](file:///v/gone) (not included: the editor could not read it: no such file)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(r.asked())
			got := submit(t, a, acpsdk.ResourceLinkBlock("x", fileURI(tc.path)))
			if got != tc.want {
				t.Errorf("submitted\n%.300q\nwant\n%.300q", got, tc.want)
			}
			if read := len(r.asked()) > before; read != tc.read {
				t.Errorf("read = %v, want %v", read, tc.read)
			}
		})
	}

	// A file of one long line is cut between characters, never inside one.
	got := submit(t, a, acpsdk.ResourceLinkBlock("one.min", "file:///v/one.min"))
	body := strings.TrimSuffix(strings.SplitN(got, "````\n", 2)[1], "\n````")
	if len(body) > linkedFileMax || !utf8.ValidString(body) || !strings.HasPrefix(oneLine, body) {
		t.Errorf("a long line was cut to %d bytes (valid UTF-8: %v)", len(body), utf8.ValidString(body))
	}
}

// The files of one message share linkedFilesMax: the one that reaches it is
// cut, with a note naming the shared limit, and those after it are left
// out.
func TestLinkedFilesShareOneBudget(t *testing.T) {
	r := &fakeReader{files: map[string]string{}}
	var blocks []acpsdk.ContentBlock
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		r.files["/v/"+name] = lines(1584) // 99 KB
		blocks = append(blocks, acpsdk.ResourceLinkBlock(name, "file:///v/"+name))
	}
	got := submit(t, readingAgent(r), blocks...)
	for _, want := range []string{
		"Contents of file:///v/a:\n````\n" + lines(1584) + "\n````",
		"Contents of file:///v/c:\n````\n" + lines(1584) + "\n````",
		"Contents of file:///v/d (the first 3 KB of 99 KB: fleet acp includes up to 300 KB of linked files in one message):\n````\n" + lines(48) + "\n````",
		"[e](file:///v/e) (not included: this message already carries 300 KB of linked files, the most fleet acp sends)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("submitted text is missing %.200q", want)
		}
	}
	if asked := r.asked(); len(asked) != 4 {
		t.Errorf("read %q, want a to d (e is past the budget)", asked)
	}
}

// A client that does not answer holds the message for linkedFilesWait in
// all, and the files it did not send say so.
func TestLinkedFileReadsGiveUp(t *testing.T) {
	prev := linkedFilesWait
	linkedFilesWait = 50 * time.Millisecond
	t.Cleanup(func() { linkedFilesWait = prev })
	r := &fakeReader{files: map[string]string{"/v/a": "a", "/v/b": "b"}, block: make(chan struct{})}
	start := time.Now()
	got := submit(t, readingAgent(r),
		acpsdk.ResourceLinkBlock("a", "file:///v/a"), acpsdk.ResourceLinkBlock("b", "file:///v/b"))
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("gave up after %s", took)
	}
	for _, name := range []string{"a", "b"} {
		want := "[" + name + "](file:///v/" + name + ") (not included: the editor had not sent it within the 50ms fleet acp waits for a message's linked files)"
		if !strings.Contains(got, want) {
			t.Errorf("submitted %q, want %q", got, want)
		}
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int]string{
		0: "0 bytes", 1023: "1023 bytes", 1024: "1 KB", 1025: "2 KB", 100 << 10: "100 KB",
		1 << 20: "1 MB", 3 << 19: "1.5 MB", 10 << 20: "10 MB",
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

// End to end over the SDK: the client's initialize turns reading on, the
// read names the prompt's session, and fleet gets the file's text.
func TestPromptSendsLinkedFileTextToFleet(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.client.read = func(_ context.Context, p acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
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
// cancelled, and nothing reaches fleet.
func TestCancelWhileReadingLinkedFilesSubmitsNothing(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.client.read = func(ctx context.Context, _ acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return acpsdk.ReadTextFileResponse{Content: "late"}, nil
	}
	sid := h.newReadingSession(t)
	done := make(chan promptResult, 1)
	go func() {
		r, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{
			acpsdk.TextBlock("explain"), acpsdk.ResourceLinkBlock("a.go", "file:///nonexistent/a.go"),
		}})
		done <- promptResult{r, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.client.mu.Lock()
		n := len(h.client.reads)
		h.client.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the file was never read")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	r := await(t, done, "the cancelled prompt")
	if r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v; want cancelled", r.resp, r.err)
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
