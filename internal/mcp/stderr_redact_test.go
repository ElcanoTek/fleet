package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A stdio connector that echoes its token and an auth header on stderr does
// not get them into the log: each stderr line is a slog record attributed to
// the connector, scrubbed by the log redactor — its secret-named env value by
// literal, the bearer by pattern — and the last unterminated line is flushed
// when the transport is closed (#2074 review). Before, the bytes were copied
// verbatim to os.Stderr, which in broker mode the parent forwards into
// FLEET_LOG_FILE.
func TestStdioConnectorStderrIsLoggedRedacted(t *testing.T) {
	var out lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const token = "acme-live-Pq7Wz2Kd9Lm4Xr8T" // matches no shape pattern; gitleaks:allow — a FAKE test fixture, not a credential
	shaped := "sk-ant-" + strings.Repeat("D", 32)
	script := `echo "starting with token $ACME_API_TOKEN" >&2; ` +
		`printf 'Authorization: Bearer ` + shaped + `\nlast words, no newline' >&2; exit 0`
	tr, err := newStdioTransport("acme", "sh", []string{"-c", script}, map[string]string{"ACME_API_TOKEN": token}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()

	got := out.String()
	for _, leak := range []string{token, shaped} {
		if strings.Contains(got, leak) {
			t.Fatalf("connector stderr leaked %q into the log:\n%s", leak, got)
		}
	}
	raws := strings.Split(strings.TrimSpace(got), "\n")
	lines := make([]string, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("not a JSON record: %q", raw)
		}
		if m["msg"] != logging.StderrLineMessage || m["connector"] != "acme" {
			t.Fatalf("record not attributed to the connector: %v", m)
		}
		line, _ := m["line"].(string)
		lines = append(lines, line)
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "starting with token ") || lines[2] != "last words, no newline" {
		t.Fatalf("stderr lines = %q", lines)
	}
}

// A connector's stderr line written during a call carries that call's
// correlation — conv_id, turn_id, task_id, run_id — not a background
// context's (#2074 review: only the sandbox bridge attributed its stderr).
func TestStdioConnectorStderrDuringACallCarriesItsIDs(t *testing.T) {
	var out lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	script := `read req; echo "Traceback: boom-mcp" >&2; echo '{"jsonrpc":"2.0","id":1,"result":{}}'; sleep 1`
	tr, err := newStdioTransport("acme", "sh", []string{"-c", script}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	ctx := logging.WithTask(logging.WithTurn(t.Context(), "conv-1", "turn-2"), "task-3", "run-4")
	if _, err := tr.Call(ctx, "tools/list", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) != nil || m["line"] != "Traceback: boom-mcp" {
				continue
			}
			if m[logging.KeyConvID] != "conv-1" || m[logging.KeyTurnID] != "turn-2" ||
				m[logging.KeyTaskID] != "task-3" || m[logging.KeyRunID] != "run-4" {
				t.Fatalf("connector stderr not attributed to the call: %v", m)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connector stderr record:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
