package clientconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Agent Plugins loader tests (plugins.go, ADR-0054). Every case builds a real
// bundle under t.TempDir() and goes through Load, so the assertions cover the
// same path production takes: plugin.json → Plugins inventory, skills/ → the
// merged roster Skills() serves, mcp.json → MCPServerConfigs(). TestMain pins
// FLEET_DATA_DIR to a temp dir, so the merged tree and PLUGIN_DATA dirs land
// there.

const testPluginSchema = `"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"`
const testMCPSchema = `"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"`

// pluginFixture describes one plugin dir to write: manifest JSON, skills by
// folder name → description, optional mcp.json body, optional extra files
// (plugin-relative path → content).
type pluginFixture struct {
	dir      string // folder name under plugins/ (defaults to the manifest name)
	manifest string
	skills   map[string]string
	mcp      string
	files    map[string]string
}

func writePluginBundle(t *testing.T, manifest string, plugins ...pluginFixture) string {
	t.Helper()
	dir := t.TempDir()
	if manifest == "" {
		manifest = "skills_builtin: false\n"
	}
	mustWrite(t, filepath.Join(dir, "manifest.yaml"), manifest)
	for _, p := range plugins {
		writePluginInto(t, filepath.Join(dir, "plugins"), p)
	}
	return dir
}

func writePluginInto(t *testing.T, root string, p pluginFixture) {
	t.Helper()
	name := p.dir
	if name == "" {
		name = "plug"
	}
	pdir := filepath.Join(root, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if p.manifest != "" {
		mustWrite(t, filepath.Join(pdir, "plugin.json"), p.manifest)
	}
	for skill, desc := range p.skills {
		mustWrite(t, filepath.Join(pdir, "skills", skill, "SKILL.md"),
			"---\nname: "+skill+"\ndescription: "+desc+"\n---\n\nBody of "+skill+".\n")
	}
	if p.mcp != "" {
		mustWrite(t, filepath.Join(pdir, "mcp.json"), p.mcp)
	}
	for rel, body := range p.files {
		mustWrite(t, filepath.Join(pdir, rel), body)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func minimalManifest(name string) string {
	return `{` + testPluginSchema + `, "name": "` + name + `"}`
}

func skillNames(skills []Skill) map[string]Skill {
	out := map[string]Skill{}
	for _, s := range skills {
		out[s.Name] = s
	}
	return out
}

func containsProblem(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// loadAcmeFixture builds the reference plugin — full manifest metadata, one
// skill, and an mcp.json exercising every transport — and returns the loaded
// bundle plus the plugin's resolved root.
func loadAcmeFixture(t *testing.T) (*Bundle, string) {
	t.Helper()
	dir := writePluginBundle(t, "", pluginFixture{
		dir: "acme",
		manifest: `{
  ` + testPluginSchema + `,
  "name": "acme-tools",
  "version": "1.2.0",
  "description": "Acme's portable toolkit",
  "author": {"name": "Acme", "email": "dev@acme.example", "url": "https://acme.example"},
  "homepage": "https://acme.example/docs",
  "repository": "https://github.com/acme/tools",
  "license": "MIT",
  "keywords": ["deploy", "ops"],
  "extensions": {"com.example.other": {"setting": true}}
}`,
		skills: map[string]string{"deploy": "Ship a release safely, with a rollback plan."},
		files: map[string]string{
			"bin/validator": "#!/bin/sh\nexit 0\n",
			"config.json":   "{}",
		},
		mcp: `{
  ` + testMCPSchema + `,
  "mcpServers": {
    "local_validator": {
      "type": "stdio",
      "command": "./bin/validator",
      "args": ["--data", "${PLUGIN_DATA}/validator", "${UNKNOWN}", "$${PLUGIN_ROOT}"],
      "env": {"CONFIG": "${PLUGIN_ROOT}/config.json", "LITERAL": "${HOME}"},
      "cwd": "${PLUGIN_ROOT}"
    },
    "bare_cmd": {"type": "stdio", "command": "python3", "args": ["-c", "pass"]},
    "remote": {
      "type": "streamable-http",
      "url": "https://tools.example.com/mcp",
      "headers": {"X-Tenant": "public-tenant"}
    },
    "loopback": {"type": "streamable-http", "url": "http://127.0.0.1:8080/mcp"},
    "legacy": {"type": "sse", "url": "https://legacy.example.com/sse"}
  }
}`,
	})
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1 (%v)", len(b.Plugins), b.PluginProblems())
	}
	root, err := filepath.EvalSymlinks(filepath.Join(dir, "plugins", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}

func TestPluginLoadsIdentityAndSkills(t *testing.T) {
	b, root := loadAcmeFixture(t)
	p := b.Plugins[0]
	if p.Name != "acme-tools" || p.Version != "1.2.0" || p.Description != "Acme's portable toolkit" {
		t.Errorf("plugin identity = %+v", p)
	}
	if p.Dir != root {
		t.Errorf("Dir = %q, want resolved root %q", p.Dir, root)
	}
	if p.DataDir == "" || !strings.HasSuffix(p.DataDir, filepath.Join(pluginDataDirName, "acme-tools")) {
		t.Errorf("DataDir = %q", p.DataDir)
	}
	if st, err := os.Stat(p.DataDir); err != nil || !st.IsDir() {
		t.Errorf("PLUGIN_DATA dir not created: %v", err)
	}
	if strings.Join(p.Skills, ",") != "deploy" {
		t.Errorf("Skills = %v", p.Skills)
	}
	if strings.Join(p.MCPServers, ",") != "bare_cmd,local_validator,loopback,remote" {
		t.Errorf("MCPServers = %v", p.MCPServers)
	}

	// The skill made it into the merged roster with the standard handle, and
	// its provenance is the plugin — even with the built-in pack disabled.
	roster := skillNames(b.Skills())
	sk, ok := roster["deploy"]
	if !ok {
		t.Fatalf("plugin skill missing from roster: %v", roster)
	}
	if sk.Path != filepath.Join("skills", "deploy", "SKILL.md") {
		t.Errorf("roster Path = %q", sk.Path)
	}
	if b.SkillsDir == b.BundleSkillsDir {
		t.Error("a plugin skill must force the merged tree even when skills_builtin is false")
	}
	if got := b.SkillOrigin("deploy"); got != (SkillOrigin{Source: "plugin", Plugin: "acme-tools"}) {
		t.Errorf("SkillOrigin = %+v", got)
	}
}

func TestPluginLoadsStdioServers(t *testing.T) {
	b, root := loadAcmeFixture(t)
	p := b.Plugins[0]
	cfgs := b.MCPServerConfigs()
	lv, ok := cfgs["local_validator"]
	if !ok {
		t.Fatalf("local_validator missing: %v", b.PluginProblems())
	}
	if lv.Type != "stdio" || !lv.Enabled {
		t.Errorf("local_validator = %+v", lv)
	}
	if lv.Command != filepath.Join(root, "bin", "validator") {
		t.Errorf("./-relative command not resolved under the root: %q", lv.Command)
	}
	if lv.Dir != root {
		t.Errorf("Dir = %q, want plugin root %q", lv.Dir, root)
	}
	wantArgs := []string{"--data", p.DataDir + "/validator", "${UNKNOWN}", "$" + root}
	if strings.Join(lv.Args, "|") != strings.Join(wantArgs, "|") {
		t.Errorf("args = %q, want %q (single-pass expansion; unknown placeholders literal)", lv.Args, wantArgs)
	}
	if lv.Env["CONFIG"] != root+"/config.json" {
		t.Errorf("env CONFIG = %q", lv.Env["CONFIG"])
	}
	if lv.Env["LITERAL"] != "${HOME}" {
		t.Errorf("env LITERAL = %q: fleet's ${VAR} interpolation must NOT run over plugin env values", lv.Env["LITERAL"])
	}
	if lv.Env["PLUGIN_ROOT"] != root || lv.Env["PLUGIN_DATA"] != p.DataDir {
		t.Errorf("reserved vars: PLUGIN_ROOT=%q PLUGIN_DATA=%q", lv.Env["PLUGIN_ROOT"], lv.Env["PLUGIN_DATA"])
	}
	if bc := cfgs["bare_cmd"]; bc.Command != "python3" || bc.Dir != root {
		t.Errorf("bare command must stay bare and launch in the plugin root: %+v", bc)
	}
	// Plugin args are opaque: the bundle-relative script check ignores them.
	if probs := b.ValidateMCPArgPaths(); len(probs) != 0 {
		t.Errorf("ValidateMCPArgPaths flagged plugin args: %v", probs)
	}
}

func TestPluginLoadsHTTPServersAndGates(t *testing.T) {
	b, _ := loadAcmeFixture(t)
	cfgs := b.MCPServerConfigs()
	rm, ok := cfgs["remote"]
	if !ok || rm.Type != "http" || rm.URL != "https://tools.example.com/mcp" || rm.Headers["X-Tenant"] != "public-tenant" {
		t.Errorf("remote = %+v", rm)
	}
	if _, ok := cfgs["loopback"]; !ok {
		t.Error("plain-http loopback URL must be accepted")
	}
	if _, ok := cfgs["legacy"]; ok {
		t.Error("sse entry must be skipped, not loaded")
	}
	if !containsProblem(b.PluginProblems(), `server "legacy"`) || !containsProblem(b.PluginProblems(), "HTTP+SSE") {
		t.Errorf("sse skip must be reported: %v", b.PluginProblems())
	}
	// Plugin servers are always-on and show up in the visible-but-locked list.
	servers := b.AlwaysOnServers()
	alwaysOn := make([]string, 0, len(servers))
	for _, s := range servers {
		alwaysOn = append(alwaysOn, s.Name)
	}
	if !strings.Contains(strings.Join(alwaysOn, ","), "local_validator") {
		t.Errorf("plugin servers should be listed always-on: %v", alwaysOn)
	}
	for i := range b.MCPCatalog {
		if b.MCPCatalog[i].Name == "remote" && b.MCPCatalog[i].PluginName() != "acme-tools" {
			t.Errorf("PluginName() = %q", b.MCPCatalog[i].PluginName())
		}
	}
}

func TestPluginManifestNonFatalDeviations(t *testing.T) {
	dir := writePluginBundle(t, "",
		pluginFixture{dir: "a", manifest: `{` + testPluginSchema + `, "name": "unknown-field", "color": "blue", "extensions": "nope"}`,
			skills: map[string]string{"one": "The first skill, still loaded."}},
	)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 1 || b.Plugins[0].Name != "unknown-field" {
		t.Fatalf("plugin with an unknown field must still load: %+v / %v", b.Plugins, b.PluginProblems())
	}
	probs := b.PluginProblems()
	if !containsProblem(probs, `unknown top-level field "color"`) {
		t.Errorf("unknown field must be REPORTED: %v", probs)
	}
	if !containsProblem(probs, `"extensions" is not an object`) {
		t.Errorf("non-object extensions must be reported and ignored: %v", probs)
	}
	if _, ok := skillNames(b.Skills())["one"]; !ok {
		t.Error("components must keep loading after the non-fatal deviations")
	}
}

func TestPluginManifestFatalViolations(t *testing.T) {
	cases := map[string]string{
		"missing name":        `{` + testPluginSchema + `}`,
		"empty name":          `{` + testPluginSchema + `, "name": ""}`,
		"uppercase name":      `{` + testPluginSchema + `, "name": "My-Plugin"}`,
		"leading hyphen":      `{` + testPluginSchema + `, "name": "-start"}`,
		"double hyphen":       `{` + testPluginSchema + `, "name": "has--double"}`,
		"double dot":          `{` + testPluginSchema + `, "name": "too.many..dots"}`,
		"missing schema":      `{"name": "x"}`,
		"unsupported version": `{"$schema": "https://agent-plugins.org/schemas/2.0.0/plugin.schema.json", "name": "x"}`,
		"version not string":  `{` + testPluginSchema + `, "name": "x", "version": 2}`,
		"keywords not array":  `{` + testPluginSchema + `, "name": "x", "keywords": "a,b"}`,
		"author extra field":  `{` + testPluginSchema + `, "name": "x", "author": {"name": "A", "twitter": "@a"}}`,
		"author not object":   `{` + testPluginSchema + `, "name": "x", "author": "A"}`,
		"extension not obj":   `{` + testPluginSchema + `, "name": "x", "extensions": {"com.example": 1}}`,
		"not an object":       `[1,2]`,
		"invalid json":        `{`,
	}
	for label, manifest := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writePluginBundle(t, "", pluginFixture{dir: "bad", manifest: manifest,
				skills: map[string]string{"never": "A skill that must NOT load from a rejected plugin."}})
			b, err := Load(dir)
			if err != nil {
				t.Fatalf("a bad plugin must never fail the bundle load: %v", err)
			}
			if len(b.Plugins) != 0 {
				t.Errorf("plugin must be rejected: %+v", b.Plugins)
			}
			if !containsProblem(b.PluginProblems(), "plugin rejected") {
				t.Errorf("rejection must be reported: %v", b.PluginProblems())
			}
			if _, ok := skillNames(b.Skills())["never"]; ok {
				t.Error("a rejected plugin's components must not be discovered")
			}
		})
	}
}

func TestPluginWithoutManifestIsNotAPlugin(t *testing.T) {
	dir := writePluginBundle(t, "", pluginFixture{dir: "stray", skills: map[string]string{"s": "Skill in a dir with no plugin.json."}})
	mustWrite(t, filepath.Join(dir, "plugins", "README.md"), "files beside plugins are fine\n")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 0 {
		t.Errorf("no plugin.json → not a plugin: %+v", b.Plugins)
	}
	if !containsProblem(b.PluginProblems(), "no plugin.json") {
		t.Errorf("missing manifest must be reported: %v", b.PluginProblems())
	}
}

func TestPluginNoPluginsDirIsFine(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "manifest.yaml"), "skills_builtin: false\n")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 0 || len(b.PluginProblems()) != 0 {
		t.Errorf("absent plugins/ must be silent: %+v %v", b.Plugins, b.PluginProblems())
	}
}

func TestPluginMCPTopLevelFailureDisablesMCPOnly(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": `{` + testMCPSchema + `, "mcpServers": {}, "extra": 1}`,
		"missing schema":          `{"mcpServers": {}}`,
		"version mismatch":        `{"$schema": "https://agent-plugins.org/schemas/1.1.0/mcp.schema.json", "mcpServers": {}}`,
		"missing mcpServers":      `{` + testMCPSchema + `}`,
		"mcpServers not object":   `{` + testMCPSchema + `, "mcpServers": []}`,
		"invalid json":            `{"mcpServers": `,
	}
	for label, mcp := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("p"),
				skills: map[string]string{"still-here": "Skills survive an invalid mcp.json."}, mcp: mcp})
			b, err := Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(b.Plugins) != 1 || len(b.Plugins[0].MCPServers) != 0 {
				t.Errorf("plugin should load with MCP disabled: %+v", b.Plugins)
			}
			if !containsProblem(b.PluginProblems(), "MCP disabled for this plugin") {
				t.Errorf("must report: %v", b.PluginProblems())
			}
			if _, ok := skillNames(b.Skills())["still-here"]; !ok {
				t.Error("skills must keep loading when mcp.json is invalid")
			}
		})
	}
}

func TestPluginMCPEntryFailuresSkipOnlyThatEntry(t *testing.T) {
	bad := map[string]string{
		"unknown field":         `{"type": "stdio", "command": "python3", "shell": true}`,
		"missing type":          `{"command": "python3"}`,
		"unknown type":          `{"type": "websocket", "url": "wss://x"}`,
		"missing command":       `{"type": "stdio"}`,
		"parent-escaping cmd":   `{"type": "stdio", "command": "../bin/server"}`,
		"absolute cmd":          `{"type": "stdio", "command": "/usr/bin/python3"}`,
		"placeholder cmd":       `{"type": "stdio", "command": "${PLUGIN_ROOT}/bin/x"}`,
		"missing relative cmd":  `{"type": "stdio", "command": "./bin/nope"}`,
		"reserved env key":      `{"type": "stdio", "command": "python3", "env": {"PLUGIN_ROOT": "/x"}}`,
		"env not strings":       `{"type": "stdio", "command": "python3", "env": {"A": 1}}`,
		"args not strings":      `{"type": "stdio", "command": "python3", "args": [1]}`,
		"bare cwd":              `{"type": "stdio", "command": "python3", "cwd": "data"}`,
		"escaping cwd":          `{"type": "stdio", "command": "python3", "cwd": "./../.."}`,
		"missing cwd":           `{"type": "stdio", "command": "python3", "cwd": "./does-not-exist"}`,
		"http field on stdio":   `{"type": "stdio", "command": "python3", "url": "https://x"}`,
		"http non-loopback":     `{"type": "streamable-http", "url": "http://tools.example.com/mcp"}`,
		"url with userinfo":     `{"type": "streamable-http", "url": "https://u:p@tools.example.com/mcp"}`,
		"url with fragment":     `{"type": "streamable-http", "url": "https://tools.example.com/mcp#frag"}`,
		"relative url":          `{"type": "streamable-http", "url": "/mcp"}`,
		"dup header casing":     `{"type": "streamable-http", "url": "https://x.example.com/mcp", "headers": {"X-A": "1", "x-a": "2"}}`,
		"bad header name":       `{"type": "streamable-http", "url": "https://x.example.com/mcp", "headers": {"X A": "1"}}`,
		"header line break":     `{"type": "streamable-http", "url": "https://x.example.com/mcp", "headers": {"X-A": "1\r\nInjected: y"}}`,
		"stdio field on http":   `{"type": "streamable-http", "url": "https://x.example.com/mcp", "command": "x"}`,
		"bad server name chars": `{"type": "stdio", "command": "python3"}`,
	}
	for label, entry := range bad {
		t.Run(label, func(t *testing.T) {
			key := "bad_entry"
			if label == "bad server name chars" {
				key = "has.dot"
			}
			mcp := `{` + testMCPSchema + `, "mcpServers": {"` + key + `": ` + entry + `, "good_one": {"type": "stdio", "command": "python3"}}}`
			dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("p"), mcp: mcp,
				files: map[string]string{"bin/.keep": ""}})
			b, err := Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			cfgs := b.MCPServerConfigs()
			if _, ok := cfgs[key]; ok {
				t.Errorf("bad entry %q must be skipped", key)
			}
			if _, ok := cfgs["good_one"]; !ok {
				t.Errorf("valid sibling must survive: %v", b.PluginProblems())
			}
			if !containsProblem(b.PluginProblems(), `server "`+key+`"`) || !containsProblem(b.PluginProblems(), "skipped") {
				t.Errorf("skip must be reported per entry: %v", b.PluginProblems())
			}
		})
	}
}

func TestPluginMCPCwdForms(t *testing.T) {
	dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("cwd-plugin"),
		files: map[string]string{"work/.keep": ""},
		mcp: `{` + testMCPSchema + `, "mcpServers": {
  "rel":  {"type": "stdio", "command": "python3", "cwd": "./work"},
  "root": {"type": "stdio", "command": "python3", "cwd": "${PLUGIN_ROOT}/work"},
  "data": {"type": "stdio", "command": "python3", "cwd": "${PLUGIN_DATA}/cache"}
}}`})
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 1 {
		t.Fatalf("%v", b.PluginProblems())
	}
	root := b.Plugins[0].Dir
	cfgs := b.MCPServerConfigs()
	if cfgs["rel"].Dir != filepath.Join(root, "work") || cfgs["root"].Dir != filepath.Join(root, "work") {
		t.Errorf("root-relative cwds: rel=%q root=%q", cfgs["rel"].Dir, cfgs["root"].Dir)
	}
	want := filepath.Join(b.Plugins[0].DataDir, "cache")
	if cfgs["data"].Dir != want {
		t.Errorf("data cwd = %q, want %q", cfgs["data"].Dir, want)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Errorf("PLUGIN_DATA-rooted cwd must be created before launch: %v", err)
	}
}

func TestPluginServerNameCollisions(t *testing.T) {
	manifest := `skills_builtin: false
mcp_servers:
  - name: shared
    type: stdio
    command: python3
    args: ["-c", "pass"]
    always: true
http_tools:
  - name: lookup
    description: an inline tool
    method: GET
    url: https://example.com/x
`
	mcpA := `{` + testMCPSchema + `, "mcpServers": {
  "shared": {"type": "stdio", "command": "echo"},
  "lookup": {"type": "stdio", "command": "echo"},
  "_http":  {"type": "stdio", "command": "echo"},
  "mine":   {"type": "stdio", "command": "echo"}}}`
	mcpB := `{` + testMCPSchema + `, "mcpServers": {"mine": {"type": "stdio", "command": "echo"}, "yours": {"type": "stdio", "command": "echo"}}}`
	dir := writePluginBundle(t, manifest,
		pluginFixture{dir: "a", manifest: minimalManifest("alpha"), mcp: mcpA},
		pluginFixture{dir: "b", manifest: minimalManifest("beta"), mcp: mcpB},
	)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfgs := b.MCPServerConfigs()
	if cfgs["shared"].Command != "python3" {
		t.Errorf("manifest server must win the collision: %+v", cfgs["shared"])
	}
	if _, ok := cfgs["lookup"]; ok {
		t.Error("a plugin server must not shadow an http_tool name")
	}
	if _, ok := cfgs["_http"]; ok {
		t.Error("the reserved inline-http name must be refused")
	}
	if len(b.Plugins) != 2 {
		t.Fatalf("plugins: %+v %v", b.Plugins, b.PluginProblems())
	}
	if strings.Join(b.Plugins[0].MCPServers, ",") != "mine" || strings.Join(b.Plugins[1].MCPServers, ",") != "yours" {
		t.Errorf("first plugin keeps 'mine', second loses it: %v / %v", b.Plugins[0].MCPServers, b.Plugins[1].MCPServers)
	}
	probs := b.PluginProblems()
	for _, want := range []string{`server "shared": collides`, `server "lookup": collides`, `server "_http"`, `plugins/b/mcp.json: server "mine": collides`} {
		if !containsProblem(probs, want) {
			t.Errorf("missing report %q in %v", want, probs)
		}
	}
}

func TestPluginSkillPrecedence(t *testing.T) {
	// builtin < plugin (first by plugin name) < bundle.
	dir := writePluginBundle(t, "",
		pluginFixture{dir: "z-second", manifest: minimalManifest("zeta"), skills: map[string]string{
			"shared-skill": "zeta's version", "release-notes": "zeta overrides the built-in release-notes",
		}},
		pluginFixture{dir: "a-first", manifest: minimalManifest("alpha"), skills: map[string]string{
			"shared-skill": "alpha's version", "bundle-owned": "alpha's copy, shadowed by the bundle",
		}},
	)
	// Enable builtins for this case so the plugin-over-builtin rule is exercised.
	mustWrite(t, filepath.Join(dir, "manifest.yaml"), "mcp_servers: []\n")
	mustWrite(t, filepath.Join(dir, "skills", "bundle-owned", "SKILL.md"),
		"---\nname: bundle-owned\ndescription: the bundle author's own version wins\n---\n\nBundle.\n")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	roster := skillNames(b.Skills())
	if got := roster["shared-skill"].Description; got != "alpha's version" {
		t.Errorf("between plugins the first by name wins; got %q", got)
	}
	if got := roster["bundle-owned"].Description; !strings.Contains(got, "bundle author") {
		t.Errorf("bundle must beat plugin; got %q", got)
	}
	if got := roster["release-notes"].Description; !strings.Contains(got, "zeta overrides") {
		t.Errorf("plugin must beat builtin; got %q", got)
	}
	if o := b.SkillOrigin("shared-skill"); o.Source != "plugin" || o.Plugin != "alpha" {
		t.Errorf("SkillOrigin(shared-skill) = %+v", o)
	}
	if o := b.SkillOrigin("bundle-owned"); o.Source != "bundle" {
		t.Errorf("SkillOrigin(bundle-owned) = %+v", o)
	}
	if o := b.SkillOrigin("release-notes"); o.Source != "plugin" || o.Plugin != "zeta" {
		t.Errorf("SkillOrigin(release-notes) = %+v", o)
	}
	if o := b.SkillOrigin("data-profiler"); o.Source != "builtin" {
		t.Errorf("SkillOrigin(data-profiler) = %+v", o)
	}
	// Live edit of a plugin skill body is picked up on the next Skills() read.
	mustWrite(t, filepath.Join(dir, "plugins", "a-first", "skills", "shared-skill", "SKILL.md"),
		"---\nname: shared-skill\ndescription: alpha's second revision\n---\n\nEdited.\n")
	if got := skillNames(b.Skills())["shared-skill"].Description; got != "alpha's second revision" {
		t.Errorf("plugin skill edit not picked up: %q", got)
	}
}

func TestPluginSkillFailureBoundaries(t *testing.T) {
	dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("p"),
		skills: map[string]string{"good": "A well-formed skill."},
		files: map[string]string{
			"skills/mismatch/SKILL.md":      "---\nname: other\ndescription: name does not match the folder\n---\n",
			"skills/no-md/notes.txt":        "not a skill",
			"skills/nested/deeper/SKILL.md": "---\nname: deeper\ndescription: nested skills are not discovered\n---\n",
		}})
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Join(b.Plugins[0].Skills, ",") != "good" {
		t.Errorf("Skills = %v", b.Plugins[0].Skills)
	}
	roster := skillNames(b.Skills())
	if _, ok := roster["good"]; !ok {
		t.Error("good skill missing")
	}
	for _, bad := range []string{"mismatch", "other", "no-md", "nested", "deeper"} {
		if _, ok := roster[bad]; ok {
			t.Errorf("%q must not be in the roster", bad)
		}
	}
	probs := b.PluginProblems()
	if !containsProblem(probs, "plugins/p/skills/mismatch/SKILL.md") || !containsProblem(probs, "plugins/p/skills/no-md") {
		t.Errorf("per-skill problems must be reported with the plugin prefix: %v", probs)
	}
}

func TestPluginSkillsPathNotADirDisablesSkillsOnly(t *testing.T) {
	dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("p"),
		files: map[string]string{"skills": "I am a file, not a directory"},
		mcp:   `{` + testMCPSchema + `, "mcpServers": {"srv": {"type": "stdio", "command": "python3"}}}`})
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 1 || len(b.Plugins[0].Skills) != 0 || strings.Join(b.Plugins[0].MCPServers, ",") != "srv" {
		t.Errorf("skills disabled, MCP intact: %+v (%v)", b.Plugins, b.PluginProblems())
	}
	if !containsProblem(b.PluginProblems(), "skills disabled for this plugin") {
		t.Errorf("%v", b.PluginProblems())
	}
}

func TestPluginSymlinkEscapesAreRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "SKILL.md"), "---\nname: escapee\ndescription: lives outside the plugin root\n---\n")
	mustWrite(t, filepath.Join(outside, "secret.txt"), "not yours")
	mustWrite(t, filepath.Join(outside, "server.sh"), "#!/bin/sh\n")

	dir := writePluginBundle(t, "", pluginFixture{dir: "p", manifest: minimalManifest("p"),
		skills: map[string]string{"inside": "A contained skill that also links out."},
		mcp: `{` + testMCPSchema + `, "mcpServers": {
  "linked": {"type": "stdio", "command": "./bin/server.sh"},
  "fine":   {"type": "stdio", "command": "python3"}}}`})
	pdir := filepath.Join(dir, "plugins", "p")
	// A skill whose SKILL.md is a symlink out of the root → that skill is skipped.
	if err := os.MkdirAll(filepath.Join(pdir, "skills", "escapee"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(pdir, "skills", "escapee", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// A file inside a good skill that links out → only that file is dropped.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(pdir, "skills", "inside", "REFERENCE.md")); err != nil {
		t.Fatal(err)
	}
	// A ./-relative command that links out → that entry is skipped.
	if err := os.MkdirAll(filepath.Join(pdir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "server.sh"), filepath.Join(pdir, "bin", "server.sh")); err != nil {
		t.Fatal(err)
	}

	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Join(b.Plugins[0].Skills, ",") != "inside" {
		t.Errorf("Skills = %v (%v)", b.Plugins[0].Skills, b.PluginProblems())
	}
	roster := skillNames(b.Skills())
	if _, ok := roster["escapee"]; ok {
		t.Error("escaping SKILL.md must be skipped")
	}
	if _, err := os.Stat(filepath.Join(b.SkillsDir, "inside", "REFERENCE.md")); err == nil {
		t.Error("a skill file resolving outside the plugin root must not be copied into the merged tree")
	}
	if _, err := os.Stat(filepath.Join(b.SkillsDir, "inside", "SKILL.md")); err != nil {
		t.Errorf("contained skill files must still be copied: %v", err)
	}
	cfgs := b.MCPServerConfigs()
	if _, ok := cfgs["linked"]; ok {
		t.Error("command resolving outside the plugin root must be skipped")
	}
	if _, ok := cfgs["fine"]; !ok {
		t.Error("sibling entry must survive")
	}
	if !containsProblem(b.PluginProblems(), "outside the plugin root") {
		t.Errorf("escapes must be reported: %v", b.PluginProblems())
	}
}

func TestPluginRootsAndDuplicateNames(t *testing.T) {
	dir := writePluginBundle(t, "", pluginFixture{dir: "one", manifest: minimalManifest("dup"),
		skills: map[string]string{"from-first": "Loaded from plugins/."}})
	extra := t.TempDir()
	writePluginInto(t, extra, pluginFixture{dir: "two", manifest: minimalManifest("dup"),
		skills: map[string]string{"from-second": "Must be skipped: same plugin name."}})
	writePluginInto(t, extra, pluginFixture{dir: "three", manifest: minimalManifest("other"),
		skills: map[string]string{"from-third": "Loaded from a configured plugin root."}})
	writePluginInto(t, filepath.Join(dir, "vendor"), pluginFixture{dir: "four", manifest: minimalManifest("relative-root"),
		skills: map[string]string{"from-fourth": "Loaded from a bundle-relative plugin root."}})
	mustWrite(t, filepath.Join(dir, "manifest.yaml"),
		"skills_builtin: false\nplugin_roots:\n  - "+extra+"\n  - vendor\n  - /definitely/missing/root\n")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := make([]string, 0, len(b.Plugins))
	for _, p := range b.Plugins {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "dup,other,relative-root" {
		t.Errorf("plugins = %v (%v)", names, b.PluginProblems())
	}
	if len(b.PluginRoots) != 3 || b.PluginRoots[1] != filepath.Join(dir, "vendor") {
		t.Errorf("PluginRoots = %v", b.PluginRoots)
	}
	roster := skillNames(b.Skills())
	for _, want := range []string{"from-first", "from-third", "from-fourth"} {
		if _, ok := roster[want]; !ok {
			t.Errorf("%q missing from roster", want)
		}
	}
	if _, ok := roster["from-second"]; ok {
		t.Error("duplicate plugin name must be skipped")
	}
	probs := b.PluginProblems()
	if !containsProblem(probs, `plugin name "dup" is already provided by`) {
		t.Errorf("duplicate must be reported: %v", probs)
	}
	if !containsProblem(probs, "plugin_roots: /definitely/missing/root") {
		t.Errorf("a configured root that is missing must be reported: %v", probs)
	}
}

func TestPluginManifestPluginRootsIsAKnownKey(t *testing.T) {
	// Strict manifest decoding: the new key must be accepted, and a typo of it
	// must still fail the load like any unknown key.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "manifest.yaml"), "plugin_roots: []\n")
	if _, err := Load(dir); err != nil {
		t.Fatalf("plugin_roots must be a valid manifest key: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "manifest.yaml"), "plugin_root: []\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("a misspelled key must still fail strict decoding")
	}
}

func TestExpandPluginVarsSinglePass(t *testing.T) {
	root, data := "/r", "/d"
	cases := map[string]string{
		"${PLUGIN_ROOT}/x":                "/r/x",
		"${PLUGIN_DATA}":                  "/d",
		"a${PLUGIN_ROOT}b${PLUGIN_DATA}c": "a/rb/dc",
		"${PLUGIN_ROOT":                   "${PLUGIN_ROOT",
		"${plugin_root}":                  "${plugin_root}",
		"${HOME}/${PLUGIN_ROOT}":          "${HOME}//r",
		"$${PLUGIN_ROOT}":                 "$/r", // no escape syntax in the spec: literal '$' then the expansion
		"plain":                           "plain",
	}
	for in, want := range cases {
		if got := expandPluginVars(in, root, data); got != want {
			t.Errorf("expand(%q) = %q, want %q", in, got, want)
		}
	}
	// Non-recursive: text introduced by a replacement is not rescanned.
	if got := expandPluginVars("${PLUGIN_ROOT}", "${PLUGIN_DATA}", "/d"); got != "${PLUGIN_DATA}" {
		t.Errorf("expansion must be single-pass, got %q", got)
	}
}

func TestValidPluginName(t *testing.T) {
	good := []string{"my-plugin", "acme.tools", "lint3r", "a", "a.b-c", strings.Repeat("a", 64)}
	bad := []string{"", "My-Plugin", "-start", "end-", "has--double", "too.many..dots", ".dot", "dot.", "under_score", "sp ace", strings.Repeat("a", 65)}
	for _, n := range good {
		if !validPluginName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range bad {
		if validPluginName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}
