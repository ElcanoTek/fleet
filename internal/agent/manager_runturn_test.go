package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// recordingSink is an EventSink that captures every emitted (event, payload) so
// a test can assert the SSE event vocabulary RunTurn produces.
type recordingSink struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	name    string
	payload any
}

func (r *recordingSink) Emit(event string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{event, payload})
}

func (r *recordingSink) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.name
	}
	return out
}

func (r *recordingSink) has(name string) bool {
	for _, n := range r.names() {
		if n == name {
			return true
		}
	}
	return false
}

// newFakeLLMManager builds a real *Manager wired to the wire-compatible fake LLM
// (reached via OPENROUTER_BASE_URL, so no real key) with MockMode=true, which
// gives a host-mode sandbox pool — no podman or sandbox image required. This is
// the always-on (provider-free, DB-free) seam from issue #49: it exercises the
// genuine Manager.RunTurn assembly (prompt + sandbox + model + history →
// agentcore.Run), not a mock of it.
func newFakeLLMManager(t *testing.T, fake *fakellm.Server) *Manager {
	t.Helper()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")

	// Minimal in-repo-free prompt bundle: buildSystemPrompt requires a chat.md
	// and a persona YAML; protocols may be empty.
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "system_prompts", "chat.md"), "# Test system prompt\n\nBe brief.\n")
	writePromptFile(t, filepath.Join(dir, "personas", "generic.yaml"), "name: Generic\n")
	if err := os.MkdirAll(filepath.Join(dir, "protocols"), 0o755); err != nil {
		t.Fatalf("mkdir protocols: %v", err)
	}

	cfg := &config.Config{
		MockMode:         true, // → host-mode sandbox pool (no podman)
		OpenRouterAPIKey: "test-key",
		PersonaDefault:   "generic",
	}
	mgr, err := New(ManagerOptions{
		Config:           cfg,
		PersonasDir:      filepath.Join(dir, "personas"),
		ProtocolsDir:     filepath.Join(dir, "protocols"),
		SkillsDir:        filepath.Join(dir, "skills"),
		SystemPromptsDir: filepath.Join(dir, "system_prompts"),
		// Exercise the admission path on every fake-LLM turn (never saturated at 4
		// slots, so it must be transparent — and the slot must be released after).
		Limiter: admission.New(4, 1),
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return mgr
}

func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestManagerRunTurn_TextOnly drives a complete interactive turn through the real
// Manager.RunTurn against the fake LLM: it asserts the streamed event vocabulary,
// the assembled final text, and the returned history — all with no external
// provider and no database.
func TestManagerRunTurn_TextOnly(t *testing.T) {
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("hello from the fake llm"),
	}})
	mgr := newFakeLLMManager(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink := &recordingSink{}
	res, err := mgr.RunTurn(ctx, TurnInput{
		UserMessage: "hi",
		Model:       "anthropic/claude-opus-4.8",
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if !strings.Contains(res.FinalText, "hello from the fake llm") {
		t.Errorf("FinalText = %q, want it to contain the fake reply", res.FinalText)
	}
	if res.Model != "anthropic/claude-opus-4.8" {
		t.Errorf("resolved Model = %q", res.Model)
	}
	// The turn must stream the core vocabulary the SSE layer relies on.
	for _, want := range []string{"turn.started", "text.delta", "turn.completed"} {
		if !sink.has(want) {
			t.Errorf("missing %q event; saw %v", want, sink.names())
		}
	}
	// History tail must carry at least the user turn + the assistant reply so
	// the next turn can replay it.
	if len(res.NewHistory) < 2 {
		t.Fatalf("NewHistory = %d entries, want >= 2 (user + assistant)", len(res.NewHistory))
	}
	if res.NewHistory[0].Role != "user" {
		t.Errorf("first history entry role = %q, want user", res.NewHistory[0].Role)
	}
	// The admission slot the turn held must be released once it returns.
	if n := mgr.limiter.InFlight(); n != 0 {
		t.Errorf("limiter still holds %d slot(s) after the turn; release leaked", n)
	}
}

// TestManagerRunTurn_AtCapacity verifies the interactive admission gate: when the
// shared limiter has no free slot, RunTurn waits briefly then returns ErrAtCapacity
// (which the HTTP layer surfaces as a clean turn.error) rather than hanging or
// over-subscribing the box. Uses a bare Manager — RunTurn rejects before it touches
// the model/sandbox, so no fake-LLM harness is needed.
func TestManagerRunTurn_AtCapacity(t *testing.T) {
	lim := admission.New(1, 0) // one slot, no reserve
	held, ok := lim.AcquireInteractive(context.Background().Done())
	if !ok {
		t.Fatal("setup: could not take the only slot")
	}
	defer held()

	orig := interactiveAdmitWait
	interactiveAdmitWait = 20 * time.Millisecond // don't make the test wait the real 5s
	defer func() { interactiveAdmitWait = orig }()

	mgr := &Manager{config: &config.Config{PersonaDefault: "generic"}, limiter: lim}

	start := time.Now()
	_, err := mgr.RunTurn(context.Background(), TurnInput{UserMessage: "hi"}, &recordingSink{})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("RunTurn on a full box: got err=%v, want ErrAtCapacity", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("RunTurn blocked %v before rejecting; should give up after ~interactiveAdmitWait", elapsed)
	}
}

// TestManagerRunTurn_ToolCallUsesSandbox proves the sandbox wiring is live: the
// fake LLM calls the bash tool, RunTurn executes it in the (host-mode) sandbox,
// and the tool result — carrying the command's real stdout — flows back as a
// tool.result event and into the model's next step. Skips if bash is absent.
func TestManagerRunTurn_ToolCallUsesSandbox(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found; skipping sandbox tool-call exercise")
	}
	const probe = "sandbox-probe-7f3a"
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call_1", "echo "+probe),
		fakellm.TextStep("the command ran"),
	}})
	mgr := newFakeLLMManager(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sink := &recordingSink{}
	res, err := mgr.RunTurn(ctx, TurnInput{
		UserMessage: "run the probe",
		Model:       "anthropic/claude-opus-4.8",
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	for _, want := range []string{"tool.call", "tool.result", "turn.completed"} {
		if !sink.has(want) {
			t.Errorf("missing %q event; saw %v", want, sink.names())
		}
	}
	// The probe string must appear somewhere in the recorded tool.result payload,
	// proving the bash tool actually executed in the sandbox (not a stub).
	if !sandboxProbeEcho(sink, probe) {
		t.Errorf("tool.result did not carry the executed command's output %q; events=%v", probe, sink.names())
	}
	if strings.TrimSpace(res.FinalText) == "" {
		t.Error("FinalText empty after tool-call turn")
	}
}

// sandboxProbeEcho reports whether any recorded event payload contains the probe
// string (the executed command's stdout).
func sandboxProbeEcho(sink *recordingSink, probe string) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.events {
		if containsProbe(e.payload, probe) {
			return true
		}
	}
	return false
}

func containsProbe(payload any, probe string) bool {
	switch v := payload.(type) {
	case string:
		return strings.Contains(v, probe)
	case map[string]any:
		for _, val := range v {
			if containsProbe(val, probe) {
				return true
			}
		}
	}
	return false
}
