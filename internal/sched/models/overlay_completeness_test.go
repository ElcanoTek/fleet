package models

import (
	"reflect"
	"testing"
)

// TestOverlayTaskDefinitionCarriesEveryExportField is the drift guard for
// issue #1104, in the same spirit as TestTaskToCreateCarriesEveryDefinitionField:
// OverlayTaskDefinition is the SINGLE import conflict=replace overlay shared by
// the HTTP handler and the admin CLI (via storage.ReplaceTaskDefinition), and it
// is a hand-maintained assignment list — a future TaskExportRecord field that is
// exported and materialized by ExportRecordToTaskCreate but forgotten here would
// silently keep the pre-replace value on every export→edit→import-replace round
// trip (exactly the field drop the two hand-rolled per-path overlays had).
//
// The test fills a TaskExportRecord with all-non-zero values, overlays it onto a
// zero-definition task, and asserts the SAME-NAMED Task field is non-zero for
// every record field. TaskExportRecord fields map 1:1 onto Task fields by name
// (the record doc says so), so a failed lookup is itself a drift signal.
func TestOverlayTaskDefinitionCarriesEveryExportField(t *testing.T) {
	var rec TaskExportRecord
	fillNonZero(reflect.ValueOf(&rec).Elem())

	// Scheduled, not pending, so the record's run_if is allowed to apply.
	task := &Task{Status: TaskStatusScheduled}
	if err := OverlayTaskDefinition(task, ExportRecordToTaskCreate(rec)); err != nil {
		t.Fatalf("OverlayTaskDefinition: %v", err)
	}

	tv := reflect.ValueOf(task).Elem()
	rt := reflect.TypeOf(rec)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		f := tv.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("TaskExportRecord field %q has no same-named Task field — extend this test's mapping alongside the new field", name)
			continue
		}
		if f.IsZero() {
			t.Errorf("OverlayTaskDefinition did not carry export field %q — import conflict=replace would silently keep the old value (issue #1104). Add it to OverlayTaskDefinition.", name)
		}
	}
}

// TestOverlayTaskDefinitionNeverTouchesExecutionState pins the other half of
// the #1104 contract: the overlay writes DEFINITION fields only. Every Task
// field that is NOT a TaskExportRecord field is execution/runtime state
// (status, lease, attempts, results, lineage, skip/wake/dead-letter state, …)
// and must survive a replace byte-for-byte — the old full-row upsert rewound
// exactly these. The complement is computed by reflection so a new runtime
// field is covered automatically, and a new definition field fails the sibling
// carry test above until it is threaded through both the record and the overlay.
func TestOverlayTaskDefinitionNeverTouchesExecutionState(t *testing.T) {
	recFields := map[string]bool{}
	rt := reflect.TypeOf(TaskExportRecord{})
	for i := 0; i < rt.NumField(); i++ {
		recFields[rt.Field(i).Name] = true
	}

	var task Task
	fillNonZero(reflect.ValueOf(&task).Elem())
	snap := task

	// A zero record: every definition field is overwritten with its zero/default,
	// which is the harshest overlay — anything else it changes is a leak into
	// execution state. (The filled task's run_if is being REMOVED, which is
	// allowed in any editable status.)
	if err := OverlayTaskDefinition(&task, ExportRecordToTaskCreate(TaskExportRecord{})); err != nil {
		t.Fatalf("OverlayTaskDefinition: %v", err)
	}

	tv := reflect.ValueOf(task)
	sv := reflect.ValueOf(snap)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if recFields[name] {
			continue
		}
		if !reflect.DeepEqual(tv.Field(i).Interface(), sv.Field(i).Interface()) {
			t.Errorf("OverlayTaskDefinition modified execution-state field %q (%v -> %v) — replace must never copy or clear runtime state (issue #1104)",
				name, sv.Field(i).Interface(), tv.Field(i).Interface())
		}
	}
}

// TestOverlayTaskDefinitionRefusesGateOnPending mirrors the PUT /tasks edit
// rule the overlay inherited: a pending task is past the scheduled→pending
// promotion where run_if is evaluated, so attaching a NEW gate there is
// refused — and the refusal must leave the task entirely unmodified, since the
// import loop reports the record as errored and moves on.
func TestOverlayTaskDefinitionRefusesGateOnPending(t *testing.T) {
	task := Task{Status: TaskStatusPending, Prompt: "original prompt"}
	err := OverlayTaskDefinition(&task, TaskCreate{
		Prompt: "replacement prompt",
		RunIf:  &RunIf{Command: "true", TimeoutSeconds: 30},
	})
	if err == nil {
		t.Fatal("expected a refusal attaching run_if to a pending task")
	}
	if task.Prompt != "original prompt" || task.RunIf != nil {
		t.Errorf("refused overlay must leave the task unmodified, got %+v", task)
	}
}
