package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/settings"
)

// Admin workspace-settings endpoints. Load-bearing assertions: admin-gated,
// 501 when the service isn't wired (tests/mock mode), the error taxonomy
// (unknown key → 404, invalid value → 400), and set/reset round-trips that
// return the row's new resolved state.

// fakeSettingsService implements workspaceSettingsService in-memory; the real
// service logic is covered in internal/settings.
type fakeSettingsService struct {
	values map[string]string // key → override
	dflt   map[string]string
}

func newFakeSettingsService() *fakeSettingsService {
	return &fakeSettingsService{
		values: map[string]string{},
		dflt:   map[string]string{"pii_redaction_mode": "off", "subagents_enabled": "false"},
	}
}

func (f *fakeSettingsService) resolved(key string) settings.Resolved {
	r := settings.Resolved{
		Spec:    settings.Spec{Key: key, Kind: settings.KindEnum, EnvVar: "FLEET_X"},
		Value:   f.dflt[key],
		Default: f.dflt[key],
		Source:  settings.SourceDefault,
	}
	if v, ok := f.values[key]; ok {
		r.Value = v
		r.Source = settings.SourceAdmin
		r.UpdatedBy = "boss@x.com"
	}
	return r
}

func (f *fakeSettingsService) Snapshot(context.Context) ([]settings.Resolved, error) {
	out := make([]settings.Resolved, 0, len(f.dflt))
	for k := range f.dflt {
		out = append(out, f.resolved(k))
	}
	return out, nil
}

func (f *fakeSettingsService) Set(_ context.Context, key, value, _ string) (settings.Resolved, error) {
	if _, ok := f.dflt[key]; !ok {
		return settings.Resolved{}, settings.ErrUnknownKey
	}
	if value == "junk" {
		return settings.Resolved{}, fmt.Errorf("%w: junk", settings.ErrInvalidValue)
	}
	f.values[key] = value
	return f.resolved(key), nil
}

func (f *fakeSettingsService) Reset(_ context.Context, key, _ string) (settings.Resolved, error) {
	if _, ok := f.dflt[key]; !ok {
		return settings.Resolved{}, settings.ErrUnknownKey
	}
	delete(f.values, key)
	return f.resolved(key), nil
}

func settingsFixture(t *testing.T) (http.Handler, *fakeSettingsService) {
	t.Helper()
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	svc := newFakeSettingsService()
	s.workspaceSettings = svc
	return s.Routes(), svc
}

func TestAdminSettingsGate(t *testing.T) {
	h, _ := settingsFixture(t)

	// Non-admin member: 403 on both the list and item surfaces.
	if w := do(t, h, http.MethodGet, "/admin/settings", nil, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member GET: status %d want 403", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/admin/settings/pii_redaction_mode",
		map[string]any{"value": "redact"}, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member PUT: status %d want 403", w.Code)
	}
}

func TestAdminSettingsUnwired501(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes() // no WithWorkspaceSettings
	if w := do(t, h, http.MethodGet, "/admin/settings", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired GET: status %d want 501", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/admin/settings/x", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired DELETE: status %d want 501", w.Code)
	}
}

func TestAdminSettingsRoundTrip(t *testing.T) {
	h, svc := settingsFixture(t)

	// List: every registered setting, default-sourced.
	w := do(t, h, http.MethodGet, "/admin/settings", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: status %d body %s", w.Code, w.Body.String())
	}
	var list struct {
		Settings []settings.Resolved `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Settings) != 2 {
		t.Fatalf("list has %d settings, want 2", len(list.Settings))
	}

	// Set: returns the new resolved state with admin provenance.
	w = do(t, h, http.MethodPut, "/admin/settings/pii_redaction_mode",
		map[string]any{"value": "redact"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: status %d body %s", w.Code, w.Body.String())
	}
	var r settings.Resolved
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode put: %v", err)
	}
	if r.Value != "redact" || r.Source != settings.SourceAdmin {
		t.Errorf("put resolved = %+v, want admin redact", r)
	}
	if svc.values["pii_redaction_mode"] != "redact" {
		t.Errorf("service saw %q", svc.values["pii_redaction_mode"])
	}

	// Reset: back to the default source.
	w = do(t, h, http.MethodDelete, "/admin/settings/pii_redaction_mode", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE: status %d body %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if r.Source != settings.SourceDefault || r.Value != "off" {
		t.Errorf("reset resolved = %+v, want default off", r)
	}
}

// TestAdminSettingsEndToEnd wires the REAL settings.Service over the concrete
// Postgres store (migration 035) — proving the full slice: handler → service →
// validation → workspace_settings row → apply hook, and reset back to default.
func TestAdminSettingsEndToEnd(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")

	applied := map[string]string{}
	defaults := map[string]string{}
	hooks := map[string]settings.ApplyFunc{}
	for _, spec := range settings.Registry() {
		key := spec.Key
		hooks[key] = func(v string, _ bool) error { applied[key] = v; return nil }
		switch spec.Kind {
		case settings.KindBool:
			defaults[key] = "false"
		case settings.KindInt:
			defaults[key] = "65536"
		case settings.KindEnum:
			defaults[key] = spec.Enum[0]
		case settings.KindURL:
			defaults[key] = ""
		}
	}
	svc, err := settings.NewService(s.concreteStore(t), defaults, hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.workspaceSettings = svc
	h := s.Routes()

	w := do(t, h, http.MethodPut, "/admin/settings/pii_redaction_mode",
		map[string]any{"value": "redact"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: status %d body %s", w.Code, w.Body.String())
	}
	if applied["pii_redaction_mode"] != "redact" {
		t.Fatalf("apply hook saw %q, want redact", applied["pii_redaction_mode"])
	}

	// The persisted override survives an independent snapshot read.
	var list struct {
		Settings []settings.Resolved `json:"settings"`
	}
	w = do(t, h, http.MethodGet, "/admin/settings", nil, "boss@x.com")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, r := range list.Settings {
		if r.Key == "pii_redaction_mode" {
			found = true
			if r.Value != "redact" || r.Source != settings.SourceAdmin || r.UpdatedBy != "boss@x.com" {
				t.Errorf("resolved = %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("pii_redaction_mode missing from snapshot")
	}

	// Reset reverts to the default and re-applies it.
	w = do(t, h, http.MethodDelete, "/admin/settings/pii_redaction_mode", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE: status %d body %s", w.Code, w.Body.String())
	}
	if applied["pii_redaction_mode"] != "off" {
		t.Errorf("reset applied %q, want off", applied["pii_redaction_mode"])
	}
}

func TestAdminSettingsErrorTaxonomy(t *testing.T) {
	h, _ := settingsFixture(t)

	if w := do(t, h, http.MethodPut, "/admin/settings/not_a_setting",
		map[string]any{"value": "true"}, "boss@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown key: status %d want 404", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/admin/settings/pii_redaction_mode",
		map[string]any{"value": "junk"}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid value: status %d want 400", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/admin/settings/pii_redaction_mode",
		map[string]any{"value": "redact"}, "boss@x.com"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST item: status %d want 405", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/admin/settings/", map[string]any{"value": "x"}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("empty key: status %d want 400", w.Code)
	}
}
