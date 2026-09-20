package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
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

// TestValidateEnvKnobsPreflight pins the registry-driven preflight (#1119):
// unset is fine, well-formed is fine, and a set-but-malformed or out-of-range
// value is a problem naming the variable — for the historical three knobs AND
// for knobs the old hand-list never covered. The checks now mirror the boot
// path exactly (same registry), so values boot accepts — a zero cost ceiling,
// zero concurrency ("use the runner default") — preflight clean here too.
// clearKnobEnv blanks every FLEET_/CHAT_/CUTLASS_-prefixed variable plus the
// registry's direct (unprefixed) keys, so ambient process env cannot leak into
// the exact-count assertions below. A blank value counts as unset for knob
// resolution, and t.Setenv restores the originals on teardown.
func clearKnobEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "FLEET_") || strings.HasPrefix(name, "CHAT_") || strings.HasPrefix(name, "CUTLASS_") {
			t.Setenv(name, "")
		}
	}
	for _, name := range []string{"CONVERSATION_TTL_DAYS", "CONVERSATION_UNPINNED_CAP", "LLM_MAX_TOKENS"} {
		t.Setenv(name, "")
	}
}

func TestValidateEnvKnobsPreflight(t *testing.T) {
	clearKnobEnv(t)

	// Unset: no problems.
	t.Setenv("FLEET_MAX_COST_USD", "")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "")
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "")
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("unset should be clean, got %v", p)
	}

	// Well-formed.
	t.Setenv("FLEET_MAX_COST_USD", "12.5")
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "8")
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "30")
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("well-formed should be clean, got %v", p)
	}

	// Malformed cost flags; an explicit 0 is legal (0 = no cost ceiling,
	// the documented agentcore budget convention) and preflights clean.
	t.Setenv("FLEET_MAX_COST_USD", "free")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_MAX_COST_USD") {
		t.Errorf("malformed cost should flag, got %v", p)
	}
	t.Setenv("FLEET_MAX_COST_USD", "0")
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("FLEET_MAX_COST_USD=0 (no cost ceiling) should preflight clean, got %v", p)
	}
	t.Setenv("FLEET_MAX_COST_USD", "12.5")

	// Concurrency must be >= 1: `fleet serve` hands it to admission.New,
	// which floors 0 to a box-wide cap of ONE turn — 0 is a misconfiguration,
	// not "use a default", so preflight rejects it like boot does.
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "-2")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_MAX_CONCURRENT_AGENTS") {
		t.Errorf("negative concurrency should flag, got %v", p)
	}
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "0")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_MAX_CONCURRENT_AGENTS") {
		t.Errorf("zero concurrency must be rejected at preflight like at boot, got %v", p)
	}
	t.Setenv("FLEET_MAX_CONCURRENT_AGENTS", "8")

	// Zero explicitly disables terminal queue-row retention; negative is invalid.
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "0")
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("zero input queue retention should be valid, got %v", p)
	}
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "-1")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_INPUT_QUEUE_RETENTION_DAYS") {
		t.Errorf("negative input queue retention should flag, got %v", p)
	}
	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "30")

	// Knobs the pre-#1119 hand-list never covered are preflighted now: the
	// fail-open lockdown boolean and a bare-number duration.
	t.Setenv("FLEET_LOCKDOWN_ONLY", "enabled")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_LOCKDOWN_ONLY") {
		t.Errorf("malformed lockdown boolean should flag, got %v", p)
	}
	t.Setenv("FLEET_LOCKDOWN_ONLY", "")
	t.Setenv("FLEET_CHAT_DB_CONNECT_TIMEOUT", "30")
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_CHAT_DB_CONNECT_TIMEOUT") {
		t.Errorf("bare-number duration should flag, got %v", p)
	}
	t.Setenv("FLEET_CHAT_DB_CONNECT_TIMEOUT", "")

	// #1273: knobs parsed OUTSIDE the loader are preflighted from the same
	// registry — including the three the verb's doc comment used to call out as
	// an exception, and one read by a package the loader never touches.
	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_MINUTE", "6O") // letter O
	if p := config.ValidateEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_SCHED_RATE_LIMIT_PER_MINUTE") {
		t.Errorf("malformed scheduler rate limit should flag, got %v", p)
	}
	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_MINUTE", "0") // 0 disables the window
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("a disabled rate-limit window should preflight clean, got %v", p)
	}
	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_MINUTE", "")
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "lots")
	if p := config.ValidateEnvKnobs(); len(p) != 1 ||
		!strings.Contains(p[0], "FLEET_SANDBOX_KATA_OVERHEAD_MB") {
		t.Errorf("malformed kata overhead should flag, got %v", p)
	}
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "")

	// The documented-lenient knob is preflighted too, but as an ADVISORY: it
	// must never appear in the blocking list, since boot does not refuse on it.
	t.Setenv("FLEET_OTEL_SAMPLE_RATIO", "half")
	if p := config.ValidateEnvKnobs(); len(p) != 0 {
		t.Errorf("the lenient OTEL ratio must not block, got %v", p)
	}
	if p := config.ValidateLenientEnvKnobs(); len(p) != 1 || !strings.Contains(p[0], "FLEET_OTEL_SAMPLE_RATIO") {
		t.Errorf("the lenient OTEL ratio should be reported as an advisory, got %v", p)
	}
}

// TestCheckEnvVarsLenientKnobIsAdvisory pins the validate-config wiring for the
// lenient class (#1273): a malformed lenient knob makes env_vars WARN and
// non-blocking (so the verb still exits 0), and the advisory carries the
// registry's rationale — while a strict knob keeps failing blockingly.
func TestCheckEnvVarsLenientKnobIsAdvisory(t *testing.T) {
	clearKnobEnv(t)
	t.Setenv("FLEET_OTEL_SAMPLE_RATIO", "half")

	cfg := &config.Config{
		SharedToken:     "placeholder-token",
		MockMode:        true,
		DatabaseURL:     "postgres://u:p@127.0.0.1:5432/chat?sslmode=disable",
		ConversationTTL: 30,
		UnpinnedCap:     100,
		UploadMaxBytes:  1 << 20,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test fixture config must be valid on its own: %v", err)
	}

	res := checkEnvVars(cfg, nil)
	if res.Status != statusWarn || res.Blocking {
		t.Fatalf("want a non-blocking warn, got status=%q blocking=%v detail=%q", res.Status, res.Blocking, res.Detail)
	}
	if res.failed() {
		t.Error("a lenient-knob advisory must not count as a blocking failure")
	}
	for _, want := range []string{"FLEET_OTEL_SAMPLE_RATIO", "not a boot failure", "AlwaysSample"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("advisory detail should mention %q, got: %q", want, res.Detail)
		}
	}

	// A strict knob alongside it still fails, and the advisory rides along.
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "lots")
	res = checkEnvVars(cfg, nil)
	if res.Status != statusFail || !res.Blocking {
		t.Fatalf("want a blocking fail, got status=%q blocking=%v", res.Status, res.Blocking)
	}
	if !strings.Contains(res.Detail, "FLEET_SANDBOX_KATA_OVERHEAD_MB") ||
		!strings.Contains(res.Detail, "FLEET_OTEL_SAMPLE_RATIO") {
		t.Errorf("detail should report both, got: %q", res.Detail)
	}
}

// TestSchedRateLimits pins the serve-start read of the three rate-limit windows
// through the registry (#1273): unset → the documented defaults, 0 → an
// explicitly disabled window, malformed → an error naming the variable (rather
// than the warn-and-default it used to be).
func TestSchedRateLimits(t *testing.T) {
	clearKnobEnv(t)

	rl, err := schedRateLimits()
	if err != nil {
		t.Fatalf("unset knobs must resolve to defaults: %v", err)
	}
	if rl.perMinute != 60 || rl.perDay != 500 || rl.globalPerMinute != 200 {
		t.Errorf("defaults = %+v, want 60/500/200", rl)
	}

	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_DAY", "0")
	rl, err = schedRateLimits()
	if err != nil || rl.perDay != 0 {
		t.Errorf("explicit 0 must disable the window: %+v, %v", rl, err)
	}

	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_DAY", "5OO") // letters
	if _, err := schedRateLimits(); err == nil {
		t.Error("a malformed window must be an error, not a silent default")
	} else if !strings.Contains(err.Error(), "FLEET_SCHED_RATE_LIMIT_PER_DAY") {
		t.Errorf("error should name the variable, got: %v", err)
	}

	t.Setenv("FLEET_SCHED_RATE_LIMIT_PER_DAY", "-1")
	if _, err := schedRateLimits(); err == nil {
		t.Error("a negative window must be refused (0 is the way to disable one)")
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

// TestCheckCredentialsEmptyCatalog: the generic bundle ships no connectors, so
// no credential is missing — EnvVarNames now inventories every manifest ${VAR}
// reference (#1123), and the generic manifest references the
// FLEET_SANDBOX_IMAGE / FLEET_SANDBOX_RUNTIME override knobs via "${...:-}"
// defaults. Default-carrying knobs are reported as "manifest defaults in
// effect", NOT as missing credentials: the check stays OK and non-blocking on
// a pristine install.
func TestCheckCredentialsEmptyCatalog(t *testing.T) {
	for _, name := range []string{"FLEET_SANDBOX_IMAGE", "FLEET_SANDBOX_RUNTIME"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	bundle, err := clientconfig.Load(repoConfigDefault(t))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	res := checkCredentials(bundle, nil)
	if res.Status != statusOK || res.Blocking {
		t.Errorf("empty catalog creds = %s blocking=%v: %s", res.Status, res.Blocking, res.Detail)
	}
	for _, want := range []string{"FLEET_SANDBOX_IMAGE", "FLEET_SANDBOX_RUNTIME", "manifest defaults in effect"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", res.Detail, want)
		}
	}
}

// TestCheckCredentialsSplitsDefaultsFromMissing: an absent bare-referenced
// credential still warns, listed apart from absent default-carrying knobs.
func TestCheckCredentialsSplitsDefaultsFromMissing(t *testing.T) {
	for _, name := range []string{"FLEET_SANDBOX_IMAGE", "FLEET_SANDBOX_RUNTIME", "VC_TEST_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    optional: true
    enabled_env: [VC_TEST_TOKEN]
    env:
      TOKEN: "${VC_TEST_TOKEN}"
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	res := checkCredentials(bundle, nil)
	if res.Status != statusWarn || res.Blocking {
		t.Fatalf("creds = %s blocking=%v: %s", res.Status, res.Blocking, res.Detail)
	}
	absent, defaults, split := strings.Cut(res.Detail, "manifest defaults in effect:")
	if !split {
		t.Fatalf("detail = %q, want the defaults-in-effect section", res.Detail)
	}
	if !strings.Contains(absent, "VC_TEST_TOKEN") || strings.Contains(defaults, "VC_TEST_TOKEN") {
		t.Errorf("detail = %q, want VC_TEST_TOKEN listed as absent, not a default", res.Detail)
	}
	if !strings.Contains(defaults, "FLEET_SANDBOX_IMAGE") || strings.Contains(absent, "FLEET_SANDBOX_IMAGE") {
		t.Errorf("detail = %q, want FLEET_SANDBOX_IMAGE listed under defaults in effect", res.Detail)
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

// TestCheckEnvVarsKnobProblemsReportedWhenLoadFails: the registry walk needs no
// *Config, so a malformed knob is still named when config.Load failed for an
// unrelated reason — the operator sees every env problem in one pass.
func TestCheckEnvVarsKnobProblemsReportedWhenLoadFails(t *testing.T) {
	clearKnobEnv(t)
	t.Setenv("FLEET_MAX_COST_USD", "5O")
	res := checkEnvVars(nil, errors.New("invalid FLEET_ALLOWED_IPS entry"))
	if res.Status != statusFail || !res.Blocking {
		t.Fatalf("load failure must be a blocking fail, got %s (blocking=%v)", res.Status, res.Blocking)
	}
	for _, want := range []string{"invalid FLEET_ALLOWED_IPS entry", "FLEET_MAX_COST_USD", `"5O"`} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail should mention %q, got %q", want, res.Detail)
		}
	}
}

// TestCheckSandboxNilConfigWarns pins the nil-cfg guard: when config.Load fails
// (easy to hit since #1119 made the loader refuse any malformed knob), cfg is
// nil and checkSandbox must degrade to a non-blocking warn instead of
// dereferencing it — env_vars already reports the blocking failure.
func TestCheckSandboxNilConfigWarns(t *testing.T) {
	res := checkSandbox(context.Background(), nil, nil)
	if res.Status != statusWarn {
		t.Errorf("status = %q, want %q", res.Status, statusWarn)
	}
	if res.Blocking {
		t.Error("nil-cfg sandbox check must be non-blocking (env_vars owns the load failure)")
	}
	if !strings.Contains(res.Detail, "config not loaded") {
		t.Errorf("detail should say the config was not loaded, got %q", res.Detail)
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

// TestPinBundleDirFromEnvFile pins the precedence that made a healthy box
// preflight as broken: FLEET_CLIENT_CONFIG_DIR lives only in the deployment env
// file, the unit's UnsetEnvironment= keeps it out of an operator's shell, and
// clientconfig.Dir() reads only the process env — so the bundle fell back to the
// RELATIVE default and every bundle-dependent check failed or degraded.
func TestPinBundleDirFromEnvFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("fills an unset variable from the file", func(t *testing.T) {
		t.Setenv(clientconfig.EnvDir, "")
		pinBundleDirFromEnvFile(envFile)
		if got := os.Getenv(clientconfig.EnvDir); got != "/opt/fleet/client" {
			t.Errorf("bundle dir = %q, want the env file's value", got)
		}
	})

	// The process env is what --bundle-path pins into before this runs, so this
	// case is also what keeps an explicit flag winning over the file.
	t.Run("never overrides the process env", func(t *testing.T) {
		t.Setenv(clientconfig.EnvDir, "/explicit/bundle")
		pinBundleDirFromEnvFile(envFile)
		if got := os.Getenv(clientconfig.EnvDir); got != "/explicit/bundle" {
			t.Errorf("bundle dir = %q, want the pre-set value untouched", got)
		}
	})

	// Every bootstrap failure is a deliberate no-op: a diagnostic that cannot
	// read its own env file must still run and name the REAL problem.
	t.Run("a missing file or absent key is a quiet no-op", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "other.env")
		if err := os.WriteFile(empty, []byte("SOMETHING_ELSE=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, path := range map[string]string{
			"absent key":   empty,
			"missing file": filepath.Join(t.TempDir(), "nope.env"),
		} {
			t.Run(name, func(t *testing.T) {
				t.Setenv(clientconfig.EnvDir, "")
				pinBundleDirFromEnvFile(path)
				if got := os.Getenv(clientconfig.EnvDir); got != "" {
					t.Errorf("bundle dir = %q, want it left unset", got)
				}
			})
		}
	})
}

// TestServiceStorePodmanExecRunsFromServiceHome pins the cwd rule: rootless
// podman re-execs and chdir()s back to the inherited working directory, which
// the service user may not be able to enter (a root shell's /root is 0700).
// The rule lives in PodmanExec.CommandContext, fed by
// sandbox.ServiceStorePodmanExec — the same contract the removed
// serviceStorePodmanCmd helper pinned, on the surface the preflights now share.
func TestServiceStorePodmanExecRunsFromServiceHome(t *testing.T) {
	execCtx, _ := sandbox.ServiceStorePodmanExec("fleet", "/var/lib/fleet", true)
	if got := execCtx.CommandContext(context.Background(), "", "info").Dir; got != "/var/lib/fleet" {
		t.Errorf("root + non-root service user: Dir = %q, want the service home", got)
	}
	// Running as the caller: no hop, so no reason to move the cwd.
	plain, _ := sandbox.ServiceStorePodmanExec("fleet", "/var/lib/fleet", false)
	if got := plain.CommandContext(context.Background(), "", "info").Dir; got != "" {
		t.Errorf("non-root caller: Dir = %q, want the inherited cwd", got)
	}
	root, _ := sandbox.ServiceStorePodmanExec("root", "/root", true)
	if got := root.CommandContext(context.Background(), "", "info").Dir; got != "" {
		t.Errorf("root service user: Dir = %q, want the inherited cwd", got)
	}
}

// TestCheckMCPCatalog pins the CI-facing structural gate over the FULL MCP
// catalog — including servers an enabled_env gate turns off, which
// checkMCPServers never sees (the regression this check exists for). Table-
// driven against an in-memory bundle; Dir points at a temp dir so
// ValidateMCPArgPaths has a real root for its relative script-arg lookups.
func TestCheckMCPCatalog(t *testing.T) {
	catalog := func(servers ...clientconfig.ServerDef) *clientconfig.Bundle {
		return &clientconfig.Bundle{Dir: t.TempDir(), MCPCatalog: servers}
	}

	cases := []struct {
		name       string
		bundle     *clientconfig.Bundle
		bundleErr  error
		wantStatus checkStatus
		// wantDetail, when non-empty, must appear in the result Detail.
		wantDetail string
	}{
		{
			// THE regression: the only server in the catalog is gated off (no
			// ENABLED_API_KEY here), so checkMCPServers would report "no enabled
			// MCP servers" and validate nothing. The breakage must still surface.
			name: "a gated-off server is still structurally validated",
			bundle: catalog(clientconfig.ServerDef{
				Name: "example_api", Type: "http", EnabledEnv: []string{"ENABLED_API_KEY"},
				URL: "://not-a-url",
			}),
			wantStatus: statusFail,
			wantDetail: "example_api",
		},
		{
			name:       "nil bundle degrades to a skip",
			bundle:     nil,
			wantStatus: statusWarn,
			wantDetail: "skipped (bundle not loaded)",
		},
		{
			name:       "bundle load error degrades to a skip",
			bundle:     catalog(),
			bundleErr:  errors.New("manifest: nope"),
			wantStatus: statusWarn,
			wantDetail: "skipped (bundle not loaded)",
		},
		{
			name:       "empty catalog is ok",
			bundle:     catalog(),
			wantStatus: statusOK,
			wantDetail: "no servers declared",
		},
		{
			name: "healthy catalog is ok — command need NOT be installed",
			// A command this machine does not have: structure, not installation.
			// This row pins the rule that keeps the CI gate from requiring the
			// bundles' tooling on the runner.
			bundle: catalog(
				clientconfig.ServerDef{Name: "local", Command: "definitely-not-installed-mcp-uvx-xyz", EnabledEnv: []string{"SOME_KEY"}},
				clientconfig.ServerDef{Name: "remote", Type: "http", URL: "https://example.invalid/mcp", Always: true},
			),
			wantStatus: statusOK,
			wantDetail: "2 server(s): structure ok",
		},
		{
			name:       "http server with empty url fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http"}),
			wantStatus: statusFail,
			wantDetail: "empty url",
		},
		{
			name:       "http url that does not parse fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "://broken"}),
			wantStatus: statusFail,
			wantDetail: "does not parse",
		},
		{
			name:       "http url with a non-http scheme fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "ftp://example.invalid/mcp"}),
			wantStatus: statusFail,
			wantDetail: "url scheme is not http or https", // the scheme is never echoed: "${API_KEY}://host" puts a value there
		},
		{
			name:       "stdio server with empty command fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "  "}),
			wantStatus: statusFail,
			wantDetail: "empty command",
		},
		{
			name: "duplicate names fail, reported once for the dupe",
			bundle: catalog(
				clientconfig.ServerDef{Name: "dup", Command: "a"},
				clientconfig.ServerDef{Name: "dup", Command: "b"},
				clientconfig.ServerDef{Name: "dup", Command: "c"},
			),
			wantStatus: statusFail,
			wantDetail: `mcp_catalog["dup"]: duplicate server name`,
		},
		{
			name:       "empty server name fails",
			bundle:     catalog(clientconfig.ServerDef{Name: " ", Command: "a"}),
			wantStatus: statusFail,
			wantDetail: "empty server name",
		},
		{
			name:       "env var name with surrounding whitespace fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", EnabledEnv: []string{" API_KEY"}}),
			wantStatus: statusFail,
			wantDetail: "surrounding whitespace",
		},
		{
			name: "missing script arg path is folded in from ValidateMCPArgPaths",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "python3", Args: []string{"mcp/missing_probe_script.py"},
			}),
			wantStatus: statusFail,
			wantDetail: "does not resolve to a file under the bundle",
		},
		{
			// The name becomes mcp_<server>_<tool>; providers reject a dot there.
			// The loader does not enforce this for manifest servers, so a gated
			// connector would pass boot and break the first turn that enabled it.
			name:       "server name with a dot fails (provider-safe shape)",
			bundle:     catalog(clientconfig.ServerDef{Name: "sales.api", Command: "a"}),
			wantStatus: statusFail,
			wantDetail: "1-64 chars of letters, digits",
		},
		{
			name:       "server name with a space fails (provider-safe shape)",
			bundle:     catalog(clientconfig.ServerDef{Name: "sales api", Command: "a"}),
			wantStatus: statusFail,
			wantDetail: "1-64 chars of letters, digits",
		},
		{
			// url.Parse is happy with these; nothing can dial them.
			name:       "http url with a scheme but no host fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "https:///mcp"}),
			wantStatus: statusFail,
			wantDetail: "has no host",
		},
		{
			name:       "opaque http url fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "http:foo"}),
			wantStatus: statusFail,
			wantDetail: "has no host",
		},
		{
			// enabled() looks every group member up verbatim, so a padded name
			// reads an unset var and the connector is silently disabled everywhere.
			name: "enabled_groups member with surrounding whitespace fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", EnabledGroups: [][]string{{"API_KEY"}, {" OTHER_KEY", "OTHER_SECRET"}},
			}),
			wantStatus: statusFail,
			wantDetail: `" OTHER_KEY"`,
		},
		{
			// allSet(nil) is vacuously true: an empty alternative enables the
			// server with no gate at all.
			name: "empty enabled_groups alternative fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", EnabledGroups: [][]string{{"API_KEY"}, {}},
			}),
			wantStatus: statusFail,
			wantDetail: "enabled_groups[1] is empty",
		},
		{
			// Well-formed groups must not false-fail — the whole point is that a
			// gated-off server is validated, not that gating is suspicious.
			name: "well-formed enabled_groups on a gated-off server is ok",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", EnabledGroups: [][]string{{"API_KEY"}, {"OTHER_KEY", "OTHER_SECRET"}},
			}),
			wantStatus: statusOK,
		},
		{
			// MCPServerConfigs copies the URL verbatim; net/http rejects it padded.
			name:       "http url with surrounding whitespace fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: " https://x.example.com/mcp"}),
			wantStatus: statusFail,
			wantDetail: "has surrounding whitespace",
		},
		{
			// The loader keeps the command verbatim; exec looks for " python3 ".
			name:       "stdio command with surrounding whitespace fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: " python3 "}),
			wantStatus: statusFail,
			wantDetail: "command has surrounding whitespace", // the value is never echoed: it may be ${VAR}-interpolated
		},
		{
			// The loader trims identity_env for its own lookup but propagates the
			// padded original; the named-account guard then reads it as unset.
			name: "identity_env entry with surrounding whitespace fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", Env: map[string]string{"OWNER_ID": "x"}, IdentityEnv: []string{" OWNER_ID "},
			}),
			wantStatus: statusFail,
			wantDetail: `" OWNER_ID "`,
		},
		{
			// Manifest headers get no validation in Load; net/http fails at send.
			name: "http header with an invalid name fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Headers: map[string]string{"Bad Header": "v"},
			}),
			wantStatus: statusFail,
			wantDetail: "is not a valid HTTP header name",
		},
		{
			name: "http header value with a line break fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Headers: map[string]string{"X-A": "1\r\nInjected: y"},
			}),
			wantStatus: statusFail,
			wantDetail: "contains a line break",
		},
		{
			name: "http headers duplicated under different casing fail",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Headers: map[string]string{"X-A": "1", "x-a": "2"},
			}),
			wantStatus: statusFail,
			wantDetail: "same header under different casing",
		},
		{
			name: "well-formed http headers are ok",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Always: true,
				Headers: map[string]string{"Authorization": "Bearer ${TOKEN}", "X-Tenant": "acme"},
			}),
			wantStatus: statusOK,
		},
		{
			// net/http rejects NUL/DEL/control bytes in a value at send time, not
			// only CR/LF.
			name: "http header value with a NUL byte fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Always: true, Headers: map[string]string{"X-A": "a\x00b"},
			}),
			wantStatus: statusFail,
			wantDetail: "byte net/http rejects",
		},
		{
			// Environment entries split at the first '='; os.Getenv("API=KEY") can
			// never find anything, so the connector stays silently disabled.
			name:       "gate var name containing '=' fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", EnabledEnv: []string{"API=KEY"}}),
			wantStatus: statusFail,
			wantDetail: `"API=KEY"`,
		},
		{
			// exec.Cmd.Start rejects a NUL anywhere in argv.
			name:       "stdio arg with a NUL byte fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "python3", Args: []string{"mcp\x00.py"}, EnabledEnv: []string{"K"}}),
			wantStatus: statusFail,
			wantDetail: "args[0] contains a NUL byte",
		},
		{
			// enabled() returns false for not-always + no gate: declared, sound,
			// and never offered on any box.
			name:       "server with no activation path fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a"}),
			wantStatus: statusFail,
			wantDetail: "no activation path",
		},
		{
			// optional: true is a picker hint, not a gate — the raptive class of bug.
			name:       "optional server with no gate still has no activation path",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", Optional: true}),
			wantStatus: statusFail,
			wantDetail: "no activation path",
		},
		{
			name:       "always: true with no gate is ok",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", Always: true}),
			wantStatus: statusOK,
		},
		{
			// url.Parse keeps ":99999"; the transport rejects it only when dialling.
			name:       "http url with an out-of-range port fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "https://example.com:99999/mcp", Always: true}),
			wantStatus: statusFail,
			wantDetail: `port "99999" is not in 1-65535`,
		},
		{
			name:       "http url with a valid explicit port is ok",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "https://example.com:8443/mcp", Always: true}),
			wantStatus: statusOK,
		},
		{
			// A malformed URL may carry userinfo or a signed query; the diagnostic
			// must name the server, never the URL.
			name:       "url diagnostics never echo the url",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: " https://user:s3cr3t@example.com/mcp?sig=abc", Always: true}),
			wantStatus: statusFail,
			wantDetail: "url has surrounding whitespace",
		},
		{
			// Host is ":443" but Hostname is "": nothing to dial or derive SNI from.
			name:       "http url with a port but no hostname fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "remote", Type: "http", URL: "https://:443/mcp", Always: true}),
			wantStatus: statusFail,
			wantDetail: "url has no host",
		},
		{
			// 59 chars passes the shape regex but mcp_<59>_<1 char> is already 65.
			name:       "server name that leaves no room for any tool name fails",
			bundle:     catalog(clientconfig.ServerDef{Name: strings.Repeat("a", 59), Command: "a", Always: true}),
			wantStatus: statusFail,
			wantDetail: "leaves no room for any tool name",
		},
		{
			// With an allowlist the exact generated name is checked: mcp_ + 10 + _ + 50 = 65.
			name: "declared tool whose generated name exceeds the provider cap fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "abcdefghij", Command: "a", Always: true, Tools: []string{"ok_tool", strings.Repeat("t", 50)},
			}),
			wantStatus: statusFail,
			wantDetail: "65-char name",
		},
		{
			// mcp_ + 10 + _ + 49 = 64 exactly: at the cap is fine.
			name: "declared tools that fit the provider cap are ok",
			bundle: catalog(clientconfig.ServerDef{
				Name: "abcdefghij", Command: "a", Always: true, Tools: []string{"ok_tool", strings.Repeat("t", 49)},
			}),
			wantStatus: statusOK,
		},
		{
			// A command with a path separator is bundle content, not a PATH
			// dependency; probeMCPServer resolves it against the bundle dir too.
			name:       "bundle-relative command that does not exist fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "./mcp/server", Always: true}),
			wantStatus: statusFail,
			wantDetail: `bundle-relative command "./mcp/server" is not an executable file`,
		},
		{
			name: "bundle-relative command that exists and is executable is ok",
			bundle: func() *clientconfig.Bundle {
				b := catalog(clientconfig.ServerDef{Name: "local", Command: "./mcp/server", Always: true})
				if err := os.MkdirAll(filepath.Join(b.Dir, "mcp"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(b.Dir, "mcp", "server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				return b
			}(),
			wantStatus: statusOK,
		},
		{
			// An absolute command names the box's filesystem, not the bundle's —
			// installation, exempt. Bare names likewise (pinned separately).
			name:       "absolute command path is not checked for existence",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "/opt/definitely/not/here/python3", Always: true}),
			wantStatus: statusOK,
		},
		{
			// The allowlist is matched exactly; a padded entry can never match.
			name:       "padded tools allowlist entry fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", Always: true, Tools: []string{" lookup "}}),
			wantStatus: statusFail,
			wantDetail: "tools[0] is blank or has surrounding whitespace",
		},
		{
			// A non-empty list of blanks filters every tool the server has.
			name:       "blank tools allowlist entry fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", Always: true, Tools: []string{""}}),
			wantStatus: statusFail,
			wantDetail: "tools[0] is blank",
		},
		{
			// account_vars → a named seat is registered as <server>_<account>, so
			// the budget is shared with the label. mcp_(4)+56+_(1)+<acct>+_(1)+x(1)
			// = 63 + len(acct): a one-character label still fits → ok, and the ok
			// detail reports the headroom so the operator knows the label cap.
			name: "account_vars server with one char of account headroom is ok and reports it",
			bundle: catalog(clientconfig.ServerDef{
				Name: strings.Repeat("s", 56), Command: "a", Always: true, Env: map[string]string{"API_KEY": "${API_KEY}"}, AccountVars: []string{"API_KEY"}, Tools: []string{"x"},
			}),
			wantStatus: statusOK,
			wantDetail: "account label headroom (chars, per account_vars server): " + strings.Repeat("s", 56) + "=1",
		},
		{
			// One char longer: no label of any length fits, so a named seat can
			// never be advertised. Without account_vars this same name passes.
			name: "account_vars server with no account headroom fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: strings.Repeat("s", 57), Command: "a", Always: true, Env: map[string]string{"API_KEY": "${API_KEY}"}, AccountVars: []string{"API_KEY"}, Tools: []string{"x"},
			}),
			wantStatus: statusFail,
			wantDetail: "no room for any account label",
		},
		{
			// A real shape from the bundle family (magnite_mcp + a 37-char tool):
			// 64 - (4 + 11 + 1 + 1 + 37) = 10, so `production` fits and the check
			// is ok — a fixed 16-char reservation would have failed it.
			name: "account_vars server with a long tool name reports realistic headroom",
			bundle: catalog(clientconfig.ServerDef{
				Name: "magnite_mcp", Command: "a", Always: true, Env: map[string]string{"K": "${K}"}, AccountVars: []string{"K"}, Tools: []string{"magnite_run_report_from_prompt_inputs"},
			}),
			wantStatus: statusOK,
			wantDetail: "magnite_mcp=10",
		},
		{
			// The tool name is advertised verbatim inside mcp_<server>_<tool>;
			// providers accept only [A-Za-z0-9_-] there.
			name:       "allowlisted tool name with a dot fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "a", Always: true, Tools: []string{"reports.run"}}),
			wantStatus: statusFail,
			wantDetail: "tools[0] contains characters providers reject",
		},
		{
			// "../bin/server" joins to a path outside the bundle — not shipped
			// with it, whatever happens to be there on this machine.
			name:       "bundle-relative command that escapes the bundle fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "../bin/server", Always: true}),
			wantStatus: statusFail,
			wantDetail: "escapes the bundle directory",
		},
		{
			// resolveMCPVariant refuses every named account on an http base, yet
			// AccountsFor would still publish the seats: selectable, never runnable.
			name: "account_vars on an http server fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "remote", Type: "http", URL: "https://x.example.com/mcp", Always: true, AccountVars: []string{"API_KEY"},
			}),
			wantStatus: statusFail,
			wantDetail: "account_vars is stdio-only",
		},
		{
			// resolveEnvMap looks optional_env up exactly; a typo drops nothing.
			name: "optional_env naming a key not in the env map fails",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", Always: true, Env: map[string]string{"TOKEN": "${TOKEN}"}, OptionalEnv: []string{"TOKEM"},
			}),
			wantStatus: statusFail,
			wantDetail: "optional_env[0] is not a key", // the entry is never echoed: it may be ${VAR}-interpolated
		},
		{
			name: "padded optional_env entry fails on spelling",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", Always: true, Env: map[string]string{"TOKEN": "${TOKEN}"}, OptionalEnv: []string{" TOKEN "},
			}),
			wantStatus: statusFail,
			wantDetail: `" TOKEN "`,
		},
		{
			name: "well-formed optional_env is ok",
			bundle: catalog(clientconfig.ServerDef{
				Name: "local", Command: "a", Always: true, Env: map[string]string{"TOKEN": "${TOKEN}"}, OptionalEnv: []string{"TOKEN"},
			}),
			wantStatus: statusOK,
		},
		{
			// A name that fails the provider shape is exactly the shape a pasted
			// or interpolated secret takes: identified by index, never printed.
			name:       "shape-invalid server name is labelled by index",
			bundle:     catalog(clientconfig.ServerDef{Name: "sales.api", Command: "a", Always: true}),
			wantStatus: statusFail,
			wantDetail: "mcp_catalog[#0]: name must be 1-64 chars",
		},
		{
			// ValidateMCPArgPaths joins-and-stats; a script that exists elsewhere in
			// the checkout would pass while the bundle alone does not ship it.
			name:       "relative script arg that escapes the bundle fails",
			bundle:     catalog(clientconfig.ServerDef{Name: "local", Command: "python3", Args: []string{"../shared/server.py"}, Always: true}),
			wantStatus: statusFail,
			wantDetail: "args[0] is a relative path that escapes the bundle directory",
		},
		{
			// account_vars is documented as informational for seat DISCOVERY while
			// the overlay reads Env's keys ("as env keys or account_vars",
			// docs/MCP-BUNDLE-ENV.md); two production bundles list the SOURCE
			// variables rather than the env keys. The check must not reject that.
			name: "account_vars naming a source variable rather than an env key is ok",
			bundle: catalog(clientconfig.ServerDef{
				Name: "email", Command: "a", Always: true,
				Env:         map[string]string{"AWS_ACCESS_KEY_ID": "${ACME_EMAIL_AWS_ACCESS_KEY_ID}"},
				AccountVars: []string{"ACME_EMAIL_AWS_ACCESS_KEY_ID"},
			}),
			wantStatus: statusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkMCPCatalog(tc.bundle, tc.bundleErr)
			if res.Name != "mcp_catalog" {
				t.Errorf("Name = %q, want mcp_catalog", res.Name)
			}
			if res.Blocking {
				t.Error("mcp_catalog must never be Blocking — the workflow gate keys on status, and Blocking would change operator-box exit codes")
			}
			if res.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", res.Status, tc.wantStatus, res.Detail)
			}
			if tc.wantDetail != "" && !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("Detail %q should contain %q", res.Detail, tc.wantDetail)
			}
		})
	}
}

// TestCheckMCPCatalogNeverChecksCommandInstallation is the explicit guard for
// the check's most important boundary: a stdio server naming a binary that is
// not on this machine (and not under the bundle) still comes back ok. Without
// this, a future "harmless" PATH-lookup addition would silently turn the CI
// gate red on every real bundle.
func TestCheckMCPCatalogNeverChecksCommandInstallation(t *testing.T) {
	bundle := &clientconfig.Bundle{Dir: t.TempDir(), MCPCatalog: []clientconfig.ServerDef{
		{Name: "uvx_server", Command: "uvx", Args: []string{"some-mcp-package"}, Always: true},
		{Name: "node_server", Command: "npx", Args: []string{"-y", "another-mcp-package"}, Always: true},
	}}
	res := checkMCPCatalog(bundle, nil)
	if res.Status != statusOK {
		t.Fatalf("a catalog naming only uninstalled commands = %q (%s), want %q — the check must validate structure, not installation", res.Status, res.Detail, statusOK)
	}
}

// TestCheckMCPCatalogFailsOnRejectedPluginServer: an Agent Plugin mcp.json
// server the loader skips as invalid never reaches MCPCatalog, and Load still
// succeeds — checkManifest demotes the problem to an advisory so a running box
// is not taken down by a plugin defect. Walking only the survivors would
// therefore report "ok" over a connector that just vanished. The catalog check
// must fold PluginProblems in, or the default CI gate stays green while a
// plugin server disappears. Goes through the real loader on a real fixture:
// there is no seam for injecting plugin problems, and there should not be.
func TestCheckMCPCatalogFailsOnRejectedPluginServer(t *testing.T) {
	t.Setenv("FLEET_DATA_DIR", t.TempDir()) // keep the plugin-data dir out of ~/.cache
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "skills_builtin: false\n")
	write("plugins/p/plugin.json", `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "p"}`)
	// "has.dot" is rejected for its name (it would become a tool name a
	// provider refuses) and skipped; "good_one" survives, so the catalog is
	// non-empty and the only signal that anything was lost is PluginProblems.
	write("plugins/p/mcp.json", `{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": {`+
		`"has.dot": {"type": "stdio", "command": "python3"}, `+
		`"good_one": {"type": "stdio", "command": "python3"}}}`)

	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load must succeed — the loader skips the bad server rather than failing: %v", err)
	}
	if len(bundle.PluginProblems()) == 0 {
		t.Fatal("fixture no longer produces a plugin problem; the test proves nothing")
	}
	if _, survived := bundle.MCPServerConfigs()["has.dot"]; survived {
		t.Fatal("fixture no longer skips the bad server; the test proves nothing")
	}

	res := checkMCPCatalog(bundle, nil)
	if res.Status != statusFail {
		t.Fatalf("Status = %q (%s), want %q: a skipped plugin server must fail the catalog check", res.Status, res.Detail, statusFail)
	}
	if !strings.Contains(res.Detail, "plugin:") || !strings.Contains(res.Detail, `"has.dot"`) {
		t.Errorf("Detail should name the dropped plugin server, got: %s", res.Detail)
	}
	if res.Blocking {
		t.Error("mcp_catalog must stay non-blocking even when plugin problems are folded in")
	}
}

// TestCheckMCPCatalogFailsWhenOnlyPluginServerIsRejected pins the order of
// operations the previous test cannot: no manifest servers, and the bundle's
// ONLY server is a plugin entry the loader skips. MCPCatalog is then empty,
// and an "empty catalog → ok" early return would report success over a bundle
// whose one connector just vanished. The plugin fold must run first.
func TestCheckMCPCatalogFailsWhenOnlyPluginServerIsRejected(t *testing.T) {
	t.Setenv("FLEET_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "skills_builtin: false\n")
	write("plugins/p/plugin.json", `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "p"}`)
	write("plugins/p/mcp.json", `{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": {`+
		`"has.dot": {"type": "stdio", "command": "python3"}}}`)

	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load must succeed — the loader skips the bad server rather than failing: %v", err)
	}
	if len(bundle.MCPCatalog) != 0 {
		t.Fatalf("fixture must leave the catalog EMPTY to prove the ordering; got %d server(s)", len(bundle.MCPCatalog))
	}
	if len(bundle.PluginProblems()) == 0 {
		t.Fatal("fixture no longer produces a plugin problem; the test proves nothing")
	}

	res := checkMCPCatalog(bundle, nil)
	if res.Status != statusFail {
		t.Fatalf("Status = %q (%s), want %q: an empty catalog must not short-circuit past the plugin problems", res.Status, res.Detail, statusFail)
	}
	if !strings.Contains(res.Detail, `"has.dot"`) {
		t.Errorf("Detail should name the dropped plugin server, got: %s", res.Detail)
	}
}

// TestCheckMCPCatalogNeverEchoesURL: a manifest url may carry userinfo or a
// signed query string, and validate-config output lands in JSON reports, CI
// job summaries and journals. Every URL diagnostic must name the server and
// never the URL — including the *url.Error text, which embeds it.
func TestCheckMCPCatalogNeverEchoesURL(t *testing.T) {
	const secret = "s3cr3t-token-value"
	for _, u := range []string{
		" https://user:" + secret + "@example.com/mcp",    // padded → whitespace branch
		"https://user:" + secret + "@example.com:99999/x", // port branch
		"://user:" + secret + "@bad",                      // parse-error branch (*url.Error embeds the URL)
		"ftp://user:" + secret + "@example.com/",          // scheme branch
	} {
		bundle := &clientconfig.Bundle{Dir: t.TempDir(), MCPCatalog: []clientconfig.ServerDef{
			{Name: "remote", Type: "http", URL: u, Always: true},
		}}
		res := checkMCPCatalog(bundle, nil)
		if res.Status != statusFail {
			t.Errorf("url %q: Status = %q, want fail", u, res.Status)
		}
		if strings.Contains(res.Detail, secret) {
			t.Errorf("url %q: diagnostic leaked the URL's embedded credential: %s", u, res.Detail)
		}
	}
}

// TestCheckManifestFiles pins the bundle-intrinsic half of checkManifest that
// CI gates: the two prompt files every box reads. A missing chat.md fails
// every interactive turn, yet Load and mcp_catalog both succeed without it.
func TestCheckManifestFiles(t *testing.T) {
	withPrompts := func(names ...string) *clientconfig.Bundle {
		dir := t.TempDir()
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("# prompt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return &clientconfig.Bundle{Dir: dir, SystemPromptsDir: dir}
	}
	cases := []struct {
		name       string
		bundle     *clientconfig.Bundle
		bundleErr  error
		wantStatus checkStatus
		wantDetail string
	}{
		{"both prompts present is ok", withPrompts("chat.md", "default.md"), nil, statusOK, "system prompts present"},
		{"missing chat.md fails", withPrompts("default.md"), nil, statusFail, "system prompt chat.md missing"},
		{"missing default.md fails", withPrompts("chat.md"), nil, statusFail, "system prompt default.md missing"},
		{"nil bundle degrades to a skip", nil, nil, statusWarn, "skipped (bundle not loaded)"},
		{"load error degrades to a skip", withPrompts("chat.md", "default.md"), errors.New("nope"), statusWarn, "skipped (bundle not loaded)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkManifestFiles(tc.bundle, tc.bundleErr)
			if res.Name != "manifest_files" {
				t.Errorf("Name = %q, want manifest_files", res.Name)
			}
			if res.Blocking {
				t.Error("manifest_files must never be Blocking — `manifest` owns the operator exit code; CI keys on status")
			}
			if res.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", res.Status, tc.wantStatus, res.Detail)
			}
			if !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("Detail %q should contain %q", res.Detail, tc.wantDetail)
			}
		})
	}
}

// TestCheckMCPCatalogIgnoresDeploymentLocalPluginRoots: an absolute
// plugin_roots entry such as /opt/fleet/site-plugins legitimately exists only
// on the deployment box (docs/AGENT-PLUGINS.md). The loader records its
// absence here as a problem; folding that into mcp_catalog would pin every PR
// of such a bundle red for a configuration that is valid in production. The
// catalog check must consume PluginEntryProblems only, and the root problem
// must still be visible through PluginProblems for the manifest advisory.
func TestCheckMCPCatalogIgnoresDeploymentLocalPluginRoots(t *testing.T) {
	t.Setenv("FLEET_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "site-plugins-not-on-this-machine")
	manifest := "skills_builtin: false\nplugin_roots: [\"" + missing + "\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load must succeed — a missing explicit root is a problem, not a load failure: %v", err)
	}
	if len(bundle.PluginRootProblems()) == 0 {
		t.Fatal("fixture no longer produces a root problem; the test proves nothing")
	}
	if n := len(bundle.PluginEntryProblems()); n != 0 {
		t.Fatalf("a missing root must not be classified as an entry problem, got %d: %v", n, bundle.PluginEntryProblems())
	}
	if len(bundle.PluginProblems()) == 0 {
		t.Error("PluginProblems() must still surface the root problem for the manifest advisory")
	}

	res := checkMCPCatalog(bundle, nil)
	if res.Status != statusOK {
		t.Fatalf("Status = %q (%s), want ok: a deployment-local plugin root must not fail the catalog gate", res.Status, res.Detail)
	}
}

// TestCheckMCPCatalogGatesMissingRelativePluginRoot is the other half of the
// root split: a RELATIVE plugin_roots entry (vendor/plugins) is bundle content
// by spec, so when that checked-in directory is deleted every plugin under it
// vanishes on every box — an entry-level defect the catalog gate must fail,
// not a machine fact to wave through.
func TestCheckMCPCatalogGatesMissingRelativePluginRoot(t *testing.T) {
	t.Setenv("FLEET_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("skills_builtin: false\nplugin_roots: [\"vendor/plugins\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load must succeed: %v", err)
	}
	if len(bundle.PluginEntryProblems()) == 0 {
		t.Fatalf("a missing RELATIVE root must be an entry problem; entry=%v root=%v", bundle.PluginEntryProblems(), bundle.PluginRootProblems())
	}
	if len(bundle.PluginRootProblems()) != 0 {
		t.Errorf("a relative root must not be classified deployment-local: %v", bundle.PluginRootProblems())
	}
	if res := checkMCPCatalog(bundle, nil); res.Status != statusFail || !strings.Contains(res.Detail, "vendor/plugins") {
		t.Fatalf("Status = %q (%s), want fail naming vendor/plugins", res.Status, res.Detail)
	}
}

// TestCheckMCPCatalogNeverEchoesInvalidName: on an operator run the real
// deployment env is loaded before the manifest is ${VAR}-interpolated, so
// `name: "${API_KEY}"` arrives as the key itself and the label would carry it
// into JSON and journal output. A name that fails the provider shape is
// identified by index and its text must appear nowhere in the detail.
func TestCheckMCPCatalogNeverEchoesInvalidName(t *testing.T) {
	const leaked = "sk-live.s3cr3t/value with space"
	bundle := &clientconfig.Bundle{Dir: t.TempDir(), MCPCatalog: []clientconfig.ServerDef{
		{Name: leaked, Command: "a", Always: true, EnabledEnv: []string{" PAD "}, Tools: []string{"bad.tool"},
			Env: map[string]string{"TOKEN": "${TOKEN}"}, OptionalEnv: []string{leaked}, AccountVars: []string{leaked}}, // interpolated values in optional_env / account_vars must not be echoed either
		{Name: leaked, Command: "", Always: true}, // duplicate + empty command: more diagnostics that carry the label
	}}
	res := checkMCPCatalog(bundle, nil)
	if res.Status != statusFail {
		t.Fatalf("Status = %q, want fail", res.Status)
	}
	if strings.Contains(res.Detail, leaked) || strings.Contains(res.Detail, "s3cr3t") {
		t.Fatalf("an invalid server name must never be echoed, got: %s", res.Detail)
	}
	for _, want := range []string{"mcp_catalog[#0]", "mcp_catalog[#1]", "duplicate server name"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("Detail should contain %q (index labels), got: %s", want, res.Detail)
		}
	}
}

// TestCheckMCPCatalogGatesEscapingRelativePluginRoot: a RELATIVE plugin root
// is bundle content, so "../shared/plugins" — which cleans to a directory the
// bundle does not ship — must be an entry problem even when that directory
// exists and loads fine on this machine. The loader still loads it (no running
// box changes); the preflight gates it.
func TestCheckMCPCatalogGatesEscapingRelativePluginRoot(t *testing.T) {
	t.Setenv("FLEET_DATA_DIR", t.TempDir())
	parent := t.TempDir()
	dir := filepath.Join(parent, "bundle")
	if err := os.MkdirAll(filepath.Join(parent, "shared", "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("skills_builtin: false\nplugin_roots: [\"../shared/plugins\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load must succeed — this is a problem, not a load failure: %v", err)
	}
	if len(bundle.PluginEntryProblems()) == 0 {
		t.Fatalf("an escaping relative root must be an entry problem; entry=%v root=%v", bundle.PluginEntryProblems(), bundle.PluginRootProblems())
	}
	if res := checkMCPCatalog(bundle, nil); res.Status != statusFail || !strings.Contains(res.Detail, "must stay inside the bundle") {
		t.Fatalf("Status = %q (%s), want fail naming the escaping root", res.Status, res.Detail)
	}
}

// TestCheckBundleSkills: Load logs a malformed bundle skill to stderr and
// carries on, which a CI gate reading the JSON report never sees. The check
// must turn ValidateSkills into a result the floor can gate.
func TestCheckBundleSkills(t *testing.T) {
	skillsDir := func(withSkillMD bool) string {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, "my-skill"), 0o755); err != nil {
			t.Fatal(err)
		}
		if withSkillMD {
			body := "---\nname: my-skill\ndescription: Does a thing.\n---\n\nBody.\n"
			if err := os.WriteFile(filepath.Join(d, "my-skill", "SKILL.md"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	cases := []struct {
		name       string
		bundle     *clientconfig.Bundle
		bundleErr  error
		wantStatus checkStatus
		wantDetail string
	}{
		{"well-formed skill is ok", &clientconfig.Bundle{Dir: t.TempDir(), BundleSkillsDir: skillsDir(true)}, nil, statusOK, "well-formed"},
		{"skill folder without SKILL.md fails", &clientconfig.Bundle{Dir: t.TempDir(), BundleSkillsDir: skillsDir(false)}, nil, statusFail, "my-skill"},
		{"nil bundle degrades to a skip", nil, nil, statusWarn, "skipped (bundle not loaded)"},
		{"load error degrades to a skip", &clientconfig.Bundle{Dir: t.TempDir(), BundleSkillsDir: skillsDir(true)}, errors.New("nope"), statusWarn, "skipped (bundle not loaded)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkBundleSkills(tc.bundle, tc.bundleErr)
			if res.Name != "bundle_skills" {
				t.Errorf("Name = %q, want bundle_skills", res.Name)
			}
			if res.Blocking {
				t.Error("bundle_skills must never be Blocking — CI keys on status; operator exit codes must not change")
			}
			if res.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", res.Status, tc.wantStatus, res.Detail)
			}
			if !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("Detail %q should contain %q", res.Detail, tc.wantDetail)
			}
		})
	}
}
