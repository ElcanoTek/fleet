// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestExtractTemplateVars(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   []string
	}{
		{"none", "just a plain prompt", []string{}},
		{"single", "review {repo_path} please", []string{"repo_path"}},
		{"sorted+deduped", "use {b} then {a} then {b} again", []string{"a", "b"}},
		{"underscore+digits", "hello {user_name} on {date2}", []string{"date2", "user_name"}},
		{"empty-braces-ignored", "nothing here {} or { }", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTemplateVars(tc.prompt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("extractTemplateVars(%q) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}

func TestListTaskTemplates_EmptyWhenNoProvider(t *testing.T) {
	h := &Handlers{} // nil taskTemplates provider
	rr := httptest.NewRecorder()
	h.ListTaskTemplates(rr, httptest.NewRequest(http.MethodGet, "/task-templates", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// A nil provider must still yield a well-formed empty ARRAY (not null), so the
	// UI can branch on length and simply suppress the template section.
	var body []wireTaskTemplate
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body == nil {
		t.Fatal("body is null; want []")
	}
	if len(body) != 0 {
		t.Fatalf("body = %+v, want empty", body)
	}
}

func TestListTaskTemplates_FromProvider(t *testing.T) {
	h := &Handlers{}
	h.SetTaskTemplateProvider(func() []clientconfig.TaskTemplate {
		return []clientconfig.TaskTemplate{
			{
				Name:        "Code Review",
				Description: "Review changes",
				Icon:        "🔍",
				Task: clientconfig.TaskTemplateTask{
					Prompt:        "Review {repo_path} for issues in {area}.",
					Model:         strptr("anthropic/claude-opus-4.8"),
					MaxIterations: intptr(25),
					Tags:          []string{"review"},
				},
			},
		}
	})
	rr := httptest.NewRecorder()
	h.ListTaskTemplates(rr, httptest.NewRequest(http.MethodGet, "/task-templates", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body []wireTaskTemplate
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("templates = %d, want 1", len(body))
	}
	got := body[0]
	if got.Name != "Code Review" || got.Icon != "🔍" {
		t.Errorf("template head wrong: %+v", got)
	}
	// Variables are extracted from the prompt, sorted + deduped.
	if want := []string{"area", "repo_path"}; !reflect.DeepEqual(got.Variables, want) {
		t.Errorf("Variables = %v, want %v", got.Variables, want)
	}
	if got.Task.Model == nil || *got.Task.Model != "anthropic/claude-opus-4.8" {
		t.Errorf("Task.Model = %v", got.Task.Model)
	}
}

// TestTaskTemplateAppliesToTaskCreate is the "from-template field application"
// half of the feature contract. It proves the partial payload a template serves
// over the wire decodes cleanly into a real models.TaskCreate AND passes the
// SAME validation the live POST /tasks path runs — so a task created from a
// template is a legitimate create request, not a shape that only the UI
// understands. Per-invocation substitution of {variable} placeholders is a
// UI-side concern; here we substitute then assert the create path accepts it.
func TestTaskTemplateAppliesToTaskCreate(t *testing.T) {
	tmpl := clientconfig.TaskTemplate{
		Name: "Code Review",
		Task: clientconfig.TaskTemplateTask{
			Prompt:        "Review {repo_path} for bugs.",
			Model:         strptr("anthropic/claude-opus-4.8"),
			FallbackModel: strptr("anthropic/claude-sonnet-4-6"),
			MaxIterations: intptr(25),
			Recurrence:    "0 9 * * 1-5",
			Timezone:      "America/New_York",
			Persona:       "security-auditor",
			Tags:          []string{"review", "code"},
		},
	}

	// Marshal the template's task payload, then decode it back into a TaskCreate —
	// exactly the round-trip the UI -> POST /tasks flow performs (the wire shape is
	// JSON either way). The {variable} is substituted as the UI would before send.
	raw, err := json.Marshal(tmpl.Task)
	if err != nil {
		t.Fatalf("marshal template task: %v", err)
	}
	var tc models.TaskCreate
	if err := json.Unmarshal(raw, &tc); err != nil {
		t.Fatalf("decode template task into TaskCreate: %v", err)
	}
	// Substitute the placeholder (UI-side step) so the create path sees a real prompt.
	tc.Prompt = "Review ./service for bugs."

	// Field application landed on the real TaskCreate fields.
	if tc.Model == nil || *tc.Model != "anthropic/claude-opus-4.8" {
		t.Errorf("Model = %v, want the template's model", tc.Model)
	}
	if tc.FallbackModel == nil || *tc.FallbackModel != "anthropic/claude-sonnet-4-6" {
		t.Errorf("FallbackModel = %v", tc.FallbackModel)
	}
	if tc.MaxIterations == nil || *tc.MaxIterations != 25 {
		t.Errorf("MaxIterations = %v, want 25", tc.MaxIterations)
	}
	if tc.Recurrence != "0 9 * * 1-5" {
		t.Errorf("Recurrence = %q", tc.Recurrence)
	}
	if tc.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q", tc.Timezone)
	}
	if tc.Persona != "security-auditor" {
		t.Errorf("Persona = %q", tc.Persona)
	}
	if !reflect.DeepEqual(tc.Tags, []string{"review", "code"}) {
		t.Errorf("Tags = %v", tc.Tags)
	}

	// The applied recipe passes the real create-path validation (no DB needed):
	// prompt length, model normalization, timezone load, cron parse, tag
	// normalization all run here.
	h := &Handlers{}
	if err := h.validateTaskCreate(&tc); err != nil {
		t.Fatalf("template-derived TaskCreate failed create validation: %v", err)
	}
}

// strptr (task_stream_handler_test.go) and intptr (estimate_test.go) are defined
// elsewhere in this test package and reused here.
