package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
)

// mcpTestBundle copies the shipped generic bundle into a temp dir and appends
// the given mcp_servers YAML block, so clientconfig.Load sees a fully valid
// bundle whose catalog is exactly the test's servers.
func mcpTestBundle(t *testing.T, serversYAML string) string {
	t.Helper()
	src := repoConfigDefault(t)
	dst := t.TempDir()
	if out, err := exec.Command("cp", "-r", src+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("copy bundle: %v: %s", err, out)
	}
	manifest := filepath.Join(dst, "manifest.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	patched := strings.Replace(string(raw), "mcp_servers: []", serversYAML, 1)
	if patched == string(raw) {
		t.Fatal("manifest.yaml no longer contains the empty mcp_servers marker")
	}
	if err := os.WriteFile(manifest, []byte(patched), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dst
}

// dummyServerPath locates internal/mcp's stdio fixture from the module root.
func dummyServerPath(t *testing.T) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(repoConfigDefault(t))) // …/config/default → repo root
	p := filepath.Join(root, "internal", "mcp", "testdata", "dummy_server.py")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("dummy server fixture missing: %v", err)
	}
	return p
}

// The happy path: a real stdio handshake against the dummy fixture connects,
// lists its tools, and exits 0; a broken command is captured as that server's
// failure (exit 1) without aborting the sweep. Red/green: the verb is new.
func TestMCPTestVerb_ProbesStdioServers(t *testing.T) {
	dummy := dummyServerPath(t)
	bundle := mcpTestBundle(t, `mcp_servers:
  - name: dummy
    type: stdio
    command: python3
    args: ["`+dummy+`"]
    always: true
  - name: broken
    type: stdio
    command: /nonexistent-mcp-binary
    always: true
`)

	// --all sweeps both; the sweep must report per-server outcomes and fail
	// the run because one server failed.
	var out bytes.Buffer
	report := mcpTestReport{Passed: true}
	catalog := loadCatalogForTest(t, bundle)
	for _, name := range []string{"broken", "dummy"} {
		res := probeBundleServer(name, catalog[name], 30*time.Second, false)
		if !res.Connected {
			report.Passed = false
			report.Failed++
		}
		report.Results = append(report.Results, res)
	}
	code := emitMCPTestReport(&out, report, true)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (one server failed); output: %s", code, out.String())
	}

	var parsed mcpTestReport
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json output: %v", err)
	}
	byName := map[string]mcpTestResult{}
	for _, r := range parsed.Results {
		byName[r.Server] = r
	}
	d := byName["dummy"]
	if !d.Connected || d.ToolCount < 1 {
		t.Errorf("dummy = %+v, want connected with >=1 tool", d)
	}
	b := byName["broken"]
	if b.Connected || b.Error == "" {
		t.Errorf("broken = %+v, want a captured failure", b)
	}
	if parsed.Failed != 1 || parsed.Passed {
		t.Errorf("report = passed=%v failed=%d, want failed sweep", parsed.Passed, parsed.Failed)
	}
}

// A gate-disabled server is excluded from the catalog — requesting it by name
// must be distinguishable from a typo (both exit 1, but the message differs;
// here we pin the selection behavior).
func TestMCPTestVerb_SelectionAndGating(t *testing.T) {
	bundle := mcpTestBundle(t, `mcp_servers:
  - name: gated
    type: stdio
    command: python3
    enabled_env: ["FLEET_TEST_UNSET_GATE_VAR"]
`)
	t.Setenv("FLEET_TEST_UNSET_GATE_VAR", "")
	catalog := loadCatalogForTest(t, bundle)
	if _, ok := catalog["gated"]; ok {
		t.Fatal("gated server should be excluded by its enable gate")
	}

	targets, unknown := selectMCPTestTargets(catalog, mcpTestOptions{names: []string{"gated", "typo"}})
	if len(targets) != 0 || len(unknown) != 2 {
		t.Errorf("selection = targets %v unknown %v, want both unknown", targets, unknown)
	}

	// --all on an empty catalog selects nothing (the caller reports it).
	targets, unknown = selectMCPTestTargets(catalog, mcpTestOptions{all: true})
	if len(targets) != 0 || unknown != nil {
		t.Errorf("--all on empty catalog = %v/%v, want none", targets, unknown)
	}
}

// --deep calls advertised auth-status tools: a healthy upstream check reports
// ok with the result text; an isError result fails the check (and via
// deepFailed, the sweep); a server without an auth-status tool is skipped
// with an explanation rather than failed. Red/green: the flag is new.
func TestMCPTestVerb_DeepAuthStatus(t *testing.T) {
	fixture := filepath.Join(mustGetwd(t), "testdata", "authstatus_server.py")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	dummy := dummyServerPath(t)
	bundle := mcpTestBundle(t, `mcp_servers:
  - name: auth_ok
    type: stdio
    command: python3
    args: ["`+fixture+`"]
    always: true
  - name: auth_bad
    type: stdio
    command: python3
    args: ["`+fixture+`"]
    env:
      AUTH_FAIL: "1"
    always: true
  - name: plain
    type: stdio
    command: python3
    args: ["`+dummy+`"]
    always: true
`)
	catalog := loadCatalogForTest(t, bundle)

	ok := probeBundleServer("auth_ok", catalog["auth_ok"], 30*time.Second, true)
	if !ok.Connected || len(ok.DeepChecks) != 1 || !ok.DeepChecks[0].OK {
		t.Fatalf("auth_ok = %+v, want one passing deep check", ok)
	}
	if !strings.Contains(ok.DeepChecks[0].Detail, "authenticated") {
		t.Errorf("detail = %q, want the tool's text surfaced", ok.DeepChecks[0].Detail)
	}
	if deepFailed(ok) {
		t.Error("passing deep check must not mark the sweep failed")
	}

	bad := probeBundleServer("auth_bad", catalog["auth_bad"], 30*time.Second, true)
	if !bad.Connected || len(bad.DeepChecks) != 1 || bad.DeepChecks[0].OK {
		t.Fatalf("auth_bad = %+v, want one FAILING deep check on a connected server", bad)
	}
	if !strings.Contains(bad.DeepChecks[0].Detail, "401") {
		t.Errorf("detail = %q, want the error text surfaced", bad.DeepChecks[0].Detail)
	}
	if !deepFailed(bad) {
		t.Error("isError deep check must mark the sweep failed")
	}

	plain := probeBundleServer("plain", catalog["plain"], 30*time.Second, true)
	if !plain.Connected || len(plain.DeepChecks) != 1 || !plain.DeepChecks[0].OK || plain.DeepChecks[0].Tool != "" {
		t.Fatalf("plain = %+v, want a skipped (ok) placeholder deep check", plain)
	}
}

// mustGetwd is Getwd with a test failure instead of an error return.
func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMCPTestVerb_FlagParsing(t *testing.T) {
	if _, err := parseMCPTestFlags(nil); err == nil {
		t.Error("no names and no --all must be a usage error")
	}
	if _, err := parseMCPTestFlags([]string{"--all", "extra"}); err == nil {
		t.Error("--all plus names must be a usage error")
	}
	opts, err := parseMCPTestFlags([]string{"--json", "--timeout", "5s", "alpha", "beta"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.jsonOutput || opts.timeout != 5*time.Second || len(opts.names) != 2 {
		t.Errorf("opts = %+v", opts)
	}
}

// loadCatalogForTest loads the temp bundle exactly as runMCPTest does.
func loadCatalogForTest(t *testing.T, dir string) map[string]config.MCPServerConfig {
	t.Helper()
	b, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return b.MCPServerConfigs()
}
