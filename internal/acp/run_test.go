package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestRunSpeaksACPOnStdio drives the real `fleet acp` entry point the way an
// ACP client does: newline-delimited JSON-RPC on stdin, responses on stdout.
// Every stdout line must be a JSON-RPC message — a stray print would corrupt
// the protocol stream for the client.
func TestRunSpeaksACPOnStdio(t *testing.T) {
	srv := httptest.NewServer(&fakeFleet{t: t})
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	exit := make(chan int, 1)
	go func() {
		exit <- run([]string{"--server", srv.URL, "--email", "bot@example.com", "--token-file", tokenFile}, inR, outW, &stderr, nil)
		_ = outW.Close()
	}()

	lines := bufio.NewScanner(outR)
	lines.Buffer(make([]byte, 0, 64*1024), 1<<20)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	// next returns the next stdout message, failing on anything that is not
	// JSON-RPC 2.0.
	next := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("stdout closed early: %v (stderr: %s)", lines.Err(), stderr.String())
		}
		var m map[string]any
		if err := json.Unmarshal(lines.Bytes(), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("non-JSON-RPC line on stdout: %q", lines.Text())
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	if m := next(); m["id"] != float64(1) || m["result"].(map[string]any)["protocolVersion"] != float64(1) {
		t.Fatalf("initialize reply = %v", m)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`)
	m := next()
	sid, _ := m["result"].(map[string]any)["sessionId"].(string)
	if !strings.HasPrefix(sid, "fleet-acp-") {
		t.Fatalf("session/new reply = %v", m)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[{"type":"text","text":"hi"}]}}`)
	var text strings.Builder
	for {
		m := next()
		if m["method"] == "session/update" {
			u := m["params"].(map[string]any)["update"].(map[string]any)
			if u["sessionUpdate"] == "agent_message_chunk" {
				text.WriteString(u["content"].(map[string]any)["text"].(string))
			}
			continue
		}
		if m["id"] != float64(3) {
			t.Fatalf("unexpected message %v", m)
		}
		if m["result"].(map[string]any)["stopReason"] != "end_turn" {
			t.Fatalf("prompt reply = %v", m)
		}
		break
	}
	if text.String() != "Hello there" {
		t.Errorf("streamed %q", text.String())
	}

	_ = inW.Close() // the client hangs up → fleet acp exits cleanly
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fleet acp did not exit after stdin closed")
	}
	if strings.Contains(stderr.String(), "test-token") {
		t.Error("the token leaked to stderr")
	}
}

func TestRunRejectsStrayArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"serve"}, strings.NewReader(""), io.Discard, &stderr, nil); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code := run([]string{"--nope"}, strings.NewReader(""), io.Discard, &stderr, nil); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	// Only 0 disables the bound: a negative timeout is refused, not taken as
	// "unbounded".
	stderr.Reset()
	if code := run([]string{"--timeout=-1s"}, strings.NewReader(""), io.Discard, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "negative") {
		t.Errorf("negative timeout: exit %d, stderr %q; want 2 and a refusal", code, stderr.String())
	}
}

// lockedBuffer is a run's stderr under test: slog, awaitStops and reportGone
// write it from their own goroutines while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// acpClient is the ACP client's side of fleet acp's stdio, whether fleet acp
// runs in-process (startRun) or as a child process (startProcess).
type acpClient struct {
	t      *testing.T
	in     io.WriteCloser // fleet acp's stdin; closing it is a hang-up
	out    chan string    // fleet acp's stdout, one line each; closed at EOF
	stderr *lockedBuffer
}

func newACPClient(t *testing.T, in io.WriteCloser, out io.Reader, stderr *lockedBuffer) *acpClient {
	c := &acpClient{t: t, in: in, out: make(chan string, 1024), stderr: stderr}
	go func() {
		lines := bufio.NewScanner(out)
		lines.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for lines.Scan() {
			c.out <- lines.Text()
		}
		close(c.out)
	}()
	return c
}

func (c *acpClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// next returns the next stdout message, failing on anything that is not
// JSON-RPC 2.0.
func (c *acpClient) next() map[string]any {
	c.t.Helper()
	select {
	case line, ok := <-c.out:
		if !ok {
			c.t.Fatalf("stdout closed early (stderr: %s)", c.stderr)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
			c.t.Fatalf("non-JSON-RPC line on stdout: %q", line)
		}
		return m
	case <-time.After(10 * time.Second):
		c.t.Fatalf("nothing on stdout (stderr: %s)", c.stderr)
		return nil
	}
}

// openSession initializes and opens one ACP session.
func (c *acpClient) openSession() string {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	c.next()
	return c.newSession(2)
}

// newSession opens another ACP session, as request id.
func (c *acpClient) newSession(id int) string {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`, id))
	m := c.answer(id)
	result, _ := m["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		c.t.Fatalf("session/new reply = %v", m)
	}
	return sid
}

// prompt sends a session/prompt request without waiting for its answer.
func (c *acpClient) prompt(id int, sid, text string) {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":%q}]}}`, id, sid, text))
}

// working reads stdout up to the long turn's "working" chunk (blockingTurn's
// last frame before it holds), so every frame the turn sent has been read.
func (c *acpClient) working() {
	c.t.Helper()
	for {
		m := c.next()
		params, _ := m["params"].(map[string]any)
		u, _ := params["update"].(map[string]any)
		if m["method"] != "session/update" || u == nil {
			c.t.Fatalf("unexpected message %v", m)
		}
		content, _ := u["content"].(map[string]any)
		if u["sessionUpdate"] == "agent_message_chunk" && content["text"] == "working" {
			return
		}
	}
}

// answer reads stdout up to the reply to request id.
func (c *acpClient) answer(id int) map[string]any {
	c.t.Helper()
	for {
		if m := c.next(); m["id"] == float64(id) {
			return m
		}
	}
}

// rest is what reached stdout after the reads so far, up to its EOF.
func (c *acpClient) rest() []string {
	var lines []string
	for line := range c.out {
		lines = append(lines, line)
	}
	return lines
}

// awaitStderr waits (bounded) until stderr contains want.
func (c *acpClient) awaitStderr(want string) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.stderr.String(), want) {
		if time.Now().After(deadline) {
			c.t.Fatalf("stderr never said %q: %s", want, c.stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stdioRun is `fleet acp` run in-process through its real entry point (run)
// on pipes, the way an ACP client launches it. stop stands in for the
// signals Run relays (stopSignals), so a test can deliver a SIGTERM without
// signalling the test process; TestProcess* send real ones to a real process.
type stdioRun struct {
	*acpClient
	stop chan os.Signal
	exit chan int
}

func startRun(t *testing.T, ff *fakeFleet) *stdioRun {
	t.Helper()
	stderr := &lockedBuffer{}
	return startRunWithStderr(t, ff, stderr, stderr)
}

// startRunWithStderr is startRun with errOut as run's stderr; the test reads
// what reached it from stderr.
func startRunWithStderr(t *testing.T, ff *fakeFleet, stderr *lockedBuffer, errOut io.Writer) *stdioRun {
	t.Helper()
	// The fake sends no terminal frame after a Stop; do not wait long for
	// one (the real server sends turn.cancelled).
	prevSettle := stopSettleWait
	stopSettleWait = 50 * time.Millisecond
	t.Cleanup(func() { stopSettleWait = prevSettle })
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &stdioRun{
		acpClient: newACPClient(t, inW, outR, stderr),
		stop:      make(chan os.Signal, 1),
		exit:      make(chan int, 1),
	}
	go func() {
		r.exit <- run([]string{"--server", srv.URL, "--email", "bot@example.com", "--token-file", tokenFile}, inR, outW, errOut, func() <-chan os.Signal { return r.stop })
		// The SDK writes a prompt's answer just after the prompt returns,
		// which can be after run has: stdout stays open a moment longer, so
		// a frame written after the client went away reaches rest() rather
		// than a closed pipe.
		time.Sleep(stopWindow)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
	})
	return r
}

// awaitExit waits for run to return, and returns its exit code.
func (r *stdioRun) awaitExit(within time.Duration) int {
	r.t.Helper()
	select {
	case code := <-r.exit:
		return code
	case <-time.After(within):
		r.t.Fatalf("fleet acp did not exit within %s (stderr: %s)", within, r.stderr)
		return -1
	}
}

// heldUntilStopped is a long turn (blockingTurn) that refuses a second
// prompt: one that reaches fleet after the client has gone is an error.
func heldUntilStopped(ff **fakeFleet, started chan<- struct{}) func(w *sseWriter, r *http.Request) {
	return func(w *sseWriter, r *http.Request) {
		if (*ff).nth() > 1 {
			w.emit("turn.error", map[string]any{"message": "submitted after the client went away"})
			return
		}
		blockingTurn(started)(w, r)
	}
}

// slowTurnStop is the Stop for blockingTurn's turn.
const slowTurnStop = `conv-slow {"scope":"turn","turn_id":"turn-slow"}`

// An ACP client that exits or crashes mid turn closes fleet acp's stdin. The
// running turn is stopped server-side before fleet acp exits: run waits for
// the Stop to be answered, where it used to return at once and let the
// process end before the Stop went out, leaving the turn running with nobody
// watching. A prompt still waiting for the session is abandoned unsubmitted.
// Nothing more reaches stdout, and stderr says what happened.
func TestRunStopsInFlightTurnsBeforeExiting(t *testing.T) {
	started, hold := make(chan struct{}), make(chan struct{})
	var ff *fakeFleet
	ff = &fakeFleet{t: t, cancelHold: hold, turn: heldUntilStopped(&ff, started)}
	r := startRun(t, ff)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	sid := r.openSession()
	r.prompt(3, sid, "long job")
	r.working()
	r.prompt(4, sid, "queued behind it")
	time.Sleep(stopWindow) // it reaches the agent and waits for the session
	_ = r.in.Close()       // the client exits

	if got := ff.awaitStop(t); got[0] != slowTurnStop {
		t.Fatalf("Stop = %q, want the running turn's", got)
	}
	select {
	case <-r.exit:
		t.Fatal("fleet acp exited before its Stop was answered")
	case <-time.After(stopWindow):
	}
	release()
	if code := r.awaitExit(10 * time.Second); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if extra := r.rest(); len(extra) != 0 {
		t.Errorf("written to stdout after the client hung up: %q", extra)
	}
	ff.mu.Lock()
	chats, cancels := len(ff.chats), slices.Clone(ff.cancels)
	ff.mu.Unlock()
	if chats != 1 {
		t.Errorf("%d prompts reached fleet, want 1: the waiting one must not be submitted", chats)
	}
	if len(cancels) != 1 {
		t.Errorf("Stops = %q, want exactly the running turn's", cancels)
	}
	stderr := r.stderr.String()
	for _, want := range []string{
		"fleet acp: the ACP client closed the connection with 2 prompts in flight; stopping their fleet turns",
		"fleet acp: the client went away, so its turn was stopped: the fleet web chat (conversation conv-slow)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A Stop that never gets its answer (a hung server) must not keep fleet acp
// alive: after hangUpWait it exits anyway and says on stderr that the turn
// may still be running.
func TestRunGivesUpOnAStuckStopAfterTheBound(t *testing.T) {
	prevWait := hangUpWait
	hangUpWait = 300 * time.Millisecond
	t.Cleanup(func() { hangUpWait = prevWait })
	started, hold := make(chan struct{}), make(chan struct{})
	ff := &fakeFleet{t: t, cancelHold: hold, turn: blockingTurn(started)}
	r := startRun(t, ff)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	sid := r.openSession()
	r.prompt(3, sid, "long job")
	r.working()
	hungUp := time.Now()
	_ = r.in.Close()

	ff.awaitStop(t)
	// Well under the Stop request's own 10s timeout: the bound ended the wait.
	if code := r.awaitExit(5 * time.Second); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if waited := time.Since(hungUp); waited < hangUpWait {
		t.Errorf("exited %s after the hang-up, before the %s bound", waited, hangUpWait)
	}
	if want := "fleet acp: gave up after 300ms waiting for the in-flight fleet turns to finish stopping; exiting anyway. A Stop may not have reached fleet, or fleet had not confirmed it, so a turn may still be running: check the fleet web chat (conversation conv-slow)\n"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
	}
	// Let the held Stop finish before the cleanup restores the waits it reads.
	release()
	r.awaitStderr("its turn was stopped")
}

// Editors usually stop an agent subprocess with a signal rather than by
// closing its stdin. A SIGTERM, SIGINT or SIGHUP (relayed by Run's
// stopSignals) is handled like a hang-up: the turn is stopped server-side
// and fleet acp exits, though its stdin is still open.
func TestRunTreatsAStopSignalAsAHangUp(t *testing.T) {
	started := make(chan struct{})
	var ff *fakeFleet
	ff = &fakeFleet{t: t, turn: heldUntilStopped(&ff, started)}
	r := startRun(t, ff)
	sid := r.openSession()
	r.prompt(3, sid, "long job")
	r.working()
	r.stop <- syscall.SIGTERM

	if got := ff.awaitStop(t); len(got) != 1 || got[0] != slowTurnStop {
		t.Errorf("Stops = %q, want the running turn's", got)
	}
	if code := r.awaitExit(10 * time.Second); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if extra := r.rest(); len(extra) != 0 {
		t.Errorf("written to stdout after the signal: %q", extra)
	}
	stderr := r.stderr.String()
	for _, want := range []string{"fleet acp: got SIGTERM with 1 prompt in flight", "its turn was stopped"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A client that goes away with nothing in flight — the usual end of a
// session, its turn long finished — gets no Stop and no wait.
func TestRunWithNothingInFlightExitsAtOnce(t *testing.T) {
	prevWait := hangUpWait
	hangUpWait = time.Minute // a wait would show as a late exit
	t.Cleanup(func() { hangUpWait = prevWait })
	for _, tc := range []struct {
		name   string
		goAway func(r *stdioRun)
	}{
		{"stdin closed", func(r *stdioRun) { _ = r.in.Close() }},
		{"SIGTERM", func(r *stdioRun) { r.stop <- syscall.SIGTERM }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := &fakeFleet{t: t}
			r := startRun(t, ff)
			sid := r.openSession()
			r.prompt(3, sid, "hi")
			if m := r.answer(3); m["result"].(map[string]any)["stopReason"] != "end_turn" {
				t.Fatalf("prompt reply = %v", m)
			}
			tc.goAway(r)
			if code := r.awaitExit(5 * time.Second); code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if got := ff.cancelsSnapshot(); len(got) != 0 {
				t.Errorf("Stops = %q, want none: the turn had finished", got)
			}
			if stderr := r.stderr.String(); strings.Contains(stderr, "in flight") {
				t.Errorf("stderr reports prompts in flight:\n%s", stderr)
			}
		})
	}
}

// gatedWriter is a stderr nobody drains: every write blocks until open is
// closed, then lands in buf.
type gatedWriter struct {
	open chan struct{}
	buf  *lockedBuffer
}

func (g gatedWriter) Write(p []byte) (int, error) {
	<-g.open
	return g.buf.Write(p)
}

// A client that goes away may leave stderr a full pipe that nobody drains.
// What fleet acp says then must not hold the shutdown past its bound: each
// line is waited for at most stderrWait, so run still returns, here with a
// Stop that never gets its answer either.
func TestRunExitsThoughStderrIsStuck(t *testing.T) {
	prevWait, prevStderr := hangUpWait, stderrWait
	hangUpWait, stderrWait = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { hangUpWait, stderrWait = prevWait, prevStderr })
	started, hold := make(chan struct{}), make(chan struct{})
	ff := &fakeFleet{t: t, cancelHold: hold, turn: blockingTurn(started)}
	stuck := gatedWriter{open: make(chan struct{}), buf: &lockedBuffer{}}
	r := startRunWithStderr(t, ff, stuck.buf, stuck)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	unstick := sync.OnceFunc(func() { close(stuck.open) })
	t.Cleanup(unstick)
	sid := r.openSession()
	r.prompt(3, sid, "long job")
	r.working()
	_ = r.in.Close()

	ff.awaitStop(t)
	if code := r.awaitExit(5 * time.Second); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	// stderr drains at last, and the held Stop finishes, before the cleanup
	// restores the waits they read.
	unstick()
	release()
	r.awaitStderr("its turn was stopped")
	if want := "fleet acp: gave up after 300ms"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
	}
}

// A prompt that arrives after a SIGTERM, on a stdin the client still holds
// open, is never submitted: fleet acp is shutting down, and nobody would
// read its answer.
func TestRunSubmitsNothingAfterAStopSignal(t *testing.T) {
	started, hold := make(chan struct{}), make(chan struct{})
	var ff *fakeFleet
	ff = &fakeFleet{t: t, cancelHold: hold, turn: heldUntilStopped(&ff, started)}
	r := startRun(t, ff)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	sid := r.openSession()
	other := r.newSession(3) // idle: a prompt there need not wait for a session
	r.prompt(4, sid, "long job")
	r.working()
	r.stop <- syscall.SIGTERM
	ff.awaitStop(t) // shutting down, its Stop held

	r.prompt(5, other, "a late prompt")
	time.Sleep(stopWindow) // it reaches the agent while run waits
	release()
	if code := r.awaitExit(10 * time.Second); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	ff.mu.Lock()
	chats := len(ff.chats)
	ff.mu.Unlock()
	if chats != 1 {
		t.Errorf("%d prompts reached fleet, want 1: the late one must not be submitted", chats)
	}
	if extra := r.rest(); len(extra) != 0 {
		t.Errorf("written to stdout after the signal: %q", extra)
	}
}

// TestMain re-execs this test binary as a real `fleet acp` process when
// FLEET_ACP_TEST_CHILD holds its arguments (a JSON array), so what Run does
// to the whole process — relaying SIGTERM/SIGINT/SIGHUP, restoring their
// default after the first, ignoring SIGPIPE — is tested with real signals on
// a real process. The child writes only protocol frames to stdout, so it runs
// and exits before m.Run().
func TestMain(m *testing.M) {
	if args := os.Getenv("FLEET_ACP_TEST_CHILD"); args != "" {
		var argv []string
		if err := json.Unmarshal([]byte(args), &argv); err != nil {
			fmt.Fprintln(os.Stderr, "FLEET_ACP_TEST_CHILD:", err)
			os.Exit(2)
		}
		stopSettleWait = 50 * time.Millisecond // the fake sends no turn.cancelled
		os.Exit(Run(argv))
	}
	os.Exit(m.Run())
}

// acpProcess is `fleet acp` as the child process an editor launches.
type acpProcess struct {
	*acpClient
	cmd    *exec.Cmd
	stdout *os.File // the client's end of the child's stdout
	exited chan error
}

// startProcess starts the child. With wrap, it is started as wrap followed
// by this test binary's path (a shell that execs "$0", say).
func startProcess(t *testing.T, ff *fakeFleet, wrap ...string) *acpProcess {
	t.Helper()
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, _ := json.Marshal([]string{"--server", srv.URL, "--email", "bot@example.com", "--token-file", tokenFile})
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	if len(wrap) > 0 {
		cmd = exec.Command(wrap[0], append(wrap[1:], os.Args[0])...)
	}
	cmd.Env = append(os.Environ(), "FLEET_ACP_TEST_CHILD="+string(argv))
	stderr := &lockedBuffer{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = inR.Close() // the child holds its own copies
	_ = outW.Close()
	p := &acpProcess{acpClient: newACPClient(t, inW, outR, stderr), cmd: cmd, stdout: outR, exited: make(chan error, 1)}
	go func() { p.exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // a failed test's child; a no-op once it has exited
		_ = inW.Close()
		_ = outR.Close()
	})
	return p
}

// awaitExit waits for the child to exit, and returns how (nil = status 0).
func (p *acpProcess) awaitExit(within time.Duration) error {
	p.t.Helper()
	select {
	case err := <-p.exited:
		return err
	case <-time.After(within):
		p.t.Fatalf("fleet acp did not exit within %s (stderr: %s)", within, p.stderr)
		return nil
	}
}

// startLongTurn opens a session on the child and starts a long turn on it.
func (p *acpProcess) startLongTurn() {
	p.t.Helper()
	sid := p.openSession()
	p.prompt(3, sid, "long job")
	p.working()
}

// killedBy reports whether a child's exit was death by sig.
func killedBy(err error, sig syscall.Signal) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}

// The real process, stopped the way editors stop it: a SIGTERM, SIGINT or
// SIGHUP stops the turn server-side, and only then does fleet acp exit (0).
// Without the handler, Go's default action ends the process on the spot and
// the turn runs on.
func TestProcessStopsItsTurnOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skipf("%v is ignored in the test process, so the child inherits it ignored, and fleet acp leaves it so", sig)
			}
			started, hold := make(chan struct{}), make(chan struct{})
			ff := &fakeFleet{t: t, cancelHold: hold, turn: blockingTurn(started)}
			p := startProcess(t, ff)
			release := sync.OnceFunc(func() { close(hold) })
			t.Cleanup(release)
			p.startLongTurn()
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if got := ff.awaitStop(t); got[0] != slowTurnStop {
				t.Fatalf("Stop = %q, want the running turn's", got)
			}
			select {
			case err := <-p.exited:
				t.Fatalf("fleet acp exited (%v) before its Stop was answered", err)
			case <-time.After(stopWindow):
			}
			release()
			if err := p.awaitExit(10 * time.Second); err != nil {
				t.Errorf("exit = %v, want status 0 (stderr: %s)", err, p.stderr)
			}
			if want := "fleet acp: got " + signalName(sig) + " with 1 prompt in flight"; !strings.Contains(p.stderr.String(), want) {
				t.Errorf("stderr lacks %q:\n%s", want, p.stderr)
			}
		})
	}
}

// A second signal ends the process at once, even while the first is still
// waiting for its Stop: shutdown can never hang on a slow server.
func TestProcessSecondSignalExitsAtOnce(t *testing.T) {
	if signal.Ignored(syscall.SIGTERM) {
		t.Skip("SIGTERM is ignored in the test process, so the child inherits it ignored")
	}
	started, hold := make(chan struct{}), make(chan struct{})
	ff := &fakeFleet{t: t, cancelHold: hold, turn: blockingTurn(started)}
	p := startProcess(t, ff)
	t.Cleanup(sync.OnceFunc(func() { close(hold) }))
	p.startLongTurn()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	ff.awaitStop(t) // the first SIGTERM is being handled: its Stop is out, and held
	select {
	case err := <-p.exited:
		t.Fatalf("fleet acp exited (%v) on the first SIGTERM, before its Stop was answered", err)
	case <-time.After(stopWindow):
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Far inside the child's 20s hangUpWait: the second signal did not wait.
	if err := p.awaitExit(5 * time.Second); !killedBy(err, syscall.SIGTERM) {
		t.Errorf("exit = %v, want death by the second SIGTERM", err)
	}
}

// An editor that crashes takes the read end of fleet acp's stdout with it,
// and the turn keeps streaming. The write that fails must not kill the
// process (Go's default for a broken stdout is death by SIGPIPE), and it is
// itself the client going away: the turn is stopped and fleet acp exits,
// without waiting for a stdin EOF that may never come (stdin stays open
// here).
func TestProcessStopsItsTurnWhenStdoutBreaks(t *testing.T) {
	started, more := make(chan struct{}), make(chan struct{})
	ff := &fakeFleet{t: t, turn: func(w *sseWriter, r *http.Request) {
		w.emit("conversation", map[string]any{"id": "conv-slow"})
		w.emit("turn.started", map[string]any{"turn_id": "turn-slow"})
		w.emit("text.delta", map[string]any{"text": "working"})
		close(started)
		select {
		case <-more:
		case <-r.Context().Done():
			return
		}
		w.emit("text.delta", map[string]any{"text": " still working"})
		<-r.Context().Done()
	}}
	p := startProcess(t, ff)
	sendMore := sync.OnceFunc(func() { close(more) })
	t.Cleanup(sendMore)
	p.startLongTurn()
	_ = p.stdout.Close() // the client's end of stdout is gone
	sendMore()           // the turn streams on: the child writes to a broken pipe
	if got := ff.awaitStop(t); got[0] != slowTurnStop {
		t.Errorf("Stop = %q, want the running turn's", got)
	}
	if err := p.awaitExit(10 * time.Second); err != nil {
		t.Errorf("exit = %v, want status 0 (stderr: %s)", err, p.stderr)
	}
	if want := "fleet acp: the ACP client stopped reading stdout ("; !strings.Contains(p.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, p.stderr)
	}
}

// A SIGHUP (or SIGINT) the process was started with ignored — nohup, or a
// `trap "" HUP` in the shell that launches it — stays ignored: it neither
// stops the turn nor ends fleet acp. Go keeps no inherited ignore of
// SIGTERM, so a SIGTERM is handled even under `trap "" TERM`.
func TestProcessKeepsAnInheritedSignalIgnore(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to start the child with signals ignored")
	}
	started := make(chan struct{})
	ff := &fakeFleet{t: t, turn: blockingTurn(started)}
	p := startProcess(t, ff, sh, "-c", `trap "" HUP TERM; exec "$0"`)
	p.startLongTurn()
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(stopWindow) // time enough for a wrongly handled SIGHUP to send its Stop
	if got := ff.cancelsSnapshot(); len(got) != 0 {
		t.Fatalf("a SIGHUP started ignored stopped the turn: Stops = %q", got)
	}
	select {
	case err := <-p.exited:
		t.Fatalf("fleet acp exited (%v) on a SIGHUP it was started with ignored", err)
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if got := ff.awaitStop(t); got[0] != slowTurnStop {
		t.Errorf("Stop = %q, want the running turn's", got)
	}
	if err := p.awaitExit(10 * time.Second); err != nil {
		t.Errorf("exit = %v, want status 0 (stderr: %s)", err, p.stderr)
	}
	if want := "fleet acp: got SIGTERM with 1 prompt in flight"; !strings.Contains(p.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, p.stderr)
	}
}

// TestPackageStaysAClient pins the one-governed-loop invariant (ADR-0001) for
// this adapter: `fleet acp` is a protocol translator in front of POST /chat,
// so it must never import the packages that execute a turn. A change that
// wants to run the agent in-process here is a second governance path, and the
// right fix is to route it through the server, not to edit this list.
func TestPackageStaysAClient(t *testing.T) {
	forbidden := []string{
		"internal/agentcore", "internal/agent", "internal/sandbox", "internal/tools",
		"internal/mcp", "internal/mcpbroker", "internal/creds", "internal/store",
		"internal/httpapi", "internal/runner", "internal/sched",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == "github.com/ElcanoTek/fleet/"+bad || strings.HasPrefix(path, "github.com/ElcanoTek/fleet/"+bad+"/") {
					t.Errorf("%s imports %s — fleet acp must stay a client of the running server", f, path)
				}
			}
		}
	}
}
