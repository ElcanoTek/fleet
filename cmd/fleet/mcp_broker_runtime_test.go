package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
)

const fakeProductionBrokerScript = `
import json, os, sys
for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method")
    resp = {"id": req.get("id")}
    if method == "ping" and os.environ.get("CONNECTOR_TOKEN") != "child-secret":
        resp["err"] = "connector environment was not inherited"
    elif method == "list_tools":
        resp["tools"] = [{"server":"demo","tool":"lookup","description":"lookup","inputSchema":{"type":"object"}}]
    elif method == "list_accounts":
        resp["accounts"] = ["blue"]
    elif method == "scope_open":
        spec = req.get("scopeSpec", {})
        remote = spec.get("remote")
        if remote is not None:
            if remote == {"userEmail":"scheduled@example.com", "shadowed":["demo"]}:
                resp["scope"] = "scheduled-remote-scope"
            elif remote != {"userEmail":"user@example.com", "filterEnabled":True, "enabled":["alpha","zeta"], "shadowed":["base","demo"]}:
                resp["err"] = "remote scope metadata mismatch"
            else:
                resp["scope"] = "remote-scope"
                resp["tools"] = [{"server":"hosted","tool":"search","inputSchema":{"type":"object"}}]
                resp["skipped"] = ["needs-login"]
        elif spec.get("taskId") != "task-123" or spec.get("workspace") != "/workspace" or spec.get("selection") != [{"server":"demo","account":"blue"}]:
            resp["err"] = "scope metadata mismatch"
        else:
            resp["scope"] = "scope-1"
            resp["tools"] = [{"server":"demo","tool":"scoped_lookup","inputSchema":{"type":"object"}}]
    elif method == "call":
        if req.get("scope") != "remote-scope" or req.get("server") != "hosted" or req.get("tool") != "search":
            resp["err"] = "remote call routing mismatch"
        else:
            resp["text"] = "remote result"
    elif method == "reload":
        resp["reload"] = {
            "summary": {"added":["future"], "removed":["demo"], "restarted":[], "unchanged":[]},
            "tools": [{"server":"future","tool":"fresh_lookup","inputSchema":{"type":"object"}}],
            "accounts": {"future":["green"]},
            "servers": [{"name":"future", "toolAllowlist":["fresh_lookup"], "accountVars":["TOKEN"], "optional":True, "usesWorkspace":True}]
        }
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

func TestStartProductionMCPRuntime_InheritsThenScrubsParent(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	t.Setenv("CONNECTOR_TOKEN", "child-secret")
	t.Setenv("TOKEN_BLUE", "account-secret")
	t.Setenv("MODEL_KEY", "parent-model-secret")
	bundleDir := mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${CONNECTOR_TOKEN}"
      WORK: "${FLEET_WORKSPACE}"
    tools: [lookup]
    account_vars: [TOKEN]
`)
	bundle, err := clientconfig.Load(bundleDir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	bundle.Providers = []clientconfig.ProviderDef{{Name: "models", Type: "openai", APIKeyEnv: "MODEL_KEY"}}
	cfg := &config.Config{
		MCPServers: bundle.MCPServerConfigs(),
		HTTPTools:  []config.HTTPToolConfig{{Name: "sensitive", Headers: map[string]string{"Authorization": "secret"}}},
	}
	cmd := exec.Command("python3", "-u", "-c", fakeProductionBrokerScript)
	cmd.Stderr = os.Stderr

	runtime, specs, err := startProductionMCPRuntime(bundle, cfg, cmd)
	if err != nil {
		t.Fatalf("startProductionMCPRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if os.Getenv("CONNECTOR_TOKEN") != "" || os.Getenv("TOKEN_BLUE") != "" {
		t.Fatalf("connector environment survived scrub: base=%q account=%q", os.Getenv("CONNECTOR_TOKEN"), os.Getenv("TOKEN_BLUE"))
	}
	if os.Getenv("MODEL_KEY") != "parent-model-secret" {
		t.Fatal("parent-owned provider credential was scrubbed")
	}
	if cfg.MCPServers != nil || cfg.HTTPTools != nil || bundle.MCPCatalog != nil || bundle.HTTPTools != nil {
		t.Fatalf("resolved connector state survived: cfg=%+v bundle_servers=%+v bundle_tools=%+v", cfg.MCPServers, bundle.MCPCatalog, bundle.HTTPTools)
	}
	if len(runtime.catalog) != 1 || runtime.catalog[0].ServerName != "demo" || runtime.catalog[0].Tool.Name != "lookup" {
		t.Fatalf("public catalog = %+v", runtime.catalog)
	}
	if !slices.Equal(runtime.accounts["demo"], []string{"blue"}) {
		t.Fatalf("public accounts = %+v", runtime.accounts)
	}
	if len(specs) != 1 || specs["demo"].Command != "" || specs["demo"].Env != nil || specs["demo"].Headers != nil || specs["demo"].URL != "" {
		t.Fatalf("parent specs retained credential-bearing fields: %+v", specs)
	}
	if !runtime.inventory.snapshot()["demo"].UsesWorkspace {
		t.Fatal("public scheduled inventory lost uses-workspace metadata")
	}
	if !slices.Equal(runtime.inventory.snapshot()["demo"].ToolAllowlist, []string{"lookup"}) {
		t.Fatalf("public scheduled inventory lost the Gate-2 tool allowlist after scrub: %+v", runtime.inventory.snapshot()["demo"])
	}
	if err := runtime.client.Ping(context.Background()); err != nil {
		t.Fatalf("child lost inherited environment after parent scrub: %v", err)
	}
	scope, err := runtime.openTaskScope(context.Background(), agentcore.MCPSelection{{Server: "demo", Account: "blue"}}, "task-123", "/workspace")
	if err != nil {
		t.Fatalf("open task scope: %v", err)
	}
	if len(scope.Catalog) != 1 || scope.Catalog[0].Tool.Name != "scoped_lookup" {
		t.Fatalf("scoped catalog = %+v", scope.Catalog)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatalf("close task scope: %v", err)
	}
	assertProductionRemoteOverlay(t, runtime)
	assertProductionReload(t, runtime)
}

func assertProductionReload(t *testing.T, runtime *productionMCPRuntime) {
	t.Helper()
	reloaded, err := runtime.reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Catalog) != 1 || reloaded.Catalog[0].ServerName != "future" ||
		!slices.Equal(reloaded.Accounts["future"], []string{"green"}) ||
		!reloaded.Specs["future"].Optional || reloaded.Specs["future"].Env != nil {
		t.Fatalf("public reload result = %+v", reloaded)
	}
	if inventory := runtime.inventory.snapshot(); len(inventory) != 1 || !inventory["future"].UsesWorkspace ||
		!slices.Equal(inventory["future"].ToolAllowlist, []string{"fresh_lookup"}) {
		t.Fatalf("live inventory after reload = %+v", inventory)
	}
}

func assertProductionRemoteOverlay(t *testing.T, runtime *productionMCPRuntime) {
	t.Helper()
	remote, err := runtime.openRemoteOverlay(
		context.Background(),
		"user@example.com",
		map[string]bool{"demo": true, "ignored": false, "base": true},
		map[string]bool{"zeta": true, "off": false, "alpha": true},
	)
	if err != nil {
		t.Fatalf("open remote overlay: %v", err)
	}
	if err := remote.Validate(); err != nil {
		t.Fatalf("validate remote overlay: %v", err)
	}
	if len(remote.Catalog) != 1 || remote.Catalog[0].ServerName != "hosted" ||
		remote.Catalog[0].Tool.Name != "search" || !remote.Servers["hosted"] ||
		!slices.Equal(remote.Skipped, []string{"needs-login"}) {
		t.Fatalf("remote overlay = %+v", remote)
	}
	text, isError, err := remote.Broker.CallMCP(context.Background(), "hosted", "search", map[string]any{"q": "fleet"})
	if err != nil || isError || text != "remote result" {
		t.Fatalf("remote call = (%q, %v, %v)", text, isError, err)
	}
	remote.Close()
	scheduledRemote, err := runtime.openRemoteOverlay(
		context.Background(),
		"scheduled@example.com",
		map[string]bool{"demo": true},
		nil,
	)
	if err != nil {
		t.Fatalf("open scheduled remote overlay: %v", err)
	}
	if scheduledRemote.Active() || scheduledRemote.Servers == nil {
		t.Fatalf("empty scheduled remote overlay = %+v", scheduledRemote)
	}
	scheduledRemote.Close()
}

func TestProductionRemoteMCPOverlayOpenerGatesFeature(t *testing.T) {
	runtime := &productionMCPRuntime{}
	if opener := productionRemoteMCPOverlayOpener(nil, runtime); opener != nil {
		t.Fatal("disabled remote MCP produced an overlay opener")
	}
	if opener := productionRemoteMCPOverlayOpener(new(remotemcp.Service), nil); opener != nil {
		t.Fatal("missing broker runtime produced an overlay opener")
	}
	if opener := productionRemoteMCPOverlayOpener(new(remotemcp.Service), runtime); opener == nil {
		t.Fatal("enabled remote MCP did not produce an overlay opener")
	}
}

func TestValidateConnectorParentEnvSeparation_AllowsCutlassConnectorWireKeys(t *testing.T) {
	t.Setenv("CUTLASS_ALLOWED_DIRS", "/srv/drops")
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: sendgrid
    type: stdio
    command: /bin/true
    env:
      CUTLASS_MOC_TASK_ID: "${FLEET_TASK_ID}"
      CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}"
      CUTLASS_RUN_WORKDIR: "${FLEET_WORKSPACE}"
      CUTLASS_USER_AGENT: "${CUTLASS_USER_AGENT}"
      CUTLASS_REPORT_DIR: "${CUTLASS_REPORT_DIR}"
      CUTLASS_INPUT_DIR: "${FLEET_WORKSPACE}/inputs"
    optional_env:
      - CUTLASS_ALLOWED_DIRS
      - CUTLASS_RUN_WORKDIR
      - CUTLASS_MOC_TASK_ID
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if err := validateConnectorParentEnvSeparation(bundle); err != nil {
		t.Fatalf("cutlass-family connector wire keys refused: %v", err)
	}
}

func TestValidateConnectorParentEnvSeparation_RejectsParentOwnedCutlassNames(t *testing.T) {
	for _, name := range []string{
		"CUTLASS_TASK_MODEL", "CUTLASS_LOG_FILE", "CUTLASS_RETRY_MAX_ATTEMPTS",
		// Legacy spellings the parent resolves lazily after broker boot: the
		// shared server auth secret plus the per-run lookups through the
		// alias machinery (config.lookupFleet, agentcore.EnvPrefix).
		"CUTLASS_SERVER_TOKEN",
		"CUTLASS_OPENROUTER_BASE_URL", "CUTLASS_MODEL_CACHE_TTL_MINUTES",
		"CUTLASS_CONTEXT_PRESSURE_WARN_THRESHOLD", "CUTLASS_CONTEXT_COMPACTION_THRESHOLD",
		"CUTLASS_SCHEDULED_AUTO_COMPACT", "CUTLASS_MAX_ITERATIONS",
	} {
		t.Run(name, func(t *testing.T) {
			bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      WIRE_KEY: "${`+name+`}"
`))
			if err != nil {
				t.Fatalf("load bundle: %v", err)
			}
			if err := validateConnectorParentEnvSeparation(bundle); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("overlap error = %v, want name-only %s refusal", err, name)
			}
		})
	}
}

// Walks the alias machinery over the whole parent-owned enumeration: every
// spelling config's FLEET_/CHAT_/CUTLASS_ prefix aliasing resolves for a
// parent-owned name must be refused, so a name added to the enumeration later
// cannot silently escape in one of its legacy spellings.
func TestValidateConnectorParentEnvSeparation_RefusesEveryAliasSpelling(t *testing.T) {
	spellings := map[string]bool{}
	var refs strings.Builder
	for _, name := range parentOwnedRuntimeEnvNames(nil) {
		for _, spelling := range config.EnvAliases(name) {
			if spellings[spelling] {
				continue
			}
			spellings[spelling] = true
			fmt.Fprintf(&refs, "      WIRE_KEY_%d: \"${%s}\"\n", len(spellings), spelling)
		}
	}
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
`+refs.String()))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	err = validateConnectorParentEnvSeparation(bundle)
	if err == nil {
		t.Fatal("bundle claiming every parent-owned spelling validated")
	}
	_, list, ok := strings.Cut(err.Error(), ": ")
	if !ok {
		t.Fatalf("overlap error = %v, want ': '-separated name list", err)
	}
	refused := map[string]bool{}
	for _, name := range strings.Split(list, ", ") {
		refused[name] = true
	}
	for spelling := range spellings {
		if !refused[spelling] {
			t.Errorf("alias spelling %s of a parent-owned name was not refused", spelling)
		}
	}
}

func TestValidateConnectorParentEnvSeparation(t *testing.T) {
	t.Setenv("MODEL_KEY", "placeholder")
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${MODEL_KEY}"
providers:
  - name: models
    type: openai
    api_key_env: MODEL_KEY
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	err = validateConnectorParentEnvSeparation(bundle)
	if err == nil || !strings.Contains(err.Error(), "MODEL_KEY") || strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("overlap error = %v, want name-only MODEL_KEY refusal", err)
	}
}

func TestValidateConnectorParentEnvSeparation_RejectsRuntimeAndAccountOverlap(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		prepare  func(*clientconfig.Bundle)
		want     string
	}{
		{
			name: "runtime environment",
			manifest: `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      CONNECTOR_HOME: "${HOME}"
`,
			want: "HOME",
		},
		{
			name: "account-suffixed provider key",
			manifest: `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      MODEL_KEY: "${CONNECTOR_TOKEN:-default}"
`,
			prepare: func(bundle *clientconfig.Bundle) {
				bundle.Providers = []clientconfig.ProviderDef{{Name: "models", Type: "openai", APIKeyEnv: "MODEL_KEY_BLUE"}}
			},
			want: "MODEL_KEY_BLUE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle, err := clientconfig.Load(mcpTestBundle(t, tt.manifest))
			if err != nil {
				t.Fatalf("load bundle: %v", err)
			}
			if tt.prepare != nil {
				tt.prepare(bundle)
			}
			err = validateConnectorParentEnvSeparation(bundle)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("overlap error = %v, want name-only %s refusal", err, tt.want)
			}
		})
	}
}

func TestStartProductionMCPRuntime_FailureDoesNotScrubParent(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	t.Setenv("CONNECTOR_TOKEN", "must-survive")
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${CONNECTOR_TOKEN}"
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	cfg := &config.Config{MCPServers: bundle.MCPServerConfigs()}
	cmd := exec.Command("python3", "-u", "-c", `
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    sys.stdout.write(json.dumps({"id": req.get("id"), "err": "boot refused"}) + "\n")
    sys.stdout.flush()
`)

	runtime, _, err := startProductionMCPRuntime(bundle, cfg, cmd)
	if err == nil || runtime != nil {
		t.Fatalf("startProductionMCPRuntime = (%v, %v), want failure", runtime, err)
	}
	if os.Getenv("CONNECTOR_TOKEN") != "must-survive" {
		t.Fatal("failed startup scrubbed connector environment")
	}
	if len(cfg.MCPServers) != 1 || len(bundle.MCPCatalog) != 1 {
		t.Fatalf("failed startup scrubbed resolved state: cfg=%+v bundle=%+v", cfg.MCPServers, bundle.MCPCatalog)
	}
}

func TestStartProductionMCPRuntime_RejectsExplicitCommandEnvWithoutScrubbing(t *testing.T) {
	t.Setenv("CONNECTOR_TOKEN", "must-survive")
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${CONNECTOR_TOKEN}"
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	cfg := &config.Config{MCPServers: bundle.MCPServerConfigs()}
	cmd := exec.Command("unused")
	cmd.Env = []string{"CONNECTOR_TOKEN=must-survive"}

	runtime, _, err := startProductionMCPRuntime(bundle, cfg, cmd)
	if err == nil || runtime != nil || !strings.Contains(err.Error(), "must be inherited") {
		t.Fatalf("startProductionMCPRuntime = (%v, %v), want inherited-environment refusal", runtime, err)
	}
	if os.Getenv("CONNECTOR_TOKEN") != "must-survive" || len(cfg.MCPServers) != 1 || len(bundle.MCPCatalog) != 1 {
		t.Fatal("pre-spawn refusal mutated parent connector state")
	}
}

func TestPublicMCPServerSpecsStripPrivateRuntimeFields(t *testing.T) {
	tlsConfig := &mcp.TLSOptions{ServerName: "private.example"}
	src := map[string]agent.MCPServerSpec{
		"enabled": {
			Enabled: true, Command: "/private/bin", Args: []string{"secret"},
			Env: map[string]string{"TOKEN": "secret"}, Dir: "/private/dir",
			URL: "https://private.example", Headers: map[string]string{"Authorization": "secret"}, TLS: tlsConfig,
			ToolAllowlist: []string{"lookup"}, AccountVars: []string{"TOKEN"}, Optional: true,
			DisplayName: "Public", Description: "Public description", Beta: true, EnabledByDefault: true,
		},
		"disabled": {Enabled: false, Env: map[string]string{"TOKEN": "secret"}},
	}

	got := publicMCPServerSpecs(src)
	want := map[string]agent.MCPServerSpec{
		"enabled": {
			Enabled: true, ToolAllowlist: []string{"lookup"}, AccountVars: []string{"TOKEN"}, Optional: true,
			DisplayName: "Public", Description: "Public description", Beta: true, EnabledByDefault: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publicMCPServerSpecs = %#v, want %#v", got, want)
	}
	src["enabled"].ToolAllowlist[0] = "mutated"
	src["enabled"].AccountVars[0] = "mutated"
	if got["enabled"].ToolAllowlist[0] != "lookup" || got["enabled"].AccountVars[0] != "TOKEN" {
		t.Fatal("public spec aliases source slices")
	}
}

func TestLoadMCPBrokerConfigUsesBootEnvironmentSnapshot(t *testing.T) {
	t.Setenv("CONNECTOR_TOKEN", "boot-secret")
	bundleDir := mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${CONNECTOR_TOKEN}"
`)
	t.Setenv(clientconfig.EnvDir, bundleDir)
	envPath := t.TempDir() + "/fleet.env"
	if err := os.WriteFile(envPath, []byte("CONNECTOR_TOKEN=rotated-secret\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	t.Setenv("FLEET_ENV_FILE", envPath)

	cfg, err := loadMCPBrokerConfig()
	if err != nil {
		t.Fatalf("loadMCPBrokerConfig: %v", err)
	}
	if got := cfg.MCPServers["demo"].Env["TOKEN"]; got != "boot-secret" {
		t.Fatalf("broker token = %q, want boot snapshot", got)
	}
	if got := os.Getenv("CONNECTOR_TOKEN"); got != "boot-secret" {
		t.Fatalf("broker config load mutated process environment to %q", got)
	}
}
