package main

// mcp_probe.go is `fleet mcp test [<server> ...] [--all]`: a per-server smoke
// test for the bundle's MCP catalog. It loads the bundle through the SAME
// loader the server boots through (clientconfig.Load → MCPServerConfigs, so
// env interpolation, the enable gate, TLS hardening, and the bundle-root Dir
// are byte-identical to a real boot), spawns/handshakes each requested server
// (MCP initialize + tools/list via the same internal/mcp client the broker
// uses — one credential path, #167), and reports per server: connected or
// not, the tool count, and the tool names.
//
// Like validate-config it boots nothing else: no Postgres, no web tier, no
// sandbox — bundle MCP servers run host-side under the broker, so the probe
// spawns them the same way. Run it where the deployment's env lives (the
// deploy box, or any box with the env file sourced): the point is validating
// the exact machine-plus-credentials combination production uses.
//
// It never prints a credential VALUE. Failures name the server and the error
// (a missing executable, a dead process, a handshake timeout) — the same
// classes that otherwise surface as cryptic mid-chat tool errors.
//
// Exit codes: 0 = every requested server connected; 1 = at least one failed
// (or a requested name is unknown/disabled); 2 = usage error.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// mcpTestOptions are the parsed `fleet mcp test` flags + positional names.
type mcpTestOptions struct {
	bundlePath string
	all        bool
	jsonOutput bool
	timeout    time.Duration
	names      []string
}

// mcpTestResult is one server's probe outcome. JSON shape mirrors
// validate-config's machine-readable contract style.
type mcpTestResult struct {
	Server    string   `json:"server"`
	Type      string   `json:"type"` // stdio | http
	Connected bool     `json:"connected"`
	ToolCount int      `json:"tool_count"`
	Tools     []string `json:"tools,omitempty"`
	Error     string   `json:"error,omitempty"`
	// Optional mirrors the manifest flag so a sweep's output distinguishes
	// always-on servers from per-conversation opt-ins.
	Optional bool `json:"optional"`
}

// mcpTestReport is the top-level --json envelope.
type mcpTestReport struct {
	Results []mcpTestResult `json:"results"`
	Passed  bool            `json:"passed"`
	Failed  int             `json:"failed"`
}

// runMCPTest is the `fleet mcp test` entry point; returns the process exit code.
func runMCPTest(args []string) int {
	opts, err := parseMCPTestFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(opts.bundlePath) != "" {
		_ = os.Setenv(clientconfig.EnvDir, opts.bundlePath)
	}

	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "load bundle: %v\n", err)
		return 1
	}
	// The SAME resolved catalog a boot registers: enable gate applied, env
	// interpolated, stdio Dir pinned to the bundle root.
	catalog := bundle.MCPServerConfigs()

	targets, unknown := selectMCPTestTargets(catalog, opts)
	if len(unknown) > 0 {
		// Name the disabled-vs-unknown distinction: a gated-off server is the
		// most common "why isn't my connector there" answer.
		for _, n := range unknown {
			if gatedOffServer(bundle, n) {
				fmt.Fprintf(os.Stderr, "server %q exists in the manifest but its enable gate is off (enabled_env/enabled_groups unmet)\n", n)
			} else {
				fmt.Fprintf(os.Stderr, "unknown server %q (not in the bundle manifest)\n", n)
			}
		}
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no enabled MCP servers to test (pass names, or --all for the whole catalog)")
		return 1
	}

	report := mcpTestReport{Passed: true}
	for _, name := range targets {
		res := probeBundleServer(name, catalog[name], opts.timeout)
		if !res.Connected {
			report.Passed = false
			report.Failed++
		}
		report.Results = append(report.Results, res)
	}
	return emitMCPTestReport(os.Stdout, report, opts.jsonOutput)
}

// parseMCPTestFlags parses flags + positional server names for `fleet mcp test`.
func parseMCPTestFlags(args []string) (mcpTestOptions, error) {
	fs := flag.NewFlagSet("mcp test", flag.ContinueOnError)
	var opts mcpTestOptions
	fs.StringVar(&opts.bundlePath, "bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR; default config/default)")
	fs.BoolVar(&opts.all, "all", false, "test every enabled server in the catalog")
	fs.BoolVar(&opts.jsonOutput, "json", false, "machine-readable output")
	fs.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-server handshake timeout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleet mcp test [--all | <server> ...] [--bundle-path dir] [--timeout 30s] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	opts.names = fs.Args()
	if !opts.all && len(opts.names) == 0 {
		return opts, fmt.Errorf("usage: fleet mcp test [--all | <server> ...] (see --help)")
	}
	if opts.all && len(opts.names) > 0 {
		return opts, fmt.Errorf("pass either --all or explicit server names, not both")
	}
	return opts, nil
}

// selectMCPTestTargets resolves the requested names against the enabled
// catalog. Returns the sorted target list and any requested-but-absent names.
func selectMCPTestTargets(catalog map[string]config.MCPServerConfig, opts mcpTestOptions) (targets, unknown []string) {
	if opts.all {
		for name := range catalog {
			targets = append(targets, name)
		}
		sort.Strings(targets)
		return targets, nil
	}
	for _, n := range opts.names {
		if _, ok := catalog[n]; ok {
			targets = append(targets, n)
		} else {
			unknown = append(unknown, n)
		}
	}
	sort.Strings(targets)
	return targets, unknown
}

// gatedOffServer reports whether name exists in the raw manifest but was
// excluded from MCPServerConfigs by its enable gate.
func gatedOffServer(bundle *clientconfig.Bundle, name string) bool {
	for i := range bundle.MCPCatalog {
		if bundle.MCPCatalog[i].Name == name {
			return true
		}
	}
	return false
}

// probeBundleServer spawns/connects one resolved server and runs the MCP
// handshake, mirroring agent.BuildMCPClient's registration semantics exactly
// (same client methods, same ${FLEET_WORKSPACE} expansion, same TLS options)
// — but capturing the per-server error instead of log-and-skip, because the
// error IS this verb's product.
func probeBundleServer(name string, spec config.MCPServerConfig, timeout time.Duration) mcpTestResult {
	res := mcpTestResult{Server: name, Type: spec.Type, Optional: spec.Optional, ToolCount: -1}
	if res.Type == "" {
		if spec.URL != "" {
			res.Type = "http"
		} else {
			res.Type = "stdio"
		}
	}

	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var addErr error
	switch {
	case spec.URL != "":
		addErr = client.AddHTTPServerWithOptions(ctx, name, spec.URL, mcp.HTTPServerOptions{Headers: spec.Headers, TLS: spec.TLS})
	case spec.Command != "":
		env := spec.Env
		if agentcore.EnvReferencesWorkspace(env) {
			env = agentcore.ExpandWorkspaceEnv(env, agentcore.SharedMCPWorkspaceDir())
		}
		addErr = client.AddStdioServer(ctx, name, spec.Command, spec.Args, env, spec.Dir)
	default:
		addErr = fmt.Errorf("manifest entry has neither command nor url")
	}
	if addErr != nil {
		res.Error = addErr.Error()
		return res
	}

	res.Connected = true
	res.ToolCount = 0
	for _, st := range client.GetAllTools() {
		if st.ServerName != name {
			continue
		}
		res.ToolCount++
		res.Tools = append(res.Tools, st.Tool.Name)
	}
	sort.Strings(res.Tools)
	return res
}

// emitMCPTestReport prints the report (human or --json) and returns the exit code.
func emitMCPTestReport(w io.Writer, report mcpTestReport, jsonOutput bool) int {
	if jsonOutput {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		for _, r := range report.Results {
			if r.Connected {
				fmt.Fprintf(w, "✓ %-24s %-6s %3d tools", r.Server, r.Type, r.ToolCount)
				if r.Optional {
					fmt.Fprint(w, "  (optional)")
				}
				fmt.Fprintln(w)
				for _, t := range r.Tools {
					fmt.Fprintf(w, "    - %s\n", t)
				}
			} else {
				fmt.Fprintf(w, "✗ %-24s %-6s FAILED: %s\n", r.Server, r.Type, r.Error)
			}
		}
		if report.Passed {
			fmt.Fprintf(w, "\nall %d server(s) connected\n", len(report.Results))
		} else {
			fmt.Fprintf(w, "\n%d of %d server(s) FAILED\n", report.Failed, len(report.Results))
		}
	}
	if report.Passed {
		return 0
	}
	return 1
}
