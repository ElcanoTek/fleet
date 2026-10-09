package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The live specs for these scenarios run only in the e2e-live job, minutes
// into a real stack boot; these pin the scripts themselves so a typo in a tool
// name or an argument fails here, in milliseconds.
func TestFileToolsScenarioRoundTripsThroughBothToolFamilies(t *testing.T) {
	s := fakellm.New()
	registerLiveScenarios(s)
	turns := []string{
		`[{"role":"user","content":"[[scenario:file-tools]]"}]`,
		`[{"role":"user","content":"[[scenario:file-tools]]"},{"role":"assistant","content":"1"}]`,
		`[{"role":"user","content":"[[scenario:file-tools]]"},{"role":"assistant","content":"1"},{"role":"assistant","content":"2"}]`,
	}
	wants := []string{`"name":"write_file"`, `"name":"bash"`, `"name":"view_file"`}
	bodies := make([]string, len(turns))
	for i, msgs := range turns {
		body := `{"model":"m","stream":true,"messages":` + msgs + `}`
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body)))
		bodies[i] = w.Body.String()
		if !strings.Contains(bodies[i], wants[i]) {
			t.Fatalf("turn %d: want %s, got %s", i, wants[i], bodies[i])
		}
	}
	// The bash step reads what write_file wrote and writes what view_file reads.
	for _, want := range []string{"e2e-files/from-write-file.txt", "e2e-files/from-bash.txt"} {
		if !strings.Contains(bodies[1], want) {
			t.Fatalf("bash step does not touch %s: %s", want, bodies[1])
		}
	}
}

func TestProviderErrorScenarioIsANonRetryable400(t *testing.T) {
	s := fakellm.New()
	registerLiveScenarios(s)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"[[scenario:provider-error]]"}]}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "FAKELLM_PROVIDER_REJECTED") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestStopTurnScenarioEndsWhenTheCallerCancels(t *testing.T) {
	s := fakellm.New()
	registerLiveScenarios(s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/chat/completions", strings.NewReader(
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"[[scenario:stop-turn]]"}]}`))
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the deferred Close below; the reader goroutine confuses bodyclose's escape analysis
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The role-priming chunk arrives, then the stream stalls.
	buf := make([]byte, 512)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("no priming chunk: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(resp.Body)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("stop-turn finished on its own; it must stall until cancelled")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop-turn kept streaming after the caller cancelled")
	}
	if strings.Contains(string(buf), "FAKELLM_STOP_TURN_SHOULD_NEVER_RENDER") {
		t.Fatal("stop-turn emitted its text before the stall")
	}
}
