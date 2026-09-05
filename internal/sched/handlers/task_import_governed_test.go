// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func importEnvelope(t *testing.T, records ...models.TaskExportRecord) string {
	t.Helper()
	body, err := json.Marshal(models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: records})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(body)
}

// TestTaskImportRunsTheCreateGates pins POST /tasks/import as a metered bulk
// create, the sibling of /tasks/batch: imported tasks are attributed to the
// importing KEY (not only a user id, which a key principal never has — so a
// key-imported task was readable by nobody), the per-key priority ceiling
// applies per record, and an exhausted budget refuses the whole envelope with
// the create path's 402 before any row is written.
func TestTaskImportRunsTheCreateGates(t *testing.T) {
	env := setupGovernedCreate(t)
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, env.keyMgr, "client")

	t.Run("imported task is attributed to the importing key", func(t *testing.T) {
		w := env.post(t, "/tasks/import", clientKey, importEnvelope(t,
			models.TaskExportRecord{Name: "import-attrib", Prompt: "an imported prompt that is long enough"}))
		if w.Code != http.StatusOK {
			t.Fatalf("import = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp models.TaskImportResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Created != 1 || len(resp.Results) != 1 || resp.Results[0].ID == nil {
			t.Fatalf("import result = %+v, want one created record with an id", resp)
		}
		created, err := env.store.GetTask(*resp.Results[0].ID)
		if err != nil || created == nil {
			t.Fatalf("reload: %v", err)
		}
		if created.CreatedByKeyID == nil || *created.CreatedByKeyID != clientKeyID {
			t.Fatalf("CreatedByKeyID = %v, want %s", created.CreatedByKeyID, clientKeyID)
		}
	})

	t.Run("a record above the key's priority ceiling errors per record", func(t *testing.T) {
		forty := 40
		if err := env.keyMgr.SetMaxPriority(clientKeyID, &forty); err != nil {
			t.Fatalf("SetMaxPriority: %v", err)
		}
		t.Cleanup(func() { _ = env.keyMgr.SetMaxPriority(clientKeyID, nil) })
		w := env.post(t, "/tasks/import", clientKey, importEnvelope(t,
			models.TaskExportRecord{Name: "import-urgent", Prompt: "an urgent imported prompt, long enough", Priority: 10},
			models.TaskExportRecord{Name: "import-calm", Prompt: "a calm imported prompt, long enough", Priority: 60}))
		if w.Code != http.StatusMultiStatus {
			t.Fatalf("import with an over-cap record = %d, want 207: %s", w.Code, w.Body.String())
		}
		var resp models.TaskImportResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Errors != 1 || resp.Created != 1 {
			t.Fatalf("errors=%d created=%d, want 1/1: %+v", resp.Errors, resp.Created, resp.Results)
		}
		for _, res := range resp.Results {
			if res.Name == "import-urgent" && (res.Status != models.TaskImportErrored || !strings.Contains(res.Error, "ceiling")) {
				t.Fatalf("over-cap record = %+v, want errored naming the ceiling", res)
			}
		}
	})

	t.Run("exhausted budget refuses the envelope with 402 before any write", func(t *testing.T) {
		env.h.SetBudgetGate(refusingBudgetGate{})
		t.Cleanup(func() { env.h.SetBudgetGate(nil) })
		w := env.post(t, "/tasks/import", clientKey, importEnvelope(t,
			models.TaskExportRecord{Name: "import-budget", Prompt: "an imported prompt that must not land"}))
		if w.Code != http.StatusPaymentRequired {
			t.Fatalf("import over budget = %d, want 402: %s", w.Code, w.Body.String())
		}
		if got, err := env.store.GetTaskByName(t.Context(), "import-budget"); err != nil || got != nil {
			t.Fatalf("a refused import must write nothing: task=%v err=%v", got, err)
		}
	})
}
