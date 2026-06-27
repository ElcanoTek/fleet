package agentcore

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// roundsPolicy is a minimal Policy that finishes only at/after `finishAt`, so a
// test can drive a precise number of enforcement rounds without the scheduled
// audit dance. Its orchestration carries the run's LogSession, so per-step usage
// (and thus LastStepPromptTokens) flows exactly as in production.
type roundsPolicy struct {
	orch     *orchestrationState
	finishAt int
}

func newRoundsPolicy(session *LogSession, finishAt int) *roundsPolicy {
	return &roundsPolicy{orch: newOrchestrationState(session, 0), finishAt: finishAt}
}

func (p *roundsPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (p *roundsPolicy) RecordToolResult(string, string, string, bool)        {}
func (p *roundsPolicy) CanFinish(round int) (bool, []string) {
	if round < p.finishAt {
		return false, []string{"keep going"}
	}
	return true, nil
}
func (p *roundsPolicy) orchestration() *orchestrationState { return p.orch }

// capturingModel records the message slice handed to each Stream call and
// reports a caller-specified per-call input-token count, so a multi-round test
// can (a) drive the real LastStepPromptTokens signal and (b) assert which
// history actually reached the model after a proactive compaction.
type capturingModel struct {
	mockModel
	slug        string
	inputByCall []int

	recMu sync.Mutex
	seen  [][]fantasy.Message
}

func (m *capturingModel) Model() string { return m.slug }

func (m *capturingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.recMu.Lock()
	idx := len(m.seen)
	// call.Prompt is the message slice (type Prompt = []Message) handed to this
	// stream — the post-compaction history on the round after a compaction.
	m.seen = append(m.seen, append([]fantasy.Message(nil), call.Prompt...))
	var input int64
	if idx < len(m.inputByCall) {
		input = int64(m.inputByCall[idx])
	}
	m.recMu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: input, OutputTokens: 5},
		})
	}, nil
}

func (m *capturingModel) call(i int) []fantasy.Message {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	return m.seen[i]
}

func (m *capturingModel) callCountSeen() int {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	return len(m.seen)
}

// Tests for proactive context-window pressure detection (#209): the
// engine.proactiveCompact method, the env-driven thresholds, the token
// estimate, and the run-loop wiring that warns before — and compacts before —
// the provider ever rejects an oversized prompt.

// historyInput supplies a fixed system prompt + an arbitrary pre-built message
// history, so a test can drive the run loop with a conversation that is already
// near the model's context window.
type historyInput struct {
	system string
	msgs   []fantasy.Message
	label  string
}

func (h historyInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	return h.system, h.msgs, h.label, nil
}

// fillerMessages builds n user messages of `chars` bytes each. With the
// ~4-chars-per-token estimate, each message counts as chars/4 tokens, so the
// whole history estimates to n*(chars/4) tokens — letting a test land a precise
// fraction of a (small, test-controlled) context window.
func fillerMessages(n, chars int) []fantasy.Message {
	filler := strings.Repeat("x", chars)
	msgs := make([]fantasy.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, fantasy.NewUserMessage(filler))
	}
	return msgs
}

// msgText extracts the concatenated text of a message's text parts (Message
// holds []MessagePart, which has no Text() accessor of its own).
func msgText(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

func hasEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

// ── engine.proactiveCompact ──

func TestProactiveCompact_DropsOldestHalf(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// A clean proactive compaction must NOT count toward the reactive cap.
	e.consecutiveCompactions = 2

	// head(1) + 6 active messages → midpoint 3: drop the oldest 3, keep the
	// newest 3, splice one summary between the head and the kept tail.
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("HEAD"),
		fantasy.NewUserMessage("a"), fantasy.NewUserMessage("b"), fantasy.NewUserMessage("c"),
		fantasy.NewUserMessage("d"), fantasy.NewUserMessage("e"), fantasy.NewUserMessage("f"),
	}
	res := e.proactiveCompact(context.Background(), msgs)

	if !res.compacted {
		t.Fatal("expected compaction to happen")
	}
	if res.removedTurns != 3 {
		t.Errorf("removedTurns = %d, want 3", res.removedTurns)
	}
	// head + summary + 3 kept = 5.
	if len(res.messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5", len(res.messages))
	}
	if got := msgText(res.messages[0]); got != "HEAD" {
		t.Errorf("head not preserved: %q", got)
	}
	if !strings.Contains(msgText(res.messages[1]), compactionSummaryPrefix) {
		t.Errorf("summary marker missing: %q", msgText(res.messages[1]))
	}
	if got := msgText(res.messages[len(res.messages)-1]); got != "f" {
		t.Errorf("recent tail not preserved: %q", got)
	}
	if e.consecutiveCompactions != 0 {
		t.Errorf("consecutiveCompactions = %d, want 0 (proactive compaction must reset it)", e.consecutiveCompactions)
	}
}

func TestProactiveCompact_TooSmallIsNoop(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// head(1) + 1 active → midpoint 0 → nothing droppable.
	msgs := []fantasy.Message{fantasy.NewUserMessage("HEAD"), fantasy.NewUserMessage("only")}
	res := e.proactiveCompact(context.Background(), msgs)
	if res.compacted {
		t.Error("expected no compaction for a tiny history")
	}
	if len(res.messages) != 2 {
		t.Errorf("messages should be unchanged, got len %d", len(res.messages))
	}
}

func TestProactiveCompact_UsesSummarizerWhenSet(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	e.compactionSummarizer = func(_ context.Context, _ []fantasy.Message) fantasy.Message {
		return fantasy.NewUserMessage("LLM-SUMMARY")
	}
	msgs := fillerMessages(6, 8)
	res := e.proactiveCompact(context.Background(), msgs)
	if !res.compacted {
		t.Fatal("expected compaction")
	}
	if got := msgText(res.messages[1]); got != "LLM-SUMMARY" {
		t.Errorf("summarizer hook not used, summary = %q", got)
	}
}

// ── thresholds ──

func TestContextThresholds_Defaults(t *testing.T) {
	if got := contextPressureWarnThreshold(CanonicalEnvPrefix); got != defaultContextPressureWarnThreshold {
		t.Errorf("warn default = %v, want %v", got, defaultContextPressureWarnThreshold)
	}
	if got := contextCompactionThreshold(CanonicalEnvPrefix); got != defaultContextCompactionThreshold {
		t.Errorf("compaction default = %v, want %v", got, defaultContextCompactionThreshold)
	}
}

func TestContextThresholds_OverrideAndAlias(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD", "0.6")
	if got := contextPressureWarnThreshold(CanonicalEnvPrefix); got != 0.6 {
		t.Errorf("warn override = %v, want 0.6", got)
	}
	// Legacy CHAT_ alias resolves when the canonical var is unset.
	t.Setenv("CHAT_CONTEXT_COMPACTION_THRESHOLD", "0.8")
	if got := contextCompactionThreshold(CanonicalEnvPrefix); got != 0.8 {
		t.Errorf("compaction via CHAT_ alias = %v, want 0.8", got)
	}
}

func TestContextThresholds_ClampsOutOfRange(t *testing.T) {
	for _, bad := range []string{"0", "-0.2", "1.5", "nonsense"} {
		t.Setenv("FLEET_CONTEXT_COMPACTION_THRESHOLD", bad)
		if got := contextCompactionThreshold(CanonicalEnvPrefix); got != defaultContextCompactionThreshold {
			t.Errorf("threshold %q = %v, want fallback %v", bad, got, defaultContextCompactionThreshold)
		}
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	// 4 messages × 40 chars = 160 chars → 40 tokens (each 40/4 = 10).
	if got := estimateMessagesTokens(fillerMessages(4, 40)); got != 40 {
		t.Errorf("estimate = %d, want 40", got)
	}
	if got := estimateMessagesTokens(nil); got != 0 {
		t.Errorf("estimate of empty = %d, want 0", got)
	}
}

// ── run-loop integration ──

// testContextWindow is the small, fixed window the run-loop pressure tests pin
// for their mock model's slug, so the token estimates land at precise fractions.
const testContextWindow = 1000

// newStopModel builds a named mock whose stream finishes immediately, and pins a
// test-controlled context window for its slug so the pressure ratios are
// deterministic (the observed cache is consulted before any network path).
func newStopModel(slug string) *namedMockModel {
	recordContextMax(slug, testContextWindow)
	return &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				return streamStop()(nil, call)
			},
		},
		name: slug,
	}
}

// Interactive turns are single-round and never populate LastStepPromptTokens, so
// the estimate fallback is the sole signal: a turn that STARTS at ~78% of the
// window warns, but does not compact.
func TestRun_ContextPressure_InteractiveWarns(t *testing.T) {
	model := newStopModel("ctx209-inter-warn")
	obs := &captureObserver{}
	// 6 × 520 chars = 6 × 130 = 780 tokens → 0.78 of 1000: warn band [0.75,0.90).
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    historyInput{system: "s", msgs: fillerMessages(6, 520), label: "warn"},
		Observer: obs,
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextPressure) {
		t.Errorf("expected %s, got %v", evtContextPressure, obs.events)
	}
	if hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("did not expect compaction in the warn band, got %v", obs.events)
	}
}

// At ~96% the interactive turn compacts proactively before the (single) model
// call rather than just warning.
func TestRun_ContextPressure_InteractiveCompacts(t *testing.T) {
	model := newStopModel("ctx209-inter-compact")
	obs := &captureObserver{}
	// 6 × 640 chars = 6 × 160 = 960 tokens → 0.96 of 1000: compaction band.
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    historyInput{system: "s", msgs: fillerMessages(6, 640), label: "compact"},
		Observer: obs,
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("expected %s, got %v", evtContextCompacted, obs.events)
	}
}

// finishableScheduledPolicy returns a scheduled policy whose audit gate is
// pre-satisfied so the loop terminates in one round.
func finishableScheduledPolicy(session *LogSession) *ScheduledPolicy {
	p := NewScheduledPolicy(session, 50, 0, 0)
	p.orch.mu.Lock()
	p.orch.selfAuditRequested = true
	p.orch.selfAuditConfirmedOnce = true
	p.orch.mu.Unlock()
	return p
}

// A scheduled run at high pressure must NOT silently rewrite its transcript: it
// warns (event + a session-log breadcrumb) and leaves the history intact unless
// the operator opts in.
func TestRun_ContextPressure_ScheduledWarnsOnlyWithoutFlag(t *testing.T) {
	model := newStopModel("ctx209-sched-warn")
	obs := &captureObserver{}
	session := NewLogSession()
	_, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true}, Deps{
		Input:      historyInput{system: "s", msgs: fillerMessages(6, 640), label: "sched"},
		Observer:   obs,
		Policy:     finishableScheduledPolicy(session),
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextPressure) {
		t.Errorf("expected %s, got %v", evtContextPressure, obs.events)
	}
	if hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("scheduled run must not auto-compact without the opt-in flag, got %v", obs.events)
	}
	var sawBreadcrumb bool
	for _, m := range session.Messages {
		if strings.Contains(m.Content, "[context_pressure]") {
			sawBreadcrumb = true
		}
	}
	if !sawBreadcrumb {
		t.Error("expected a [context_pressure] breadcrumb in the session log")
	}
}

// With FLEET_SCHEDULED_AUTO_COMPACT=1 the operator has opted in, so the
// scheduled run compacts like interactive.
func TestRun_ContextPressure_ScheduledCompactsWithFlag(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "1")
	model := newStopModel("ctx209-sched-compact")
	obs := &captureObserver{}
	session := NewLogSession()
	_, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true}, Deps{
		Input:      historyInput{system: "s", msgs: fillerMessages(6, 640), label: "sched"},
		Observer:   obs,
		Policy:     finishableScheduledPolicy(session),
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("expected %s with the opt-in flag, got %v", evtContextCompacted, obs.events)
	}
}

// TestCheckContextPressure_DrivenByLastStepNotCumulative is the direct guard for
// the load-bearing signal choice: the per-call LastStepPromptTokens drives the
// trigger, and the CUMULATIVE PromptTokens must NOT — using the latter is the
// documented "compaction spiral" regression. The run-loop tests above only ever
// exercise the estimate fallback (single round, LastStepPromptTokens still 0),
// so this calls checkContextPressure directly with the counters preset.
func TestCheckContextPressure_DrivenByLastStepNotCumulative(t *testing.T) {
	slug := "ctx209-groundtruth"
	recordContextMax(slug, testContextWindow)
	model := &namedMockModel{name: slug}
	e := newMockEngine(t, model)
	obs := &captureObserver{}
	sink := newStreamSink(obs)

	// Tiny history: the estimate fallback alone is far below any threshold.
	msgs := fillerMessages(4, 20) // 4 × 5 = 20 tokens → 0.02 of 1000

	// A large CUMULATIVE total with a zero per-call size must stay silent.
	e.logSession.PromptTokens = 50_000
	e.logSession.LastStepPromptTokens = 0
	if res := e.checkContextPressure(context.Background(), msgs, model, sink, false); res.warned || len(obs.events) != 0 {
		t.Fatalf("cumulative PromptTokens must not drive the check (spiral guard); events=%v warned=%v", obs.events, res.warned)
	}

	// A real per-call input size near the window DOES drive compaction.
	e.logSession.LastStepPromptTokens = 950 // 0.95 of 1000
	e.checkContextPressure(context.Background(), msgs, model, sink, false)
	if !hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("ground-truth LastStepPromptTokens should drive compaction, got %v", obs.events)
	}
}

// TestRun_ContextPressure_MultiRoundCompactsFromGroundTruthAndFeedsForward drives
// two rounds: round 0's estimate is trivially small, round 1 reads the real
// 950-token LastStepPromptTokens reported by round 0's stream and compacts. It
// asserts the compacted, summary-spliced history is what actually REACHED the
// model on round 1 (not just that the event fired) — i.e. that the run loop
// wires `messages = pressure.messages` forward.
func TestRun_ContextPressure_MultiRoundCompactsFromGroundTruthAndFeedsForward(t *testing.T) {
	slug := "ctx209-multiround-compact"
	recordContextMax(slug, testContextWindow)
	session := NewLogSession()
	model := &capturingModel{slug: slug, inputByCall: []int{950, 950}}
	obs := &captureObserver{}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: fillerMessages(10, 20), label: "mr"}, // ~0.05 estimate at round 0
		Observer:   obs,
		Policy:     newRoundsPolicy(session, 1), // round 0 cannot finish, round 1 can → exactly 2 rounds
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextCompacted) {
		t.Fatalf("expected compaction off the round-1 ground-truth tokens, got %v", obs.events)
	}
	if model.callCountSeen() < 2 {
		t.Fatalf("expected >= 2 model calls, got %d", model.callCountSeen())
	}
	// The summary marker only appears in round 1's prompt if the compacted slice
	// was actually fed forward (a banner-without-wiring regression would send the
	// un-compacted history with no marker).
	var sawSummary bool
	for _, m := range model.call(1) {
		if strings.Contains(msgText(m), compactionSummaryPrefix) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Error("round-1 prompt to the model should contain the compaction summary marker")
	}
}

// TestRun_ContextPressure_MultiRoundWarnDedupAndReset pins the cross-round warn
// contract: a hovering run surfaces ONE banner, and a compaction RESETS the
// dedup so a later climb warns again. Round 0 warns off the estimate; round 1's
// real 950 tokens compact (resetting the flag); round 2's real 800 tokens warn
// again. Exactly two warn events ⇒ both the dedup (no third) and the reset (the
// second) hold; without the reset the second warn would be suppressed.
func TestRun_ContextPressure_MultiRoundWarnDedupAndReset(t *testing.T) {
	slug := "ctx209-multiround-warn"
	recordContextMax(slug, testContextWindow)
	session := NewLogSession()
	model := &capturingModel{slug: slug, inputByCall: []int{950, 800, 800}}
	obs := &captureObserver{}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: fillerMessages(8, 400), label: "warn"}, // 8 × 100 = 800 tokens → 0.80 estimate
		Observer:   obs,
		Policy:     newRoundsPolicy(session, 2), // rounds 0, 1, 2
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("expected a compaction on round 1, got %v", obs.events)
	}
	pressureCount := 0
	for _, e := range obs.events {
		if e == evtContextPressure {
			pressureCount++
		}
	}
	if pressureCount != 2 {
		t.Errorf("expected exactly 2 warn events (round 0, and round 2 after the compaction reset), got %d in %v", pressureCount, obs.events)
	}
}

// TestRun_ContextPressure_UnsplittableHistoryStillWarns guards the worst-case
// corner: a single over-limit message can't be compacted (proactiveCompact's
// documented no-op), and the run must still WARN rather than going silent at the
// moment pressure is highest.
func TestRun_ContextPressure_UnsplittableHistoryStillWarns(t *testing.T) {
	model := newStopModel("ctx209-unsplittable")
	obs := &captureObserver{}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    historyInput{system: "s", msgs: fillerMessages(1, 8000), label: "huge"}, // 2000 tokens vs 1000 window
		Observer: obs,
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(obs.events, evtContextPressure) {
		t.Errorf("an un-splittable over-limit history must still warn, got %v", obs.events)
	}
	if hasEvent(obs.events, evtContextCompacted) {
		t.Errorf("a single message cannot be compacted, got %v", obs.events)
	}
}
