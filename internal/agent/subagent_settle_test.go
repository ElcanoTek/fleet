package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// #588 regression tests: releasing a finished child's budget reservation and
// charging its actual spend must be ATOMIC with respect to a concurrent
// reserveChildBudget. Before the fix the spawn defer released the reservation
// under a.mu and charged the spend later under only the orchestration's lock;
// a sibling reserving inside that gap read a remaining budget that reflected
// NEITHER the released reservation NOR the not-yet-charged spend, so it could
// be granted as if the finished child had never spent — over-committing the
// parent's remaining budget by up to that child's spend. Run under -race
// (make test-race covers this package).

// TestSettleChildBudget_AtomicWithConcurrentReserve hammers the reserve/settle
// primitives from many goroutines with staggered completions and probes, at
// every step, the hard-wall invariant the sub-agent budget split claims:
//
//	charged spend + outstanding reservations <= parent ceiling
//
// The probe reads both sides under a.mu — the same lock settleChildBudget holds
// across the release AND the charge — so with the fix the pair is always
// consistent; with the pre-#588 release-then-charge gap, a sibling's over-grant
// lands in the reservation before the finished child's spend is charged, and
// the very next probe sees the sum breach the ceiling.
func TestSettleChildBudget_AtomicWithConcurrentReserve(t *testing.T) {
	const ceiling = 1.00
	child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2}
	parent := newParentForSpawn(t, child, ceiling, 0 /*tokens unlimited*/, 2, 100)
	// The 100% per-child fraction (set by newParentForSpawn) makes each grant
	// take the parent's ENTIRE available budget, so while one child is in
	// flight every sibling reserve is correctly refused — the ONLY way a
	// sibling can be granted anything mid-flight is by observing the
	// release-before-charge gap this test exists to forbid.

	var (
		violationMu sync.Mutex
		violation   string
	)
	probe := func(stage string) {
		// Read the reservation and the charged spend in ONE a.mu critical
		// section (lock order a.mu → orch.mu, same as production) so the probe
		// itself cannot tear across a concurrent settle.
		parent.mu.Lock()
		reserved := parent.subagent.reservedCostUSD
		b := parent.runtimePolicy.Budget()
		parent.mu.Unlock()
		if b.SpentCostUSD+reserved > b.MaxCostUSD+1e-6 {
			violationMu.Lock()
			if violation == "" {
				violation = fmt.Sprintf("%s: spent $%.6f + reserved $%.6f exceeds ceiling $%.2f",
					stage, b.SpentCostUSD, reserved, b.MaxCostUSD)
			}
			violationMu.Unlock()
		}
	}

	// A budget poller contending on the orchestration lock from OUTSIDE a.mu:
	// with the atomic settle this is just read noise, but against the pre-#588
	// shape (release under a.mu, charge later under only orch.mu) it stretches
	// the release→charge gap, which is exactly the window a sibling's reserve
	// must not be able to observe.
	done := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = parent.runtimePolicy.Budget()
			}
		}
	}()

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1500; i++ {
				cost, tok, refusal := parent.reserveChildBudget(0, 0)
				// Probe on every iteration — refused workers keep observing, so
				// a violation surfaced by someone else's settle is still seen.
				probe("after reserve attempt")
				if refusal != "" {
					continue // spin: an in-flight sibling may settle and free budget
				}
				// The child "runs" (staggered completions), then settles HALF its
				// grant as actual spend. Against the pre-#588 gap, a sibling that
				// reserves between this child's release and its charge is granted
				// the parent's stale remaining — inflated by this child's spend —
				// and the probes above catch spent+reserved beyond the ceiling.
				time.Sleep(100 * time.Microsecond)
				parent.settleChildBudget(cost, tok, agentcore.RunUsage{CostUSD: cost * 0.5})
				probe("after settle")
			}
		}()
	}
	wg.Wait()
	close(done)
	pollWG.Wait()

	violationMu.Lock()
	defer violationMu.Unlock()
	if violation != "" {
		t.Fatalf("budget hard wall breached: %s", violation)
	}
	if b := parent.runtimePolicy.Budget(); b.SpentCostUSD > b.MaxCostUSD+1e-6 {
		t.Fatalf("final charged spend $%.6f exceeds parent ceiling $%.2f (over-granted in a release/charge gap)",
			b.SpentCostUSD, b.MaxCostUSD)
	}
}

// TestSpawn_ConcurrentSpawnsNeverOverCommitParentBudget drives the full spawn
// path: more concurrent spawns than fantasy's parallel-tool semaphore (5) with
// staggered starts (and therefore staggered defer-time settles — the window
// #588 identified as reachable only when max_children exceeds the semaphore).
// Whatever mix of grants and refusals the schedule produces, the total spend
// charged back to the parent must never exceed its cost ceiling.
func TestSpawn_ConcurrentSpawnsNeverOverCommitParentBudget(t *testing.T) {
	// Each child step charges $0.05 and every child requests a $0.05 ceiling, so
	// each successful spawn settles ~$0.05 against the parent's $0.30 — roughly
	// six of the twelve spawns can be granted, the rest must be refused.
	child := &budgetMockModel{name: "c", inTokens: 10, outTokens: 2, costUSD: 0.05}
	parent := newParentForSpawn(t, child, 0.30 /*cost ceiling*/, 0, 2, 20 /*maxChildren > 5*/)

	const spawns = 12
	var wg sync.WaitGroup
	for i := 0; i < spawns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Stagger the starts so later spawns reserve while earlier children
			// are completing — the release-before-charge gap's timing.
			time.Sleep(time.Duration(i) * time.Millisecond)
			resp, err := parent.spawn(context.Background(), spawnSubagentInput{
				Task:       "scoped subtask",
				MaxCostUSD: 0.05,
			})
			if err != nil {
				t.Errorf("spawn %d transport error: %v", i, err)
				return
			}
			if resp.Content == "" {
				t.Errorf("spawn %d returned an empty result", i)
			}
		}(i)
	}
	wg.Wait()

	b := parent.runtimePolicy.Budget()
	if b.SpentCostUSD > b.MaxCostUSD+1e-9 {
		t.Fatalf("concurrent spawns over-committed the parent budget: spent $%.6f > ceiling $%.2f",
			b.SpentCostUSD, b.MaxCostUSD)
	}
	if b.SpentCostUSD <= 0 {
		t.Fatal("expected at least one child's spend to be charged back to the parent")
	}
}
