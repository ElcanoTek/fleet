package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// Clients such as Zed send every MCP server their user configured with every
// session/new (Zed 1.22.0 sent {"name":"dummy-mcp","command":"/bin/cat",
// "args":[],"env":[]}). fleet acp accepts them, never starts, contacts,
// stores or forwards them, and says so once: on stderr at session/new, and
// to the user ahead of the session's first submitted prompt.

// noticeOpening is how the MCP-server notice starts, whatever it names.
const noticeOpening = "fleet does not use the MCP server"

// since returns the updates recorded from index i on.
func (c *recordingClient) since(i int) []acpsdk.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acpsdk.SessionUpdate(nil), c.updates[i:]...)
}

// mark is the index the next recorded update will get.
func (c *recordingClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.updates)
}

// chunkText joins the agent message chunks among updates.
func chunkText(updates []acpsdk.SessionUpdate) string {
	var b strings.Builder
	for _, u := range updates {
		if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
			b.WriteString(u.AgentMessageChunk.Content.Text.Text)
		}
	}
	return b.String()
}

// firstChunk is the text of the first update when it is an agent message
// chunk, else "".
func firstChunk(updates []acpsdk.SessionUpdate) string {
	if len(updates) == 0 || updates[0].AgentMessageChunk == nil || updates[0].AgentMessageChunk.Content.Text == nil {
		return ""
	}
	return updates[0].AgentMessageChunk.Content.Text.Text
}

func stdioServer(name string) acpsdk.McpServer {
	return acpsdk.McpServer{Stdio: &acpsdk.McpServerStdio{Name: name, Command: "/bin/cat", Args: []string{}, Env: []acpsdk.EnvVariable{}}}
}

// session/new with a server of every transport succeeds, and none of them is
// started or contacted, nor sent to fleet: the stdio server's command would
// leave a marker file, and the HTTP and SSE servers point at a listener that
// counts every request. The operator gets one stderr line naming them.
func TestClientMCPServersAreAcceptedNotUsed(t *testing.T) {
	var contacted atomic.Int32
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(canary.Close)
	marker := filepath.Join(t.TempDir(), "started")
	stderr := &lockedBuffer{}
	h := newHarness(t, harnessOpts{diag: stderr})
	names := []string{"canary-stdio-7c41", "canary-http-2e9b", "canary-sse-5d08", "canary-acp-91fa"}
	sid := h.newSessionWith(t, []acpsdk.McpServer{
		{Stdio: &acpsdk.McpServerStdio{Name: names[0], Command: "/bin/sh", Args: []string{"-c", "touch " + marker}, Env: []acpsdk.EnvVariable{}}},
		{Http: &acpsdk.McpServerHttpInline{Type: "http", Name: names[1], Url: canary.URL + "/mcp", Headers: []acpsdk.HttpHeader{}}},
		{Sse: &acpsdk.McpServerSseInline{Type: "sse", Name: names[2], Url: canary.URL + "/sse", Headers: []acpsdk.HttpHeader{}}},
		{Acp: &acpsdk.McpServerAcpInline{Type: "acp", Name: names[3], Id: "acp-1"}},
	})
	if want := `fleet acp: ignoring 4 MCP servers sent by the client ("canary-stdio-7c41", "canary-http-2e9b", "canary-sse-5d08", "canary-acp-91fa"); fleet's connectors come from the operator's bundle` + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	resp, err := h.prompt(sid, "say hi")
	if err != nil || resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("prompt = %+v, %v; want end_turn", resp, err)
	}
	time.Sleep(stopWindow) // room for anything that wrongly started a server to reach it
	if n := contacted.Load(); n != 0 {
		t.Errorf("a client MCP server was contacted %d times", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the stdio MCP server's command ran (marker: %v)", err)
	}
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	for i, body := range h.fleet.bodies {
		for _, name := range append(names, canary.URL, marker) {
			if strings.Contains(body, name) {
				t.Errorf("POST /chat %d carries the client's MCP server %q: %s", i+1, name, body)
			}
		}
	}
	if strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want the one line from session/new", stderr.String())
	}
}

// The session's first prompt opens with the notice, before anything its turn
// streams; later prompts on the session do not repeat it, and a session that
// sent no servers never gets it.
func TestMCPNoticeOpensTheFirstPromptOnly(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	sid := h.newSessionWith(t, []acpsdk.McpServer{stdioServer("dummy-mcp"), stdioServer("github")})
	plain := h.newSession(t)
	single := h.newSessionWith(t, []acpsdk.McpServer{stdioServer("dummy-mcp")})

	at := h.client.mark()
	if _, err := h.prompt(sid, "say hi"); err != nil {
		t.Fatal(err)
	}
	notice := "fleet does not use the MCP servers your editor sent (`dummy-mcp`, `github`): fleet's tools and connectors come from the fleet operator and run on the fleet server.\n\n"
	first := h.client.since(at)
	if got := firstChunk(first); got != notice {
		t.Errorf("first update = %q, want the notice %q ahead of the turn", got, notice)
	}
	if got := chunkText(first); got != notice+"Hello there" {
		t.Errorf("first prompt's text = %q, want the notice and then the answer", got)
	}

	at = h.client.mark()
	if _, err := h.prompt(sid, "again"); err != nil {
		t.Fatal(err)
	}
	if got := chunkText(h.client.since(at)); got != "Hello there" {
		t.Errorf("second prompt's text = %q, want the answer alone: the notice goes out once per session", got)
	}

	at = h.client.mark()
	if _, err := h.prompt(plain, "say hi"); err != nil {
		t.Fatal(err)
	}
	if got := chunkText(h.client.since(at)); got != "Hello there" {
		t.Errorf("a session with no MCP servers got %q, want the answer alone", got)
	}

	at = h.client.mark()
	if _, err := h.prompt(single, "say hi"); err != nil {
		t.Fatal(err)
	}
	if got, want := firstChunk(h.client.since(at)), "fleet does not use the MCP server your editor sent (`dummy-mcp`): fleet's tools and connectors come from the fleet operator and run on the fleet server.\n\n"; got != want {
		t.Errorf("one server: notice = %q, want %q", got, want)
	}
}

// stdoutLog is a run's stdout (startRun) read line by line, every line kept.
type stdoutLog struct {
	r     *stdioRun
	lines []string
}

// upTo reads stdout up to the reply to request id, and returns that reply.
func (s *stdoutLog) upTo(t *testing.T, id int) map[string]any {
	t.Helper()
	for {
		select {
		case line, ok := <-s.r.out:
			if !ok {
				t.Fatalf("stdout closed early (stderr: %s)", s.r.stderr)
			}
			s.lines = append(s.lines, line)
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("non-JSON line on stdout: %q", line)
			}
			if m["id"] == float64(id) {
				return m
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no reply to request %d (stderr: %s)", id, s.r.stderr)
		}
	}
}

// close hangs up, as the client exiting does, waits for the run to exit, and
// reads the rest of stdout.
func (s *stdoutLog) close() {
	_ = s.r.in.Close()
	s.r.awaitExit(10 * time.Second)
	s.lines = append(s.lines, s.r.rest()...)
}

// text joins the agent message chunks on stdout.
func (s *stdoutLog) text() string {
	var b strings.Builder
	for _, line := range s.lines {
		var m struct {
			Params struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(line), &m)
		if m.Params.Update.SessionUpdate == "agent_message_chunk" {
			b.WriteString(m.Params.Update.Content.Text)
		}
	}
	return b.String()
}

// Secrets an MCP server entry carries — in env values, header values, args,
// a URL's query and _meta, as well as the names of env vars and headers and
// the command — appear nowhere: not on stdout (every update and response),
// not on stderr, and not in what reached fleet. Driven through the real entry
// point with the JSON a client sends, Zed's entry verbatim among them, plus a
// transport the SDK does not know and an empty entry.
func TestMCPServerSecretsNeverLeak(t *testing.T) {
	secrets := []string{
		"SECRET-CMD-6d1e", "SECRET-ARG-77a0", "SECRET-ENVNAME-3b9f", "SECRET-ENVVALUE-c2d4",
		"SECRET-META-8b52", "SECRET-URL-5e81", "SECRET-HDRNAME-0f6a", "SECRET-HDRVALUE-9a3c",
		"SECRET-HDRMETA-3f19", "SECRET-SSEURL-4b27", "SECRET-SSEHDR-e8d5", "SECRET-WSURL-1c6f",
	}
	servers := `[` +
		`{"name":"dummy-mcp","command":"/bin/cat","args":[],"env":[]},` +
		`{"name":"local-tools","command":"/opt/SECRET-CMD-6d1e/server","args":["--token","SECRET-ARG-77a0"],"env":[{"name":"SECRET-ENVNAME-3b9f","value":"SECRET-ENVVALUE-c2d4"}],"_meta":{"auth":"SECRET-META-8b52"}},` +
		`{"type":"http","name":"remote","url":"https://mcp.example.com/mcp?api_key=SECRET-URL-5e81","headers":[{"name":"SECRET-HDRNAME-0f6a","value":"Bearer SECRET-HDRVALUE-9a3c","_meta":{"k":"SECRET-HDRMETA-3f19"}}]},` +
		`{"type":"sse","name":"events","url":"https://sse.example.com/sse?sig=SECRET-SSEURL-4b27","headers":[{"name":"X-Api-Key","value":"SECRET-SSEHDR-e8d5"}]},` +
		`{"type":"websocket","name":"ws","url":"wss://ws.example.com/?k=SECRET-WSURL-1c6f"},` +
		`{}` +
		`]`
	ff := &fakeFleet{t: t}
	out := &stdoutLog{r: startRun(t, ff)}
	out.r.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	out.upTo(t, 1)
	out.r.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":` + servers + `}}`)
	m := out.upTo(t, 2)
	result, _ := m["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new = %v, want a session", m)
	}
	out.r.prompt(3, sid, "say hi")
	if m := out.upTo(t, 3); m["error"] != nil {
		t.Fatalf("first prompt = %v", m)
	}
	out.r.prompt(4, sid, "again")
	if m := out.upTo(t, 4); m["error"] != nil {
		t.Fatalf("second prompt = %v", m)
	}
	out.close()

	notice := "fleet does not use the MCP servers your editor sent (`dummy-mcp`, `local-tools`, `remote`, `events`, `ws` and 1 more): fleet's tools and connectors come from the fleet operator and run on the fleet server.\n\n"
	if got := out.text(); got != notice+"Hello there"+"Hello there" {
		t.Errorf("streamed text = %q, want the notice once, ahead of both answers", got)
	}
	if want := `fleet acp: ignoring 6 MCP servers sent by the client ("dummy-mcp", "local-tools", "remote", "events", "ws" and 1 more); fleet's connectors come from the operator's bundle` + "\n"; !strings.Contains(out.r.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", out.r.stderr, want)
	}
	ff.mu.Lock()
	sentToFleet := strings.Join(ff.bodies, "\n") + fmt.Sprint(ff.headers)
	ff.mu.Unlock()
	for _, secret := range secrets {
		for where, s := range map[string]string{"stdout": strings.Join(out.lines, "\n"), "stderr": out.r.stderr.String(), "fleet": sentToFleet} {
			if strings.Contains(s, secret) {
				t.Errorf("%s leaked to %s", secret, where)
			}
		}
	}
}

// The SDK decodes each mcpServers entry before fleet acp sees it, so what it
// makes of an odd entry is pinned here (acp-go-sdk v0.13.5). A null entry
// decodes (as HTTP, with no name) and is ignored like any other, shown as
// unnamed. An entry the SDK cannot decode is refused by the SDK with invalid
// params, before NewSession runs: one that is not an object, or one whose
// transport is recognised but has a field of the wrong JSON type, such as
// the mcp.json shapes env and headers as objects, or args as a string. The
// SDK's error text carries none of the entry's values.
func TestOddMCPServerEntriesAsTheSDKDecodesThem(t *testing.T) {
	out := &stdoutLog{r: startRun(t, &fakeFleet{t: t})}
	out.r.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	out.upTo(t, 1)

	out.r.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[null]}}`)
	m := out.upTo(t, 2)
	result, _ := m["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new with a null entry = %v, want a session", m)
	}
	out.r.prompt(3, sid, "say hi")
	if m := out.upTo(t, 3); m["error"] != nil {
		t.Fatalf("prompt = %v", m)
	}

	// Planted values, which must not come back in any reply or on stderr.
	secrets := []string{"PLANTED-ENVOBJ-2a7d", "PLANTED-ARGSTR-6c30", "PLANTED-HDROBJ-e1b4", "PLANTED-NOTOBJ-58f2"}
	refused := map[string]string{
		`{"name":"env-object","command":"/bin/x","args":[],"env":{"VAR_ONE":"PLANTED-ENVOBJ-2a7d"}}`:                                     "invalid variant payload",
		`{"name":"args-string","command":"/bin/x","args":"--token PLANTED-ARGSTR-6c30","env":[]}`:                                        "invalid variant payload",
		`{"type":"http","name":"headers-object","url":"https://x.example/mcp","headers":{"Authorization":"Bearer PLANTED-HDROBJ-e1b4"}}`: "invalid variant payload",
		`"PLANTED-NOTOBJ-58f2"`: "no matching variant for union",
	}
	id := 10
	for entry, why := range refused {
		id++
		out.r.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[%s]}}`, id, entry))
		m := out.upTo(t, id)
		rpcErr, _ := m["error"].(map[string]any)
		data, _ := rpcErr["data"].(map[string]any)
		if rpcErr == nil || rpcErr["code"] != float64(-32602) || data["error"] != why {
			t.Errorf("session/new with %s = %v, want the SDK's invalid params %q", entry, m, why)
		}
	}
	out.close()

	if got := out.text(); !strings.HasPrefix(got, "fleet does not use the MCP server your editor sent (unnamed): ") {
		t.Errorf("text = %q, want the notice naming the null entry unnamed", got)
	}
	stderr := out.r.stderr.String()
	if want := `fleet acp: ignoring 1 MCP server sent by the client (""); fleet's connectors come from the operator's bundle` + "\n"; !strings.Contains(stderr, want) || strings.Count(stderr, "ignoring") != 1 {
		t.Errorf("stderr = %q, want %q and nothing for the refused entries", stderr, want)
	}
	for _, secret := range secrets {
		if strings.Contains(strings.Join(out.lines, "\n"), secret) || strings.Contains(stderr, secret) {
			t.Errorf("%s leaked from a refused entry", secret)
		}
	}
}

// A name is shown on one line, as written: control and format characters
// cannot break the line or reorder it, a long name is cut, and only the first
// maxListedMCPServers names are listed.
func TestMCPServerNamesAreShownSafely(t *testing.T) {
	for in, want := range map[string]string{
		"dummy-mcp":                     "dummy-mcp",
		"two\nlines":                    "two lines",
		"crlf\r\nand\ttab":              "crlf and tab",
		"esc\x1b[31mred\x07":            "esc [31mred",
		"\u202eevil-rtl\u200b":          "evil-rtl",
		"line\u2028separator":           "line separator",
		"  padded  ":                    "padded",
		"":                              "",
		"\n\t\r":                        "",
		"bad\xffutf8":                   "bad\uFFFDutf8",
		strings.Repeat("é", 61):         strings.Repeat("é", 60) + "…",
		strings.Repeat("a", 60):         strings.Repeat("a", 60),
		"[click](https://x.example)`*b": "[click](https://x.example)`*b",
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}

	names := []string{"[click](https://evil.example)", "*bold*", "tick`in", "`edge`", "<b>html</b>", "six", "seven", "eight\nnine"}
	servers := make([]acpsdk.McpServer, 0, len(names))
	for _, n := range names {
		servers = append(servers, stdioServer(n))
	}
	ignored := ignoredMCPServers(servers)
	notice := ignored.notice()
	if !strings.HasSuffix(notice, ": fleet's tools and connectors come from the fleet operator and run on the fleet server.\n\n") ||
		strings.Contains(strings.TrimSuffix(notice, "\n\n"), "\n") {
		t.Errorf("notice = %q, want one line and a blank line", notice)
	}
	if !strings.Contains(notice, "and 3 more)") || strings.Contains(notice, "seven") || strings.Contains(notice, "eight") {
		t.Errorf("notice = %q, want 5 names listed and the other 3 counted", notice)
	}
	if line := ignored.stderrLine(); line != "fleet acp: ignoring 8 MCP servers sent by the client (\"[click](https://evil.example)\", \"*bold*\", \"tick`in\", \"`edge`\", \"<b>html</b>\" and 3 more); fleet's connectors come from the operator's bundle\n" {
		t.Errorf("stderr line = %q", line)
	}
	// Quoted on stderr, a name holding the list's own punctuation cannot
	// blur where it ends.
	if line := ignoredMCPServers([]acpsdk.McpServer{stdioServer(`one", "two)`), stdioServer("three")}).stderrLine(); !strings.Contains(line, `("one\", \"two)", "three");`) {
		t.Errorf("stderr line = %q, want each name quoted", line)
	}
	// A Markdown renderer shows each name as inline code, verbatim: none
	// becomes a link, emphasis or HTML of its own.
	src := []byte(notice)
	doc := goldmark.DefaultParser().Parse(gmtext.NewReader(src))
	var spans []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.CodeSpan:
			var b strings.Builder
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if tx, ok := c.(*ast.Text); ok {
					b.Write(tx.Segment.Value(src))
				}
			}
			spans = append(spans, b.String())
			return ast.WalkSkipChildren, nil
		case *ast.Link, *ast.AutoLink, *ast.Emphasis, *ast.RawHTML:
			t.Errorf("the notice renders a %s: %q", n.Kind(), notice)
		}
		return ast.WalkContinue, nil
	})
	if got, want := strings.Join(spans, "|"), strings.Join(names[:maxListedMCPServers], "|"); got != want {
		t.Errorf("code spans = %q, want the names verbatim %q", got, want)
	}

	// An entry of no variant the SDK knows (the zero value; over the wire an
	// unknown type decodes as HTTP) has no name, and is shown so without a
	// panic: "unnamed" in plain text in the notice, so it cannot pass for a
	// server named `unnamed`, and "" on stderr.
	stderr := &lockedBuffer{}
	h := newHarness(t, harnessOpts{diag: stderr})
	if _, err := h.agent.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/", McpServers: []acpsdk.McpServer{{}}}); err != nil {
		t.Fatalf("session/new with an unknown entry: %v", err)
	}
	if !strings.Contains(stderr.String(), `ignoring 1 MCP server sent by the client ("");`) {
		t.Errorf("stderr = %q", stderr)
	}
	if got := ignoredMCPServers([]acpsdk.McpServer{{}, stdioServer("unnamed")}).notice(); !strings.Contains(got, "(unnamed, `unnamed`)") {
		t.Errorf("notice = %q, want the nameless entry in plain text and the named one in code", got)
	}
}

// The notice is fleet acp's, not the turn's: text.replace still reconciles
// against the turn's own deltas, so it never sees a divergence the notice
// would cause, and the client ends on the notice plus what fleet persisted.
func TestMCPNoticeKeepsTextReplaceReconciled(t *testing.T) {
	notice := ignoredMCPServers([]acpsdk.McpServer{stdioServer("dummy-mcp")}).notice()
	cases := []struct {
		name   string
		deltas []string
		final  string
		want   string
	}{
		{"identical sends nothing more", []string{"Hello ", "there"}, "Hello there", "Hello there"},
		{"extension sends the suffix", []string{"Hel"}, "Hello", "Hello"},
		{"no deltas sends the final text", nil, "Final", "Final"},
		{"divergent draft gets a revised answer", []string{"first draft"}, "better answer", "first draft" + revisedMarker + "better answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
				w.emit("conversation", map[string]any{"id": "conv-r"})
				for _, d := range tc.deltas {
					w.emit("text.delta", map[string]any{"text": d})
				}
				w.emit("text.replace", map[string]any{"text": tc.final})
				w.emit("turn.completed", map[string]any{})
			}})
			if _, err := h.prompt(h.newSessionWith(t, []acpsdk.McpServer{stdioServer("dummy-mcp")}), "x"); err != nil {
				t.Fatal(err)
			}
			if got := h.client.text(); got != notice+tc.want {
				t.Errorf("text = %q, want %q", got, notice+tc.want)
			}
		})
	}
}

// Every way a session's first prompt can go: the notice reaches the client at
// most once per session, from the first prompt submitted to fleet, ahead of
// that prompt's output whatever its outcome. A prompt that ends before it is
// submitted leaves it for the next.
func TestMCPNoticeOnEveryFirstPromptPath(t *testing.T) {
	servers := []acpsdk.McpServer{stdioServer("dummy-mcp")}

	t.Run("queued by fleet", func(t *testing.T) {
		h := newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			w.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w.w, `{"queued":true,"input":{"id":"in-1","position":2,"ahead":0,"state":"queued"},"conversation_id":"conv-q"}`)
		}})
		sid := h.newSessionWith(t, servers)
		for i := 0; i < 2; i++ {
			if r, err := h.prompt(sid, fmt.Sprintf("job %d", i)); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
				t.Fatalf("prompt %d = %+v, %v; want an accepted end_turn", i+1, r, err)
			}
		}
		updates := h.client.since(0)
		if !strings.HasPrefix(firstChunk(updates), noticeOpening) {
			t.Errorf("first update = %q, want the notice ahead of the queued note", firstChunk(updates))
		}
		text := chunkText(updates)
		if strings.Count(text, noticeOpening) != 1 || strings.Count(text, "your message was queued") != 2 {
			t.Errorf("text = %q, want the notice once and both queued notes", text)
		}
	})

	t.Run("a prompt waiting behind the first", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var h *harness
		h = newHarness(t, harnessOpts{turn: func(w *sseWriter, r *http.Request) {
			if h.fleet.nth() > 1 {
				w.emit("conversation", map[string]any{"id": "conv-1"})
				w.emit("text.delta", map[string]any{"text": "; second answer"})
				w.emit("turn.completed", map[string]any{})
				return
			}
			heldTurn(w, r, "conv-1", "turn-1", started, release)
		}})
		sid := h.newSessionWith(t, servers)
		first := h.promptAsync(t, sid, "first job", nil, 1)
		<-started
		second := h.promptAsync(t, sid, "second job", nil, 2)
		close(release)
		for i, done := range []<-chan promptResult{first, second} {
			if r := await(t, done, fmt.Sprintf("prompt %d", i+1)); r.err != nil || r.resp.StopReason != acpsdk.StopReasonEndTurn {
				t.Errorf("prompt %d = %+v, %v; want end_turn", i+1, r.resp, r.err)
			}
		}
		notice := ignoredMCPServers(servers).notice()
		if got := h.client.text(); got != notice+"working and done; second answer" {
			t.Errorf("text = %q, want the notice once, ahead of both answers", got)
		}
	})

	t.Run("a submission that fails", func(t *testing.T) {
		h := newHarness(t, harnessOpts{serverURL: "http://127.0.0.1:1"})
		sid := h.newSessionWith(t, servers)
		for i := 0; i < 2; i++ {
			if _, err := h.prompt(sid, "x"); rpcCode(err) != -32603 {
				t.Fatalf("prompt %d: err = %v, want the daemon-down error", i+1, err)
			}
		}
		// The first attempt was submitted (and failed), so it carried the
		// notice; the retry does not repeat it.
		if got := h.client.text(); got != ignoredMCPServers(servers).notice() {
			t.Errorf("text = %q, want the notice once, from the first attempt", got)
		}
	})

	t.Run("a lost answer and its resend", func(t *testing.T) {
		var h *harness
		h = newHarness(t, harnessOpts{turn: func(w *sseWriter, _ *http.Request) {
			if h.fleet.resent() {
				replayAck(w, "completed", "conv-l")
				return
			}
			w.w.WriteHeader(http.StatusOK) // the answer is lost
		}})
		sid := h.newSessionWith(t, servers)
		if _, err := h.prompt(sid, "book the room"); err == nil {
			t.Fatal("want the lost-answer error")
		}
		if r, err := h.prompt(sid, "book the room"); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
			t.Fatalf("resend = %+v, %v; want the replay's end_turn", r, err)
		}
		text := h.client.text()
		if !strings.HasPrefix(text, noticeOpening) || strings.Count(text, noticeOpening) != 1 || !strings.Contains(text, "fleet already took this message") {
			t.Errorf("text = %q, want the notice once, then the resend's replay note", text)
		}
	})

	t.Run("prompts that never reach fleet leave it for the next", func(t *testing.T) {
		gate := make(chan struct{})
		prevHook := promptTracked
		promptTracked = func(message string) {
			if message == "held" {
				<-gate
			}
		}
		t.Cleanup(func() { promptTracked = prevHook })
		h := newHarness(t, harnessOpts{})
		sid := h.newSessionWith(t, servers)
		// Refused content: invalid params, never tracked.
		if _, err := h.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{acpsdk.ImageBlock("aGk=", "image/png")}}); rpcCode(err) != -32602 {
			t.Fatalf("image prompt: err = %v, want invalid params", err)
		}
		// Cancelled before it got the session: held between arrival and its
		// turn, and cancelled there (Agent.Cancel called as the SDK calls it
		// for session/cancel, so the cancel has landed when it is let go).
		held := h.promptAsync(t, sid, "held", nil, 1)
		if err := h.agent.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		close(gate)
		if r := await(t, held, "the cancelled prompt"); r.err != nil || r.resp.StopReason != acpsdk.StopReasonCancelled {
			t.Fatalf("cancelled prompt = %+v, %v; want cancelled", r.resp, r.err)
		}
		if n := h.fleet.nth(); n != 0 {
			t.Fatalf("%d prompts reached fleet, want none", n)
		}
		if got := h.client.text(); got != "" {
			t.Errorf("text = %q before any prompt was submitted, want nothing", got)
		}
		if _, err := h.prompt(sid, "say hi"); err != nil {
			t.Fatal(err)
		}
		if got := h.client.text(); got != ignoredMCPServers(servers).notice()+"Hello there" {
			t.Errorf("text = %q, want the notice ahead of the first submitted prompt's answer", got)
		}
	})
}

// The notice is a write to stdout, which waits on the client, so a
// session/cancel can land while it is written: after Prompt's own check,
// before the submission. It is honoured there, so the prompt is never
// submitted; the notice has gone out, so the next prompt does not repeat it.
func TestCancelWhileTheMCPNoticeIsWrittenIsNeverSubmitted(t *testing.T) {
	servers := []acpsdk.McpServer{stdioServer("dummy-mcp")}
	h := newHarness(t, harnessOpts{})
	sid := h.newSessionWith(t, servers)
	h.agent.conn = cancelOnNotice{updater: h.agent.conn, cancel: func() {
		_ = h.agent.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: sid})
	}}
	if r, err := h.prompt(sid, "long job"); err != nil || r.StopReason != acpsdk.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v; want cancelled", r, err)
	}
	if n := h.fleet.nth(); n != 0 {
		t.Fatalf("%d prompts reached fleet, want none: the cancel landed before the submission", n)
	}
	if _, err := h.prompt(sid, "say hi"); err != nil {
		t.Fatal(err)
	}
	if got := h.client.text(); got != ignoredMCPServers(servers).notice()+"Hello there" {
		t.Errorf("text = %q, want the notice once, then the next prompt's answer", got)
	}
}

// cancelOnNotice is the Agent's connection with a session/cancel landing
// while the MCP-server notice is being written to the client.
type cancelOnNotice struct {
	updater
	cancel func()
}

func (c cancelOnNotice) SessionUpdate(ctx context.Context, n acpsdk.SessionNotification) error {
	if u := n.Update.AgentMessageChunk; u != nil && u.Content.Text != nil && strings.HasPrefix(u.Content.Text.Text, noticeOpening) {
		c.cancel()
	}
	return c.updater.SessionUpdate(ctx, n)
}

// A session/new the server refuses (the identity check at session/new) opens
// no session, so stderr says nothing about the client's MCP servers: the
// ignoring line is written only once the session opens.
func TestRefusedSessionLogsNoMCPServers(t *testing.T) {
	stderr := &lockedBuffer{}
	h := newHarness(t, harnessOpts{diag: stderr, me: meAnswer(http.StatusForbidden, "text/plain; charset=utf-8", "forbidden\n")})
	if _, err := h.conn.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := h.conn.NewSession(context.Background(), acpsdk.NewSessionRequest{Cwd: "/work", McpServers: []acpsdk.McpServer{
		{Stdio: &acpsdk.McpServerStdio{Name: "refused-canary", Command: "/bin/true", Args: []string{}, Env: []acpsdk.EnvVariable{}}},
	}})
	if rpcCode(err) != -32000 {
		t.Fatalf("session/new err = %v, want auth_required", err)
	}
	if got := stderr.String(); strings.Contains(got, "refused-canary") || strings.Contains(got, "MCP") {
		t.Errorf("stderr = %q, want nothing about the client's MCP servers", got)
	}
}
