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
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "")
	if p := validateOptionalEnvVars(); len(p) != 0 {
		t.Errorf("unset should be clean, got %v", p)
	}

	// Well-formed.
	t.Setenv("FLEET_MAX_COST_USD", "12.5")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "8")
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "30")
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
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "8")

	// Zero explicitly disables terminal queue-row retention; negative is invalid.
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "0")
	if p := validateOptionalEnvVars(); len(p) != 0 {
		t.Errorf("zero input queue retention should be valid, got %v", p)
	}
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "-1")
	if p := validateOptionalEnvVars(); len(p) != 1 || !strings.Contains(p[0], "FLEET_INPUT_QUEUE_RETENTION_DAYS") {
		t.Errorf("negative input queue retention should flag, got %v", p)
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
	cfg := &config.Config{Persona: "personas/assistant.yaml", PersonaDefault: "assistant"}
	res := checkManifest(bundle, nil, cfg)
	if res.Status != statusOK {
		t.Errorf("good bundle manifest check = %s: %s", res.Status, res.Detail)
	}
	if !res.Blocking {
		t.Error("manifest check must be blocking")
	}
}

// TestCheckManifestUnknownFieldFailsLikeBoot pins issue #902's expectation:
// validate-config loads the bundle through the SAME strict decoder the serve
// boot path uses (clientconfig.Load), so a manifest with an unknown field —
// e.g. a typo'd branding key — is a blocking manifest failure carrying boot's
// "unknown field" error class, never a green validate followed by a
// crash-looping restart.
func TestCheckManifestUnknownFieldFailsLikeBoot(t *testing.T) {
	dir := t.TempDir()
	manifest := "branding:\n  logo_typo: \"x\"\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, bundleErr := clientconfig.Load(dir)
	if bundleErr == nil || !strings.Contains(bundleErr.Error(), `unknown field "logo_typo"`) {
		t.Fatalf("strict load must reject the unknown field, got: %v", bundleErr)
	}
	res := checkManifest(bundle, bundleErr, nil)
	if res.Status != statusFail || !res.Blocking {
		t.Errorf("unknown-field manifest check = %s blocking=%v, want a blocking failure", res.Status, res.Blocking)
	}
	if !strings.Contains(res.Detail, "unknown field") {
		t.Errorf("detail should carry the boot error class, got %q", res.Detail)
	}
}

// personaBundle builds a minimal loadable bundle whose personas/ holds exactly
// the named files — the shape of a client bundle that calls its persona
// something other than the loader's built-in assistant default.
func personaBundle(t *testing.T, personaFiles ...string) *clientconfig.Bundle {
	t.Helper()
	dir := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "manifest.yaml"), "mcp_servers: []\n")
	write(filepath.Join(dir, "system_prompts", "chat.md"), "chat\n")
	write(filepath.Join(dir, "system_prompts", "default.md"), "default\n")
	for _, name := range personaFiles {
		write(filepath.Join(dir, "personas", name), "role: test\n")
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return bundle
}

// TestCheckManifestInteractivePersonaDefaultMissingBlocks pins the severity the
// runtime justifies: agent.Manager.RunTurn feeds cfg.PersonaDefault to
// buildSystemPrompt, whose ReadFile miss returns an error, so a deployment whose
// interactive default is not in the bundle fails EVERY chat turn. The check used
// to look at cfg.Persona only, leaving that with no exit-code signal at all.
func TestCheckManifestInteractivePersonaDefaultMissingBlocks(t *testing.T) {
	bundle := personaBundle(t, "victoria.yaml")
	for _, name := range []string{"assistant", "does-not-exist"} {
		cfg := &config.Config{PersonaDefault: name, Persona: "personas/victoria.yaml"}
		res := checkManifest(bundle, nil, cfg)
		if res.Status != statusFail || !res.Blocking {
			t.Fatalf("default %q = %s blocking=%v, want a blocking failure: %s", name, res.Status, res.Blocking, res.Detail)
		}
		if !strings.Contains(res.Detail, "FLEET_PERSONA_DEFAULT") || !strings.Contains(res.Detail, "victoria") {
			t.Errorf("default %q: detail should name the knob and the bundle's personas, got %q", name, res.Detail)
		}
		if code := emitReport(&bytes.Buffer{}, []checkResult{res}, false); code != 1 {
			t.Errorf("default %q: exit = %d, want 1 — a turn-fatal default must fail the run", name, code)
		}
	}
}

// TestCheckManifestScheduledPersonaMissingIsAdvisory pins issue #956: a bundle
// that names its persona anything other than the loader's built-in
// personas/assistant.yaml failed the whole manifest check with a blocking ✗
// unless PERSONA happened to be exported in the validating shell. The scheduled
// driver ignores its persona ReadFile error, so a miss is a warning — carrying
// blocking=false, since #248's --json contract exposes that flag on its own.
func TestCheckManifestScheduledPersonaMissingIsAdvisory(t *testing.T) {
	bundle := personaBundle(t, "victoria.yaml")
	for _, persona := range []string{"personas/assistant.yaml", "personas/does-not-exist.yaml"} {
		cfg := &config.Config{PersonaDefault: "victoria", Persona: persona}
		res := checkManifest(bundle, nil, cfg)
		if res.Status != statusWarn || res.Blocking {
			t.Fatalf("persona %q = %s blocking=%v, want a non-blocking warn: %s", persona, res.Status, res.Blocking, res.Detail)
		}
		// The suggestion must be in the shape FLEET_PERSONA takes (a
		// bundle-relative path), not the bare name FLEET_PERSONA_DEFAULT takes.
		if !strings.Contains(res.Detail, "FLEET_PERSONA to one of: personas/victoria.yaml") {
			t.Errorf("persona %q: detail should offer the bundle's personas as paths, got %q", persona, res.Detail)
		}
		if code := emitReport(&bytes.Buffer{}, []checkResult{res}, false); code != 0 {
			t.Errorf("persona %q: exit = %d, want 0 — an advisory must not fail the run", persona, code)
		}
	}
}

// TestCheckManifestPersonaResolvesLikeTheReaders: both readers open the persona
// by BASENAME out of personas/, so a bundle-relative path and a bare filename
// name the same file — resolving against the bundle root instead reported
// "victoria.yaml missing" for a bundle that ships exactly that file. Only the
// interactive reader appends .yaml, so only that knob takes an extensionless
// name; cfg.Persona always arrives with one (config.Load appends it).
func TestCheckManifestPersonaResolvesLikeTheReaders(t *testing.T) {
	bundle := personaBundle(t, "victoria.yaml")
	for _, spelling := range []string{"personas/victoria.yaml", "victoria.yaml", "victoria"} {
		cfg := &config.Config{PersonaDefault: spelling, Persona: "personas/victoria.yaml"}
		if res := checkManifest(bundle, nil, cfg); res.Status != statusOK {
			t.Errorf("FLEET_PERSONA_DEFAULT=%q = %s: %s", spelling, res.Status, res.Detail)
		}
	}
	for _, spelling := range []string{"personas/victoria.yaml", "victoria.yaml"} {
		cfg := &config.Config{PersonaDefault: "victoria", Persona: spelling}
		if res := checkManifest(bundle, nil, cfg); res.Status != statusOK {
			t.Errorf("FLEET_PERSONA=%q = %s: %s", spelling, res.Status, res.Detail)
		}
	}
}

// TestCheckManifestBlockingProblemOutranksPersonaAdvisory: downgrading the
// scheduled persona miss must not soften a genuinely missing system prompt
// sharing the check, and must not drop the advisory from the report either.
func TestCheckManifestBlockingProblemOutranksPersonaAdvisory(t *testing.T) {
	bundle := personaBundle(t, "victoria.yaml")
	if err := os.Remove(filepath.Join(bundle.SystemPromptsDir, "chat.md")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PersonaDefault: "victoria", Persona: "personas/assistant.yaml"}
	res := checkManifest(bundle, nil, cfg)
	if res.Status != statusFail || !res.Blocking {
		t.Fatalf("status = %s blocking=%v, want a blocking failure: %s", res.Status, res.Blocking, res.Detail)
	}
	if !strings.Contains(res.Detail, "chat.md") || !strings.Contains(res.Detail, "set FLEET_PERSONA to") {
		t.Errorf("detail should carry both findings, got %q", res.Detail)
	}
}

// TestCheckManifestYmlPersonasAreNotOffered: the persona rosters are .yaml-only
// and the interactive loader forces a ".yaml" suffix onto the configured name,
// so a .yml file can never back a chat persona. The inventory used to accept
// .yml too, so a victoria.yml-only bundle looked persona-equipped and the
// report offered a remediation that loops — setting FLEET_PERSONA_DEFAULT to
// the suggestion still resolves to a victoria.yaml that does not exist — while
// chat's roster was empty. Such a bundle must report as shipping no personas.
func TestCheckManifestYmlPersonasAreNotOffered(t *testing.T) {
	bundle := personaBundle(t, "victoria.yml")
	cfg := &config.Config{PersonaDefault: "victoria", Persona: "personas/assistant.yaml"}
	res := checkManifest(bundle, nil, cfg)
	if res.Status != statusFail || !res.Blocking {
		t.Fatalf("status = %s blocking=%v, want a blocking failure: %s", res.Status, res.Blocking, res.Detail)
	}
	if strings.Contains(res.Detail, "victoria.yml") {
		t.Errorf("detail offers a .yml file no persona roster can load, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "the bundle ships no personas") {
		t.Errorf("detail should report a persona-less bundle, got %q", res.Detail)
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
		UploadMaxBytes:  1 << 30,
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
