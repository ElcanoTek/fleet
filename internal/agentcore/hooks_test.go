package agentcore

// #788 hook-engine unit tests. A scriptable fake Executor records the RunBash
// command string and returns scripted stdout/err, so hook decision parsing,
// matcher semantics, ordering, fail-open/closed, caps, redaction, and audit
// are all exercised without a real sandbox.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// scriptExecutor returns scripted output/err and records the last command.
type scriptExecutor struct {
	mu       sync.Mutex
	out      string
	err      error
	panics   bool
	commands []string
	fn       func(cmd string) (string, error) // optional per-command behavior
}

func (s *scriptExecutor) RunBash(_ context.Context, cmd string) (string, error) {
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.mu.Unlock()
	if s.panics {
		panic("executor boom")
	}
	if s.fn != nil {
		return s.fn(cmd)
	}
	return s.out, s.err
}
func (s *scriptExecutor) RunPython(context.Context, string) (string, error) { return "", nil }
func (s *scriptExecutor) lastCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commands) == 0 {
		return ""
	}
	return s.commands[len(s.commands)-1]
}

type recordObserver struct {
	mu     sync.Mutex
	events []map[string]any
}

func (o *recordObserver) Observe(eventType string, payload map[string]any) {
	if eventType != "hook.decision" {
		return
	}
	o.mu.Lock()
	m := map[string]any{"_type": eventType}
	for k, v := range payload {
		m[k] = v
	}
	o.events = append(o.events, m)
	o.mu.Unlock()
}
func (o *recordObserver) decisions() []map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]map[string]any(nil), o.events...)
}

func engineWith(t *testing.T, exec Executor, obs Observer, hooks ...LifecycleHook) *hookEngine {
	t.Helper()
	ConfigureLifecycleHooks(hooks)
	t.Cleanup(func() { ConfigureLifecycleHooks(nil) })
	return newHookEngine(ModeInteractive, exec, obs, "test-label")
}

func TestMatchesTool(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"", "bash", true}, {"*", "anything", true},
		{"bash", "bash", true}, {"bash", "run_python", false},
		{"mcp_*", "mcp_slack_send", true}, {"mcp_*", "bash", false},
	}
	for _, c := range cases {
		if got := matchesTool(c.matcher, c.tool); got != c.want {
			t.Errorf("matchesTool(%q,%q)=%v want %v", c.matcher, c.tool, got, c.want)
		}
	}
}

func TestHookEngine_NilIsNoOp(t *testing.T) {
	ConfigureLifecycleHooks(nil)
	e := newHookEngine(ModeInteractive, &stubExecutor{}, nil, "l")
	if e != nil {
		t.Fatal("no hooks configured → engine should be nil")
	}
	// All methods nil-safe.
	if b, _ := e.preToolUse(context.Background(), "bash", "c", "{}"); b {
		t.Error("nil engine preToolUse should not block")
	}
	if s := e.postToolUse(context.Background(), "bash", "c", "{}", "out", false); s != "" {
		t.Error("nil engine postToolUse should be empty")
	}
	e.turnEnd(context.Background(), "done", 1)
}

func TestHookEngine_PreBlockAndAudit(t *testing.T) {
	exec := &scriptExecutor{out: `{"decision":"block","reason":"denied by policy hook"}`}
	obs := &recordObserver{}
	e := engineWith(t, exec, obs, LifecycleHook{ID: "gate", Event: HookPreToolUse, Matcher: "bash", Command: "check"})

	blocked, reason := e.preToolUse(context.Background(), "bash", "c1", `{"command":"rm -rf /"}`)
	if !blocked || !strings.Contains(reason, "denied by policy hook") {
		t.Fatalf("expected block, got blocked=%v reason=%q", blocked, reason)
	}
	// The payload is piped via printf and the command bounded by `timeout`.
	cmd := exec.lastCommand()
	if !strings.Contains(cmd, "printf") || !strings.Contains(cmd, "timeout ") {
		t.Errorf("command should pipe payload + bound with timeout: %q", cmd)
	}
	if !strings.Contains(cmd, "hook_api_version") || !strings.Contains(cmd, "\"event\"") {
		t.Errorf("payload should carry versioned event fields: %q", cmd)
	}
	if strings.Contains(cmd, "OPENROUTER") || strings.Contains(cmd, "API_KEY") {
		t.Errorf("payload must not carry env/credentials: %q", cmd)
	}
	// One audit event with decision=block.
	evs := obs.decisions()
	if len(evs) != 1 || evs[0]["decision"] != "block" || evs[0]["hook_id"] != "gate" {
		t.Fatalf("audit events = %+v", evs)
	}
}

func TestHookEngine_MatcherFiltersAndOrder(t *testing.T) {
	// Two pre hooks; only the matching one fires. Then two matching, first-block-wins.
	var order []string
	exec := &scriptExecutor{fn: func(cmd string) (string, error) {
		// Identify which hook by its command marker embedded in the wrapper.
		switch {
		case strings.Contains(cmd, "H1"):
			order = append(order, "H1")
			return `{"decision":"block","reason":"h1 blocks"}`, nil
		case strings.Contains(cmd, "H2"):
			order = append(order, "H2")
			return `{"decision":"continue"}`, nil
		case strings.Contains(cmd, "OTHER"):
			order = append(order, "OTHER")
			return `{"decision":"continue"}`, nil
		}
		return `{"decision":"continue"}`, nil
	}}
	e := engineWith(t, exec, nil,
		LifecycleHook{ID: "other", Event: HookPreToolUse, Matcher: "run_python", Command: "echo OTHER"},
		LifecycleHook{ID: "h1", Event: HookPreToolUse, Matcher: "bash", Command: "echo H1"},
		LifecycleHook{ID: "h2", Event: HookPreToolUse, Matcher: "bash", Command: "echo H2"},
	)
	blocked, _ := e.preToolUse(context.Background(), "bash", "c", "{}")
	if !blocked {
		t.Fatal("expected block from h1")
	}
	// OTHER (matcher run_python) skipped; H1 fires and blocks; H2 skipped.
	if strings.Join(order, ",") != "H1" {
		t.Errorf("execution order = %v, want only H1 (matcher filter + first-block-wins)", order)
	}
}

func TestHookEngine_FailClosedVsObservable(t *testing.T) {
	ctx := context.Background()
	// Enforce hook + nonzero exit → block.
	execErr := &scriptExecutor{err: context.Canceled} // any RunBash error
	obs := &recordObserver{}
	e := engineWith(t, execErr, obs, LifecycleHook{ID: "enf", Event: HookPreToolUse, Command: "boom", Enforce: true})
	if blocked, reason := e.preToolUse(ctx, "bash", "c", "{}"); !blocked || !strings.Contains(reason, "enf") {
		t.Fatalf("enforce+error should block: blocked=%v reason=%q", blocked, reason)
	}

	// Advisory hook + error → continue, but audited with an error_class.
	obs2 := &recordObserver{}
	e2 := engineWith(t, &scriptExecutor{err: context.Canceled}, obs2,
		LifecycleHook{ID: "adv", Event: HookPreToolUse, Command: "boom"})
	if blocked, _ := e2.preToolUse(ctx, "bash", "c", "{}"); blocked {
		t.Fatal("advisory hook error should NOT block")
	}
	evs := obs2.decisions()
	if len(evs) != 1 || evs[0]["error_class"] == "" {
		t.Fatalf("advisory failure should audit an error_class: %+v", evs)
	}
}

func TestHookEngine_MalformedOutput(t *testing.T) {
	// No JSON line → malformed. Enforce blocks, advisory continues.
	e := engineWith(t, &scriptExecutor{out: "just some diagnostic text\nno json here"}, nil,
		LifecycleHook{ID: "enf", Event: HookPreToolUse, Command: "c", Enforce: true})
	if blocked, _ := e.preToolUse(context.Background(), "bash", "c", "{}"); !blocked {
		t.Fatal("enforce + malformed should block")
	}
}

func TestHookEngine_LastJSONLineWins(t *testing.T) {
	// A hook prints noise then its decision on the last JSON line.
	out := "starting analysis...\n{\"note\":\"ignored\"}\n{\"decision\":\"continue\",\"additional_context\":\"lint clean\"}"
	e := engineWith(t, &scriptExecutor{out: out}, nil,
		LifecycleHook{ID: "p", Event: HookPostToolUse, Matcher: "*", Command: "c"})
	frag := e.postToolUse(context.Background(), "edit_file", "c", "{}", "result", false)
	if !strings.Contains(frag, "lint clean") {
		t.Fatalf("post fragment = %q, want the last-line context", frag)
	}
	if !strings.HasPrefix(frag, "[hook:p]") {
		t.Errorf("fragment should be tagged with the hook id: %q", frag)
	}
}

func TestHookEngine_DecisionThenNoiseStillBlocks(t *testing.T) {
	// Fail-open regression: a hook prints its block verdict then a trailing
	// diagnostic JSON line. The noise line must NOT downgrade the block.
	out := "{\"decision\":\"block\",\"reason\":\"denied\"}\n{\"event\":\"done\"}"
	e := engineWith(t, &scriptExecutor{out: out}, nil,
		LifecycleHook{ID: "gate", Event: HookPreToolUse, Command: "c", Enforce: true})
	blocked, reason := e.preToolUse(context.Background(), "bash", "c", "{}")
	if !blocked || !strings.Contains(reason, "denied") {
		t.Fatalf("decision-then-noise must preserve the block: blocked=%v reason=%q", blocked, reason)
	}
	// A pure-diagnostic-only output (no decision-bearing line) is malformed →
	// enforce blocks.
	e2 := engineWith(t, &scriptExecutor{out: `{"event":"done"}`}, nil,
		LifecycleHook{ID: "g2", Event: HookPreToolUse, Command: "c", Enforce: true})
	if b, _ := e2.preToolUse(context.Background(), "bash", "c", "{}"); !b {
		t.Error("diagnostic-only output should be treated as malformed and blocked (enforce)")
	}
}

func TestHookEngine_ContextBudget(t *testing.T) {
	// A post hook returns a large fragment repeatedly; the per-run budget caps it.
	big := strings.Repeat("z", hookContextFragCap) // exactly the per-fragment cap
	e := engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"` + big + `"}`}, nil,
		LifecycleHook{ID: "p", Event: HookPostToolUse, Command: "c"})
	total := 0
	for i := 0; i < 20; i++ {
		frag := e.postToolUse(context.Background(), "bash", "c", "{}", "r", false)
		total += len(frag)
	}
	if total > hookRunContextBudget+len("[hook:p] ")*20 {
		t.Errorf("total appended context %d exceeded the per-run budget %d", total, hookRunContextBudget)
	}
}

func TestHookEngine_PanicContained(t *testing.T) {
	// A panicking Executor must be contained; enforce → block, not crash.
	e := engineWith(t, &scriptExecutor{panics: true}, &recordObserver{},
		LifecycleHook{ID: "p", Event: HookPreToolUse, Command: "c", Enforce: true})
	if blocked, _ := e.preToolUse(context.Background(), "bash", "c", "{}"); !blocked {
		t.Fatal("panicking enforce hook should block, not escape")
	}
}

func TestHookEngine_RedactsPayload(t *testing.T) {
	// A secret in the tool input must be redacted before it reaches the hook.
	exec := &scriptExecutor{out: `{"decision":"continue"}`}
	e := engineWith(t, exec, nil, LifecycleHook{ID: "p", Event: HookPreToolUse, Command: "c"})
	e.preToolUse(context.Background(), "bash", "c", `{"token":"sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"}`)
	if strings.Contains(exec.lastCommand(), "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
		t.Error("raw secret leaked into the hook payload; redaction did not run")
	}
}

func TestHookEngine_ConcurrentPostToolUse(t *testing.T) {
	// Race-safety on the shared context budget (run with -race).
	e := engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"ctx"}`}, nil,
		LifecycleHook{ID: "p", Event: HookPostToolUse, Command: "c"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.postToolUse(context.Background(), "bash", "c", "{}", "r", false)
		}()
	}
	wg.Wait()
}

func TestShellSingleQuote(t *testing.T) {
	// The wrapper must survive embedded single quotes and JSON.
	got := shellSingleQuote(`a'b"c`)
	if got != `'a'\''b"c'` {
		t.Errorf("shellSingleQuote = %q", got)
	}
}

func TestMessageText(t *testing.T) {
	m := fantasy.NewUserMessage("hello world")
	if messageText(m) != "hello world" {
		t.Errorf("messageText = %q", messageText(m))
	}
	_ = time.Second // keep time import if trimmed
}
