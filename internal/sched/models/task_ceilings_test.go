// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package models

import "testing"

// A task's own per-run ceilings (#1533) are definition fields: carried by the
// clone recipe to every occurrence, re-run and clone, and round-tripped
// through the export record; nil stays nil (inherit the deployment ceiling).
func TestPerTaskCeilingsCarryThroughCloneAndExport(t *testing.T) {
	cost, tokens := 12.5, 30_000_000
	task := NewTask(TaskCreate{Prompt: "p", MaxCostUSD: &cost, MaxTotalTokens: &tokens})
	if task.MaxCostUSD == nil || *task.MaxCostUSD != 12.5 || task.MaxTotalTokens == nil || *task.MaxTotalTokens != 30_000_000 {
		t.Fatalf("NewTask dropped the ceilings: cost=%v tokens=%v", task.MaxCostUSD, task.MaxTotalTokens)
	}
	next := NewTask(TaskToCreate(task))
	if next.MaxCostUSD == nil || *next.MaxCostUSD != 12.5 || next.MaxTotalTokens == nil || *next.MaxTotalTokens != 30_000_000 {
		t.Fatalf("clone recipe dropped the ceilings: cost=%v tokens=%v", next.MaxCostUSD, next.MaxTotalTokens)
	}
	rec := TaskToExportRecord(task)
	if rec.MaxCostUSD == nil || *rec.MaxCostUSD != 12.5 || rec.MaxTotalTokens == nil || *rec.MaxTotalTokens != 30_000_000 {
		t.Fatalf("export dropped the ceilings: %+v", rec)
	}
	imported := NewTask(ExportRecordToTaskCreate(rec))
	if imported.MaxCostUSD == nil || *imported.MaxCostUSD != 12.5 || imported.MaxTotalTokens == nil || *imported.MaxTotalTokens != 30_000_000 {
		t.Fatalf("import dropped the ceilings: cost=%v tokens=%v", imported.MaxCostUSD, imported.MaxTotalTokens)
	}

	plain := NewTask(TaskCreate{Prompt: "p"})
	if plain.MaxCostUSD != nil || plain.MaxTotalTokens != nil {
		t.Fatal("omitted ceilings must stay nil (inherit the deployment ceiling)")
	}
	if rec := TaskToExportRecord(plain); rec.MaxCostUSD != nil || rec.MaxTotalTokens != nil {
		t.Fatal("omitted ceilings must export as unset")
	}
	// Overlay (import-replace) carries them too.
	edited := NewTask(TaskCreate{Prompt: "p"})
	if err := OverlayTaskDefinition(edited, TaskToCreate(task)); err != nil {
		t.Fatal(err)
	}
	if edited.MaxCostUSD == nil || *edited.MaxCostUSD != 12.5 {
		t.Fatalf("overlay dropped max_cost_usd: %v", edited.MaxCostUSD)
	}
}
