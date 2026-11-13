package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestReloadConfigHandler_NilCfg: a route-walking/test build passes a nil cfg, so
// a real request must fail closed with 503 rather than panic.
func TestReloadConfigHandler_NilCfg(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	reloadConfigHandler(nil)(rec, httptest.NewRequest(http.MethodPost, "/admin/reload-config", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil cfg: want 503, got %d", rec.Code)
	}
}

// TestReloadConfigHandler_OK: a Load'd config reloads and the handler returns a
// 200 with a well-formed ReloadResult JSON body (arrays present, not null).
func TestReloadConfigHandler_OK(t *testing.T) {
	t.Setenv("FLEET_ENV_FILE", "") // reload reads the file named here; empty = none
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := httptest.NewRecorder()
	reloadConfigHandler(cfg)(rec, httptest.NewRequest(http.MethodPost, "/admin/reload-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var res config.ReloadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode ReloadResult: %v (body=%s)", err, rec.Body.String())
	}
	if res.ReloadedAt.IsZero() {
		t.Error("reloaded_at is zero")
	}
	// The arrays must serialize as [] (not null) so consumers can iterate safely.
	if res.Changed == nil || res.Skipped == nil || res.Errors == nil {
		t.Errorf("result arrays must be non-nil: %+v", res)
	}
}

// TestLogConfigReload exercises both branches of the SIGUSR2 reload logger (it
// only writes to the log, so we assert it does not panic on either path).
func TestLogConfigReload(_ *testing.T) {
	ctx := context.Background()
	logConfigReload(ctx, config.ReloadResult{}, errors.New("boom"))
	logConfigReload(ctx, config.ReloadResult{
		Changed: []config.FieldChange{{Key: "FLEET_MAX_COST_USD", Old: "50", New: "25"}},
		Skipped: []config.SkippedField{{Key: "FLEET_SERVER_ADDR", Reason: "restart required"}},
	}, nil)
}

// TestLogConfigReloadLevels: a clean SIGUSR2 reload logs its summary at Info
// (it is not an error), while each per-setting error is logged at Warn so it
// still shows at FLEET_LOG_LEVEL=warn.
func TestLogConfigReloadLevels(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logConfigReload(ctx, config.ReloadResult{Changed: []config.FieldChange{{Key: "FLEET_X", Old: "1", New: "2"}}}, nil)
	if out := buf.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Fatalf("clean reload logged above Info:\n%s", out)
	}

	buf.Reset()
	logConfigReload(ctx, config.ReloadResult{Errors: []config.ReloadError{{Key: "FLEET_Y", Error: "bad value"}}}, nil)
	out := buf.String()
	if !strings.Contains(out, `level=WARN msg="config reload: setting not applied" key=FLEET_Y`) {
		t.Fatalf("per-setting reload error not logged at Warn:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("reload with a per-setting error logged at Error:\n%s", out)
	}
}

type fakeWorkspaceSettingsStore struct {
	rows map[string]store.WorkspaceSetting
}

func (f *fakeWorkspaceSettingsStore) WorkspaceSettings(_ context.Context) (map[string]store.WorkspaceSetting, error) {
	out := make(map[string]store.WorkspaceSetting, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeWorkspaceSettingsStore) SetWorkspaceSetting(_ context.Context, key, value, updatedBy string) (store.WorkspaceSetting, error) {
	ws := store.WorkspaceSetting{Key: key, Value: value, UpdatedBy: updatedBy}
	f.rows[key] = ws
	return ws, nil
}

func (f *fakeWorkspaceSettingsStore) DeleteWorkspaceSetting(_ context.Context, key string) error {
	delete(f.rows, key)
	return nil
}

func TestReloadConfigHandler_ReappliesWorkspaceSettings(t *testing.T) {
	t.Setenv("FLEET_ENV_FILE", "")
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetFallbackModel("")
		agentcore.SetTaskModel("")
	})

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st := &fakeWorkspaceSettingsStore{rows: map[string]store.WorkspaceSetting{
		"default_model":  {Key: "default_model", Value: "acme/live-cli-default", UpdatedBy: "cli"},
		"advanced_model": {Key: "advanced_model", Value: "acme/live-cli-fallback", UpdatedBy: "cli"},
	}}
	svc, _, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		t.Fatalf("buildWorkspaceSettings: %v", err)
	}

	rec := httptest.NewRecorder()
	reloadConfigHandler(cfg, svc)(rec, httptest.NewRequest(http.MethodPost, "/admin/reload-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := agentcore.CurrentDefaultModel(); got != "acme/live-cli-default" {
		t.Errorf("CurrentDefaultModel = %q, want acme/live-cli-default", got)
	}
	if got := agentcore.CurrentFallbackModel(); got != "acme/live-cli-fallback" {
		t.Errorf("CurrentFallbackModel = %q, want acme/live-cli-fallback", got)
	}
}
