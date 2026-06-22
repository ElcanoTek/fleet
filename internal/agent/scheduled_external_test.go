package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
)

// SCHEDULED-EXTERNAL tests (P-ACP-4). These prove the fail-closed gate and the
// containment-tier wiring WITHOUT podman and WITHOUT a live model key: the
// scheduled-external path is exercised through an injected fake externalRuntime
// that emulates a self-executing external ACP agent (it stamps the governance
// tier, issues a permission request, and reflects the outcome the broker
// returned). The agentcore loop is never reached on the external path, so the
// (nil) model never matters — but we still build a non-nil mock model so the
// no-model guard does not fire first.

// fakeExternalRuntime emulates acpruntime.ExternalRuntime for the scheduled path:
// it captures the deps it was handed (so the test can assert PermissionBroker is
// nil — no human on the scheduled loop), stamps the governance: delegated tier,
// stages ONE permission request resolved EXACTLY as acpruntime's externalClient
// resolves it (nil broker → fail-closed deny; otherwise the broker decides), and
// streams a final chunk reflecting the outcome. It mirrors the fakeExternalAgent
// in acpruntime/external_test.go without needing the unexported wiring.
type fakeExternalRuntime struct {
	cfg acpruntime.ExternalConfig

	mu              sync.Mutex
	ran             bool
	gotPrompt       string
	gotBroker       acpruntime.PermissionBroker
	gotBrokerWasNil bool
	permGranted     bool
	// usage is the self-reported usage the fake agent returns on Result.Usage, so a
	// test can assert the scheduled-external driver reconciles it into the session
	// log (issue #31). Zero value = an agent that reported nothing.
	usage agentcore.RunUsage
}

func (f *fakeExternalRuntime) Run(ctx context.Context, promptText string, deps acpruntime.ExternalDeps) (acpruntime.Result, error) {
	f.mu.Lock()
	f.ran = true
	f.gotPrompt = promptText
	f.gotBroker = deps.PermissionBroker
	f.gotBrokerWasNil = deps.PermissionBroker == nil
	f.mu.Unlock()

	// Stamp the containment tier exactly as the real ExternalRuntime does so the
	// run record honestly shows governance: delegated.
	deps.Observer.Observe(acpruntime.EventGovernance, map[string]any{
		"tier":  string(acpruntime.GovernanceDelegated),
		"image": f.cfg.Image,
	})

	// A self-reported opening chunk.
	deps.Observer.Observe("text.delta", map[string]any{"text": "working on it. "})

	// A sensitive action: the agent asks permission. Resolve it the SAME way the
	// real externalClient does — a nil broker (scheduled: no human on the loop)
	// FAIL-CLOSES to a deny; a wired broker decides. Either way, no approve-all.
	granted := false
	if deps.PermissionBroker != nil {
		dec, err := deps.PermissionBroker.RequestDecision(ctx, acpruntime.PermissionRequest{
			RequestID: "perm-1",
			Title:     "Modify config.json",
		})
		granted = err == nil && dec.Allowed
	}
	f.mu.Lock()
	f.permGranted = granted
	f.mu.Unlock()
	deps.Observer.Observe("permission.resolved", map[string]any{
		"request_id": "perm-1",
		"allowed":    granted,
		"reason":     denyReasonFor(deps.PermissionBroker, granted),
	})

	final := "skipped the change."
	if granted {
		final = "applied the change."
	}
	deps.Observer.Observe("text.delta", map[string]any{"text": final})
	return acpruntime.Result{FinalText: "working on it. " + final, StopReason: "end_turn", Usage: f.usage}, nil
}

func denyReasonFor(b acpruntime.PermissionBroker, granted bool) string {
	if granted {
		return ""
	}
	if b == nil {
		return "no permission broker wired (fail-closed deny)"
	}
	return "denied by user"
}

// newExternalScheduledAgent builds a scheduled Agent pinned to an EXTERNAL flavor,
// with the fake external runtime injected. The flag and sandbox are caller-set.
func newExternalScheduledAgent(t *testing.T, allow bool, image string, fake *fakeExternalRuntime) *Agent {
	t.Helper()
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         &itMockModel{}, // a fleet model the EXTERNAL path never uses; the flag-off test watches its streamCount to prove there was no fallback
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
		Runtime:       "claude-code",
		RuntimeFlavor: clientconfig.Runtime{
			Name:            "claude-code",
			Type:            clientconfig.RuntimeTypeACP,
			Image:           image,
			DelegatedPolicy: true,
			ModelEnv:        []string{"ANTHROPIC_API_KEY"},
		},
		AllowUngovernedScheduled: allow,
	})
	a.newExternalRuntime = func(cfg acpruntime.ExternalConfig) externalRuntime {
		fake.cfg = cfg
		return fake
	}
	return a
}

// TestScheduledExternal_FlagOffLoudError is the CARDINAL test: a scheduled task
// that selects an external flavor with the per-client opt-in OFF is a LOUD ERROR
// at dispatch — NOT a silent fallback to a native flavor. We assert:
//   - Execute returns an error naming the governance gate;
//   - the fake external runtime was NEVER invoked (no containment-tier run);
//   - the in-process / native loop was NOT taken (the mock model never streamed),
//     i.e. there was no fallback;
//   - the run/session log records the fatal governance reason.
func TestScheduledExternal_FlagOffLoudError(t *testing.T) {
	fake := &fakeExternalRuntime{}
	a := newExternalScheduledAgent(t, false /* flag OFF */, "registry/claude-code:pinned", fake)
	a.sb = nil // even with a sandbox the gate must fire first; prove the gate, not the sandbox check

	model := a.model.(*itMockModel)

	err := a.Execute(context.Background(), "do the thing")
	if err == nil {
		t.Fatal("flag OFF + scheduled-external MUST be a loud error at dispatch")
	}
	if !strings.Contains(err.Error(), "allow_ungoverned_scheduled_agents") {
		t.Errorf("error must name the governance opt-in; got %v", err)
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Errorf("error must state it is fail-closed (no fallback); got %v", err)
	}

	fake.mu.Lock()
	ran := fake.ran
	fake.mu.Unlock()
	if ran {
		t.Fatal("the external runtime must NOT run when the flag is OFF")
	}

	// No silent fallback: the native/in-process agentcore loop must NOT have run
	// (the mock model never streamed).
	model.mu.Lock()
	streams := model.streamCount
	model.mu.Unlock()
	if streams != 0 {
		t.Fatalf("flag OFF must NOT fall back to the native/in-process loop; model streamed %d times", streams)
	}

	// The run record honestly shows WHY it failed (Execute's deferred fatal
	// handler records "[fatal] ..." with the governance reason).
	if !sessionLogContains(a, "allow_ungoverned_scheduled_agents") {
		t.Error("the session log must record the governance failure reason")
	}
}

// TestScheduledExternal_FlagOnRunsContainment proves the happy path: with the
// per-client opt-in ON and a sandbox image present, the scheduled-external run is
// admitted at the containment tier. We assert:
//   - the external runtime ran;
//   - governance: delegated is stamped in the session log;
//   - the scrubbed ProviderEnv was built from model_env (containment invariant);
//   - NO PermissionBroker was wired (nil) — no human on the scheduled loop;
//   - the staged permission attempt was AUTO-DENIED (fail-closed), and the deny is
//     recorded.
func TestScheduledExternal_FlagOnRunsContainment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // obvious placeholder; the path never calls a model
	fake := &fakeExternalRuntime{}
	a := newExternalScheduledAgent(t, true /* flag ON */, "registry/claude-code:pinned", fake)
	// An external agent drives its OWN provider endpoint; fleet's OpenRouter model
	// is irrelevant to it. Prove the path runs even with no resolved fleet model.
	a.model = nil

	if err := a.Execute(context.Background(), "do the thing"); err != nil {
		t.Fatalf("flag ON + sandbox + external should run, got error: %v", err)
	}

	fake.mu.Lock()
	ran, brokerNil, granted := fake.ran, fake.gotBrokerWasNil, fake.permGranted
	cfg, prompt := fake.cfg, fake.gotPrompt
	fake.mu.Unlock()

	if !ran {
		t.Fatal("the external runtime must run when the flag is ON")
	}
	if prompt != "do the thing" {
		t.Errorf("the external agent must receive the task prompt; got %q", prompt)
	}
	if !brokerNil {
		t.Fatal("scheduled-external MUST wire NO PermissionBroker (nil): no human on the loop")
	}
	if granted {
		t.Fatal("a nil broker MUST fail-closed (deny) every permission request; got granted")
	}
	// Scrubbed env: ONLY the provider's own model key, from model_env.
	if cfg.Image != "registry/claude-code:pinned" {
		t.Errorf("external config image = %q, want the flavor image", cfg.Image)
	}
	if _, ok := cfg.ProviderEnv["ANTHROPIC_API_KEY"]; !ok {
		t.Errorf("ProviderEnv must carry the model_env key; got %v", cfg.ProviderEnv)
	}

	// governance: delegated stamped in the run record.
	if !sessionLogContains(a, string(acpruntime.GovernanceDelegated)) {
		t.Error("the session log must stamp governance: delegated for a scheduled-external run")
	}
	// The auto-deny is recorded honestly.
	if !sessionLogContains(a, "skipped the change") {
		t.Error("the final text should reflect the auto-denied action (skipped)")
	}
}

// TestScheduledExternal_RecordsSelfReportedUsage: a containment-tier run must
// reconcile the agent's SELF-REPORTED usage into the captain's-log session, so it
// never shows a misleading zero-token / $0 run at the very tier where the agent
// drives its own model endpoint (issue #31).
func TestScheduledExternal_RecordsSelfReportedUsage(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // obvious placeholder; the path never calls a model
	fake := &fakeExternalRuntime{usage: agentcore.RunUsage{
		PromptTokens:     1234,
		CompletionTokens: 567,
		CachedTokens:     89,
		CostUSD:          0.1234,
	}}
	a := newExternalScheduledAgent(t, true /* flag ON */, "registry/claude-code:pinned", fake)
	a.model = nil // external drives its own endpoint; prove usage is recorded regardless

	if err := a.Execute(context.Background(), "do the thing"); err != nil {
		t.Fatalf("flag ON + sandbox + external should run, got: %v", err)
	}

	if a.logSession.PromptTokens != 1234 || a.logSession.CompletionTokens != 567 {
		t.Errorf("session tokens = (prompt %d, completion %d), want (1234, 567)",
			a.logSession.PromptTokens, a.logSession.CompletionTokens)
	}
	if a.logSession.CachedTokens != 89 {
		t.Errorf("session cached tokens = %d, want 89", a.logSession.CachedTokens)
	}
	if a.logSession.Cost != 0.1234 {
		t.Errorf("session cost = %v, want 0.1234 (self-reported)", a.logSession.Cost)
	}
}

// TestScheduledExternal_SandboxRequired proves containment is mandatory: with the
// flag ON but NO sandbox image, a scheduled-external attempt is an ERROR, not a
// degraded run. The external runtime is never invoked.
func TestScheduledExternal_SandboxRequired(t *testing.T) {
	fake := &fakeExternalRuntime{}
	a := newExternalScheduledAgent(t, true /* flag ON */, "" /* no sandbox image */, fake)

	err := a.Execute(context.Background(), "do the thing")
	if err == nil {
		t.Fatal("scheduled-external without a sandbox image MUST error (containment is mandatory)")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("error must state the sandbox requirement; got %v", err)
	}
	fake.mu.Lock()
	ran := fake.ran
	fake.mu.Unlock()
	if ran {
		t.Fatal("the external runtime must NOT run without a sandbox image")
	}
}

// TestIsExternalFlavor pins the routing predicate: type acp OR delegated_policy
// routes to the scheduled-external path; the native flavors do not.
func TestIsExternalFlavor(t *testing.T) {
	cases := []struct {
		name   string
		flavor clientconfig.Runtime
		want   bool
	}{
		{"acp-type", clientconfig.Runtime{Type: clientconfig.RuntimeTypeACP}, true},
		{"delegated-only", clientconfig.Runtime{Type: "", DelegatedPolicy: true}, true},
		{"native-inprocess", clientconfig.Runtime{Type: clientconfig.RuntimeTypeNativeInprocess}, false},
		{"native-acp", clientconfig.Runtime{Type: clientconfig.RuntimeTypeNativeACP}, false},
		{"zero", clientconfig.Runtime{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{runtimeFlavor: tc.flavor}
			if got := a.isExternalFlavor(); got != tc.want {
				t.Fatalf("isExternalFlavor(%+v) = %v, want %v", tc.flavor, got, tc.want)
			}
		})
	}
}

// TestBuildExternalRuntime_DefaultsToReal proves production wiring: with no test
// seam injected, buildExternalRuntime returns the REAL acpruntime.ExternalRuntime
// (not a fork) — the same runtime the interactive-external path uses.
func TestBuildExternalRuntime_DefaultsToReal(t *testing.T) {
	a := &Agent{} // no newExternalRuntime seam
	rt := a.buildExternalRuntime(acpruntime.ExternalConfig{Image: "x"})
	if _, ok := rt.(*acpruntime.ExternalRuntime); !ok {
		t.Fatalf("buildExternalRuntime must default to *acpruntime.ExternalRuntime, got %T", rt)
	}
}

// sessionLogContains reports whether any message in the agent's session log
// contains substr (case-insensitive over content).
func sessionLogContains(a *Agent, substr string) bool {
	for _, m := range a.logSession.SnapshotMessages() {
		if strings.Contains(strings.ToLower(m.Content), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

var _ = fantasy.NewUserMessage
