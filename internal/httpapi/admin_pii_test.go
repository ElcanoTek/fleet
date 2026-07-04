package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/rampartinstall"
)

// Admin PII probe + one-click install endpoints. Load-bearing assertions:
// both are admin-gated, answer 501 when their seam isn't wired, the probe
// returns the injected result, and the install endpoint routes GET/POST/DELETE
// to the installer (POST conflict → 409).

func TestAdminPIIProbe(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	s.piiProbe = func(context.Context) PIIProbeResult {
		return PIIProbeResult{OK: true, Engine: "rampart", Mode: "redact", Detail: "email×1", LatencyMS: 12}
	}
	h := s.Routes()

	// Non-admin: 403.
	if w := do(t, h, http.MethodPost, "/admin/pii-redaction/test", nil, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member probe: %d want 403", w.Code)
	}
	// Admin: returns the injected result.
	w := do(t, h, http.MethodPost, "/admin/pii-redaction/test", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("probe: %d body %s", w.Code, w.Body.String())
	}
	var res PIIProbeResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK || res.Engine != "rampart" || res.Detail != "email×1" {
		t.Errorf("probe result = %+v", res)
	}
	// Wrong method → 405.
	if w := do(t, h, http.MethodGet, "/admin/pii-redaction/test", nil, "boss@x.com"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe: %d want 405", w.Code)
	}
}

func TestAdminPIIProbeUnwired501(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	if w := do(t, s.Routes(), http.MethodPost, "/admin/pii-redaction/test", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired probe: %d want 501", w.Code)
	}
}

// fakeInstaller implements piiRampartInstaller.
type fakeInstaller struct {
	status   rampartinstall.Status
	startErr error
	started  bool
	removed  bool
}

func (f *fakeInstaller) Start(string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	f.status.State = rampartinstall.StateRunning
	return nil
}
func (f *fakeInstaller) Status(context.Context) rampartinstall.Status { return f.status }
func (f *fakeInstaller) Uninstall(context.Context) error {
	f.removed = true
	f.status = rampartinstall.Status{State: rampartinstall.StateIdle}
	return nil
}

func TestAdminPIIInstall(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	inst := &fakeInstaller{status: rampartinstall.Status{State: rampartinstall.StateIdle}}
	s.piiInstaller = inst
	h := s.Routes()

	// Non-admin: 403.
	if w := do(t, h, http.MethodGet, "/admin/pii-redaction/install", nil, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member install status: %d want 403", w.Code)
	}
	// GET status.
	if w := do(t, h, http.MethodGet, "/admin/pii-redaction/install", nil, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("GET install: %d", w.Code)
	}
	// POST starts.
	w := do(t, h, http.MethodPost, "/admin/pii-redaction/install", nil, "boss@x.com")
	if w.Code != http.StatusOK || !inst.started {
		t.Fatalf("POST install: %d started=%v", w.Code, inst.started)
	}
	// DELETE uninstalls.
	w = do(t, h, http.MethodDelete, "/admin/pii-redaction/install", nil, "boss@x.com")
	if w.Code != http.StatusOK || !inst.removed {
		t.Fatalf("DELETE install: %d removed=%v", w.Code, inst.removed)
	}
}

func TestAdminPIIInstallConflictAndUnwired(t *testing.T) {
	// A Start that reports "already running" maps to 409.
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	s.piiInstaller = &fakeInstaller{startErr: fmt.Errorf("an install is already running")}
	if w := do(t, s.Routes(), http.MethodPost, "/admin/pii-redaction/install", nil, "boss@x.com"); w.Code != http.StatusConflict {
		t.Fatalf("busy install: %d want 409", w.Code)
	}

	// Unwired → 501.
	s2 := memberFixture(t, "boss@x.com")
	setRole(t, s2, "boss@x.com", "admin", "")
	if w := do(t, s2.Routes(), http.MethodGet, "/admin/pii-redaction/install", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired install: %d want 501", w.Code)
	}
}
