// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMACSHA256(t *testing.T) {
	secret := "this-is-a-32-byte-minimum-secret!"
	body := []byte(`{"action":"opened"}`)
	good := sign(secret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   bool
	}{
		{"valid with sha256= prefix", secret, body, good, true},
		{"valid without prefix", secret, body, strings.TrimPrefix(good, "sha256="), true},
		{"valid uppercase hex digest", secret, body, "sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256=")), true},
		{"wrong secret", "other-secret-also-32-bytes-long!!", body, good, false},
		{"tampered body", secret, []byte(`{"action":"closed"}`), good, false},
		{"empty secret fails closed", "", body, good, false},
		{"empty header", secret, body, "", false},
		{"malformed (short) sig", secret, body, "sha256=deadbeef", false},
		{"non-hex sig of right length", secret, body, "sha256=" + strings.Repeat("z", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyHMACSHA256(tc.body, tc.secret, tc.header); got != tc.want {
				t.Errorf("verifyHMACSHA256 = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderTriggerTemplate(t *testing.T) {
	body := []byte(`{"action":"opened","pr":{"number":7}}`)
	req := httptest.NewRequest(http.MethodPost, "/triggers/x", nil)
	req.Header.Set("User-Agent", "GitHub-Hookshot/abc")
	req.Header.Set("Content-Type", "application/json")

	cases := []struct {
		name      string
		tmpl      string
		body      []byte
		contains  string
		wantErr   bool
		wantEmpty bool
	}{
		{name: "empty template yields empty", tmpl: "", body: body, wantEmpty: true},
		{name: "raw payload", tmpl: "Payload: {{.Payload}}", body: body, contains: `"action":"opened"`},
		{name: "dot-path index access", tmpl: `Action is {{index .Body "action"}}`, body: body, contains: "Action is opened"},
		{name: "header access", tmpl: "UA={{.Headers.UserAgent}}", body: body, contains: "UA=GitHub-Hookshot/abc"},
		{name: "missing key is zero, not error", tmpl: `{{index .Body "nope"}}`, body: body, contains: ""},
		{name: "non-json body still exposes payload", tmpl: "raw={{.Payload}}", body: []byte("not json"), contains: "raw=not json"},
		{name: "bad template errors", tmpl: "{{.Unclosed", body: body, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderTriggerTemplate(tc.tmpl, tc.body, req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%q)", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantEmpty && out != "" {
				t.Errorf("expected empty output, got %q", out)
			}
			if tc.contains != "" && !strings.Contains(out, tc.contains) {
				t.Errorf("output %q does not contain %q", out, tc.contains)
			}
		})
	}
}

// TestHandleWebhookTrigger_Integration seeds a webhook template task + trigger,
// fires a signed webhook, and asserts a fresh pending run is spawned with the
// rendered prompt. It also covers the indistinguishable-401 paths.
func TestHandleWebhookTrigger_Integration(t *testing.T) {
	_, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	h := New(Config{Version: "0.1.0"}, store, nil)
	r := chi.NewRouter()
	r.Post("/triggers/{slug}", h.HandleWebhookTrigger)

	// Seed an inert webhook template task.
	template := models.NewTask(models.TaskCreate{
		Prompt:      "base prompt",
		TriggerType: models.TriggerTypeWebhook,
	})
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("seed template task: %v", err)
	}
	secret := "webhook-secret-at-least-32-bytes-long!"
	trig := &models.TaskTrigger{
		ID:             uuid.New(),
		TaskID:         template.ID,
		Slug:           "gh-hook",
		Secret:         secret,
		PromptTemplate: `New event: {{index .Body "action"}}`,
	}
	if err := store.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	body := []byte(`{"action":"deploy"}`)

	// 1) Valid signed request → 202 + run_id, and a new pending run exists.
	req := httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid request: got status %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	runID, err := uuid.Parse(resp["run_id"])
	if err != nil {
		t.Fatalf("run_id not a uuid: %q", resp["run_id"])
	}
	run, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("load spawned run: %v", err)
	}
	if run.Status != models.TaskStatusPending {
		t.Errorf("spawned run status = %q, want pending", run.Status)
	}
	if run.TriggerType != models.TriggerTypeCron {
		t.Errorf("spawned run trigger_type = %q, want cron (must be claimable)", run.TriggerType)
	}
	if run.Prompt != "New event: deploy" {
		t.Errorf("spawned run prompt = %q, want rendered template", run.Prompt)
	}

	// 2) Bad signature → 401.
	req = httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("wrong-secret-still-32-bytes-long!!!", body))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: got status %d, want 401", rec.Code)
	}

	// 3) Unknown slug → 401 (indistinguishable from bad signature).
	req = httptest.NewRequest(http.MethodPost, "/triggers/does-not-exist", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown slug: got status %d, want 401", rec.Code)
	}
}
