package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
)

// TestParseValidateFlags covers the verb's flag surface: defaults, each flag, and
// an unknown flag erroring.
func TestParseValidateFlags(t *testing.T) {
	got, err := parseValidateFlags(nil)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if got.bundlePath != "" || got.skipNetworkChecks || got.jsonOutput {
		t.Errorf("unexpected defaults: %+v", got)
	}

	got, err = parseValidateFlags([]string{"--bundle-path", "config/default", "--skip-network-checks", "--json"})
	if err != nil {
		t.Fatalf("parse all flags: %v", err)
	}
	if got.bundlePath != "config/default" || !got.skipNetworkChecks || !got.jsonOutput {
		t.Errorf("flags not parsed: %+v", got)
	}

	if _, err := parseValidateFlags([]string{"--nope"}); err == nil {
		t.Error("expected error for unknown flag")
	}
}

// TestValidateOptionalEnvVars pins the well-formedness checks for the optional
// numeric knobs: unset is fine, a positive value is fine, a malformed/negative
// value is a problem.
func TestValidateOptionalEnvVars(t *testing.T) {
	// Unset: no problems.
	t.Setenv("FLEET_MAX_COST_USD", "")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "")
	if p := validateOptionalEnvVars(); len(p) != 0 {
		t.Errorf("unset should be clean, got %v", p)
	}

	// Well-formed.
	t.Setenv("FLEET_MAX_COST_USD", "12.5")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "8")
	if p := validateOptionalEnvVars(); len(p) != 0 {
		t.Errorf("well-formed should be clean, got %v", p)
	}

	// Malformed cost.
	t.Setenv("FLEET_MAX_COST_USD", "free")
	if p := validateOptionalEnvVars(); len(p) != 1 || !strings.Contains(p[0], "FLEET_MAX_COST_USD") {
		t.Errorf("malformed cost should flag, got %v", p)
	}
	t.Setenv("FLEET_MAX_COST_USD", "12.5")

	// Non-positive concurrency.
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "0")
	if p := validateOptionalEnvVars(); len(p) != 1 || !strings.Contains(p[0], "FLEET_MAX_CONCURRENT_AGENTS") {
		t.Errorf("zero concurrency should flag, got %v", p)
	}
}

// TestEmitReportExitCode verifies the exit-code contract: a blocking failure → 1,
// a non-blocking warn → 0.
func TestEmitReportExitCode(t *testing.T) {
	allOK := []checkResult{{Name: "a", Status: statusOK, Blocking: true}}
	if code := emitReport(&bytes.Buffer{}, allOK, false); code != 0 {
		t.Errorf("all-ok exit = %d, want 0", code)
	}

	warnOnly := []checkResult{
		{Name: "a", Status: statusOK, Blocking: true},
		{Name: "b", Status: statusWarn, Blocking: false},
	}
	if code := emitReport(&bytes.Buffer{}, warnOnly, false); code != 0 {
		t.Errorf("warn-only exit = %d, want 0", code)
	}

	blockingFail := []checkResult{
		{Name: "a", Status: statusFail, Blocking: true},
		{Name: "b", Status: statusWarn, Blocking: false},
	}
	if code := emitReport(&bytes.Buffer{}, blockingFail, false); code != 1 {
		t.Errorf("blocking-fail exit = %d, want 1", code)
	}

	// A non-blocking fail (e.g. an http MCP server) must NOT change the exit code.
	nonBlockingFail := []checkResult{
		{Name: "mcp_servers", Status: statusWarn, Blocking: false},
	}
	if code := emitReport(&bytes.Buffer{}, nonBlockingFail, false); code != 0 {
		t.Errorf("non-blocking warn exit = %d, want 0", code)
	}
}

// TestEmitReportJSON pins the --json envelope shape + values.
func TestEmitReportJSON(t *testing.T) {
	results := []checkResult{
		{Name: "env_vars", Status: statusOK, Blocking: true, Detail: "ok"},
		{Name: "database", Status: statusFail, Blocking: true, Detail: "refused"},
	}
	var buf bytes.Buffer
	code := emitReport(&buf, results, true)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var report validateReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}
	if report.Passed {
		t.Error("passed should be false")
	}
	if report.BlockingFailures != 1 {
		t.Errorf("blocking_failures = %d, want 1", report.BlockingFailures)
	}
	if len(report.Checks) != 2 || report.Checks[1].Status != statusFail {
		t.Errorf("checks not round-tripped: %+v", report.Checks)
	}
}

// TestStatusGlyph covers the glyph mapping.
func TestStatusGlyph(t *testing.T) {
	cases := map[checkStatus]string{statusOK: "✓", statusFail: "✗", statusWarn: "⚠"}
	for s, want := range cases {
		if got := statusGlyph(s); got != want {
			t.Errorf("glyph(%s) = %q, want %q", s, got, want)
		}
	}
}

// TestSortedServerNames verifies stable alphabetical ordering.
func TestSortedServerNames(t *testing.T) {
	m := map[string]config.MCPServerConfig{"web": {}, "bash": {}, "python": {}}
	got := sortedServerNames(m)
	want := []string{"bash", "python", "web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sorted = %v, want %v", got, want)
	}
}

// TestFileAndExecHelpers covers fileExists / isExecutableFile against a temp dir.
func TestFileAndExecHelpers(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	execFile := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(execFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !fileExists(plain) {
		t.Error("plain file should exist")
	}
	if fileExists(dir) {
		t.Error("dir should not count as a file")
	}
	if fileExists(filepath.Join(dir, "nope")) {
		t.Error("missing file should not exist")
	}
	if !isExecutableFile(execFile) {
		t.Error("0755 file should be executable")
	}
	if isExecutableFile(plain) {
		t.Error("0600 file should not be executable")
	}
}

// TestCheckManifestGoodBundle runs the manifest check against the shipped generic
// bundle — it must pass with the persona + system prompts present.
func TestCheckManifestGoodBundle(t *testing.T) {
	dir := repoConfigDefault(t)
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	cfg := &config.Config{Persona: "personas/assistant.yaml"}
	res := checkManifest(bundle, nil, cfg)
	if res.Status != statusOK {
		t.Errorf("good bundle manifest check = %s: %s", res.Status, res.Detail)
	}
	if !res.Blocking {
		t.Error("manifest check must be blocking")
	}
}

// TestCheckManifestMissingPersona escalates a missing referenced persona to a
// blocking failure.
func TestCheckManifestMissingPersona(t *testing.T) {
	bundle, err := clientconfig.Load(repoConfigDefault(t))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	cfg := &config.Config{Persona: "personas/does-not-exist.yaml"}
	res := checkManifest(bundle, nil, cfg)
	if res.Status != statusFail {
		t.Errorf("missing persona should fail, got %s: %s", res.Status, res.Detail)
	}
}

// TestCheckCredentialsEmptyCatalog: the generic bundle references no credential
// vars, so the check is ok and non-blocking.
func TestCheckCredentialsEmptyCatalog(t *testing.T) {
	bundle, err := clientconfig.Load(repoConfigDefault(t))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	res := checkCredentials(bundle, nil)
	if res.Status != statusOK || res.Blocking {
		t.Errorf("empty catalog creds = %s blocking=%v: %s", res.Status, res.Blocking, res.Detail)
	}
}

// TestCheckDatabaseSkipNetwork: with --skip-network-checks the DB check validates
// the DSN + distinctness without a live probe and stays blocking.
func TestCheckDatabaseSkipNetwork(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://u:p@localhost:5432/fleet_chat?sslmode=disable"}
	t.Setenv("FLEET_CHAT_DATABASE_URL", "")
	t.Setenv("FLEET_SCHED_DATABASE_URL", "postgres://u:p@localhost:5432/fleet_sched?sslmode=disable")
	t.Setenv("SCHED_DATABASE_URL", "")
	res := checkDatabase(t.Context(), cfg, nil, validateOptions{skipNetworkChecks: true})
	if res.Status != statusOK || !res.Blocking {
		t.Errorf("skip-network DB = %s blocking=%v: %s", res.Status, res.Blocking, res.Detail)
	}
}

// TestCheckDatabaseSameDB: chat and sched resolving to the SAME database is a
// blocking failure (the ensureDistinctDatabases invariant), even with the probe
// skipped.
func TestCheckDatabaseSameDB(t *testing.T) {
	same := "postgres://u:p@localhost:5432/fleet?sslmode=disable"
	cfg := &config.Config{DatabaseURL: same}
	t.Setenv("FLEET_CHAT_DATABASE_URL", "")
	t.Setenv("FLEET_SCHED_DATABASE_URL", same)
	t.Setenv("SCHED_DATABASE_URL", "")
	res := checkDatabase(t.Context(), cfg, nil, validateOptions{skipNetworkChecks: true})
	if res.Status != statusFail {
		t.Errorf("same-db should fail, got %s: %s", res.Status, res.Detail)
	}
}

// TestCheckEnvVarsMockMode: in mock mode the env check passes without an
// OpenRouter key, given the other required fields.
func TestCheckEnvVarsMockMode(t *testing.T) {
	cfg := &config.Config{
		MockMode:        true,
		SharedToken:     "tok",
		ConversationTTL: 14,
		UnpinnedCap:     50,
		DatabaseURL:     "postgres://u:p@localhost:5432/fleet_chat?sslmode=disable",
		TLSMode:         "off",
	}
	t.Setenv("FLEET_MAX_COST_USD", "")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "")
	res := checkEnvVars(cfg, nil)
	if res.Status != statusOK {
		t.Errorf("mock-mode env check = %s: %s", res.Status, res.Detail)
	}
}

// TestCheckEnvVarsMissingToken: a missing FLEET_SERVER_TOKEN is a blocking
// failure surfaced via cfg.Validate.
func TestCheckEnvVarsMissingToken(t *testing.T) {
	cfg := &config.Config{
		MockMode:        true,
		ConversationTTL: 14,
		UnpinnedCap:     50,
		DatabaseURL:     "postgres://u:p@localhost:5432/fleet_chat?sslmode=disable",
		TLSMode:         "off",
	}
	res := checkEnvVars(cfg, nil)
	if res.Status != statusFail || !strings.Contains(res.Detail, "FLEET_SERVER_TOKEN") {
		t.Errorf("missing token should fail, got %s: %s", res.Status, res.Detail)
	}
}

// repoConfigDefault locates the repo's config/default bundle from the test's cwd
// (cmd/fleet) by walking up to the module root.
func repoConfigDefault(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "config", "default")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("config/default not found from test cwd")
	return ""
}
