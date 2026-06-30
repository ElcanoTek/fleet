package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeEnv(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
}

// TestReload_AppliesChangedReloadable: a reloadable value edited in the env file
// is picked up, reported in Changed, and visible through the Live* getter.
func TestReload_AppliesChangedReloadable(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\nFLEET_MAX_ITERATIONS=100\nFLEET_TEMPERATURE=0.2\n")

	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LiveMaxCostUSD(); got != 10 {
		t.Fatalf("initial LiveMaxCostUSD = %v, want 10", got)
	}

	writeEnv(t, envPath, "FLEET_MAX_COST_USD=22.5\nFLEET_MAX_ITERATIONS=250\nFLEET_TEMPERATURE=0.2\n")
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if cfg.LiveMaxCostUSD() != 22.5 {
		t.Errorf("LiveMaxCostUSD = %v, want 22.5", cfg.LiveMaxCostUSD())
	}
	if cfg.LiveMaxIterations() != 250 {
		t.Errorf("LiveMaxIterations = %v, want 250", cfg.LiveMaxIterations())
	}
	// Temperature did NOT change, so it must not appear in Changed.
	if cfg.LiveTemperature() != 0.2 {
		t.Errorf("LiveTemperature = %v, want 0.2 (unchanged)", cfg.LiveTemperature())
	}
	if len(res.Errors) != 0 {
		t.Errorf("unexpected errors: %+v", res.Errors)
	}
	changed := map[string]string{}
	for _, c := range res.Changed {
		changed[c.Key] = c.New
	}
	if changed["FLEET_MAX_COST_USD"] != "22.5" {
		t.Errorf("Changed missing FLEET_MAX_COST_USD->22.5; got %+v", res.Changed)
	}
	if changed["FLEET_MAX_ITERATIONS"] != "250" {
		t.Errorf("Changed missing FLEET_MAX_ITERATIONS->250; got %+v", res.Changed)
	}
	if _, ok := changed["FLEET_TEMPERATURE"]; ok {
		t.Errorf("FLEET_TEMPERATURE reported changed but its value was untouched: %+v", res.Changed)
	}
}

// TestReload_UnparseableKeepsCurrent: a non-numeric value records an Error and
// leaves the running value intact.
func TestReload_UnparseableKeepsCurrent(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\n")
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	writeEnv(t, envPath, "FLEET_MAX_COST_USD=not-a-number\n")
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.LiveMaxCostUSD() != 10 {
		t.Errorf("value changed despite parse error: %v, want 10", cfg.LiveMaxCostUSD())
	}
	if len(res.Changed) != 0 {
		t.Errorf("unexpected Changed on parse error: %+v", res.Changed)
	}
	if len(res.Errors) != 1 || res.Errors[0].Key != "FLEET_MAX_COST_USD" {
		t.Errorf("want one error for FLEET_MAX_COST_USD; got %+v", res.Errors)
	}
}

// TestReload_OutOfRangeKeepsCurrent: a value outside the allowed bound records an
// Error and keeps the current value (a bad value never poisons a running process).
func TestReload_OutOfRangeKeepsCurrent(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_MAX_ITERATIONS=100\n")
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// MAX_ITERATIONS must stay within [1, 10000].
	writeEnv(t, envPath, "FLEET_MAX_ITERATIONS=0\n")
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.LiveMaxIterations() != 100 {
		t.Errorf("out-of-range value applied: %v, want 100", cfg.LiveMaxIterations())
	}
	if len(res.Errors) != 1 || res.Errors[0].Key != "FLEET_MAX_ITERATIONS" {
		t.Errorf("want one range error; got %+v", res.Errors)
	}
}

// TestReload_NonReloadableReportedSkipped: changing a non-reloadable setting is
// surfaced in Skipped (so the operator learns a restart is required) and never
// silently applied.
func TestReload_NonReloadableReportedSkipped(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_SERVER_ADDR=127.0.0.1:8080\n")
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	writeEnv(t, envPath, "FLEET_SERVER_ADDR=0.0.0.0:9999\n")
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The bound address on the live Config is unchanged (it is not reloadable).
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr changed on reload: %q, want 127.0.0.1:8080", cfg.Addr)
	}
	found := false
	for _, s := range res.Skipped {
		if s.Key == "FLEET_SERVER_ADDR" {
			found = true
			if s.Reason == "" {
				t.Error("skipped FLEET_SERVER_ADDR has no reason")
			}
		}
	}
	if !found {
		t.Errorf("FLEET_SERVER_ADDR change not reported in Skipped: %+v", res.Skipped)
	}
}

// TestReload_DiscreteDBChangeReportedSkipped: changing the DSN via the discrete
// DB_* vars (not an explicit DATABASE_URL) must still be reported in Skipped —
// the watch resolves the effective DSN via buildDatabaseURL, so the common
// DB_HOST/DB_PORT/... deployment path is covered, not just DATABASE_URL.
func TestReload_DiscreteDBChangeReportedSkipped(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "DB_HOST=db-a.internal\nDB_NAME=fleet\n")
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	writeEnv(t, envPath, "DB_HOST=db-b.internal\nDB_NAME=fleet\n")
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s.Key == "DATABASE_URL / DB_*" {
			found = true
		}
	}
	if !found {
		t.Errorf("discrete DB_HOST change not reported in Skipped: %+v", res.Skipped)
	}
}

// TestReload_BootProcessEnvWins: a value pinned in the PROCESS environment at boot
// keeps boot precedence on reload — an env-file edit cannot override it (exactly
// as at startup). This is the asymmetry that makes reload predictable.
func TestReload_BootProcessEnvWins(t *testing.T) {
	isolateEnv(t)
	t.Setenv("FLEET_MAX_COST_USD", "50") // pinned in the process env at boot
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\n") // file loses to the process env
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LiveMaxCostUSD() != 50 {
		t.Fatalf("boot: process env should win; got %v want 50", cfg.LiveMaxCostUSD())
	}

	writeEnv(t, envPath, "FLEET_MAX_COST_USD=99\n") // operator edits the file
	res, err := cfg.Reload(envPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.LiveMaxCostUSD() != 50 {
		t.Errorf("reload: process env should still win; got %v want 50", cfg.LiveMaxCostUSD())
	}
	if len(res.Changed) != 0 {
		t.Errorf("no change expected (process env pinned); got %+v", res.Changed)
	}
}

// TestReload_NilStateGettersReadDirectly: a Config built directly (no Load) has a
// nil reload state; the Live* getters must read the field directly without
// panicking — that is the path the test suite's hand-built Configs take.
func TestReload_NilStateGettersReadDirectly(t *testing.T) {
	cfg := &Config{MaxCostUSD: 7.5, MaxTotalTokens: 9, MaxIterations: 11, Temperature: 0.4, LLMTemperature: 0.6}
	if cfg.LiveMaxCostUSD() != 7.5 || cfg.LiveMaxTotalTokens() != 9 || cfg.LiveMaxIterations() != 11 ||
		cfg.LiveTemperature() != 0.4 || cfg.LiveLLMTemperature() != 0.6 {
		t.Errorf("nil-state getters did not read fields directly: %+v", cfg)
	}
	if _, err := cfg.Reload(""); err == nil {
		t.Error("Reload on a non-Load Config should error, not panic or no-op")
	}
}

// TestReload_RaceSafe runs concurrent Live* reads against repeated reloads; under
// `-race` it proves the runtime read path is synchronized with Reload's writes.
func TestReload_RaceSafe(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\n")
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = os.WriteFile(envPath, []byte(fmt.Sprintf("FLEET_MAX_COST_USD=%d\n", 10+i%7)), 0o600)
			_, _ = cfg.Reload(envPath)
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = cfg.LiveMaxCostUSD()
					_ = cfg.LiveMaxIterations()
					_ = cfg.LiveTemperature()
					_ = cfg.LiveMaxTotalTokens()
					_ = cfg.LiveLLMTemperature()
				}
			}
		}()
	}
	<-done
	readers.Wait()
}
