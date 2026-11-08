package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

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
