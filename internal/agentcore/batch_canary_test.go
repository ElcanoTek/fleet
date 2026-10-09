package agentcore

// Tests for the single-record canary before a multi-record deal_ids batch —
// ported from cutlass internal/agent/orchestration.go (canaryKey / canaryShape
// / creditBatchCanary, cutlass#740.7, #1081; block wording 4a6f3362).

import (
	"fmt"
	"strings"
	"testing"
)

const (
	canaryMergeTool       = "mcp_openx_mcp_ox_merge_deal_domains"
	canaryMergeUploadTool = "mcp_openx_mcp_ox_merge_deal_domains_upload"
	canaryVariantTool     = "mcp_openx_mcp_tunnl_ox_merge_deal_domains"
	canaryDigest          = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
	canaryOtherDigest     = "ff11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
)

// withMergePolicy installs the DSP fixture plus a merge suffix and its staged
// upload twin, aliased (critical_tool_aliases, #1604).
func withMergePolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	p := testFixturePolicy()
	p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, "merge_deal_domains", "merge_deal_domains_upload")
	p.CriticalToolAliases = map[string][]string{"merge_deal_domains": {"merge_deal_domains_upload"}}
	ConfigureAgentPolicy(p)
}

func batchArgs(digest string, ids ...string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	if digest == "" {
		return `{"deal_ids":[` + strings.Join(quoted, ",") + `]}`
	}
	return fmt.Sprintf(`{"deal_ids":[%s],"values_sha256":%q}`, strings.Join(quoted, ","), digest)
}

func successRows(ids ...string) string {
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = fmt.Sprintf(`{"deal_id":%q,"success":true}`, id)
	}
	return `{"results":[` + strings.Join(rows, ",") + `]}`
}

func TestBatchCanary_Gate(t *testing.T) {
	withMergePolicy(t)

	type step struct {
		tool, args, result string // a recorded call (result "" = not executed)
	}
	cases := []struct {
		name     string
		digest   string // approved values_digest ("" = undigested approval)
		canary   []step
		tool     string
		batch    string
		blocked  bool
		wantText string
	}{
		{
			name: "batch of 3 refused without a canary", digest: canaryDigest,
			tool: canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
			blocked: true, wantText: "a one-record batch with the matching digest IS the canary",
		},
		{
			name: "allowed after a one-record batch succeeded with the same digest", digest: canaryDigest,
			canary: []step{{canaryMergeTool, batchArgs(canaryDigest, "1"), successRows("1")}},
			tool:   canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
		},
		{
			name:   "a different digest needs its own canary",
			canary: []step{{canaryMergeTool, batchArgs(canaryDigest, "1"), successRows("1")}},
			tool:   canaryMergeTool, batch: batchArgs(canaryOtherDigest, "1", "2", "3"),
			blocked: true, wantText: "CANARY",
		},
		{
			name: "a failed canary grants nothing", digest: canaryDigest,
			canary: []step{{canaryMergeTool, batchArgs(canaryDigest, "1"), `{"results":[{"deal_id":"1","success":false}]}`}},
			tool:   canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
			blocked: true, wantText: "CANARY",
		},
		{
			name: "a success row for a different record grants nothing", digest: canaryDigest,
			canary: []step{{canaryMergeTool, batchArgs(canaryDigest, "1"), successRows("2")}},
			tool:   canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
			blocked: true, wantText: "CANARY",
		},
		{
			name: "a malformed canary result grants nothing", digest: canaryDigest,
			canary: []step{{canaryMergeTool, batchArgs(canaryDigest, "1"), `{"results":[{"deal_id":"1"}]}`}},
			tool:   canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
			blocked: true, wantText: "CANARY",
		},
		{
			name: "canary through the alias twin covers the batch", digest: canaryDigest,
			canary: []step{{canaryMergeUploadTool, batchArgs(canaryDigest, "1"), successRows("1")}},
			tool:   canaryMergeTool, batch: batchArgs(canaryDigest, "1", "2", "3"),
		},
		{
			name:   "a different operation shape needs its own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","list_type":"allowlist"}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `","list_type":"blocklist"}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "different inline values need their own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values":["safe.example"]}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values":["other.example"]}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "the same inline values (any order/case) ride the canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values":["b.example","A.example"]}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values":["a.example","b.example"]}`,
		},
		{
			name:   "a successful single-record call is a canary",
			canary: []step{{canaryMergeTool, `{"deal_id":"1"}`, `{"success":true}`}},
			tool:   canaryMergeTool, batch: batchArgs("", "1", "2", "3"),
		},
		// The shape is every argument but record addressing and value
		// transport, so a connector's own mode words bind too (Codex on #1712).
		{
			name:   "a dry-run canary does not unlock the real write",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","dry_run":true}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `"}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "a connector-specific mode argument needs its own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","is_excluded":false}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `","is_excluded":true}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "a case-sensitive argument keeps its case",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","namespace":"TenantA"}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `","namespace":"tenanta"}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "a different seat argument needs its own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","member_id":101}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `","member_id":202}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "different per-dimension inline lists need their own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"countries_include":["US"]}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"countries_exclude":["CA"]}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name:   "a different values_file without a digest needs its own canary",
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_file":"a.txt"}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_file":"b.txt"}`,
			blocked: true, wantText: "CANARY",
		},
		{
			name: "record addressing, output verbosity and etag ride the canary", digest: canaryDigest,
			canary: []step{{canaryMergeTool, `{"deal_ids":["1"],"values_sha256":"` + canaryDigest + `","values_file":"canary.txt","verbose":true,"etag":"e1","merge_mode":"add","member_id":101}`, successRows("1")}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"values_sha256":"` + canaryDigest + `","values_file":"batch.txt","verbose":false,"merge_mode":"add","member_id":101}`,
		},
		{
			name:   "a single-record call carrying the same mode arguments is a canary",
			canary: []step{{canaryMergeTool, `{"internal_deal_id":"1","merge_mode":"add","operator":"exclude"}`, `{"success":true}`}},
			tool:   canaryMergeTool, batch: `{"deal_ids":["1","2","3"],"merge_mode":"add","operator":"exclude"}`,
		},
		{
			name: "a batch of one needs no canary", digest: canaryDigest,
			tool: canaryMergeTool, batch: batchArgs(canaryDigest, "2"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: tc.tool, DealIDs: []string{"1", "2", "3"}, ValuesDigest: tc.digest})
			for _, s := range tc.canary {
				if blocked, msg := o.checkCriticalTool(s.tool, "", s.args); blocked {
					t.Fatalf("canary call %s %s blocked: %s", s.tool, s.args, msg)
				}
				o.recordToolResult(s.tool, s.args, s.result, true)
			}
			blocked, msg := o.checkCriticalTool(tc.tool, "", tc.batch)
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v, want %v (msg: %s)", blocked, tc.blocked, msg)
			}
			if tc.wantText != "" && !strings.Contains(msg, tc.wantText) {
				t.Fatalf("block message %q missing %q", msg, tc.wantText)
			}
		})
	}
}

// TestBatchCanary_KeyScope pins the canary key: alias twins on one server share
// it; a client-variant seat of the same server does not.
func TestBatchCanary_KeyScope(t *testing.T) {
	withMergePolicy(t)
	args := batchArgs(canaryDigest, "1")
	if canaryKey(canaryMergeTool, args) != canaryKey(canaryMergeUploadTool, args) {
		t.Fatal("alias twins on the same server must share a canary")
	}
	if canaryKey(canaryMergeTool, args) == canaryKey(canaryVariantTool, args) {
		t.Fatal("a client-variant seat must not ride another seat's canary")
	}
	if canaryKey(canaryMergeTool, args) == canaryKey(canaryMergeTool, batchArgs(canaryOtherDigest, "1")) {
		t.Fatal("a different digest must key apart")
	}
}

// TestBatchCanary_SingleRecordDigestBlockPointsAtOneRecordBatch pins the
// cutlass 4a6f3362 wording: the single-record refusal under a digest-bound
// batch names the one-record batch that IS the canary, and that batch rides.
func TestBatchCanary_SingleRecordDigestBlockPointsAtOneRecordBatch(t *testing.T) {
	withMergePolicy(t)
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: canaryMergeTool, DealIDs: []string{"1", "2"}, ValuesDigest: canaryDigest})
	blocked, msg := o.checkCriticalTool(canaryMergeTool, "", `{"deal_id":"1"}`)
	if !blocked || !strings.Contains(msg, `deal_ids=["1"]`) || !strings.Contains(msg, "IS accepted as the canary") {
		t.Fatalf("blocked=%v msg=%q; want the one-record-batch canary steer", blocked, msg)
	}
	if blocked, msg := o.checkCriticalTool(canaryMergeTool, "", batchArgs(canaryDigest, "1")); blocked {
		t.Fatalf("the one-record batch the message names was refused: %s", msg)
	}
}
