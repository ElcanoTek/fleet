package agentcore

// Tests for critical_tool_aliases (#1604): one critical action exposed under
// two names (Pages' inline update_page_data and its staged-upload twin) is one
// commitment for the audit gate, in both directions, on the same server only.

import (
	"strings"
	"testing"
)

const (
	aliasInlineTool = "mcp_pages_update_page_data"
	aliasUploadTool = "mcp_pages_update_page_data_upload"
	// The same pair on a different server, and on a client variant of the
	// same server: #715's cross-server / cross-variant shapes.
	aliasOtherServerUpload = "mcp_pagesb_update_page_data_upload"
	aliasVariantUpload     = "mcp_pages_client2_update_page_data_upload"
)

// withPagesPolicy installs the DSP fixture plus the Pages write suffixes, with
// aliases (or nil for the no-key baseline), and restores the fixture after.
func withPagesPolicy(t *testing.T, aliases map[string][]string) {
	t.Helper()
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	p := testFixturePolicy()
	p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, "update_page_data", "update_page_data_upload", "deploy_page", "deploy_page_upload")
	p.CriticalToolAliases = aliases
	ConfigureAgentPolicy(p)
}

var pagesAliases = map[string][]string{"update_page_data": {"update_page_data_upload"}}

// assertCleanFinish checks the state a correct publish must leave: nothing
// owed, the token auto-locked, finish allowed, and no abort on the record.
func assertCleanFinish(t *testing.T, o *orchestrationState) {
	t.Helper()
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("declaration still owed after its alias executed: %v", got)
	}
	if o.auditConfirmed {
		t.Fatal("audit token must auto-lock once the declared action is discharged")
	}
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish refused after the aliased write: %v", msgs)
	}
	if aborted, _, executed := o.auditVerdict(); aborted || executed != 1 {
		t.Fatalf("verdict aborted=%t executed=%d, want a clean single write", aborted, executed)
	}
}

// The production shape (a Pages refresh of page A): the audit declared the
// inline write, the payload went by reference through the upload twin.
func TestCriticalToolAliases_DeclaredInlineExecutedUpload(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasInlineTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", `{"slug":"page-a","upload_id":"u-1"}`); blocked {
		t.Fatalf("the upload twin of the declared write must ride the audit: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, `{"slug":"page-a","upload_id":"u-1"}`, `{"ok":true,"version":{"id":"42"}}`, true)
	assertCleanFinish(t, o)
}

// Symmetric: declaring the upload variant and publishing inline discharges too.
func TestCriticalToolAliases_DeclaredUploadExecutedInline(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasUploadTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(aliasInlineTool, "", `{"slug":"page-b","data":{}}`); blocked {
		t.Fatalf("the inline twin of the declared write must ride the audit: %s", msg)
	}
	o.recordToolResult(aliasInlineTool, `{"slug":"page-b","data":{}}`, `{"ok":true,"version":{"id":"43"}}`, true)
	assertCleanFinish(t, o)
}

// Aliases never cross a server or a client variant: the aliased suffix on
// another server is blocked up front and cannot discharge the declaration,
// exactly like a same-suffix call there (#715).
func TestCriticalToolAliases_CrossServerStaysBlocked(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	for _, wrong := range []string{aliasOtherServerUpload, aliasVariantUpload, "mcp_pagesb_update_page_data"} {
		t.Run(wrong, func(t *testing.T) {
			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: aliasInlineTool})
			blocked, msg := o.checkCriticalTool(wrong, "", `{"slug":"x"}`)
			if !blocked || !strings.Contains(msg, "matches no outstanding audited commitment") {
				t.Fatalf("%s must not ride a %s declaration (blocked=%t): %s", wrong, aliasInlineTool, blocked, msg)
			}
			o.recordToolResult(wrong, `{"slug":"x"}`, `{"ok":true}`, true)
			if got := o.unexecutedCommitments(); len(got) != 1 {
				t.Fatalf("a %s success discharged the declaration: outstanding=%v", wrong, got)
			}
		})
	}
}

// Without the key the gate is byte-for-byte what it was: the upload twin is
// blocked and the inline declaration stays owed — the failure the alias ends.
func TestCriticalToolAliases_AbsentKeyKeepsExactNameBinding(t *testing.T) {
	withPagesPolicy(t, nil)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasInlineTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, _ := o.checkCriticalTool(aliasUploadTool, "", `{"slug":"x"}`); !blocked {
		t.Fatal("with no aliases declared, the upload variant must stay bound out of an inline declaration")
	}
	o.recordToolResult(aliasUploadTool, `{"slug":"x"}`, `{"ok":true}`, true)
	if allowed, _ := o.checkFinishEnforcement(); allowed {
		t.Fatal("with no aliases declared, the inline declaration must still be owed")
	}
}

// The record binding carries over to the alias unchanged: a declaration bound
// to one record lets its twin write that record and no other.
func TestCriticalToolAliases_RecordBindingCarriesOver(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: aliasInlineTool, DealID: "page-7"})
	if blocked, _ := o.checkCriticalTool(aliasUploadTool, "", `{"deal_id":"page-8"}`); !blocked {
		t.Fatal("the alias must not widen the record binding")
	}
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", `{"deal_id":"page-7"}`); blocked {
		t.Fatalf("the alias on the bound record must ride: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, `{"deal_id":"page-8"}`, `{"ok":true}`, true)
	if got := o.unexecutedCommitments(); len(got) != 1 {
		t.Fatalf("a write to another record discharged the bound declaration: %v", got)
	}
	o.recordToolResult(aliasUploadTool, `{"deal_id":"page-7"}`, `{"ok":true}`, true)
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("the bound record's aliased write did not discharge: %v", got)
	}
}

// A batch approval (deal_ids + values_digest) binds a batch sent through the
// alias exactly as it binds the declared tool, and each record discharges once.
func TestCriticalToolAliases_BatchBindingCarriesOver(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: aliasInlineTool, DealIDs: []string{"a", "b"}, ValuesDigest: "D1"})
	for _, tc := range []struct{ name, args string }{
		{"unapproved record", `{"deal_ids":["a","c"],"values_sha256":"d1"}`},
		{"wrong digest", `{"deal_ids":["a","b"],"values_sha256":"d2"}`},
	} {
		if blocked, _ := o.checkCriticalTool(aliasUploadTool, "", tc.args); !blocked {
			t.Fatalf("%s: the alias must not escape the batch binding", tc.name)
		}
	}
	args := `{"deal_ids":["a","b"],"values_sha256":"d1"}`
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", args); blocked {
		t.Fatalf("the approved batch through the alias must ride: %s", msg)
	}
	result := `{"results":[{"deal_id":"a","success":true},{"deal_id":"b","success":true}]}`
	o.recordToolResult(aliasUploadTool, args, result, true)
	o.recordToolResult(aliasInlineTool, args, result, true) // the twin reporting the same records again
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("batch through the alias left records owed: %v", got)
	}
	if o.criticalExecutedCount != 1 {
		t.Fatalf("criticalExecutedCount = %d, want 1: the twin's echo of done records is not new progress", o.criticalExecutedCount)
	}
}

// A re-audit that switches the transport supersedes the stale declaration of
// its twin instead of stacking on it (the page B production shape: a stale
// deploy_page_upload declaration outlived the publish).
func TestCriticalToolAliases_ReauditSwitchingVariantSupersedes(t *testing.T) {
	withPagesPolicy(t, map[string][]string{"deploy_page": {"deploy_page_upload"}})
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: "mcp_pages_deploy_page_upload"}}, nil); resp.IsError {
		t.Fatalf("first audit should pass: %s", resp.Content)
	}
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: "mcp_pages_deploy_page"}}, nil); resp.IsError {
		t.Fatalf("re-audit should pass: %s", resp.Content)
	}
	if got := o.outstandingCommitmentSummary(); len(got) != 1 || !strings.HasPrefix(got[0], "mcp_pages_deploy_page ") {
		t.Fatalf("re-audit must leave exactly the fresh declaration, got %v", got)
	}
	o.recordToolResult("mcp_pages_deploy_page", `{"slug":"page-b"}`, `{"ok":true,"version":{"id":"43"}}`, true)
	assertCleanFinish(t, o)
}

// An inline write blocked BEFORE the audit sits in the pending list; when the
// audited write then goes out through the upload twin, that pending entry is
// done — finish must not demand the inline call as well.
func TestCriticalToolAliases_PendingBlockedTwinIsDischarged(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if blocked, _ := o.checkCriticalTool(aliasInlineTool, "", `{"slug":"x","data":{}}`); !blocked {
		t.Fatal("a critical call before the audit must be blocked")
	}
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasInlineTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	o.recordToolResult(aliasUploadTool, `{"slug":"x","upload_id":"u-1"}`, `{"ok":true}`, true)
	if len(o.pendingCriticalActions) != 0 {
		t.Fatalf("the blocked inline call is still pending after its upload twin published: %v", o.pendingCriticalActions)
	}
	assertCleanFinish(t, o)
}

// The legacy free-text audit honours aliases like it honours substitutes:
// suffix-level, since a free-text declaration carries no server identity.
func TestCriticalToolAliases_LegacyAuditDischargesByAlias(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, nil, []string{aliasInlineTool}); resp.IsError {
		t.Fatalf("legacy audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", `{}`); blocked {
		t.Fatalf("legacy declaration must authorize its alias: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, `{}`, `{"ok":true}`, true)
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("legacy declaration still owed after its alias executed: %v", got)
	}
}

// Bundle-load validation: every member must be a critical suffix, entries that
// share a member merge into one class, and an entry left with one member is
// dropped — so a typo can neither open a path around the gate nor alias a
// tool to itself.
func TestConfigureAgentPolicy_CriticalToolAliasClasses(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"update_page_data", "update_page_data_upload", "update_page_data_v2", "deploy_page", "deploy_page_upload"},
		CriticalToolAliases: map[string][]string{
			"update_page_data":        {"update_page_data_upload", " not_critical_tool "},
			"update_page_data_v2":     {"update_page_data_upload"}, // shares a member: merges
			"deploy_page":             {"deploy_page_uploda"},      // typo: nothing valid left
			"deploy_page_upload":      {"deploy_page_upload"},      // itself only
			"not_critical_either_key": {"deploy_page"},
		},
	})
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"update_page_data", "update_page_data_upload", true},
		{"update_page_data_upload", "update_page_data", true},
		{"update_page_data", "update_page_data_v2", true}, // transitive via the shared member
		{"update_page_data", "not_critical_tool", false},
		{"deploy_page", "deploy_page_upload", false},
		{"deploy_page", "not_critical_either_key", false},
		{"update_page_data", "update_page_data", false}, // a suffix is not its own alias
		{"update_page_data", "deploy_page", false},
	} {
		if got := criticalAliasesEquivalent(tc.a, tc.b); got != tc.want {
			t.Errorf("criticalAliasesEquivalent(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
	if got := criticalAliasClassOf("update_page_data_v2"); got != criticalAliasClassOf("update_page_data_upload") || got != "update_page_data" {
		t.Errorf("class key = %q, want the class's smallest member update_page_data for every member", got)
	}
	if got := criticalAliasClassOf("deploy_page"); got != "deploy_page" {
		t.Errorf("an unaliased suffix keys its own batch ledger, got %q", got)
	}
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"update_page_data", "update_page_data_upload"}})
	if criticalAliasesEquivalent("update_page_data", "update_page_data_upload") {
		t.Error("aliases must be replaced, not merged, by the next ConfigureAgentPolicy")
	}
}

// One audit may approve several batches under one approval key, each under its
// own values_digest. The key held a single digest slot, so the last
// declaration's digest replaced the first's and the first batch was falsely
// refused. That already happened for two batches of one tool, and #1604 extended
// it to twins, whose ledgers share the alias class key (the tester's probe on
// PR #1606). Each batch still rides only under its own records and digest.
func TestBatchApprovalsKeepEveryDeclaredDigest(t *testing.T) {
	check := func(t *testing.T, o *orchestrationState, tool, args string, wantBlocked bool) {
		t.Helper()
		if blocked, msg := o.checkCriticalTool(tool, "", args); blocked != wantBlocked {
			t.Fatalf("%s %s: blocked=%t (%s), want %t", tool, args, blocked, msg, wantBlocked)
		}
	}
	t.Run("two batches of one tool", func(t *testing.T) {
		o := newOrchStateForTest()
		registerTyped(t, o,
			criticalActionStruct{Tool: typedCreateToolA, DealIDs: []string{"a", "b"}, ValuesDigest: "D1"},
			criticalActionStruct{Tool: typedCreateToolA, DealIDs: []string{"c", "d"}, ValuesDigest: "D2"})
		check(t, o, typedCreateToolA, `{"deal_ids":["a","b"],"values_sha256":"d1"}`, false)
		check(t, o, typedCreateToolA, `{"deal_ids":["c","d"],"values_sha256":"d2"}`, false)
		check(t, o, typedCreateToolA, `{"deal_ids":["a","b"],"values_sha256":"d2"}`, true) // another batch's digest
		check(t, o, typedCreateToolA, `{"deal_ids":["a","b"],"values_sha256":"d3"}`, true) // no batch's digest
		check(t, o, typedCreateToolA, `{"deal_ids":["a","c"],"values_sha256":"d1"}`, true) // records of two batches
	})
	t.Run("one batch per twin", func(t *testing.T) {
		withPagesPolicy(t, pagesAliases)
		o := newOrchStateForTest()
		registerTyped(t, o,
			criticalActionStruct{Tool: aliasInlineTool, DealIDs: []string{"a", "b"}, ValuesDigest: "D1"},
			criticalActionStruct{Tool: aliasUploadTool, DealIDs: []string{"c", "d"}, ValuesDigest: "D2"})
		check(t, o, aliasInlineTool, `{"deal_ids":["a","b"],"values_sha256":"d1"}`, false)
		check(t, o, aliasUploadTool, `{"deal_ids":["c","d"],"values_sha256":"d2"}`, false)
		check(t, o, aliasUploadTool, `{"deal_ids":["a","b"],"values_sha256":"d2"}`, true)
		check(t, o, aliasOtherServerUpload, `{"deal_ids":["c","d"],"values_sha256":"d2"}`, true)
	})
}

// validate-config's view of alias problems is the boot path's: the same
// members and entries ConfigureAgentPolicy would ignore.
func TestCriticalToolAliasProblems(t *testing.T) {
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"update_page_data", "update_page_data_upload"},
		CriticalToolAliases: map[string][]string{
			"update_page_data": {"update_page_data_uplaod"},
			"send_email":       {"send_template_email"}, // base suffixes count as critical
		},
	}
	problems := CriticalToolAliasProblems(p)
	joined := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(joined, `member "update_page_data_uplaod"`) || !strings.Contains(joined, `entry "update_page_data"`) {
		t.Fatalf("problems = %v, want the typo'd member and the entry it empties", problems)
	}
	p.CriticalToolAliases = map[string][]string{"update_page_data": {"update_page_data_upload"}}
	if problems := CriticalToolAliasProblems(p); len(problems) != 0 {
		t.Fatalf("a valid declaration reported problems: %v", problems)
	}
}

// Two disjoint batches, one per twin, in one audit share the alias-class
// approval and discharge ledgers. A server response to the FIRST batch's call
// that reports a success for a record of the SECOND batch must not discharge
// the second commitment: that call carried only the first batch's records and
// digest, so the second critical action never ran (Codex review on PR #1606).
func TestCriticalToolAliases_BatchResultBoundToInvokedBatch(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: aliasInlineTool, DealIDs: []string{"a"}, ValuesDigest: "D1"},
		criticalActionStruct{Tool: aliasUploadTool, DealIDs: []string{"b"}, ValuesDigest: "D2"})
	args := `{"deal_ids":["a"],"values_sha256":"d1"}`
	if blocked, msg := o.checkCriticalTool(aliasInlineTool, "", args); blocked {
		t.Fatalf("the first batch must ride its own declaration: %s", msg)
	}
	o.recordToolResult(aliasInlineTool, args,
		`{"results":[{"deal_id":"a","success":true},{"deal_id":"b","success":true}]}`, true)
	got := o.unexecutedCommitments()
	if len(got) != 1 || !strings.HasSuffix(got[0], "update_page_data_upload") {
		t.Fatalf("the second batch's commitment was discharged by the first batch's call: outstanding=%v", got)
	}
	if allowed, _ := o.checkFinishEnforcement(); allowed {
		t.Fatal("finish allowed with the second critical action never executed")
	}
	// The second batch's own call still discharges it.
	args2 := `{"deal_ids":["b"],"values_sha256":"d2"}`
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", args2); blocked {
		t.Fatalf("the second batch must still ride its own declaration: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, args2, `{"results":[{"deal_id":"b","success":true}]}`, true)
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("second batch left owed after its own call: %v", got)
	}
}

// The digest requirement belongs to each declaration, not to the alias class:
// an undigested batch for one record set is not refused because a twin's batch
// in the same audit declared a values_digest for ANOTHER record set, while the
// digest-bound batch keeps requiring its digest (Codex review on PR #1606).
func TestCriticalToolAliases_DigestRequirementScopedPerBatch(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: aliasInlineTool, DealIDs: []string{"a", "b"}},
		criticalActionStruct{Tool: aliasUploadTool, DealIDs: []string{"c", "d"}, ValuesDigest: "D2"})
	if blocked, msg := o.checkCriticalTool(aliasInlineTool, "", `{"deal_ids":["a","b"]}`); blocked {
		t.Fatalf("the undigested batch was refused by the twin's digest: %s", msg)
	}
	for _, args := range []string{`{"deal_ids":["c","d"]}`, `{"deal_ids":["c","d"],"values_sha256":"d9"}`} {
		if blocked, _ := o.checkCriticalTool(aliasUploadTool, "", args); !blocked {
			t.Fatalf("the digest-bound batch must still require its digest: %s", args)
		}
	}
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", `{"deal_ids":["c","d"],"values_sha256":"d2"}`); blocked {
		t.Fatalf("the digest-bound batch with its digest must ride: %s", msg)
	}
}

// A pending entry left by a call blocked before the audit is cleared by an
// alias only when the alias wrote the SAME record: an inline write of record A
// blocked pre-audit is not done because the upload twin later wrote record B,
// and must neither leave the pending list nor be recorded as completed (Codex
// review on PR #1606). Whether finish then passes is the audit's call, not the
// alias's: the audit declared only B, and without aliases an undeclared
// pending call is likewise not owed once the token auto-locks (ADR-0034).
func TestCriticalToolAliases_PendingTwinOfAnotherRecordStaysPending(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if blocked, _ := o.checkCriticalTool(aliasInlineTool, "", `{"deal_id":"page-a","data":{}}`); !blocked {
		t.Fatal("a critical call before the audit must be blocked")
	}
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasUploadTool, DealID: "page-b"}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	args := `{"deal_id":"page-b","upload_id":"u-1"}`
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", args); blocked {
		t.Fatalf("the declared upload must ride: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, args, `{"ok":true}`, true)
	if len(o.pendingCriticalActions) != 1 || o.pendingCriticalActions[0].toolName != aliasInlineTool {
		t.Fatalf("record A's blocked inline write was cleared by the twin's write of record B: pending=%v", o.pendingCriticalActions)
	}
	if len(o.completedCriticalActions) != 0 {
		t.Fatalf("record A's blocked write was recorded as completed: %v", o.completedCriticalActions)
	}
}

// The batch discharge ledger dedups a record per server/variant: two aliased
// batch calls on DIFFERENT servers for the same record id are two actions,
// each owed under its own server-bound commitment, and the second server's
// success must not be skipped as an echo of the first (Codex review on PR #1606).
func TestCriticalToolAliases_BatchLedgerKeyedByServer(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: aliasInlineTool, DealIDs: []string{"a"}},
		criticalActionStruct{Tool: aliasOtherServerUpload, DealIDs: []string{"a"}})
	args := `{"deal_ids":["a"]}`
	result := `{"results":[{"deal_id":"a","success":true}]}`
	for _, tool := range []string{aliasInlineTool, aliasOtherServerUpload} {
		if blocked, msg := o.checkCriticalTool(tool, "", args); blocked {
			t.Fatalf("%s must ride its own declaration: %s", tool, msg)
		}
		o.recordToolResult(tool, args, result, true)
	}
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("the second server's write of the same record id was skipped: outstanding=%v", got)
	}
	if o.criticalExecutedCount != 2 {
		t.Fatalf("criticalExecutedCount = %d, want 2: two servers, two writes", o.criticalExecutedCount)
	}
}

// A re-audit that corrects a refused batch supersedes the stale batch
// commitment only when it re-declares every record the stale one still owes.
// Inline batch A/B plus upload batch C; a blocked upload for A/C is refused by
// both; re-auditing the upload for A/C must not retire the A/B commitment,
// because B is not in the correction and would never be owed again (Codex
// review on PR #1606).
func TestCriticalToolAliases_PartialSupersedeKeepsUncoveredRecords(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{
		{Tool: aliasInlineTool, DealIDs: []string{"a", "b"}},
		{Tool: aliasUploadTool, DealIDs: []string{"c"}},
	}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, _ := o.checkCriticalTool(aliasUploadTool, "", `{"deal_ids":["a","c"]}`); !blocked {
		t.Fatal("a batch spanning two declarations must be blocked")
	}
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasUploadTool, DealIDs: []string{"a", "c"}}}, nil); resp.IsError {
		t.Fatalf("re-audit should pass: %s", resp.Content)
	}
	args := `{"deal_ids":["a","c"]}`
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", args); blocked {
		t.Fatalf("the re-audited batch must ride: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, args, `{"results":[{"deal_id":"a","success":true},{"deal_id":"c","success":true}]}`, true)
	if allowed, _ := o.checkFinishEnforcement(); allowed {
		t.Fatalf("finish allowed with record B never written: outstanding=%v", o.outstandingCommitmentSummary())
	}
	var owesB bool
	for _, s := range o.outstandingCommitmentSummary() {
		owesB = owesB || strings.HasSuffix(s, "b)")
	}
	if !owesB {
		t.Fatalf("record B is no longer owed: %v", o.outstandingCommitmentSummary())
	}
}

// CriticalActionKey: one key per action — server/variant prefix plus the alias
// class — so same-server twins share it and nothing else does.
func TestCriticalActionKey(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	inline, ok := CriticalActionKey(aliasInlineTool)
	upload, uploadOK := CriticalActionKey(aliasUploadTool)
	if !ok || !uploadOK || upload != inline {
		t.Fatalf("same-server twins must share a key: %+v vs %+v", inline, upload)
	}
	for _, other := range []string{aliasOtherServerUpload, aliasVariantUpload, "mcp_pagesb_update_page_data"} {
		if key, _ := CriticalActionKey(other); key == inline {
			t.Errorf("%s must not share the key of %s", other, aliasInlineTool)
		}
	}
	if key, critical := CriticalActionKey("mcp_pages_get_page_data"); critical {
		t.Errorf("a non-critical tool keyed %+v", key)
	}
	withPagesPolicy(t, nil)
	inline, _ = CriticalActionKey(aliasInlineTool)
	if upload, _ := CriticalActionKey(aliasUploadTool); upload == inline {
		t.Errorf("without aliases the two spellings are two actions, both keyed %+v", inline)
	}
}

// A critical suffix that ends in "_"+another alias class must not key onto
// that class's twin one segment up the prefix: mcp_x_bulk_create_deal (suffix
// bulk_create_deal) and mcp_x_bulk_create_deal_upload (prefix mcp_x_bulk,
// suffix create_deal_upload, class create_deal) are different actions — a
// joined "prefix_class" string would collide them; sameAliasedTool agrees.
func TestCriticalActionKeyDoesNotCollideAcrossThePrefixBoundary(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	p := testFixturePolicy()
	p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, "create_deal", "create_deal_upload", "bulk_create_deal")
	p.CriticalToolAliases = map[string][]string{"create_deal": {"create_deal_upload"}}
	ConfigureAgentPolicy(p)
	a, b := "mcp_x_bulk_create_deal", "mcp_x_bulk_create_deal_upload"
	ka, _ := CriticalActionKey(a)
	kb, _ := CriticalActionKey(b)
	if ka == kb {
		t.Fatalf("%s and %s share key %+v", a, b, ka)
	}
	if sameAliasedTool(a, b) {
		t.Fatalf("sameAliasedTool(%s, %s) = true, want false", a, b)
	}
}
