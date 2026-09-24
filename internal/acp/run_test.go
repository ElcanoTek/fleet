package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		exit <- run([]string{"--server", srv.URL, "--email", "bot@example.com", "--token-file", tokenFile}, inR, outW, &stderr)
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
	if code := run([]string{"serve"}, strings.NewReader(""), io.Discard, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code := run([]string{"--nope"}, strings.NewReader(""), io.Discard, &stderr); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	// Only 0 disables the bound: a negative timeout is refused, not taken as
	// "unbounded".
	stderr.Reset()
	if code := run([]string{"--timeout=-1s"}, strings.NewReader(""), io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "negative") {
		t.Errorf("negative timeout: exit %d, stderr %q; want 2 and a refusal", code, stderr.String())
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
