package mcpbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A brokered call's failure is logged in the child with the parent's
// correlation — conv_id, turn_id, task_id, run_id, request_id — carried in the
// request frame; an identifier outside the allowlist (user_id) does not cross
// (#2074 review: broker lines named no run or request).
func TestBrokeredCallFailureCarriesTheParentsCorrelation(t *testing.T) {
	var out lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := loopback(t, &fakeBroker{err: errors.New("upstream refused")})
	ctx := logging.WithTurn(context.Background(), "conv-1", "turn-2")
	ctx = logging.WithTask(ctx, "task-3", "run-4")
	ctx = logging.WithRequestID(ctx, "req-5")
	ctx = logging.WithUser(ctx, "alice@example.com")
	if _, _, err := client.CallMCP(ctx, "srv", "tool", nil); err == nil {
		t.Fatal("the backend error did not surface")
	}

	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && strings.HasPrefix(m["msg"].(string), "mcpbroker: call failed") {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("no call-failure line:\n%s", out.String())
	}
	for k, want := range map[string]string{
		logging.KeyConvID: "conv-1", logging.KeyTurnID: "turn-2", logging.KeyTaskID: "task-3",
		logging.KeyRunID: "run-4", logging.KeyRequestID: "req-5",
	} {
		if line[k] != want {
			t.Fatalf("%s = %v, want %q: %v", k, line[k], want, line)
		}
	}
	if _, ok := line[logging.KeyUserID]; ok {
		t.Fatalf("a key outside the allowlist crossed the boundary: %v", line)
	}
}

// The child keeps only allowlisted keys from a frame, whatever a peer sends.
func TestWithCorrelationKeepsOnlyAllowlistedKeys(t *testing.T) {
	ctx := withCorrelation(context.Background(), map[string]string{
		logging.KeyRunID: "run-9", "api_key": "x", logging.KeyUserID: "bob", logging.KeyConvID: strings.Repeat("a", 300),
	})
	if logging.Value(ctx, logging.KeyRunID) != "run-9" || logging.Value(ctx, "api_key") != "" ||
		logging.Value(ctx, logging.KeyUserID) != "" || logging.Value(ctx, logging.KeyConvID) != "" {
		t.Fatalf("attrs = %v", logging.Attrs(ctx))
	}
}
