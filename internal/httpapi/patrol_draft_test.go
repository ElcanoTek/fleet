package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestPatrolDraft_ReturnsDraft drives the Launchpad's "Draft it with AI"
// endpoint: the wizard's answers reach the synthesizer as given, and the
// draft comes back as JSON for the review step. Nothing is persisted.
func TestPatrolDraft_ReturnsDraft(t *testing.T) {
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{
		Title:   "Morning deal sweep",
		Prompt:  "**Mission** …",
		Summary: "Every weekday morning I check your deals.",
	}}
	s := &Server{agent: fake}
	body := `{"goals":["Flag misconfigured deals","  ","Scan my inbox"],"notes":"focus on CTV",
		"connectors":[{"name":"gmail","description":"Read mail"}],"memories":["Never email clients"],
		"cadence":"Weekdays at 08:00","mode":"act","persona":"analyst"}`
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct{ Title, Prompt, Summary string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Title != "Morning deal sweep" || got.Prompt == "" || got.Summary == "" {
		t.Errorf("draft = %+v", got)
	}
	in := fake.patrolInput
	if len(in.Goals) != 2 || in.Goals[1] != "Scan my inbox" {
		t.Errorf("blank goals should be dropped, got %q", in.Goals)
	}
	if len(in.Connectors) != 1 || in.Connectors[0].Name != "gmail" || in.Connectors[0].Description != "Read mail" {
		t.Errorf("connectors = %+v", in.Connectors)
	}
	if in.Mode != "act" || in.Cadence != "Weekdays at 08:00" || in.Persona != "analyst" || in.Notes != "focus on CTV" {
		t.Errorf("input = %+v", in)
	}
	if len(in.Memories) != 1 {
		t.Errorf("memories = %q", in.Memories)
	}
}

// TestPatrolDraft_Rejections pins the refusals: nothing to watch is a 400
// before any model call, a failed synthesis is a 502 the wizard reports, and
// the endpoint is POST-only.
func TestPatrolDraft_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
		fake   *fakeTurnEngine
		want   int
	}{
		{"nothing to watch", http.MethodPost, `{"goals":["  "],"notes":""}`, &fakeTurnEngine{}, http.StatusBadRequest},
		{"bad json", http.MethodPost, `{"goals":`, &fakeTurnEngine{}, http.StatusBadRequest},
		{"synthesis failed", http.MethodPost, `{"goals":["Scan my inbox"]}`, &fakeTurnEngine{patrolDraftErr: errors.New("boom")}, http.StatusBadGateway},
		{"no draft", http.MethodPost, `{"notes":"watch spend"}`, &fakeTurnEngine{}, http.StatusBadGateway},
		{"GET", http.MethodGet, ``, &fakeTurnEngine{}, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{agent: tc.fake}
			rec := httptest.NewRecorder()
			s.handlePatrolDraft(rec, httptest.NewRequest(tc.method, "/patrol-draft", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// A refused request never reached the model.
	fake := &fakeTurnEngine{}
	s := &Server{agent: fake}
	s.handlePatrolDraft(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{}`)))
	if fake.patrolInput.Goals != nil || fake.patrolInput.Notes != "" {
		t.Error("an empty request should not reach the synthesizer")
	}
}

// TestPatrolDraft_BodyCap refuses an oversized request rather than reading it.
func TestPatrolDraft_BodyCap(t *testing.T) {
	s := &Server{agent: &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p"}}}
	big := `{"goals":["` + strings.Repeat("x", patrolDraftMaxBody) + `"]}`
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(big)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
