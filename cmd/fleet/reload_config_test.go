package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
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
	logConfigReload(config.ReloadResult{}, errors.New("boom"))
	logConfigReload(config.ReloadResult{
		Changed: []config.FieldChange{{Key: "FLEET_MAX_COST_USD", Old: "50", New: "25"}},
		Skipped: []config.SkippedField{{Key: "FLEET_SERVER_ADDR", Reason: "restart required"}},
	}, nil)
}

// TestLogConfigReloadLevels: a clean SIGUSR2 reload logs its summary at Info
// (it is not an error), while each per-setting error is logged at Warn so it
// still shows at FLEET_LOG_LEVEL=warn.
func TestLogConfigReloadLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logConfigReload(config.ReloadResult{Changed: []config.FieldChange{{Key: "FLEET_X", Old: "1", New: "2"}}}, nil)
	if out := buf.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Fatalf("clean reload logged above Info:\n%s", out)
	}

	buf.Reset()
	logConfigReload(config.ReloadResult{Errors: []config.ReloadError{{Key: "FLEET_Y", Error: "bad value"}}}, nil)
	out := buf.String()
	if !strings.Contains(out, `level=WARN msg="config reload: setting not applied" key=FLEET_Y`) {
		t.Fatalf("per-setting reload error not logged at Warn:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("reload with a per-setting error logged at Error:\n%s", out)
	}
}
