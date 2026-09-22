package agentcore

import (
	"context"
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

// The narrowed allowlist the scheduled driver derives for required_tools
// [mcp_pages_get_page_data, mcp_pages_update_page_data_upload].
var pagesNarrowed = mcpAllowlist{"pages": {"get_page_data", "update_page_data_upload"}}

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
	// The seat has no entry of its own and narrows by its base server's entry.
	want := "mcp_pages_acct_get_page_data,mcp_pages_get_page_data,mcp_pages_update_page_data_upload"
	if got != want {
		t.Fatalf("registered MCP tools = %s, want %s", got, want)
	}
	names := toolNamesOf(registered)
	if !names["task_tracker"] || !names[toolNameConfirmAudit] {
		t.Fatalf("native tools and confirm_audit must survive a narrowed roster: %v", names)
	}

	// Without the exhaustive flag the same allowlist leaves ungoverned servers
	// open, as Gate-2 always has.
	_, open, err := buildFantasyToolsWithRoster(native, pagesRosterCatalog(), &fakeBroker{}, pagesNarrowed, passPolicy{}, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(open.directMCP, ",") != "mcp_fast_io_share,mcp_fast_io_upload,mcp_pages_acct_get_page_data,mcp_pages_get_page_data,mcp_pages_update_page_data_upload" {
		t.Fatalf("baseline Gate-2 changed: %v", open.directMCP)
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
// roster registered, a live-registry section listing exactly those, and a call
// to a tool the narrowing removed answered in-band as "tool not found" — the
// layout tool is not reachable at all.
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
			MCPCatalog: pagesRosterCatalog(),
			MCPBroker:  broker,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	crumbs := 0
	for _, m := range session.Messages {
		if strings.HasPrefix(m.Content, "[roster] required_tools_only: 3 mcp tools registered") {
			crumbs++
		}
	}
	if crumbs != 1 {
		t.Fatalf("want exactly one [roster] breadcrumb naming 3 MCP tools, got %d", crumbs)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(systemPrompt, liveRegistryHeading) || !strings.Contains(systemPrompt, "mcp_pages_update_page_data_upload") ||
		strings.Contains(systemPrompt, "deploy_page_upload") || strings.Contains(systemPrompt, "mcp_fast_io") {
		t.Fatalf("live registry must list exactly the narrowed roster:\n%s", systemPrompt)
	}
	if !strings.Contains(toolResult, "tool not found: mcp_pages_deploy_page_upload") {
		t.Fatalf("a call to a narrowed-away tool must answer 'tool not found', got %q", toolResult)
	}
	if broker.lastTool != "" {
		t.Fatalf("the narrowed-away tool reached the broker: %s/%s", broker.lastServer, broker.lastTool)
	}
}
