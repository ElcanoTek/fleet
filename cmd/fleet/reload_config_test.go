package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestReloadConfigHandler_RefreshesEnvDefaultsWithoutOverridingAdminSettings(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "fleet.env")
	t.Setenv("FLEET_ENV_FILE", envPath)
	// Ensure process env does not shadow the file keys under test.
	for _, k := range []string{
		"FLEET_SUBAGENTS_ENABLED",
		"FLEET_APPROVAL_TIMEOUT_SECONDS",
		"FLEET_MAX_COST_USD",
		"FLEET_DEFAULT_MODEL",
	} {
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetFallbackModel("")
		agentcore.SetTaskModel("")
	})

	initialEnv := "FLEET_SUBAGENTS_ENABLED=true\nFLEET_APPROVAL_TIMEOUT_SECONDS=86400\nFLEET_MAX_COST_USD=50\n"
	if err := os.WriteFile(envPath, []byte(initialEnv), 0o600); err != nil {
		t.Fatalf("write initial env: %v", err)
	}
	cfg, err := config.Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st := &fakeWorkspaceSettingsStore{rows: map[string]store.WorkspaceSetting{
		"default_model": {Key: "default_model", Value: "acme/cli-override", UpdatedBy: "cli"},
	}}
	svc, _, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		t.Fatalf("buildWorkspaceSettings: %v", err)
	}
	if err := svc.ApplyAll(context.Background()); err != nil {
		t.Fatalf("initial ApplyAll: %v", err)
	}

	updatedEnv := "FLEET_SUBAGENTS_ENABLED=false\nFLEET_APPROVAL_TIMEOUT_SECONDS=120\nFLEET_MAX_COST_USD=25\nFLEET_DEFAULT_MODEL=acme/env-default\n"
	if err := os.WriteFile(envPath, []byte(updatedEnv), 0o600); err != nil {
		t.Fatalf("write updated env: %v", err)
	}

	rec := httptest.NewRecorder()
	reloadConfigHandler(cfg, svc)(rec, httptest.NewRequest(http.MethodPost, "/admin/reload-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	if cfg.LiveSubagentsEnabled() {
		t.Error("LiveSubagentsEnabled = true after reloading FLEET_SUBAGENTS_ENABLED=false, want false")
	}
	if got := cfg.LiveApprovalTimeoutSeconds(); got != 120 {
		t.Errorf("LiveApprovalTimeoutSeconds = %d, want 120", got)
	}
	if got := cfg.LiveMaxCostUSD(); got != 25 {
		t.Errorf("LiveMaxCostUSD = %v, want 25", got)
	}
	if got := agentcore.CurrentDefaultModel(); got != "acme/cli-override" {
		t.Errorf("persisted override lost: CurrentDefaultModel = %q, want acme/cli-override", got)
	}

	// Clearing the persisted override and running SIGUSR2 reloadRuntimeConfig
	// reverts to the newly reloaded env default (not the boot-time constant).
	_ = st.DeleteWorkspaceSetting(context.Background(), "default_model")
	reloadRuntimeConfig(context.Background(), cfg, svc)
	if got := agentcore.CurrentDefaultModel(); got != "acme/env-default" {
		t.Errorf("CurrentDefaultModel after clearing override = %q, want acme/env-default", got)
	}

	// If cfg.Reload fails (e.g. a malformed line exceeding the scanner buffer
	// after an earlier line), reloadRuntimeConfig must stop immediately without
	// refreshing/applying partially scanned env defaults.
	badEnv := "FLEET_DEFAULT_MODEL=acme/partial-scan\nBROKEN=" + strings.Repeat("x", 200*1024) + "\n"
	if err := os.WriteFile(envPath, []byte(badEnv), 0o600); err != nil {
		t.Fatalf("write bad env: %v", err)
	}
	reloadRuntimeConfig(context.Background(), cfg, svc)
	if got := agentcore.CurrentDefaultModel(); got != "acme/env-default" {
		t.Errorf("CurrentDefaultModel changed after failed reload = %q, want acme/env-default", got)
	}
}

func TestReloadConfigHandler_InvalidGuardrailEnumPreservesActiveGuardrail(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "fleet.env")
	t.Setenv("FLEET_ENV_FILE", envPath)
	for _, k := range []string{"FLEET_GUARDRAIL_MODE", "FLEET_GUARDRAIL_URL"} {
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		agentcore.SetGuardrail(false, false, "off", "prompt-injection", nil)
	})

	initialEnv := "FLEET_GUARDRAIL_URL=http://127.0.0.1:19999\nFLEET_GUARDRAIL_MODE=block\n"
	if err := os.WriteFile(envPath, []byte(initialEnv), 0o600); err != nil {
		t.Fatalf("write initial env: %v", err)
	}
	cfg, err := config.Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st := &fakeWorkspaceSettingsStore{rows: map[string]store.WorkspaceSetting{}}
	svc, guard, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		t.Fatalf("buildWorkspaceSettings: %v", err)
	}
	if err := svc.ApplyAll(context.Background()); err != nil {
		t.Fatalf("initial ApplyAll: %v", err)
	}

	// Reloading a typo in FLEET_GUARDRAIL_MODE must retain "block", never fail open to "off".
	typoEnv := "FLEET_GUARDRAIL_URL=http://127.0.0.1:19999\nFLEET_GUARDRAIL_MODE=blok\n"
	if err := os.WriteFile(envPath, []byte(typoEnv), 0o600); err != nil {
		t.Fatalf("write typo env: %v", err)
	}
	rec := httptest.NewRecorder()
	reloadConfigHandler(cfg, svc)(rec, httptest.NewRequest(http.MethodPost, "/admin/reload-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	probe := guard.Probe(context.Background())
	if probe.Mode != "block" {
		t.Fatalf("guardrail mode after typo reload = %q, want block", probe.Mode)
	}
}
