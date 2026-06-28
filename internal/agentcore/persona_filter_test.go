package agentcore

import (
	"context"
	"slices"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// namedTool is a minimal fantasy.AgentTool whose only meaningful behaviour for
// these tests is reporting its Info().Name — the field resolvePersonaTools keys
// on. Run is never reached (the filter operates on the schema, before any call).
func namedTool(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"test tool "+name,
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

func toolNames(tools []fantasy.AgentTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Info().Name)
	}
	return out
}

// recordingObserver captures persona_tool_blocked events so tests can assert the
// audit trail. It is concurrency-safe though these tests are single-threaded.
type recordingObserver struct {
	mu     sync.Mutex
	events []map[string]any
}

func (o *recordingObserver) Observe(_ string, payload map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, payload)
}

func (o *recordingObserver) blocked() []map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

func TestMatchesToolPattern(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		pattern string
		want    bool
	}{
		{"exact native match", "bash", "bash", true},
		{"exact native mismatch", "bash", "run_python", false},
		{"star matches everything", "anything", "*", true},
		{"mcp specific tool match", "mcp_filesystem_read_file", "mcp:filesystem/read_file", true},
		{"mcp specific tool mismatch tool", "mcp_filesystem_write_file", "mcp:filesystem/read_file", false},
		{"mcp specific tool mismatch server", "mcp_email_read_file", "mcp:filesystem/read_file", false},
		{"mcp server wildcard match", "mcp_filesystem_write_file", "mcp:filesystem/*", true},
		{"mcp server wildcard non-member", "mcp_email_send", "mcp:filesystem/*", false},
		{"mcp server-only treated as whole surface", "mcp_email_send", "mcp:email", true},
		{"prefix wildcard match", "mcp_filesystem_read_file", "mcp_filesystem/*", true},
		{"prefix wildcard mismatch", "mcp_email_send", "mcp_filesystem/*", false},
		{"bare prefix wildcard", "filesystem_x", "filesystem/*", true},
		{"blank pattern matches nothing", "bash", "", false},
		{"whitespace trimmed", "bash", "  bash  ", true},
		// A server wildcard must not leak across a server whose name is a prefix of
		// another (mcp:fs/* should not match mcp_fsx_*) because the trailing "_"
		// anchors the boundary.
		{"server boundary anchored", "mcp_fsx_tool", "mcp:fs/*", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesToolPattern(tc.tool, tc.pattern); got != tc.want {
				t.Fatalf("matchesToolPattern(%q, %q) = %v, want %v", tc.tool, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestResolvePersonaTools_EmptyPolicyPassthrough(t *testing.T) {
	all := []fantasy.AgentTool{namedTool("bash"), namedTool("run_python"), namedTool("mcp_email_send")}
	obs := &recordingObserver{}
	got := resolvePersonaTools("p", PersonaToolPermissions{}, all, obs)
	// An empty policy must return the SAME slice header (zero-alloc passthrough)
	// and emit no audit events.
	if &got[0] != &all[0] || len(got) != len(all) {
		t.Fatalf("empty policy should return the input slice unchanged; got %v", toolNames(got))
	}
	if len(obs.blocked()) != 0 {
		t.Fatalf("empty policy should emit no persona_tool_blocked events, got %d", len(obs.blocked()))
	}
}

func TestResolvePersonaTools_AllowOnly(t *testing.T) {
	all := []fantasy.AgentTool{
		namedTool("bash"),
		namedTool("run_python"),
		namedTool("mcp_filesystem_read_file"),
		namedTool("mcp_filesystem_write_file"),
		namedTool("mcp_email_send"),
		namedTool("generate_image"),
	}
	policy := PersonaToolPermissions{
		Allow: []string{"bash", "run_python", "mcp:filesystem/*"},
	}
	obs := &recordingObserver{}
	got := toolNames(resolvePersonaTools("code-reviewer", policy, all, obs))
	want := []string{"bash", "run_python", "mcp_filesystem_read_file", "mcp_filesystem_write_file"}
	if !slices.Equal(got, want) {
		t.Fatalf("allow-only: got %v, want %v", got, want)
	}
	// Everything not in the allow list is blocked with reason not_in_allow.
	blocked := obs.blocked()
	if len(blocked) != 2 {
		t.Fatalf("expected 2 blocked events (email, generate_image), got %d: %v", len(blocked), blocked)
	}
	for _, e := range blocked {
		if e["reason"] != "not_in_allow" {
			t.Fatalf("allow-only blocks should carry reason=not_in_allow, got %v", e["reason"])
		}
		if e["persona"] != "code-reviewer" {
			t.Fatalf("blocked event should label the persona, got %v", e["persona"])
		}
	}
}

func TestResolvePersonaTools_DenyOnly(t *testing.T) {
	all := []fantasy.AgentTool{
		namedTool("bash"),
		namedTool("run_python"),
		namedTool("mcp_email_send"),
		namedTool("mcp_email_read"),
		namedTool("web_search"),
	}
	policy := PersonaToolPermissions{
		Deny: []string{"bash", "run_python", "mcp:email/*"},
	}
	obs := &recordingObserver{}
	got := toolNames(resolvePersonaTools("executive-assistant", policy, all, obs))
	// Deny-only is default-allow: everything except the denied tools survives.
	want := []string{"web_search"}
	if !slices.Equal(got, want) {
		t.Fatalf("deny-only: got %v, want %v", got, want)
	}
	for _, e := range obs.blocked() {
		if e["reason"] != "deny" {
			t.Fatalf("deny-only blocks should carry reason=deny, got %v", e["reason"])
		}
	}
}

func TestResolvePersonaTools_DenyWinsOverAllow(t *testing.T) {
	all := []fantasy.AgentTool{
		namedTool("mcp_filesystem_read_file"),
		namedTool("mcp_filesystem_write_file"),
	}
	// A tool that matches BOTH allow and deny must be DENIED (deny precedence).
	policy := PersonaToolPermissions{
		Allow: []string{"mcp:filesystem/*"},
		Deny:  []string{"mcp:filesystem/write_file"},
	}
	obs := &recordingObserver{}
	got := toolNames(resolvePersonaTools("p", policy, all, obs))
	want := []string{"mcp_filesystem_read_file"}
	if !slices.Equal(got, want) {
		t.Fatalf("deny-wins: got %v, want %v", got, want)
	}
	blocked := obs.blocked()
	if len(blocked) != 1 || blocked[0]["tool"] != "mcp_filesystem_write_file" || blocked[0]["reason"] != "deny" {
		t.Fatalf("deny-wins: expected write_file blocked with reason=deny, got %v", blocked)
	}
}

// TestResolvePersonaTools_CannotWidenBeyondInput is the security-critical
// property: the filter can only SUBTRACT from the slice it is handed (which the
// caller has already run through the server + credential gates). A tool the
// persona "allows" but that the earlier gates dropped from `all` can never
// reappear in the output.
func TestResolvePersonaTools_CannotWidenBeyondInput(t *testing.T) {
	// The earlier gates already removed mcp_email_send from the roster (it is NOT
	// in `all`). The persona explicitly allows it anyway.
	all := []fantasy.AgentTool{namedTool("bash"), namedTool("mcp_filesystem_read_file")}
	policy := PersonaToolPermissions{
		Allow: []string{"bash", "mcp:filesystem/*", "mcp:email/send", "*"},
	}
	got := toolNames(resolvePersonaTools("p", policy, all, &recordingObserver{}))
	if slices.Contains(got, "mcp_email_send") {
		t.Fatal("persona allowlist widened the roster beyond the input — a denied-upstream tool reappeared")
	}
	// The output is a subset of the input regardless of how permissive the policy.
	for _, name := range got {
		if !slices.Contains([]string{"bash", "mcp_filesystem_read_file"}, name) {
			t.Fatalf("output contains %q which was not in the input roster", name)
		}
	}
}

// TestRun_PersonaPolicyFiltersToolsBeforeLLM is the end-to-end property required
// by #294: a persona with a restrictive deny block must mean the denied tool
// never appears in the tool list the model RECEIVES — enforced at registration,
// before the first LLM call, not at call time. We capture the fantasy.Call the
// mock model is streamed and assert the denied tool is absent and the permitted
// ones are present.
func TestRun_PersonaPolicyFiltersToolsBeforeLLM(t *testing.T) {
	var captured []string
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			captured = captured[:0]
			for _, tl := range call.Tools {
				captured = append(captured, tl.GetName())
			}
			return streamStop()(nil, call)
		},
	}
	obs := &recordingObserver{}
	denyPolicy := PersonaToolPermissions{Deny: []string{"send_email", "generate_image"}}

	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix: CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{
			namedTool("bash"),
			namedTool("run_python"),
			namedTool("send_email"),
			namedTool("generate_image"),
		},
		PersonaName:   "code-reviewer",
		PersonaPolicy: &denyPolicy,
	}, Deps{
		Input:    stubInput{system: "test", user: "hi", label: "t"},
		Observer: obs,
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if slices.Contains(captured, "send_email") || slices.Contains(captured, "generate_image") {
		t.Fatalf("denied tools reached the model's tool list: %v", captured)
	}
	if !slices.Contains(captured, "bash") || !slices.Contains(captured, "run_python") {
		t.Fatalf("permitted tools missing from the model's tool list: %v", captured)
	}
	// The audit trail records the two suppressed tools.
	if len(obs.blocked()) != 2 {
		t.Fatalf("expected 2 persona_tool_blocked events, got %d: %v", len(obs.blocked()), obs.blocked())
	}
}

// TestRun_NoPersonaPolicyUnchanged confirms backward compatibility: with no
// persona policy the model receives every native tool (the persona filter is a
// zero-overhead passthrough).
func TestRun_NoPersonaPolicyUnchanged(t *testing.T) {
	var captured []string
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			captured = captured[:0]
			for _, tl := range call.Tools {
				captured = append(captured, tl.GetName())
			}
			return streamStop()(nil, call)
		},
	}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix: CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{
			namedTool("bash"),
			namedTool("send_email"),
		},
		// No PersonaPolicy set.
	}, Deps{
		Input:  stubInput{system: "test", user: "hi", label: "t"},
		Policy: NewInteractivePolicy(0, 0, nil, nil),
		Model:  model,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !slices.Contains(captured, "bash") || !slices.Contains(captured, "send_email") {
		t.Fatalf("no-policy run should offer all native tools, got %v", captured)
	}
}
