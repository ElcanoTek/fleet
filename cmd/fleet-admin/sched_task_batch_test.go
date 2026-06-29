// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeBatchStore is an in-memory batchTaskStore for deterministic, DB-free tests
// of the batch-create seam.
type fakeBatchStore struct {
	added      []*models.Task
	atomicSeen bool
	fail       error
}

func (f *fakeBatchStore) AddTaskBatch(_ context.Context, tasks []*models.Task, atomic bool) (int, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	f.atomicSeen = atomic
	f.added = append(f.added, tasks...)
	return len(tasks), nil
}

func TestBatchCreateTasks_AllValid(t *testing.T) {
	body := `[{"prompt":"Review file a"},{"prompt":"Review file b"}]`
	st := &fakeBatchStore{}
	res, err := batchCreateTasks(context.Background(), st, bytes.NewReader([]byte(body)), false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Count != 2 || len(res.Created) != 2 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want Count=2 Created=2 Failed=0", res)
	}
	if len(st.added) != 2 {
		t.Fatalf("added %d tasks, want 2", len(st.added))
	}
}

func TestBatchCreateTasks_PartialFailNonAtomic(t *testing.T) {
	// Index 1 has an empty prompt (validation failure); indices 0 and 2 valid.
	body := `[{"prompt":"Review file a"},{"prompt":"   "},{"prompt":"Review file c"}]`
	st := &fakeBatchStore{}
	res, err := batchCreateTasks(context.Background(), st, bytes.NewReader([]byte(body)), false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	if len(res.Created) != 2 || len(res.Failed) != 1 {
		t.Fatalf("len(Created)=%d len(Failed)=%d, want 2/1", len(res.Created), len(res.Failed))
	}
	if res.Failed[0].Index != 1 {
		t.Errorf("failed index = %d, want 1", res.Failed[0].Index)
	}
}

func TestBatchCreateTasks_AtomicAbortsOnValidationFailure(t *testing.T) {
	body := `[{"prompt":"Review file a"},{"prompt":""},{"prompt":"Review file c"}]`
	st := &fakeBatchStore{}
	res, err := batchCreateTasks(context.Background(), st, bytes.NewReader([]byte(body)), true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Count != 0 || len(res.Created) != 0 {
		t.Fatalf("atomic failure must create nothing, got %+v", res)
	}
	if len(res.Failed) < 2 {
		t.Fatalf("expected the failing index + aborted tail, got %d failures", len(res.Failed))
	}
	if len(st.added) != 0 {
		t.Fatalf("atomic failure must insert nothing, got %d", len(st.added))
	}
}

func TestBatchCreateTasks_AtomicDBErrorRollsBack(t *testing.T) {
	body := `[{"prompt":"Review file a"},{"prompt":"Review file b"}]`
	st := &fakeBatchStore{fail: errors.New("boom")}
	res, err := batchCreateTasks(context.Background(), st, bytes.NewReader([]byte(body)), true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Count != 0 || len(res.Created) != 0 || len(res.Failed) != 2 {
		t.Fatalf("atomic DB error must surface 2 failures, got %+v", res)
	}
	for _, f := range res.Failed {
		if !strings.Contains(f.Error, "rolled back") {
			t.Errorf("failure %d error = %q, want 'rolled back'", f.Index, f.Error)
		}
	}
}

func TestBatchCreateTasks_OversizedRejected(t *testing.T) {
	tasks := make([]models.TaskCreate, MaxBatchSize+1)
	for i := range tasks {
		tasks[i] = models.TaskCreate{Prompt: "x"}
	}
	body, _ := json.Marshal(tasks)
	st := &fakeBatchStore{}
	_, err := batchCreateTasks(context.Background(), st, bytes.NewReader(body), false)
	if err == nil {
		t.Fatal("expected an oversized error, got nil")
	}
}

func TestValidateBatchTaskCreate(t *testing.T) {
	cases := map[string]models.TaskCreate{
		"empty prompt":       {Prompt: "  "},
		"bad cron":           {Prompt: "x", Recurrence: "not a cron"},
		"mcp without server": {Prompt: "x", MCPSelection: models.MCPSelection{{Account: "a"}}},
	}
	for name, tc := range cases {
		if err := validateBatchTaskCreate(&tc); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
	good := models.TaskCreate{Prompt: "do it", Recurrence: "0 9 * * *", MCPSelection: models.MCPSelection{{Server: "acme"}}}
	if err := validateBatchTaskCreate(&good); err != nil {
		t.Fatalf("valid task rejected: %v", err)
	}
}
