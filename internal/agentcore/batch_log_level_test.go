package agentcore

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A critical batch logs at the level its outcome earns: Info when every
// record succeeded, Warn when some failed, and Error only when it made no
// forward progress (#2052).
func TestCriticalBatchLogLevelFollowsOutcome(t *testing.T) {
	withMergePolicy(t)
	for _, tc := range []struct {
		name, result, wantLevel string
	}{
		{"clean", `{"results":[{"deal_id":"1","success":true},{"deal_id":"2","success":true}]}`, "level=INFO"},
		{"partial", `{"results":[{"deal_id":"1","success":true},{"deal_id":"2","success":false}]}`, "level=WARN"},
		{"no progress", `{"results":[{"deal_id":"1","success":false},{"deal_id":"2","success":false}]}`, "level=ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: canaryMergeTool, DealIDs: []string{"1", "2"}})
			o.creditCanary(canaryMergeTool, batchArgs("", "1", "2"))
			args := batchArgs("", "1", "2")
			if blocked, msg := o.checkCriticalTool(canaryMergeTool, "", args); blocked {
				t.Fatalf("batch blocked: %s", msg)
			}
			o.recordToolResult(canaryMergeTool, args, tc.result, true)

			var line string
			for _, l := range strings.Split(buf.String(), "\n") {
				if strings.Contains(l, "Critical batch") {
					line = l
				}
			}
			if !strings.Contains(line, tc.wantLevel) {
				t.Fatalf("critical batch line %q, want %s", line, tc.wantLevel)
			}
		})
	}
}
