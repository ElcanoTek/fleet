package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// A panic recovered under a task run's context carries the run's task_id and
// run_id: the panic record no longer writes an empty task_id of its own, and
// an empty call-site attribute never shadows the context's (#2074 review:
// runner.worker's panic line came out with task_id="").
func TestRecoverContextKeepsTheRunsTaskID(t *testing.T) {
	var buf bytes.Buffer
	safe.SetPanicHandler(contextAttrHandler{inner: NewHandler("json", &buf, slog.LevelDebug)})
	t.Cleanup(func() { safe.SetPanicHandler(nil) })

	ctx := WithTask(context.Background(), "task-7", "run-9")
	func() {
		defer safe.RecoverContext(ctx, "runner.worker", nil)
		panic("boom")
	}()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("%s: %v", buf.String(), err)
	}
	if m[KeyTaskID] != "task-7" || m[KeyRunID] != "run-9" {
		t.Fatalf("panic record = %v, want the context's task_id and run_id", m)
	}
	if _, ok := m["tool_name"]; ok {
		t.Fatalf("an empty metadata attribute was written: %v", m)
	}
}

// From's handler: a call site's empty-string attribute does not shadow the
// context's non-empty one of the same key; a non-empty one still wins.
func TestEmptyCallSiteAttrDoesNotShadowTheContext(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(NewHandler("json", &buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := WithTask(context.Background(), "task-7", "run-9")
	From(ctx).Info("x", KeyTaskID, "", KeyRunID, "run-explicit")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("%s: %v", buf.String(), err)
	}
	if m[KeyTaskID] != "task-7" || m[KeyRunID] != "run-explicit" {
		t.Fatalf("record = %v", m)
	}
}
