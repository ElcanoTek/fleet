package agentcore

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// These tests target the SHARED loop directly (not either driver's legacy
// suite), which is the gap the readiness audit (#89) flagged: each driver's
// suite passes if ITS mode works, so a shared-code change that rebalances
// behavior between modes can stay green in both while breaking the other.
// They assert (1) invariants that must hold for EVERY mode, (2) the genuine
// per-mode divergence, and (3) — structurally — that divergence stays in the
// seams and never leaks into the trunk.

// runModeCleanPass drives one clean single-pass turn in `mode` against the
// streamStop fake (one finish, usage 50/10) and returns the result. For
// scheduled mode the audit gate is pre-satisfied so CanFinish clears on round 0,
// giving the degenerate single-round run that should match interactive.
func runModeCleanPass(t *testing.T, mode Mode) (Result, *captureObserver, *roundCountingPolicy) {
	t.Helper()
	model := &mockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text-1", Delta: "done"})
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonStop,
					Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
				})
			}, nil
		},
	}
	var inner Policy
	if mode == ModeScheduled {
		sp := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
		sp.orch.mu.Lock()
		sp.orch.selfAuditRequested = true
		sp.orch.selfAuditConfirmedOnce = true
		sp.orch.mu.Unlock()
		inner = sp
	} else {
		inner = NewInteractivePolicy(0, 0, nil, nil)
	}
	policy := &roundCountingPolicy{inner: inner}
	obs := &captureObserver{}
	res, err := Run(context.Background(), mode, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		Temperature: 0.2,
	}, Deps{
		Input:    stubInput{system: "sys", user: "hi", label: "turn-" + mode.String()},
		Observer: obs,
		Policy:   policy,
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("[%s] Run error: %v", mode, err)
	}
	return res, obs, policy
}

// TestModeParity_SharedInvariants asserts the invariants that must hold for
// EVERY mode on a clean single pass: usage is accounted identically (the
// "accounting drift" risk), the label is echoed, the observer saw events, and
// the loop collapsed to exactly one round.
func TestModeParity_SharedInvariants(t *testing.T) {
	for _, mode := range []Mode{ModeInteractive, ModeScheduled} {
		t.Run(mode.String(), func(t *testing.T) {
			res, obs, policy := runModeCleanPass(t, mode)

			// Usage accounting must be identical across modes — the streamStop
			// fake reports 50 input / 10 output, and nothing should drop it.
			if res.Usage.PromptTokens != 50 || res.Usage.CompletionTokens != 10 {
				t.Errorf("usage not accounted: got prompt=%d completion=%d, want 50/10",
					res.Usage.PromptTokens, res.Usage.CompletionTokens)
			}
			if res.Label != "turn-"+mode.String() {
				t.Errorf("label = %q, want echoed input label", res.Label)
			}
			if res.FinalText != "done" {
				t.Errorf("final text = %q, want %q (streaming bridge must carry text in both modes)", res.FinalText, "done")
			}
			if len(obs.events) == 0 {
				t.Error("observer recorded no events for a turn that streamed text")
			}
			if res.Rounds != 1 {
				t.Errorf("clean pass should be exactly 1 round, got %d", res.Rounds)
			}
			// No double-finish: CanFinish consulted exactly once on a clean pass.
			if policy.finishes != 1 {
				t.Errorf("CanFinish consulted %d times, want exactly 1", policy.finishes)
			}
		})
	}
}

// TestModeParity_Divergence pins the ONE genuine divergence: interactive
// finishes on round 0 unconditionally, while scheduled loops until its audit
// gate clears (so an un-satisfied audit forces a second round). This is the
// behavior the "interactive == scheduled with rounds collapsed" framing rests
// on; if a shared-code change broke it, this fails.
func TestModeParity_Divergence(t *testing.T) {
	// Scheduled with the audit UNSATISFIED until round 1 must run >1 round.
	sp := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	round := 0
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			round++
			if round == 2 {
				sp.orch.mu.Lock()
				sp.orch.selfAuditRequested = true
				sp.orch.selfAuditConfirmedOnce = true
				sp.orch.mu.Unlock()
			}
			return streamStop()(nil, call)
		},
	}
	res, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    stubInput{system: "sys", user: "do it", label: "sched"},
		Observer: &captureObserver{},
		Policy:   sp,
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("scheduled Run error: %v", err)
	}
	if res.Rounds < 2 {
		t.Errorf("scheduled run with an un-cleared audit should run >1 round, got %d", res.Rounds)
	}
	// Usage must accumulate ACROSS rounds (the audit's #1 flagged risk: ceiling /
	// accounting drift across the round boundary). Two streams → 2×(50/10).
	if res.Usage.PromptTokens != 100 || res.Usage.CompletionTokens != 20 {
		t.Errorf("multi-round usage did not accumulate: got prompt=%d completion=%d, want 100/20",
			res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}

// TestSeamPurity_NoModeBranchInTrunk is the structural guard for the whole
// "one loop, Mode + four seams are the only divergence" thesis: the trunk must
// not branch on the Mode enum. Divergence belongs in the seam constructors
// (seams.go defines the enum; policy.go/the drivers pick behavior) — a
// `switch mode` / `== ModeInteractive` in engine.go/run.go/orchestration.go is
// exactly the hidden "fifth seam" that lets one mode's change silently break the
// other. Passing `Mode: mode` as struct DATA to a seam (FinalizeInput) is fine;
// BRANCHING on it in the trunk is not.
func TestSeamPurity_NoModeBranchInTrunk(t *testing.T) {
	// Branch constructs that must never appear in the trunk.
	branches := []*regexp.Regexp{
		regexp.MustCompile(`\bswitch\s+mode\b`),
		regexp.MustCompile(`==\s*Mode(Interactive|Scheduled)\b`),
		regexp.MustCompile(`\bMode(Interactive|Scheduled)\s*==`),
		regexp.MustCompile(`\bcase\s+Mode(Interactive|Scheduled)\b`),
		regexp.MustCompile(`\bmode\s*==`),
	}
	// seams.go legitimately defines + stringifies the enum; tests are exempt.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "seams.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			for _, re := range branches {
				if re.MatchString(line) {
					t.Errorf("%s:%d branches on Mode in the trunk (move divergence into a seam): %s",
						name, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no trunk files — the guard is not actually running")
	}
}
