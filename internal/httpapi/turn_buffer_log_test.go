package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/store"
)

type lockedLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// failingTurnPersister blocks inserts on gate (so Emit saturates persistCh),
// then fails every insert and FinishTurn.
type failingTurnPersister struct{ gate chan struct{} }

func (p *failingTurnPersister) CreateTurn(context.Context, string, string, int64) error { return nil }

func (p *failingTurnPersister) InsertTurnEvents(ctx context.Context, _ []store.TurnEvent) error {
	select {
	case <-p.gate:
	case <-ctx.Done():
	}
	return errors.New("insert failed")
}

func (p *failingTurnPersister) FinishTurn(context.Context, string, store.TurnStatus, int64, bool) error {
	return errors.New("finish failed")
}

// The turn buffer's own log lines — channel full, batch insert failed,
// backfill failed, FinishTurn failed — run on the persister goroutine or
// under detached contexts. They carry the turn's conv_id and turn_id and the
// starting request's attributes, not turn_id alone (#2074 review).
func TestTurnBufferLogLinesCarryTheTurnCorrelation(t *testing.T) {
	var out lockedLogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	buf := newTurnBuffer("conv-2074", "turn-2074")
	p := &failingTurnPersister{gate: make(chan struct{})}
	reqCtx := logging.WithRequestID(context.Background(), "req-2074")
	if err := buf.attachPersister(reqCtx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < persistChanDepth+flushBatchSize+50; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Emit("turn.completed", map[string]any{})
	close(p.gate)
	buf.Finish()

	want := map[string]bool{
		"persister channel full; event deferred to Finish backfill": false,
		"persist turn_events failed":                                false,
		"persist backfill failed":                                   false,
		"persist FinishTurn failed":                                 false,
	}
	for _, m := range out.records(t) {
		msg, _ := m["msg"].(string)
		if _, ok := want[msg]; !ok {
			continue
		}
		if m["conv_id"] != "conv-2074" || m["turn_id"] != "turn-2074" || m["request_id"] != "req-2074" {
			t.Fatalf("%q lacks the turn correlation: %v", msg, m)
		}
		want[msg] = true
	}
	for msg, seen := range want {
		if !seen {
			t.Fatalf("no %q line", msg)
		}
	}
}
