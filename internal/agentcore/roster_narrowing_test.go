package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Tests for the exhaustive Gate-2 behind a narrowed roster (#1603).

// pagesRosterCatalog is a Pages-shaped catalog: the managed-data tools a
// refresh needs, a layout tool it must never reach, a named-account seat of the
// same server, and an always-on helper server the refresh does not need.
func pagesRosterCatalog() []mcp.ServerTool {
	return []mcp.ServerTool{
		variantTool("pages", "get_page_data"),
		variantTool("pages", "update_page_data_upload"),
		variantTool("pages", "deploy_page_upload"),
		variantTool("pages_acct", "get_page_data"),
		variantTool("pages_acct", "deploy_page_upload"),
		variantTool("fast_io", "upload"),
		variantTool("fast_io", "share"),
	}
}

// rosterDeniesAll stands in for the scheduled driver's never-matching deny
// entry (scheduledrun rosterNarrowingDeniesAll); any name no catalog tool
// carries behaves the same.
const rosterDeniesAll = "__roster_narrowing_denies_all_tools__"

// The narrowed allowlist the scheduled driver derives (scheduledrun
// narrowedAllowlist) for required_tools [mcp_pages_get_page_data,
// mcp_pages_update_page_data_upload] over pagesRosterCatalog: the required
// server's own entry, and an explicit deny entry for every other catalog
// server — the named-account seat included, since required_tools names none of
// its tools in the seat's own form.
var pagesNarrowed = mcpAllowlist{
	"pages":      {"get_page_data", "update_page_data_upload"},
	"pages_acct": {rosterDeniesAll},
	"fast_io":    {rosterDeniesAll},
}

func TestExclusiveAllowlistRegistersOnlyTheNarrowedTools(t *testing.T) {
	native := []fantasy.AgentTool{fantasy.NewAgentTool("task_tracker", "track", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})}
	registered, roster, err := buildFantasyToolsWithRoster(native, pagesRosterCatalog(), &fakeBroker{}, pagesNarrowed, passPolicy{}, nil, nil,
		toolBuildConfig{includeConfirmAudit: true, exclusiveAllowlist: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(roster.directMCP, ",")
	want := "mcp_pages_get_page_data,mcp_pages_update_page_data_upload"
	if got != want {
		t.Fatalf("registered MCP tools = %s, want %s", got, want)
	}
	if strings.Join(roster.mcp, ",") != want {
		t.Fatalf("roster.mcp = %v, want the registered set %s", roster.mcp, want)
	}
	names := toolNamesOf(registered)
	if !names["task_tracker"] || !names[toolNameConfirmAudit] {
		t.Fatalf("native tools and confirm_audit must survive a narrowed roster: %v", names)
	}
}

// The exhaustive Gate-2 is an EXACT lookup. The driver's explicit deny entries
// cover only the servers it saw at dispatch, so a server registered later
// without its own entry — a `<server>_<account>` seat from
// mcp_load_servers(client=…), or an independent server whose name merely
// extends a narrowed one — must register nothing. Through the ordinary keying
// rule both would resolve to the pages entry and register get_page_data.
func TestExclusiveAllowlistIsAnExactLookup(t *testing.T) {
	catalog := append(pagesRosterCatalog(),
		variantTool("pages_acct2", "get_page_data"),
		variantTool("pages_archive", "get_page_data"),
		variantTool("notes", "list_notes"),
	)
	_, narrowed, err := buildFantasyToolsWithRoster(nil, catalog, &fakeBroker{}, pagesNarrowed, passPolicy{}, nil, nil, toolBuildConfig{exclusiveAllowlist: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(narrowed.directMCP, ","); got != "mcp_pages_get_page_data,mcp_pages_update_page_data_upload" {
		t.Fatalf("a server without its own entry registered tools under a narrowed roster: %s", got)
	}

	// Without the exhaustive flag the same allowlist keeps the ordinary rule:
	// the seat and the prefix-named server inherit the pages entry, and a
	// server with no entry at all is open.
	_, open, err := buildFantasyToolsWithRoster(nil, catalog, &fakeBroker{}, pagesNarrowed, passPolicy{}, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := "mcp_notes_list_notes,mcp_pages_acct2_get_page_data,mcp_pages_archive_get_page_data,mcp_pages_get_page_data,mcp_pages_update_page_data_upload"
	if got := strings.Join(open.directMCP, ","); got != want {
		t.Fatalf("baseline Gate-2 changed: %s, want %s", got, want)
	}
}

// A narrowed roster is as deterministic as any other: the same inputs
// serialize byte-identically, in catalog order (docs/PROMPT-CACHE-CONTRACT.md).
func TestExclusiveAllowlistPrefixDeterministic(t *testing.T) {
	build := func() []byte {
		tools, _, err := buildFantasyToolsWithRoster(nil, pagesRosterCatalog(), &fakeBroker{}, pagesNarrowed, passPolicy{}, nil, nil, toolBuildConfig{exclusiveAllowlist: true})
		if err != nil {
			t.Fatal(err)
		}
		return serializeToolPrefix(t, tools)
	}
	want := build()
	for i := 0; i < 32; i++ {
		if got := build(); string(got) != string(want) {
			t.Fatalf("narrowed tool prefix is non-deterministic (iteration %d)", i)
		}
	}
}

// End to end through Run: one breadcrumb naming how many MCP tools the narrowed
// roster registered, a live-registry section listing exactly those (a seat and
// a prefix-named server the driver never saw register nothing), and a call to a
// tool the narrowing removed answered in-band as "tool not found" — the layout
// tool is not reachable at all.
func TestRun_MCPRosterNarrowing(t *testing.T) {
	var mu sync.Mutex
	var systemPrompt, toolResult string
	calls := 0
	model := &namedMockModel{name: "roster-narrowing", mockModel: mockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		for _, m := range call.Prompt {
			for _, part := range m.Content {
				if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && m.Role == fantasy.MessageRoleSystem {
					systemPrompt = tp.Text
				}
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
						toolResult = e.Error.Error()
					}
				}
			}
		}
		if calls == 1 {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "mcp_pages_deploy_page_upload", ToolCallInput: `{}`})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			}, nil
		}
		return streamTextThenFinish("done"), nil
	}}}
	broker := &fakeBroker{}
	session := NewLogSession()
	_, err := Run(context.Background(), ModeInteractive,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, Allowlist: pagesNarrowed, MCPRosterNarrowing: "required_tools_only"},
		Deps{
			Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("refresh")}, label: "narrow"},
			Policy:     NewInteractivePolicy(0, 0, nil, nil),
			Model:      model,
			MCPCatalog: append(pagesRosterCatalog(), variantTool("pages_acct2", "get_page_data"), variantTool("pages_archive", "get_page_data")),
			MCPBroker:  broker,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	crumbs := 0
	for _, m := range session.Messages {
		if strings.HasPrefix(m.Content, "[roster] required_tools_only: 2 mcp tools registered") {
			crumbs++
		}
	}
	if crumbs != 1 {
		t.Fatalf("want exactly one [roster] breadcrumb naming 2 MCP tools, got %d", crumbs)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(systemPrompt, liveRegistryHeading) || !strings.Contains(systemPrompt, "mcp_pages_update_page_data_upload") ||
		strings.Contains(systemPrompt, "deploy_page_upload") || strings.Contains(systemPrompt, "mcp_fast_io") ||
		strings.Contains(systemPrompt, "mcp_pages_acct") || strings.Contains(systemPrompt, "mcp_pages_archive") {
		t.Fatalf("live registry must list exactly the narrowed roster:\n%s", systemPrompt)
	}
	if !strings.Contains(toolResult, "tool not found: mcp_pages_deploy_page_upload") {
		t.Fatalf("a call to a narrowed-away tool must answer 'tool not found', got %q", toolResult)
	}
	if broker.lastTool != "" {
		t.Fatalf("the narrowed-away tool reached the broker: %s/%s", broker.lastServer, broker.lastTool)
	}
}

// narrowedAuditInput is a complete confirm_audit(success=true) envelope typed
// to the given critical tools.
func narrowedAuditInput(t *testing.T, tools ...string) string {
	t.Helper()
	actions := make([]criticalActionStruct, 0, len(tools))
	for _, tool := range tools {
		actions = append(actions, criticalActionStruct{Tool: tool})
	}
	raw, err := json.Marshal(confirmAuditInput{
		Success: true, Reasoning: "checked the deal sheet", ArtifactsChecked: []string{"workspace/deals.csv"},
		WorkflowSectionsChecked: []string{"verify"}, CriticalActions: actions, SendContractChecked: true,
		AttachmentsChecked: []string{}, RemainingRisks: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// End to end through a scheduled Run: under a narrowed roster, confirm_audit
// refuses a typed approval for an MCP tool the run did not register — the
// narrowed-away twin, and a named-account seat the exact Gate-2 never
// registers. Either would answer "tool not found", so the commitment could
// never be discharged and finish would be refused until the run failed. The
// refusal registers nothing; the re-audit naming the registered tool is
// accepted, the call executes and discharges it, and the run finishes.
func TestRun_NarrowedRosterRefusesAuditOfUnregisteredTool(t *testing.T) {
	steps := []struct{ tool, input string }{
		{toolNameConfirmAudit, narrowedAuditInput(t, "mcp_dsp_create_curated_deal", "mcp_dsp_acct2_create_deal")},
		{toolNameConfirmAudit, narrowedAuditInput(t, "mcp_dsp_create_deal")},
		{"mcp_dsp_create_deal", `{}`},
	}
	var mu sync.Mutex
	results := map[string]string{}
	calls := 0
	model := &namedMockModel{name: "narrowed-audit", mockModel: mockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range call.Prompt {
			for _, part := range m.Content {
				tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					continue
				}
				if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
					results[tr.ToolCallID] = out.Text
				}
				if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
					results[tr.ToolCallID] = "ERROR: " + out.Error.Error()
				}
			}
		}
		if calls < len(steps) {
			step, id := steps[calls], fmt.Sprintf("c%d", calls)
			calls++
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: step.tool, ToolCallInput: step.input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			}, nil
		}
		calls++
		return streamTextThenFinish("done"), nil
	}}}
	broker := &fakeBroker{}
	policy := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	res, err := Run(context.Background(), ModeScheduled,
		RunConfig{
			EnvPrefix: CanonicalEnvPrefix, IncludeConfirmAudit: true, MCPRosterNarrowing: "required_tools_only",
			Allowlist: mcpAllowlist{"dsp": {"create_deal"}},
		},
		Deps{
			Input:  historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("create the deal")}, label: "narrow-audit"},
			Policy: policy,
			Model:  model,
			MCPCatalog: []mcp.ServerTool{
				variantTool("dsp", "create_deal"),
				variantTool("dsp", "create_curated_deal"),
				variantTool("dsp_acct2", "create_deal"),
			},
			MCPBroker:  broker,
			LogSession: NewLogSession(),
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	refusal := results["c0"]
	if !strings.HasPrefix(refusal, "ERROR: Audit Rejected") || !strings.Contains(refusal, "mcp_dsp_create_curated_deal, mcp_dsp_acct2_create_deal") ||
		!strings.Contains(refusal, "cannot call") {
		t.Fatalf("an approval for unregistered tools must be refused, naming them; got %q", refusal)
	}
	if got := results["c1"]; !strings.HasPrefix(got, "Audit Confirmed") {
		t.Fatalf("an approval for a registered tool must be accepted; got %q", got)
	}
	if got := results["c2"]; got != "called dsp/create_deal" {
		t.Fatalf("the approved call must execute; got %q", got)
	}
	if res.Cancelled || calls != len(steps)+1 {
		t.Fatalf("the run must finish once the approved call discharged: cancelled=%v model calls=%d", res.Cancelled, calls)
	}
	o := policy.orchestration()
	o.mu.Lock()
	defer o.mu.Unlock()
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("outstanding commitments after the run: %v", missing)
	}
	for _, tc := range o.typedCommitments {
		if tc.tool != "mcp_dsp_create_deal" {
			t.Fatalf("the refused audit registered a commitment: %s", tc.describe())
		}
	}
}

// The roster check is scoped to a narrowed run: without one, confirm_audit
// still registers a typed action whatever the roster (the behavior before
// #1603), and a narrowed run that registered no MCP tool refuses every typed
// MCP action.
func TestTypedAuditRosterCheckOnlyUnderNarrowing(t *testing.T) {
	open := newOrchStateForTest()
	if resp := confirmAudit(t, open, []criticalActionStruct{{Tool: "mcp_dsp_create_curated_deal"}}, nil); resp.IsError {
		t.Fatalf("un-narrowed audit refused: %s", resp.Content)
	}

	empty := newOrchStateForTest()
	empty.setNarrowedMCPRoster(nil)
	resp := confirmAudit(t, empty, []criticalActionStruct{{Tool: "mcp_dsp_create_deal"}}, nil)
	if !resp.IsError || !strings.Contains(resp.Content, "mcp_dsp_create_deal") {
		t.Fatalf("a narrowed run with no MCP tools must refuse every typed MCP action; got %q", resp.Content)
	}
	if len(empty.typedCommitments) != 0 || empty.auditConfirmed {
		t.Fatalf("the refusal must register nothing and grant nothing: commitments=%d confirmed=%v", len(empty.typedCommitments), empty.auditConfirmed)
	}

	// A bare suffix is still left to the full-name check, which refuses it
	// with its own message.
	bare := newOrchStateForTest()
	bare.setNarrowedMCPRoster([]string{"mcp_dsp_create_deal"})
	if resp := confirmAudit(t, bare, []criticalActionStruct{{Tool: "create_deal"}}, nil); !resp.IsError || !strings.Contains(resp.Content, "full server-qualified") {
		t.Fatalf("a bare suffix must get the full-name refusal; got %q", resp.Content)
	}
}
