package sandbox

import (
	"bytes"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

type lockedSlogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedSlogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedSlogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The bridge's last stderr fragment — written without a newline, as a
// traceback cut short by the kill would be — is logged when the bridge is
// torn down: terminateBridgeLocked Closes the stderr logger once Wait has
// joined the copier (#2074 review).
func TestTerminateBridgeFlushesTheLastStderrLine(t *testing.T) {
	var out lockedSlogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	stderrLog := bridgeStderrLogger(t.Context())
	cmd := exec.Command("sh", "-c", `printf 'Traceback: last words' >&2; sleep 30`)
	cmd.Stderr = stderrLog
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &containerImpl{bridgeCmd: cmd, bridgeStderrLog: stderrLog}
	// Let the fragment reach the logger's buffer before the teardown.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(out.String(), "last words") {
		t.Fatal("a partial line was logged before the bridge ended")
	}
	c.terminateBridgeLocked(t.Context())
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "Traceback: last words") {
		if time.Now().After(deadline) {
			t.Fatalf("the last stderr fragment was never logged: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `"subprocess":"sandbox-bridge"`) {
		t.Fatalf("record not attributed: %s", out.String())
	}
}
