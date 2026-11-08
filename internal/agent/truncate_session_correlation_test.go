package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// A session log over the size limit is truncated to its first and last
// messages; the truncated file keeps the whole session's metadata — the
// correlation ids above all (#2052) — not just the fields a hand-written copy
// listed.
func TestTruncatedSessionKeepsCorrelationIDs(t *testing.T) {
	s := agentcore.NewLogSession()
	s.SetCorrelation("0af7651916cd43dd8448eb211c80319c", "run-7f3c", "turn-42")
	s.RunOutcome = "blocked"
	s.OutputJSON = `{"ok":true}`
	big := strings.Repeat("x", 4096)
	for range 40 {
		s.AddMessage("assistant", big, nil, nil)
	}
	full, err := marshalLogSession(s)
	if err != nil {
		t.Fatal(err)
	}

	data := truncateLogSession(context.Background(), s, len(full)/4)
	if len(data) >= len(full) {
		t.Fatalf("not truncated: %d bytes of %d", len(data), len(full))
	}
	var got struct {
		TraceID    string `json:"trace_id"`
		RunID      string `json:"run_id"`
		TurnID     string `json:"turn_id"`
		RunOutcome string `json:"run_outcome"`
		OutputJSON string `json:"output_json"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.TraceID != "0af7651916cd43dd8448eb211c80319c" || got.RunID != "run-7f3c" || got.TurnID != "turn-42" ||
		got.RunOutcome != "blocked" || got.OutputJSON != `{"ok":true}` {
		t.Fatalf("truncated session lost metadata: %+v", got)
	}
}
