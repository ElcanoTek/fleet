package models

import "testing"

func strp(s string) *string { return &s }

// TestComputePipelineMetrics pins the derivation contract (#543): a tool turn
// is an assistant message with ≥1 tool call; distinct tools are unique names
// across the whole run; user/tool-result messages never count.
func TestComputePipelineMetrics(t *testing.T) {
	ls := &LogSession{
		PromptTokens:     1200,
		CompletionTokens: 300,
		Cost:             0.0421,
		CreatedAt:        1000,
		UpdatedAt:        1090,
		Messages: []LogMessage{
			{Role: "user", Content: "do the thing"},
			{Role: "assistant", ToolCalls: []LogToolCall{{Name: "bash"}, {Name: "run_python"}}},
			{Role: "tool", Content: "result", ToolCallID: strp("t1")},
			{Role: "assistant", ToolCalls: []LogToolCall{{Name: "bash"}}},
			{Role: "assistant", Content: "final answer, no tools"},
		},
	}
	m := ComputePipelineMetrics("task-1", ls)
	if m.ToolTurns != 2 || m.TotalToolCalls != 3 || m.DistinctTools != 2 || m.MaxCallsInOneTurn != 2 {
		t.Errorf("metrics = %+v", m)
	}
	if m.WallClockSeconds != 90 || m.PromptTokens != 1200 || m.CompletionTokens != 300 {
		t.Errorf("carry-through fields wrong: %+v", m)
	}

	// nil session and a session with a clock that went backwards degrade sanely.
	if z := ComputePipelineMetrics("x", nil); z.ToolTurns != 0 || z.WallClockSeconds != 0 {
		t.Errorf("nil session: %+v", z)
	}
	back := &LogSession{CreatedAt: 100, UpdatedAt: 50}
	if b := ComputePipelineMetrics("x", back); b.WallClockSeconds != 0 {
		t.Errorf("backwards clock should clamp to 0, got %d", b.WallClockSeconds)
	}
}

// TestAggregatePipelineMetrics pins the rollup: histogram banding, the ≥5
// long-pipeline share, and most-recent-first ordering of the runs slice.
func TestAggregatePipelineMetrics(t *testing.T) {
	runs := []RunPipelineMetrics{
		{TaskID: "a", ToolTurns: 0, CreatedAt: 1},
		{TaskID: "b", ToolTurns: 1, DistinctTools: 1, CreatedAt: 2},
		{TaskID: "c", ToolTurns: 3, DistinctTools: 2, CreatedAt: 3},
		{TaskID: "d", ToolTurns: 7, DistinctTools: 4, CreatedAt: 4},
		{TaskID: "e", ToolTurns: 12, DistinctTools: 6, CreatedAt: 5},
	}
	agg := AggregatePipelineMetrics(runs)
	if agg.Runs != 5 {
		t.Fatalf("runs = %d", agg.Runs)
	}
	want := map[string]int{"0": 1, "1": 1, "2-4": 1, "5-9": 1, "10+": 1}
	for k, v := range want {
		if agg.ToolTurnHistogram[k] != v {
			t.Errorf("histogram[%s] = %d, want %d", k, agg.ToolTurnHistogram[k], v)
		}
	}
	if agg.PctRunsAtLeast5ToolTurns != 40 {
		t.Errorf("pct ≥5 = %v, want 40", agg.PctRunsAtLeast5ToolTurns)
	}
	if runs[0].TaskID != "e" || runs[4].TaskID != "a" {
		t.Errorf("runs not sorted most-recent-first: %v", runs)
	}

	empty := AggregatePipelineMetrics(nil)
	if empty.Runs != 0 || empty.ToolTurnHistogram["0"] != 0 {
		t.Errorf("empty aggregate: %+v", empty)
	}
}

func TestPipelineMetricsAccumulator_MatchesAggregate(t *testing.T) {
	runs := []RunPipelineMetrics{
		{TaskID: "a", ToolTurns: 0, CreatedAt: 1},
		{TaskID: "b", ToolTurns: 1, DistinctTools: 1, CreatedAt: 2},
		{TaskID: "c", ToolTurns: 3, DistinctTools: 2, CreatedAt: 3},
		{TaskID: "d", ToolTurns: 7, DistinctTools: 4, CreatedAt: 4},
		{TaskID: "e", ToolTurns: 12, DistinctTools: 6, CreatedAt: 5},
	}
	acc := NewPipelineMetricsAccumulator()
	for _, r := range runs {
		acc.Add(r)
	}
	got := acc.Result()
	want := AggregatePipelineMetrics(append([]RunPipelineMetrics(nil), runs...))
	if got.Runs != want.Runs || got.PctRunsAtLeast5ToolTurns != want.PctRunsAtLeast5ToolTurns {
		t.Fatalf("acc=%+v want=%+v", got, want)
	}
	for k, v := range want.ToolTurnHistogram {
		if got.ToolTurnHistogram[k] != v {
			t.Errorf("histogram[%s] = %d, want %d", k, got.ToolTurnHistogram[k], v)
		}
	}
}

func TestRecentRuns_KeepsMostRecentN(t *testing.T) {
	r := NewRecentRuns(3)
	for _, m := range []RunPipelineMetrics{
		{TaskID: "old", CreatedAt: 1},
		{TaskID: "mid", CreatedAt: 2},
		{TaskID: "new", CreatedAt: 4},
		{TaskID: "older", CreatedAt: 0},
		{TaskID: "newest", CreatedAt: 5},
	} {
		r.Add(m)
	}
	got := r.Items()
	if len(got) != 3 {
		t.Fatalf("kept %d, want 3: %+v", len(got), got)
	}
	if got[0].TaskID != "newest" || got[1].TaskID != "new" || got[2].TaskID != "mid" {
		t.Errorf("order/contents = %v,%v,%v", got[0].TaskID, got[1].TaskID, got[2].TaskID)
	}
}

func TestRecentRuns_ZeroLimitHoldsNothing(t *testing.T) {
	r := NewRecentRuns(0)
	r.Add(RunPipelineMetrics{TaskID: "x", CreatedAt: 1})
	if items := r.Items(); len(items) != 0 {
		t.Fatalf("limit 0 kept %d", len(items))
	}
}

// The limit reaches NewRecentRuns from a request parameter, so the constructor
// bounds it itself rather than trusting the handler's clamp: a huge ?runs= must
// buy the requester MaxRecentRuns of memory, not the number they asked for.
func TestRecentRuns_ClampsLimitToMax(t *testing.T) {
	r := NewRecentRuns(1 << 40)
	for i := 0; i < MaxRecentRuns+10; i++ {
		r.Add(RunPipelineMetrics{TaskID: "t", CreatedAt: int64(i)})
	}
	if got := len(r.Items()); got != MaxRecentRuns {
		t.Fatalf("kept %d, want %d", got, MaxRecentRuns)
	}
	if got := cap(r.items); got > 2*MaxRecentRuns {
		t.Errorf("capacity %d far exceeds the %d bound", got, MaxRecentRuns)
	}
}

// A negative limit is a caller bug, not an invitation to panic in make().
func TestRecentRuns_NegativeLimitHoldsNothing(t *testing.T) {
	r := NewRecentRuns(-5)
	r.Add(RunPipelineMetrics{TaskID: "x", CreatedAt: 1})
	if items := r.Items(); len(items) != 0 {
		t.Fatalf("negative limit kept %d", len(items))
	}
}
