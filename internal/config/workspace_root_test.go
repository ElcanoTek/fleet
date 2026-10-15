package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPinWorkspaceRootDefaultsToAbsoluteCwdWorkspace: with no root configured
// the pin resolves ./workspace against the cwd and exports it, so every lazy
// env reader sees the same ABSOLUTE root an operator would have set by hand.
func TestPinWorkspaceRootDefaultsToAbsoluteCwdWorkspace(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.PinWorkspaceRoot()
	want := filepath.Join(dir, "workspace")
	if cfg.WorkspaceRoot != want {
		t.Fatalf("cfg.WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if got := os.Getenv("FLEET_WORKSPACE_ROOT"); got != want {
		t.Fatalf("FLEET_WORKSPACE_ROOT = %q, want %q", got, want)
	}
}

// TestPinWorkspaceRootSurvivesReload: a RELATIVE operator value is pinned
// absolute, and a reload — which re-applies both the env file and the boot env
// snapshot holding the relative spelling — must not put the relative value
// back, or the lazy readers would split from cfg after the first SIGUSR2.
func TestPinWorkspaceRootSurvivesReload(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	want := filepath.Join(dir, "custom", "ws")

	for _, tc := range []struct {
		name    string
		fileEnv string
		procEnv bool
	}{
		{name: "env file", fileEnv: "FLEET_WORKSPACE_ROOT=custom/ws\n"},
		{name: "process env", procEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			envPath := filepath.Join(t.TempDir(), ".env")
			writeEnv(t, envPath, tc.fileEnv)
			if tc.procEnv {
				t.Setenv("FLEET_WORKSPACE_ROOT", "custom/ws")
			}
			cfg, err := Load(envPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cfg.PinWorkspaceRoot()
			if cfg.WorkspaceRoot != want {
				t.Fatalf("cfg.WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
			}
			if _, err := cfg.Reload(envPath); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			if got := os.Getenv("FLEET_WORKSPACE_ROOT"); got != want {
				t.Fatalf("after reload FLEET_WORKSPACE_ROOT = %q, want pinned %q", got, want)
			}
		})
	}
}
