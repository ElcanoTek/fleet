package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestSessionTail(t *testing.T) {
	// nil / empty → "".
	if got := sessionTail(nil, 5, 100); got != "" {
		t.Errorf("nil session → %q, want empty", got)
	}
	if got := sessionTail(&models.LogSession{}, 5, 100); got != "" {
		t.Errorf("empty session → %q, want empty", got)
	}

	sess := &models.LogSession{Messages: []models.LogMessage{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "working on it"},
		{Role: "tool", Content: "boom: permission denied"},
	}}
	// Keeps only the last N, role-prefixed.
	got := sessionTail(sess, 2, 1000)
	if want := "assistant: working on it\ntool: boom: permission denied\n"; got != want {
		t.Errorf("tail(2) = %q, want %q", got, want)
	}

	// maxChars keeps the TAIL and stays valid UTF-8 even with multibyte content.
	multi := &models.LogSession{Messages: []models.LogMessage{{Role: "x", Content: string(make([]rune, 0))}}}
	multi.Messages[0].Content = ""
	for i := 0; i < 300; i++ {
		multi.Messages[0].Content += "é"
	}
	tail := sessionTail(multi, 1, 50)
	if len([]rune(tail)) > 50 {
		t.Errorf("tail rune len = %d, want ≤ 50", len([]rune(tail)))
	}
	for _, r := range tail {
		if r == '�' {
			t.Errorf("tail contains the replacement char (byte-split rune): %q", tail)
			break
		}
	}
}

// fakeAnalyzer records its invocation and returns canned validated JSON.
type fakeAnalyzer struct {
	called  chan struct{}
	gotErr  string
	gotTail string
	ret     json.RawMessage
	retErr  error
}

func (f *fakeAnalyzer) AnalyzeTaskFailure(_ context.Context, _, errMsg, sessionTail string) (json.RawMessage, error) {
	f.gotErr = errMsg
	f.gotTail = sessionTail
	close(f.called)
	return f.ret, f.retErr
}

// TestMaybeAnalyzeFailure_PersistsOnTerminal drives a task to a TERMINAL
// (non-retryable) failure and asserts the injected analyzer ran and its result
// was persisted to error_analysis via the lease-free path.
func TestMaybeAnalyzeFailure_PersistsOnTerminal(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)
	taskID := tasks[0].ID

	analyzer := &fakeAnalyzer{
		called: make(chan struct{}),
		ret:    json.RawMessage(`{"category":"tool_error","summary":"perm denied","remediation":["grant access"]}`),
	}
	// A plain error is non-retryable (not a transient class) → terminal dead-letter
	// branch → maybeAnalyzeFailure fires.
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s", Messages: []models.LogMessage{
			{Role: "assistant", Content: "trying"},
			{Role: "tool", Content: "boom"},
		}}, errors.New("boom: deterministic failure")
	})
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		ErrorAnalyzer:       analyzer,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)

	select {
	case <-analyzer.called:
	case <-time.After(3 * time.Second):
		t.Fatal("analyzer was not invoked on terminal failure")
	}
	if analyzer.gotErr == "" || analyzer.gotTail == "" {
		t.Errorf("analyzer received empty inputs: err=%q tail=%q", analyzer.gotErr, analyzer.gotTail)
	}

	// The async persistence races the analyzer callback; poll for it.
	waitFor(t, 3*time.Second, func() bool {
		task, err := store.GetTask(taskID)
		return err == nil && task != nil && len(task.ErrorAnalysis) > 0
	})
	task, _ := store.GetTask(taskID)
	// Postgres JSONB normalizes key order, so compare semantically, not byte-exact.
	var got, want map[string]any
	if err := json.Unmarshal(task.ErrorAnalysis, &got); err != nil {
		t.Fatalf("persisted error_analysis is not valid JSON: %v (%s)", err, task.ErrorAnalysis)
	}
	if err := json.Unmarshal(analyzer.ret, &want); err != nil {
		t.Fatalf("canned ret invalid: %v", err)
	}
	if got["category"] != want["category"] || got["summary"] != want["summary"] {
		t.Errorf("error_analysis = %s, want category/summary from %s", task.ErrorAnalysis, analyzer.ret)
	}
}

// TestMaybeAnalyzeFailure_SkipsOnSuccess confirms a SUCCESSFUL task never invokes
// the analyzer (the hook is on terminal-failure branches only).
func TestMaybeAnalyzeFailure_SkipsOnSuccess(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)
	taskID := tasks[0].ID

	analyzer := &fakeAnalyzer{called: make(chan struct{}), ret: json.RawMessage(`{"category":"unknown","summary":"x"}`)}
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s"}, nil // success
	})
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		ErrorAnalyzer:       analyzer,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)

	// Wait for the task to reach success.
	waitFor(t, 3*time.Second, func() bool {
		task, err := store.GetTask(taskID)
		return err == nil && task != nil && task.Status == models.TaskStatusSuccess
	})
	// The analyzer must NOT have been called, and error_analysis must be nil.
	select {
	case <-analyzer.called:
		t.Fatal("analyzer should not run on a successful task")
	case <-time.After(200 * time.Millisecond):
	}
	task, _ := store.GetTask(taskID)
	if len(task.ErrorAnalysis) != 0 {
		t.Errorf("successful task has error_analysis: %s", task.ErrorAnalysis)
	}
}
