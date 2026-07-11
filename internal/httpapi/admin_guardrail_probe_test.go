package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAdminGuardrailProbe(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	s.guardrailProbe = func(context.Context) GuardrailProbeResult {
		return GuardrailProbeResult{OK: true, Mode: "observe", Profile: "prompt-injection", Flagged: true, Score: .98}
	}
	h := s.Routes()
	if w := do(t, h, http.MethodPost, "/admin/guardrail/test", nil, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member probe: %d want 403", w.Code)
	}
	w := do(t, h, http.MethodPost, "/admin/guardrail/test", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("probe: %d body %s", w.Code, w.Body.String())
	}
	var got GuardrailProbeResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.OK || !got.Flagged {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if w := do(t, h, http.MethodGet, "/admin/guardrail/test", nil, "boss@x.com"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe: %d want 405", w.Code)
	}
}

func TestAdminGuardrailProbeUnwired(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	if w := do(t, s.Routes(), http.MethodPost, "/admin/guardrail/test", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired probe: %d want 501", w.Code)
	}
}
