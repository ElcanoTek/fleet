package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

func TestScheduledScenariosProvideCompletionVerdict(t *testing.T) {
	s := fakellm.New()
	registerLiveScenarios(s)
	for _, name := range []string{"sched-task", "tck-complete", "a2a-delegate", "tck-artifact-text", "tck-artifact-file"} {
		t.Run(name, func(t *testing.T) {
			// The verifier makes a fresh non-streaming request whose original-task
			// text still carries the worker's scenario marker. It needs a verdict,
			// not a restart of the scripted worker's tool loop.
			body := `{"model":"openai/gpt-5.6-sol","stream":false,"messages":[{"role":"system","content":"You are a strict end-of-run verifier for an automated agent."},{"role":"user","content":"ORIGINAL TASK: [[scenario:` + name + `]]"}]}`
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body)))
			var response struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 {
				t.Fatalf("expected one verdict, got %s", w.Body.String())
			}
			var verdict struct {
				Missing []string `json:"missing_actions"`
			}
			if err := json.Unmarshal([]byte(response.Choices[0].Message.Content), &verdict); err != nil {
				t.Fatalf("fixture returned no valid verifier verdict: %s", w.Body.String())
			}
			if verdict.Missing == nil || len(verdict.Missing) != 0 {
				t.Fatalf("expected explicit complete verdict, got %#v", verdict)
			}
		})
	}
}

func TestScheduledScenarioStillRunsSandboxTool(t *testing.T) {
	s := fakellm.New()
	registerLiveScenarios(s)
	body := `{"model":"openai/gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"[[scenario:sched-task]]"}]}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body)))
	if !strings.Contains(w.Body.String(), `"name":"run_python"`) || strings.Contains(w.Body.String(), "missing_actions") {
		t.Fatalf("worker must execute its sandbox tool, got %s", w.Body.String())
	}
}
