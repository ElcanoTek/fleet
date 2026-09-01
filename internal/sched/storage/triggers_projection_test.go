// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package storage

// Drift guard for issue #1357: buildTriggerRun used to project the template
// task into the spawned run's TaskCreate field by hand-picked field, so every
// task field added after it (Persona, Title, Description, Tags, SandboxLimits,
// LoopConfig, WorktreeConfig, SerializationKey, ExpectedDurationMinutes, …)
// silently reset to its zero value on every webhook/email-triggered run. The
// projection is now models.TaskToCreate (the one canonical clone, itself
// completeness-tested) minus the explicit triggerRunNotCarried set — and this
// test fails whenever a TaskCreate field is neither carried verbatim nor
// registered there with a reason.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestTriggerRunProjectionAccountsForEveryField(t *testing.T) {
	var template models.Task
	fillTaskNonZero(reflect.ValueOf(&template).Elem())

	want := models.TaskToCreate(&template)
	got := triggerRunCreate(&template, "rendered prompt", connectorsDenied)

	gv := reflect.ValueOf(got)
	gt := gv.Type()
	for i := 0; i < gt.NumField(); i++ {
		name := gt.Field(i).Name
		reason, excluded := triggerRunNotCarried[name]
		if !excluded {
			if !reflect.DeepEqual(gv.Field(i).Interface(), reflect.ValueOf(want).Field(i).Interface()) {
				t.Errorf("triggerRunCreate dropped or altered definition field %q — a webhook/email-trigger run would silently lose it (issue #1357). Carry it, or register it in triggerRunNotCarried with a reason.", name)
			}
			continue
		}
		// Registered exclusions must actually be excluded: Prompt is replaced,
		// the connector facets are asserted per-mode below, everything else
		// must be zero. A registered-but-carried field means the registry and
		// the code disagree — fix whichever is wrong.
		switch name {
		case "Prompt", "MCPSelection", "CredentialAllowlist":
		default:
			if !gv.Field(i).IsZero() {
				t.Errorf("triggerRunCreate carried %q, but triggerRunNotCarried registers it as excluded (%s)", name, reason)
			}
		}
	}

	if got.Prompt != "rendered prompt" {
		t.Errorf("Prompt = %q, want the rendered trigger prompt", got.Prompt)
	}
	// Empty render falls back to the template's own prompt.
	if fb := triggerRunCreate(&template, "", connectorsDenied); fb.Prompt != template.Prompt {
		t.Errorf("empty render: Prompt = %q, want the template's %q", fb.Prompt, template.Prompt)
	}

	// The connector facets are the event-trigger security boundary (#511/#979):
	// only the inheritance switch may set them, per mode.
	denied := triggerRunCreate(&template, "p", connectorsDenied)
	if denied.MCPSelection != nil {
		t.Errorf("connectorsDenied must not carry the MCP selection, got %+v", denied.MCPSelection)
	}
	if denied.CredentialAllowlist == nil || len(denied.CredentialAllowlist) != 0 {
		t.Errorf("connectorsDenied must set an EXPLICIT empty (non-nil) credential allowlist — nil means inherit-global (#979), got %+v", denied.CredentialAllowlist)
	}
	mcpOnly := triggerRunCreate(&template, "p", connectorsMCPOnly)
	if !reflect.DeepEqual(mcpOnly.MCPSelection, template.MCPSelection) || mcpOnly.CredentialAllowlist != nil {
		t.Errorf("connectorsMCPOnly must carry ONLY the MCP selection: mcp=%+v cred=%+v", mcpOnly.MCPSelection, mcpOnly.CredentialAllowlist)
	}
	inherited := triggerRunCreate(&template, "p", connectorsInherited)
	if !reflect.DeepEqual(inherited.MCPSelection, template.MCPSelection) ||
		!reflect.DeepEqual(inherited.CredentialAllowlist, template.CredentialAllowlist) {
		t.Errorf("connectorsInherited must carry both facets: mcp=%+v cred=%+v", inherited.MCPSelection, inherited.CredentialAllowlist)
	}
}

// TestSpawnTriggerRun_CarriesDefinitionFields pins the #1357 symptom end to
// end: a webhook-spawned run must execute under the template's pinned persona
// (and keep the operator-facing definition fields), not the global defaults.
func TestSpawnTriggerRun_CarriesDefinitionFields(t *testing.T) {
	store, _ := newTestStore(t)
	expected := 45
	template := models.NewTask(models.TaskCreate{
		Prompt:                  "template prompt",
		TriggerType:             models.TriggerTypeWebhook,
		Persona:                 "security-auditor",
		Title:                   "Nightly audit",
		Description:             "Runbook: docs/audit.md",
		Tags:                    []string{"audit", "nightly"},
		SandboxLimits:           &models.TaskSandboxLimits{MemoryMB: 1024},
		ExpectedDurationMinutes: &expected,
	})
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("add template: %v", err)
	}
	trig := &models.TaskTrigger{TaskID: template.ID}

	runID, err := store.SpawnWebhookRun(context.Background(), trig, "rendered")
	if err != nil {
		t.Fatalf("SpawnWebhookRun: %v", err)
	}
	run, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if run.Persona != "security-auditor" {
		t.Errorf("spawned run persona = %q, want the template's pinned persona (issue #1357)", run.Persona)
	}
	if run.Title != "Nightly audit" || run.Description != "Runbook: docs/audit.md" {
		t.Errorf("spawned run lost title/description: title=%q description=%q", run.Title, run.Description)
	}
	if len(run.Tags) != 2 {
		t.Errorf("spawned run lost tags: %+v", run.Tags)
	}
	if run.SandboxLimits == nil || run.SandboxLimits.MemoryMB != 1024 {
		t.Errorf("spawned run lost sandbox limits: %+v", run.SandboxLimits)
	}
	if run.ExpectedDurationMinutes == nil || *run.ExpectedDurationMinutes != expected {
		t.Errorf("spawned run lost the SLA expectation: %+v", run.ExpectedDurationMinutes)
	}
	if run.Name != "" {
		t.Errorf("spawned run must not inherit the unique Name key, got %q", run.Name)
	}
}

// fillTaskNonZero mirrors the models package's fillNonZero (unexported there):
// every settable field gets a distinctive non-zero value so a dropped field is
// reliably detectable.
func fillTaskNonZero(v reflect.Value) {
	switch v.Type() {
	case reflect.TypeOf(time.Time{}):
		v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	case reflect.TypeOf(uuid.UUID{}):
		v.Set(reflect.ValueOf(uuid.New()))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("nonzero")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillTaskNonZero(v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillTaskNonZero(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillTaskNonZero(k)
		val := reflect.New(v.Type().Elem()).Elem()
		fillTaskNonZero(val)
		m.SetMapIndex(k, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				fillTaskNonZero(f)
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillTaskNonZero(v.Index(i))
		}
	default:
		// interface / chan / func — no Task definition field uses these kinds.
	}
}
