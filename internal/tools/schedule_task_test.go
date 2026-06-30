package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestScheduleTaskParamsValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      ScheduleTaskParams
		wantErr string // substring; "" = expect success
	}{
		{
			name:    "empty prompt rejected",
			in:      ScheduleTaskParams{Prompt: "   "},
			wantErr: "non-empty prompt",
		},
		{
			name:    "both cron and run_at rejected",
			in:      ScheduleTaskParams{Prompt: "do it", Cron: "0 9 * * *", RunAt: "2026-07-01T09:00:00Z"},
			wantErr: "EITHER cron",
		},
		{
			name:    "bad run_at rejected",
			in:      ScheduleTaskParams{Prompt: "do it", RunAt: "tomorrow at 9"},
			wantErr: "not a valid RFC3339",
		},
		{
			name: "valid cron accepted",
			in:   ScheduleTaskParams{Prompt: "weekly report", Cron: "0 9 * * MON"},
		},
		{
			name: "valid run_at accepted",
			in:   ScheduleTaskParams{Prompt: "one-off", RunAt: "2026-07-01T09:00:00Z"},
		},
		{
			name: "neither cron nor run_at accepted (run immediately)",
			in:   ScheduleTaskParams{Prompt: "right now"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestScheduleTaskParamsRunAtTime(t *testing.T) {
	// run_at parses to UTC.
	p := ScheduleTaskParams{RunAt: "2026-07-01T09:00:00+02:00"}
	got, ok := p.RunAtTime()
	if !ok {
		t.Fatalf("expected ok for a valid run_at")
	}
	want := time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("RunAtTime = %v, want %v (UTC-normalized)", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("RunAtTime location = %v, want UTC", got.Location())
	}

	// Empty run_at → not set.
	if _, ok := (ScheduleTaskParams{}).RunAtTime(); ok {
		t.Fatalf("expected ok=false for empty run_at")
	}
	// Unparseable run_at → not set (Validate surfaces the error).
	if _, ok := (ScheduleTaskParams{RunAt: "nope"}).RunAtTime(); ok {
		t.Fatalf("expected ok=false for unparseable run_at")
	}
}

// The tool's Run is a deliberate error: orchestration must intercept and stage
// the call for approval before it ever executes. If Run fires, the gate is
// mis-wired — assert the guard so a future refactor that drops the gate fails
// loudly here rather than silently no-op'ing a "scheduled" task.
func TestScheduleTaskToolRunIsGuardedError(t *testing.T) {
	tool := NewScheduleTaskTool()
	if got := tool.Info().Name; got != ScheduleTaskToolName {
		t.Fatalf("tool name = %q, want %q", got, ScheduleTaskToolName)
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name:  ScheduleTaskToolName,
		Input: `{"prompt":"do a thing","cron":"0 9 * * *"}`,
	})
	if err == nil {
		t.Fatalf("expected a guard error from direct Run, got nil")
	}
	if !resp.IsError {
		t.Fatalf("expected an error tool response, got %+v", resp)
	}
}
