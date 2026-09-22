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

// The prod shape (husqvarna 561b0153, ultima 8486611d): the audit declared the
// inline write, the payload went by reference through the upload twin.
func TestCriticalToolAliases_DeclaredInlineExecutedUpload(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasInlineTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(aliasUploadTool, "", `{"slug":"husqvarna","upload_id":"u-1"}`); blocked {
		t.Fatalf("the upload twin of the declared write must ride the audit: %s", msg)
	}
	o.recordToolResult(aliasUploadTool, `{"slug":"husqvarna","upload_id":"u-1"}`, `{"ok":true,"version":{"id":"874"}}`, true)
	assertCleanFinish(t, o)
}

// Symmetric: declaring the upload variant and publishing inline discharges too.
func TestCriticalToolAliases_DeclaredUploadExecutedInline(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: aliasUploadTool}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(aliasInlineTool, "", `{"slug":"brookfield","data":{}}`); blocked {
		t.Fatalf("the inline twin of the declared write must ride the audit: %s", msg)
	}
	o.recordToolResult(aliasInlineTool, `{"slug":"brookfield","data":{}}`, `{"ok":true,"version":{"id":"872"}}`, true)
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
// its twin instead of stacking on it (the brookfield 89afe409 shape: a stale
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
	o.recordToolResult("mcp_pages_deploy_page", `{"slug":"brookfield"}`, `{"ok":true,"version":{"id":"872"}}`, true)
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
