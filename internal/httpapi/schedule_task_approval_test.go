package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestRunStagedScheduleTask drives the approval-resolution path (#239): an
// approved schedule_task call is parsed, validated, mapped to the injected
// scheduler seam, and formatted into a user-facing confirmation. The seam is a
// fake so no DB is needed — exactly the in-process round-trip the production
// wiring performs.
func TestRunStagedScheduleTask(t *testing.T) {
	var got TaskScheduleRequest
	s := &Server{
		cfg: &config.Config{PublicBaseURL: "https://fleet.example.com"},
		scheduleTask: func(_ context.Context, req TaskScheduleRequest) (*TaskScheduleResult, error) {
			got = req
			return &TaskScheduleResult{
				ID:      "11111111-2222-3333-4444-555555555555",
				Status:  "scheduled",
				NextRun: time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC),
			}, nil
		},
	}

	approval := &store.Approval{
		ToolName: "schedule_task",
		ArgsJSON: `{"name":"Weekly report","prompt":"Summarize the week's PRs","cron":"0 9 * * MON","model":"x","allow_network":true,"tags":["reports"]}`,
	}
	text, err := s.runStagedScheduleTask(context.Background(), approval)
	if err != nil {
		t.Fatalf("runStagedScheduleTask error: %v", err)
	}

	// The request was mapped faithfully.
	if got.Name != "Weekly report" || got.Prompt != "Summarize the week's PRs" {
		t.Fatalf("mapped request name/prompt wrong: %+v", got)
	}
	if got.Cron != "0 9 * * MON" || got.RunAt != nil {
		t.Fatalf("expected cron set, run_at nil; got %+v", got)
	}
	if got.Model != "x" || !got.AllowNetwork {
		t.Fatalf("model/network not mapped: %+v", got)
	}

	// The confirmation names the task, the schedule, the id, and links the Ops Center.
	for _, want := range []string{"Weekly report", "recurring", "scheduled", "11111111-2222", "https://fleet.example.com/orchestrator"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirmation missing %q:\n%s", want, text)
		}
	}
}

func TestRunStagedScheduleTask_OneTimeRunAt(t *testing.T) {
	s := &Server{
		cfg: &config.Config{}, // no PublicBaseURL → no link, but still succeeds
		scheduleTask: func(_ context.Context, req TaskScheduleRequest) (*TaskScheduleResult, error) {
			if req.RunAt == nil {
				t.Fatal("expected run_at to be set for a one-time task")
			}
			return &TaskScheduleResult{ID: "id-1", Status: "scheduled"}, nil
		},
	}
	approval := &store.Approval{
		ToolName: "schedule_task",
		ArgsJSON: `{"prompt":"do it once","run_at":"2026-07-01T09:00:00Z"}`,
	}
	text, err := s.runStagedScheduleTask(context.Background(), approval)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(text, "one-time") {
		t.Errorf("expected a one-time schedule line, got:\n%s", text)
	}
}

func TestRunStagedScheduleTask_RejectsInvalidArgs(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		scheduleTask: func(context.Context, TaskScheduleRequest) (*TaskScheduleResult, error) {
			t.Fatal("seam must not be called for invalid args")
			return nil, nil
		},
	}
	// Both cron and run_at → Validate rejects before the seam is touched.
	approval := &store.Approval{
		ToolName: "schedule_task",
		ArgsJSON: `{"prompt":"x","cron":"0 9 * * *","run_at":"2026-07-01T09:00:00Z"}`,
	}
	if _, err := s.runStagedScheduleTask(context.Background(), approval); err == nil {
		t.Fatal("expected validation error for both cron and run_at")
	}
}

func TestRunStagedScheduleTask_NilSeam(t *testing.T) {
	s := &Server{cfg: &config.Config{}} // scheduleTask nil → feature unconfigured
	approval := &store.Approval{ToolName: "schedule_task", ArgsJSON: `{"prompt":"x"}`}
	_, err := s.runStagedScheduleTask(context.Background(), approval)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil seam: err=%v, want a 'not configured' error", err)
	}
}

// TestRunStagedScheduleTask_SeamErrorPropagates ensures a storage failure (e.g. a
// duplicate task name) surfaces to the caller rather than being swallowed.
func TestRunStagedScheduleTask_SeamErrorPropagates(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		scheduleTask: func(context.Context, TaskScheduleRequest) (*TaskScheduleResult, error) {
			return nil, errors.New("task name already exists")
		},
	}
	approval := &store.Approval{ToolName: "schedule_task", ArgsJSON: `{"prompt":"x","name":"dup"}`}
	_, err := s.runStagedScheduleTask(context.Background(), approval)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected the seam error to propagate, got %v", err)
	}
}

func TestSummarizeScheduleTaskInput(t *testing.T) {
	// Recurring task: carries cron + a runs-per-month estimate.
	got := summarizeScheduleTaskInput("schedule_task",
		`{"name":"daily","prompt":"`+strings.Repeat("p", 250)+`","cron":"0 0 * * *"}`)
	if got["recurring"] != true {
		t.Errorf("expected recurring=true, got %v", got["recurring"])
	}
	if got["cron"] != "0 0 * * *" {
		t.Errorf("cron = %v", got["cron"])
	}
	// "@daily" → ~30 firings in 30 days.
	if n, ok := got["runs_per_month"].(int); !ok || n < 28 || n > 32 {
		t.Errorf("runs_per_month for a daily cron = %v, want ~30", got["runs_per_month"])
	}
	// Prompt preview is truncated to 200 chars + ellipsis.
	if preview, _ := got["prompt_preview"].(string); len([]rune(preview)) != 201 || !strings.HasSuffix(preview, "…") {
		t.Errorf("prompt_preview not truncated to 200 chars + ellipsis: len=%d", len([]rune(preview)))
	}

	// Multibyte prompt must truncate by rune (not byte) and stay valid UTF-8 —
	// 250 'é' (2 bytes each) would split mid-rune under byte truncation.
	got = summarizeScheduleTaskInput("schedule_task",
		`{"prompt":"`+strings.Repeat("é", 250)+`","cron":"0 0 * * *"}`)
	preview, _ := got["prompt_preview"].(string)
	if !utf8.ValidString(preview) {
		t.Errorf("multibyte prompt_preview is not valid UTF-8: %q", preview)
	}
	if len([]rune(preview)) != 201 {
		t.Errorf("multibyte prompt_preview rune len = %d, want 201 (200 + ellipsis)", len([]rune(preview)))
	}

	// One-time task: carries run_at, recurring=false, no frequency.
	got = summarizeScheduleTaskInput("schedule_task", `{"prompt":"once","run_at":"2026-07-01T09:00:00Z"}`)
	if got["recurring"] != false || got["run_at"] != "2026-07-01T09:00:00Z" {
		t.Errorf("one-time summary wrong: %+v", got)
	}
	if _, has := got["runs_per_month"]; has {
		t.Errorf("one-time task should not carry runs_per_month")
	}

	// Run-immediately: neither cron nor run_at.
	got = summarizeScheduleTaskInput("schedule_task", `{"prompt":"now"}`)
	if got["run_immediately"] != true {
		t.Errorf("expected run_immediately=true, got %+v", got)
	}
}

func TestEstimateRunsPerMonth(t *testing.T) {
	// Hourly → ~720/month (24*30); daily → ~30; weekly → ~4.
	for _, tc := range []struct {
		cron    string
		lo, hi  int
		wantErr bool
	}{
		{cron: "0 * * * *", lo: 700, hi: 744}, // hourly
		{cron: "0 0 * * *", lo: 28, hi: 32},   // daily
		{cron: "0 9 * * MON", lo: 3, hi: 6},   // weekly
		{cron: "not a cron", wantErr: true},
	} {
		n, ok := estimateRunsPerMonth(tc.cron)
		if tc.wantErr {
			if ok {
				t.Errorf("cron %q: expected ok=false", tc.cron)
			}
			continue
		}
		if !ok {
			t.Errorf("cron %q: expected ok=true", tc.cron)
			continue
		}
		if n < tc.lo || n > tc.hi {
			t.Errorf("cron %q: runs/month = %d, want [%d,%d]", tc.cron, n, tc.lo, tc.hi)
		}
	}

	// Per-minute cron is capped, never an unbounded walk.
	n, ok := estimateRunsPerMonth("* * * * *")
	if !ok || n != runsPerMonthCountCap {
		t.Errorf("per-minute cron should hit the cap %d, got %d (ok=%v)", runsPerMonthCountCap, n, ok)
	}

	// An impossible-but-parseable date (Feb 30) parses, but Next() never fires →
	// must report 0, NOT spin to the cap and claim "1000+ runs/month".
	if n, ok := estimateRunsPerMonth("0 0 30 2 *"); !ok || n != 0 {
		t.Errorf("impossible cron (Feb 30): got %d (ok=%v), want 0", n, ok)
	}
}

func TestRejectionMessages(t *testing.T) {
	claim, history := rejectionMessages("schedule_task")
	if !strings.Contains(claim, "scheduled task") || !strings.Contains(history, "scheduled task") {
		t.Errorf("schedule_task rejection wording wrong: %q / %q", claim, history)
	}
	claim, _ = rejectionMessages("mcp_sendgrid_send_email")
	if !strings.Contains(claim, "send") {
		t.Errorf("email rejection wording wrong: %q", claim)
	}
}
