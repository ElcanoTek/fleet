package acpruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// recordingVerifyBroker is the host-side end-of-run verifier seam for tests: it
// records what the agent shipped over `_fleet/verify` and returns a scripted
// verdict, standing in for the real runEndOfRunVerifier (which runs the host
// fallback model).
type recordingVerifyBroker struct {
	mu      sync.Mutex
	calls   int
	records []ToolExecRecord
	missing []string
	err     error
}

func (b *recordingVerifyBroker) Verify(_ context.Context, _ int, records []ToolExecRecord) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.records = append([]ToolExecRecord(nil), records...)
	return b.missing, b.err
}

func (b *recordingVerifyBroker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *recordingVerifyBroker) gotRecords() []ToolExecRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ToolExecRecord(nil), b.records...)
}

func hasSucceededTool(records []ToolExecRecord, name string) bool {
	for _, r := range records {
		if r.Name == name && r.Succeeded {
			return true
		}
	}
	return false
}

// TestACPGovern_ScheduledVerifierForcesRound is the end-to-end #35 proof: a
// scheduled native-acp run whose audit clears still reaches the HOST verifier over
// `_fleet/verify`, and a non-empty missing-actions verdict forces a final
// enforcement round — exactly as the in-process scheduledPolicy.CanFinish does,
// instead of silently finishing unverified. The agent ships its OWN authoritative
// tool-exec summary (so the bash it ran shows up as succeeded).
func TestACPGovern_ScheduledVerifierForcesRound(t *testing.T) {
	// Round 0: bash + confirm_audit (clears the inner audit gate). Steps then
	// exhaust, so the model emits final text and tries to finish.
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{
			{id: "c1", name: "bash", input: `{"command":"echo hi"}`},
			confirmAuditCall("c2"),
		},
	}}
	broker := &recordingVerifyBroker{missing: []string{"send the weekly report"}}
	spec := baseGovSchedSpec()
	spec.VerifierWired = true

	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{
			Executor: &recordingExecutor{},
			Observer: &recordingObserver{},
			Verifier: broker,
		},
	})

	// The verifier ran exactly once (attempt-once), host-side, over the seam.
	if broker.callCount() != 1 {
		t.Fatalf("verifier called %d times, want exactly 1 (attempt-once)", broker.callCount())
	}
	// It received the agent's authoritative tool-exec summary: the bash it ran.
	if recs := broker.gotRecords(); !hasSucceededTool(recs, "bash") {
		t.Fatalf("verifier records %+v missing a succeeded bash entry (agent should ship its own summary)", recs)
	}
	// The missing-actions verdict forced an EXTRA round: round 0 (tools), round 1
	// (final text, BLOCKED by the verifier), round 2 (final text, allowed). Without
	// the verifier the model would stream only twice.
	model.mu.Lock()
	streams := model.calls
	model.mu.Unlock()
	if streams < 3 {
		t.Fatalf("model streamed %d times, want >=3 (verifier must force a final round)", streams)
	}
}

// TestACPGovern_ScheduledVerifierFailOpen: a host-side verifier error must NOT
// block finishing — the run completes (end_turn) and no extra round is forced,
// mirroring the in-process verifier's "log and allow finish" on error.
func TestACPGovern_ScheduledVerifierFailOpen(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{
			{id: "c1", name: "bash", input: `{"command":"echo hi"}`},
			confirmAuditCall("c2"),
		},
	}}
	broker := &recordingVerifyBroker{err: errors.New("fallback model unavailable")}
	spec := baseGovSchedSpec()
	spec.VerifierWired = true

	// runGovHarness asserts end_turn (no toleratePromptErr) — so reaching it proves
	// the verifier error did not trap the loop.
	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{
			Executor: &recordingExecutor{},
			Observer: &recordingObserver{},
			Verifier: broker,
		},
	})

	if broker.callCount() != 1 {
		t.Fatalf("verifier called %d times, want exactly 1", broker.callCount())
	}
	model.mu.Lock()
	streams := model.calls
	model.mu.Unlock()
	if streams != 2 {
		t.Fatalf("model streamed %d times, want 2 (fail-open must not force an extra round)", streams)
	}
}

// TestACPGovern_ScheduledVerifierNotWired: when the host does NOT advertise a
// verifier (RunSpec.VerifierWired=false — the in-process gate when no fallback
// model exists), the agent never calls the seam, finishing exactly as a run with
// no verifier configured. The broker is wired but must stay untouched.
func TestACPGovern_ScheduledVerifierNotWired(t *testing.T) {
	model := &govScriptedModel{steps: [][]scriptToolCall{
		{confirmAuditCall("c1")},
	}}
	broker := &recordingVerifyBroker{missing: []string{"should not be consulted"}}
	spec := baseGovSchedSpec() // VerifierWired defaults false

	runGovHarness(t, govHarness{
		t: t, model: model, spec: spec,
		deps: Deps{
			Executor: &recordingExecutor{},
			Observer: &recordingObserver{},
			Verifier: broker,
		},
	})

	if broker.callCount() != 0 {
		t.Fatalf("verifier called %d times, want 0 when VerifierWired is false", broker.callCount())
	}
}

// --- focused unit tests for the agent-side pieces ---

// TestToolExecAccumulator pairs tool.call with tool.result the SAME way the host's
// buildToolExecSummary does: a result marks success from is_err, and a call with
// no result counts as failed.
func TestToolExecAccumulator(t *testing.T) {
	acc := newToolExecAccumulator()
	acc.Observe("tool.call", map[string]any{"id": "a", "name": "bash"})
	acc.Observe("tool.result", map[string]any{"id": "a", "is_err": false})
	acc.Observe("tool.call", map[string]any{"id": "b", "name": "send_email"})
	acc.Observe("tool.result", map[string]any{"id": "b", "is_err": true})
	acc.Observe("tool.call", map[string]any{"id": "c", "name": "mcp_acme_lookup"}) // no result → failed

	recs := acc.records()
	want := map[string]bool{"bash": true, "send_email": false, "mcp_acme_lookup": false}
	if len(recs) != len(want) {
		t.Fatalf("records = %+v, want %d entries", recs, len(want))
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.Name] = r.Succeeded
	}
	for name, succ := range want {
		if got[name] != succ {
			t.Errorf("record[%q].Succeeded = %v, want %v (all: %+v)", name, got[name], succ, recs)
		}
	}
}

// stubPolicy is an agentcore.Policy whose CanFinish always clears, so the verifier
// wrapper's own logic is the thing under test.
type stubPolicy struct{}

func (stubPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (stubPolicy) RecordToolResult(string, string, string, bool)        {}
func (stubPolicy) CanFinish(int) (bool, []string)                       { return true, nil }

// fakeVerifier is an injectable endOfRunVerifier.
type fakeVerifier struct {
	mu      sync.Mutex
	calls   int
	missing []string
	err     error
}

func (v *fakeVerifier) verify(int, []ToolExecRecord) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return v.missing, v.err
}

func TestVerifyingScheduledPolicy_CanFinish(t *testing.T) {
	t.Run("missing actions force a nudge round", func(t *testing.T) {
		fv := &fakeVerifier{missing: []string{"send the report"}}
		p := &verifyingScheduledPolicy{inner: stubPolicy{}, verifier: fv}
		ok, msgs := p.CanFinish(1)
		if ok || len(msgs) == 0 {
			t.Fatalf("CanFinish = (%v, %v), want (false, [nudge])", ok, msgs)
		}
	})

	t.Run("clean verdict allows finish", func(t *testing.T) {
		fv := &fakeVerifier{missing: nil}
		p := &verifyingScheduledPolicy{inner: stubPolicy{}, verifier: fv}
		if ok, _ := p.CanFinish(1); !ok {
			t.Fatal("CanFinish = false, want true on a clean verdict")
		}
	})

	t.Run("verifier error fails open and is attempted once", func(t *testing.T) {
		fv := &fakeVerifier{err: errors.New("boom")}
		p := &verifyingScheduledPolicy{inner: stubPolicy{}, verifier: fv}
		if ok, _ := p.CanFinish(1); !ok {
			t.Fatal("CanFinish = false, want true (fail-open on verifier error)")
		}
		// A second CanFinish must NOT re-run the verifier (attempt-once) and still allow finish.
		if ok, _ := p.CanFinish(2); !ok {
			t.Fatal("second CanFinish = false, want true")
		}
		if fv.calls != 1 {
			t.Fatalf("verifier called %d times, want exactly 1 (attempt-once)", fv.calls)
		}
	})

	t.Run("nil verifier is a passthrough", func(t *testing.T) {
		p := &verifyingScheduledPolicy{inner: stubPolicy{}}
		if ok, _ := p.CanFinish(1); !ok {
			t.Fatal("CanFinish = false, want true with no verifier")
		}
	})

	t.Run("inner block short-circuits before verifying", func(t *testing.T) {
		fv := &fakeVerifier{}
		p := &verifyingScheduledPolicy{inner: blockingPolicy{}, verifier: fv}
		if ok, _ := p.CanFinish(0); ok {
			t.Fatal("CanFinish = true, want false when inner blocks")
		}
		if fv.calls != 0 {
			t.Fatalf("verifier called %d times, want 0 when inner blocks finish", fv.calls)
		}
	})

	// Unwrap exposes the inner policy for agentcore's orchestration binding.
	var _ agentcore.PolicyUnwrapper = (*verifyingScheduledPolicy)(nil)
}

// blockingPolicy never lets the run finish (stands in for an unsatisfied audit gate).
type blockingPolicy struct{}

func (blockingPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (blockingPolicy) RecordToolResult(string, string, string, bool)        {}
func (blockingPolicy) CanFinish(int) (bool, []string)                       { return false, []string{"audit not confirmed"} }
