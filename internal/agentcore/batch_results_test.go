package agentcore

// Tests for the fail-closed handling of malformed per-record results[] and the
// policy reading the raw (unclipped) result text — ported from cutlass
// internal/agent/orchestration.go (malformedDealOutcomes, cutlass#1067).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMalformedDealOutcomes(t *testing.T) {
	cases := []struct {
		name, text string
		want       bool
	}{
		{"empty", "", false},
		{"plain text", "done", false},
		{"no results key", `{"success":true}`, false},
		{"null results", `{"results":null}`, false},
		{"unrelated list shape", `{"results":[{"name":"x"}]}`, false},
		{"well-formed", `{"results":[{"deal_id":"1","success":true}]}`, false},
		{"results not an array", `{"results":{"deal_id":"1"}}`, true},
		{"empty results", `{"results":[]}`, true},
		{"row missing success", `{"results":[{"deal_id":"1"}]}`, true},
		{"row missing deal_id", `{"results":[{"success":true}]}`, true},
		{"success mistyped", `{"results":[{"deal_id":"1","success":"yes"}]}`, true},
		{"row not an object", `{"results":["1"]}`, true},
		{"case-folded key", `{"Results":[{"deal_id":"1"}]}`, true},
		{"object prefix that does not decode", `{"results":[{"deal_id":"1","succ`, true},
		{"over the inspect cap", `{"results":[` + strings.Repeat(" ", maxDealOutcomesBytes) + `]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// recordToolResult consults malformedDealOutcomes only after
			// parseDealOutcomes refused the text; mirror that order.
			_, parsed := parseDealOutcomes(tc.text)
			if got := !parsed && malformedDealOutcomes(tc.text); got != tc.want {
				t.Fatalf("fails closed = %v, want %v (parsed=%v)", got, tc.want, parsed)
			}
		})
	}
}

// TestMalformedBatchResultFailsClosed: a present-but-malformed results[] is a
// failed call — no record discharged, the retry budget charged — never a
// single-call success crediting the input's records.
func TestMalformedBatchResultFailsClosed(t *testing.T) {
	withMergePolicy(t)
	for _, result := range []string{
		`{"results":[{"deal_id":"1"},{"deal_id":"2"}]}`,
		`{"results":"ok"}`,
		`{"results":[]}`,
	} {
		t.Run(result, func(t *testing.T) {
			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: canaryMergeTool, DealIDs: []string{"1", "2"}})
			o.creditCanary(canaryMergeTool, batchArgs("", "1", "2"))
			args := batchArgs("", "1", "2")
			if blocked, msg := o.checkCriticalTool(canaryMergeTool, "", args); blocked {
				t.Fatalf("batch blocked: %s", msg)
			}
			o.recordToolResult(canaryMergeTool, args, result, true)
			if got := o.committedCriticalActions["merge_deal_domains"]; got != 2 {
				t.Fatalf("malformed results discharged records: outstanding=%d, want 2", got)
			}
			if got := o.criticalToolFailureAttempts[retryBudgetKey(canaryMergeTool, hashString(args))]; got != 1 {
				t.Fatalf("retry budget charged %d time(s), want 1", got)
			}
			if !o.auditConfirmed {
				t.Fatal("audit token must stay armed: nothing was discharged")
			}
		})
	}
}

// TestLargeBatchResultReachesPolicyRaw is the clipped-vs-raw regression: a
// 200-record results[] runs past the model-visible ceiling, so the model gets a
// truncation envelope — but the policy must parse the full governed text and
// discharge every succeeded record.
func TestLargeBatchResultReachesPolicyRaw(t *testing.T) {
	withMergePolicy(t)
	const n = 200
	ids := make([]string, n)
	rows := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("DEAL-%04d", i)
		rows[i] = fmt.Sprintf(`{"deal_id":%q,"success":true,"verification_status":"verified","diff":{"added":%d,"removed":0,"note":%q}}`,
			ids[i], i, strings.Repeat("x", 300))
	}
	result := `{"results":[` + strings.Join(rows, ",") + `]}`
	if len(result) <= maxToolOutputBytes() {
		t.Fatalf("fixture result %d bytes must exceed the %d-byte model-visible ceiling", len(result), maxToolOutputBytes())
	}

	policy := NewScheduledPolicy(&LogSession{}, 100, 0, 0)
	o := policy.orch
	registerTyped(t, o, criticalActionStruct{Tool: canaryMergeTool, DealIDs: ids, ValuesDigest: canaryDigest})
	args := batchArgs(canaryDigest, ids...)
	o.creditCanary(canaryMergeTool, args)

	tool := &mcpTool{
		serverName: "openx_mcp",
		tool:       mcp.Tool{Name: "ox_merge_deal_domains", InputSchema: map[string]interface{}{"type": "object"}},
		broker:     &hugeBroker{payload: result},
		policy:     policy,
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-big", Name: tool.Name(), Input: args})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.IsError {
		t.Fatalf("Run returned an error response: %.300s", resp.Content)
	}
	if len(resp.Content) > maxToolOutputBytes() || !strings.Contains(resp.Content, "truncated") {
		t.Fatalf("model-visible result was not clipped (%d bytes)", len(resp.Content))
	}
	if got := o.committedCriticalActions["merge_deal_domains"]; got != 0 {
		t.Fatalf("outstanding=%d after a fully successful %d-record batch, want 0", got, n)
	}
	if got := len(o.dischargedDeals[criticalAliasClassOf("merge_deal_domains")]); got != n {
		t.Fatalf("discharged %d records, want %d", got, n)
	}
}

func TestPolicyResultText(t *testing.T) {
	bounded := fantasy.NewTextResponse("bounded")
	if got := policyResultText(fantasy.NewTextResponse("raw"), bounded); got != "raw" {
		t.Fatalf("text response: policy got %q, want the raw governed text", got)
	}
	media := fantasy.ToolResponse{Type: "image", Data: []byte{1, 2, 3}, Content: "caption"}
	if got := policyResultText(media, bounded); got != "bounded" {
		t.Fatalf("media response: policy got %q, want the bounded text", got)
	}
	huge := fantasy.NewTextResponse("{" + strings.Repeat(" ", maxDealOutcomesBytes+10))
	if got := policyResultText(huge, bounded); len(got) != maxDealOutcomesBytes+1 {
		t.Fatalf("oversized text: policy got %d bytes, want %d", len(got), maxDealOutcomesBytes+1)
	}
}
