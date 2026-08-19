package config

// Tests for BootstrapEnvFile (#1123): the early, pre-bundle-interpolation
// application of the env file must not disturb Load's precedence or the
// reload state's boot-winner snapshot.

import (
	"os"
	"path/filepath"
	"testing"
)

// resetBootstrapWritten restores the package-global bookkeeping after a test
// that ran BootstrapEnvFile, so later tests' Load calls don't subtract this
// test's file-sourced keys from their own reload snapshots.
func resetBootstrapWritten(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		registerMu.Lock()
		bootstrapEnvFileWritten = map[string]bool{}
		registerMu.Unlock()
	})
}

// TestBootstrapEnvFile_FileValuesStayReloadable: the bootstrap application
// writes file values into the process env BEFORE Load runs. Load must still
// classify them as FILE-sourced (not process-env boot winners), or a
// hot-reload could never pick up an operator's env-file edit again.
func TestBootstrapEnvFile_FileValuesStayReloadable(t *testing.T) {
	isolateEnv(t)
	resetBootstrapWritten(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\n")
	t.Setenv("FLEET_MAX_COST_USD", "") // register teardown restore for the bootstrap write
	os.Unsetenv("FLEET_MAX_COST_USD")

	if err := BootstrapEnvFile(envPath); err != nil {
		t.Fatalf("BootstrapEnvFile: %v", err)
	}
	if got := os.Getenv("FLEET_MAX_COST_USD"); got != "10" {
		t.Fatalf("bootstrap should apply the file value; got %q", got)
	}
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LiveMaxCostUSD(); got != 10 {
		t.Fatalf("boot LiveMaxCostUSD = %v, want 10", got)
	}

	writeEnv(t, envPath, "FLEET_MAX_COST_USD=22.5\n") // operator edits the file
	if _, err := cfg.Reload(envPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := cfg.LiveMaxCostUSD(); got != 22.5 {
		t.Errorf("reload LiveMaxCostUSD = %v, want 22.5 — a bootstrap-applied file value must stay reloadable, not freeze as a process winner", got)
	}
}

// TestBootstrapEnvFile_ProcessEnvStillWins: bootstrap keeps Load's precedence
// bit-for-bit — a process-env value beats the file at bootstrap, at Load, and
// across a reload.
func TestBootstrapEnvFile_ProcessEnvStillWins(t *testing.T) {
	isolateEnv(t)
	resetBootstrapWritten(t)
	t.Setenv("FLEET_MAX_COST_USD", "50")
	envPath := filepath.Join(t.TempDir(), ".env")
	writeEnv(t, envPath, "FLEET_MAX_COST_USD=10\n")

	if err := BootstrapEnvFile(envPath); err != nil {
		t.Fatalf("BootstrapEnvFile: %v", err)
	}
	if got := os.Getenv("FLEET_MAX_COST_USD"); got != "50" {
		t.Fatalf("process env must win at bootstrap; got %q", got)
	}
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LiveMaxCostUSD(); got != 50 {
		t.Fatalf("boot LiveMaxCostUSD = %v, want the process winner 50", got)
	}

	writeEnv(t, envPath, "FLEET_MAX_COST_USD=99\n")
	if _, err := cfg.Reload(envPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := cfg.LiveMaxCostUSD(); got != 50 {
		t.Errorf("reload LiveMaxCostUSD = %v, want 50 — the process-env boot winner must keep winning", got)
	}
}

// TestBootstrapEnvFile_RepeatCallsMergeBookkeeping: a second application must
// MERGE its file-introduced keys into the bookkeeping, never replace it — a
// wholesale reset would promote the first application's file-sourced keys to
// "process winners" and silently freeze them against hot-reload.
func TestBootstrapEnvFile_RepeatCallsMergeBookkeeping(t *testing.T) {
	isolateEnv(t)
	resetBootstrapWritten(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	writeEnv(t, first, "FLEET_MAX_COST_USD=10\n")
	writeEnv(t, second, "FLEET_MAX_ITERATIONS=100\n")
	for _, name := range []string{"FLEET_MAX_COST_USD", "FLEET_MAX_ITERATIONS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if err := BootstrapEnvFile(first); err != nil {
		t.Fatalf("BootstrapEnvFile(first): %v", err)
	}
	if err := BootstrapEnvFile(second); err != nil {
		t.Fatalf("BootstrapEnvFile(second): %v", err)
	}
	registerMu.RLock()
	defer registerMu.RUnlock()
	for _, k := range []string{"FLEET_MAX_COST_USD", "FLEET_MAX_ITERATIONS"} {
		if !bootstrapEnvFileWritten[k] {
			t.Errorf("bootstrapEnvFileWritten missing %s — a repeat call must merge, not replace", k)
		}
	}
}

// TestBootstrapEnvFile_MissingFileIsNotAnError mirrors Load's contract.
func TestBootstrapEnvFile_MissingFileIsNotAnError(t *testing.T) {
	isolateEnv(t)
	resetBootstrapWritten(t)
	if err := BootstrapEnvFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("missing env file must not error: %v", err)
	}
	if err := BootstrapEnvFile(""); err != nil {
		t.Fatalf("empty path must not error: %v", err)
	}
}
