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
// --deep goes one rung further: for every connected server that advertises an
// auth-status tool ("auth_status" or "*_auth_status" — the bundle servers'
// convention for "ask the upstream if my credentials are actually valid"), it
// CALLS that tool and reports the result. The handshake proves dial tone;
// --deep proves the far end accepts the call. Servers without such a tool are
// noted and skipped, never failed.
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
	deep       bool
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
	// DeepChecks holds --deep auth-status call outcomes (absent otherwise).
	DeepChecks []mcpDeepCheck `json:"deep_checks,omitempty"`
}

// mcpDeepCheck is one --deep auth-status tool call's outcome. OK means the
// CALL succeeded and the server did not flag the result as an error
// (ToolResult.IsError); Detail carries the result's first text block (or the
// error) so the operator judges the CONTENT — the probe does not interpret
// auth semantics beyond the MCP error flag, deliberately: result shapes are
// server-specific and guessing would fake precision.
type mcpDeepCheck struct {
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
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
		res := probeBundleServer(name, catalog[name], opts.timeout, opts.deep)
		if !res.Connected || deepFailed(res) {
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
	fs.BoolVar(&opts.deep, "deep", false, "also call each server's auth-status tool (auth_status / *_auth_status) to verify credentials against the upstream")
	fs.BoolVar(&opts.jsonOutput, "json", false, "machine-readable output")
	fs.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-server handshake timeout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleet mcp test [--all | <server> ...] [--deep] [--bundle-path dir] [--timeout 30s] [--json]")
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
// deepFailed reports whether any --deep check on a connected server failed.
func deepFailed(res mcpTestResult) bool {
	for _, c := range res.DeepChecks {
		if !c.OK {
			return true
		}
	}
	return false
}

func probeBundleServer(name string, spec config.MCPServerConfig, timeout time.Duration, deep bool) mcpTestResult {
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

	if deep {
		res.DeepChecks = runDeepChecks(client, name, res.Tools, timeout)
	}
	return res
}

// runDeepChecks calls every advertised auth-status tool (the bundle servers'
// convention: "auth_status" or "*_auth_status") with empty arguments and a
// fresh per-call timeout. A server with no such tool gets a single skipped
// (OK) entry so the report says WHY nothing deeper was proven.
func runDeepChecks(client *mcp.Client, server string, tools []string, timeout time.Duration) []mcpDeepCheck {
	var checks []mcpDeepCheck
	for _, t := range tools {
		if t != "auth_status" && !strings.HasSuffix(t, "_auth_status") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		result, err := client.CallToolOn(ctx, server, t, map[string]interface{}{})
		cancel()
		switch {
		case err != nil:
			checks = append(checks, mcpDeepCheck{Tool: t, OK: false, Detail: err.Error()})
		case result.IsError:
			checks = append(checks, mcpDeepCheck{Tool: t, OK: false, Detail: firstResultText(result)})
		default:
			checks = append(checks, mcpDeepCheck{Tool: t, OK: true, Detail: firstResultText(result)})
		}
	}
	if len(checks) == 0 {
		checks = append(checks, mcpDeepCheck{Tool: "", OK: true, Detail: "no auth-status tool advertised — deep check skipped"})
	}
	return checks
}

// firstResultText extracts the result's first text block, collapsed to one
// line and capped so a verbose status payload cannot flood the report.
func firstResultText(result *mcp.ToolResult) string {
	for _, block := range result.Content {
		text := strings.Join(strings.Fields(block.Text), " ")
		if text == "" {
			continue
		}
		const maxDetail = 200
		if r := []rune(text); len(r) > maxDetail {
			text = string(r[:maxDetail]) + "…"
		}
		return text
	}
	return "(no text content)"
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
				for _, c := range r.DeepChecks {
					switch {
					case c.Tool == "":
						fmt.Fprintf(w, "    deep: %s\n", c.Detail)
					case c.OK:
						fmt.Fprintf(w, "    deep ✓ %s — %s\n", c.Tool, c.Detail)
					default:
						fmt.Fprintf(w, "    deep ✗ %s FAILED — %s\n", c.Tool, c.Detail)
					}
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
