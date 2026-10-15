package fakellm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// The fake answers the Decisions API through fleet's real classifier client,
// so the live E2E stack's default-on frustration check gets an answer instead
// of a 404: pushback only when the latest user message carries the marker.
func TestDecisionsSeam(t *testing.T) {
	srv := httptest.NewServer(fakellm.New().Handler())
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL+"/api/v1")

	for _, c := range []struct {
		name, conversation string
		pushback           bool
	}{
		{"normal", "User: revenue by month\n\nAssistant: Total is $1M.\n\nUser: thanks", false},
		{"pushback", "User: revenue by month\n\nAssistant: Total is $1M.\n\nUser: no, by month " + fakellm.PushbackMarker, true},
		{"marker only earlier", "User: " + fakellm.PushbackMarker + "\n\nAssistant: Sorry.\n\nUser: great, thanks", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := agentcore.CheckStruggle(context.Background(), "sk-or-fake", "~typesafe/jev-latest", c.conversation)
			if err != nil {
				t.Fatalf("CheckStruggle: %v", err)
			}
			if got := v.Probability >= 0.8; got != c.pushback {
				t.Errorf("probability %v: pushback = %v, want %v", v.Probability, got, c.pushback)
			}
			if v.Model != "~typesafe/jev-latest" || v.Provider != "fake-llm" {
				t.Errorf("verdict = %+v, want the requested model echoed", v)
			}
		})
	}
	if _, err := agentcore.CheckStruggle(context.Background(), "", "m/x", "User: hi"); err == nil {
		t.Error("a missing key must fail before any request")
	}
}

// The filtered listing fleet's admin picker asks for carries one paid
// decision model; the default listing, which feeds the chat picker, does not.
func TestDecisionModelListing(t *testing.T) {
	srv := httptest.NewServer(fakellm.New().Handler())
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL+"/api/v1")

	models, err := agentcore.ListDecisionModels(context.Background())
	if err != nil {
		t.Fatalf("ListDecisionModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != fakellm.DecisionModelID || models[0].InputPerMillion <= 0 {
		t.Fatalf("decision models = %+v, want the fake's one paid decision model", models)
	}

	resp, err := http.Get(srv.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	for _, m := range listing.Data {
		if m.ID == fakellm.DecisionModelID {
			t.Fatal("the default listing must not carry the decision model")
		}
	}
}
