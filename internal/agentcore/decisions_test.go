package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The struggle check posts one yes/no question to the Decisions API with the
// conversation tail as state, and returns the model's probability.
func TestCheckStruggle_ParsesProbability(t *testing.T) {
	var got decisionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/alpha/decisions" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"typesafe/jev-1.14-20261014","answers":{"struggle":{"type":"noul","noul":0.97}},"usage":{"cost":0.000018},"provider":"TypeSafe"}`))
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	v, err := CheckStruggle(context.Background(), "test-key", "~typesafe/jev-latest", "User: no, that's wrong again")
	if err != nil {
		t.Fatalf("CheckStruggle: %v", err)
	}
	if v.Probability != 0.97 || v.Model != "typesafe/jev-1.14-20261014" || v.Provider != "TypeSafe" || v.CostUSD != 0.000018 {
		t.Errorf("verdict = %+v", v)
	}
	if got.Model != "~typesafe/jev-latest" || got.State["conversation"] != "User: no, that's wrong again" {
		t.Errorf("request = %+v", got)
	}
	q, ok := got.Questions[struggleQuestionID]
	if !ok || q.Type != "noul" || q.Criteria["true"] == "" || q.Criteria["false"] == "" {
		t.Errorf("question = %+v", q)
	}
}

// Only the conversation's tail is sent: the question is about how it ends.
func TestCheckStruggle_SendsTail(t *testing.T) {
	var got decisionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"answers":{"struggle":{"type":"noul","noul":0.1}}}`))
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)
	long := strings.Repeat("x", MaxStruggleTranscriptRunes) + "User: the end"
	if _, err := CheckStruggle(context.Background(), "k", "m/x", long); err != nil {
		t.Fatalf("CheckStruggle: %v", err)
	}
	if n := len([]rune(got.State["conversation"])); n != MaxStruggleTranscriptRunes || !strings.HasSuffix(got.State["conversation"], "User: the end") {
		t.Errorf("sent %d runes, want the last %d", n, MaxStruggleTranscriptRunes)
	}
}

// Every failure is an error the caller turns into "no card": no key, a
// non-200, a missing or out-of-range probability.
func TestCheckStruggle_Failures(t *testing.T) {
	if _, err := CheckStruggle(context.Background(), "", "m/x", "User: hi"); !errors.Is(err, ErrDecisionUnavailable) {
		t.Errorf("no key: err = %v", err)
	}
	for name, resp := range map[string]struct {
		status int
		body   string
	}{
		"http error":     {http.StatusBadGateway, `{"error":"echo of the prompt"}`},
		"no answer":      {http.StatusOK, `{"answers":{}}`},
		"no probability": {http.StatusOK, `{"answers":{"struggle":{"type":"noul"}}}`},
		"out of range":   {http.StatusOK, `{"answers":{"struggle":{"type":"noul","noul":1.7}}}`},
		"not json":       {http.StatusOK, `oops`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(resp.status)
				_, _ = w.Write([]byte(resp.body))
			}))
			defer srv.Close()
			t.Setenv("OPENROUTER_BASE_URL", srv.URL)
			_, err := CheckStruggle(context.Background(), "k", "m/x", "User: hi")
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "echo of the prompt") {
				t.Errorf("error leaks the response body: %v", err)
			}
		})
	}
}

// The admin picker lists paid decision models only, cheapest first.
func TestParseDecisionModels(t *testing.T) {
	body := []byte(`{"data":[
	 {"id":"typesafe/jev-1.14","name":"TypeSafe: Jev","context_length":262144,"architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.000000042"}},
	 {"id":"liquid/d1","name":"Liquid: D1","architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.00000002"}},
	 {"id":"x/free:free","architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0"}},
	 {"id":"anthropic/claude-haiku-5.5","architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.0000001"}}
	]}`)
	got, err := parseDecisionModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0].ID != "liquid/d1" || got[1].ID != "typesafe/jev-1.14" {
		t.Fatalf("models = %+v", got)
	}
	if got[1].InputPerMillion < 0.0419 || got[1].InputPerMillion > 0.0421 || got[1].ContextLength != 262144 {
		t.Errorf("jev = %+v", got[1])
	}
}
