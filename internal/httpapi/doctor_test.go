// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/boxdoctor"
	"github.com/ElcanoTek/fleet/internal/config"
)

// TestHandleDoctor — the admin doctor endpoint must return a boxdoctor report
// whose chat-DB check reflects the server's own store, and must reject
// non-GET methods. Auth/admin gating is applied at route registration (same
// wrapper as /admin/health-summary) and covered by the middleware tests.
func TestHandleDoctor(t *testing.T) {
	st := &healthStubStore{}
	s := New(&config.Config{}, &fakeEngine{}, st)

	rr := httptest.NewRecorder()
	s.handleDoctor(rr, httptest.NewRequest(http.MethodGet, "/admin/doctor", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}

	var got boxdoctor.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Checks) == 0 {
		t.Fatal("no checks in the report")
	}
	if got.Deep {
		t.Error("deep must default to false (page-load runs must never launch a container)")
	}
	if st.pings == 0 {
		t.Error("chat-DB check did not ping through the server store")
	}
	var chat *boxdoctor.Check
	for i := range got.Checks {
		if got.Checks[i].Name == "chat database" {
			chat = &got.Checks[i]
		}
	}
	if chat == nil || chat.Status != boxdoctor.StatusOK {
		t.Errorf("chat database check = %+v, want ok via the stub store", chat)
	}

	// Method gate.
	rr = httptest.NewRecorder()
	s.handleDoctor(rr, httptest.NewRequest(http.MethodPost, "/admin/doctor", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code = %d, want 405", rr.Code)
	}
}

// TestHandleDoctorNoStore — a server without a store must still answer (the
// chat-DB check reports skip, not a panic or a 500).
func TestHandleDoctorNoStore(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.handleDoctor(rr, httptest.NewRequest(http.MethodGet, "/admin/doctor?deep=0", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var got boxdoctor.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range got.Checks {
		if c.Name == "chat database" && c.Status != boxdoctor.StatusSkip {
			t.Errorf("chat database without a store = %s, want skip", c.Status)
		}
	}
}
