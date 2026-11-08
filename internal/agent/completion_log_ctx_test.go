package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// The completion-predicate and blocked-rule log lines are written by
// scheduledPolicy methods that take no context but hold the run's (runCtx);
// they log through logging.From(p.runCtx), so each carries the run's task_id
// and run_id like every other line of the run (#2074 review).
func TestCompletionPredicateLogLinesCarryTheRunIDs(t *testing.T) {
	var out syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	scriptedRunContext = logging.WithTask(context.Background(), "task-2074", "run-2074")
	t.Cleanup(func() { scriptedRunContext = nil })

	broker := &pagesBroker{calls: map[string]int{}}
	if _, err := blockedRun(t, []struct{ tool, input string }{
		{"confirm_audit", cleanAudit},
		{"mcp_pages_record_refresh_check", `{"slug":"page-a","outcome":"source_unreachable","detail":"no report"}`},
	}, broker); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := map[string]bool{
		"Completion predicate satisfied; skipping the end-of-run verifier and phone-a-friend review": false,
		"Completion blocked rule matched; the run finishes as blocked":                               false,
	}
	for _, raw := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		msg, _ := m["msg"].(string)
		if _, ok := want[msg]; !ok {
			continue
		}
		if m[logging.KeyTaskID] != "task-2074" || m[logging.KeyRunID] != "run-2074" {
			t.Fatalf("%q lacks the run's ids: %s", msg, raw)
		}
		want[msg] = true
	}
	for msg, seen := range want {
		if !seen {
			t.Fatalf("no %q line logged", msg)
		}
	}
}
