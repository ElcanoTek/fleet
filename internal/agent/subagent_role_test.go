package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Typed children + isolation + default-on registration (#1043). Fake-LLM seam
// only (mock fantasy.LanguageModel) — no real key, no network, no sandbox.

func TestNormalizeSubagentRole(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", SubagentRoleExplore},
		{"explore", SubagentRoleExplore},
		{"EXPLORE", SubagentRoleExplore},
		{"worker", SubagentRoleWorker},
		{"  Worker ", SubagentRoleWorker},
		{"banana", SubagentRoleExplore}, // invalid must fail SAFE, never to worker
		{"workers", SubagentRoleExplore},
	}
	for _, c := range cases {
		if got := normalizeSubagentRole(c.in); got != c.want {
			t.Errorf("normalizeSubagentRole(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExploreStripSet_PinsKnownWriters pins the ONE place the read-only child
// posture is defined: every known write-capable native tool is denied, and the
// core read tools are not.
func TestExploreStripSet_PinsKnownWriters(t *testing.T) {
	for _, name := range []string{
		"write_file", "edit_file", "xlsx_workbook", "generate_image",
		"create_task", "publish_artifact", "remember", "propose_note", "propose_skill",
	} {
		if !exploreDeniedNativeTools[name] {
			t.Errorf("explore strip set must deny %q", name)
		}
	}
	for _, name := range []string{"view_file", "bash", "run_python", "web_fetch", "download_url", "recall", "task_tracker"} {
		if exploreDeniedNativeTools[name] {
			t.Errorf("explore strip set must NOT deny the read tool %q", name)
		}
	}
}

// newRosterParentForSpawn builds a spawn-ready parent whose native roster is the
// full default tool bundle (writers included) and whose model — inherited by
// children — captures the CHILD's advertised tool roster.
func newRosterParentForSpawn(t *testing.T, capModel fantasy.LanguageModel) *Agent {
	t.Helper()
	t.Setenv("FLEET_LOG_FILE", filepath.Join(t.TempDir(), "session.json"))
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         capModel,
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
		NativeTools:   tools.DefaultTools(),
		Subagent:      SubagentOptions{Enabled: true, MaxDepth: 1, MaxChildren: 5},
	})
	a.runtimePolicy = agentcore.NewScheduledPolicy(a.logSession, a.maxIterations, 1.0, 0)
	a.subagent.budgetFraction = 1.0
	return a
}

// spawnCtx returns a context carrying a fresh forced working dir, the way a
// scheduled run's context arrives at the spawn tool.
func spawnCtx(t *testing.T) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	return tools.WithForcedWorkingDir(context.Background(), root), root
}

// TestSpawn_ExploreChildHasNoWriteTools proves the role=explore strip end to
// end: the CHILD run advertises no write-capable native tools, while the read
// tools survive. The default (empty role) is explore.
func TestSpawn_ExploreChildHasNoWriteTools(t *testing.T) {
	for _, role := range []string{"", "explore", "not-a-role"} {
		t.Run("role="+role, func(t *testing.T) {
			capModel := &toolCapturingModel{}
			parent := newRosterParentForSpawn(t, capModel)
			ctx, _ := spawnCtx(t)

			resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "research something", Role: role}, "call-1")
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			out := parseSpawnOutput(t, resp)
			if out.Role != SubagentRoleExplore {
				t.Fatalf("result role = %q, want explore (safe default)", out.Role)
			}
			for _, denied := range []string{"write_file", "edit_file", "xlsx_workbook", "generate_image"} {
				if containsStr(capModel.advertised, denied) {
					t.Errorf("explore child must not advertise %q; advertised=%v", denied, capModel.advertised)
				}
			}
			for _, kept := range []string{"view_file", "bash", "web_fetch"} {
				if !containsStr(capModel.advertised, kept) {
					t.Errorf("explore child must keep the read tool %q; advertised=%v", kept, capModel.advertised)
				}
			}
		})
	}
}

// TestSpawn_WorkerChildKeepsWriteTools proves role=worker keeps the full roster
// minus the interactive-only staging tools (whose raw bodies are fatal outside a
// supervised chat turn).
func TestSpawn_WorkerChildKeepsWriteTools(t *testing.T) {
	capModel := &toolCapturingModel{}
	parent := newRosterParentForSpawn(t, capModel)
	ctx, _ := spawnCtx(t)

	resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "write the report file", Role: "worker"}, "call-1")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := parseSpawnOutput(t, resp)
	if out.Role != SubagentRoleWorker {
		t.Fatalf("result role = %q, want worker", out.Role)
	}
	for _, kept := range []string{"write_file", "edit_file", "view_file", "bash"} {
		if !containsStr(capModel.advertised, kept) {
			t.Errorf("worker child must advertise %q; advertised=%v", kept, capModel.advertised)
		}
	}
	for _, denied := range []string{"preview_email", "schedule_task", "suggest_advanced_model", "propose_memory"} {
		if containsStr(capModel.advertised, denied) {
			t.Errorf("a child must not advertise the interactive-only tool %q; advertised=%v", denied, capModel.advertised)
		}
	}
}

// TestSpawn_ChildWorkdirIsolation proves every child gets its own
// <workspace>/subagents/<child-session-id>/ directory (#1043): the dir exists,
// is unique per child, and is reported back in the JSON result together with
// role + child_session_id so the parent knows where outputs live.
func TestSpawn_ChildWorkdirIsolation(t *testing.T) {
	parent := newParentForSpawn(t, &budgetMockModel{name: "c", inTokens: 10, outTokens: 5}, 10.0, 0, 1, 5)
	ctx, root := spawnCtx(t)

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		resp, err := parent.spawn(ctx, spawnSubagentInput{Task: "t"}, "call-1")
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		out := parseSpawnOutput(t, resp)
		// The mock child never confirms the scheduled self-audit, so the run ends
		// unsuccessful — irrelevant here: isolation metadata must be reported for
		// every spawned child, success or not.
		if out.ChildSessionID == "" || !strings.HasPrefix(out.ChildSessionID, "subagent-") {
			t.Fatalf("child_session_id = %q, want subagent-* (linkage id)", out.ChildSessionID)
		}
		want := filepath.Join(root, "subagents", out.ChildSessionID)
		if out.Workdir != want {
			t.Fatalf("workdir = %q, want %q (isolated per-child subdir)", out.Workdir, want)
		}
		if fi, statErr := os.Stat(out.Workdir); statErr != nil || !fi.IsDir() {
			t.Fatalf("workdir %q not created: %v", out.Workdir, statErr)
		}
		if seen[out.Workdir] {
			t.Fatalf("two children shared the workdir %q — parallel writes would collide", out.Workdir)
		}
		seen[out.Workdir] = true
	}
}

// TestRecordSubagentSpawn_CarriesRoleAndWorkdir pins the parent-log linkage
// payload the task page's child cards are built from (#1043).
func TestRecordSubagentSpawn_CarriesRoleAndWorkdir(t *testing.T) {
	parent := newParentForSpawn(t, &budgetMockModel{name: "c"}, 1.0, 0, 1, 5)
	parent.recordSubagentSpawn("subagent-abc", SubagentRoleWorker, "/ws/subagents/subagent-abc",
		agentcore.RunUsage{CostUSD: 0.02, PromptTokens: 100, CompletionTokens: 20}, true)

	msgs := parent.logSession.Messages
	if len(msgs) == 0 {
		t.Fatal("no linkage entry recorded")
	}
	last := msgs[len(msgs)-1]
	if last.MessageType == nil || *last.MessageType != "subagent_spawned" {
		t.Fatalf("message type = %v, want subagent_spawned", last.MessageType)
	}
	for _, want := range []string{`"role":"worker"`, `"workdir":"/ws/subagents/subagent-abc"`, `"child_session_id":"subagent-abc"`} {
		if !strings.Contains(last.Content, want) {
			t.Errorf("linkage payload missing %s: %s", want, last.Content)
		}
	}
}

// TestRunInteractiveTurn_SpawnToolRegistration proves interactive chat registers
// spawn_subagent iff the driver-composed gate is on (#1043) — the same
// structural non-registration rule the scheduled path pins.
func TestRunInteractiveTurn_SpawnToolRegistration(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			var advertised []string
			model := &itMockModel{
				streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
					if advertised == nil {
						for _, tl := range call.Tools {
							if ft, ok := tl.(fantasy.FunctionTool); ok {
								advertised = append(advertised, ft.GetName())
							}
						}
					}
					return func(yield func(fantasy.StreamPart) bool) {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "hi"}) {
							return
						}
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
					}, nil
				},
			}
			tc := TurnConfig{
				SystemPrompt: "sys",
				Messages:     []fantasy.Message{fantasy.NewUserMessage("hello")},
				Model:        model,
				MaxTokens:    1024,
				Config:       &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
				Subagent:     SubagentOptions{Enabled: enabled, MaxDepth: 1, MaxChildren: 5},
			}
			if _, err := RunInteractiveTurn(context.Background(), tc, &captureObs{}); err != nil {
				t.Fatalf("RunInteractiveTurn: %v", err)
			}
			if got := containsStr(advertised, "spawn_subagent"); got != enabled {
				t.Fatalf("spawn_subagent advertised=%v, want %v (structural registration); tools=%v", got, enabled, advertised)
			}
		})
	}
}

// TestExploreMCPToolAllowlist pins the best-effort MCP write-tool name denylist
// (#1043): read tools survive, mutation-verb tools are stripped, an all-writer
// server gets the deny-all sentinel (an empty entry would mean "allow all"),
// the parent's allowlist is only ever narrowed, and parent entries for
// non-catalog servers are preserved.
func TestExploreMCPToolAllowlist(t *testing.T) {
	catalog := []mcp.ServerTool{
		{ServerName: "crm", Tool: mcp.Tool{Name: "get_contact"}},
		{ServerName: "crm", Tool: mcp.Tool{Name: "list_deals"}},
		{ServerName: "crm", Tool: mcp.Tool{Name: "update_contact"}},
		{ServerName: "crm", Tool: mcp.Tool{Name: "delete_deal"}},
		{ServerName: "mail", Tool: mcp.Tool{Name: "send_email"}},
		{ServerName: "mail", Tool: mcp.Tool{Name: "create_draft"}},
		{ServerName: "search", Tool: mcp.Tool{Name: "search_web"}},
	}
	parent := agentcore.MCPAllowlist{
		// The parent itself may only call get_contact — the child must not
		// regain list_deals through the explore derivation.
		"crm":   {"get_contact", "update_contact"},
		"other": {"anything"},
	}
	out := exploreMCPToolAllowlist(catalog, parent)

	if got := out["crm"]; len(got) != 1 || got[0] != "get_contact" {
		t.Fatalf("crm allowlist = %v, want [get_contact] (reads kept, writers stripped, parent narrowing honored)", got)
	}
	if got := out["mail"]; len(got) != 1 || got[0] != exploreNoToolsSentinel {
		t.Fatalf("mail allowlist = %v, want the deny-all sentinel (every tool is a writer)", got)
	}
	if got := out["search"]; len(got) != 1 || got[0] != "search_web" {
		t.Fatalf("search allowlist = %v, want [search_web]", got)
	}
	if got := out["other"]; len(got) != 1 || got[0] != "anything" {
		t.Fatalf("non-catalog parent entry must be preserved, got %v", got)
	}
	// Read verbs never match the denylist pattern.
	for _, ok := range []string{"get_posts", "list_reports", "download_file", "search_emails", "find_meeting_availability"} {
		if exploreDeniedMCPNamePattern.MatchString(ok) {
			t.Errorf("read tool %q must not match the explore denylist", ok)
		}
	}
	for _, bad := range []string{"send_email", "batch_delete_messages", "sharepoint_upload_file", "set_vacation", "create_event", "respond_to_event"} {
		if !exploreDeniedMCPNamePattern.MatchString(bad) {
			t.Errorf("writer %q must match the explore denylist", bad)
		}
	}
}

// TestSubagentSessionIDValidation pins the transcript API's id gate (#1043):
// only the exact "subagent-<uuid>" shape passes, so a request can never smuggle
// path components into the child log-file lookup.
func TestSubagentSessionIDValidation(t *testing.T) {
	ok := "subagent-12345678-1234-1234-1234-123456789abc"
	if !IsSubagentSessionID(ok) {
		t.Fatalf("%q must validate", ok)
	}
	for _, bad := range []string{
		"", "subagent-", "subagent-notauuid",
		"subagent-12345678-1234-1234-1234-123456789abc/../../etc/passwd",
		"../subagent-12345678-1234-1234-1234-123456789abc",
		"subagent-12345678-1234-1234-1234-123456789ABC", // uppercase — uuid.NewString is lowercase
		"fleet-session.json",
	} {
		if IsSubagentSessionID(bad) {
			t.Errorf("%q must NOT validate", bad)
		}
	}
	// The path derivation matches what buildChild used for a server-mode parent.
	t.Setenv("FLEET_LOG_FILE", "/var/lib/fleet/fleet-session.json")
	if got, want := ChildLogFilePath(ok), "/var/lib/fleet/fleet-session."+ok+".json"; got != want {
		t.Fatalf("ChildLogFilePath = %q, want %q", got, want)
	}
}

// TestInteractivePolicy_BudgetSeamForSpawn proves the InteractivePolicy exposes
// the same budget seam the spawn tool needs (#1043): a child's charge-back is
// visible to the next Budget() read, so sibling slicing and the chat cost chip
// account for child spend.
func TestInteractivePolicy_BudgetSeamForSpawn(t *testing.T) {
	p := agentcore.NewInteractivePolicy(1.0, 0, nil, nil)
	before := p.Budget().RemainingCostUSD()
	p.ChargeChildUsage(agentcore.RunUsage{CostUSD: 0.25, PromptTokens: 100, CompletionTokens: 50})
	after := p.Budget().RemainingCostUSD()
	if diff := before - after; diff < 0.24 || diff > 0.26 {
		t.Fatalf("child spend not charged into the interactive budget: before=%v after=%v", before, after)
	}
}
