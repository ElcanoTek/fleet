package config

import (
	"path/filepath"
	"sync"
	"testing"
)

// Admin-settings live setters/getters. Load-bearing assertions: a setter's
// value is visible through the Live getter on a Load-produced config (guarded
// path), the nil-reload-state test-literal path works unguarded, and
// concurrent set/get is race-clean (exercised under `make test-race`).
func TestLiveOverrideSettersRoundTrip(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeEnv(t, envPath, "FLEET_PHONE_A_FRIEND_ENABLED=false\nFLEET_AUTO_TITLE=true\nFLEET_APPROVAL_TIMEOUT_SECONDS=900\n")

	cfg, err := Load(envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LivePhoneAFriendEnabled() {
		t.Fatal("env default should be false")
	}
	if !cfg.LiveAutoTitle() {
		t.Fatal("env default should be true")
	}
	if got := cfg.LiveApprovalTimeoutSeconds(); got != 900 {
		t.Fatalf("env default approval timeout = %d, want 900", got)
	}

	cfg.SetPhoneAFriendEnabled(true)
	cfg.SetAutoTitle(false)
	cfg.SetSubagentsEnabled(true)
	cfg.SetMemoryAutoIndexEnabled(true)
	cfg.SetErrorAnalysisEnabled(false)
	cfg.SetConnectorRecommendationsEnabled(true)
	cfg.SetContextHandlesEnabled(true)
	cfg.SetApprovalTimeoutSeconds(7200)
	if got := cfg.LiveApprovalTimeoutSeconds(); got != 7200 {
		t.Errorf("approval_timeout: got %d want 7200", got)
	}

	checks := []struct {
		name string
		got  bool
		want bool
	}{
		{"phone_a_friend", cfg.LivePhoneAFriendEnabled(), true},
		{"auto_title", cfg.LiveAutoTitle(), false},
		{"subagents", cfg.LiveSubagentsEnabled(), true},
		{"memory_autoindex", cfg.LiveMemoryAutoIndexEnabled(), true},
		{"error_analysis", cfg.LiveErrorAnalysisEnabled(), false},
		{"connector_recs", cfg.LiveConnectorRecommendationsEnabled(), true},
		{"context_handles", cfg.LiveContextHandlesEnabled(), true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}

	// Concurrent set/get across the shared lock — meaningful under -race.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); cfg.SetSubagentsEnabled(false) }()
		go func() { defer wg.Done(); _ = cfg.LiveSubagentsEnabled() }()
	}
	wg.Wait()
}

// TestLiveOverridesNilReloadState: a Config literal (no Load) reads and writes
// directly — the contract test-built configs rely on.
func TestLiveOverridesNilReloadState(t *testing.T) {
	cfg := &Config{ErrorAnalysisEnabled: true}
	if !cfg.LiveErrorAnalysisEnabled() {
		t.Fatal("literal field should read through")
	}
	cfg.SetErrorAnalysisEnabled(false)
	if cfg.LiveErrorAnalysisEnabled() {
		t.Fatal("setter should write through on nil reload state")
	}
}
